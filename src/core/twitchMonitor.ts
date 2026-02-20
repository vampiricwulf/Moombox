/**
 * Dedicated Twitch monitoring cycle.
 *
 * Polls all configured Twitch channels for live streams via GQL at a fast
 * interval (default 15s with ±5s jitter). Replaces the previous split approach
 * where Twitch was checked by both FeedMonitor and DecapiMonitor.
 */

import { ConfigManager } from "./config.js";
import { Database } from "./database.js";
import { Logger } from "./logger.js";
import { getErrorMessage } from "../types/errors.js";
import { processTwitchStream } from "./monitorUtils.js";
import { cancellableSleep } from "../utils/async.js";
import {
  TWITCH_CHECK_INTERVAL_DEFAULT_MS,
  TWITCH_CHECK_JITTER_MS,
  TWITCH_REQUEST_STAGGER_MS,
  TWITCH_CHECK_MIN_INTERVAL_MS,
} from "../constants.js";

export class TwitchMonitor {
  private static instance: TwitchMonitor | null = null;

  private timer: NodeJS.Timeout | null = null;
  private running = false;
  private checking = false;
  private abortController: AbortController | null = null;
  private logger: Logger;

  /** Epoch ms of the next scheduled check. 0 = not scheduled. */
  public nextCheckAt = 0;

  /** Optional callback invoked when the next check is scheduled. */
  public onSchedule: (() => void) | null = null;

  private constructor() {
    this.logger = Logger.getInstance();
  }

  static getInstance(): TwitchMonitor {
    if (!TwitchMonitor.instance) {
      TwitchMonitor.instance = new TwitchMonitor();
    }
    return TwitchMonitor.instance;
  }

  /** For testing. */
  static clearInstance(): void {
    TwitchMonitor.instance = null;
  }

  start(): void {
    if (this.running) return;
    this.running = true;
    this.logger.info("[TwitchMonitor] Monitor started.");
    this.runCycle();
  }

  stop(): void {
    if (this.timer) clearTimeout(this.timer);
    this.timer = null;
    this.running = false;
    this.nextCheckAt = 0;
    this.abortController?.abort();
    this.abortController = null;
    this.logger.info("[TwitchMonitor] Monitor stopped.");
  }

  private async runCycle(): Promise<void> {
    if (!this.running || this.checking) return;
    this.checking = true;
    this.abortController = new AbortController();

    try {
      await this.doCheckAll();
    } catch (e: unknown) {
      this.logger.error(`[TwitchMonitor] Cycle error: ${getErrorMessage(e)}`);
    } finally {
      this.checking = false;
      this.abortController = null;
      this.scheduleNext();
    }
  }

  private async doCheckAll(): Promise<void> {
    const config = ConfigManager.getInstance().get();
    const db = await Database.getInstance();
    const channels = (config.channels || []).filter(
      (c) => c.enabled !== false && c.platform === "twitch",
    );

    if (channels.length === 0) {
      this.logger.debug("[TwitchMonitor] No enabled Twitch channels, skipping cycle.");
      return;
    }

    this.logger.debug(`[TwitchMonitor] Starting cycle for ${channels.length} Twitch channels...`);

    for (let i = 0; i < channels.length; i++) {
      if (!this.running) break;

      try {
        await processTwitchStream(channels[i], config, db);
      } catch (e: unknown) {
        this.logger.debug(
          `[TwitchMonitor] Error checking ${channels[i].name || channels[i].id}: ${getErrorMessage(e)}`,
        );
      }

      // Stagger requests between channels
      if (this.running && i < channels.length - 1) {
        await cancellableSleep(TWITCH_REQUEST_STAGGER_MS, this.abortController?.signal);
      }
    }
  }

  private scheduleNext(): void {
    if (!this.running) return;

    const config = ConfigManager.getInstance().get();
    const channels = (config.channels || []).filter(
      (c) => c.enabled !== false && c.platform === "twitch",
    );

    // No Twitch channels — clear timer, broadcast idle state
    if (channels.length === 0) {
      this.nextCheckAt = 0;
      this.onSchedule?.();
      return;
    }

    // Config value is in seconds; default to 15s
    const baseMs = config.twitch_check_interval
      ? config.twitch_check_interval * 1000
      : TWITCH_CHECK_INTERVAL_DEFAULT_MS;

    // Apply ±jitter
    const jitter = Math.floor(Math.random() * TWITCH_CHECK_JITTER_MS * 2) - TWITCH_CHECK_JITTER_MS;
    const intervalMs = Math.max(TWITCH_CHECK_MIN_INTERVAL_MS, baseMs + jitter);

    this.nextCheckAt = Date.now() + intervalMs;
    this.timer = setTimeout(() => this.runCycle(), intervalMs);

    this.logger.debug(
      `[TwitchMonitor] Next cycle in ${Math.round(intervalMs / 1000)}s (${channels.length} Twitch channels)`,
    );

    this.onSchedule?.();
  }
}
