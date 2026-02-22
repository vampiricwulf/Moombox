import fs from "fs-extra";
import path from "path";
import { ConfigManager } from "./config.js";

export enum LogLevel {
  DEBUG = 0,
  INFO = 1,
  WARNING = 2,
  ERROR = 3,
}

const DEFAULT_MAX_FILE_SIZE = 10 * 1024 * 1024; // 10MB
const DEFAULT_MAX_FILES = 5;

export class Logger {
  private static instance: Logger;
  private logFile: string = "";
  private logLevel: LogLevel = LogLevel.INFO;
  private listeners: ((msg: string) => void)[] = [];
  private history: string[] = [];
  private static readonly MAX_HISTORY = 200;
  private maxFileSize: number = DEFAULT_MAX_FILE_SIZE;
  private maxFiles: number = DEFAULT_MAX_FILES;
  private currentSize: number = 0;
  private rotationInProgress: boolean = false;
  private writeChain: Promise<void> = Promise.resolve();

  private constructor() {}

  static getInstance(): Logger {
    if (!Logger.instance) {
      Logger.instance = new Logger();
    }
    return Logger.instance;
  }

  async init() {
    try {
      const config = ConfigManager.getInstance().get();
      const levelStr = config.log_level?.toUpperCase() || "INFO";
      this.logLevel =
        LogLevel[levelStr as keyof typeof LogLevel] ?? LogLevel.INFO;

      // Default log file
      this.logFile =
        config.log_file_path || path.join(process.cwd(), "moombox.log");

      // Configure rotation settings from config (optional)
      this.maxFileSize = config.log_max_file_size || DEFAULT_MAX_FILE_SIZE;
      this.maxFiles = config.log_max_files || DEFAULT_MAX_FILES;

      await fs.ensureFile(this.logFile);

      // Get current file size for rotation tracking
      try {
        const stats = await fs.stat(this.logFile);
        this.currentSize = stats.size;
      } catch {
        this.currentSize = 0;
      }

      this.info(`Logger initialized. Level: ${LogLevel[this.logLevel]}`);
    } catch (e) {
      console.error("Logger init failed (Config might not be loaded yet):", e);
    }
  }

  private async rotateLogsIfNeeded() {
    if (this.rotationInProgress || this.currentSize < this.maxFileSize) {
      return;
    }

    this.rotationInProgress = true;

    try {
      // Rotate existing log files
      // moombox.log.4 -> delete
      // moombox.log.3 -> moombox.log.4
      // moombox.log.2 -> moombox.log.3
      // moombox.log.1 -> moombox.log.2
      // moombox.log -> moombox.log.1

      for (let i = this.maxFiles - 1; i >= 1; i--) {
        const oldPath = `${this.logFile}.${i}`;
        const newPath = `${this.logFile}.${i + 1}`;

        if (await fs.pathExists(oldPath)) {
          if (i === this.maxFiles - 1) {
            // Delete oldest file
            await fs.remove(oldPath);
          } else {
            await fs.move(oldPath, newPath, { overwrite: true });
          }
        }
      }

      // Rename current log to .1
      if (await fs.pathExists(this.logFile)) {
        await fs.move(this.logFile, `${this.logFile}.1`, { overwrite: true });
      }

      // Create new empty log file
      await fs.ensureFile(this.logFile);
      this.currentSize = 0;
    } catch (err) {
      console.error("Log rotation failed:", err);
    } finally {
      this.rotationInProgress = false;
    }
  }

  log(level: LogLevel, message: string) {
    if (level < this.logLevel) return;

    const timestamp = this.formatTimestamp(new Date());
    const levelName = LogLevel[level];
    const formattedMsg = `[${timestamp}] [${levelName}] ${message}`;

    // Buffer for late subscribers (e.g. TUI starts after early log messages).
    // Batch-trim instead of shift() to avoid O(n) per message at capacity.
    this.history.push(formattedMsg);
    if (this.history.length > Logger.MAX_HISTORY * 2) {
      this.history = this.history.slice(-Logger.MAX_HISTORY);
    }

    // Emit to listeners (e.g. TUI).
    // Unsubscribe replaces the array, so for-of on the current reference is safe.
    for (const l of this.listeners) {
      try { l(formattedMsg); } catch (e: unknown) { process.stderr.write(`Logger listener error: ${e instanceof Error ? e.message : String(e)}\n`); }
    }

    // Write to file (serialized via promise chain to prevent race with rotation)
    if (this.logFile) {
      const line = formattedMsg + "\n";

      this.writeChain = this.writeChain.then(async () => {
        try {
          await fs.appendFile(this.logFile, line);
          this.currentSize += Buffer.byteLength(line, "utf8");
          await this.rotateLogsIfNeeded();
        } catch (err) {
          console.error("Failed to write to log file:", err);
        }
      });
    }
  }

  /**
   * Re-read log level from config. Called after config save so changes apply immediately.
   */
  refreshLogLevel() {
    try {
      const config = ConfigManager.getInstance().get();
      const levelStr = config.log_level?.toUpperCase() || "INFO";
      const newLevel = LogLevel[levelStr as keyof typeof LogLevel] ?? LogLevel.INFO;
      if (newLevel !== this.logLevel) {
        this.logLevel = newLevel;
        this.info(`Log level changed to ${LogLevel[this.logLevel]}`);
      }
    } catch {
      // Config may not be loaded yet
    }
  }

  debug(msg: string) {
    this.log(LogLevel.DEBUG, msg);
  }
  info(msg: string) {
    this.log(LogLevel.INFO, msg);
  }
  warn(msg: string) {
    this.log(LogLevel.WARNING, msg);
  }
  error(msg: string) {
    this.log(LogLevel.ERROR, msg);
  }

  private formatTimestamp(date: Date): string {
    const y = date.getFullYear();
    const mo = String(date.getMonth() + 1).padStart(2, "0");
    const d = String(date.getDate()).padStart(2, "0");
    const h = String(date.getHours()).padStart(2, "0");
    const mi = String(date.getMinutes()).padStart(2, "0");
    const s = String(date.getSeconds()).padStart(2, "0");
    return `${y}-${mo}-${d} ${h}:${mi}:${s}`;
  }

  /**
   * Wait for all pending log writes to complete.
   */
  async flush(): Promise<void> {
    await this.writeChain;
  }

  subscribe(listener: (msg: string) => void) {
    // Replay buffered history so late subscribers see early messages
    for (const msg of this.history) {
      try { listener(msg); } catch (e: unknown) { process.stderr.write(`Logger listener error: ${e instanceof Error ? e.message : String(e)}\n`); }
    }
    this.listeners.push(listener);
    return () => {
      this.listeners = this.listeners.filter((l) => l !== listener);
    };
  }
}
