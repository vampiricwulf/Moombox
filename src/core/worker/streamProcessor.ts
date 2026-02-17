/**
 * Stream Processor
 *
 * Handles stream status checking and metadata extraction.
 */

import path from "path";
import ms from "ms";
import fs from "fs-extra";
import { Database, type Job } from "../database.js";
import { ConfigManager } from "../config.js";
import { Logger } from "../logger.js";
import { YouTubeService } from "../../engine/youtube/index.js";
import { TwitchService } from "../../engine/twitch/index.js";
import { ChatDownloader, ChatApi } from "../../engine/chat/index.js";
import { TwitchChatDownloader } from "../../engine/twitch/twitchChat.js";
import { TwitchVodChatDownloader } from "../../engine/twitch/twitchVodChat.js";
import { createEmptyVideoInfo, type VideoInfo, type StreamStatus } from "../../types/youtube.js";
import { NotificationManager, NotificationType } from "../notifications.js";
import { STREAM_RECHECK_INTERVAL_MS, PROBE_JITTER_MAX_MS, MAX_CONSECUTIVE_PROBE_ERRORS, TWITCH_URLS } from "../../constants.js";
import { getErrorMessage } from "../../types/errors.js";
import { extractTwitchLoginFromJob } from "../../utils/twitch.js";
import type { TwitchStreamInfo, TwitchHlsVariant } from "../../types/twitch.js";

/**
 * Stream processing result
 */
export interface StreamProcessResult {
  videoInfo: VideoInfo;
  shouldDownload: boolean;
  isVod: boolean;
  error?: string;
  chatDownloader?: ChatDownloader;
  // Twitch-specific
  twitchStreamInfo?: TwitchStreamInfo;
  twitchVariant?: TwitchHlsVariant;
  twitchChatDownloader?: TwitchChatDownloader | TwitchVodChatDownloader;
}

/**
 * Stream Processor
 */
export class StreamProcessor {
  private logger: Logger;
  private running: boolean = true;
  private activeChatDownloaders: Map<string, ChatDownloader | TwitchChatDownloader | TwitchVodChatDownloader> = new Map();

  constructor() {
    this.logger = Logger.getInstance();
  }

  /**
   * Stop the processor (for graceful shutdown)
   */
  stop(): void {
    this.running = false;
    for (const dl of this.activeChatDownloaders.values()) {
      dl.stop();
    }
    this.activeChatDownloaders.clear();
  }

  /**
   * Process a job to determine stream status and extract metadata
   */
  async process(job: Job): Promise<StreamProcessResult> {
    this.running = true;

    // Twitch path — completely separate from YouTube
    if (job.platform === "twitch") {
      return this.processTwitch(job);
    }

    const db = await Database.getInstance();
    const yt = YouTubeService.getInstance();
    await yt.init();
    await yt.reloadCookies();

    // Lightweight probe first — 1 unauthenticated ANDROID_VR call
    let probeInfo: VideoInfo;
    try {
      probeInfo = await yt.probeVideoStatus(job.videoId);
      this.logger.info(
        `[StreamProcessor] Video ${job.videoId} probe: ${probeInfo.streamStatus}`,
      );
    } catch (e) {
      this.logger.error(
        `[StreamProcessor] Failed to probe video: ${getErrorMessage(e)}`,
      );
      throw e;
    }

    // Update job metadata from probe
    await this.updateJobMetadata(job, probeInfo, db);

    // If probe says members-only or login-required, we need authenticated fetch
    if (
      probeInfo.playabilityError === "members_only" ||
      probeInfo.playabilityError === "login_required"
    ) {
      if (yt.hasAuthCookies()) {
        this.logger.info(
          `[StreamProcessor] Probe returned ${probeInfo.playabilityError}, retrying with auth`,
        );
        const videoInfo = await yt.getVideoInfo(job.videoId);
        await this.updateJobMetadata(job, videoInfo, db);
        const playabilityResult = this.checkPlayability(videoInfo);
        if (playabilityResult.error) {
          return { videoInfo, shouldDownload: false, isVod: false, error: playabilityResult.error };
        }
        return this.handleStreamStatus(job, videoInfo, db, true);
      }
      // No cookies — report error
      const playabilityResult = this.checkPlayability(probeInfo);
      return {
        videoInfo: probeInfo,
        shouldDownload: false,
        isVod: false,
        error: playabilityResult.error,
      };
    }

    // Check other playability errors
    const playabilityResult = this.checkPlayability(probeInfo);
    if (playabilityResult.error) {
      return {
        videoInfo: probeInfo,
        shouldDownload: false,
        isVod: false,
        error: playabilityResult.error,
      };
    }

    // Upcoming — enter probe-based polling loop
    if (probeInfo.streamStatus === "upcoming") {
      return this.waitForLive(job, probeInfo, db, false);
    }

    // Live, VOD, post_live, or not_a_stream — need full getVideoInfo for formats
    const videoInfo = await yt.getVideoInfo(job.videoId);
    await this.updateJobMetadata(job, videoInfo, db);
    return this.handleStreamStatus(job, videoInfo, db, false);
  }

  /**
   * Process a Twitch job.
   * Simpler state model than YouTube: stream is either live or offline.
   */
  private async processTwitch(job: Job): Promise<StreamProcessResult> {
    const db = await Database.getInstance();
    const twitch = TwitchService.getInstance();
    await twitch.init();

    // Extract channel login from job ID (tw_{streamId}) or URL
    const login = extractTwitchLoginFromJob(job);
    if (!login) {
      return {
        videoInfo: createEmptyVideoInfo(job.videoId),
        shouldDownload: false,
        isVod: false,
        error: "Could not determine Twitch channel login",
      };
    }

    // Check if this is a VOD job (tw_v{vodId})
    const isVodJob = job.videoId.startsWith("tw_v");

    if (isVodJob) {
      // VOD: fetch metadata and proceed directly
      const vodId = job.videoId.replace("tw_v", "");
      try {
        const vodInfo = await twitch.getVodInfo(vodId);
        const config = ConfigManager.getInstance().get();

        await db.updateJob(job.id, {
          status: "Downloading",
          isVod: true,
          title: `${vodInfo.channelDisplayName} — ${vodInfo.title}`,
          channelName: vodInfo.channelDisplayName,
          thumbnailUrl: vodInfo.thumbnailUrl,
          lengthSeconds: vodInfo.duration,
          twitchCategory: vodInfo.gameCategory,
        });

        const variants = await twitch.getVodHlsPlaylist(vodId);
        const variant = twitch.selectBestVariant(
          variants,
          job.twitchQuality,
          config.downloader.max_video_resolution,
        );

        if (!variant) {
          return {
            videoInfo: createEmptyVideoInfo(job.videoId),
            shouldDownload: false,
            isVod: true,
            error: "No suitable HLS quality found for VOD",
          };
        }

        // Start VOD chat replay downloader
        let twitchChatDl: TwitchVodChatDownloader | undefined;
        if (config.downloader.download_chat !== false) {
          const stagingDir = config.downloader.staging_directory
            ? path.join(config.downloader.staging_directory, job.id)
            : path.join("./staging", job.id);
          await fs.ensureDir(stagingDir);
          const chatPath = path.join(stagingDir, "chat.json");

          twitchChatDl = new TwitchVodChatDownloader({
            vodId,
            channelLogin: vodInfo.channelLogin,
            channelDisplayName: vodInfo.channelDisplayName,
            outputFile: chatPath,
            vodDuration: vodInfo.duration,
            vodCreatedAt: vodInfo.createdAt,
            authToken: twitch.getAuthToken(),
          });

          this.activeChatDownloaders.set(job.id, twitchChatDl);
          await db.updateJob(job.id, { chatStatus: "downloading" });

          twitchChatDl.on("progress", (data: any) => {
            db.updateJob(job.id, { totalChatMessages: data.messageCount }).catch(() => {});
          });

          // Detach before handing to orchestrator
          this.activeChatDownloaders.delete(job.id);
        } else {
          await db.updateJob(job.id, { chatStatus: "unavailable" });
        }

        this.logger.info(`[StreamProcessor] Twitch VOD ${vodId}: using quality ${variant.name}`);
        return {
          videoInfo: createEmptyVideoInfo(job.videoId),
          shouldDownload: true,
          isVod: true,
          twitchVariant: variant,
          twitchChatDownloader: twitchChatDl,
        };
      } catch (e) {
        return {
          videoInfo: createEmptyVideoInfo(job.videoId),
          shouldDownload: false,
          isVod: true,
          error: `Twitch VOD error: ${getErrorMessage(e)}`,
        };
      }
    }

    // Live stream: check if stream is still live
    const streamInfo = await twitch.getStreamInfo(login);
    if (!streamInfo || !streamInfo.isLive) {
      this.logger.info(`[StreamProcessor] Twitch channel ${login} is offline`);
      return {
        videoInfo: createEmptyVideoInfo(job.videoId),
        shouldDownload: false,
        isVod: false,
        error: "Twitch channel is offline",
      };
    }

    // Update job metadata from stream info
    const updates: Partial<Job> = {};
    if (streamInfo.title) updates.title = `${streamInfo.channelDisplayName} — ${streamInfo.title}`;
    if (streamInfo.channelDisplayName) updates.channelName = streamInfo.channelDisplayName;
    if (streamInfo.thumbnailUrl) updates.thumbnailUrl = streamInfo.thumbnailUrl;
    if (streamInfo.startedAt && !job.streamStartTime) updates.streamStartTime = streamInfo.startedAt;
    if (streamInfo.gameCategory) updates.twitchCategory = streamInfo.gameCategory;
    if (Object.keys(updates).length > 0) {
      await db.updateJob(job.id, updates);
      Object.assign(job, updates);
    }

    // Get HLS variants
    const variants = await twitch.getHlsMasterPlaylist(login);
    const config = ConfigManager.getInstance().get();
    const variant = twitch.selectBestVariant(
      variants,
      job.twitchQuality,
      config.downloader.max_video_resolution,
    );

    if (!variant) {
      return {
        videoInfo: createEmptyVideoInfo(job.videoId),
        shouldDownload: false,
        isVod: false,
        error: "No suitable HLS quality found",
      };
    }

    this.logger.info(
      `[StreamProcessor] Twitch LIVE ${login}: ${variant.name} (${variant.width}x${variant.height})`,
    );

    await db.updateJob(job.id, {
      status: "Live",
      isVod: false,
      twitchQuality: variant.name,
    });

    // Start Twitch chat downloader
    let twitchChatDl: TwitchChatDownloader | undefined;
    if (config.downloader.download_chat !== false) {
      const stagingDir = config.downloader.staging_directory
        ? path.join(config.downloader.staging_directory, job.id)
        : path.join("./staging", job.id);
      await fs.ensureDir(stagingDir);
      const chatPath = path.join(stagingDir, "chat.json");

      twitchChatDl = new TwitchChatDownloader({
        channelLogin: login,
        channelDisplayName: streamInfo.channelDisplayName,
        channelId: streamInfo.channelId,
        streamId: streamInfo.streamId,
        outputFile: chatPath,
        streamStartTime: streamInfo.startedAt,
        authToken: twitch.getAuthToken(),
      });

      // Track for shutdown propagation
      this.activeChatDownloaders.set(job.id, twitchChatDl);

      await db.updateJob(job.id, { chatStatus: "downloading" });

      twitchChatDl.on("progress", (data: any) => {
        db.updateJob(job.id, { totalChatMessages: data.messageCount }).catch(() => {});
      });
    }

    // Send live notification
    NotificationManager.getInstance().send(
      "Twitch Stream Live",
      `Now live: ${job.title}`,
      NotificationType.SUCCESS,
      [
        { name: "Channel", value: streamInfo.channelDisplayName, inline: true },
        { name: "Quality", value: variant.name, inline: true },
        ...(streamInfo.gameCategory ? [{ name: "Category", value: streamInfo.gameCategory, inline: true }] : []),
      ],
      {
        url: `${TWITCH_URLS.BASE}/${login}`,
        thumbnail: streamInfo.thumbnailUrl,
        event: "live",
      },
    );

    // Detach chat downloader from StreamProcessor before handing to orchestrator
    // (same pattern as YouTube's detachChatDownloader — prevents double-stop on shutdown)
    if (twitchChatDl) {
      this.activeChatDownloaders.delete(job.id);
    }

    return {
      videoInfo: createEmptyVideoInfo(job.videoId),
      shouldDownload: true,
      isVod: false,
      twitchStreamInfo: streamInfo,
      twitchVariant: variant,
      twitchChatDownloader: twitchChatDl,
    };
  }

  /**
   * Update job with metadata from video info
   */
  private async updateJobMetadata(
    job: Job,
    videoInfo: VideoInfo,
    db: Database,
  ): Promise<void> {
    const updates: Partial<Job> = {};
    let shouldNotifyStartTime = false;

    if (videoInfo.title && videoInfo.title !== "Unknown Title") {
      updates.title = videoInfo.title;
    }
    if (videoInfo.channelName && videoInfo.channelName !== "Unknown Channel") {
      updates.channelName = videoInfo.channelName;
    }
    if (videoInfo.description) {
      updates.description = videoInfo.description;
    }
    if (videoInfo.thumbnailUrl) {
      updates.thumbnailUrl = videoInfo.thumbnailUrl;
    }
    // Store stream start time for filename template
    if (videoInfo.scheduledStartTime && !job.streamStartTime) {
      updates.streamStartTime = videoInfo.scheduledStartTime;
      shouldNotifyStartTime = true;
    }
    // Video duration (skip 0 from live streams)
    if (videoInfo.lengthSeconds && videoInfo.lengthSeconds > 0) {
      updates.lengthSeconds = videoInfo.lengthSeconds;
    }
    // Stream end time
    if (videoInfo.endTimestamp && !job.streamEndTime) {
      updates.streamEndTime = videoInfo.endTimestamp;
    }

    if (Object.keys(updates).length > 0) {
      await db.updateJob(job.id, updates);
      Object.assign(job, updates);
      this.logger.debug(
        `[StreamProcessor] Updated job metadata: ${JSON.stringify(updates)}`,
      );
    }

    // Notify when scheduled start time is first confirmed
    if (shouldNotifyStartTime && (videoInfo.isUpcoming || videoInfo.isLive)) {
      const startDate = new Date(videoInfo.scheduledStartTime!);
      NotificationManager.getInstance().send(
        "Start Time Confirmed",
        `Scheduled: ${job.title}`,
        NotificationType.INFO,
        [
          { name: "Channel", value: job.channelName, inline: true },
          { name: "Starts At", value: startDate.toLocaleString(), inline: true },
        ],
        {
          url: `https://www.youtube.com/watch?v=${job.videoId}`,
          thumbnail: job.thumbnailUrl,
          event: "scheduled",
        },
      );
    }
  }

  /**
   * Update job metadata when stream goes live
   * This refreshes all metadata (title may change right before going live)
   * and sets streamStartTime to the actual start time from YouTube
   */
  private async updateJobMetadataOnLive(
    job: Job,
    videoInfo: VideoInfo,
    db: Database,
  ): Promise<void> {
    const updates: Partial<Job> = {};

    if (videoInfo.title && videoInfo.title !== "Unknown Title") {
      updates.title = videoInfo.title;
    }
    if (videoInfo.channelName && videoInfo.channelName !== "Unknown Channel") {
      updates.channelName = videoInfo.channelName;
    }
    if (videoInfo.description) {
      updates.description = videoInfo.description;
    }
    if (videoInfo.thumbnailUrl) {
      updates.thumbnailUrl = videoInfo.thumbnailUrl;
    }
    // Update streamStartTime to the actual start time (YouTube updates this when stream goes live)
    if (videoInfo.scheduledStartTime) {
      updates.streamStartTime = videoInfo.scheduledStartTime;
    } else if (!job.streamStartTime) {
      // No start time from YouTube — use the moment we detected it going live
      updates.streamStartTime = new Date().toISOString();
    }
    // Video duration (skip 0 from live streams)
    if (videoInfo.lengthSeconds && videoInfo.lengthSeconds > 0) {
      updates.lengthSeconds = videoInfo.lengthSeconds;
    }
    // Stream end time
    if (videoInfo.endTimestamp) {
      updates.streamEndTime = videoInfo.endTimestamp;
    }

    if (Object.keys(updates).length > 0) {
      await db.updateJob(job.id, updates);
      Object.assign(job, updates);
      this.logger.info(
        `[StreamProcessor] Refreshed metadata on live: ${JSON.stringify(updates)}`,
      );
    }
  }

  /**
   * Check playability and return error if not playable
   */
  private checkPlayability(videoInfo: VideoInfo): { error?: string } {
    if (videoInfo.playabilityError && videoInfo.playabilityError !== "ok") {
      const reason = videoInfo.playabilityReason || "Unknown error";

      switch (videoInfo.playabilityError) {
        case "members_only":
          return { error: `Member-only: ${reason}` };
        case "login_required":
          return { error: `Login required: ${reason}` };
        case "age_restricted":
          return { error: `Age restricted: ${reason}` };
        case "private":
        case "unavailable":
          return { error: reason };
        default:
          return { error: reason };
      }
    }
    return {};
  }

  /**
   * Handle stream status and determine if download should proceed
   */
  private async handleStreamStatus(
    job: Job,
    videoInfo: VideoInfo,
    db: Database,
    membersOnly: boolean = false,
  ): Promise<StreamProcessResult> {
    const streamStatus = videoInfo.streamStatus;

    // Not a stream - check if manually added or allowNonStream is set
    if (streamStatus === "not_a_stream") {
      if (job.manuallyAdded || job.allowNonStream) {
        const reason = job.manuallyAdded
          ? "manually added"
          : "include_non_live_content enabled";
        this.logger.info(
          `[StreamProcessor] ${job.videoId} is not a stream but ${reason}, downloading as VOD.`,
        );
        await db.updateJob(job.id, { status: "Downloading", isVod: true });
        return { videoInfo, shouldDownload: true, isVod: true };
      } else {
        this.logger.warn(
          `[StreamProcessor] ${job.videoId} is not a stream, skipping.`,
        );
        return {
          videoInfo,
          shouldDownload: false,
          isVod: false,
          error: "Not a livestream",
        };
      }
    }

    // Upcoming - wait for it to go live
    if (streamStatus === "upcoming") {
      return this.waitForLive(job, videoInfo, db, membersOnly);
    }

    // Live
    if (streamStatus === "live") {
      this.logger.info(
        `[StreamProcessor] Stream ${job.videoId} is Live! Starting download.`,
      );
      await db.updateJob(job.id, { status: "Live", isVod: false });
      this.sendLiveNotification(job);

      return { videoInfo, shouldDownload: true, isVod: false };
    }

    // VOD or Post-live
    if (streamStatus === "vod" || streamStatus === "post_live") {
      this.logger.info(
        `[StreamProcessor] Stream ${job.videoId} is a VOD, downloading directly.`,
      );
      await db.updateJob(job.id, { status: "Downloading", isVod: true });
      return { videoInfo, shouldDownload: true, isVod: true };
    }

    return { videoInfo, shouldDownload: false, isVod: false };
  }

  /**
   * Wait for an upcoming stream to go live
   */
  private async waitForLive(
    job: Job,
    initialVideoInfo: VideoInfo,
    db: Database,
    membersOnly: boolean = false,
  ): Promise<StreamProcessResult> {
    this.logger.info(
      `[StreamProcessor] Stream ${job.videoId} is upcoming${membersOnly ? " (members-only)" : ""}, waiting...`,
    );
    await db.updateJob(job.id, {
      status: "Upcoming",
      lastRecheckAt: new Date().toISOString(),
    });

    const yt = YouTubeService.getInstance();
    let scheduledStartTime: string | undefined;
    let consecutiveErrors = 0;

    // Start chat downloader early to capture pre-stream chat
    let chatDl: ChatDownloader | null = null;
    const config = ConfigManager.getInstance().get();
    if (config.downloader.download_chat !== false) {
      chatDl = await this.startEarlyChat(job, initialVideoInfo, yt, db);
    }

    while (this.running) {
      // Calculate dynamic recheck interval with jitter to stagger concurrent probes
      const recheckInterval = this.calculateRecheckInterval(scheduledStartTime);
      const jitter = Math.floor(Math.random() * PROBE_JITTER_MAX_MS);
      await this.delay(recheckInterval + jitter);

      // Check if job was cancelled
      const freshJob = (await db.getJobs()).find((j) => j.id === job.id);
      if (!freshJob || freshJob.status === "Cancelled") {
        // Job was cancelled - stop chat downloader
        if (chatDl) {
          chatDl.stop();
          this.activeChatDownloaders.delete(job.id);
        }
        return {
          videoInfo: createEmptyVideoInfo(job.videoId),
          shouldDownload: false,
          isVod: false,
        };
      }

      try {
        // Use authenticated probe for members-only, unauthenticated for public
        const probeInfo = membersOnly
          ? await yt.probeVideoStatusAuthenticated(job.videoId)
          : await yt.probeVideoStatus(job.videoId);

        // Probe succeeded — reset error counter
        consecutiveErrors = 0;

        // Update scheduled start time if available (persist to DB if not yet stored)
        if (probeInfo.scheduledStartTime) {
          scheduledStartTime = probeInfo.scheduledStartTime;
          if (!job.streamStartTime) {
            job.streamStartTime = probeInfo.scheduledStartTime;
            await db.updateJob(job.id, {
              streamStartTime: probeInfo.scheduledStartTime,
              lastRecheckAt: new Date().toISOString(),
            });
          } else {
            await db.updateJob(job.id, {
              lastRecheckAt: new Date().toISOString(),
            });
          }
        } else {
          await db.updateJob(job.id, {
            lastRecheckAt: new Date().toISOString(),
          });
        }

        this.logger.debug(
          `[StreamProcessor] ${job.videoId} probe${membersOnly ? " (auth)" : ""}: ${probeInfo.streamStatus}` +
            (scheduledStartTime ? ` (scheduled: ${scheduledStartTime})` : ""),
        );

        // Retry starting chat if it wasn't available earlier (chat may have opened)
        if (!chatDl && config.downloader.download_chat !== false) {
          chatDl = await this.startEarlyChat(job, probeInfo, yt, db);
        }

        // Stream transitioned — full fetch for format selection
        if (probeInfo.streamStatus === "live") {
          this.logger.info(
            `[StreamProcessor] Stream ${job.videoId} is now Live! Fetching full video info...`,
          );
          const videoInfo = await yt.getVideoInfo(job.videoId);
          await this.updateJobMetadataOnLive(job, videoInfo, db);
          await db.updateJob(job.id, { status: "Live", isVod: false });
          this.sendLiveNotification(job);
          this.detachChatDownloader(job.id, chatDl);
          return { videoInfo, shouldDownload: true, isVod: false, chatDownloader: chatDl ?? undefined };
        }

        if (
          probeInfo.streamStatus === "vod" ||
          probeInfo.streamStatus === "post_live"
        ) {
          this.logger.info(
            `[StreamProcessor] Stream ${job.videoId} ended, fetching full video info...`,
          );
          const videoInfo = await yt.getVideoInfo(job.videoId);
          await this.updateJobMetadataOnLive(job, videoInfo, db);
          await db.updateJob(job.id, { isVod: true });
          this.detachChatDownloader(job.id, chatDl);
          return { videoInfo, shouldDownload: true, isVod: true, chatDownloader: chatDl ?? undefined };
        }

        // Unauthenticated probe returned members-only — switch to authenticated probing
        if (
          !membersOnly &&
          (probeInfo.playabilityError === "members_only" ||
            probeInfo.playabilityError === "login_required") &&
          yt.hasAuthCookies()
        ) {
          this.logger.info(
            `[StreamProcessor] Stream ${job.videoId} became members-only, switching to authenticated probe`,
          );
          membersOnly = true;
        }

        // Authenticated probe no longer returns "upcoming" — stream may have transitioned
        // but the auth probe can't clearly tell us the new state. Do a full fetch to find out.
        if (membersOnly && probeInfo.streamStatus !== "upcoming") {
          this.logger.info(
            `[StreamProcessor] Auth probe unclear (${probeInfo.streamStatus}/${probeInfo.playabilityError}), doing full fetch`,
          );
          const videoInfo = await yt.getVideoInfo(job.videoId);
          if (videoInfo.streamStatus === "live") {
            await this.updateJobMetadataOnLive(job, videoInfo, db);
            await db.updateJob(job.id, { status: "Live", isVod: false });
            this.sendLiveNotification(job);
            this.detachChatDownloader(job.id, chatDl);
            return { videoInfo, shouldDownload: true, isVod: false, chatDownloader: chatDl ?? undefined };
          }
          if (videoInfo.streamStatus === "vod" || videoInfo.streamStatus === "post_live") {
            await this.updateJobMetadataOnLive(job, videoInfo, db);
            await db.updateJob(job.id, { isVod: true });
            this.detachChatDownloader(job.id, chatDl);
            return { videoInfo, shouldDownload: true, isVod: true, chatDownloader: chatDl ?? undefined };
          }
          // Still upcoming per full fetch — continue polling
        }
      } catch (e) {
        consecutiveErrors++;
        this.logger.warn(
          `[StreamProcessor] Probe failed (${consecutiveErrors}/${MAX_CONSECUTIVE_PROBE_ERRORS}): ${getErrorMessage(e)}`,
        );
        if (consecutiveErrors >= MAX_CONSECUTIVE_PROBE_ERRORS) {
          this.logger.error(
            `[StreamProcessor] ${job.videoId} exceeded max consecutive probe errors, giving up`,
          );
          if (chatDl) {
            chatDl.stop();
            this.activeChatDownloaders.delete(job.id);
          }
          return {
            videoInfo: createEmptyVideoInfo(job.videoId),
            shouldDownload: false,
            isVod: false,
            error: `Probe failed ${MAX_CONSECUTIVE_PROBE_ERRORS} times consecutively`,
          };
        }
      }
    }

    // Processor stopped - stop chat downloader
    if (chatDl) {
      chatDl.stop();
      this.activeChatDownloaders.delete(job.id);
    }
    return {
      videoInfo: createEmptyVideoInfo(job.videoId),
      shouldDownload: false,
      isVod: false,
    };
  }

  /**
   * Start chat downloader early during the upcoming phase to capture pre-stream chat
   */
  private async startEarlyChat(
    job: Job,
    videoInfo: VideoInfo,
    yt: YouTubeService,
    db: Database,
  ): Promise<ChatDownloader | null> {
    try {
      const html = await yt.fetchWatchPageHtml(job.videoId);
      const chatApi = new ChatApi();
      const { continuation, isReplay } = chatApi.extractChatContinuation(html);

      if (!continuation) {
        this.logger.debug(`[Chat] No chat available yet for upcoming ${job.videoId}`);
        return null;
      }

      const config = ConfigManager.getInstance().get();
      const stagingDir = config.downloader.staging_directory
        ? path.join(config.downloader.staging_directory, job.id)
        : path.join("./staging", job.id);
      await fs.ensureDir(stagingDir);

      const chatPath = path.join(stagingDir, "chat.json");
      const chatDl = new ChatDownloader({
        videoId: job.videoId,
        videoTitle: job.title,
        channelName: job.channelName,
        outputFile: chatPath,
        initialContinuation: continuation,
        apiKey: yt.getApiKey(),
        visitorData: yt.getVisitorData(),
        cookieHeader: yt.getCookieHeader(),
        isReplay,
        isLiveOrUpcoming: true,
        streamStartTime: videoInfo.scheduledStartTime,
      });

      await db.updateJob(job.id, { chatStatus: "downloading" });
      this.activeChatDownloaders.set(job.id, chatDl);

      chatDl.on("progress", (data) => {
        db.updateJob(job.id, { totalChatMessages: data.messageCount }).catch(() => {});
      });

      chatDl.on("finish", () => {
        this.activeChatDownloaders.delete(job.id);
        this.logger.info(`[Chat] Early chat download finished for ${job.videoId}`);
      });

      // Start in the background
      chatDl.start().catch((e) => {
        this.logger.warn(`[Chat] Early chat download error: ${e.message}`);
      });

      this.logger.info(`[Chat] Started early chat download for upcoming ${job.videoId}`);
      return chatDl;
    } catch (e: any) {
      this.logger.warn(`[Chat] Failed to start early chat: ${e.message}`);
      return null;
    }
  }

  /**
   * Calculate recheck interval based on scheduled start time
   * - 10 minutes if no start time or > 1 hour away
   * - 5 minutes if within 1 hour
   * - 1 minute if within 5 minutes
   */
  private calculateRecheckInterval(scheduledStartTime?: string): number {
    const TEN_MINUTES = ms("10m");
    const FIVE_MINUTES = ms("5m");
    const ONE_MINUTE = ms("1m");
    const ONE_HOUR = ms("1h");

    if (!scheduledStartTime) {
      return TEN_MINUTES;
    }

    try {
      const startTime = new Date(scheduledStartTime).getTime();
      const now = Date.now();
      const timeUntilStart = startTime - now;

      if (timeUntilStart <= FIVE_MINUTES) {
        this.logger.debug(
          `[StreamProcessor] Less than 5 min to start, checking every 1 min`,
        );
        return ONE_MINUTE;
      } else if (timeUntilStart <= ONE_HOUR) {
        this.logger.debug(
          `[StreamProcessor] Less than 1 hour to start, checking every 5 min`,
        );
        return FIVE_MINUTES;
      } else {
        return TEN_MINUTES;
      }
    } catch {
      return TEN_MINUTES;
    }
  }

  /**
   * Remove StreamProcessor's listeners from a chat downloader before handing it
   * off to the DownloadOrchestrator, so listeners don't leak across owners.
   */
  private detachChatDownloader(jobId: string, chatDl: ChatDownloader | null): void {
    if (!chatDl) return;
    chatDl.removeAllListeners();
    this.activeChatDownloaders.delete(jobId);
  }

  /**
   * Send notification when stream goes live
   */
  private sendLiveNotification(job: Job): void {
    NotificationManager.getInstance().send(
      "Stream Live",
      `Stream is now live: ${job.title}`,
      NotificationType.SUCCESS,
      [
        { name: "Channel", value: job.channelName, inline: true },
        { name: "Video ID", value: job.videoId, inline: true },
      ],
      {
        url: job.url || `https://www.youtube.com/watch?v=${job.videoId}`,
        thumbnail: job.thumbnailUrl,
        event: "live",
      },
    );
  }

  /**
   * Delay helper
   */
  private delay(delayMs: number): Promise<void> {
    return new Promise((resolve) => setTimeout(resolve, delayMs));
  }
}
