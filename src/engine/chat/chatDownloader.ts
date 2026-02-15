/**
 * Chat Downloader
 *
 * Downloads live chat messages from YouTube streams, following the same
 * patterns as SegmentDownloader (EventEmitter, resume state, polling loop).
 */

import fs from "fs-extra";
import path from "path";
import ms from "ms";
import { EventEmitter } from "events";
import { ChatApi } from "./chatApi.js";
import { Logger } from "../../core/logger.js";
import { LIMITS } from "../../constants/limits.js";
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

  // Memory bounding: flush messages to disk when they exceed this threshold
  private static readonly FLUSH_THRESHOLD = LIMITS.CHAT_MESSAGES_FLUSH_THRESHOLD;
  private static readonly DEDUP_KEEP = LIMITS.CHAT_MESSAGES_IN_MEMORY; // Keep last N messages in memory after flush
  private totalMessageCount: number = 0;
  private flushedToDisk: boolean = false;

  constructor(options: ChatDownloaderOptions) {
    super();
    this.options = options;
    this.continuation = options.initialContinuation;
    this.chatApi = new ChatApi();
    this.logger = Logger.getInstance();

    // Parse stream start time for offset calculation
    if (options.streamStartTime) {
      const ms = new Date(options.streamStartTime).getTime();
      this.streamStartMs = Number.isNaN(ms) ? 0 : ms;
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
    const lastMessage = this.messages[this.messages.length - 1];
    const state: ChatResumeState = {
      lastTimestampUsec: lastMessage?.timestampUsec || "0",
      messageCount: this.totalMessageCount,
      continuation: this.continuation,
      timestamp: Date.now(),
      videoId: this.options.videoId,
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
   * Write chat data to output file atomically (write to temp, then rename).
   * If messages were previously flushed to disk, merges existing file with
   * current in-memory messages to produce the complete output.
   */
  private async writeChatFile(): Promise<void> {
    let allMessages = this.messages;

    if (this.flushedToDisk) {
      // Load previously flushed messages from disk and merge with in-memory tail
      const existing = await this.loadExistingMessages();
      if (existing.length > 0 && this.messages.length > 0) {
        // Find overlap between existing (flushed) and in-memory (tail)
        const firstInMemoryId = this.messages[0].id;
        const overlapIdx = existing.findIndex((m) => m.id === firstInMemoryId);
        if (overlapIdx >= 0) {
          allMessages = [...existing.slice(0, overlapIdx), ...this.messages];
        } else {
          allMessages = [...existing, ...this.messages];
        }
      } else if (existing.length > 0) {
        allMessages = existing;
      }
    }

    const chatData: ChatData = {
      videoId: this.options.videoId,
      videoTitle: this.options.videoTitle,
      channelName: this.options.channelName,
      streamStartTime: this.options.streamStartTime,
      downloadedAt: new Date().toISOString(),
      messageCount: allMessages.length,
      messages: allMessages,
    };

    await fs.ensureDir(path.dirname(this.options.outputFile));
    const tmpPath = this.options.outputFile + ".tmp";
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
        // Load existing messages and continuation
        this.messages = await this.loadExistingMessages();
        this.seenIds = new Set(this.messages.map((m) => m.id));
        this.totalMessageCount = this.messages.length;
        this.continuation = resumeState.continuation;
        isResuming = true;
        this.logger.info(
          `[ChatDownloader] Resuming from ${this.totalMessageCount} messages`,
        );
      }

      this.emit("start", { messageCount: this.totalMessageCount, resuming: isResuming });

      // Main polling loop
      await this.runChatLoop();

      // Write final chat file
      if (this.totalMessageCount > 0) {
        await this.writeChatFile();
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
    let saveCounter = 0;
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
              newInBatch++;
            }
          }

          // Emit progress
          const progress: ChatProgress = {
            messageCount: this.totalMessageCount,
            lastTimestamp: response.messages[response.messages.length - 1]?.timestampText,
          };
          this.emit("progress", progress);

          // Save periodically — count only deduplicated new messages,
          // and scale threshold with message count to reduce I/O on long streams
          saveCounter += newInBatch;
          const saveThreshold = Math.max(100, Math.floor(this.totalMessageCount * 0.1));
          if (saveCounter >= saveThreshold) {
            try {
              await this.writeChatFile();
              await this.saveResumeState();
            } catch (ioErr: any) {
              this.logger.debug(`[ChatDownloader] Save error: ${ioErr?.message}`);
            }
            saveCounter = 0;

            // Flush old messages from memory after persisting to disk.
            // Keep only the last DEDUP_KEEP messages for overlap detection on next write.
            if (this.messages.length > ChatDownloader.FLUSH_THRESHOLD) {
              this.messages = this.messages.slice(-ChatDownloader.DEDUP_KEEP);
              this.flushedToDisk = true;
              this.logger.debug(
                `[ChatDownloader] Flushed to disk, ${this.totalMessageCount} total, ${this.messages.length} in memory`,
              );
            }

            // Prune seenIds to prevent unbounded growth on long streams
            if (this.seenIds.size > LIMITS.CHAT_MESSAGES_FLUSH_THRESHOLD) {
              this.seenIds = new Set(this.messages.slice(-LIMITS.CHAT_MESSAGES_FLUSH_THRESHOLD).map(m => m.id));
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
              await this.sleep(10000);
              continue;
            }
            // No fresh continuation — retry with exponential backoff
            this.logger.debug("[ChatDownloader] No fresh continuation available — retrying with backoff");
            let contRetryDelay = 10000; // Start at 10s
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
              contRetryDelay = Math.min(contRetryDelay * 2, ms("5m"));
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
        const waitMs = response.timeoutMs ?? (this.options.isReplay ? 0 : 5000);
        if (waitMs > 0) {
          await this.sleep(waitMs);
        }
      } catch (e: any) {
        // If aborted (stop/markStreamEnded), exit the loop without counting as error
        if (e?.name === "AbortError") {
          break;
        }

        consecutiveErrors++;
        this.logger.debug(`[ChatDownloader] Error: ${e.message}`);

        // Higher tolerance for live/upcoming streams (transient issues are normal)
        const maxErrors = this.isStreamActive ? 20 : 5;
        if (consecutiveErrors > maxErrors) {
          this.logger.warn("[ChatDownloader] Too many errors, stopping");
          this.emit("error", new Error("Too many consecutive chat API errors"));
          break;
        }

        // Exponential backoff (cap at 30s for VOD, 60s for live)
        const maxBackoff = this.isStreamActive ? 60000 : 30000;
        await this.sleep(Math.min(5000 * consecutiveErrors, maxBackoff));
      }
    }

    // Save final state only when cancelled (resume needed);
    // streamEnded paths clear resume state after writing final file
    if (this.messages.length > 0 && this.cancelFlag) {
      await this.saveResumeState();
    }
  }

  /**
   * Sleep helper that wakes early on cancel or stream end
   */
  private async sleep(ms: number): Promise<void> {
    const checkInterval = 500;
    let elapsed = 0;

    while (elapsed < ms && this.running && !this.cancelFlag && !this.streamEnded) {
      await new Promise((r) => setTimeout(r, Math.min(checkInterval, ms - elapsed)));
      elapsed += checkInterval;
    }
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
