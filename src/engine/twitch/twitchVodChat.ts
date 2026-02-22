/**
 * Twitch VOD Chat Replay Downloader
 *
 * Downloads chat messages from Twitch VOD replays via the GQL API.
 * Implements the same interface as TwitchChatDownloader:
 * - Emits: start, progress, finish, error
 * - Writes to disk within 1-second batching window, clears memory after each write
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
  recentIds?: string[];
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
  private lastOffsetSec = 0;

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

    // Try to resume from state, or detect existing chat file from a previous session
    let offsetSeconds = 0;
    const resumeState = await this.loadResumeState();
    if (resumeState?.recentIds) {
      // Restore dedup set directly from resume state — no file I/O needed
      this.totalMessageCount = resumeState.messageCount;
      this.flushedToDisk = true;
      this.seenIds = new Set(resumeState.recentIds);
      this.lastOffsetSec = resumeState.lastOffsetSeconds;
      offsetSeconds = resumeState.lastOffsetSeconds;
      this.logger.info(
        `[TwitchVodChat] Resuming from ${this.totalMessageCount} messages (offset ${offsetSeconds}s)`,
      );
    } else {
      // Legacy resume state without recentIds — load file for dedup IDs
      const existing = await this.loadExistingMessages();
      if (existing.length > 0) {
        this.totalMessageCount = resumeState?.messageCount ?? existing.length;
        this.flushedToDisk = true;
        const tail = existing.slice(-TWITCH_IRC.DEDUP_IDS);
        this.seenIds = new Set(tail.map(m => m.id));
        if (resumeState) {
          offsetSeconds = resumeState.lastOffsetSeconds;
        } else {
          const lastMsg = existing[existing.length - 1];
          if (lastMsg?.offsetMs) offsetSeconds = lastMsg.offsetMs / 1000;
        }
        this.logger.info(
          `[TwitchVodChat] Resuming from ${this.totalMessageCount} messages (offset ${offsetSeconds}s)`,
        );
      }
    }

    this.emit("start");

    try {
      await this.downloadLoop(offsetSeconds);
    } catch (e) {
      this.emit("error", e);
    } finally {
      // Write any remaining unwritten messages
      if (this.messages.length > 0) {
        await this.writeChatFile();
        this.messages = [];
        this.flushedToDisk = true;
      }
      // Resolve and inject emotes on final write
      if (this.totalMessageCount > 0 && this.options.channelId) {
        await this.injectEmotes();
      }
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
        await new Promise(r => setTimeout(r, 2_000 * consecutiveErrors));
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
    this.lastOffsetSec = msg.offsetMs / 1000;

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
    if (!this.saving && now - this.lastSaveMs >= 1_000) {
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

    // All messages now on disk — clear from memory
    this.messages = [];
    this.flushedToDisk = true;

    // Bound seenIds to prevent unbounded growth
    if (this.seenIds.size > TWITCH_IRC.DEDUP_IDS) {
      const arr = [...this.seenIds];
      this.seenIds = new Set(arr.slice(-TWITCH_IRC.DEDUP_IDS));
    }

    await this.saveResumeState();
  }

  // =========================================================================
  // File I/O
  // =========================================================================

  /**
   * Write chat data to output file. Two paths:
   * - Not flushed: all messages in memory, write complete JSON.
   * - Flushed: append only new messages via raw string manipulation
   *   (avoids JSON.parse + merge memory overhead).
   */
  private async writeChatFile(): Promise<void> {
    await fs.ensureDir(path.dirname(this.options.outputFile));
    const tmpPath = this.options.outputFile + ".tmp";

    if (!this.flushedToDisk) {
      // All messages in memory — write complete file
      const chatData: TwitchChatData = {
        platform: "twitch",
        channelLogin: this.options.channelLogin,
        channelDisplayName: this.options.channelDisplayName,
        streamId: this.options.vodId,
        downloadedAt: new Date().toISOString(),
        messageCount: this.totalMessageCount,
        messages: this.messages,
      };
      await fs.writeJson(tmpPath, chatData, { spaces: 2 });
      await fs.move(tmpPath, this.options.outputFile, { overwrite: true });
      return;
    }

    // Flushed path: append only new messages via raw string manipulation
    const newMessages = this.messages;

    if (!(await fs.pathExists(this.options.outputFile))) {
      // Disk file missing — write what we have
      const chatData: TwitchChatData = {
        platform: "twitch",
        channelLogin: this.options.channelLogin,
        channelDisplayName: this.options.channelDisplayName,
        streamId: this.options.vodId,
        downloadedAt: new Date().toISOString(),
        messageCount: this.totalMessageCount,
        messages: this.messages,
      };
      await fs.writeJson(tmpPath, chatData, { spaces: 2 });
      await fs.move(tmpPath, this.options.outputFile, { overwrite: true });
      return;
    }

    // Read existing file as raw text (NOT JSON.parse)
    const raw = await fs.readFile(this.options.outputFile, "utf8");
    const closingIdx = raw.lastIndexOf("]");
    if (closingIdx === -1) {
      this.logger.warn("[TwitchVodChat] Corrupt chat file, rewriting with in-memory messages");
      const chatData: TwitchChatData = {
        platform: "twitch",
        channelLogin: this.options.channelLogin,
        channelDisplayName: this.options.channelDisplayName,
        streamId: this.options.vodId,
        downloadedAt: new Date().toISOString(),
        messageCount: this.totalMessageCount,
        messages: this.messages,
      };
      await fs.writeJson(tmpPath, chatData, { spaces: 2 });
      await fs.move(tmpPath, this.options.outputFile, { overwrite: true });
      return;
    }

    // Build output: everything before ']' + new messages + closing
    const beforeBracket = raw.substring(0, closingIdx);
    const hasExisting = beforeBracket.trimEnd().endsWith("}");

    let content = beforeBracket;
    if (newMessages.length > 0) {
      content += hasExisting ? ",\n" : "";
      content += newMessages.map(m => "    " + JSON.stringify(m)).join(",\n");
      content += "\n";
    }
    content += "  ]\n}";

    // Update metadata in the header
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
   * Resolve third-party emotes and inject into the final chat file.
   * Called once after the download loop exits.
   */
  private async injectEmotes(): Promise<void> {
    try {
      const emotes = await resolveChannelEmotes(
        this.options.channelId!,
        this.options.channelLogin,
      );
      if (!emotes) return;

      if (!(await fs.pathExists(this.options.outputFile))) return;
      const raw = await fs.readFile(this.options.outputFile, "utf8");
      const lastBrace = raw.lastIndexOf("}");
      if (lastBrace === -1) return;

      // Insert emotes field before closing brace
      const before = raw.substring(0, lastBrace).trimEnd();
      const content = `${before},\n  "emotes": ${JSON.stringify(emotes)}\n}`;

      const tmpPath = this.options.outputFile + ".tmp";
      await fs.writeFile(tmpPath, content);
      await fs.move(tmpPath, this.options.outputFile, { overwrite: true });
    } catch (e: unknown) {
      this.logger.warn(`[TwitchVodChat] Failed to resolve emotes: ${e instanceof Error ? e.message : String(e)}`);
    }
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
    const state: TwitchVodChatResumeState = {
      messageCount: this.totalMessageCount,
      lastOffsetSeconds: this.lastOffsetSec,
      timestamp: Date.now(),
      vodId: this.options.vodId,
      recentIds: [...this.seenIds],
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
