/**
 * ZIP archive import endpoint.
 */

import type { Router } from "express";
import express from "express";
import path from "path";
import fsExtra from "fs-extra";
import { randomBytes } from "crypto";
import AdmZip from "adm-zip";
import { Logger } from "../../core/logger.js";
import { Database } from "../../core/database.js";
import { ConfigManager } from "../../core/config.js";
import { asyncHandler } from "./errorHandler.js";
import { createRateLimiter } from "./rateLimiter.js";
import { sanitizeForFilename } from "../../utils/sanitize.js";

export function registerImportRoutes(router: Router): void {
  const logger = Logger.getInstance();

  // Import zip archive (rate limited to 5 per minute)
  const importRateLimiter = createRateLimiter(5, 60 * 1000);
  router.post(
    "/import",
    express.raw({ type: "application/octet-stream", limit: "500mb" }),
    importRateLimiter,
    asyncHandler(async (req, res) => {
      const tempFiles: string[] = [];
      try {
        const config = ConfigManager.getInstance().get();
        const outputDir = config.downloader?.output_directory || "./output";
        const stagingDir = config.downloader?.staging_directory || "./staging";

        // Write request body to temp file
        const tempPath = path.join(stagingDir, `import-${Date.now()}.zip`);
        await fsExtra.ensureDir(stagingDir);
        await fsExtra.writeFile(tempPath, req.body as Buffer);
        tempFiles.push(tempPath);

        // Open zip
        let zip: AdmZip;
        try {
          zip = new AdmZip(tempPath);
        } catch (e) {
          return res.status(400).json({ error: "Invalid zip file" });
        }

        const entries = zip.getEntries();

        // Zip bomb protection
        const MAX_UNCOMPRESSED = 2 * 1024 * 1024 * 1024; // 2GB (reduced from 50GB)
        const MAX_FILES = 1000; // Limit file count
        const MAX_COMPRESSION_RATIO = 100; // Alert on >100x compression

        // Check file count
        if (entries.length > MAX_FILES) {
          return res.status(400).json({ error: "Too many files in zip" });
        }

        // Check total uncompressed size and compression ratio
        let totalUncompressed = 0;
        for (const entry of entries) {
          totalUncompressed += entry.header.size;
          if (totalUncompressed > MAX_UNCOMPRESSED) {
            return res
              .status(400)
              .json({ error: "Zip file too large (uncompressed, max 2GB)" });
          }
        }

        // Check compression ratio (get compressed size from file)
        const stat = await fsExtra.stat(tempPath);
        const compressedSize = stat.size;
        if (compressedSize > 0 && totalUncompressed / compressedSize > MAX_COMPRESSION_RATIO) {
          return res.status(400).json({ error: "Suspicious compression ratio (possible zip bomb)" });
        }

        // Validate zip entries for path traversal
        for (const entry of entries) {
          const normalized = path.normalize(entry.entryName);
          if (
            normalized.includes("..") ||
            path.isAbsolute(normalized) ||
            normalized.startsWith("/") ||
            normalized.startsWith("\\")
          ) {
            return res
              .status(400)
              .json({ error: "Invalid zip entry path" });
          }
        }

        // Scan for video and chat files
        const VIDEO_EXTS = [".mp4", ".mkv", ".webm", ".ts"];
        let videoEntry: AdmZip.IZipEntry | null = null;
        let chatEntry: AdmZip.IZipEntry | null = null;

        for (const entry of entries) {
          if (entry.isDirectory) continue;
          const name = entry.entryName.toLowerCase();
          const ext = path.extname(name);

          if (!videoEntry && VIDEO_EXTS.includes(ext)) {
            videoEntry = entry;
          }

          if (!chatEntry && name.endsWith(".chat.json")) {
            chatEntry = entry;
          }
        }

        // If no explicit .chat.json, try to find a .json with messages array
        const MAX_JSON_SIZE = 10 * 1024 * 1024; // 10MB
        if (!chatEntry) {
          for (const entry of entries) {
            if (entry.isDirectory) continue;
            if (!entry.entryName.toLowerCase().endsWith(".json")) continue;
            if (entry.header.size > MAX_JSON_SIZE) continue;
            try {
              const jsonStr = entry.getData().toString("utf-8");
              const parsed = JSON.parse(jsonStr);
              if (
                Array.isArray(parsed.messages) &&
                parsed.messages.length > 0 &&
                typeof parsed.messages[0].offsetMs === "number"
              ) {
                chatEntry = entry;
                break;
              }
            } catch {
              // Not valid JSON, skip
            }
          }
        }

        if (!videoEntry) {
          return res
            .status(400)
            .json({ error: "No video file found in zip (.mp4, .mkv, .webm, .ts)" });
        }

        // Derive metadata
        const videoFilename = path.basename(videoEntry.entryName);
        const videoExt = path.extname(videoFilename);
        const videoBasename = videoFilename.slice(0, -videoExt.length);

        // Try to extract video ID from [XXXXXXXXXXX] pattern
        const idMatch = videoBasename.match(/\[([a-zA-Z0-9_-]{11})\]/);
        let videoId = idMatch
          ? idMatch[1]
          : `imp_${randomBytes(4).toString("hex")}`;

        // Read optional chat metadata
        let chatMeta: {
          videoId?: string;
          videoTitle?: string;
          channelName?: string;
        } = {};
        if (chatEntry) {
          try {
            const chatJson = JSON.parse(
              chatEntry.getData().toString("utf-8"),
            );
            chatMeta = {
              videoId: chatJson.videoId,
              videoTitle: chatJson.videoTitle,
              channelName: chatJson.channelName,
            };
          } catch {
            // Ignore parse errors for metadata
          }
        }

        // Use chat metadata videoId if we generated a random one
        if (!idMatch && chatMeta.videoId) {
          videoId = chatMeta.videoId;
        }

        const titleHeader = req.headers["x-import-title"] as
          | string
          | undefined;
        const channelHeader = req.headers["x-import-channel"] as
          | string
          | undefined;

        const title =
          titleHeader || chatMeta.videoTitle || videoBasename || "Import";
        const channelName = channelHeader || chatMeta.channelName || "Import";

        // Check for duplicate
        const db = await Database.getInstance();
        const exists = await db.jobExists(videoId);
        if (exists) {
          return res
            .status(409)
            .json({ error: `Job already exists for video ID: ${videoId}` });
        }

        // Compute output paths
        // Sanitize for filesystem
        const safeName = sanitizeForFilename(title);
        const relativeDir = "imports";
        const baseFilename = `${safeName} [${videoId}]`;
        const filename = path.join(
          relativeDir,
          `${baseFilename}${videoExt}`,
        );
        const chatFilename = chatEntry
          ? path.join(relativeDir, `${baseFilename}.chat.json`)
          : undefined;

        // Extract files
        const videoOutPath = path.join(outputDir, filename);
        await fsExtra.ensureDir(path.dirname(videoOutPath));
        zip.extractEntryTo(
          videoEntry,
          path.dirname(videoOutPath),
          false,
          true,
          false,
          path.basename(videoOutPath),
        );

        if (chatEntry && chatFilename) {
          const chatOutPath = path.join(outputDir, chatFilename);
          zip.extractEntryTo(
            chatEntry,
            path.dirname(chatOutPath),
            false,
            true,
            false,
            path.basename(chatOutPath),
          );
        }

        // Create job
        const job = await db.addJob({
          videoId,
          url: `https://www.youtube.com/watch?v=${videoId}`,
          title,
          channelName,
          thumbnailUrl: `https://i.ytimg.com/vi/${videoId}/maxresdefault.jpg`,
          manuallyAdded: true,
        });

        if (!job) {
          return res
            .status(500)
            .json({ error: "Failed to create job entry" });
        }

        // Immediately mark as Finished
        await db.updateJob(job.id, {
          status: "Finished",
          filename,
          chatFilename,
          percent: 100,
          progress: "Imported",
        });

        logger.info(`[Import] Imported: ${title} (${videoId})`);

        // Fetch the updated job to return
        const updatedJob = await db.getJob(job.id);
        res.status(201).json(updatedJob);
      } finally {
        // Clean up temp files
        for (const f of tempFiles) {
          try {
            await fsExtra.remove(f);
          } catch {}
        }
      }
    }),
  );
}
