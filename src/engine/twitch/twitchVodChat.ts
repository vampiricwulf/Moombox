/**
 * Twitch VOD Chat Replay Downloader
 *
 * Downloads chat messages from Twitch VOD replays via the GQL API.
 * Implements the same interface as TwitchChatDownloader:
 * - Emits: start, progress, finish, error
 * - Same flush threshold (50k messages, keep last 5k)
 * - Same atomic write pattern (.tmp + rename)
 * - Resume state sidecar (.resume.json)
 */

import fs from "fs-extra";
import path from "path";
import { EventEmitter } from "events";
import { Logger } from "../../core/logger.js";
import { TWITCH_IRC } from "../../constants.js";
import { getErrorMessage } from "../../types/errors.js";
import type { TwitchChatMessage, TwitchChatData } from "../../types/twitch.js";
import { getVodComments, type VodCommentEdge } from "./twitchApi.js";
import { resolveChannelEmotes } from "./twitchEmotes.js";

export interface TwitchVodChatDownloaderOptions {
  vodId: string;
  channelLogin: string;
  channelDisplayName: string;
  channelId?: string;
  outputFile: string;
  vodDuration?: number;       // Duration in seconds (for progress percentage)
  vodCreatedAt?: string;      // ISO timestamp of VOD creation (for timestampMs)
  authToken?: string | null;
  resumeFile?: string;
}

interface TwitchVodChatResumeState {
  messageCount: number;
  lastOffsetSeconds: number;
  timestamp: number;
  vodId: string;
}

export class TwitchVodChatDownloader extends EventEmitter {
  private options: TwitchVodChatDownloaderOptions;
  private logger: Logger;
  private running = false;
  private cancelFlag = false;

  private messages: TwitchChatMessage[] = [];
  private seenIds = new Set<string>();
  private totalMessageCount = 0;
  private flushedToDisk = false;
  private vodStartMs = 0;
  private lastSaveMs = 0;
  private saving = false;

  constructor(options: TwitchVodChatDownloaderOptions) {
    super();
    this.options = options;
    this.logger = Logger.getInstance();

    if (options.vodCreatedAt) {
      const startMs = new Date(options.vodCreatedAt).getTime();
      this.vodStartMs = Number.isNaN(startMs) ? 0 : startMs;
    }
  }

  async start(): Promise<void> {
    if (this.running) return;
    this.running = true;
    this.cancelFlag = false;

    // Try to resume
    let offsetSeconds = 0;
    const resumeState = await this.loadResumeState();
    if (resumeState) {
      this.totalMessageCount = resumeState.messageCount;
      this.flushedToDisk = true;
      offsetSeconds = resumeState.lastOffsetSeconds;
      this.logger.info(
        `[TwitchVodChat] Resuming from ${this.totalMessageCount} messages (offset ${offsetSeconds}s)`,
      );
      // Load existing messages for dedup
      const existing = await this.loadExistingMessages();
      for (const msg of existing) this.seenIds.add(msg.id);
    }

    this.emit("start");

    try {
      await this.downloadLoop(offsetSeconds);
    } catch (e) {
      this.emit("error", e);
    } finally {
      await this.writeChatFile(true);
      await this.clearResumeState();
      this.running = false;
      this.emit("finish");
    }
  }

  stop(): void {
    this.cancelFlag = true;
  }

  markStreamEnded(): void {
    // No-op for VOD — download completes when all pages are fetched
  }

  isRunning(): boolean {
    return this.running;
  }

  getMessageCount(): number {
    return this.totalMessageCount;
  }

  private async downloadLoop(startOffset: number): Promise<void> {
    let offsetSeconds = startOffset;
    let consecutiveErrors = 0;
    const MAX_CONSECUTIVE_ERRORS = 5;

    while (this.running && !this.cancelFlag) {
      try {
        const result = await getVodComments(
          this.options.vodId,
          offsetSeconds,
          this.options.authToken,
        );

        consecutiveErrors = 0;

        if (result.edges.length === 0) {
          this.logger.info(
            `[TwitchVodChat] Reached end of VOD comments (${this.totalMessageCount} messages)`,
          );
          break;
        }

        // Track how many new (non-duplicate) messages this page added
        let newMessages = 0;
        let lastOffset = offsetSeconds;

        for (const edge of result.edges) {
          const msg = this.convertComment(edge);
          if (!msg) continue;
          if (this.seenIds.has(msg.id)) continue;
          this.addMessage(msg);
          newMessages++;
          lastOffset = edge.node.contentOffsetSeconds;
        }

        // If no new messages, we've exhausted this offset — done
        if (newMessages === 0) {
          this.logger.info(
            `[TwitchVodChat] All VOD comments fetched (${this.totalMessageCount} messages)`,
          );
          break;
        }

        // Advance offset to last message's position for next page
        offsetSeconds = lastOffset;

        if (!result.hasNextPage) {
          this.logger.info(
            `[TwitchVodChat] All VOD comments fetched (${this.totalMessageCount} messages)`,
          );
          break;
        }
      } catch (e) {
        consecutiveErrors++;
        this.logger.warn(
          `[TwitchVodChat] Error fetching comments (${consecutiveErrors}/${MAX_CONSECUTIVE_ERRORS}): ${getErrorMessage(e)}`,
        );
        if (consecutiveErrors >= MAX_CONSECUTIVE_ERRORS) {
          throw new Error(
            `VOD chat download failed after ${MAX_CONSECUTIVE_ERRORS} consecutive errors: ${getErrorMessage(e)}`,
          );
        }
        // Brief backoff before retry
        await new Promise(r => setTimeout(r, 2000 * consecutiveErrors));
      }
    }
  }

  private convertComment(edge: VodCommentEdge): TwitchChatMessage | null {
    const node = edge.node;
    if (!node.id) return null;

    const offsetMs = node.contentOffsetSeconds * 1000;
    const timestampMs = this.vodStartMs > 0 ? this.vodStartMs + offsetMs : offsetMs;

    // Join message fragments
    const messageText = node.message.fragments.map(f => f.text).join("");

    // Extract emote info from fragments
    const emotes: TwitchChatMessage["emotes"] = [];
    let charOffset = 0;
    for (const fragment of node.message.fragments) {
      if (fragment.emote) {
        emotes.push({
          id: fragment.emote.emoteID,
          name: fragment.text,
          start: charOffset,
          end: charOffset + fragment.text.length - 1,
        });
      }
      charOffset += fragment.text.length;
    }

    // Format badges as "setID/version" strings
    const badges = node.message.userBadges.map(b => `${b.setID}/${b.version}`);

    return {
      id: node.id,
      timestampMs,
      offsetMs,
      authorName: node.commenter?.displayName || "Deleted User",
      authorId: node.commenter?.id || "",
      authorBadges: badges,
      authorColor: node.message.userColor || undefined,
      message: messageText,
      emotes: emotes.length > 0 ? emotes : undefined,
      messageType: "chat",
      raw: "", // No raw IRC line for VOD replay
    };
  }

  private addMessage(msg: TwitchChatMessage): void {
    this.seenIds.add(msg.id);
    this.messages.push(msg);
    this.totalMessageCount++;

    // Emit progress periodically
    if (this.totalMessageCount % 500 === 0) {
      const progressData: { messageCount: number; offsetSeconds?: number; percent?: number } = {
        messageCount: this.totalMessageCount,
        offsetSeconds: msg.offsetMs / 1000,
      };
      if (this.options.vodDuration && this.options.vodDuration > 0) {
        progressData.percent = Math.min(
          100,
          Math.round(((msg.offsetMs / 1000) / this.options.vodDuration) * 100),
        );
      }
      this.emit("progress", progressData);
    }

    // Save to disk at most once per second (skip if a save is already in flight)
    const now = Date.now();
    if (!this.saving && now - this.lastSaveMs >= 1000) {
      this.lastSaveMs = now;
      this.saving = true;
      this.periodicSave()
        .catch(e => this.logger.debug(`[TwitchVodChat] Save error: ${getErrorMessage(e)}`))
        .finally(() => { this.saving = false; });
    }
  }

  /**
   * Save chat to disk and manage memory.
   */
  private async periodicSave(): Promise<void> {
    await this.writeChatFile();
    await this.saveResumeState();

    // Flush old messages from memory after persisting to disk
    if (this.messages.length > TWITCH_IRC.FLUSH_THRESHOLD) {
      this.flushedToDisk = true;
      const keep = this.messages.slice(-TWITCH_IRC.KEEP_IN_MEMORY);
      this.seenIds = new Set(keep.map(m => m.id));
      this.messages = keep;
      this.logger.debug(
        `[TwitchVodChat] Flushed to disk, ${this.totalMessageCount} total, ${this.messages.length} in memory`,
      );
    }

    // Prune seenIds to prevent unbounded growth
    if (this.seenIds.size > TWITCH_IRC.FLUSH_THRESHOLD) {
      this.seenIds = new Set(this.messages.slice(-TWITCH_IRC.FLUSH_THRESHOLD).map(m => m.id));
    }
  }

  // =========================================================================
  // File I/O
  // =========================================================================

  private async writeChatFile(isFinal = false): Promise<void> {
    let allMessages = this.messages;

    if (this.flushedToDisk) {
      const existing = await this.loadExistingMessages();
      if (existing.length > 0 && this.messages.length > 0) {
        const firstInMemoryId = this.messages[0].id;
        const overlapIdx = existing.findIndex(m => m.id === firstInMemoryId);
        if (overlapIdx >= 0) {
          allMessages = [...existing.slice(0, overlapIdx), ...this.messages];
        } else {
          allMessages = [...existing, ...this.messages];
        }
      } else if (existing.length > 0) {
        allMessages = existing;
      }
    }

    const chatData: TwitchChatData = {
      platform: "twitch",
      channelLogin: this.options.channelLogin,
      channelDisplayName: this.options.channelDisplayName,
      streamId: this.options.vodId, // Use vodId as streamId for VODs
      downloadedAt: new Date().toISOString(),
      messageCount: allMessages.length,
      messages: allMessages,
    };

    // Resolve third-party emotes on final write
    if (isFinal && this.options.channelId) {
      try {
        chatData.emotes = await resolveChannelEmotes(
          this.options.channelId,
          this.options.channelLogin,
        );
      } catch (e: unknown) {
        this.logger.warn(`[TwitchVodChat] Failed to resolve emotes: ${e instanceof Error ? e.message : String(e)}`);
      }
    }

    await fs.ensureDir(path.dirname(this.options.outputFile));
    const tmpPath = this.options.outputFile + ".tmp";
    await fs.writeJson(tmpPath, chatData, { spaces: 2 });
    await fs.move(tmpPath, this.options.outputFile, { overwrite: true });
  }

  private async loadExistingMessages(): Promise<TwitchChatMessage[]> {
    try {
      if (await fs.pathExists(this.options.outputFile)) {
        const data = await fs.readJson(this.options.outputFile);
        if (data.messages && Array.isArray(data.messages)) {
          return data.messages;
        }
      }
    } catch {
      // Ignore
    }
    return [];
  }

  private getResumeFilePath(): string {
    return this.options.resumeFile || `${this.options.outputFile}.resume.json`;
  }

  private async loadResumeState(): Promise<TwitchVodChatResumeState | null> {
    try {
      const resumePath = this.getResumeFilePath();
      if (await fs.pathExists(resumePath)) {
        const data = await fs.readJson(resumePath);
        if (data.vodId === this.options.vodId) {
          return data as TwitchVodChatResumeState;
        }
      }
    } catch {
      // Ignore
    }
    return null;
  }

  private async saveResumeState(): Promise<void> {
    const lastMsg = this.messages[this.messages.length - 1];
    const state: TwitchVodChatResumeState = {
      messageCount: this.totalMessageCount,
      lastOffsetSeconds: lastMsg ? lastMsg.offsetMs / 1000 : 0,
      timestamp: Date.now(),
      vodId: this.options.vodId,
    };
    try {
      const resumePath = this.getResumeFilePath();
      const tmpPath = resumePath + ".tmp";
      await fs.writeJson(tmpPath, state);
      await fs.move(tmpPath, resumePath, { overwrite: true });
    } catch {
      // Ignore
    }
  }

  private async clearResumeState(): Promise<void> {
    try {
      await fs.remove(this.getResumeFilePath());
    } catch {
      // Ignore
    }
  }
}
