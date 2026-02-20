/**
 * Feed Monitor — RSS-based channel monitoring.
 *
 * Polls YouTube RSS feeds for live streams.
 * DECAPI checks are handled independently by DecapiMonitor.
 * Twitch channels are handled by TwitchMonitor.
 */

import { XMLParser } from "fast-xml-parser";
import { ConfigManager, ChannelConfig } from "./config.js";
import { Database } from "./database.js";
import { Logger } from "./logger.js";
import { MoomboxConfig } from "../types/config.js";
import { fetchWithTimeout } from "./http.js";
import { getErrorMessage } from "../types/errors.js";
import { matchesTerms, processYouTubeVideo } from "./monitorUtils.js";

/** Parsed YouTube Atom feed entry */
interface FeedEntry {
  id: string;
  title: string;
  link: string;
  description: string;
}

export class FeedMonitor {
  private static instance: FeedMonitor | null = null;

  private xmlParser: XMLParser;
  private interval: NodeJS.Timeout | null = null;
  private running = false;
  private checking = false;
  private logger: Logger;
  private metadataFailures: Map<string, number> = new Map();

  /** Epoch ms of the next scheduled check. 0 = not scheduled. */
  public nextCheckAt = 0;

  /** Optional callback invoked when the next check is scheduled. */
  public onSchedule: (() => void) | null = null;

  private constructor() {
    this.xmlParser = new XMLParser({
      ignoreAttributes: false,
      attributeNamePrefix: "@_",
    });
    this.logger = Logger.getInstance();
  }

  static getInstance(): FeedMonitor {
    if (!FeedMonitor.instance) {
      FeedMonitor.instance = new FeedMonitor();
    }
    return FeedMonitor.instance;
  }

  /** For testing. */
  static clearInstance(): void {
    FeedMonitor.instance = null;
  }

  start(): void {
    if (this.running) return;
    this.running = true;
    this.logger.info("Feed Monitor started.");
    this.checkFeeds();
    this.scheduleNext();
  }

  stop(): void {
    if (this.interval) clearTimeout(this.interval);
    this.interval = null;
    this.running = false;
    this.nextCheckAt = 0;
    this.logger.info("Feed Monitor stopped.");
  }

  private scheduleNext(): void {
    if (!this.running) return;
    const config = ConfigManager.getInstance().get();
    const intervalMs = (config.feed_check_interval ?? 10) * 60_000;
    this.nextCheckAt = Date.now() + intervalMs;
    this.interval = setTimeout(() => {
      this.checkFeeds();
      this.scheduleNext();
    }, intervalMs);
    this.onSchedule?.();
  }

  private async checkFeeds(): Promise<void> {
    if (this.checking) return;
    this.checking = true;
    try {
      await this.doCheckFeeds();
    } finally {
      this.checking = false;
    }
  }

  private async doCheckFeeds(): Promise<void> {
    const config = ConfigManager.getInstance().get();
    const db = await Database.getInstance();

    if (!config.channels) return;

    this.logger.debug(`Checking ${config.channels.length} channels...`);

    for (const channel of config.channels) {
      if (channel.enabled === false) {
        this.logger.debug(`[Monitor] Skipping disabled channel: ${channel.name || channel.id}`);
        continue;
      }
      // Twitch channels are handled by TwitchMonitor
      if (channel.platform === "twitch") continue;

      try {
        await this.checkChannelFeed(channel, config, db);
      } catch (e: unknown) {
        this.logger.error(
          `[Monitor] Error checking ${channel.name || channel.id}: ${getErrorMessage(e)}`,
        );
      }
    }
  }

  /**
   * Parse YouTube Atom feed XML into a flat array of entries.
   * Handles both single-entry and multi-entry feeds (fast-xml-parser
   * returns an object instead of an array when there's only one entry).
   */
  private parseFeedXml(xml: string): FeedEntry[] {
    const parsed = this.xmlParser.parse(xml);
    const feed = parsed.feed;
    if (!feed || !feed.entry) return [];

    const rawEntries = Array.isArray(feed.entry) ? feed.entry : [feed.entry];
    // eslint-disable-next-line @typescript-eslint/no-explicit-any -- XML parser output is untyped
    return rawEntries.map((entry: Record<string, any>) => {
      // Link can be a single object or array (rel="self" + rel="alternate")
      const links = Array.isArray(entry.link) ? entry.link : [entry.link];
      const altLink = links.find((l: Record<string, unknown>) => l?.["@_rel"] === "alternate");
      const href = altLink?.["@_href"] || "";

      return {
        id: String(entry["yt:videoId"] || entry.id || ""),
        title: String(entry.title || ""),
        link: href,
        description: String(entry["media:group"]?.["media:description"] || ""),
      };
    });
  }

  private async checkChannelFeed(
    channel: ChannelConfig,
    config: MoomboxConfig,
    db: Database,
  ): Promise<void> {
    const feedUrl = `https://www.youtube.com/feeds/videos.xml?channel_id=${channel.id}`;
    this.logger.debug(`Fetching feed: ${feedUrl}`);

    const response = await fetchWithTimeout(feedUrl, {}, 15_000);
    if (!response.ok) {
      throw new Error(`Status code ${response.status}`);
    }
    const xml = await response.text();
    const allEntries = this.parseFeedXml(xml);

    // Sliding window / Lookbehind logic
    const lookbehind = channel.num_desc_lookbehind || 0;

    // Max feed items limit
    const maxItems = channel.max_feed_items || config.max_feed_items || 15;
    const itemsToProcess = allEntries.slice(0, maxItems);

    // Track the most recent video for DECAPI baseline
    if (itemsToProcess.length > 0) {
      const newestVideoId = itemsToProcess[0].id.replace("yt:video:", "");
      if (newestVideoId) {
        await db.setLastVideo(channel.id, newestVideoId);
      }
    }

    // Entries are usually sorted newest first.
    for (let i = 0; i < itemsToProcess.length; i++) {
      const item = itemsToProcess[i];
      const videoId = item.id.replace("yt:video:", "");
      if (!videoId) continue;

      if (await db.hasProcessed(videoId)) continue;

      // Collect older items for description comparison
      const olderItems = allEntries.slice(i + 1, i + 1 + lookbehind);

      // Filter lines present in older items (template boilerplate)
      const olderLines = new Set<string>();
      for (const older of olderItems) {
        older.description
          .split("\n")
          .forEach((line) => olderLines.add(line.trim()));
      }

      const currentLines = item.description.split("\n");
      const uniqueDescription = currentLines
        .filter((line) => !olderLines.has(line.trim()))
        .join("\n");

      // Check matches in Title AND Unique Description
      const titleMatch = matchesTerms(item.title || "", channel);
      const descMatch = matchesTerms(uniqueDescription, channel);

      if (titleMatch || descMatch) {
        await processYouTubeVideo(
          videoId,
          item.title || "Unknown",
          item.link || `https://www.youtube.com/watch?v=${videoId}`,
          channel,
          config,
          db,
          this.metadataFailures,
        );
      }
    }
  }

}
