/**
 * Twitch Chat Downloader
 *
 * Downloads live chat messages via IRC WebSocket connection.
 * Follows the same event interface as YouTube's ChatDownloader:
 * - Emits: start, progress, finish, error
 * - Same flush threshold (50k messages, keep last 5k)
 * - Same atomic write pattern (.tmp + rename)
 * - Resume state sidecar (.resume.json)
 */

import fs from "fs-extra";
import path from "path";
import { EventEmitter } from "events";
import WebSocket from "ws";
import { Logger } from "../../core/logger.js";
import { TWITCH_URLS, TWITCH_IRC } from "../../constants.js";
import { getErrorMessage } from "../../types/errors.js";
import type { TwitchChatMessage, TwitchChatData, TwitchMessageType } from "../../types/twitch.js";
import { resolveChannelEmotes } from "./twitchEmotes.js";

export interface TwitchChatDownloaderOptions {
  channelLogin: string;
  channelDisplayName: string;
  channelId?: string;         // Numeric Twitch user ID (for emote resolution)
  streamId: string;
  outputFile: string;         // Path to chat.json in staging
  streamStartTime?: string;   // ISO timestamp for calculating offsetMs
  authToken?: string | null;  // OAuth token for authenticated access
  resumeFile?: string;
}

interface TwitchChatResumeState {
  messageCount: number;
  lastTimestampMs: number;
  timestamp: number;
  streamId: string;
}

/**
 * Parse IRC tags from a TMI message line.
 * Format: @key=value;key2=value2 :prefix COMMAND #channel :message
 */
function parseTags(tagStr: string): Map<string, string> {
  const tags = new Map<string, string>();
  if (!tagStr) return tags;

  for (const pair of tagStr.split(";")) {
    const eqIdx = pair.indexOf("=");
    if (eqIdx === -1) {
      tags.set(pair, "");
    } else {
      tags.set(pair.substring(0, eqIdx), pair.substring(eqIdx + 1));
    }
  }
  return tags;
}

/**
 * Parse a single IRC message line into structured data.
 */
function parseIrcMessage(line: string): {
  tags: Map<string, string>;
  prefix: string;
  command: string;
  params: string[];
  trailing: string;
} | null {
  if (!line) return null;

  let cursor = 0;
  let tags = new Map<string, string>();

  // Parse tags (@key=value;...)
  if (line[cursor] === "@") {
    const spaceIdx = line.indexOf(" ", cursor);
    if (spaceIdx === -1) return null;
    tags = parseTags(line.substring(1, spaceIdx));
    cursor = spaceIdx + 1;
  }

  // Parse prefix (:prefix)
  let prefix = "";
  if (line[cursor] === ":") {
    const spaceIdx = line.indexOf(" ", cursor);
    if (spaceIdx === -1) return null;
    prefix = line.substring(cursor + 1, spaceIdx);
    cursor = spaceIdx + 1;
  }

  // Parse command and params
  const rest = line.substring(cursor);
  const trailingIdx = rest.indexOf(" :");
  let commandAndParams: string;
  let trailing = "";

  if (trailingIdx !== -1) {
    commandAndParams = rest.substring(0, trailingIdx);
    trailing = rest.substring(trailingIdx + 2);
  } else {
    commandAndParams = rest;
  }

  const parts = commandAndParams.split(" ").filter(Boolean);
  const command = parts[0] || "";
  const params = parts.slice(1);

  return { tags, prefix, command, params, trailing };
}

/**
 * Parse emote positions from the IRC emotes tag.
 * Format: "id:start-end,start-end/id:start-end"
 */
function parseEmotes(emoteTag: string, message: string): TwitchChatMessage["emotes"] {
  if (!emoteTag) return undefined;

  const emotes: NonNullable<TwitchChatMessage["emotes"]> = [];

  for (const entry of emoteTag.split("/")) {
    const [id, positions] = entry.split(":");
    if (!id || !positions) continue;

    for (const pos of positions.split(",")) {
      const [startStr, endStr] = pos.split("-");
      const start = parseInt(startStr, 10);
      const end = parseInt(endStr, 10);
      if (isNaN(start) || isNaN(end)) continue;

      const name = message.substring(start, end + 1);
      emotes.push({ id, name, start, end });
    }
  }

  return emotes.length > 0 ? emotes : undefined;
}

export class TwitchChatDownloader extends EventEmitter {
  private options: TwitchChatDownloaderOptions;
  private logger: Logger;
  private ws: WebSocket | null = null;
  private running = false;
  private cancelFlag = false;
  private streamEnded = false;

  private messages: TwitchChatMessage[] = [];
  private seenIds = new Set<string>();
  private totalMessageCount = 0;
  private flushedToDisk = false;
  private streamStartMs = 0;
  private lastSaveMs = 0;
  private saving = false;

  constructor(options: TwitchChatDownloaderOptions) {
    super();
    this.options = options;
    this.logger = Logger.getInstance();

    if (options.streamStartTime) {
      const startMs = new Date(options.streamStartTime).getTime();
      this.streamStartMs = Number.isNaN(startMs) ? 0 : startMs;
    }
  }

  /**
   * Start downloading chat messages.
   */
  async start(): Promise<void> {
    if (this.running) return;
    this.running = true;
    this.cancelFlag = false;
    this.streamEnded = false;

    // Try to resume
    const resumeState = await this.loadResumeState();
    if (resumeState) {
      this.totalMessageCount = resumeState.messageCount;
      this.flushedToDisk = true; // Ensure final write merges old + new messages
      this.logger.info(
        `[TwitchChat] Resuming from ${this.totalMessageCount} messages`,
      );
      // Load existing messages for dedup
      const existing = await this.loadExistingMessages();
      for (const msg of existing) this.seenIds.add(msg.id);
    }

    this.emit("start");

    try {
      await this.connect();
    } catch (e) {
      this.emit("error", e);
    } finally {
      // Write final chat file with emote data
      await this.writeChatFile(true);
      await this.clearResumeState();
      this.running = false;
      this.emit("finish");
    }
  }

  /**
   * Stop the chat downloader.
   */
  stop(): void {
    this.cancelFlag = true;
    if (this.ws) {
      try {
        this.ws.close();
      } catch {
        // Ignore close errors
      }
      this.ws = null;
    }
  }

  /**
   * Mark the stream as ended — the loop will exit after current iteration.
   */
  markStreamEnded(): void {
    this.streamEnded = true;
    // Give the loop a moment to finish, then force close
    setTimeout(() => {
      if (this.ws && this.ws.readyState === WebSocket.OPEN) {
        this.ws.close();
      }
    }, 500);
  }

  isRunning(): boolean {
    return this.running;
  }

  getMessageCount(): number {
    return this.totalMessageCount;
  }

  /**
   * Connect to Twitch IRC and start receiving messages.
   */
  private connect(): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      const ws = new WebSocket(TWITCH_URLS.IRC_WS);
      this.ws = ws;

      let resolved = false;
      let consecutiveErrors = 0;

      ws.on("open", () => {
        this.logger.info(`[TwitchChat] Connected to IRC for #${this.options.channelLogin}`);

        // Authenticate (anonymous or with token)
        // Note: Twitch IRC ignores the NICK for authenticated users and uses the token's identity
        if (this.options.authToken) {
          ws.send(`PASS oauth:${this.options.authToken}`);
          ws.send(`NICK justinfan${Math.floor(Math.random() * 99999)}`);
        } else {
          ws.send("PASS SCHMOOPIIE");
          ws.send(`NICK justinfan${Math.floor(Math.random() * 99999)}`);
        }

        // Request capabilities for tags, commands, membership
        ws.send("CAP REQ :twitch.tv/tags twitch.tv/commands twitch.tv/membership");

        // Join channel
        ws.send(`JOIN #${this.options.channelLogin}`);
      });

      ws.on("message", (data) => {
        const raw = data.toString();
        for (const line of raw.split("\r\n")) {
          if (!line) continue;

          // Handle PING/PONG keepalive
          if (line.startsWith("PING")) {
            ws.send("PONG :tmi.twitch.tv");
            continue;
          }

          try {
            this.handleIrcLine(line);
            consecutiveErrors = 0;
          } catch (e) {
            consecutiveErrors++;
            if (consecutiveErrors > TWITCH_IRC.MAX_CONSECUTIVE_ERRORS) {
              this.logger.error(`[TwitchChat] Too many parse errors, disconnecting`);
              ws.close();
            }
          }
        }
      });

      ws.on("close", () => {
        this.logger.info(`[TwitchChat] IRC disconnected for #${this.options.channelLogin}`);
        this.ws = null;

        if (!resolved) {
          resolved = true;
          // If stream ended or cancelled, resolve normally
          if (this.cancelFlag || this.streamEnded) {
            resolve();
          } else {
            // Unexpected disconnect — try to reconnect
            this.reconnect().then(resolve).catch(reject);
          }
        }
      });

      ws.on("error", (err) => {
        this.logger.warn(`[TwitchChat] IRC error: ${err.message}`);
        if (!resolved) {
          resolved = true;
          reject(err);
        }
      });
    });
  }

  /**
   * Attempt to reconnect after an unexpected disconnect.
   */
  private async reconnect(): Promise<void> {
    const MAX_RECONNECTS = 10;
    let attempts = 0;

    while (!this.cancelFlag && !this.streamEnded && attempts < MAX_RECONNECTS) {
      attempts++;
      const delay = Math.min(1000 * Math.pow(2, attempts), 30000);
      this.logger.info(
        `[TwitchChat] Reconnecting in ${delay / 1000}s (attempt ${attempts}/${MAX_RECONNECTS})`,
      );
      await new Promise(r => setTimeout(r, delay));

      if (this.cancelFlag || this.streamEnded) break;

      try {
        await this.connect();
        return; // Reconnect succeeded
      } catch {
        // Will retry
      }
    }
  }

  /**
   * Handle a single IRC line.
   */
  private handleIrcLine(line: string): void {
    const parsed = parseIrcMessage(line);
    if (!parsed) return;

    const { tags, command, trailing } = parsed;

    switch (command) {
      case "PRIVMSG":
        this.handlePrivmsg(tags, trailing, line);
        break;
      case "USERNOTICE":
        this.handleUsernotice(tags, trailing, line);
        break;
      case "CLEARCHAT":
      case "CLEARMSG":
      case "ROOMSTATE":
      case "USERSTATE":
      case "GLOBALUSERSTATE":
      case "NOTICE":
        // Silently ignore meta messages
        break;
    }
  }

  /**
   * Handle a PRIVMSG (regular chat message or bits/cheer).
   */
  private handlePrivmsg(tags: Map<string, string>, message: string, raw: string): void {
    const msgId = tags.get("id");
    if (!msgId) return;
    if (this.seenIds.has(msgId)) return;

    const timestampMs = parseInt(tags.get("tmi-sent-ts") || "0", 10) || Date.now();
    const bits = parseInt(tags.get("bits") || "0", 10);

    const chatMsg: TwitchChatMessage = {
      id: msgId,
      timestampMs,
      offsetMs: this.streamStartMs > 0 ? timestampMs - this.streamStartMs : 0,
      authorName: tags.get("display-name") || tags.get("login") || "Anonymous",
      authorId: tags.get("user-id") || "",
      authorBadges: (tags.get("badges") || "").split(",").filter(Boolean),
      authorColor: tags.get("color") || undefined,
      message,
      emotes: parseEmotes(tags.get("emotes") || "", message),
      bits: bits > 0 ? bits : undefined,
      messageType: bits > 0 ? "bits" : "chat",
      raw,
    };

    this.addMessage(chatMsg);
  }

  /**
   * Handle a USERNOTICE (sub, resub, subgift, raid, etc.).
   */
  private handleUsernotice(tags: Map<string, string>, message: string, raw: string): void {
    const msgId = tags.get("id");
    if (!msgId) return;
    if (this.seenIds.has(msgId)) return;

    const timestampMs = parseInt(tags.get("tmi-sent-ts") || "0", 10) || Date.now();
    const msgType = tags.get("msg-id") || "system";

    let messageType: TwitchMessageType = "system";
    if (msgType === "sub") messageType = "sub";
    else if (msgType === "resub") messageType = "resub";
    else if (msgType === "subgift" || msgType === "submysterygift") messageType = "subgift";
    else if (msgType === "raid") messageType = "raid";

    const systemMsg = tags.get("system-msg")?.replace(/\\s/g, " ") || "";

    const chatMsg: TwitchChatMessage = {
      id: msgId,
      timestampMs,
      offsetMs: this.streamStartMs > 0 ? timestampMs - this.streamStartMs : 0,
      authorName: tags.get("display-name") || tags.get("login") || "System",
      authorId: tags.get("user-id") || "",
      authorBadges: (tags.get("badges") || "").split(",").filter(Boolean),
      authorColor: tags.get("color") || undefined,
      message: message || systemMsg,
      messageType,
      raw,
    };

    this.addMessage(chatMsg);
  }

  /**
   * Add a message, handle dedup and periodic saving.
   */
  private addMessage(msg: TwitchChatMessage): void {
    this.seenIds.add(msg.id);
    this.messages.push(msg);
    this.totalMessageCount++;

    // Emit progress periodically
    if (this.totalMessageCount % 100 === 0) {
      this.emit("progress", { messageCount: this.totalMessageCount });
    }

    // Save to disk at most once per second (skip if a save is already in flight)
    const now = Date.now();
    if (!this.saving && now - this.lastSaveMs >= 1000) {
      this.lastSaveMs = now;
      this.saving = true;
      this.periodicSave()
        .catch(e => this.logger.debug(`[TwitchChat] Save error: ${getErrorMessage(e)}`))
        .finally(() => { this.saving = false; });
    }
  }

  /**
   * Save chat to disk and manage memory.
   * Called periodically via addMessage() and on clean exit.
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
        `[TwitchChat] Flushed to disk, ${this.totalMessageCount} total, ${this.messages.length} in memory`,
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
      streamId: this.options.streamId,
      streamStartTime: this.options.streamStartTime,
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
        this.logger.warn(`[TwitchChat] Failed to resolve emotes: ${e instanceof Error ? e.message : String(e)}`);
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

  private async loadResumeState(): Promise<TwitchChatResumeState | null> {
    try {
      const resumePath = this.getResumeFilePath();
      if (await fs.pathExists(resumePath)) {
        const data = await fs.readJson(resumePath);
        if (data.streamId === this.options.streamId) {
          return data as TwitchChatResumeState;
        }
      }
    } catch {
      // Ignore
    }
    return null;
  }

  private async saveResumeState(): Promise<void> {
    const lastMsg = this.messages[this.messages.length - 1];
    const state: TwitchChatResumeState = {
      messageCount: this.totalMessageCount,
      lastTimestampMs: lastMsg?.timestampMs || 0,
      timestamp: Date.now(),
      streamId: this.options.streamId,
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
