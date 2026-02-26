/**
 * Chat Downloader
 *
 * Downloads live chat messages from YouTube streams, following the same
 * patterns as SegmentDownloader (EventEmitter, resume state, polling loop).
 */

import fs from "fs-extra";
import { open as fsOpen } from "node:fs/promises";
import path from "path";
import { EventEmitter } from "events";
import { ChatApi } from "./chatApi.js";
import { Logger } from "../../core/logger.js";
import { LIMITS } from "../../constants/limits.js";
import { getErrorMessage } from "../../types/errors.js";
import { cancellableSleep } from "../../utils/async.js";
import type {
  ChatDownloaderOptions,
  ChatMessage,
  ChatData,
  ChatResumeState,
  ChatProgress,
} from "../../types/chat.js";

/**
 * Chat Downloader - downloads live chat messages alongside video streams
 */
export class ChatDownloader extends EventEmitter {
  private options: ChatDownloaderOptions;
  private chatApi: ChatApi;
  private logger: Logger;

  private running: boolean = false;
  private cancelFlag: boolean = false;
  private streamEnded: boolean = false;
  private messages: ChatMessage[] = [];
  private seenIds: Set<string> = new Set();
  private continuation: string;
  private streamStartMs: number = 0;
  private completionPromise: Promise<void> | null = null;
  private abortController: AbortController | null = null;

  // Write to disk within 1 second of receiving new messages
  private static readonly WRITE_INTERVAL_MS = 1_000;
  private static readonly DEDUP_KEEP = LIMITS.CHAT_DEDUP_IDS;
  private totalMessageCount: number = 0;
  private flushedToDisk: boolean = false;
  private lastWriteMs: number = 0;
  private lastTimestampUsec: string = "0";

  constructor(options: ChatDownloaderOptions) {
    super();
    this.options = options;
    this.continuation = options.initialContinuation;
    this.chatApi = new ChatApi();
    this.logger = Logger.getInstance();

    // Parse stream start time for offset calculation
    if (options.streamStartTime) {
      const startMs = new Date(options.streamStartTime).getTime();
      this.streamStartMs = Number.isNaN(startMs) ? 0 : startMs;
    }
  }

  /**
   * Whether the stream is still active (live/upcoming and not ended).
   * Used in the chat loop for error tolerance and backoff decisions.
   */
  private get isStreamActive(): boolean {
    return !!this.options.isLiveOrUpcoming && !this.streamEnded;
  }

  private getResumeFilePath(): string {
    return this.options.resumeFile || `${this.options.outputFile}.resume.json`;
  }

  private async loadResumeState(): Promise<ChatResumeState | null> {
    const resumePath = this.getResumeFilePath();
    try {
      if (await fs.pathExists(resumePath)) {
        const data = await fs.readJson(resumePath);
        // Validate that the video ID matches (same stream)
        if (data.videoId === this.options.videoId) {
          return data as ChatResumeState;
        }
      }
    } catch {
      // Ignore errors reading resume state
    }
    return null;
  }

  private async saveResumeState(): Promise<void> {
    const resumePath = this.getResumeFilePath();
    const state: ChatResumeState = {
      lastTimestampUsec: this.lastTimestampUsec,
      messageCount: this.totalMessageCount,
      continuation: this.continuation,
      timestamp: Date.now(),
      videoId: this.options.videoId,
      recentIds: [...this.seenIds], // Bounded to DEDUP_KEEP entries (~5k IDs, ~100KB)
    };
    try {
      const tmpPath = resumePath + ".tmp";
      await fs.writeJson(tmpPath, state);
      await fs.move(tmpPath, resumePath, { overwrite: true });
    } catch {
      // Ignore errors saving resume state
    }
  }

  private async clearResumeState(): Promise<void> {
    const resumePath = this.getResumeFilePath();
    try {
      await fs.remove(resumePath);
    } catch {
      // Ignore errors
    }
  }

  /**
   * Load existing chat messages from output file if resuming
   */
  private async loadExistingMessages(): Promise<ChatMessage[]> {
    try {
      if (await fs.pathExists(this.options.outputFile)) {
        const data = await fs.readJson(this.options.outputFile);
        if (data.messages && Array.isArray(data.messages)) {
          return data.messages;
        }
      }
    } catch {
      // Ignore errors
    }
    return [];
  }

  /**
   * Write chat data to output file.
   *
   * Two paths:
   * - Not flushed: all messages are in memory, write complete JSON atomically.
   * - Flushed: the on-disk file already has old messages. Use file-handle-based
   *   in-place append — read only the last 10 bytes to locate ']', truncate
   *   there, then append new messages + closing structure. Memory cost: O(new
   *   messages) instead of O(file size). For a 400MB chat file with non-Latin1
   *   characters, the old approach read it as a ~800MB V8 UTF-16 string.
   */
  private async writeChatFile(): Promise<void> {
    await fs.ensureDir(path.dirname(this.options.outputFile));
    const tmpPath = this.options.outputFile + ".tmp";

    if (!this.flushedToDisk) {
      // All messages in memory — write complete file directly
      const chatData: ChatData = {
        videoId: this.options.videoId,
        videoTitle: this.options.videoTitle,
        channelName: this.options.channelName,
        streamStartTime: this.options.streamStartTime,
        downloadedAt: new Date().toISOString(),
        messageCount: this.totalMessageCount,
        messages: this.messages,
      };
      await fs.writeJson(tmpPath, chatData, { spaces: 2 });
      await fs.move(tmpPath, this.options.outputFile, { overwrite: true });
      return;
    }

    // Flushed path: this.messages only contains unwritten messages (cleared on flush)
    const newMessages = this.messages;

    if (!(await fs.pathExists(this.options.outputFile))) {
      // Disk file missing — write what we have in memory
      const chatData: ChatData = {
        videoId: this.options.videoId,
        videoTitle: this.options.videoTitle,
        channelName: this.options.channelName,
        streamStartTime: this.options.streamStartTime,
        downloadedAt: new Date().toISOString(),
        messageCount: this.totalMessageCount,
        messages: this.messages,
      };
      await fs.writeJson(tmpPath, chatData, { spaces: 2 });
      await fs.move(tmpPath, this.options.outputFile, { overwrite: true });
      return;
    }

    // In-place append: read only the tail to find ']', truncate, and write new messages
    const handle = await fsOpen(this.options.outputFile, "r+");
    try {
      const stat = await handle.stat();

      // Read last 10 bytes to locate ']'
      const tailSize = Math.min(10, stat.size);
      if (tailSize < 3) {
        // File too small — fall back to full rewrite
        await handle.close();
        return this.rewriteChatFileFallback(newMessages, tmpPath);
      }
      const tailBuf = Buffer.alloc(tailSize);
      await handle.read(tailBuf, 0, tailSize, stat.size - tailSize);
      const tail = tailBuf.toString("utf8");
      const bracketOffset = tail.lastIndexOf("]");

      if (bracketOffset === -1) {
        // Corrupt file (no ']' found) — fall back to full rewrite
        this.logger.warn("[ChatDownloader] Corrupt chat file, rewriting with in-memory messages");
        await handle.close();
        return this.rewriteChatFileFallback(newMessages, tmpPath);
      }

      const bracketBytePos = stat.size - tailSize + bracketOffset;

      // Read a few bytes before ']' to check for existing messages
      let hasExisting = false;
      if (bracketBytePos > 5) {
        const checkSize = Math.min(5, bracketBytePos);
        const checkBuf = Buffer.alloc(checkSize);
        await handle.read(checkBuf, 0, checkSize, bracketBytePos - checkSize);
        hasExisting = checkBuf.toString("utf8").trimEnd().endsWith("}");
      }

      // Build only the new content to append
      let appendStr = "";
      if (newMessages.length > 0) {
        appendStr += hasExisting ? ",\n" : "";
        appendStr += newMessages.map((m) => "    " + JSON.stringify(m)).join(",\n");
        appendStr += "\n";
      }
      appendStr += "  ]\n}";

      // Truncate at ']' position, then write new content
      await handle.truncate(bracketBytePos);
      const appendBuf = Buffer.from(appendStr, "utf8");
      await handle.write(appendBuf, 0, appendBuf.length, bracketBytePos);
    } finally {
      await handle.close();
    }
  }

  /**
   * Update the chat file header metadata (messageCount, downloadedAt) in-place.
   * Reads only the first 1KB — avoids loading the entire file into memory.
   * Called once after the chat loop ends.
   */
  private async updateChatFileHeader(): Promise<void> {
    if (!(await fs.pathExists(this.options.outputFile))) return;

    const handle = await fsOpen(this.options.outputFile, "r+");
    try {
      const stat = await handle.stat();
      const headerSize = Math.min(1024, stat.size);
      if (headerSize < 50) return; // Too small to have a valid header

      const headerBuf = Buffer.alloc(headerSize);
      await handle.read(headerBuf, 0, headerSize, 0);
      const header = headerBuf.toString("utf8");

      const updated = header
        .replace(/"messageCount": \d+/, `"messageCount": ${this.totalMessageCount}`)
        .replace(/"downloadedAt": "[^"]*"/, `"downloadedAt": "${new Date().toISOString()}"`);

      // Only write if byte length is unchanged (safe in-place update).
      // Length changes when messageCount gains a digit (e.g. 999→1000) — rare, skip.
      const updatedBuf = Buffer.from(updated, "utf8");
      if (updatedBuf.length === headerBuf.length) {
        await handle.write(updatedBuf, 0, updatedBuf.length, 0);
      }
    } finally {
      await handle.close();
    }
  }

  /**
   * Full file rewrite fallback for corrupt/missing cases.
   */
  private async rewriteChatFileFallback(newMessages: ChatMessage[], tmpPath: string): Promise<void> {
    const chatData: ChatData = {
      videoId: this.options.videoId,
      videoTitle: this.options.videoTitle,
      channelName: this.options.channelName,
      streamStartTime: this.options.streamStartTime,
      downloadedAt: new Date().toISOString(),
      messageCount: this.totalMessageCount,
      messages: newMessages,
    };
    await fs.writeJson(tmpPath, chatData, { spaces: 2 });
    await fs.move(tmpPath, this.options.outputFile, { overwrite: true });
  }

  /**
   * Start downloading chat messages.
   * If already running (e.g., pre-started during upcoming phase),
   * returns the existing completion promise so callers can track when chat finishes.
   */
  async start(): Promise<void> {
    if (this.running && this.completionPromise) {
      return this.completionPromise;
    }
    if (this.running) return;

    this.running = true;
    this.cancelFlag = false;
    this.streamEnded = false;

    this.completionPromise = this.run();
    return this.completionPromise;
  }

  private async run(): Promise<void> {
    this.abortController = new AbortController();
    try {
      // Check for resume state
      const resumeState = await this.loadResumeState();
      let isResuming = false;

      if (resumeState) {
        this.continuation = resumeState.continuation;
        this.messages = [];

        if (resumeState.recentIds) {
          // Restore dedup set directly from resume state — no file I/O needed
          this.totalMessageCount = resumeState.messageCount;
          this.seenIds = new Set(resumeState.recentIds);
        } else {
          // Legacy resume state without recentIds — load file for dedup IDs
          const existing = await this.loadExistingMessages();
          this.totalMessageCount = existing.length;
          const tail = existing.slice(-ChatDownloader.DEDUP_KEEP);
          this.seenIds = new Set(tail.map((m) => m.id));
        }

        this.flushedToDisk = this.totalMessageCount > 0;
        isResuming = true;
        this.logger.info(
          `[ChatDownloader] Resuming from ${this.totalMessageCount} messages`,
        );
      }

      this.emit("start", { messageCount: this.totalMessageCount, resuming: isResuming });

      // Main polling loop
      await this.runChatLoop();

      // Write any remaining unwritten messages
      if (this.messages.length > 0) {
        await this.writeChatFile();
      }
      // Update header metadata (messageCount, downloadedAt) once at the end
      if (this.flushedToDisk && this.totalMessageCount > 0) {
        await this.updateChatFileHeader();
      }
      if (this.totalMessageCount > 0) {
        this.logger.info(
          `[ChatDownloader] Saved ${this.totalMessageCount} chat messages`,
        );
      }

      // Clear resume state on successful completion
      if (!this.cancelFlag) {
        await this.clearResumeState();
      }

      this.emit("finish");
    } finally {
      this.running = false;
      this.completionPromise = null;
      this.abortController = null;
    }
  }

  /**
   * Main chat polling loop
   */
  private async runChatLoop(): Promise<void> {
    let consecutiveErrors = 0;

    while (this.running && !this.cancelFlag && !this.streamEnded) {
      try {
        // Fetch chat messages
        const signal = this.abortController?.signal;
        const response = this.options.isReplay
          ? await this.chatApi.fetchChatReplay(
              this.continuation,
              this.options.apiKey,
              this.options.visitorData,
              this.options.cookieHeader,
              signal,
            )
          : await this.chatApi.fetchLiveChat(
              this.continuation,
              this.options.apiKey,
              this.options.visitorData,
              this.options.cookieHeader,
              signal,
            );

        consecutiveErrors = 0;

        // Process messages
        if (response.messages.length > 0) {
          let newInBatch = 0;
          for (const msg of response.messages) {
            // Calculate offsetMs if not already set (for live chat)
            // Preserve negative offsets for pre-stream chat messages
            if (msg.offsetMs === 0 && this.streamStartMs > 0) {
              const msgMs = parseInt(msg.timestampUsec, 10) / 1000;
              if (!Number.isNaN(msgMs)) {
                msg.offsetMs = msgMs - this.streamStartMs;
              }
            }

            // Deduplicate by ID
            if (!this.seenIds.has(msg.id)) {
              this.seenIds.add(msg.id);
              this.messages.push(msg);
              this.totalMessageCount++;
              this.lastTimestampUsec = msg.timestampUsec;
              newInBatch++;
            }
          }

          // Emit progress
          const progress: ChatProgress = {
            messageCount: this.totalMessageCount,
            lastTimestamp: response.messages[response.messages.length - 1]?.timestampText,
          };
          this.emit("progress", progress);

          // Write to disk within 1-second batching window
          const now = Date.now();
          if (now - this.lastWriteMs >= ChatDownloader.WRITE_INTERVAL_MS) {
            try {
              await this.writeChatFile();
              this.lastWriteMs = now;

              // All messages now on disk — clear from memory
              this.messages = [];
              this.flushedToDisk = true;

              // Bound seenIds to prevent unbounded growth
              if (this.seenIds.size > ChatDownloader.DEDUP_KEEP) {
                const arr = [...this.seenIds];
                this.seenIds = new Set(arr.slice(-ChatDownloader.DEDUP_KEEP));
              }

              await this.saveResumeState();
            } catch (ioErr: unknown) {
              this.logger.debug(`[ChatDownloader] Save error: ${getErrorMessage(ioErr)}`);
            }
          }
        }

        // Check if chat has ended
        if (response.isComplete || !response.nextContinuation) {
          if (this.isStreamActive) {
            // Stream is still live/upcoming — continuation went stale.
            // Fetch a fresh continuation token from the watch page.
            this.logger.debug(
              "[ChatDownloader] Chat returned complete/no continuation but stream is live — fetching fresh continuation",
            );
            const fresh = await this.chatApi.fetchFreshContinuation(
              this.options.videoId,
              this.options.cookieHeader,
              signal,
            );
            if (fresh.continuation) {
              this.logger.debug("[ChatDownloader] Got fresh continuation token");
              this.continuation = fresh.continuation;
              await this.sleep(10_000);
              continue;
            }
            // No fresh continuation — retry with exponential backoff
            this.logger.debug("[ChatDownloader] No fresh continuation available — retrying with backoff");
            let contRetryDelay = 10_000;
            const MAX_CONT_RETRIES = 30; // ~30min max
            let contRetries = 0;
            while (this.running && !this.cancelFlag && !this.streamEnded && contRetries < MAX_CONT_RETRIES) {
              await this.sleep(contRetryDelay);
              contRetries++;
              const retry = await this.chatApi.fetchFreshContinuation(
                this.options.videoId,
                this.options.cookieHeader,
                signal,
              );
              if (retry.continuation) {
                this.logger.debug("[ChatDownloader] Got fresh continuation token on retry");
                this.continuation = retry.continuation;
                break;
              }
              // Exponential backoff: 10s, 20s, 40s, 80s, cap at 5min
              contRetryDelay = Math.min(contRetryDelay * 2, 5 * 60_000);
            }
            if (contRetries >= MAX_CONT_RETRIES) {
              this.logger.warn("[ChatDownloader] Max continuation retries reached, stopping chat download");
              break;
            }
            continue;
          }
          this.logger.debug("[ChatDownloader] Chat stream complete");
          break;
        }

        // Update continuation for next request
        this.continuation = response.nextContinuation;

        // Wait before next poll (respect API timeout)
        // For live chat, YouTube returns timeoutMs indicating when to poll next
        // For replay/VOD, timeoutMs is often 0 meaning we can fetch immediately
        const waitMs = response.timeoutMs ?? (this.options.isReplay ? 0 : 5_000);
        if (waitMs > 0) {
          await this.sleep(waitMs);
        }
      } catch (e: unknown) {
        // If aborted (stop/markStreamEnded), exit the loop without counting as error
        if (e instanceof Error && e.name === "AbortError") {
          break;
        }

        consecutiveErrors++;
        this.logger.debug(`[ChatDownloader] Error: ${getErrorMessage(e)}`);

        // Higher tolerance for live/upcoming streams (transient issues are normal)
        const maxErrors = this.isStreamActive ? 20 : 5;
        if (consecutiveErrors > maxErrors) {
          this.logger.warn("[ChatDownloader] Too many errors, stopping");
          this.emit("error", new Error("Too many consecutive chat API errors"));
          break;
        }

        // Exponential backoff (cap at 30s for VOD, 60s for live)
        const maxBackoff = this.isStreamActive ? 60_000 : 30_000;
        await this.sleep(Math.min(5_000 * consecutiveErrors, maxBackoff));
      }
    }

    // Save final state only when cancelled (resume needed);
    // streamEnded paths clear resume state after writing final file
    if (this.messages.length > 0 && this.cancelFlag) {
      await this.saveResumeState();
    }
  }

  /**
   * Sleep helper that wakes early on cancel or stream end.
   * Uses the abort signal (fired by stop() and markStreamEnded()) instead of a
   * polling loop, reducing promise/timer creation from ~10 per 5s wait to just 1.
   */
  private sleep(delayMs: number): Promise<void> {
    return cancellableSleep(delayMs, this.abortController?.signal);
  }

  /**
   * Signal that the stream has ended — exits the polling loop promptly,
   * writes the final chat file, and clears resume state (successful completion).
   * Distinct from stop() which is for cancellation/shutdown.
   */
  markStreamEnded(): void {
    this.streamEnded = true;
    this.abortController?.abort();
  }

  /**
   * Stop downloading
   */
  stop(): void {
    this.running = false;
    this.cancelFlag = true;
    this.abortController?.abort();
  }

  /**
   * Get current message count (includes messages flushed to disk)
   */
  getMessageCount(): number {
    return this.totalMessageCount;
  }

  /**
   * Whether the downloader is currently running
   */
  isRunning(): boolean {
    return this.running;
  }
}
