/**
 * Structured Logger using Pino
 *
 * Provides structured JSON logging for future Datadog/ELK integration.
 * Runs ALONGSIDE the existing Logger (doesn't replace it).
 *
 * Why keep existing Logger:
 * - Pub/sub system critical for TUI/Web real-time log streaming
 * - 383 usage sites across 37 files
 * - File rotation works well
 *
 * Why add Pino:
 * - Structured JSON logs for future aggregation
 * - Contextual child loggers (job ID, video ID)
 * - Much faster for high-volume logging
 */

import pino from "pino";
import { ConfigManager } from "./config.js";
import path from "path";
import fs from "fs-extra";

/**
 * Get log directory from config
 */
function getLogDir(): string {
  const config = ConfigManager.getInstance().get();
  const logFile = config.log_file_path || "./moombox.log";
  return path.dirname(logFile);
}

/**
 * Get log level from config
 */
function getLogLevel(): pino.Level {
  const config = ConfigManager.getInstance().get();
  const level = config.log_level?.toLowerCase() || "info";

  // Map custom log levels to Pino levels
  const levelMap: Record<string, pino.Level> = {
    verbose: "trace",
    debug: "debug",
    info: "info",
    warn: "warn",
    error: "error",
  };

  return levelMap[level] || "info";
}

/**
 * Create the main structured logger
 */
export const structuredLogger = pino({
  level: getLogLevel(),
  transport: {
    targets: [
      // Pretty console output for development
      {
        target: "pino-pretty",
        level: "debug",
        options: {
          colorize: true,
          translateTime: "SYS:standard",
          ignore: "pid,hostname",
          messageFormat: "{levelLabel} [{context}] {msg}",
        },
      },
      // JSON file output for production/aggregation
      {
        target: "pino/file",
        level: "info",
        options: {
          destination: path.join(getLogDir(), "structured.jsonl"),
          mkdir: true,
        },
      },
    ],
  },
  formatters: {
    level: (label) => {
      return { level: label };
    },
  },
  timestamp: pino.stdTimeFunctions.isoTime,
});

/**
 * Create a child logger for a specific job
 * @param jobId Job ID to attach to all log entries
 * @returns Child logger with jobId context
 */
export function createJobLogger(jobId: string) {
  return structuredLogger.child({ context: "job", jobId });
}

/**
 * Create a child logger for a specific video
 * @param videoId Video ID to attach to all log entries
 * @returns Child logger with videoId context
 */
export function createVideoLogger(videoId: string) {
  return structuredLogger.child({ context: "video", videoId });
}

/**
 * Create a child logger for a specific component
 * @param component Component name (e.g., "downloader", "muxer", "chat")
 * @returns Child logger with component context
 */
export function createComponentLogger(component: string) {
  return structuredLogger.child({ context: component });
}

/**
 * Log a structured event with custom fields
 * @param event Event name (e.g., "download_start", "mux_complete")
 * @param data Additional structured data
 */
export function logEvent(event: string, data: Record<string, any> = {}) {
  structuredLogger.info({ event, ...data });
}
