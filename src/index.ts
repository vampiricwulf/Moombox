/**
 * Moombox - YouTube Live Stream Archiver
 *
 * Entry point for the web dashboard and TUI.
 */

import { ConfigManager } from "./core/config.js";
import { Database } from "./core/database.js";
import { Logger } from "./core/logger.js";
import { FeedMonitor } from "./core/monitor.js";
import { DecapiMonitor } from "./core/decapiMonitor.js";
import { TwitchMonitor } from "./core/twitchMonitor.js";
import { DownloadWorker } from "./core/worker/index.js";
import { CookieRefreshService } from "./core/cookieRefresh.js";
import { PotProvider } from "./core/potProvider.js";
import { AutoCookieService } from "./core/autoCookies.js";
import { TwitchService } from "./engine/twitch/index.js";
import { NotificationManager, NotificationType } from "./core/notifications.js";
import { WebServer } from "./web/server.js";
import { getErrorMessage } from "./types/errors.js";
import { extractMediaId } from "./utils/mediaId.js";
import { spawn as nodeSpawn } from "child_process";
import * as v8 from "v8";

// Dynamic import for TUI (may not be available in packaged build)
async function loadTUI(): Promise<{ startTUI: () => Promise<void> } | null> {
  try {
    return await import("./tui/index.js");
  } catch {
    return null;
  }
}

// Global error handling
process.on("uncaughtException", (error) => {
  console.error("Fatal error:", error.message);
  console.error(error.stack);
  process.exit(1);
});

process.on("unhandledRejection", (reason) => {
  console.error("Unhandled promise rejection:", reason);
});

// Wait for a keypress before exiting (prevents .exe window from vanishing)
function waitForKeypress(): Promise<void> {
  return new Promise((resolve) => {
    console.error("\nPress any key to exit...");
    if (!process.stdin.isTTY) { resolve(); return; }
    process.stdin.setRawMode(true);
    process.stdin.resume();
    process.stdin.once("data", () => {
      process.stdin.setRawMode(false);
      resolve();
    });
  });
}

// Module-level shutdown reference (set during run())
let shutdownFn: (() => Promise<void>) | null = null;

/**
 * Restart the process: runs shutdown, then re-spawns the same process.
 * Works for both SEA executable and `npm run dev`.
 */
export async function restart(): Promise<never> {
  if (shutdownFn) {
    await shutdownFn();
  }
  // Re-spawn identical process
  const child = nodeSpawn(process.argv[0], process.argv.slice(1), {
    stdio: "inherit",
    detached: true,
    cwd: process.cwd(),
  });
  child.unref();
  process.exit(0);
}

// Parse command line arguments
const args = process.argv.slice(2);
const command = args[0] || "run";

async function addVideo(input: string) {
  const target = extractMediaId(input);
  if (!target) {
    console.error(`Invalid video ID or URL: ${input}`);
    process.exit(1);
  }

  await ConfigManager.getInstance().load();
  const db = await Database.getInstance();

  if (target.platform === "twitch") {
    const twitchTarget = target.target;
    if (twitchTarget.type === "clip") {
      console.error("Twitch clips are not supported.");
      process.exit(1);
    }

    let videoId: string;
    let url: string;
    if (twitchTarget.type === "vod") {
      videoId = `tw_v${twitchTarget.vodId}`;
      url = `https://www.twitch.tv/videos/${twitchTarget.vodId}`;
    } else {
      videoId = twitchTarget.login; // Will be resolved by the worker
      url = `https://www.twitch.tv/${twitchTarget.login}`;
    }

    const exists = await db.jobExists(videoId);
    if (exists) {
      console.log(`Job already exists: ${videoId}`);
      process.exit(0);
    }

    const job = await db.addJob({
      videoId,
      url,
      title: "Manual Add",
      channelName: twitchTarget.type === "channel" ? twitchTarget.login : "Manual",
      manuallyAdded: true,
      platform: "twitch",
    });

    if (job) {
      console.log(`Added Twitch ${twitchTarget.type} ${videoId} to queue.`);
      await NotificationManager.getInstance().send(
        "Video Added",
        `Manually added Twitch ${twitchTarget.type}: ${videoId}`,
        NotificationType.INFO,
        [{ name: "ID", value: videoId, inline: true }],
        { url, event: "added" },
      );
    } else {
      console.log(`Failed to add ${videoId} (may already exist).`);
    }
  } else {
    // YouTube path
    const videoId = target.videoId;

    const exists = await db.jobExists(videoId);
    if (exists) {
      console.log(`Job already exists for video: ${videoId}`);
      process.exit(0);
    }

    const job = await db.addJob({
      videoId,
      url: `https://www.youtube.com/watch?v=${videoId}`,
      title: "Manual Add",
      channelName: "Manual",
      thumbnailUrl: "",
      manuallyAdded: true,
    });

    if (job) {
      console.log(`Added ${videoId} to queue.`);
      await NotificationManager.getInstance().send(
        "Video Added",
        `Manually added: ${videoId}`,
        NotificationType.INFO,
        [{ name: "Video ID", value: videoId, inline: true }],
        { url: `https://www.youtube.com/watch?v=${videoId}`, event: "added" },
      );
    } else {
      console.log(`Failed to add ${videoId} (may already exist).`);
    }
  }
}

async function run() {
  // Check if running in a TTY (interactive terminal)
  const isTTY = process.stdout.isTTY && process.stdin.isTTY;
  const noTUI = process.argv.includes("--no-tui") || /^(1|true|yes)$/i.test(process.env.MOOMBOX_NO_TUI?.trim() || "");
  const useTUI = isTTY && !noTUI;

  if (!useTUI) {
    console.log("Moombox - YouTube Live Stream Archiver");
    console.log("======================================\n");
    console.log("Loading configuration...");
  }

  // Load config
  await ConfigManager.getInstance().load();

  // Initialize logger
  const logger = Logger.getInstance();
  await logger.init();

  // Redirect console to logger when TUI is active
  if (useTUI) {
    const originalConsoleLog = console.log;
    const originalConsoleError = console.error;
    const originalConsoleWarn = console.warn;

    console.log = (...args: unknown[]) => {
      logger.info(args.map(String).join(" "));
    };
    console.error = (...args: unknown[]) => {
      logger.error(args.map(String).join(" "));
    };
    console.warn = (...args: unknown[]) => {
      logger.warn(args.map(String).join(" "));
    };

    // Restore on exit
    process.on("exit", () => {
      console.log = originalConsoleLog;
      console.error = originalConsoleError;
      console.warn = originalConsoleWarn;
    });
  }

  // Initialize database
  if (!useTUI) {
    console.log("Initializing database...");
  }
  await Database.getInstance();

  // Start services
  if (!useTUI) {
    console.log("Starting services...");
  }

  // Initialize Twitch service (loads auth token from cookies if available)
  await TwitchService.getInstance().init();

  const monitor = FeedMonitor.getInstance();
  monitor.start();

  const decapiMonitor = DecapiMonitor.getInstance();
  decapiMonitor.start();

  const twitchMonitor = TwitchMonitor.getInstance();
  twitchMonitor.start();

  const worker = DownloadWorker.getInstance();
  worker.start();

  CookieRefreshService.getInstance().start();
  AutoCookieService.getInstance(); // Initialize (doesn't start anything)

  // Start web server (dashboard and POT provider)
  const config = ConfigManager.getInstance().get();
  const port = config.port || 774;
  const netAccess = config.network_access ?? "localhost";
  const allowLan = netAccess === "lan" || netAccess === "external";
  const allowExternal = netAccess === "external";
  const server = new WebServer({ port, allowLan, allowExternal });
  let serverStarted = false;
  try {
    const actualPort = await server.start(!useTUI);
    serverStarted = true;
    // Expose the actual port for TUI and other components
    process.env.MOOMBOX_PORT = String(actualPort);
    logger.info(`[Moombox] Web dashboard available at https://localhost:${actualPort}`);

    // Wire up countdown timer broadcasts
    monitor.onSchedule = () => server.broadcastCheckTimers?.();
    decapiMonitor.onSchedule = () => server.broadcastCheckTimers?.();
    twitchMonitor.onSchedule = () => server.broadcastCheckTimers?.();
  } catch (error: unknown) {
    const detail = (error as NodeJS.ErrnoException).code === "EADDRINUSE"
      ? `Port ${port} (and nearby ports) already in use. Is another instance running?`
      : `Web server failed: ${getErrorMessage(error)}`;

    if (useTUI) {
      // TUI will start below and show the error — downloads still work
      logger.error(`[Moombox] ${detail}`);
      logger.error("[Moombox] Web dashboard is unavailable. Downloads will still work.");
    } else {
      console.error(`\nError: ${detail}`);
      console.error("Web dashboard is unavailable.");
      await waitForKeypress();
      process.exit(1);
    }
  }

  // Handle shutdown — stop each service with error isolation
  let shuttingDown = false;
  const shutdown = async () => {
    if (shuttingDown) return;
    shuttingDown = true;
    shutdownFn = null; // Prevent restart from calling us again
    logger.info("[Moombox] Shutting down...");

    // Force exit after 10 seconds to prevent hanging
    const forceExitTimer = setTimeout(() => {
      logger.warn("[Moombox] Force exit after timeout");
      process.exit(1);
    }, 10_000);
    forceExitTimer.unref();

    // Stop services in dependency order (consumers first, infrastructure last)
    const stopService = async (name: string, fn: () => void | Promise<void>) => {
      try {
        await fn();
        logger.debug(`[Moombox] Stopped ${name}`);
      } catch (e) {
        logger.error(`[Moombox] Error stopping ${name}: ${getErrorMessage(e)}`);
      }
    };

    await stopService("TwitchMonitor", () => twitchMonitor.stop());
    await stopService("DecapiMonitor", () => decapiMonitor.stop());
    await stopService("FeedMonitor", () => monitor.stop());
    await stopService("DownloadWorker", () => worker.stop());
    await stopService("CookieRefresh", () => CookieRefreshService.getInstance().stop());
    await stopService("AutoCookies", () => AutoCookieService.getInstance().stop());
    await stopService("PotProvider", () => PotProvider.getInstance().shutdown());
    if (serverStarted) await stopService("WebServer", () => server.stop());

    // Flush pending log writes and give DB writes a moment to complete
    await logger.flush();
    await new Promise((r) => setTimeout(r, 1_000));
    process.exit(0);
  };

  // Expose shutdown for restart()
  shutdownFn = shutdown;

  process.on("SIGTERM", () => { shutdown(); });

  // Periodic memory usage logging (every 2 minutes) for diagnostics.
  // If node is started with --expose-gc, forces a full GC before measuring
  // to distinguish true leaks from lazy GC.
  let prevHeapUsed = 0;
  const memoryLogInterval = setInterval(async () => {
    // Force GC if available (node --expose-gc) for accurate measurement
    if (typeof globalThis.gc === "function") {
      globalThis.gc();
    }
    const mem = process.memoryUsage();
    const heapMB = mem.heapUsed / 1048576;
    const delta = prevHeapUsed > 0 ? ` (${heapMB > prevHeapUsed ? "+" : ""}${(heapMB - prevHeapUsed).toFixed(1)})` : "";
    prevHeapUsed = heapMB;
    logger.info(
      `[Memory] RSS: ${(mem.rss / 1048576).toFixed(1)}MB, ` +
      `Heap: ${heapMB.toFixed(1)}${delta}/${(mem.heapTotal / 1048576).toFixed(1)}MB, ` +
      `External: ${(mem.external / 1048576).toFixed(1)}MB, ` +
      `ArrayBuffers: ${(mem.arrayBuffers / 1048576).toFixed(1)}MB` +
      (typeof globalThis.gc === "function" ? " [post-GC]" : ""),
    );

    // V8 heap space breakdown — identifies which heap region is growing
    const heapStats = v8.getHeapStatistics();
    const spaces = v8.getHeapSpaceStatistics();
    const oldSpace = spaces.find((s) => s.space_name === "old_space");
    const loSpace = spaces.find((s) => s.space_name === "large_object_space");
    logger.info(
      `[Memory] V8: contexts=${heapStats.number_of_native_contexts}, ` +
      `detached=${heapStats.number_of_detached_contexts}, ` +
      `handles=${(heapStats.used_global_handles_size / 1024).toFixed(0)}KB, ` +
      `old_space=${oldSpace ? (oldSpace.space_used_size / 1048576).toFixed(1) : "?"}MB, ` +
      `large_obj=${loSpace ? (loSpace.space_used_size / 1048576).toFixed(1) : "?"}MB`,
    );
  }, 120_000);
  memoryLogInterval.unref();

  // Start TUI if in interactive terminal
  if (useTUI) {
    const tui = await loadTUI();
    if (tui) {
      // SIGINT is handled by TUI
      await tui.startTUI();
      await shutdown();
    } else {
      // TUI import failed, fall back to console
      logger.info("[Moombox] TUI not available, using web dashboard only");
      process.on("SIGINT", () => { shutdown(); });
    }
  } else {
    process.on("SIGINT", () => { shutdown(); });
  }
}

// Main entry point
(async () => {
  try {
    if (command === "add") {
      const input = args[1];
      if (!input) {
        console.error("Usage: moombox add <video_id_or_url>");
        process.exit(1);
      }
      await addVideo(input);
    } else if (command === "run" || command === "") {
      await run();
    } else {
      console.log(`
Usage:
  moombox [command]

Commands:
  run       Start the web dashboard and monitor (default)
  add <id>  Manually add a video ID to the queue
`);
    }
  } catch (error: unknown) {
    console.error("Startup error:", getErrorMessage(error));
    if (error instanceof Error) console.error(error.stack);
    if (process.stdin.isTTY) {
      await waitForKeypress();
    }
    process.exit(1);
  }
})();
