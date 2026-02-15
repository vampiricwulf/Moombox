/**
 * Muxing, finalization, and file download utilities.
 */

import path from "path";
import fs from "fs-extra";
import { open as fsOpen } from "node:fs/promises";
import { spawn } from "child_process";
import { Database, type Job } from "../database.js";
import { ConfigManager } from "../config.js";
import { Logger } from "../logger.js";
import { Muxer, type MuxTrimOptions } from "../../engine/muxer.js";
import { NotificationManager, NotificationType } from "../notifications.js";
import { AssetDownloader } from "./assetDownloader.js";
import { SegmentDownloader } from "../../engine/downloader.js";
import {
  DOWNLOAD_CHUNK_SIZE,
  USER_AGENTS,
} from "../../constants.js";
import { getErrorMessage } from "../../types/errors.js";
import { formatElapsed, formatBytes } from "../../utils/timeFormat.js";

/**
 * Extract video metadata using ffprobe
 */
async function extractVideoMetadata(
  filePath: string,
): Promise<{ duration: number; width: number; height: number; size: number } | null> {
  return new Promise((resolve) => {
    const ffprobe = spawn("ffprobe", [
      "-v",
      "quiet",
      "-print_format",
      "json",
      "-show_format",
      "-show_streams",
      filePath,
    ]);

    let stdout = "";
    let stderr = "";

    ffprobe.stdout.on("data", (data) => {
      stdout += data.toString();
    });

    ffprobe.stderr.on("data", (data) => {
      stderr += data.toString();
    });

    ffprobe.on("close", (code) => {
      if (code !== 0 || !stdout) {
        Logger.getInstance().warn(
          `[Metadata] ffprobe failed (code ${code}): ${stderr}`,
        );
        resolve(null);
        return;
      }

      try {
        const data = JSON.parse(stdout);
        const videoStream = data.streams?.find((s: any) => s.codec_type === "video");
        const format = data.format;

        if (!videoStream || !format) {
          resolve(null);
          return;
        }

        resolve({
          duration: Math.floor(parseFloat(format.duration) || 0),
          width: parseInt(videoStream.width) || 0,
          height: parseInt(videoStream.height) || 0,
          size: parseInt(format.size) || 0,
        });
      } catch (e) {
        Logger.getInstance().warn(
          `[Metadata] Failed to parse ffprobe output: ${e}`,
        );
        resolve(null);
      }
    });

    ffprobe.on("error", (err) => {
      Logger.getInstance().warn(`[Metadata] ffprobe error: ${err.message}`);
      resolve(null);
    });
  });
}

/**
 * Mux streams and finalize job
 */
export async function muxAndFinalize(
  job: Job,
  videoPath: string,
  audioPath: string | null,
  outputDir: string,
  stagingDir: string,
  finalExtension: string,
  db: Database,
  signal?: AbortSignal,
  trimOptions?: MuxTrimOptions,
): Promise<void> {
  const logger = Logger.getInstance();
  const config = ConfigManager.getInstance().get();
  const assetDownloader = new AssetDownloader();

  logger.info("[DownloadOrchestrator] Muxing...");
  await db.updateJob(job.id, { status: "Muxing" });

  // "Muxing Starting" notification
  {
    const freshJob = (await db.getJobs()).find(j => j.id === job.id);
    const muxFields: { name: string; value: string; inline?: boolean }[] = [];
    if (freshJob?.lastVideoSeq) muxFields.push({ name: "Video Segments", value: String(freshJob.lastVideoSeq), inline: true });
    if (freshJob?.lastAudioSeq) muxFields.push({ name: "Audio Segments", value: String(freshJob.lastAudioSeq), inline: true });
    if (freshJob?.totalChatMessages) muxFields.push({ name: "Chat Messages", value: String(freshJob.totalChatMessages), inline: true });
    if (freshJob?.downloadStartedAt) {
      const elapsed = Date.now() - new Date(freshJob.downloadStartedAt).getTime();
      muxFields.push({ name: "Download Time", value: formatElapsed(elapsed), inline: true });
    }
    NotificationManager.getInstance().send(
      "Muxing Starting",
      `Download complete, muxing: ${job.title}`,
      NotificationType.MUXING,
      muxFields,
      {
        url: `https://www.youtube.com/watch?v=${job.videoId}`,
        thumbnail: job.thumbnailUrl,
        event: "muxing",
      },
    );
  }

  const template = config.downloader.output_template || "${title} [${id}]";
  // Use stream start time if available, otherwise fall back to job creation time
  const streamDate = job.streamStartTime
    ? new Date(job.streamStartTime)
    : new Date(job.createdAt);
  const filenameBase = ConfigManager.resolveTemplate(template, {
    title: job.title,
    id: job.id,
    channel: job.channelName || "Unknown",
    date: streamDate,
  });

  const finalFilename = `${filenameBase}${finalExtension}`;
  const finalPath = path.join(outputDir, finalFilename);

  try {
    await Muxer.mux(videoPath, audioPath, finalPath, signal, trimOptions);
    logger.info(`[DownloadOrchestrator] Muxing complete: ${finalPath}`);

    // Extract actual video metadata (duration, resolution, size)
    const metadata = await extractVideoMetadata(finalPath);
    if (metadata) {
      logger.info(
        `[Metadata] Extracted: ${metadata.duration}s, ${metadata.width}x${metadata.height}, ${(metadata.size / 1024 / 1024).toFixed(2)}MB`,
      );
    }

    // Save description and thumbnail
    if (job.description) {
      await assetDownloader.saveDescription(
        job.description,
        outputDir,
        filenameBase,
      );
    }

    if (job.thumbnailUrl) {
      await assetDownloader.downloadThumbnail(
        job.thumbnailUrl,
        outputDir,
        filenameBase,
      );
    }

    // Copy chat file if exists
    const chatStagingPath = path.join(stagingDir, "chat.json");
    let chatFilename: string | undefined;
    if (await fs.pathExists(chatStagingPath)) {
      chatFilename = `${filenameBase}.chat.json`;
      const chatOutputPath = path.join(outputDir, chatFilename);
      await fs.copy(chatStagingPath, chatOutputPath);
      logger.info(`[Chat] Saved chat to ${chatOutputPath}`);
    }

    // Get file size
    let fileSize: number | undefined;
    try {
      const stats = await fs.stat(finalPath);
      fileSize = stats.size;
    } catch {}

    await db.updateJob(job.id, {
      status: "Finished",
      filename: finalFilename,
      progress: "",
      percent: 100,
      speed: "",
      eta: "",
      chatStatus: chatFilename ? "finished" : job.chatStatus,
      chatFilename,
      fileSize: metadata?.size || fileSize,
      // Update with actual metadata from muxed file
      lengthSeconds: metadata?.duration,
      videoWidth: metadata?.width,
      videoHeight: metadata?.height,
    });

    // Build rich "Download Finished" notification
    const finishedJob = (await db.getJobs()).find(j => j.id === job.id);
    const finFields: { name: string; value: string; inline?: boolean }[] = [];
    finFields.push({ name: "File", value: finalFilename, inline: false });

    if (job.videoWidth && job.videoHeight) {
      const res = `${job.videoWidth}x${job.videoHeight}${job.videoFps ? ` @${job.videoFps}fps` : ""}`;
      finFields.push({ name: "Resolution", value: res, inline: true });
    }

    if (fileSize) {
      const sizeStr = fileSize > 1024 ** 3
        ? `${(fileSize / 1024 ** 3).toFixed(2)} GB`
        : `${(fileSize / 1024 ** 2).toFixed(1)} MB`;
      finFields.push({ name: "File Size", value: sizeStr, inline: true });
    }

    if (finishedJob?.lengthSeconds && finishedJob.lengthSeconds > 0) {
      finFields.push({ name: "Duration", value: formatElapsed(finishedJob.lengthSeconds * 1000), inline: true });
    }

    if (finishedJob?.downloadStartedAt) {
      const elapsed = Date.now() - new Date(finishedJob.downloadStartedAt).getTime();
      finFields.push({ name: "Total Time", value: formatElapsed(elapsed), inline: true });
    }

    if (finishedJob?.lastVideoSeq) {
      finFields.push({
        name: "Segments",
        value: `V: ${finishedJob.lastVideoSeq}${finishedJob.lastAudioSeq ? ` A: ${finishedJob.lastAudioSeq}` : ""}`,
        inline: true,
      });
    }

    if (finishedJob?.totalChatMessages) {
      finFields.push({ name: "Chat Messages", value: String(finishedJob.totalChatMessages), inline: true });
    }

    // Add advanced options info if used
    if (job.selectedVideoItag != null || job.selectedAudioItag != null) {
      let formatInfo = "";
      if (job.selectedVideoItag != null) {
        formatInfo += job.selectedVideoItag === -1 ? "Video: None" : `Video: itag ${job.selectedVideoItag}`;
      }
      if (job.selectedAudioItag != null) {
        if (formatInfo) formatInfo += ", ";
        formatInfo += job.selectedAudioItag === -1 ? "Audio: None" : `Audio: itag ${job.selectedAudioItag}`;
      }
      finFields.push({ name: "Format Selection", value: formatInfo, inline: false });
    }

    if (job.startTime != null || job.endTime != null) {
      const startMin = Math.floor((job.startTime || 0) / 60);
      const startSec = (job.startTime || 0) % 60;
      const startStr = `${startMin}:${String(startSec).padStart(2, "0")}`;
      const endStr = job.endTime != null
        ? `${Math.floor(job.endTime / 60)}:${String(job.endTime % 60).padStart(2, "0")}`
        : "end";
      finFields.push({ name: "Trimmed Range", value: `${startStr} - ${endStr}`, inline: true });
    }

    if (job.description) {
      const desc = job.description.length > 300 ? job.description.substring(0, 297) + "..." : job.description;
      finFields.push({ name: "Description", value: desc, inline: false });
    }

    NotificationManager.getInstance().send(
      "Download Finished",
      `Successfully archived: ${job.title}`,
      NotificationType.SUCCESS,
      finFields,
      {
        url: `https://www.youtube.com/watch?v=${job.videoId}`,
        image: job.thumbnailUrl,
        event: "finished",
      },
    );

    // Cleanup staging
    await fs.remove(stagingDir);
  } catch (e: any) {
    // Don't mark as error if cancelled — the main execute() handler deals with that
    if (e?.name === "AbortError") {
      await fs.remove(finalPath).catch((e: any) => logger.debug(`[DownloadOrchestrator] Cleanup failed: ${e.message}`));
      throw e;
    }
    logger.error(
      `[DownloadOrchestrator] Muxing failed: ${getErrorMessage(e)}`,
    );
    await db.updateJob(job.id, { status: "Error", error: "Muxing Failed" });
    throw e;
  }
}

/**
 * Download a file with chunked range requests
 */
export async function downloadFile(
  url: string,
  outputPath: string,
  _activeSegmentDownloaders: Set<SegmentDownloader>,
  signal?: AbortSignal,
  onProgress?: (progress: string, percent?: number) => void,
): Promise<void> {
  const logger = Logger.getInstance();
  const headers: Record<string, string> = {
    "User-Agent": USER_AGENTS.ANDROID,
  };

  // Get file size
  let totalBytes = 0;
  try {
    const probeResponse = await fetch(url, {
      method: "GET",
      headers: { ...headers, Range: "bytes=0-0" },
      signal,
    });

    if (probeResponse.status === 206) {
      const contentRange = probeResponse.headers.get("content-range");
      if (contentRange) {
        const match = contentRange.match(/bytes \d+-\d+\/(\d+)/);
        if (match) {
          totalBytes = parseInt(match[1], 10);
        }
      }
    }
    await probeResponse.arrayBuffer();
  } catch (e: any) {
    if (e?.name === "AbortError" || signal?.aborted) return;
    logger.debug(
      "[DownloadOrchestrator] Size probe failed, downloading without size info",
    );
  }

  if (signal?.aborted) return;

  // Download in chunks
  let downloadedBytes = 0;
  let lastProgressUpdate = 0;
  const fileHandle = await fsOpen(outputPath, "w");

  try {
    while (true) {
      if (signal?.aborted) break;

      const start = downloadedBytes;
      const end =
        totalBytes > 0
          ? Math.min(start + DOWNLOAD_CHUNK_SIZE - 1, totalBytes - 1)
          : start + DOWNLOAD_CHUNK_SIZE - 1;

      let response!: Response;
      for (let chunkAttempt = 0; ; chunkAttempt++) {
        try {
          response = await fetch(url, {
            headers: { ...headers, Range: `bytes=${start}-${end}` },
            signal,
          });
          if (response.status >= 500) {
            await response.arrayBuffer();
            throw new Error(`HTTP ${response.status}: ${response.statusText}`);
          }
          break;
        } catch (e: any) {
          if (e?.name === "AbortError" || signal?.aborted) throw e;
          if (chunkAttempt >= 2) throw e;
          logger.debug(
            `[DownloadOrchestrator] Chunk retry ${chunkAttempt + 1}/3 for bytes ${start}-${end}`,
          );
          await new Promise((r) => setTimeout(r, 1000 * (chunkAttempt + 1)));
        }
      }

      if (response.status === 416) {
        await response.arrayBuffer();
        break;
      }

      if (!response.ok && response.status !== 206) {
        await response.arrayBuffer();
        throw new Error(`HTTP ${response.status}: ${response.statusText}`);
      }

      const data = await response.arrayBuffer();
      const buffer = Buffer.from(data);

      if (buffer.length === 0) break;

      await fileHandle.write(buffer, 0, buffer.length, downloadedBytes);
      downloadedBytes += buffer.length;

      const now = Date.now();
      if (onProgress && now - lastProgressUpdate > 500) {
        lastProgressUpdate = now;
        if (totalBytes > 0) {
          const percent = (downloadedBytes / totalBytes) * 100;
          onProgress(
            `${percent.toFixed(1)}% (${formatBytes(downloadedBytes)} / ${formatBytes(totalBytes)})`,
            percent,
          );
        } else {
          onProgress(formatBytes(downloadedBytes));
        }
      }

      if (response.status === 200) break;
      if (totalBytes > 0 && downloadedBytes >= totalBytes) break;
    }
  } catch (e: any) {
    if (e?.name === "AbortError" || signal?.aborted) {
      logger.debug(`[DownloadOrchestrator] Download aborted for ${outputPath}`);
      return;
    }
    throw e;
  } finally {
    await fileHandle.close();
  }

  logger.debug(
    `[DownloadOrchestrator] Downloaded ${formatBytes(downloadedBytes)} to ${outputPath}`,
  );
}
