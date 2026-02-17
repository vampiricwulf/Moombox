import { XMLParser } from "fast-xml-parser";
import ms from "ms";
import { ConfigManager, ChannelConfig } from "./config.js";
import { Database } from "./database.js";
import { YouTubeService } from "../engine/youtube/index.js";
import { TwitchService } from "../engine/twitch/index.js";
import { Logger } from "./logger.js";
import { NotificationManager, NotificationType } from "./notifications.js";
import { MoomboxConfig } from "../types/config.js";
import { fetchWithTimeout } from "./http.js";
import { fuzzyMatch } from "../utils/textNormalization.js";
import { getErrorMessage } from "../types/errors.js";
import { TWITCH_URLS } from "../constants.js";

/** Parsed YouTube Atom feed entry */
interface FeedEntry {
  id: string;
  title: string;
  link: string;
  description: string;
}

export class FeedMonitor {
  private xmlParser: XMLParser;
  private interval: NodeJS.Timeout | null = null;
  private running: boolean = false;
  private checking: boolean = false;
  private logger: Logger;
  private metadataFailures: Map<string, number> = new Map();
  private static readonly MAX_METADATA_FAILURES = 3;

  constructor() {
    this.xmlParser = new XMLParser({
      ignoreAttributes: false,
      attributeNamePrefix: "@_",
    });
    this.logger = Logger.getInstance();
  }

  start() {
    if (this.running) return;
    this.running = true;
    this.logger.info("Feed Monitor started.");
    this.checkFeeds();
    this.scheduleNext();
  }

  stop() {
    if (this.interval) clearTimeout(this.interval);
    this.running = false;
    this.logger.info("Feed Monitor stopped.");
  }

  private scheduleNext() {
    if (!this.running) return;
    const config = ConfigManager.getInstance().get();
    const intervalMs = (config.feed_check_interval ?? 10) * ms("1m");
    this.interval = setTimeout(() => {
      this.checkFeeds();
      this.scheduleNext();
    }, intervalMs);
  }

  private async checkFeeds() {
    if (this.checking) return;
    this.checking = true;
    try {
      await this.doCheckFeeds();
    } finally {
      this.checking = false;
    }
  }

  private async doCheckFeeds() {
    const config = ConfigManager.getInstance().get();
    const db = await Database.getInstance();

    if (!config.channels) return;

    this.logger.debug(`Checking ${config.channels.length} channels...`);

    for (const channel of config.channels) {
      if (channel.enabled === false) {
        this.logger.debug(`[Monitor] Skipping disabled channel: ${channel.name || channel.id}`);
        continue;
      }
      try {
        if (channel.platform === "twitch") {
          await this.checkTwitchChannel(channel, config, db);
        } else {
          await this.checkChannelFeed(channel, config, db);
        }
      } catch (e: any) {
        const errorMsg = e.message || String(e);
        const isHttpError = errorMsg.includes("Status code");

        if (isHttpError && channel.platform !== "twitch") {
          this.logger.warn(
            `[Monitor] RSS feed blocked for ${channel.name || channel.id}: ${errorMsg}`,
          );
          // Fall back to DECAPI
          try {
            await this.checkChannelDecapi(channel, config, db);
          } catch (fallbackErr: any) {
            this.logger.error(
              `[Monitor] DECAPI fallback also failed for ${channel.name || channel.id}: ${fallbackErr.message}`,
            );
          }
        } else {
          this.logger.error(
            `[Monitor] Error checking ${channel.name || channel.id}: ${errorMsg}`,
          );
        }
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
    return rawEntries.map((entry: any) => {
      // Link can be a single object or array (rel="self" + rel="alternate")
      const links = Array.isArray(entry.link) ? entry.link : [entry.link];
      const altLink = links.find((l: any) => l?.["@_rel"] === "alternate");
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
  ) {
    const feedUrl = `https://www.youtube.com/feeds/videos.xml?channel_id=${channel.id}`;
    this.logger.debug(`Fetching feed: ${feedUrl}`);

    const response = await fetchWithTimeout(feedUrl, {}, ms("15s"));
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

    // Track the most recent video for DECAPI fallback baseline
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
      const titleMatch = this.matches(item.title || "", channel);
      const descMatch = this.matches(uniqueDescription, channel);

      if (titleMatch || descMatch) {
        await this.processVideo(
          videoId,
          item.title || "Unknown",
          item.link || `https://www.youtube.com/watch?v=${videoId}`,
          channel,
          config,
          db,
        );
      }
    }
  }

  /**
   * DECAPI fallback: fetch the latest video for a channel when RSS is blocked.
   * Response format: "TITLE - https://youtu.be/VIDEO_ID"
   */
  private async checkChannelDecapi(
    channel: ChannelConfig,
    config: MoomboxConfig,
    db: Database,
  ) {
    const url = `https://decapi.me/youtube/latest_video?id=${channel.id}`;
    this.logger.debug(`[Monitor] DECAPI fallback for ${channel.name || channel.id}: ${url}`);

    const response = await fetchWithTimeout(url, { headers: { "User-Agent": "Moombox/1.0" } }, ms("15s"));
    if (!response.ok) {
      throw new Error(`DECAPI returned HTTP ${response.status}`);
    }

    const text = (await response.text()).trim();
    if (!text) {
      this.logger.warn(`[Monitor] DECAPI returned empty response for ${channel.name || channel.id}`);
      return;
    }

    // Parse response: "TITLE - https://youtu.be/VIDEO_ID"
    const videoId = this.parseDecapiVideoId(text);
    if (!videoId) {
      this.logger.warn(`[Monitor] Could not parse video ID from DECAPI response: ${text}`);
      return;
    }

    // Parse title (everything before the last " - https://youtu.be/")
    const title = this.parseDecapiTitle(text);

    // Compare with last known video
    const lastVideo = await db.getLastVideo(channel.id);
    if (lastVideo === videoId) {
      this.logger.debug(`[Monitor] DECAPI: No new video for ${channel.name || channel.id} (still ${videoId})`);
      return;
    }

    this.logger.info(
      `[Monitor] DECAPI: New video detected for ${channel.name || channel.id}: ${title} (${videoId})`,
    );

    // Update stored last video
    await db.setLastVideo(channel.id, videoId);

    // Skip if already processed
    if (await db.hasProcessed(videoId)) {
      this.logger.debug(`[Monitor] DECAPI: ${videoId} already processed, skipping`);
      return;
    }

    // Apply term matching on the title (no description available from DECAPI)
    if (!this.matches(title, channel)) {
      this.logger.debug(`[Monitor] DECAPI: ${title} (${videoId}) did not match channel terms`);
      return;
    }

    await this.processVideo(
      videoId,
      title,
      `https://www.youtube.com/watch?v=${videoId}`,
      channel,
      config,
      db,
    );
  }

  private parseDecapiVideoId(text: string): string | null {
    // Match youtu.be/VIDEO_ID or youtube.com/watch?v=VIDEO_ID
    const match = text.match(
      /(?:youtu\.be\/|youtube\.com\/watch\?v=)([a-zA-Z0-9_-]{11})/,
    );
    return match ? match[1] : null;
  }

  private parseDecapiTitle(text: string): string {
    // DECAPI format: "TITLE - https://youtu.be/VIDEO_ID"
    const urlIndex = text.lastIndexOf(" - https://youtu.be/");
    if (urlIndex === -1) {
      const urlIndex2 = text.lastIndexOf(" - https://www.youtube.com/");
      return urlIndex2 !== -1 ? text.substring(0, urlIndex2) : text;
    }
    return text.substring(0, urlIndex);
  }

  /**
   * Shared video processing logic used by both RSS and DECAPI paths.
   */
  private async processVideo(
    videoId: string,
    title: string,
    videoUrl: string,
    channel: ChannelConfig,
    config: MoomboxConfig,
    db: Database,
  ) {
    const includeNonLive = channel.include_non_live_content ?? false;

    // Always check if this is actually a livestream
    try {
      const yt = YouTubeService.getInstance();
      const meta = await yt.getMetadata(videoId);

      // Handle different stream statuses
      if (meta.streamStatus === "not_a_stream") {
        if (!includeNonLive) {
          this.logger.info(
            `[Monitor] Skipping non-stream content: ${title} (${videoId})`,
          );
          await db.addToHistory(videoId);
          return;
        }
        this.logger.info(
          `[Monitor] Including non-stream content (include_non_live_content=true): ${title} (${videoId})`,
        );
      } else if (meta.streamStatus === "upcoming") {
        this.logger.info(
          `[Monitor] Found upcoming stream/premiere: ${title} (${videoId}) - will monitor`,
        );
      } else if (meta.streamStatus === "live") {
        this.logger.info(
          `[Monitor] Found LIVE stream/premiere: ${title} (${videoId}) - queuing for download`,
        );
      } else if (
        meta.streamStatus === "post_live" ||
        meta.streamStatus === "vod"
      ) {
        if (!includeNonLive) {
          this.logger.info(
            `[Monitor] Skipping ended stream (${meta.streamStatus}): ${title} (${videoId})`,
          );
          await db.addToHistory(videoId);
          return;
        }
        this.logger.info(
          `[Monitor] Including ended stream (include_non_live_content=true): ${title} (${videoId})`,
        );
      }

      this.logger.debug(
        `[Monitor] Video ${videoId} classified as: ${meta.streamStatus}`,
      );
    } catch (err: any) {
      const failures = (this.metadataFailures.get(videoId) || 0) + 1;
      this.metadataFailures.set(videoId, failures);

      if (failures >= FeedMonitor.MAX_METADATA_FAILURES) {
        this.logger.warn(
          `[Monitor] Failed to check metadata for ${videoId} ${failures} times, adding to history to stop retrying: ${err.message}`,
        );
        this.metadataFailures.delete(videoId);
        await db.addToHistory(videoId);
      } else {
        this.logger.warn(
          `[Monitor] Failed to check metadata for ${videoId} (attempt ${failures}/${FeedMonitor.MAX_METADATA_FAILURES}): ${err.message}`,
        );
      }
      return;
    }

    this.logger.info(
      `[Monitor] Queuing job: ${title} (${videoId})`,
    );

    // Queue Job
    const newJob = await db.addJob({
      videoId: videoId,
      url: videoUrl,
      title: title,
      channelName: channel.name || "Unknown",
      thumbnailUrl: `https://i.ytimg.com/vi/${videoId}/maxresdefault.jpg`,
      outputDirectory:
        channel.output_directory || config.downloader.output_directory,
      allowNonStream: includeNonLive,
    });

    if (!newJob) {
      this.logger.debug(
        `[Monitor] Job already exists for ${videoId}, skipping`,
      );
      return;
    }

    await db.addToHistory(videoId);

    await NotificationManager.getInstance().send(
      "Stream Found",
      `Found matching stream: ${title}`,
      NotificationType.INFO,
      [
        {
          name: "Channel",
          value: channel.name || "Unknown",
          inline: true,
        },
        { name: "Video ID", value: videoId, inline: true },
      ],
      { url: videoUrl, thumbnail: `https://i.ytimg.com/vi/${videoId}/maxresdefault.jpg`, event: "found" },
    );
  }

  /**
   * Check a Twitch channel for live status.
   * Uses DECAPI uptime check first (lightweight), then GQL for full metadata.
   */
  private async checkTwitchChannel(
    channel: ChannelConfig,
    config: MoomboxConfig,
    db: Database,
  ) {
    const login = channel.id.toLowerCase();
    const displayName = channel.name || login;

    // Quick uptime check via DECAPI
    const isLive = await this.checkTwitchUptime(login);
    if (!isLive) {
      this.logger.debug(`[Monitor] Twitch channel ${displayName} is offline`);
      return;
    }

    // Channel is live — fetch full metadata via GQL
    const twitch = TwitchService.getInstance();
    await twitch.init();

    const streamInfo = await twitch.getStreamInfo(login);
    if (!streamInfo || !streamInfo.isLive) {
      this.logger.debug(`[Monitor] Twitch GQL says ${displayName} is not live (stale DECAPI?)`);
      return;
    }

    // Check deduplication — stream ID is unique per broadcast session
    const jobId = `tw_${streamInfo.streamId}`;
    if (await db.hasProcessed(jobId)) {
      this.logger.debug(`[Monitor] Twitch stream ${jobId} already processed`);
      return;
    }

    // Apply term matching on stream title and game category
    const titleMatch = this.matches(streamInfo.title || "", channel);
    const categoryMatch = streamInfo.gameCategory
      ? this.matches(streamInfo.gameCategory, channel)
      : false;

    if (!titleMatch && !categoryMatch) {
      this.logger.debug(
        `[Monitor] Twitch stream "${streamInfo.title}" (${streamInfo.gameCategory || "no category"}) ` +
        `did not match terms for ${displayName}`,
      );
      return;
    }

    this.logger.info(
      `[Monitor] Twitch LIVE: ${displayName} — ${streamInfo.title} (${jobId})`,
    );

    // Create job
    const title = `${streamInfo.channelDisplayName} — ${streamInfo.title || new Date().toISOString()}`;
    const newJob = await db.addJob({
      videoId: jobId,
      url: `${TWITCH_URLS.BASE}/${login}`,
      title,
      channelName: streamInfo.channelDisplayName,
      platform: "twitch",
      thumbnailUrl: streamInfo.thumbnailUrl,
      channelAvatarUrl: streamInfo.profileImageUrl,
      outputDirectory: channel.output_directory || config.downloader?.output_directory,
      streamStartTime: streamInfo.startedAt,
      twitchCategory: streamInfo.gameCategory,
      twitchQuality: channel.qualityPreference,
    });

    if (!newJob) {
      this.logger.debug(`[Monitor] Twitch job already exists for ${jobId}`);
      return;
    }

    // Twitch monitor already confirmed stream is live — set status immediately
    // so the job doesn't sit as "Upcoming" while waiting for a queue slot
    await db.updateJob(jobId, { status: "Live" });

    await db.addToHistory(jobId);

    await NotificationManager.getInstance().send(
      "Twitch Stream Found",
      `Live: ${title}`,
      NotificationType.INFO,
      [
        { name: "Channel", value: displayName, inline: true },
        { name: "Stream ID", value: streamInfo.streamId, inline: true },
        ...(streamInfo.gameCategory ? [{ name: "Category", value: streamInfo.gameCategory, inline: true }] : []),
      ],
      {
        url: `${TWITCH_URLS.BASE}/${login}`,
        thumbnail: streamInfo.thumbnailUrl,
        event: "found",
      },
    );
  }

  /**
   * Lightweight Twitch uptime check via DECAPI.
   * Returns true if the channel is live.
   */
  private async checkTwitchUptime(login: string): Promise<boolean> {
    try {
      const response = await fetchWithTimeout(
        `${TWITCH_URLS.DECAPI_UPTIME}/${login}`,
        { headers: { "User-Agent": "Moombox/1.0" } },
        ms("15s"),
      );
      if (!response.ok) return false;
      const text = await response.text();
      const lower = text.toLowerCase().trim();
      // DECAPI returns "X is offline" when not live, or error strings
      if (lower.includes("is offline")) return false;
      if (lower.includes("not found") || lower.includes("error")) return false;
      // Valid uptime response contains a digit and a time-unit word
      return /\d/.test(text) && /(second|minute|hour|day|week)/i.test(text);
    } catch (e) {
      this.logger.debug(`[Monitor] DECAPI Twitch uptime check failed for ${login}: ${getErrorMessage(e)}`);
      // Assume possibly live — the caller's GQL call will verify
      return true;
    }
  }

  private matches(title: string, config: ChannelConfig): boolean {
    // If no terms are configured, match all videos from this channel
    if (!config.terms) return true;

    // Get patterns to test - handle both string and object formats
    const patterns: string[] = [];
    if (typeof config.terms === "string") {
      patterns.push(config.terms);
    } else {
      patterns.push(...Object.values(config.terms));
    }

    // Test each pattern
    for (const pattern of patterns) {
      try {
        let finalPattern = pattern;
        let flags = "";

        // Handle Python/Go style (?i) case-insensitive flag
        if (finalPattern.startsWith("(?i)")) {
          finalPattern = finalPattern.slice(4);
          flags = "i";
        }

        const regex = new RegExp(finalPattern, flags);
        if (fuzzyMatch(title, regex)) return true;
      } catch (e) {
        this.logger.error(`Invalid regex for ${config.name}: ${pattern}`);
      }
    }
    return false;
  }
}
