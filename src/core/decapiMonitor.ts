/**
 * Independent DECAPI monitoring cycle.
 *
 * Proactively checks all configured YouTube channels for live streams using DECAPI,
 * with dynamic rate-limited polling. Runs independently of the RSS-based FeedMonitor.
 *
 * Twitch channels are handled by the dedicated TwitchMonitor.
 */

import { ConfigManager, ChannelConfig } from "./config.js";
import { Database } from "./database.js";
import { Logger } from "./logger.js";
import { getErrorMessage } from "../types/errors.js";
import { matchesTerms, processYouTubeVideo } from "./monitorUtils.js";
import { cancellableSleep } from "../utils/async.js";
import {
  DECAPI_URLS,
  DECAPI_MIN_INTERVAL_MS,
  DECAPI_REQUEST_STAGGER_MS,
  DECAPI_DEFAULT_RATE_LIMIT,
  DECAPI_REQUEST_TIMEOUT_MS,
} from "../constants.js";

interface RateLimitState {
  /** Requests per minute allowed */
  limit: number;
  /** Remaining requests in current window */
  remaining: number;
  /** Epoch ms when the rate limit window resets */
  resetAt: number;
}

export class DecapiMonitor {
  private static instance: DecapiMonitor | null = null;

  private timer: NodeJS.Timeout | null = null;
  private running = false;
  private checking = false;
  private abortController: AbortController | null = null;
  private logger: Logger;
  private metadataFailures: Map<string, number> = new Map();
  /** Rate limit state for DECAPI (YouTube only). */
  private rateLimit: RateLimitState | null = null;

  /** Epoch ms of the next scheduled check. 0 = not scheduled. */
  public nextCheckAt = 0;

  /** Optional callback invoked when the next check is scheduled. */
  public onSchedule: (() => void) | null = null;

  private constructor() {
    this.logger = Logger.getInstance();
  }

  /** Get or create rate limit state. */
  private getRateLimit(): RateLimitState {
    if (!this.rateLimit) {
      this.rateLimit = {
        limit: DECAPI_DEFAULT_RATE_LIMIT,
        remaining: DECAPI_DEFAULT_RATE_LIMIT,
        resetAt: 0,
      };
    }
    return this.rateLimit;
  }

  static getInstance(): DecapiMonitor {
    if (!DecapiMonitor.instance) {
      DecapiMonitor.instance = new DecapiMonitor();
    }
    return DecapiMonitor.instance;
  }

  /** For testing. */
  static clearInstance(): void {
    DecapiMonitor.instance = null;
  }

  start(): void {
    if (this.running) return;
    this.running = true;
    this.logger.info("[DECAPI] Monitor started.");
    this.runCycle();
  }

  stop(): void {
    if (this.timer) clearTimeout(this.timer);
    this.timer = null;
    this.running = false;
    this.nextCheckAt = 0;
    this.abortController?.abort();
    this.abortController = null;
    this.metadataFailures.clear();
    this.logger.info("[DECAPI] Monitor stopped.");
  }

  private async runCycle(): Promise<void> {
    if (!this.running || this.checking) return;
    this.checking = true;
    this.abortController = new AbortController();

    try {
      await this.doCheckAll();
    } catch (e: unknown) {
      this.logger.error(`[DECAPI] Cycle error: ${getErrorMessage(e)}`);
    } finally {
      this.checking = false;
      this.abortController = null;
      this.scheduleNext();
    }
  }

  private async doCheckAll(): Promise<void> {
    const config = ConfigManager.getInstance().get();
    const db = await Database.getInstance();
    // Only YouTube channels — Twitch is handled by TwitchMonitor
    const channels = (config.channels || []).filter(
      (c) => c.enabled !== false && c.platform !== "twitch",
    );

    if (channels.length === 0) {
      this.logger.debug("[DECAPI] No enabled YouTube channels, skipping cycle.");
      return;
    }

    this.logger.debug(`[DECAPI] Starting cycle for ${channels.length} YouTube channels...`);

    for (let i = 0; i < channels.length; i++) {
      if (!this.running) break;

      // Check rate limit — pause if exhausted
      await this.waitForRateLimit();
      if (!this.running) break;

      try {
        await this.checkYouTubeDecapi(channels[i], config, db);
      } catch (e: unknown) {
        this.logger.debug(
          `[DECAPI] Error checking ${channels[i].name || channels[i].id}: ${getErrorMessage(e)}`,
        );
      }

      // Stagger requests between channels
      if (this.running && i < channels.length - 1) {
        await cancellableSleep(DECAPI_REQUEST_STAGGER_MS, this.abortController?.signal);
      }
    }
  }

  /**
   * Check a YouTube channel via DECAPI latest_video endpoint.
   * Response format: "TITLE - https://youtu.be/VIDEO_ID"
   */
  private async checkYouTubeDecapi(
    channel: ChannelConfig,
    config: import("../types/config.js").MoomboxConfig,
    db: Database,
  ): Promise<void> {
    const url = `${DECAPI_URLS.YOUTUBE_LATEST}?id=${channel.id}`;
    const response = await this.fetchDecapi(url);
    if (!response) return;

    const text = (await response.text()).trim();
    if (!text) {
      this.logger.debug(`[DECAPI] Empty response for ${channel.name || channel.id}`);
      return;
    }

    const videoId = this.parseDecapiVideoId(text);
    if (!videoId) {
      this.logger.debug(`[DECAPI] Could not parse video ID: ${text.substring(0, 100)}`);
      return;
    }

    const title = this.parseDecapiTitle(text);

    // Compare with last known video
    const lastVideo = await db.getLastVideo(channel.id);
    if (lastVideo === videoId) {
      return; // No new video
    }

    this.logger.info(
      `[DECAPI] New video for ${channel.name || channel.id}: ${title} (${videoId})`,
    );
    await db.setLastVideo(channel.id, videoId);

    if (await db.hasProcessed(videoId)) {
      this.logger.debug(`[DECAPI] ${videoId} already processed, skipping`);
      return;
    }

    // Term matching on title (no description available from DECAPI)
    if (!matchesTerms(title, channel)) {
      this.logger.debug(`[DECAPI] ${title} (${videoId}) did not match terms`);
      return;
    }

    await processYouTubeVideo(
      videoId,
      title,
      `https://www.youtube.com/watch?v=${videoId}`,
      channel,
      config,
      db,
      this.metadataFailures,
    );
  }

  /**
   * Fetch a DECAPI URL with timeout and rate limit header tracking.
   * Uses raw fetch() (not fetchWithTimeout) so we can read headers on non-OK responses
   * and avoid auto-retrying 429s.
   */
  private async fetchDecapi(url: string): Promise<Response | null> {
    const rl = this.getRateLimit();

    try {
      const response = await fetch(url, {
        headers: { "User-Agent": "Moombox/1.0" },
        signal: AbortSignal.timeout(DECAPI_REQUEST_TIMEOUT_MS),
      });

      // Start a 1-minute rate limit window if none is active
      if (rl.resetAt === 0) {
        rl.resetAt = Date.now() + 60_000;
      }

      // Decrement locally first (covers the case where server omits headers)
      rl.remaining = Math.max(0, rl.remaining - 1);

      // Server headers override with authoritative count
      this.updateRateLimit(response.headers);

      if (response.status === 429) {
        const retryAfter = parseInt(response.headers.get("Retry-After") || "60", 10);
        this.logger.warn(`[DECAPI] Rate limited (429). Retry after ${retryAfter}s`);
        rl.remaining = 0;
        rl.resetAt = Date.now() + retryAfter * 1000;
        return null;
      }

      if (!response.ok) {
        this.logger.debug(`[DECAPI] HTTP ${response.status} for ${url}`);
        return null;
      }

      return response;
    } catch (e: unknown) {
      if (this.running) {
        this.logger.debug(`[DECAPI] Fetch failed: ${getErrorMessage(e)}`);
      }
      return null;
    }
  }

  /** Update rate limit state from response headers (authoritative, overrides local tracking). */
  private updateRateLimit(headers: Headers): void {
    const rl = this.getRateLimit();
    const limit = headers.get("X-RateLimit-Limit");
    const remaining = headers.get("X-RateLimit-Remaining");
    const reset = headers.get("X-RateLimit-Reset");

    if (limit) {
      const parsed = parseInt(limit, 10);
      if (!isNaN(parsed) && parsed > 0) rl.limit = parsed;
    }
    if (remaining) {
      const parsed = parseInt(remaining, 10);
      if (!isNaN(parsed)) rl.remaining = parsed;
    }
    if (reset) {
      const parsed = parseInt(reset, 10);
      if (!isNaN(parsed)) {
        // Reset header can be epoch seconds or relative seconds
        rl.resetAt = parsed > 1e9 ? parsed * 1000 : Date.now() + parsed * 1000;
      }
    }
  }

  /** Wait if rate limit is exhausted. */
  private async waitForRateLimit(): Promise<void> {
    const rl = this.getRateLimit();

    // Proactively reset if the rate limit window has expired
    if (rl.resetAt > 0 && Date.now() >= rl.resetAt) {
      rl.remaining = rl.limit;
      rl.resetAt = 0;
    }

    if (rl.remaining > 0) return;

    const waitMs = Math.max(0, rl.resetAt - Date.now());
    if (waitMs <= 0) {
      // Reset window has passed (or was never set), refresh
      rl.remaining = rl.limit;
      rl.resetAt = 0;
      return;
    }

    this.logger.debug(`[DECAPI] Rate limit exhausted, waiting ${Math.ceil(waitMs / 1000)}s...`);
    await cancellableSleep(waitMs, this.abortController?.signal);
    rl.remaining = rl.limit;
    rl.resetAt = 0;
  }

  /** Calculate interval for next cycle based on rate limits and channel count. */
  private calculateInterval(channelCount: number): number {
    const config = ConfigManager.getInstance().get();

    // Manual override from config (in seconds)
    if (config.decapi_check_interval && config.decapi_check_interval >= 15) {
      return config.decapi_check_interval * 1000;
    }

    // Dynamic: ceil(channels / ratePerMinute * 60) seconds
    const rl = this.getRateLimit();
    const dynamicSeconds = channelCount > 0 ? Math.ceil((channelCount / rl.limit) * 60) : 0;
    return Math.max(DECAPI_MIN_INTERVAL_MS, dynamicSeconds * 1000);
  }

  private scheduleNext(): void {
    if (!this.running) return;

    const config = ConfigManager.getInstance().get();
    const channels = (config.channels || []).filter(
      (c) => c.enabled !== false && c.platform !== "twitch",
    );

    // No YouTube channels — clear timer, broadcast idle state
    if (channels.length === 0) {
      this.nextCheckAt = 0;
      this.onSchedule?.();
      return;
    }

    const intervalMs = this.calculateInterval(channels.length);

    this.nextCheckAt = Date.now() + intervalMs;
    this.timer = setTimeout(() => this.runCycle(), intervalMs);

    const rl = this.getRateLimit();
    this.logger.debug(
      `[DECAPI] Next cycle in ${Math.round(intervalMs / 1000)}s (${channels.length} YT channels, ` +
      `${rl.remaining}/${rl.limit})`,
    );

    this.onSchedule?.();
  }

  // ===== Helpers =====

  private parseDecapiVideoId(text: string): string | null {
    const match = text.match(
      /(?:youtu\.be\/|youtube\.com\/watch\?v=)([a-zA-Z0-9_-]{11})/,
    );
    return match ? match[1] : null;
  }

  private parseDecapiTitle(text: string): string {
    const urlIndex = text.lastIndexOf(" - https://youtu.be/");
    if (urlIndex === -1) {
      const urlIndex2 = text.lastIndexOf(" - https://www.youtube.com/");
      return urlIndex2 !== -1 ? text.substring(0, urlIndex2) : text;
    }
    return text.substring(0, urlIndex);
  }
}
