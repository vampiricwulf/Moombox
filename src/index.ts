/**
 * Moombox - YouTube Live Stream Archiver
 *
 * Entry point for the web dashboard and TUI.
 */

import { ConfigManager } from "./core/config.js";
import { Database } from "./core/database.js";
import { Logger } from "./core/logger.js";
import { FeedMonitor } from "./core/monitor.js";
import { DownloadWorker } from "./core/worker/index.js";
import { CookieRefreshService } from "./core/cookieRefresh.js";
import { PotProvider } from "./core/potProvider.js";
import { AutoCookieService } from "./core/autoCookies.js";
import { NotificationManager, NotificationType } from "./core/notifications.js";
import { WebServer } from "./web/server.js";
import { getErrorMessage } from "./types/errors.js";

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

// Parse command line arguments
const args = process.argv.slice(2);
const command = args[0] || "run";

async function addVideo(input: string) {
  const videoId = extractVideoId(input);
  if (!videoId) {
    console.error(`Invalid video ID or URL: ${input}`);
    process.exit(1);
  }

  await ConfigManager.getInstance().load();
  const db = await Database.getInstance();

  const exists = await db.jobExists(videoId);
  if (exists) {
    console.log(`Job already exists for video: ${videoId}`);
    process.exit(0);
  }

  const job = await db.addJob({
    videoId: videoId,
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

function extractVideoId(input: string): string | null {
  const trimmed = input.trim();

  // Direct video ID (11 chars)
  if (/^[a-zA-Z0-9_-]{11}$/.test(trimmed)) {
    return trimmed;
  }

  // YouTube URL patterns
  const patterns = [
    /(?:youtube\.com\/watch\?v=|youtu\.be\/|youtube\.com\/embed\/|youtube\.com\/v\/|youtube\.com\/shorts\/)([a-zA-Z0-9_-]{11})/,
    /youtube\.com\/live\/([a-zA-Z0-9_-]{11})/,
  ];

  for (const pattern of patterns) {
    const match = trimmed.match(pattern);
    if (match) {
      return match[1];
    }
  }

  return null;
}

async function run() {
  // Check if running in a TTY (interactive terminal)
  const isTTY = process.stdout.isTTY && process.stdin.isTTY;
  const noTUI = process.argv.includes("--no-tui");
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

  const monitor = new FeedMonitor();
  monitor.start();

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
    logger.info(`[Moombox] Web dashboard available at http://localhost:${actualPort}`);
  } catch (error: any) {
    const detail = error.code === "EADDRINUSE"
      ? `Port ${port} (and nearby ports) already in use. Is another instance running?`
      : `Web server failed: ${error.message}`;

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
    logger.info("[Moombox] Shutting down...");

    // Force exit after 10 seconds to prevent hanging
    const forceExitTimer = setTimeout(() => {
      logger.warn("[Moombox] Force exit after timeout");
      process.exit(1);
    }, 10000);
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

    await stopService("FeedMonitor", () => monitor.stop());
    await stopService("DownloadWorker", () => worker.stop());
    await stopService("CookieRefresh", () => CookieRefreshService.getInstance().stop());
    await stopService("AutoCookies", () => AutoCookieService.getInstance().stop());
    await stopService("PotProvider", () => PotProvider.getInstance().shutdown());
    if (serverStarted) await stopService("WebServer", () => server.stop());

    // Flush pending log writes and give DB writes a moment to complete
    await logger.flush();
    await new Promise((r) => setTimeout(r, 1000));
    process.exit(0);
  };

  process.on("SIGTERM", () => { shutdown(); });

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
  } catch (error: any) {
    console.error("Startup error:", error.message);
    console.error(error.stack);
    if (process.stdin.isTTY) {
      await waitForKeypress();
    }
    process.exit(1);
  }
})();
