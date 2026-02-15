/**
 * YouTube Live Chat API
 *
 * Handles extraction of chat continuation tokens and fetching chat messages
 * from YouTube's Innertube API.
 */

import ms from "ms";
import { Logger } from "../../core/logger.js";
import {
  YOUTUBE_URLS,
  USER_AGENTS,
  WEB_CLIENT,
  DEFAULT_API_KEY,
} from "../../constants.js";
import { fetchWithTimeout } from "../../core/http.js";
import type {
  ChatMessage,
  ChatApiResponse,
  MessagePart,
  SuperchatInfo,
} from "../../types/chat.js";

/**
 * Chat API for YouTube live chat
 */
export class ChatApi {
  private logger: Logger;

  constructor() {
    this.logger = Logger.getInstance();
  }

  /**
   * Fetch a fresh chat continuation token by loading the watch page.
   * Used when the current continuation goes stale (API returns "complete"
   * while the stream is still live).
   */
  async fetchFreshContinuation(
    videoId: string,
    cookieHeader?: string,
    signal?: AbortSignal,
  ): Promise<{ continuation: string | null; isReplay: boolean }> {
    try {
      const url = `${YOUTUBE_URLS.WATCH}?v=${videoId}`;
      const headers: Record<string, string> = {
        "User-Agent": USER_AGENTS.WEB,
      };
      if (cookieHeader) {
        headers["Cookie"] = cookieHeader;
      }

      const response = await fetchWithTimeout(url, { headers, signal }, ms("15s"));

      if (!response.ok) {
        this.logger.debug(`[ChatApi] Watch page fetch failed: HTTP ${response.status}`);
        return { continuation: null, isReplay: false };
      }

      const html = await response.text();
      return this.extractChatContinuation(html);
    } catch (e: any) {
      if (e?.name === "AbortError") throw e;
      this.logger.debug(`[ChatApi] Error fetching fresh continuation: ${e.message}`);
      return { continuation: null, isReplay: false };
    }
  }

  /**
   * Extract initial chat continuation token from watch page HTML
   *
   * The continuation is found in ytInitialData at path:
   * contents.twoColumnWatchNextResults.conversationBar.liveChatRenderer
   *   .continuations[0].reloadContinuationData.continuation
   */
  extractChatContinuation(html: string): {
    continuation: string | null;
    isReplay: boolean;
  } {
    try {
      // Extract ytInitialData from HTML
      const initialDataMatch = html.match(
        /var ytInitialData = ({.+?});<\/script>/s,
      );
      if (!initialDataMatch) {
        this.logger.debug("[ChatApi] ytInitialData not found in HTML");
        return { continuation: null, isReplay: false };
      }

      const ytInitialData = JSON.parse(initialDataMatch[1]);

      // Navigate to live chat renderer
      const conversationBar =
        ytInitialData?.contents?.twoColumnWatchNextResults?.conversationBar;
      const liveChatRenderer = conversationBar?.liveChatRenderer;

      if (!liveChatRenderer) {
        this.logger.debug("[ChatApi] No liveChatRenderer found - chat may be disabled");
        return { continuation: null, isReplay: false };
      }

      // Check if this is a replay (VOD with chat replay)
      const isReplay = liveChatRenderer?.isReplay === true;

      // Get continuation from continuations array
      const continuations = liveChatRenderer?.continuations;
      if (!continuations || continuations.length === 0) {
        this.logger.debug("[ChatApi] No continuations found in liveChatRenderer");
        return { continuation: null, isReplay };
      }

      // Try different continuation types
      let continuation: string | null = null;

      for (const cont of continuations) {
        if (cont.reloadContinuationData?.continuation) {
          continuation = cont.reloadContinuationData.continuation;
          break;
        }
        if (cont.invalidationContinuationData?.continuation) {
          continuation = cont.invalidationContinuationData.continuation;
          break;
        }
        if (cont.timedContinuationData?.continuation) {
          continuation = cont.timedContinuationData.continuation;
          break;
        }
        if (cont.liveChatReplayContinuationData?.continuation) {
          continuation = cont.liveChatReplayContinuationData.continuation;
          break;
        }
      }

      if (continuation) {
        this.logger.debug(
          `[ChatApi] Found chat continuation (isReplay: ${isReplay})`,
        );
      }

      return { continuation, isReplay };
    } catch (e) {
      this.logger.debug(`[ChatApi] Error extracting chat continuation: ${e}`);
      return { continuation: null, isReplay: false };
    }
  }

  /**
   * Fetch live chat messages from YouTube API
   */
  async fetchLiveChat(
    continuation: string,
    apiKey: string = DEFAULT_API_KEY,
    visitorData?: string,
    cookieHeader?: string,
    signal?: AbortSignal,
  ): Promise<ChatApiResponse> {
    const url = `${YOUTUBE_URLS.API}/live_chat/get_live_chat?key=${apiKey}`;
    return this.fetchChat(url, continuation, visitorData, cookieHeader, signal);
  }

  /**
   * Fetch chat replay for VOD
   */
  async fetchChatReplay(
    continuation: string,
    apiKey: string = DEFAULT_API_KEY,
    visitorData?: string,
    cookieHeader?: string,
    signal?: AbortSignal,
  ): Promise<ChatApiResponse> {
    const url = `${YOUTUBE_URLS.API}/live_chat/get_live_chat_replay?key=${apiKey}`;
    return this.fetchChat(url, continuation, visitorData, cookieHeader, signal);
  }

  /**
   * Internal method to fetch chat from either endpoint
   */
  private async fetchChat(
    url: string,
    continuation: string,
    visitorData?: string,
    cookieHeader?: string,
    signal?: AbortSignal,
  ): Promise<ChatApiResponse> {
    const headers: Record<string, string> = {
      "User-Agent": USER_AGENTS.WEB,
      "Content-Type": "application/json",
      Origin: YOUTUBE_URLS.BASE,
      Referer: YOUTUBE_URLS.BASE,
    };

    if (cookieHeader) {
      headers["Cookie"] = cookieHeader;
    }

    const body = {
      context: {
        client: {
          ...WEB_CLIENT.context,
          visitorData: visitorData || undefined,
        },
      },
      continuation,
    };

    try {
      const response = await fetchWithTimeout(url, {
        method: "POST",
        headers,
        body: JSON.stringify(body),
        signal,
      });

      if (!response.ok) {
        throw new Error(`Chat API error: HTTP ${response.status}`);
      }

      const data = await response.json();
      return this.parseResponse(data);
    } catch (e: any) {
      this.logger.debug(`[ChatApi] Fetch error: ${e.message}`);
      throw e;
    }
  }

  /**
   * Parse API response into ChatMessage array
   */
  private parseResponse(data: any): ChatApiResponse {
    const messages: ChatMessage[] = [];
    let nextContinuation: string | null = null;
    let timeoutMs = ms("5s"); // Default polling interval
    let isComplete = false;

    const liveChatContinuation = data?.continuationContents?.liveChatContinuation;

    if (!liveChatContinuation) {
      // No continuation contents means chat has ended
      return { messages: [], nextContinuation: null, timeoutMs: 0, isComplete: true };
    }

    // Extract next continuation
    const continuations = liveChatContinuation.continuations;
    if (continuations && continuations.length > 0) {
      for (const cont of continuations) {
        if (cont.timedContinuationData) {
          nextContinuation = cont.timedContinuationData.continuation;
          timeoutMs = cont.timedContinuationData.timeoutMs || ms("5s");
          break;
        }
        if (cont.invalidationContinuationData) {
          nextContinuation = cont.invalidationContinuationData.continuation;
          timeoutMs = cont.invalidationContinuationData.timeoutMs || ms("5s");
          break;
        }
        if (cont.liveChatReplayContinuationData) {
          nextContinuation = cont.liveChatReplayContinuationData.continuation;
          timeoutMs = cont.liveChatReplayContinuationData.timeoutMs || 0;
          break;
        }
      }
    }

    if (!nextContinuation) {
      isComplete = true;
    }

    // Parse actions (messages)
    const actions = liveChatContinuation.actions || [];

    for (const action of actions) {
      // Handle replay chat items (wrapped in replayChatItemAction)
      const chatAction = action.replayChatItemAction?.actions?.[0] || action;

      const addAction = chatAction.addChatItemAction;
      if (!addAction) continue;

      const item = addAction.item;
      if (!item) continue;

      // Handle different message types
      const renderer =
        item.liveChatTextMessageRenderer ||
        item.liveChatPaidMessageRenderer ||
        item.liveChatPaidStickerRenderer ||
        item.liveChatMembershipItemRenderer;

      if (!renderer) continue;

      const message = this.parseMessageRenderer(renderer, item);
      if (message) {
        // For replay, extract video offset from replayChatItemAction
        if (action.replayChatItemAction?.videoOffsetTimeMsec) {
          message.offsetMs = parseInt(
            action.replayChatItemAction.videoOffsetTimeMsec,
            10,
          );
        }
        messages.push(message);
      }
    }

    return { messages, nextContinuation, timeoutMs, isComplete };
  }

  /**
   * Parse a single message renderer into ChatMessage
   */
  private parseMessageRenderer(
    renderer: any,
    item: any,
  ): ChatMessage | null {
    try {
      const id = renderer.id || `gen-${renderer.timestampUsec || Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
      const timestampUsec = renderer.timestampUsec || "0";
      const timestampText =
        renderer.timestampText?.simpleText || this.formatTimestamp(timestampUsec);

      const authorName =
        renderer.authorName?.simpleText || renderer.authorName?.runs?.[0]?.text || "Unknown";
      const authorChannelId =
        renderer.authorExternalChannelId || "";

      // Parse author badges
      const authorBadges: string[] = [];
      const badges = renderer.authorBadges || [];
      for (const badge of badges) {
        const badgeRenderer =
          badge.liveChatAuthorBadgeRenderer || badge;
        const iconType = badgeRenderer.icon?.iconType;
        const tooltip = badgeRenderer.tooltip;

        if (iconType === "OWNER") authorBadges.push("owner");
        else if (iconType === "MODERATOR") authorBadges.push("moderator");
        else if (iconType === "VERIFIED") authorBadges.push("verified");
        else if (tooltip?.toLowerCase().includes("member"))
          authorBadges.push("member");
        else if (badgeRenderer.customThumbnail)
          authorBadges.push("member"); // Custom badge = membership
      }

      // Parse message content
      const messageParts: MessagePart[] = [];
      const messageRuns = renderer.message?.runs || [];

      for (const run of messageRuns) {
        if (run.text) {
          messageParts.push({ type: "text", text: run.text });
        } else if (run.emoji) {
          const emoji = run.emoji;
          const emojiId = emoji.emojiId || emoji.shortcuts?.[0] || "";
          const emojiUrl =
            emoji.image?.thumbnails?.[0]?.url || "";
          const isCustomEmoji = emoji.isCustomEmoji || false;

          messageParts.push({
            type: "emoji",
            emojiId,
            emojiUrl,
            isCustomEmoji,
          });
        }
      }

      // Parse superchat info if present
      let superchat: SuperchatInfo | undefined;
      if (item.liveChatPaidMessageRenderer || item.liveChatPaidStickerRenderer) {
        const paidRenderer = item.liveChatPaidMessageRenderer || item.liveChatPaidStickerRenderer;
        superchat = {
          amount: paidRenderer.purchaseAmountText?.simpleText || "",
          currency: this.extractCurrency(paidRenderer.purchaseAmountText?.simpleText || ""),
          color: this.getSuperchatColor(paidRenderer.headerBackgroundColor || 0),
          tier: this.getSuperchatTier(paidRenderer.headerBackgroundColor || 0),
        };
      }

      // Check if membership message
      const isMembership = !!item.liveChatMembershipItemRenderer;

      return {
        id,
        timestampUsec,
        timestampText,
        offsetMs: 0, // Will be set by caller for replay
        authorName,
        authorChannelId,
        authorBadges,
        message: messageParts,
        superchat,
        isMembership,
      };
    } catch (e) {
      this.logger.debug(`[ChatApi] Error parsing message: ${e}`);
      return null;
    }
  }

  /**
   * Format microsecond timestamp to human readable
   */
  private formatTimestamp(usec: string): string {
    try {
      const timestamp = parseInt(usec, 10) / 1000;
      const date = new Date(timestamp);
      return date.toISOString().slice(11, 19); // HH:MM:SS
    } catch {
      return "0:00:00";
    }
  }

  /**
   * Extract currency from amount string (e.g., "$5.00" -> "USD")
   */
  private extractCurrency(amount: string): string {
    if (amount.startsWith("$")) return "USD";
    if (amount.startsWith("€")) return "EUR";
    if (amount.startsWith("£")) return "GBP";
    if (amount.startsWith("¥")) return "JPY";
    if (amount.includes("CA$")) return "CAD";
    if (amount.includes("A$")) return "AUD";
    return "UNKNOWN";
  }

  /**
   * Get superchat color from background color value
   */
  private getSuperchatColor(colorValue: number): string {
    // YouTube uses specific colors for different tiers
    const colors: Record<number, string> = {
      4280191205: "blue", // $1-$1.99
      4278248959: "cyan", // $2-$4.99
      4280150454: "green", // $5-$9.99
      4294953512: "yellow", // $10-$19.99
      4294278144: "orange", // $20-$49.99
      4293467747: "magenta", // $50-$99.99
      4293271831: "red", // $100-$199.99
    };
    return colors[colorValue] || "blue";
  }

  /**
   * Get superchat tier from background color value
   */
  private getSuperchatTier(colorValue: number): number {
    const tiers: Record<number, number> = {
      4280191205: 1,
      4278248959: 2,
      4280150454: 3,
      4294953512: 4,
      4294278144: 5,
      4293467747: 6,
      4293271831: 7,
    };
    return tiers[colorValue] || 1;
  }
}
