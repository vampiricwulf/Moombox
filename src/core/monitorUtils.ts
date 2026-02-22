/**
 * Shared monitoring utilities used by FeedMonitor, DecapiMonitor, and TwitchMonitor.
 */

import { ChannelConfig } from "./config.js";
import { Database } from "./database.js";
import { YouTubeService } from "../engine/youtube/index.js";
import { TwitchService } from "../engine/twitch/index.js";
import { Logger } from "./logger.js";
import { NotificationManager, NotificationType } from "./notifications.js";
import { MoomboxConfig } from "../types/config.js";
import { fuzzyMatch } from "../utils/textNormalization.js";
import { getErrorMessage } from "../types/errors.js";
import { TWITCH_URLS } from "../constants.js";

/**
 * Check if text matches a channel's configured terms (regex patterns).
 * Returns true if no terms are configured (match all).
 */
export function matchesTerms(text: string, channel: ChannelConfig): boolean {
  if (!channel.terms) return true;

  const patterns: string[] = [];
  if (typeof channel.terms === "string") {
    patterns.push(channel.terms);
  } else {
    patterns.push(...Object.values(channel.terms));
  }

  const logger = Logger.getInstance();
  for (const pattern of patterns) {
    try {
      let finalPattern = pattern;
      let flags = "";

      if (finalPattern.startsWith("(?i)")) {
        finalPattern = finalPattern.slice(4);
        flags = "i";
      }

      const regex = new RegExp(finalPattern, flags);
      if (fuzzyMatch(text, regex)) return true;
    } catch (e) {
      logger.error(`Invalid regex for ${channel.name}: ${pattern}`);
    }
  }
  return false;
}

/** Maximum metadata fetch failures before adding to history. */
const MAX_METADATA_FAILURES = 3;

/** Maximum entries in the metadataFailures map to prevent unbounded growth. */
const MAX_METADATA_FAILURES_MAP_SIZE = 500;

/**
 * Process a YouTube video: check metadata, classify stream status, create job.
 * Returns true if a job was created.
 */
export async function processYouTubeVideo(
  videoId: string,
  title: string,
  videoUrl: string,
  channel: ChannelConfig,
  config: MoomboxConfig,
  db: Database,
  metadataFailures: Map<string, number>,
): Promise<boolean> {
  const logger = Logger.getInstance();
  const includeNonLive = channel.include_non_live_content ?? false;

  try {
    const yt = YouTubeService.getInstance();
    const meta = await yt.getMetadata(videoId);

    // Clear failure counter on success (prevents unbounded Map growth)
    metadataFailures.delete(videoId);

    if (meta.streamStatus === "not_a_stream") {
      if (!includeNonLive) {
        logger.info(`[Monitor] Skipping non-stream content: ${title} (${videoId})`);
        await db.addToHistory(videoId);
        return false;
      }
      logger.info(`[Monitor] Including non-stream content (include_non_live_content=true): ${title} (${videoId})`);
    } else if (meta.streamStatus === "upcoming") {
      logger.info(`[Monitor] Found upcoming stream/premiere: ${title} (${videoId}) - will monitor`);
    } else if (meta.streamStatus === "live") {
      logger.info(`[Monitor] Found LIVE stream/premiere: ${title} (${videoId}) - queuing for download`);
    } else if (meta.streamStatus === "post_live" || meta.streamStatus === "vod") {
      if (!includeNonLive) {
        logger.info(`[Monitor] Skipping ended stream (${meta.streamStatus}): ${title} (${videoId})`);
        await db.addToHistory(videoId);
        return false;
      }
      logger.info(`[Monitor] Including ended stream (include_non_live_content=true): ${title} (${videoId})`);
    }

    logger.debug(`[Monitor] Video ${videoId} classified as: ${meta.streamStatus}`);
  } catch (err: unknown) {
    const failures = (metadataFailures.get(videoId) || 0) + 1;
    metadataFailures.set(videoId, failures);

    if (failures >= MAX_METADATA_FAILURES) {
      logger.warn(
        `[Monitor] Failed to check metadata for ${videoId} ${failures} times, adding to history to stop retrying: ${getErrorMessage(err)}`,
      );
      metadataFailures.delete(videoId);
      await db.addToHistory(videoId);
    } else {
      logger.warn(
        `[Monitor] Failed to check metadata for ${videoId} (attempt ${failures}/${MAX_METADATA_FAILURES}): ${getErrorMessage(err)}`,
      );
    }

    // Evict oldest entries if the map exceeds the cap (prevents unbounded growth
    // from orphaned entries for videos that dropped out of the feed)
    if (metadataFailures.size > MAX_METADATA_FAILURES_MAP_SIZE) {
      const excess = metadataFailures.size - MAX_METADATA_FAILURES_MAP_SIZE;
      const iter = metadataFailures.keys();
      for (let i = 0; i < excess; i++) {
        const key = iter.next().value;
        if (key !== undefined) metadataFailures.delete(key);
      }
    }

    return false;
  }

  logger.info(`[Monitor] Queuing job: ${title} (${videoId})`);

  const newJob = await db.addJob({
    videoId,
    url: videoUrl,
    title,
    channelName: channel.name || "Unknown",
    thumbnailUrl: `https://i.ytimg.com/vi/${videoId}/maxresdefault.jpg`,
    outputDirectory: channel.output_directory || config.downloader.output_directory,
    allowNonStream: includeNonLive,
  });

  if (!newJob) {
    logger.debug(`[Monitor] Job already exists for ${videoId}, skipping`);
    return false;
  }

  await db.addToHistory(videoId);

  await NotificationManager.getInstance().send(
    "Stream Found",
    `Found matching stream: ${title}`,
    NotificationType.INFO,
    [
      { name: "Channel", value: channel.name || "Unknown", inline: true },
      { name: "Video ID", value: videoId, inline: true },
    ],
    { url: videoUrl, thumbnail: `https://i.ytimg.com/vi/${videoId}/maxresdefault.jpg`, event: "found" },
  );

  return true;
}

/**
 * Process a Twitch stream: GQL metadata, dedup, term matching, create job.
 * Returns true if a job was created.
 */
export async function processTwitchStream(
  channel: ChannelConfig,
  config: MoomboxConfig,
  db: Database,
): Promise<boolean> {
  const logger = Logger.getInstance();
  const login = channel.id.toLowerCase();
  const displayName = channel.name || login;

  const twitch = TwitchService.getInstance();
  await twitch.init();

  const streamInfo = await twitch.getStreamInfo(login);
  if (!streamInfo || !streamInfo.isLive) {
    logger.debug(`[Monitor] Twitch GQL says ${displayName} is not live`);
    return false;
  }

  // Dedup by stream ID
  const jobId = `tw_${streamInfo.streamId}`;
  if (await db.hasProcessed(jobId)) {
    logger.debug(`[Monitor] Twitch stream ${jobId} already processed`);
    return false;
  }

  // Term matching
  const titleMatch = matchesTerms(streamInfo.title || "", channel);
  const categoryMatch = streamInfo.gameCategory
    ? matchesTerms(streamInfo.gameCategory, channel)
    : false;

  if (!titleMatch && !categoryMatch) {
    logger.debug(
      `[Monitor] Twitch stream "${streamInfo.title}" (${streamInfo.gameCategory || "no category"}) ` +
      `did not match terms for ${displayName}`,
    );
    return false;
  }

  logger.info(`[Monitor] Twitch LIVE: ${displayName} — ${streamInfo.title} (${jobId})`);

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
    twitchQuality: channel.quality_preference,
  });

  if (!newJob) {
    logger.debug(`[Monitor] Twitch job already exists for ${jobId}`);
    return false;
  }

  // Stream already confirmed live — set status immediately
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

  return true;
}
