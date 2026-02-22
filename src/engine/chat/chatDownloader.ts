/**
 * Chat Downloader
 *
 * Downloads live chat messages from YouTube streams, following the same
 * patterns as SegmentDownloader (EventEmitter, resume state, polling loop).
 */

import fs from "fs-extra";
import path from "path";
import { EventEmitter } from "events";
import { ChatApi } from "./chatApi.js";
import { Logger } from "../../core/logger.js";
import { LIMITS } from "../../constants/limits.js";
import { getErrorMessage } from "../../types/errors.js";
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
   * Write chat data to output file atomically (write to temp, then rename).
   *
   * Two paths:
   * - Not flushed: all messages are in memory, write complete JSON directly.
   * - Flushed: the on-disk file already has old messages. Instead of re-reading
   *   and re-parsing the entire file (which causes 5-10x memory overhead from
   *   JSON.parse + array merge + JSON.stringify), read the raw text, locate the
   *   closing ']', and append only the NEW messages that arrived after the flush.
   *   Memory cost: ~1x file size (raw string) instead of ~6-10x.
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

    // Read existing file as raw text (NOT JSON.parse — avoids massive object allocation)
    const raw = await fs.readFile(this.options.outputFile, "utf8");

    // Locate the end of the messages array
    const closingIdx = raw.lastIndexOf("]");
    if (closingIdx === -1) {
      // Corrupt file — fall back to writing what we have in memory
      this.logger.warn("[ChatDownloader] Corrupt chat file, rewriting with in-memory messages");
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

    // Build output: everything before ']' + new messages + closing structure
    const beforeBracket = raw.substring(0, closingIdx);
    const hasExisting = beforeBracket.trimEnd().endsWith("}");

    let content = beforeBracket;
    if (newMessages.length > 0) {
      content += hasExisting ? ",\n" : "";
      content += newMessages.map((m) => "    " + JSON.stringify(m)).join(",\n");
      content += "\n";
    }
    content += "  ]\n}";

    // Update metadata in the header (search only before "messages" key to avoid
    // matching message content that coincidentally contains these field names)
    const messagesKeyIdx = content.indexOf('"messages"');
    if (messagesKeyIdx > 0) {
      let header = content.substring(0, messagesKeyIdx);
      header = header.replace(/"messageCount": \d+/, `"messageCount": ${this.totalMessageCount}`);
      header = header.replace(/"downloadedAt": "[^"]*"/, `"downloadedAt": "${new Date().toISOString()}"`);
      content = header + content.substring(messagesKeyIdx);
    }

    await fs.writeFile(tmpPath, content);
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
   * Sleep helper that wakes early on cancel or stream end
   */
  private async sleep(delayMs: number): Promise<void> {
    const checkInterval = 500;
    let elapsed = 0;

    while (elapsed < delayMs && this.running && !this.cancelFlag && !this.streamEnded) {
      await new Promise((r) => setTimeout(r, Math.min(checkInterval, delayMs - elapsed)));
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
