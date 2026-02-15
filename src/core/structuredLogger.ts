/**
 * Structured Logger using Pino
 *
 * Provides structured JSON logging integrated into the existing Logger.
 * Every Logger.log() call automatically produces a structured JSON entry.
 *
 * Architecture:
 * - Logger (text) → file with rotation + pub/sub (drives TUI/WebSocket)
 * - Pino (JSON) → structured.jsonl (for future Datadog/ELK/Grafana Loki)
 * - Both run from the same Logger.log() call — zero changes to 383 call sites
 *
 * Uses pino.destination() (synchronous file writer) instead of transports
 * for reliability in bundled Node.js SEA executables (no worker threads).
 */

import pino from "pino";
import path from "path";
import fs from "fs-extra";

/** The singleton pino instance, initialized lazily by Logger.init() */
let _instance: pino.Logger | null = null;

/** Map Logger levels to pino method names */
const LEVEL_MAP: Record<string, "debug" | "info" | "warn" | "error"> = {
  DEBUG: "debug",
  INFO: "info",
  WARNING: "warn",
  ERROR: "error",
};

/**
 * Initialize the structured logger.
 * Called by Logger.init() after config is loaded, ensuring correct paths and levels.
 *
 * @param logDir - Directory for structured.jsonl (same as text log directory)
 * @param level - Log level string from config (e.g. "DEBUG", "INFO")
 */
export function initStructuredLogger(logDir: string, level: string): void {
  if (_instance) return;

  try {
    const jsonPath = path.resolve(logDir, "structured.jsonl");
    fs.ensureDirSync(logDir);

    const dest = pino.destination({
      dest: jsonPath,
      sync: false, // Async flushing for performance
      mkdir: true,
    });

    _instance = pino(
      {
        level: mapLevel(level),
        timestamp: pino.stdTimeFunctions.isoTime,
        formatters: {
          level: (label) => ({ level: label }),
        },
      },
      dest,
    );
  } catch {
    // Non-fatal — structured logging is optional.
    // Text logger + pub/sub still function normally.
  }
}

/**
 * Forward a Logger.log() call to pino for structured JSON output.
 * Extracts component name from the conventional [ComponentName] prefix.
 *
 * @param levelName - Logger level name: "DEBUG", "INFO", "WARNING", "ERROR"
 * @param message - The raw log message (may contain [Component] prefix)
 */
export function forwardToStructuredLog(levelName: string, message: string): void {
  if (!_instance) return;

  const pinoMethod = LEVEL_MAP[levelName] || "info";
  const component = extractComponent(message);

  if (component) {
    _instance.child({ component })[pinoMethod](message);
  } else {
    _instance[pinoMethod](message);
  }
}

/**
 * Get the raw pino instance. Returns null if not initialized.
 */
export function getStructuredLogger(): pino.Logger | null {
  return _instance;
}

/**
 * Create a child logger for a specific job.
 * All entries include { context: "job", jobId }.
 */
export function createJobLogger(jobId: string): pino.Logger {
  if (!_instance) return pino({ level: "silent" });
  return _instance.child({ context: "job", jobId });
}

/**
 * Create a child logger for a specific video.
 * All entries include { context: "video", videoId }.
 */
export function createVideoLogger(videoId: string): pino.Logger {
  if (!_instance) return pino({ level: "silent" });
  return _instance.child({ context: "video", videoId });
}

/**
 * Create a child logger for a named component.
 * All entries include { context: componentName }.
 */
export function createComponentLogger(component: string): pino.Logger {
  if (!_instance) return pino({ level: "silent" });
  return _instance.child({ context: component });
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function mapLevel(level: string): string {
  const map: Record<string, string> = {
    verbose: "trace",
    debug: "debug",
    info: "info",
    warning: "warn",
    warn: "warn",
    error: "error",
  };
  return map[level.toLowerCase()] || "info";
}

/**
 * Extract [ComponentName] prefix from log messages.
 * E.g. "[Monitor] Feed check started" → "Monitor"
 */
function extractComponent(message: string): string | undefined {
  const match = message.match(/^\[([^\]]+)\]/);
  return match ? match[1] : undefined;
}
