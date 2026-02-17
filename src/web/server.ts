/**
 * Web Server for Moombox Dashboard
 *
 * Provides HTTP server with WebSocket support for real-time updates.
 */

import express from "express";
import ms from "ms";
import { createServer } from "http";
import net from "net";
import { WebSocketServer, WebSocket } from "ws";
import path from "path";
import fs from "fs-extra";
import { fileURLToPath } from "url";
import { spawn } from "child_process";
import compression from "compression";
import { Logger } from "../core/logger.js";
import { Database } from "../core/database.js";
import { ConfigManager } from "../core/config.js";
import type { Job } from "../types/jobs.js";
import { registerPotRoutes, registerConfigRoutes, registerImportRoutes, registerJobRoutes, registerTrimRoutes, errorMiddleware } from "./routes/index.js";
import { isPrivateIP, isLoopback } from "../utils/ipValidation.js";

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
  private clientJobs: Map<WebSocket, Set<string>> = new Map(); // Track which jobs each client receives updates for

  constructor(options: WebServerOptions = {}) {
    this.port = options.port || DEFAULT_PORT;
    this.allowLan = options.allowLan ?? false;
    this.allowExternal = this.allowLan && (options.allowExternal ?? false);
    this.logger = Logger.getInstance();
    this.app = express();
    this.server = createServer(this.app);
    this.wss = new WebSocketServer({
      server: this.server,
      maxPayload: 1024 * 1024, // 1MB max WebSocket message size
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
  static isLoopback(ip: string): boolean {
    return isLoopback(ip);
  }

  /**
   * Check whether a connecting client IP is allowed based on access config.
   */
  private isAllowedClient(ip: string | undefined): boolean {
    if (!ip) return false;
    if (WebServer.isLoopback(ip)) return true;
    if (this.allowExternal) return true;
    if (this.allowLan) return isPrivateIP(ip);
    return false;
  }

  private setupMiddleware() {
    // CORS middleware (must be first to handle preflight requests)
    this.app.use((req, res, next) => {
      const origin = req.headers.origin;

      // Determine if origin is allowed
      let allowOrigin = false;
      if (origin) {
        try {
          const hostname = new URL(origin).hostname;
          allowOrigin =
            WebServer.isLoopback(hostname) ||
            hostname === "localhost" ||
            (this.allowLan && isPrivateIP(hostname)) ||
            this.allowExternal;
        } catch {
          allowOrigin = false;
        }
      }

      // Set CORS headers if origin is allowed
      if (allowOrigin && origin) {
        res.setHeader("Access-Control-Allow-Origin", origin);
        res.setHeader("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS");
        res.setHeader("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With");
        res.setHeader("Access-Control-Allow-Credentials", "true");
        res.setHeader("Access-Control-Max-Age", "86400"); // 24 hours
      }

      // Handle preflight OPTIONS requests
      if (req.method === "OPTIONS") {
        return res.status(204).end();
      }

      next();
    });

    // Gate ALL requests by client IP based on network_access config
    this.app.use((req, res, next) => {
      if (!this.isAllowedClient(req.socket.remoteAddress)) {
        return res.status(403).send("Forbidden");
      }
      next();
    });

    // Compression middleware (gzip/deflate) - 90% bandwidth savings
    this.app.use(compression({
      level: 6, // Balance between speed and compression ratio
      threshold: 1024, // Only compress responses > 1KB
    }));

    this.app.use(express.json());

    // Security headers
    this.app.use((req, res, next) => {
      res.setHeader("X-Frame-Options", "DENY");
      res.setHeader("X-Content-Type-Options", "nosniff");
      res.setHeader("Referrer-Policy", "no-referrer");

      // Permissions-Policy: deny all potentially dangerous features
      res.setHeader("Permissions-Policy",
        "accelerometer=(), " +
        "autoplay=(self), " + // Allow autoplay for embedded videos
        "camera=(), " +
        "clipboard-write=(self), " + // Allow clipboard for copy/paste
        "encrypted-media=(self), " + // Allow encrypted media for video playback
        "geolocation=(), " +
        "gyroscope=(), " +
        "microphone=(), " +
        "picture-in-picture=(self)" // Allow PiP for video player
      );

      // Content Security Policy (prevents XSS attacks)
      res.setHeader("Content-Security-Policy",
        "default-src 'self'; " +
        "script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; " + // Shoelace from CDN + inline event handlers
        "style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; " + // Shoelace CSS from CDN + inline styles
        "font-src 'self' https://cdn.jsdelivr.net; " + // Shoelace fonts from CDN
        "img-src 'self' data: https://i.ytimg.com https://yt3.ggpht.com https://*.jtvnw.net https://*.ttvnw.net https://cdn.jsdelivr.net; " + // YouTube/Twitch thumbnails + Shoelace icons
        "connect-src 'self' https://cdn.jsdelivr.net data:; " + // WebSocket + Shoelace icon fetches + data URIs
        "frame-src https://www.youtube-nocookie.com https://player.twitch.tv; " +
        "object-src 'none'; " +
        "base-uri 'self'; " +
        "form-action 'self'"
      );

      next();
    });

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

          // Cache static assets for 1 year (immutable in production)
          if ([".css", ".js", ".png", ".jpg", ".svg", ".ico"].includes(ext)) {
            res.setHeader("Cache-Control", "public, max-age=31536000, immutable");
          } else {
            res.setHeader("Cache-Control", "no-cache");
          }

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
          this.app.use(express.static(publicPath, {
            maxAge: "1y", // Cache static assets for 1 year
            immutable: true,
          }));
          break;
        }
      }
    }
  }

  private setupRoutes() {
    // Helper to create an API router with all routes
    const createApiRouter = () => {
      const router = express.Router();

      // CSRF protection for mutating requests — verify Origin header matches a trusted source
      // (The IP-level gate in setupMiddleware already blocks disallowed clients,
      //  but Origin checks guard against cross-site requests from allowed IPs.)
      router.use((req, res, next) => {
        if (req.method !== "GET" && req.method !== "HEAD") {
          const origin = req.headers.origin || req.headers.referer;
          // Only validate Origin if present (same-origin requests may not include it)
          if (origin) {
            try {
              const h = new URL(origin as string).hostname;
              const trusted =
                WebServer.isLoopback(h) ||
                h === "localhost" ||
                (this.allowLan && isPrivateIP(h)) ||
                this.allowExternal;
              if (!trusted) {
                return res.status(403).json({ error: "Forbidden: invalid origin" });
              }
            } catch {
              return res.status(403).json({ error: "Forbidden: invalid origin" });
            }
          }
          // If no Origin/Referer, allow (IP check + CORS headers provide protection)
        }
        next();
      });

      // Register route modules
      registerJobRoutes(router, {
        jobLogs: this.jobLogs,
        filterJobsByAge: (jobs, archived) => this.filterJobsByAge(jobs, archived),
        broadcast: (msg) => this.broadcast(msg),
        isLoopback: WebServer.isLoopback,
      });

      registerConfigRoutes(router, {
        logBuffer: this.logBuffer,
      });

      registerImportRoutes(router);

      registerTrimRoutes(router);

      return router;
    };

    // Mount API routes at both /api/v1 (current version) and /api (backward compatibility)
    this.app.use("/api/v1", createApiRouter());
    this.app.use("/api", createApiRouter());

    // POT Provider endpoints are mounted on the app directly (not /api router)
    registerPotRoutes(this.app);

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

    // Error-handling middleware (must be after all routes)
    this.app.use(errorMiddleware);
  }

  private setupWebSocket() {
    // Error handler for WebSocketServer instance
    this.wss.on("error", (error) => {
      this.logger.error(`[WebServer] WebSocketServer error: ${error.message}`);
    });

    this.wss.on("connection", (ws) => {
      this.clients.add(ws);
      this.logger.debug("[WebServer] Client connected");

      // Send current state
      this.sendInitialState(ws);

      // Error handler for individual connection
      ws.on("error", (error) => {
        this.logger.debug(`[WebServer] WebSocket connection error: ${error.message}`);
      });

      ws.on("message", (data) => {
        // Convert RawData to Buffer for length check
        const buffer = Buffer.isBuffer(data) ? data : Buffer.from(data as ArrayBuffer);

        // Validate message size (prevent DoS)
        if (buffer.length > 100000) {
          this.logger.warn("[WebServer] Oversized WebSocket message rejected");
          ws.close(1009, "Message too large");
          return;
        }

        try {
          const message: WSMessage = JSON.parse(buffer.toString());

          // Validate message structure
          if (typeof message.type !== "string" || message.type.length > 50) {
            ws.close(1008, "Invalid message format");
            return;
          }

          this.handleWSMessage(ws, message);
        } catch (e) {
          this.logger.debug("[WebServer] Invalid WebSocket message");
          ws.close(1008, "Invalid JSON");
        }
      });

      ws.on("close", () => {
        this.clients.delete(ws);
        this.logger.debug("[WebServer] Client disconnected");

        // Clear timers for jobs this client was tracking
        const clientJobIds = this.clientJobs.get(ws);
        if (clientJobIds) {
          for (const jobId of clientJobIds) {
            // Only clear timer if no other clients are tracking this job
            let stillTracked = false;
            for (const [otherClient, otherJobIds] of this.clientJobs.entries()) {
              if (otherClient !== ws && otherJobIds.has(jobId)) {
                stillTracked = true;
                break;
              }
            }

            if (!stillTracked) {
              const timer = this.jobTrailingTimers.get(jobId);
              if (timer) {
                clearTimeout(timer);
                this.jobTrailingTimers.delete(jobId);
              }
            }
          }
          this.clientJobs.delete(ws);
        }

        // If no clients remain, clear all pending throttle timers
        if (this.clients.size === 0) {
          for (const timer of this.jobTrailingTimers.values()) {
            clearTimeout(timer);
          }
          this.jobTrailingTimers.clear();
          this.clientJobs.clear();
          this.logger.debug("[WebServer] Cleared all pending job update timers (no clients)");
        }
      });
    });
  }

  private async sendInitialState(ws: WebSocket) {
    try {
      const db = await Database.getInstance();
      const jobs = await db.getJobs();

      const payload = JSON.stringify({
        type: "initial_state",
        payload: {
          jobs: this.filterJobsByAge(jobs, false),
          logs: this.logBuffer,
        },
      });

      // Only send if socket is still open
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(payload);
      }
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
        this.trackJobForClients(job.id);
      } else {
        // Schedule trailing-edge send to ensure the last update is always delivered
        const existing = this.jobTrailingTimers.get(job.id);
        if (existing) clearTimeout(existing);
        this.jobTrailingTimers.set(job.id, setTimeout(() => {
          this.jobTrailingTimers.delete(job.id);
          jobUpdateTimestamps.set(job.id, Date.now());
          this.broadcast({ type: "job_update", payload: job });
          this.trackJobForClients(job.id);
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
    const hideAgeMs = (config.tasklist?.hide_finished_age_days || 30) * ms("1d");

    return jobs.filter((job) => {
      if (job.status !== "Finished") return !archived;
      const age = now - new Date(job.updatedAt).getTime();
      return archived ? age > hideAgeMs : age <= hideAgeMs;
    });
  }

  private broadcast(message: WSMessage) {
    let data: string;
    try {
      data = JSON.stringify(message);
    } catch (e) {
      this.logger.error(`[WebServer] Failed to stringify broadcast message: ${e}`);
      return;
    }

    for (const client of this.clients) {
      if (client.readyState === WebSocket.OPEN) {
        try {
          client.send(data);
        } catch (e) {
          this.logger.debug(`[WebServer] Failed to send to client: ${e}`);
          // Remove dead client from set
          this.clients.delete(client);
        }
      }
    }
  }

  /**
   * Track that all current clients received updates for this job (for memory leak prevention)
   */
  private trackJobForClients(jobId: string) {
    for (const client of this.clients) {
      if (!this.clientJobs.has(client)) {
        this.clientJobs.set(client, new Set());
      }
      this.clientJobs.get(client)!.add(jobId);
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
    // spawn passes args directly to the OS — no shell metachar issues
    const [prog, args] = process.platform === "win32"
      ? ["explorer.exe", [url]]
      : [process.platform === "darwin" ? "open" : "xdg-open", [url]];
    spawn(prog, args, { stdio: "ignore", detached: true }).unref();
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
