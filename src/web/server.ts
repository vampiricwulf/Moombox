/**
 * Web Server for Moombox Dashboard
 *
 * Provides HTTP server with WebSocket support for real-time updates.
 */

import express from "express";
import { createServer } from "http";
import net from "net";
import { WebSocketServer, WebSocket } from "ws";
import path from "path";
import fs from "fs";
import fsExtra from "fs-extra";
import { randomBytes } from "crypto";
import { fileURLToPath } from "url";
import { spawn } from "child_process";
import AdmZip from "adm-zip";
import { Logger } from "../core/logger.js";
import { Database } from "../core/database.js";
import { NotificationManager, NotificationType } from "../core/notifications.js";
import { ConfigManager } from "../core/config.js";
import { YouTubeService } from "../engine/youtube/index.js";
import { CookieRefreshService } from "../core/cookieRefresh.js";
import { CookieJar } from "../core/cookies.js";
import { PotProvider } from "../core/potProvider.js";
import type { Job } from "../types/jobs.js";
import { getYtdlpPluginStatus, installYtdlpPlugin } from "../core/ytdlpPlugin.js";
import { AutoCookieService } from "../core/autoCookies.js";

// POT Provider version for compatibility
const POT_PROVIDER_VERSION = "1.0.0";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

export interface WebServerOptions {
  port?: number;
  openBrowser?: boolean;
  allowLan?: boolean;
  allowExternal?: boolean;
}

// Dashboard port
const DEFAULT_PORT = 774;

interface WSMessage {
  type: string;
  payload?: any;
}

export class WebServer {
  private app: express.Application;
  private server: ReturnType<typeof createServer>;
  private wss: WebSocketServer;
  private port: number;
  private allowLan: boolean;
  private allowExternal: boolean;
  private logger: Logger;
  private clients: Set<WebSocket> = new Set();
  private logBuffer: string[] = [];
  private jobLogs: Map<string, string[]> = new Map();
  private knownJobIds: Set<string> = new Set();
  private loggerUnsubscribe: (() => void) | null = null;
  private jobUpdateUnsubscribe: (() => void) | null = null;
  private jobsChangeUnsubscribe: (() => void) | null = null;
  private jobTrailingTimers: Map<string, NodeJS.Timeout> = new Map();

  constructor(options: WebServerOptions = {}) {
    this.port = options.port || DEFAULT_PORT;
    this.allowLan = options.allowLan ?? false;
    this.allowExternal = this.allowLan && (options.allowExternal ?? false);
    this.logger = Logger.getInstance();
    this.app = express();
    this.server = createServer(this.app);
    this.wss = new WebSocketServer({
      server: this.server,
      verifyClient: (info: { req: { socket: { remoteAddress?: string } } }) =>
        this.isAllowedClient(info.req.socket.remoteAddress),
    });

    this.setupMiddleware();
    this.setupRoutes();
    this.setupWebSocket();
    this.setupLogSubscription();
  }

  /**
   * Check whether an IP address is a private/LAN address.
   */
  private static isPrivateIP(ip: string): boolean {
    return (
      ip === "127.0.0.1" ||
      ip === "::1" ||
      ip === "::ffff:127.0.0.1" ||
      ip.startsWith("192.168.") ||
      ip.startsWith("::ffff:192.168.") ||
      ip.startsWith("10.") ||
      ip.startsWith("::ffff:10.") ||
      /^(::ffff:)?172\.(1[6-9]|2\d|3[01])\./.test(ip)
    );
  }

  private static isLoopback(ip: string): boolean {
    return ip === "127.0.0.1" || ip === "::1" || ip === "::ffff:127.0.0.1";
  }

  /**
   * Check whether a connecting client IP is allowed based on access config.
   */
  private isAllowedClient(ip: string | undefined): boolean {
    if (!ip) return false;
    if (WebServer.isLoopback(ip)) return true;
    if (this.allowExternal) return true;
    if (this.allowLan) return WebServer.isPrivateIP(ip);
    return false;
  }

  private setupMiddleware() {
    // Gate ALL requests by client IP based on network_access config
    this.app.use((req, res, next) => {
      if (!this.isAllowedClient(req.socket.remoteAddress)) {
        return res.status(403).send("Forbidden");
      }
      next();
    });

    this.app.use(express.json());

    // Check for embedded assets (SEA mode)
    const embeddedAssets = (globalThis as any).__MOOMBOX_ASSETS__;
    if (embeddedAssets) {
      this.logger.debug("[WebServer] Using embedded assets (SEA mode)");

      // Serve embedded assets
      this.app.use((req, res, next) => {
        // Remove leading slash and normalize path
        let assetPath = path.posix.normalize(req.path).replace(/^\/+/, "") || "index.html";

        // Reject traversal attempts
        if (assetPath.includes("..") || assetPath.startsWith("/")) {
          return res.status(400).send("Bad request");
        }

        // Check if asset exists
        if (embeddedAssets[assetPath]) {
          const content = embeddedAssets[assetPath];

          // Set content type based on extension
          const ext = path.extname(assetPath).toLowerCase();
          const contentTypes: Record<string, string> = {
            ".html": "text/html",
            ".css": "text/css",
            ".js": "application/javascript",
            ".json": "application/json",
            ".png": "image/png",
            ".jpg": "image/jpeg",
            ".svg": "image/svg+xml",
            ".ico": "image/x-icon",
          };

          res.setHeader(
            "Content-Type",
            contentTypes[ext] || "application/octet-stream",
          );
          return res.send(content);
        }

        next();
      });
    } else {
      // Serve static files from public directory
      // Try multiple paths to support both development and compiled versions
      const possiblePaths = [
        path.join(__dirname, "public"), // dist/web/public
        path.join(__dirname, "..", "..", "src", "web", "public"), // from dist, look in src
        path.join(process.cwd(), "src", "web", "public"), // cwd/src/web/public
        path.join(process.cwd(), "bundle", "public"), // bundle/public (pkg mode)
      ];

      for (const publicPath of possiblePaths) {
        if (fs.existsSync(publicPath)) {
          this.app.use(express.static(publicPath));
          break;
        }
      }
    }
  }

  private setupRoutes() {
    // API Routes
    const api = express.Router();

    // CSRF protection for mutating requests — verify Origin header matches a trusted source
    // (The IP-level gate in setupMiddleware already blocks disallowed clients,
    //  but Origin checks guard against cross-site requests from allowed IPs.)
    api.use((req, res, next) => {
      if (req.method !== "GET" && req.method !== "HEAD") {
        const origin = req.headers.origin || req.headers.referer;
        if (origin) {
          try {
            const h = new URL(origin as string).hostname;
            const trusted =
              WebServer.isLoopback(h) ||
              h === "localhost" ||
              (this.allowLan && WebServer.isPrivateIP(h)) ||
              this.allowExternal;
            if (!trusted) {
              return res.status(403).json({ error: "Forbidden: invalid origin" });
            }
          } catch {
            return res.status(403).json({ error: "Forbidden: invalid origin" });
          }
        } else {
          // No Origin/Referer — reject to guard against CSRF edge cases.
          // All browser-initiated requests include Origin for non-GET methods.
          return res.status(403).json({ error: "Forbidden: missing origin" });
        }
      }
      next();
    });

    // Get all jobs
    api.get("/jobs", async (req, res) => {
      try {
        const db = await Database.getInstance();
        const jobs = await db.getJobs();
        res.json(this.filterJobsByAge(jobs, false));
      } catch (error: any) {
        this.logger.error(`[WebServer] GET /api/jobs failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Get archived jobs (finished jobs older than hide_finished_age_days)
    api.get("/jobs/archived", async (req, res) => {
      try {
        const db = await Database.getInstance();
        const jobs = await db.getJobs();
        const archivedJobs = this.filterJobsByAge(jobs, true)
          .sort((a, b) => new Date(b.updatedAt).getTime() - new Date(a.updatedAt).getTime());
        res.json(archivedJobs);
      } catch (error: any) {
        this.logger.error(`[WebServer] GET /api/jobs/archived failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Get single job
    api.get("/jobs/:id", async (req, res) => {
      try {
        const db = await Database.getInstance();
        const job = await db.getJob(req.params.id);
        if (!job) {
          return res.status(404).json({ error: "Job not found" });
        }
        res.json(job);
      } catch (error: any) {
        this.logger.error(`[WebServer] GET /api/jobs/:id failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Get logs for a job
    api.get("/jobs/:id/logs", (req, res) => {
      const logs = this.jobLogs.get(req.params.id) || [];
      res.json(logs);
    });

    // Stream video file for a job
    api.get("/jobs/:id/video", async (req, res) => {
      try {
        const db = await Database.getInstance();
        const config = ConfigManager.getInstance().get();
        const job = await db.getJob(req.params.id);

        if (!job) {
          return res.status(404).json({ error: "Job not found" });
        }

        if (!job.filename) {
          return res.status(404).json({ error: "No video file available" });
        }

        const outputDir =
          job.outputDirectory ||
          config.downloader?.output_directory ||
          "./output";
        const filePath = path.resolve(path.join(outputDir, job.filename));

        // Prevent path traversal
        const resolvedOutputDir = path.resolve(outputDir);
        if (!filePath.startsWith(resolvedOutputDir + path.sep)) {
          return res.status(403).json({ error: "Access denied" });
        }

        if (!fs.existsSync(filePath)) {
          return res.status(404).json({ error: "Video file not found" });
        }

        const stat = fs.statSync(filePath);
        const fileSize = stat.size;

        const range = req.headers.range;
        if (range) {
          const parts = range.replace(/bytes=/, "").split("-");
          const start = parseInt(parts[0], 10);
          const end = parts[1] ? parseInt(parts[1], 10) : fileSize - 1;

          if (start >= fileSize || end >= fileSize || start > end) {
            res.status(416).setHeader("Content-Range", `bytes */${fileSize}`);
            return res.end();
          }

          res.status(206);
          res.setHeader("Content-Range", `bytes ${start}-${end}/${fileSize}`);
          res.setHeader("Accept-Ranges", "bytes");
          res.setHeader("Content-Length", end - start + 1);
          res.setHeader("Content-Type", "video/mp4");
          fs.createReadStream(filePath, { start, end }).pipe(res);
        } else {
          res.setHeader("Content-Length", fileSize);
          res.setHeader("Content-Type", "video/mp4");
          res.setHeader("Accept-Ranges", "bytes");
          fs.createReadStream(filePath).pipe(res);
        }
      } catch (error: any) {
        this.logger.error(`[WebServer] GET /api/jobs/:id/video failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Get chat data for a job
    api.get("/jobs/:id/chat", async (req, res) => {
      try {
        const db = await Database.getInstance();
        const config = ConfigManager.getInstance().get();
        const job = await db.getJob(req.params.id);

        if (!job) {
          return res.status(404).json({ error: "Job not found" });
        }

        if (!job.chatFilename) {
          return res.status(404).json({ error: "No chat data available" });
        }

        const outputDir =
          job.outputDirectory ||
          config.downloader?.output_directory ||
          "./output";
        const chatPath = path.resolve(path.join(outputDir, job.chatFilename));

        // Prevent path traversal
        const resolvedOutputDir = path.resolve(outputDir);
        if (!chatPath.startsWith(resolvedOutputDir + path.sep)) {
          return res.status(403).json({ error: "Access denied" });
        }

        if (!fs.existsSync(chatPath)) {
          return res.status(404).json({ error: "Chat file not found" });
        }

        const chatRaw = await fsExtra.readFile(chatPath, "utf-8");
        const chatData = JSON.parse(chatRaw);
        res.json(chatData);
      } catch (error: any) {
        this.logger.error(`[WebServer] GET /api/jobs/:id/chat failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Add new job
    api.post("/jobs", async (req, res) => {
      try {
        const { videoId } = req.body;
        if (!videoId) {
          return res.status(400).json({ error: "videoId is required" });
        }
        if (!/^[a-zA-Z0-9_-]{11}$/.test(videoId)) {
          return res.status(400).json({ error: "Invalid videoId format" });
        }

        const db = await Database.getInstance();

        const exists = await db.jobExists(videoId);
        if (exists) {
          return res.status(409).json({ error: "Job already exists" });
        }

        // Fetch metadata
        let title = "Manual Add";
        let channelName = "Manual";

        try {
          const yt = YouTubeService.getInstance();
          await yt.init();
          const metadata = await yt.getMetadata(videoId);
          title = metadata.title || title;
          channelName = metadata.channelName || channelName;
        } catch (e) {
          this.logger.warn(`Could not fetch metadata for ${videoId}`);
        }

        const job = await db.addJob({
          videoId,
          url: `https://www.youtube.com/watch?v=${videoId}`,
          title,
          channelName,
          thumbnailUrl: `https://i.ytimg.com/vi/${videoId}/maxresdefault.jpg`,
          manuallyAdded: true,
        });

        if (job) {
          this.logger.info(`Added job: ${title} (${videoId})`);
          this.broadcast({ type: "job_added", payload: job });

          NotificationManager.getInstance().send(
            "Video Added",
            `Manually added: ${title}`,
            NotificationType.INFO,
            [
              { name: "Channel", value: channelName, inline: true },
              { name: "Video ID", value: videoId, inline: true },
            ],
            { url: `https://www.youtube.com/watch?v=${videoId}`, thumbnail: job.thumbnailUrl, event: "added" },
          );

          res.status(201).json(job);
        } else {
          res.status(500).json({ error: "Failed to create job" });
        }
      } catch (error: any) {
        this.logger.error(`[WebServer] POST /api/jobs failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Cancel job
    api.post("/jobs/:id/cancel", async (req, res) => {
      try {
        const db = await Database.getInstance();
        const job = await db.getJob(req.params.id);

        if (!job) {
          return res.status(404).json({ error: "Job not found" });
        }

        if (
          !["Downloading", "Live", "Upcoming", "Muxing", "COOKIES?"].includes(
            job.status,
          )
        ) {
          return res
            .status(400)
            .json({ error: "Job cannot be cancelled in current state" });
        }

        await db.updateJob(job.id, { status: "Cancelled" });
        this.logger.info(`Cancelled job: ${job.title}`);

        NotificationManager.getInstance().send(
          "Job Cancelled",
          `Cancelled: ${job.title}`,
          NotificationType.CANCELLED,
          [
            { name: "Channel", value: job.channelName, inline: true },
            { name: "Video ID", value: job.videoId, inline: true },
          ],
          {
            url: `https://www.youtube.com/watch?v=${job.videoId}`,
            thumbnail: job.thumbnailUrl,
            event: "cancelled",
          },
        );

        res.json({ success: true });
      } catch (error: any) {
        this.logger.error(`[WebServer] POST /api/jobs/:id/cancel failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Retry job
    api.post("/jobs/:id/retry", async (req, res) => {
      try {
        const db = await Database.getInstance();
        const job = await db.getJob(req.params.id);

        if (!job) {
          return res.status(404).json({ error: "Job not found" });
        }

        if (!["Error", "Cancelled", "COOKIES?"].includes(job.status)) {
          return res
            .status(400)
            .json({ error: "Job cannot be retried in current state" });
        }

        await db.updateJob(job.id, {
          status: "Upcoming",
          error: undefined,
          progress: "",
        });
        this.logger.info(`Retrying job: ${job.title}`);
        res.json({ success: true });
      } catch (error: any) {
        this.logger.error(`[WebServer] POST /api/jobs/:id/retry failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Delete job
    api.delete("/jobs/:id", async (req, res) => {
      try {
        const db = await Database.getInstance();
        const job = await db.getJob(req.params.id);

        if (!job) {
          return res.status(404).json({ error: "Job not found" });
        }

        if (
          !["Finished", "Error", "Cancelled", "COOKIES?"].includes(job.status)
        ) {
          return res
            .status(400)
            .json({ error: "Job cannot be deleted in current state" });
        }

        await db.deleteJob(job.id);
        this.jobLogs.delete(job.id);
        this.logger.info(`Deleted job: ${job.title}`);
        res.json({ success: true });
      } catch (error: any) {
        this.logger.error(`[WebServer] DELETE /api/jobs/:id failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Import zip archive
    api.post(
      "/import",
      express.raw({ type: "application/octet-stream", limit: "4gb" }),
      async (req, res) => {
        const tempFiles: string[] = [];
        try {
          const config = ConfigManager.getInstance().get();
          const outputDir = config.downloader?.output_directory || "./output";
          const stagingDir = config.downloader?.staging_directory || "./staging";

          // Write request body to temp file
          const tempPath = path.join(stagingDir, `import-${Date.now()}.zip`);
          await fsExtra.ensureDir(stagingDir);
          fs.writeFileSync(tempPath, req.body as Buffer);
          tempFiles.push(tempPath);

          // Open zip
          let zip: AdmZip;
          try {
            zip = new AdmZip(tempPath);
          } catch (e) {
            return res.status(400).json({ error: "Invalid zip file" });
          }

          const entries = zip.getEntries();

          // Zip bomb check: limit total uncompressed size to 50GB
          const MAX_UNCOMPRESSED = 50 * 1024 * 1024 * 1024;
          let totalUncompressed = 0;
          for (const entry of entries) {
            totalUncompressed += entry.header.size;
            if (totalUncompressed > MAX_UNCOMPRESSED) {
              return res
                .status(400)
                .json({ error: "Zip file too large (uncompressed)" });
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
          if (!chatEntry) {
            for (const entry of entries) {
              if (entry.isDirectory) continue;
              if (!entry.entryName.toLowerCase().endsWith(".json")) continue;
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
          const sanitize = (s: string) =>
            s.replace(/[<>:"/\\|?*]/g, "_").replace(/\s+/g, " ").trim();
          const safeName = sanitize(title);
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

          this.logger.info(`[Import] Imported: ${title} (${videoId})`);

          // Fetch the updated job to return
          const updatedJob = await db.getJob(job.id);
          res.status(201).json(updatedJob);
        } catch (error: any) {
          this.logger.error(`[Import] Failed: ${error.stack || error.message}`);
          res.status(500).json({ error: error.message });
        } finally {
          // Clean up temp files
          for (const f of tempFiles) {
            try {
              fs.unlinkSync(f);
            } catch {}
          }
        }
      },
    );

    // Open folder (for finished jobs)
    api.post("/jobs/:id/open-folder", async (req, res) => {
      try {
        // Only allow from localhost — opening a folder on the server makes no
        // sense for remote clients.
        if (!req.ip || !WebServer.isLoopback(req.ip)) {
          return res.status(403).json({ error: "Only available from localhost" });
        }

        const db = await Database.getInstance();
        const config = ConfigManager.getInstance().get();
        const job = await db.getJob(req.params.id);

        if (!job || !job.filename) {
          return res.status(404).json({ error: "Job or file not found" });
        }

        const outputDir =
          job.outputDirectory ||
          config.downloader?.output_directory ||
          "./output";
        const filePath = path.resolve(path.join(outputDir, job.filename));
        const resolvedOutputDir = path.resolve(outputDir);
        if (!filePath.startsWith(resolvedOutputDir + path.sep)) {
          return res.status(403).json({ error: "Access denied" });
        }
        const folderPath = path.dirname(filePath);

        const program =
          process.platform === "win32"
            ? "explorer"
            : process.platform === "darwin"
              ? "open"
              : "xdg-open";

        const child = spawn(program, [folderPath], {
          stdio: "ignore",
        });
        child.unref();
        res.json({ success: true });
      } catch (error: any) {
        this.logger.error(`[WebServer] POST /api/jobs/:id/open-folder failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // open-url removed — browser uses window.open() directly

    // Get recent logs
    api.get("/logs", (req, res) => {
      res.json(this.logBuffer);
    });

    // Get status
    api.get("/status", async (req, res) => {
      const config = ConfigManager.getInstance().get();

      // Check cookie status by directly loading and parsing the cookie file
      let cookieStatus = { found: false, authenticated: false };
      try {
        const cookieFile = config.downloader?.cookie_file;
        if (cookieFile && fs.existsSync(cookieFile)) {
          cookieStatus.found = true;
          // Load cookies fresh to check authentication
          await CookieJar.load(cookieFile, true);
          cookieStatus.authenticated = CookieJar.hasAuthCookies();
        }
      } catch (e) {
        // Ignore errors
      }

      res.json({
        status: "running",
        uptime: process.uptime(),
        timestamp: new Date().toISOString(),
        cookieStatus,
      });
    });

    // Get config
    api.get("/config", (req, res) => {
      try {
        const config = ConfigManager.getInstance().get();
        res.json(config);
      } catch (error: any) {
        this.logger.error(`[WebServer] GET /api/config failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Update config
    api.put("/config", async (req, res) => {
      try {
        const configManager = ConfigManager.getInstance();
        const newConfig = req.body;

        // Validate required fields
        if (!newConfig.downloader || typeof newConfig.downloader !== "object") {
          return res
            .status(400)
            .json({ error: "downloader configuration is required" });
        }

        // Validate known top-level keys only
        const allowedKeys = new Set([
          "port", "network_access",
          "log_level", "log_file_path", "log_max_file_size", "log_max_files",
          "database_path", "max_feed_items", "feed_check_interval",
          "downloader", "tasklist",
          "auto_cookies", "notifications", "channels",
        ]);
        for (const key of Object.keys(newConfig)) {
          if (!allowedKeys.has(key)) {
            return res.status(400).json({ error: `Unknown config key: ${key}` });
          }
        }

        await configManager.save(newConfig);
        // Apply runtime-reloadable settings immediately
        this.logger.refreshLogLevel();
        res.json({ success: true });
      } catch (error: any) {
        this.logger.error(`[WebServer] PUT /api/config failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Add channel
    api.post("/config/channels", async (req, res) => {
      try {
        const configManager = ConfigManager.getInstance();
        const config = configManager.get();
        const channel = req.body;

        if (!channel.id) {
          return res.status(400).json({ error: "Channel ID is required" });
        }

        if (!config.channels) {
          config.channels = [];
        }

        // Check if channel already exists
        const existingIndex = config.channels.findIndex(
          (c) => c.id === channel.id,
        );
        if (existingIndex >= 0) {
          config.channels[existingIndex] = channel;
        } else {
          config.channels.push(channel);
        }

        await configManager.save(config);
        res.json({ success: true, channel });
      } catch (error: any) {
        this.logger.error(`[WebServer] POST /api/config/channels failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Delete channel
    api.delete("/config/channels/:id", async (req, res) => {
      try {
        const configManager = ConfigManager.getInstance();
        const config = configManager.get();
        const channelId = req.params.id;

        if (!config.channels) {
          return res.status(404).json({ error: "Channel not found" });
        }

        const index = config.channels.findIndex((c) => c.id === channelId);
        if (index < 0) {
          return res.status(404).json({ error: "Channel not found" });
        }

        config.channels.splice(index, 1);
        await configManager.save(config);
        res.json({ success: true });
      } catch (error: any) {
        this.logger.error(`[WebServer] DELETE /api/config/channels/:id failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Check if first run (no config exists)
    api.get("/setup/status", (req, res) => {
      const configManager = ConfigManager.getInstance();
      const isFirstRun = !configManager.hasConfig();
      res.json({ isFirstRun });
    });

    // Complete first-run setup
    api.post("/setup/complete", async (req, res) => {
      try {
        const configManager = ConfigManager.getInstance();
        const config = req.body;

        // Validate required fields
        if (!config.downloader) {
          return res
            .status(400)
            .json({ error: "downloader configuration is required" });
        }

        // Save the configuration
        await configManager.save(config);

        this.logger.info("[Setup] First-run setup completed");
        res.json({ success: true });
      } catch (error: any) {
        this.logger.error(`[Setup] Failed to complete setup: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Recheck cookies
    api.post("/cookies/recheck", async (req, res) => {
      try {
        const cookieService = CookieRefreshService.getInstance();
        const success = await cookieService.refresh();

        // Get updated status by directly loading and parsing the cookie file
        const config = ConfigManager.getInstance().get();
        let cookieStatus = { found: false, authenticated: false };

        const cookieFile = config.downloader?.cookie_file;
        if (cookieFile && fs.existsSync(cookieFile)) {
          cookieStatus.found = true;
          // Load cookies fresh to check authentication
          await CookieJar.load(cookieFile, true);
          cookieStatus.authenticated = CookieJar.hasAuthCookies();
        }

        res.json({
          success,
          cookieStatus,
        });
      } catch (error: any) {
        this.logger.error(`[WebServer] POST /api/cookies/recheck failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // ===== Auto Cookie Endpoints =====

    // Get auto-cookie status
    api.get("/cookies/auto-status", (req, res) => {
      try {
        res.json(AutoCookieService.getInstance().getStatus());
      } catch (error: any) {
        this.logger.error(`[WebServer] GET /api/cookies/auto-status failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Start auto-cookie setup (launches browser)
    api.post("/cookies/auto-setup/start", async (req, res) => {
      try {
        const result = await AutoCookieService.getInstance().startSetup();
        res.json(result);
      } catch (error: any) {
        this.logger.error(`[WebServer] POST /api/cookies/auto-setup/start failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Finish auto-cookie setup (extract cookies)
    api.post("/cookies/auto-setup/finish", async (req, res) => {
      try {
        const result = await AutoCookieService.getInstance().finishSetup();
        res.json(result);
      } catch (error: any) {
        this.logger.error(`[WebServer] POST /api/cookies/auto-setup/finish failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Cancel auto-cookie setup
    api.post("/cookies/auto-setup/cancel", async (req, res) => {
      try {
        await AutoCookieService.getInstance().cancelSetup();
        res.json({ success: true });
      } catch (error: any) {
        this.logger.error(`[WebServer] POST /api/cookies/auto-setup/cancel failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // ===== yt-dlp Plugin Endpoints =====

    // Get yt-dlp plugin install status
    api.get("/ytdlp-plugin/status", (req, res) => {
      try {
        const config = ConfigManager.getInstance().get();
        const currentPort = config.port || 774;
        res.json(getYtdlpPluginStatus(currentPort));
      } catch (error: any) {
        this.logger.error(`[WebServer] GET /api/ytdlp-plugin/status failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Install yt-dlp plugin
    api.post("/ytdlp-plugin/install", async (req, res) => {
      try {
        const body = req.body || {};
        const force = body.force === true;
        const config = ConfigManager.getInstance().get();
        const currentPort = config.port || 774;

        const result = await installYtdlpPlugin(currentPort, force);

        if (!result.success) {
          return res.status(500).json({ error: result.error });
        }

        if (!result.alreadyInstalled) {
          this.logger.info(
            `[yt-dlp Plugin] Installed to ${result.path} (port ${currentPort})`,
          );
        }

        res.json(result);
      } catch (error: any) {
        this.logger.error(
          `[yt-dlp Plugin] Install failed: ${error.stack || error.message}`,
        );
        res.status(500).json({ error: error.message });
      }
    });

    // ===== End yt-dlp Plugin Endpoints =====

    this.app.use("/api", api);

    // ===== POT Provider Endpoints (bgutil-ytdlp-pot-provider compatible) =====
    // These endpoints allow yt-dlp to use Moombox as a POT provider

    // Generate PO Token - main endpoint for yt-dlp
    this.app.post("/get_pot", async (req, res) => {
      const body = req.body || {};

      // Handle deprecated parameters
      if (body.data_sync_id) {
        this.logger.warn(
          "[PotProvider] Rejected request using deprecated data_sync_id",
        );
        return res.status(400).json({
          error: "data_sync_id is deprecated, use content_binding instead",
        });
      }
      if (body.visitor_data) {
        this.logger.warn(
          "[PotProvider] Rejected request using deprecated visitor_data",
        );
        return res.status(400).json({
          error: "visitor_data is deprecated, use content_binding instead",
        });
      }

      const contentBinding: string | undefined = body.content_binding;
      const bypassCache: boolean = body.bypass_cache || false;

      this.logger.info(
        `[PotProvider] POT requested${contentBinding ? ` for ${contentBinding.substring(0, 20)}...` : " (no binding)"}${bypassCache ? " (bypass cache)" : ""}`,
      );

      try {
        const potProvider = PotProvider.getInstance();
        const sessionData = await potProvider.generatePoToken(
          contentBinding,
          bypassCache,
        );

        this.logger.info(
          `[PotProvider] POT generated: ${sessionData.poToken.substring(0, 30)}...`,
        );
        res.json(sessionData);
      } catch (error: any) {
        this.logger.error(`[PotProvider] Generation failed: ${error.stack || error.message}`);
        res.status(500).json({ error: error.message });
      }
    });

    // Invalidate all caches
    this.app.post("/invalidate_caches", (req, res) => {
      this.logger.info("[PotProvider] Cache invalidation requested");
      const potProvider = PotProvider.getInstance();
      potProvider.invalidateCaches();
      res.status(204).send();
    });

    // Invalidate integrity tokens
    this.app.post("/invalidate_it", (req, res) => {
      this.logger.info("[PotProvider] Integrity token invalidation requested");
      const potProvider = PotProvider.getInstance();
      potProvider.invalidateIntegrityTokens();
      res.status(204).send();
    });

    // Ping endpoint for health checks
    this.app.get("/ping", (req, res) => {
      this.logger.debug("[PotProvider] Ping received");
      res.json({
        server_uptime: process.uptime(),
        version: POT_PROVIDER_VERSION,
      });
    });

    // Debug endpoint to view minter cache
    this.app.get("/minter_cache", (req, res) => {
      this.logger.debug("[PotProvider] Minter cache requested");
      const potProvider = PotProvider.getInstance();
      const cache = potProvider.getMinterCache();
      res.json(Array.from(cache.keys()));
    });

    // ===== End POT Provider Endpoints =====

    // Serve index.html for all other routes (SPA)
    this.app.get("/{*splat}", (req, res) => {
      // Check for embedded assets (SEA mode)
      const embeddedAssets = (globalThis as any).__MOOMBOX_ASSETS__;
      if (embeddedAssets && embeddedAssets["index.html"]) {
        res.setHeader("Content-Type", "text/html");
        return res.send(embeddedAssets["index.html"]);
      }

      // Try multiple paths
      const possiblePaths = [
        path.join(__dirname, "public", "index.html"),
        path.join(__dirname, "..", "..", "src", "web", "public", "index.html"),
        path.join(process.cwd(), "src", "web", "public", "index.html"),
        path.join(process.cwd(), "bundle", "public", "index.html"),
      ];

      for (const indexPath of possiblePaths) {
        try {
          if (fs.existsSync(indexPath)) {
            return res.sendFile(indexPath);
          }
        } catch {}
      }

      res.status(404).send("Dashboard not found");
    });
  }

  private setupWebSocket() {
    this.wss.on("connection", (ws) => {
      this.clients.add(ws);
      this.logger.debug("[WebServer] Client connected");

      // Send current state
      this.sendInitialState(ws);

      ws.on("message", (data) => {
        try {
          const message: WSMessage = JSON.parse(data.toString());
          this.handleWSMessage(ws, message);
        } catch (e) {
          this.logger.debug("[WebServer] Invalid WebSocket message");
        }
      });

      ws.on("close", () => {
        this.clients.delete(ws);
        this.logger.debug("[WebServer] Client disconnected");
      });
    });
  }

  private async sendInitialState(ws: WebSocket) {
    try {
      const db = await Database.getInstance();
      const jobs = await db.getJobs();

      ws.send(
        JSON.stringify({
          type: "initial_state",
          payload: {
            jobs: this.filterJobsByAge(jobs, false),
            logs: this.logBuffer,
          },
        }),
      );
    } catch (e) {
      this.logger.error(`[WebServer] Failed to send initial state: ${e}`);
    }
  }

  private handleWSMessage(ws: WebSocket, message: WSMessage) {
    // Handle any client-to-server WebSocket messages if needed
    switch (message.type) {
      case "ping":
        ws.send(JSON.stringify({ type: "pong" }));
        break;
    }
  }

  private setupLogSubscription() {
    const logger = Logger.getInstance();
    this.loggerUnsubscribe = logger.subscribe((msg) => {
      // Add to general log buffer
      this.logBuffer.push(msg);
      if (this.logBuffer.length > 200) {
        this.logBuffer.shift();
      }

      // Store in job-specific logs (match only known job IDs, not arbitrary 11-char words)
      for (const jobId of this.knownJobIds) {
        if (msg.includes(jobId)) {
          if (!this.jobLogs.has(jobId)) {
            this.jobLogs.set(jobId, []);
          }
          const logs = this.jobLogs.get(jobId)!;
          logs.push(msg);
          if (logs.length > 100) {
            logs.shift();
          }
          break; // Each log line belongs to at most one job
        }
      }

      // Broadcast to all clients
      this.broadcast({ type: "log", payload: msg });
    });

    // Subscribe to job updates for real-time broadcasting
    this.subscribeToJobUpdates();
  }

  private async subscribeToJobUpdates() {
    const db = await Database.getInstance();

    // Initialize known job IDs for log routing
    const jobs = await db.getJobs();
    for (const job of jobs) this.knownJobIds.add(job.id);

    // Throttle individual job updates to max ~10/second per job with trailing edge
    const jobUpdateTimestamps: Map<string, number> = new Map();
    const UPDATE_THROTTLE_MS = 100;

    // Subscribe to individual job updates (progress changes)
    this.jobUpdateUnsubscribe = db.onJobUpdate((job) => {
      const now = Date.now();
      const lastUpdate = jobUpdateTimestamps.get(job.id) || 0;

      if (now - lastUpdate >= UPDATE_THROTTLE_MS) {
        jobUpdateTimestamps.set(job.id, now);
        // Clear any pending trailing-edge timer since we're sending now
        const timer = this.jobTrailingTimers.get(job.id);
        if (timer) { clearTimeout(timer); this.jobTrailingTimers.delete(job.id); }
        this.broadcast({ type: "job_update", payload: job });
      } else {
        // Schedule trailing-edge send to ensure the last update is always delivered
        const existing = this.jobTrailingTimers.get(job.id);
        if (existing) clearTimeout(existing);
        this.jobTrailingTimers.set(job.id, setTimeout(() => {
          this.jobTrailingTimers.delete(job.id);
          jobUpdateTimestamps.set(job.id, Date.now());
          this.broadcast({ type: "job_update", payload: job });
        }, UPDATE_THROTTLE_MS));
      }
    });

    // Subscribe to jobs list changes (add/delete)
    this.jobsChangeUnsubscribe = db.onJobsChange((jobs) => {
      // Keep known job IDs in sync for log routing
      this.knownJobIds = new Set(jobs.map((j) => j.id));
      // Prune job logs for jobs no longer tracked
      for (const logId of this.jobLogs.keys()) {
        if (!this.knownJobIds.has(logId)) {
          this.jobLogs.delete(logId);
        }
      }
      this.broadcast({ type: "jobs_update", payload: this.filterJobsByAge(jobs, false) });
    });
  }

  /**
   * Filter jobs by finished age threshold.
   * Returns active jobs (non-archived) when archived=false, archived jobs when archived=true.
   */
  private filterJobsByAge(jobs: Job[], archived: boolean): Job[] {
    const config = ConfigManager.getInstance().get();
    const now = Date.now();
    const hideAgeMs = (config.tasklist?.hide_finished_age_days || 30) * 86400000;

    return jobs.filter((job) => {
      if (job.status !== "Finished") return !archived;
      const age = now - new Date(job.updatedAt).getTime();
      return archived ? age > hideAgeMs : age <= hideAgeMs;
    });
  }

  private broadcast(message: WSMessage) {
    const data = JSON.stringify(message);
    for (const client of this.clients) {
      if (client.readyState === WebSocket.OPEN) {
        client.send(data);
      }
    }
  }

  /**
   * Probe whether a port is free by briefly binding a TCP server.
   */
  private static isPortFree(port: number, host: string): Promise<boolean> {
    return new Promise((resolve) => {
      const tester = net.createServer();
      tester.once("error", () => resolve(false));
      tester.listen(port, host, () => {
        tester.close(() => resolve(true));
      });
    });
  }

  async start(openBrowser = true): Promise<number> {
    // Bind to localhost only unless LAN/external access is enabled
    const host = this.allowLan ? "0.0.0.0" : "127.0.0.1";

    // Probe the preferred port before binding the real server
    let targetPort = this.port;
    if (!(await WebServer.isPortFree(targetPort, host))) {
      this.logger.warn(
        `[WebServer] Port ${targetPort} is in use, probing nearby ports...`,
      );
      let found = false;
      for (let offset = 1; offset <= 10; offset++) {
        const candidate = targetPort + offset;
        if (await WebServer.isPortFree(candidate, host)) {
          this.logger.info(
            `[WebServer] Using port ${candidate} instead of ${targetPort}`,
          );
          targetPort = candidate;
          found = true;
          break;
        }
      }
      if (!found) {
        throw Object.assign(
          new Error(`Port ${this.port} (and nearby ports) already in use`),
          { code: "EADDRINUSE" },
        );
      }
    }

    // Now bind the actual server on the confirmed-free port
    await new Promise<void>((resolve, reject) => {
      this.server.once("error", reject);
      this.server.listen(targetPort, host, () => {
        this.server.removeListener("error", reject);
        resolve();
      });
    });
    this.port = targetPort;

    const url = `http://localhost:${this.port}`;
    this.logger.info(`[WebServer] Dashboard available at ${url}`);
    if (this.allowLan) {
      this.logger.info(`[WebServer] LAN access enabled (listening on ${host})`);
    }
    console.log(`\nMoombox Dashboard: ${url}\n`);

    if (openBrowser) {
      this.openBrowser(url);
    }

    return this.port;
  }

  getPort(): number {
    return this.port;
  }

  private openBrowser(url: string) {
    const program =
      process.platform === "win32"
        ? "explorer"
        : process.platform === "darwin"
          ? "open"
          : "xdg-open";

    const child = spawn(program, [url], {
      stdio: "ignore",
    });
    child.unref();
  }

  stop(): void {
    // Unsubscribe from logger and database events
    this.loggerUnsubscribe?.();
    this.jobUpdateUnsubscribe?.();
    this.jobsChangeUnsubscribe?.();

    // Clear pending throttle timers
    for (const timer of this.jobTrailingTimers.values()) {
      clearTimeout(timer);
    }
    this.jobTrailingTimers.clear();

    this.server.close();
    this.wss.close();
  }
}
