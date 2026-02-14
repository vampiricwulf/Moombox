/**
 * Download Orchestrator
 *
 * Orchestrates the download process: manifest fetching, format selection,
 * segment downloading, and muxing.
 */

import path from "path";
import fs from "fs-extra";
import { open as fsOpen } from "node:fs/promises";
import { Database, type Job } from "../database.js";
import { ConfigManager } from "../config.js";
import { Logger } from "../logger.js";
import { YouTubeService } from "../../engine/youtube/index.js";
import { SegmentDownloader } from "../../engine/downloader.js";
import { ChatDownloader, ChatApi } from "../../engine/chat/index.js";
import { ManifestParser } from "../../engine/manifest.js";
import { Muxer } from "../../engine/muxer.js";
import { NotificationManager, NotificationType } from "../notifications.js";
import { AssetDownloader } from "./assetDownloader.js";
import { fetchWithTimeout } from "../http.js";
import type { VideoInfo } from "../../types/youtube.js";
import type { ChatProgress } from "../../types/chat.js";
import {
  DOWNLOAD_CHUNK_SIZE,
  PROGRESS_UPDATE_INTERVAL_MS,
  USER_AGENTS,
  STREAM_SEGMENT_TIMEOUT_MS,
  STREAM_END_VERIFY_INTERVAL_MS,
} from "../../constants.js";
import { getErrorMessage } from "../../types/errors.js";

function formatElapsed(ms: number): string {
  const secs = Math.floor(ms / 1000);
  if (secs < 60) return `${secs}s`;
  const mins = Math.floor(secs / 60);
  const remSecs = secs % 60;
  if (mins < 60) return `${mins}m ${remSecs}s`;
  const hrs = Math.floor(mins / 60);
  const remMins = mins % 60;
  return `${hrs}h ${remMins}m`;
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  if (bytes < 1024 * 1024 * 1024)
    return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  return `${(bytes / 1024 / 1024 / 1024).toFixed(2)} GB`;
}

function formatSpeed(bytesPerSec: number): string {
  if (bytesPerSec < 1024) return `${bytesPerSec.toFixed(0)} B/s`;
  if (bytesPerSec < 1024 * 1024)
    return `${(bytesPerSec / 1024).toFixed(1)} KB/s`;
  return `${(bytesPerSec / 1024 / 1024).toFixed(1)} MB/s`;
}

/**
 * Result from running segment downloaders
 */
interface SegmentDownloadResult {
  finished: boolean;
  lastSegmentTime: number;
}

/**
 * Download Orchestrator
 */
export class DownloadOrchestrator {
  private logger: Logger;
  private assetDownloader: AssetDownloader;
  private activeSegmentDownloaders: Set<SegmentDownloader> = new Set();
  private activeChatDownloaders: Set<ChatDownloader> = new Set();
  private activeAbortControllers: Map<string, AbortController> = new Map();

  constructor() {
    this.logger = Logger.getInstance();
    this.assetDownloader = new AssetDownloader();
  }

  /**
   * Stop all active downloaders (called during shutdown)
   */
  stop(): void {
    for (const dl of this.activeSegmentDownloaders) {
      dl.stop();
    }
    this.activeSegmentDownloaders.clear();
    for (const dl of this.activeChatDownloaders) {
      dl.stop();
    }
    this.activeChatDownloaders.clear();
    for (const ac of this.activeAbortControllers.values()) {
      ac.abort();
    }
    this.activeAbortControllers.clear();
  }

  /**
   * Execute the download for a job
   */
  async execute(job: Job, videoInfo: VideoInfo, isVod: boolean, existingChatDl?: ChatDownloader): Promise<void> {
    const db = await Database.getInstance();
    const config = ConfigManager.getInstance().get();
    const yt = YouTubeService.getInstance();

    // Check if already cancelled before starting
    const freshJob = (await db.getJobs()).find((j) => j.id === job.id);
    if (freshJob?.status === "Cancelled") {
      existingChatDl?.stop();
      return;
    }

    // Create abort controller for this job to propagate cancellation
    const abortController = new AbortController();
    this.activeAbortControllers.set(job.id, abortController);

    // Listen for cancellation via database updates
    const unsubscribe = db.onJobUpdate((updatedJob) => {
      if (updatedJob.id === job.id && updatedJob.status === "Cancelled") {
        this.logger.info(`[DownloadOrchestrator] Cancel detected for ${job.id}, aborting...`);
        abortController.abort();
      }
    });

    const signal = abortController.signal;

    // O10: Propagate abort signal to all active downloaders
    signal.addEventListener("abort", () => {
      for (const dl of this.activeSegmentDownloaders) {
        dl.stop();
      }
      for (const dl of this.activeChatDownloaders) {
        dl.stop();
      }
    }, { once: true });

    // O9: Declare chatDl before try block so it's accessible in finally
    let chatDl: ChatDownloader | null = null;
    // O13: Track staging dir for cleanup
    let stagingDir: string | null = null;

    try {
    // Determine download strategy early (needed for notification)
    const isVodDownload =
      videoInfo.streamStatus === "vod" ||
      videoInfo.streamStatus === "post_live" ||
      videoInfo.streamStatus === "not_a_stream";

    const downloadStartedAt = new Date().toISOString();
    await db.updateJob(job.id, { status: "Downloading", downloadStartedAt });
    Object.assign(job, { downloadStartedAt });

    NotificationManager.getInstance().send(
      "Download Starting",
      `Beginning download: ${job.title}`,
      NotificationType.DOWNLOAD,
      [
        { name: "Channel", value: job.channelName, inline: true },
        { name: "Type", value: isVodDownload ? "VOD" : "Live Stream", inline: true },
      ],
      {
        url: `https://www.youtube.com/watch?v=${job.videoId}`,
        thumbnail: job.thumbnailUrl,
        event: "downloading",
      },
    );

    // Setup paths
    const outputDir =
      job.outputDirectory || config.downloader.output_directory || "./output";
    stagingDir = config.downloader.staging_directory
      ? path.join(config.downloader.staging_directory, job.id)
      : path.join("./staging", job.id);

    await fs.ensureDir(stagingDir);

    let videoPath = "";
    let audioPath: string | null = null;
    const finalExtension = ".mp4";

    let videoDl: SegmentDownloader | null = null;
    let audioDl: SegmentDownloader | null = null;

    // Use pre-started chat downloader from upcoming phase, or create a new one
    if (config.downloader.download_chat !== false) {
      if (existingChatDl) {
        chatDl = existingChatDl;
        this.activeChatDownloaders.add(chatDl);
        chatDl.on("finish", () => {
          this.activeChatDownloaders.delete(chatDl!);
          db.updateJob(job.id, { chatStatus: "finished" }).catch((e: any) => this.logger.debug(`[Chat] DB update failed: ${e.message}`));
        });
        chatDl.on("error", (err) => {
          this.logger.warn(`[Chat] Error (pre-started): ${err.message}`);
          db.updateJob(job.id, { chatStatus: "error" }).catch((e: any) => this.logger.debug(`[Chat] DB update failed: ${e.message}`));
        });
        // If chatDl already finished before we attached listeners, clean up
        if (!chatDl.isRunning()) {
          this.activeChatDownloaders.delete(chatDl);
          db.updateJob(job.id, { chatStatus: "finished" }).catch((e: any) => this.logger.debug(`[Chat] DB update failed: ${e.message}`));
        }
      } else {
        chatDl = await this.setupChatDownloader(
          job,
          videoInfo,
          yt,
          stagingDir,
          db,
        );
      }
    } else {
      // O12: Stop pre-started chat downloader when download_chat is disabled
      existingChatDl?.stop();
    }

    // Post-live VODs (streamStatus "vod" or "post_live") with a DASH manifest should
    // use DASH segment download. The player API format URLs for post-live content may
    // only serve individual segments, so direct range-based download would fail.
    // Regular uploads ("not_a_stream") use efficient direct format download.
    const useDirectVodDownload = isVodDownload && (
      videoInfo.streamStatus === "not_a_stream" || !videoInfo.dashManifestUrl
    );

    if (useDirectVodDownload) {
      // VOD: Use direct format URLs
      // Start chat downloader in background for replay
      let chatFinished = false;
      const chatPromise = chatDl
        ? chatDl.start().then(() => { chatFinished = true; }).catch((e) => {
            chatFinished = true;
            this.logger.warn(`[Chat] VOD chat download failed: ${e.message}`);
          })
        : Promise.resolve();

      const vodResult = await this.downloadVod(job, videoInfo, yt, stagingDir, db, chatDl, signal);

      // Check abort immediately after download — avoid false 100% and chat timeout wait
      if (signal?.aborted) return;

      videoPath = path.join(stagingDir, "video_stream");
      audioPath = vodResult.hasAudio ? path.join(stagingDir, "audio_stream") : null;

      // Set progress to 100% now that download is complete
      const chatCount1 = chatDl ? chatDl.getMessageCount() : 0;
      const chatSuffix1 = chatCount1 > 0 ? ` C:${chatCount1}` : "";
      await db.updateJob(job.id, { progress: `V:100% A:100%${chatSuffix1}`, percent: 100 });

      // Wait for chat to finish (with timeout so we don't block muxing forever)
      // Give chat 2 minutes after video/audio complete to finish
      const CHAT_TIMEOUT_MS = 2 * 60 * 1000;
      let chatTimeoutTimer: ReturnType<typeof setTimeout>;
      const chatTimeout = new Promise<void>((resolve) => {
        chatTimeoutTimer = setTimeout(() => {
          if (chatDl && !chatFinished) {
            this.logger.warn(
              `[Chat] Chat download timed out after ${CHAT_TIMEOUT_MS / 1000}s, proceeding to mux`,
            );
            chatDl.stop();
          }
          resolve();
        }, CHAT_TIMEOUT_MS);
      });
      await Promise.race([chatPromise, chatTimeout]);
      clearTimeout(chatTimeoutTimer!);
    } else if (videoInfo.dashManifestUrl) {
      // DASH: Live streams and post-live VODs
      const result = await this.downloadDash(
        job,
        videoInfo,
        yt,
        stagingDir,
        db,
      );
      videoDl = result.videoDl;
      audioDl = result.audioDl;
      videoPath = result.videoPath;
      audioPath = result.audioPath;
    } else if (videoInfo.hlsManifestUrl) {
      // HLS: Fallback for some streams
      const result = await this.downloadHls(job, videoInfo, yt, stagingDir, db);
      videoDl = result.videoDl;
      videoPath = result.videoPath;
    } else if (videoInfo.formats && videoInfo.formats.length > 0) {
      // Fallback: Direct format download
      // Start chat downloader in background
      let chatFinished2 = false;
      const chatPromise = chatDl
        ? chatDl.start().then(() => { chatFinished2 = true; }).catch((e) => {
            chatFinished2 = true;
            this.logger.warn(`[Chat] Chat download failed: ${e.message}`);
          })
        : Promise.resolve();

      const vodResult2 = await this.downloadVod(job, videoInfo, yt, stagingDir, db, chatDl, signal);

      // Check abort immediately after download
      if (signal?.aborted) return;

      videoPath = path.join(stagingDir, "video_stream");
      audioPath = vodResult2.hasAudio ? path.join(stagingDir, "audio_stream") : null;

      // Set progress to 100% now that download is complete
      const chatCount2 = chatDl ? chatDl.getMessageCount() : 0;
      const chatSuffix2 = chatCount2 > 0 ? ` C:${chatCount2}` : "";
      await db.updateJob(job.id, { progress: `V:100% A:100%${chatSuffix2}`, percent: 100 });

      // Wait for chat with timeout
      const CHAT_TIMEOUT_MS = 2 * 60 * 1000;
      let chatTimeoutTimer2: ReturnType<typeof setTimeout>;
      const chatTimeout = new Promise<void>((resolve) => {
        chatTimeoutTimer2 = setTimeout(() => {
          if (chatDl && !chatFinished2) {
            this.logger.warn(
              `[Chat] Chat download timed out after ${CHAT_TIMEOUT_MS / 1000}s, proceeding to mux`,
            );
            chatDl.stop();
          }
          resolve();
        }, CHAT_TIMEOUT_MS);
      });
      await Promise.race([chatPromise, chatTimeout]);
      clearTimeout(chatTimeoutTimer2!);
    } else {
      throw new Error("No DASH, HLS Manifest or direct formats found");
    }

    // Check if cancelled before proceeding to segment downloaders or muxing
    if (signal.aborted) {
      chatDl?.stop();
      this.logger.info(`[DownloadOrchestrator] Job ${job.id} cancelled, skipping remaining steps`);
      return;
    }

    // Setup progress tracking for segment downloaders (includes chat)
    // For live streams, we need to verify stream status when segments stop arriving
    if (videoDl || audioDl) {
      const isLiveStream = !isVodDownload;

      if (isLiveStream) {
        // Live stream: run with verification loop
        const isHlsDownload = !videoInfo.dashManifestUrl && !!videoInfo.hlsManifestUrl;
        await this.runLiveStreamDownload(
          job,
          videoInfo,
          videoDl,
          audioDl,
          chatDl,
          stagingDir,
          db,
          yt,
          isHlsDownload,
        );
      } else {
        // VOD: just run the segment downloaders
        await this.runSegmentDownloaders(job, videoDl, audioDl, chatDl, db);
      }
    }

    // Check if cancelled before muxing
    if (signal.aborted) {
      chatDl?.stop();
      this.logger.info(`[DownloadOrchestrator] Job ${job.id} cancelled, skipping mux`);
      return;
    }

    // After live stream download completes, sync totals to last downloaded seq
    // so progress shows matching values (e.g. "A: 150/150 V: 150/150") before muxing
    if (videoDl || audioDl) {
      const finalJob = (await db.getJobs()).find(j => j.id === job.id);
      if (finalJob) {
        const finalUpdate: Partial<Job> = {};
        if (finalJob.lastVideoSeq) finalUpdate.totalVideoSeq = finalJob.lastVideoSeq;
        if (finalJob.lastAudioSeq) finalUpdate.totalAudioSeq = finalJob.lastAudioSeq;
        if (Object.keys(finalUpdate).length > 0) {
          await db.updateJob(job.id, finalUpdate);
        }
      }
    }

    // Set streamEndTime fallback if not already set (covers live streams where
    // YouTube's endTimestamp wasn't available at fetch time)
    if (!job.streamEndTime) {
      const endTime = new Date().toISOString();
      await db.updateJob(job.id, { streamEndTime: endTime });
      Object.assign(job, { streamEndTime: endTime });
    }

    // Mux the streams
    await this.muxAndFinalize(
      job,
      videoPath,
      audioPath,
      outputDir,
      stagingDir,
      finalExtension,
      db,
      signal,
    );
    } finally {
      unsubscribe();
      this.activeAbortControllers.delete(job.id);
      // O9: Stop chat downloader if still running (e.g. on exception)
      chatDl?.stop();
      // O13: Clean up staging for cancelled jobs (errored jobs may be retried)
      if (signal.aborted && stagingDir) {
        fs.remove(stagingDir).catch((e: any) => this.logger.debug(`[DownloadOrchestrator] Staging cleanup failed: ${e.message}`));
      }
    }
  }

  /**
   * Setup chat downloader if chat is available
   */
  private async setupChatDownloader(
    job: Job,
    videoInfo: VideoInfo,
    yt: YouTubeService,
    stagingDir: string,
    db: Database,
  ): Promise<ChatDownloader | null> {
    try {
      // Fetch watch page HTML to extract chat continuation
      const html = await yt.fetchWatchPageHtml(job.videoId);
      const chatApi = new ChatApi();
      const { continuation, isReplay } = chatApi.extractChatContinuation(html);

      if (!continuation) {
        this.logger.debug(`[Chat] No chat available for ${job.videoId}`);
        await db.updateJob(job.id, { chatStatus: "unavailable" });
        return null;
      }

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
        isLiveOrUpcoming: videoInfo.isLive || videoInfo.isUpcoming,
        streamStartTime: videoInfo.scheduledStartTime,
      });

      await db.updateJob(job.id, { chatStatus: "pending" });

      this.activeChatDownloaders.add(chatDl);

      chatDl.on("start", () => {
        db.updateJob(job.id, { chatStatus: "downloading" }).catch((e: any) => this.logger.debug(`[Chat] DB update failed: ${e.message}`));
        this.logger.info(`[Chat] Started downloading chat for ${job.videoId}`);
      });

      chatDl.on("progress", (data) => {
        db.updateJob(job.id, { totalChatMessages: data.messageCount }).catch(
          (e: any) => this.logger.debug(`[Chat] DB update failed: ${e.message}`),
        );
      });

      chatDl.on("error", (err) => {
        this.logger.warn(`[Chat] Error: ${err.message}`);
        db.updateJob(job.id, { chatStatus: "error" }).catch((e: any) => this.logger.debug(`[Chat] DB update failed: ${e.message}`));
      });

      chatDl.on("finish", () => {
        this.activeChatDownloaders.delete(chatDl);
        this.logger.info(`[Chat] Finished downloading chat for ${job.videoId}`);
        db.updateJob(job.id, { chatStatus: "finished" }).catch((e: any) => this.logger.debug(`[Chat] DB update failed: ${e.message}`));
      });

      return chatDl;
    } catch (e: any) {
      this.logger.warn(`[Chat] Failed to setup chat downloader: ${e.message}`);
      await db.updateJob(job.id, { chatStatus: "error" });
      return null;
    }
  }

  /**
   * Download VOD using direct format URLs
   */
  private async downloadVod(
    job: Job,
    videoInfo: VideoInfo,
    yt: YouTubeService,
    stagingDir: string,
    db: Database,
    chatDl?: ChatDownloader | null,
    signal?: AbortSignal,
  ): Promise<{ hasAudio: boolean }> {
    this.logger.info(
      "[DownloadOrchestrator] VOD download - using direct format URLs",
    );

    const { video: bestVideo, audio: bestAudio } = yt.getBestFormats(
      videoInfo.formats,
    );

    if (!bestVideo || !bestVideo.url) {
      throw new Error("No video format with URL found for VOD");
    }

    // Store format metadata for notifications
    if (bestVideo.width || bestVideo.height) {
      const metaUpdate: Partial<Job> = {
        videoWidth: bestVideo.width,
        videoHeight: bestVideo.height,
        videoFps: bestVideo.fps,
      };
      await db.updateJob(job.id, metaUpdate);
      Object.assign(job, metaUpdate);
    }

    // Detect progressive format (video+audio pre-muxed, e.g. itag 18 at 360p)
    const isProgressive = !!(bestVideo.audioQuality && (bestVideo.width || bestVideo.height));
    if (isProgressive) {
      this.logger.warn(
        `[DownloadOrchestrator] Progressive format selected (itag ${bestVideo.itag}, ` +
        `${bestVideo.qualityLabel || (bestVideo.width + "x" + bestVideo.height)}) — ` +
        `video and audio are pre-muxed, no separate audio download needed`,
      );
    }

    this.logger.info(
      `[DownloadOrchestrator] Selected Video: ${bestVideo.width}x${bestVideo.height} @ ${bestVideo.bitrate}`,
    );
    if (bestAudio && !isProgressive) {
      this.logger.info(
        `[DownloadOrchestrator] Selected Audio: ${bestAudio.bitrate}`,
      );
    }

    const videoPath = path.join(stagingDir, "video_stream");
    const audioPath = (!isProgressive && bestAudio?.url)
      ? path.join(stagingDir, "audio_stream")
      : null;

    // Track progress
    let videoProgress = "0%";
    let audioProgress = "0%";
    let videoPercent = 0;
    let audioPercent = 0;
    let chatMsgCount = 0;

    // Track chat progress if available
    const chatProgressHandler = chatDl
      ? (data: ChatProgress) => { chatMsgCount = data.messageCount; updateCombinedProgress(); }
      : null;
    if (chatDl && chatProgressHandler) chatDl.on("progress", chatProgressHandler);

    const updateCombinedProgress = () => {
      const combinedPercent = audioPath
        ? (videoPercent + audioPercent) / 2
        : videoPercent;
      const chatStr = chatMsgCount > 0 ? ` C:${chatMsgCount}` : "";
      db.updateJob(job.id, {
        progress: `V:${videoProgress} A:${audioProgress}${chatStr}`,
        percent: combinedPercent,
      }).catch((e: any) => this.logger.debug(`[DownloadOrchestrator] Progress update failed: ${e.message}`));
    };

    // Download in parallel
    this.logger.info(
      "[DownloadOrchestrator] Downloading video and audio streams in parallel...",
    );

    const downloadPromises: Promise<void>[] = [];

    downloadPromises.push(
      this.downloadFile(bestVideo.url, videoPath, signal, (progress, percent) => {
        videoProgress =
          percent !== undefined ? `${percent.toFixed(1)}%` : progress;
        videoPercent = percent || 0;
        updateCombinedProgress();
      }),
    );

    if (bestAudio?.url && audioPath) {
      downloadPromises.push(
        this.downloadFile(bestAudio.url, audioPath, signal, (progress, percent) => {
          audioProgress =
            percent !== undefined ? `${percent.toFixed(1)}%` : progress;
          audioPercent = percent || 0;
          updateCombinedProgress();
        }),
      );
    } else {
      audioProgress = "N/A";
    }

    await Promise.all(downloadPromises);
    if (chatDl && chatProgressHandler) chatDl.removeListener("progress", chatProgressHandler);
    this.logger.info("[DownloadOrchestrator] Parallel download complete");
    return { hasAudio: !isProgressive && !!audioPath };
  }

  /**
   * Download DASH stream
   */
  private async downloadDash(
    job: Job,
    videoInfo: VideoInfo,
    yt: YouTubeService,
    stagingDir: string,
    db: Database,
  ): Promise<{
    videoDl: SegmentDownloader;
    audioDl: SegmentDownloader;
    videoPath: string;
    audioPath: string;
  }> {
    this.logger.info("[DownloadOrchestrator] Fetching DASH Manifest...");

    let decryptedDashUrl = await yt.decryptDashManifestUrl(
      videoInfo.dashManifestUrl!,
      videoInfo.playerUrl,
    );

    // Add PO token to manifest URL (required for live streams)
    // yt-dlp appends /pot/{token} to the manifest path
    const poToken = yt.getPoToken();
    if (poToken) {
      // Remove trailing slash if present, then append /pot/{token}
      decryptedDashUrl = decryptedDashUrl.replace(/\/?$/, `/pot/${poToken}`);
      this.logger.debug(
        `[DownloadOrchestrator] Added PO token to manifest URL`,
      );
    } else {
      this.logger.warn(
        "[DownloadOrchestrator] No PO token available for manifest URL - may get 403 errors",
      );
    }

    const manifestResp = await fetchWithTimeout(decryptedDashUrl);
    if (!manifestResp.ok) {
      throw new Error(
        `Failed to fetch DASH manifest: HTTP ${manifestResp.status}`,
      );
    }

    const manifestXml = await manifestResp.text();
    const dashStreams = ManifestParser.parseDash(
      manifestXml,
      videoInfo.dashManifestUrl!,
    );

    this.logger.debug(
      `[DownloadOrchestrator] DASH manifest parsed, found ${dashStreams.length} streams`,
    );

    // Decrypt n parameter in segment BaseURLs (required to avoid throttling/403)
    if (videoInfo.playerUrl) {
      this.logger.debug(
        `[DownloadOrchestrator] Decrypting n parameter in segment BaseURLs...`,
      );
      await Promise.all(dashStreams.map(async (stream) => {
        if (stream.baseUrl) {
          const originalUrl = stream.baseUrl;
          stream.baseUrl = await yt.decryptNParamInUrl(
            stream.baseUrl,
            videoInfo.playerUrl!,
          );
          if (stream.baseUrl !== originalUrl) {
            this.logger.debug(
              `[DownloadOrchestrator] Decrypted n param for itag=${stream.itag}`,
            );
          }
        }
      }));
    } else {
      this.logger.warn(
        `[DownloadOrchestrator] No player URL available - cannot decrypt n params`,
      );
    }

    // Select best streams
    let bestVideo: any = null;
    let bestAudio: any = null;

    const config = ConfigManager.getInstance().get();
    const maxRes = config.downloader?.max_video_resolution || 9999;

    for (const stream of dashStreams) {
      this.logger.debug(
        `[DownloadOrchestrator] Stream: itag=${stream.itag} mime=${stream.mimeType} ${stream.width || 0}x${stream.height || 0} bw=${stream.bandwidth}`,
      );
      if (stream.mimeType?.includes("video")) {
        const streamMaxDim = Math.max(stream.width || 0, stream.height || 0);
        if (streamMaxDim > maxRes) continue;
        if (!bestVideo || stream.bandwidth > bestVideo.bandwidth) {
          bestVideo = stream;
        }
      } else if (stream.mimeType?.includes("audio")) {
        if (!bestAudio || stream.bandwidth > bestAudio.bandwidth) {
          bestAudio = stream;
        }
      }
    }

    if (!bestVideo || !bestAudio) {
      this.logger.error(
        `[DownloadOrchestrator] Stream selection failed: video=${!!bestVideo} audio=${!!bestAudio}`,
      );
      this.logger.debug(
        `[DownloadOrchestrator] Raw manifest (first 2000 chars): ${manifestXml.substring(0, 2000)}`,
      );
      throw new Error(
        "Could not find suitable Video/Audio streams in DASH Manifest",
      );
    }

    this.logger.info(
      `[DownloadOrchestrator] Selected DASH Video: ${bestVideo.width}x${bestVideo.height} @ ${bestVideo.bandwidth}`,
    );
    this.logger.info(
      `[DownloadOrchestrator] Selected DASH Audio: ${bestAudio.bandwidth}`,
    );

    // Store format metadata for notifications
    if (bestVideo.width || bestVideo.height) {
      const metaUpdate: Partial<Job> = {
        videoWidth: bestVideo.width,
        videoHeight: bestVideo.height,
      };
      await db.updateJob(job.id, metaUpdate);
      Object.assign(job, metaUpdate);
    }

    const videoPath = path.join(stagingDir, "video_stream");
    const audioPath = path.join(stagingDir, "audio_stream");

    // For live-from-start, always start from segment 0 to capture the entire stream
    // The manifest's startNumber reflects the current live position, not the beginning
    const startFromBeginning = 0;
    this.logger.info(
      `[DownloadOrchestrator] Starting from segment ${startFromBeginning} (manifest startNumber: ${bestVideo.startNumber})`,
    );

    // Callback to check if stream has ended via YouTube API
    const checkStreamStatus = async (): Promise<boolean> => {
      try {
        const freshInfo = await yt.getVideoInfo(job.videoId);
        this.logger.info(`[DownloadOrchestrator] Status check: ${freshInfo.streamStatus}`);
        return freshInfo.streamStatus === "post_live"
            || freshInfo.streamStatus === "vod"
            || freshInfo.streamStatus === "not_a_stream";
      } catch (e) {
        this.logger.warn(`[DownloadOrchestrator] Status check failed: ${getErrorMessage(e)}`);
        return false;
      }
    };

    const videoDl = new SegmentDownloader({
      baseUrl: bestVideo.baseUrl,
      outputFile: videoPath,
      startSeq: startFromBeginning,
      initUrl: bestVideo.initialization,
      onCheckStreamStatus: checkStreamStatus,
      retryDelayCap: config.downloader.segment_retry_delay_cap,
      liveCheckRetries: config.downloader.segment_live_check_retries,
    });

    const audioDl = new SegmentDownloader({
      baseUrl: bestAudio.baseUrl,
      outputFile: audioPath,
      startSeq: startFromBeginning,
      initUrl: bestAudio.initialization,
      onCheckStreamStatus: checkStreamStatus,
      retryDelayCap: config.downloader.segment_retry_delay_cap,
      liveCheckRetries: config.downloader.segment_live_check_retries,
    });

    this.activeSegmentDownloaders.add(videoDl);
    this.activeSegmentDownloaders.add(audioDl);

    this.logger.info(
      "[DownloadOrchestrator] Starting Parallel DASH Download...",
    );

    return { videoDl, audioDl, videoPath, audioPath };
  }

  /**
   * Download HLS stream
   */
  private async downloadHls(
    job: Job,
    videoInfo: VideoInfo,
    yt: YouTubeService,
    stagingDir: string,
    db: Database,
  ): Promise<{ videoDl: SegmentDownloader; videoPath: string }> {
    this.logger.info("[DownloadOrchestrator] Fetching HLS Master Playlist...");

    const manifestResp = await fetchWithTimeout(videoInfo.hlsManifestUrl!);
    if (!manifestResp.ok) {
      throw new Error(
        `Failed to fetch HLS manifest: HTTP ${manifestResp.status}`,
      );
    }
    const manifestText = await manifestResp.text();
    const masterPlaylist = ManifestParser.parseHls(
      manifestText,
      videoInfo.hlsManifestUrl!,
    );

    if (masterPlaylist.type !== "master" || !masterPlaylist.variants) {
      throw new Error("Invalid HLS Master Playlist");
    }

    // Select best variant (respecting max_video_resolution)
    const config = ConfigManager.getInstance().get();
    const maxRes = config.downloader?.max_video_resolution || 9999;
    let bestVariant: any = null;
    for (const variant of masterPlaylist.variants) {
      const varMaxDim = Math.max(variant.width || 0, variant.height || 0);
      if (varMaxDim > maxRes) continue;
      if (!bestVariant || variant.bandwidth > bestVariant.bandwidth) {
        bestVariant = variant;
      }
    }

    if (!bestVariant) {
      throw new Error("No variants found in HLS Playlist (within resolution limit)");
    }

    this.logger.info(
      `[DownloadOrchestrator] Selected HLS Variant: ${bestVariant.width}x${bestVariant.height} @ ${bestVariant.bandwidth}`,
    );

    // Store format metadata for notifications
    if (bestVariant.width || bestVariant.height) {
      const metaUpdate: Partial<Job> = {
        videoWidth: bestVariant.width,
        videoHeight: bestVariant.height,
      };
      await db.updateJob(job.id, metaUpdate);
      Object.assign(job, metaUpdate);
    }

    const videoPath = path.join(stagingDir, "stream.ts");

    // Callback to check if stream has ended via YouTube API
    const checkStreamStatus = async (): Promise<boolean> => {
      try {
        const freshInfo = await yt.getVideoInfo(job.videoId);
        this.logger.info(`[DownloadOrchestrator] HLS status check: ${freshInfo.streamStatus}`);
        return freshInfo.streamStatus === "post_live"
            || freshInfo.streamStatus === "vod"
            || freshInfo.streamStatus === "not_a_stream";
      } catch (e) {
        this.logger.warn(`[DownloadOrchestrator] HLS status check failed: ${getErrorMessage(e)}`);
        return false;
      }
    };

    const videoDl = new SegmentDownloader({
      baseUrl: bestVariant.url,
      outputFile: videoPath,
      startSeq: -1,
      isHls: true,
      onCheckStreamStatus: checkStreamStatus,
      retryDelayCap: config.downloader.segment_retry_delay_cap,
      liveCheckRetries: config.downloader.segment_live_check_retries,
    });

    this.activeSegmentDownloaders.add(videoDl);

    this.logger.info("[DownloadOrchestrator] Starting HLS Download...");

    return { videoDl, videoPath };
  }

  /**
   * Run segment downloaders with progress tracking
   */
  private async runSegmentDownloaders(
    job: Job,
    videoDl: SegmentDownloader | null,
    audioDl: SegmentDownloader | null,
    chatDl: ChatDownloader | null,
    db: Database,
  ): Promise<void> {
    let vSeq = 0;
    let aSeq = 0;
    let vTotal = 0;
    let aTotal = 0;
    let vBytes = 0;
    let aBytes = 0;
    let chatMsgCount = 0;
    let lastUpdate = 0;
    let lastBytes = 0;
    let lastBytesTime = Date.now();

    const updateProgress = () => {
      const now = Date.now();
      if (now - lastUpdate > PROGRESS_UPDATE_INTERVAL_MS) {
        lastUpdate = now;

        const totalBytes = vBytes + aBytes;
        const timeDelta = (now - lastBytesTime) / 1000;
        const bytesDelta = totalBytes - lastBytes;
        const speed = timeDelta > 0 ? bytesDelta / timeDelta : 0;

        lastBytes = totalBytes;
        lastBytesTime = now;

        let progStr = "";
        if (audioDl) {
          const vPart = vTotal > 0 ? `${vSeq}/${vTotal}` : `${vSeq}`;
          const aPart = aTotal > 0 ? `${aSeq}/${aTotal}` : `${aSeq}`;
          progStr = `(A: ${aPart} V: ${vPart}`;
          if (chatDl && chatMsgCount > 0) {
            progStr += ` C: ${chatMsgCount}`;
          }
          progStr += ")";
        } else {
          progStr = `Seq: ${vSeq}`;
          if (chatDl && chatMsgCount > 0) {
            progStr += ` C: ${chatMsgCount}`;
          }
        }

        db.updateJob(job.id, {
          progress: progStr,
          speed: formatSpeed(speed),
          lastVideoSeq: vSeq,
          lastAudioSeq: aSeq,
          totalVideoSeq: vTotal || undefined,
          totalAudioSeq: aTotal || undefined,
        }).catch((e: any) => this.logger.debug(`[DownloadOrchestrator] Progress update failed: ${e.message}`));
      }
    };

    if (videoDl) {
      videoDl.on("progress", (d) => {
        vSeq = d.seq;
        vBytes = d.bytes || 0;
        if (d.total || d.headSeq) vTotal = d.total || d.headSeq;
        updateProgress();
      });
    }

    if (audioDl) {
      audioDl.on("progress", (d) => {
        aSeq = d.seq;
        aBytes = d.bytes || 0;
        if (d.total || d.headSeq) aTotal = d.total || d.headSeq;
        updateProgress();
      });
    }

    const chatProgressHandler = chatDl
      ? (d: ChatProgress) => { chatMsgCount = d.messageCount; updateProgress(); }
      : null;
    if (chatDl && chatProgressHandler) chatDl.on("progress", chatProgressHandler);

    const promises: Promise<void>[] = [];
    if (videoDl) promises.push(videoDl.start().finally(() => this.activeSegmentDownloaders.delete(videoDl!)));
    if (audioDl) promises.push(audioDl.start().finally(() => this.activeSegmentDownloaders.delete(audioDl!)));
    if (chatDl)
      promises.push(
        chatDl.start().catch((e) => {
          this.logger.warn(`[Chat] Chat download error: ${e.message}`);
        }),
      );
    await Promise.all(promises);

    // Remove chat listener to prevent leak
    if (chatDl && chatProgressHandler) chatDl.removeListener("progress", chatProgressHandler);
  }

  /**
   * Cancellable delay — resolves after `ms` or rejects immediately if signal fires.
   */
  private cancellableDelay(ms: number, signal?: AbortSignal): Promise<void> {
    if (signal?.aborted) return Promise.reject(new DOMException("Aborted", "AbortError"));
    return new Promise((resolve, reject) => {
      const onAbort = () => {
        clearTimeout(timer);
        reject(new DOMException("Aborted", "AbortError"));
      };
      const timer = setTimeout(() => {
        signal?.removeEventListener("abort", onAbort);
        resolve();
      }, ms);
      signal?.addEventListener("abort", onAbort, { once: true });
    });
  }

  /**
   * Run live stream download with stream-end verification
   *
   * When segment downloaders stop (due to consecutive 403/410 errors),
   * we verify with YouTube API whether the stream has actually ended.
   * If YouTube still reports "is live", we wait and try again.
   * Only when YouTube confirms "was live" (post_live/vod) do we proceed to muxing.
   */
  private async runLiveStreamDownload(
    job: Job,
    videoInfo: VideoInfo,
    videoDl: SegmentDownloader | null,
    audioDl: SegmentDownloader | null,
    chatDl: ChatDownloader | null,
    stagingDir: string,
    db: Database,
    yt: YouTubeService,
    isHls: boolean = false,
  ): Promise<void> {
    let lastSegmentTime = Date.now();
    let streamConfirmedEnded = false;

    // Track segment activity
    const onSegmentProgress = () => {
      lastSegmentTime = Date.now();
    };

    if (videoDl) videoDl.on("progress", onSegmentProgress);
    if (audioDl) audioDl.on("progress", onSegmentProgress);

    // Start chat downloader in background (it runs independently)
    const chatPromise = chatDl
      ? chatDl.start().catch((e) => {
          this.logger.warn(`[Chat] Live chat download error: ${e.message}`);
        })
      : Promise.resolve();

    const signal = this.activeAbortControllers.get(job.id)?.signal;

    while (!streamConfirmedEnded) {
      if (signal?.aborted) break;

      // Run video/audio segment downloaders (they will stop when stream appears to end)
      await this.runSegmentDownloadersWithoutChat(job, videoDl, audioDl, chatDl, db);

      // Check how long since last segment
      const timeSinceLastSegment = Date.now() - lastSegmentTime;

      this.logger.info(
        `[DownloadOrchestrator] Segment downloaders stopped. ` +
          `Time since last segment: ${Math.round(timeSinceLastSegment / 1000)}s`,
      );

      // Always verify with YouTube API
      try {
        this.logger.info(
          `[DownloadOrchestrator] Verifying stream status with YouTube API...`,
        );
        const freshInfo = await yt.getVideoInfo(job.videoId);

        this.logger.info(
          `[DownloadOrchestrator] YouTube reports stream status: ${freshInfo.streamStatus}`,
        );

        if (
          freshInfo.streamStatus === "post_live" ||
          freshInfo.streamStatus === "vod" ||
          freshInfo.streamStatus === "not_a_stream"
        ) {
          // Stream has ended, proceed to muxing
          streamConfirmedEnded = true;
          this.logger.info(
            `[DownloadOrchestrator] Stream confirmed ended (${freshInfo.streamStatus}), proceeding to mux`,
          );
        } else if (freshInfo.streamStatus === "live") {
          // Stream is still live - wait and try again
          this.logger.info(
            `[DownloadOrchestrator] Stream still reports as live. ` +
              `Waiting ${STREAM_END_VERIFY_INTERVAL_MS / 60000} minutes before retrying...`,
          );

          await db.updateJob(job.id, {
            progress: "Waiting for stream to end...",
          });

          // Wait before checking again (cancellable on shutdown)
          try {
            await this.cancellableDelay(STREAM_END_VERIFY_INTERVAL_MS, signal);
          } catch { break; }

          // Refresh manifests and create new segment downloaders
          this.logger.info(
            `[DownloadOrchestrator] Refreshing manifests and resuming download...`,
          );

          try {
            if (isHls) {
              const refreshedResult = await this.downloadHls(
                job,
                freshInfo,
                yt,
                stagingDir,
                db,
              );
              videoDl = refreshedResult.videoDl;
              audioDl = null;
            } else {
              const refreshedResult = await this.downloadDash(
                job,
                freshInfo,
                yt,
                stagingDir,
                db,
              );
              videoDl = refreshedResult.videoDl;
              audioDl = refreshedResult.audioDl;
            }

            // Re-attach progress handlers — onSegmentProgress will update
            // lastSegmentTime naturally when segments actually download.
            // Do NOT reset lastSegmentTime here, otherwise the
            // STREAM_SEGMENT_TIMEOUT_MS fallback can never trigger (O6).
            if (videoDl) videoDl.on("progress", onSegmentProgress);
            if (audioDl) audioDl.on("progress", onSegmentProgress);
          } catch (e) {
            this.logger.warn(
              `[DownloadOrchestrator] Failed to refresh manifests: ${getErrorMessage(e)}. ` +
                `Will check status again in ${STREAM_END_VERIFY_INTERVAL_MS / 60000} minutes...`,
            );
            try {
              await this.cancellableDelay(STREAM_END_VERIFY_INTERVAL_MS, signal);
            } catch { break; }
          }
        } else {
          // Upcoming or unknown status - treat as ended to avoid infinite loop
          this.logger.warn(
            `[DownloadOrchestrator] Unexpected stream status: ${freshInfo.streamStatus}. ` +
              `Treating as ended.`,
          );
          streamConfirmedEnded = true;
        }
      } catch (e) {
        // API error - if we've been waiting a while without segments, assume ended
        this.logger.warn(
          `[DownloadOrchestrator] Failed to verify stream status: ${getErrorMessage(e)}`,
        );

        if (timeSinceLastSegment >= STREAM_SEGMENT_TIMEOUT_MS) {
          this.logger.info(
            `[DownloadOrchestrator] No segments for ${STREAM_SEGMENT_TIMEOUT_MS / 60000} minutes ` +
              `and API check failed. Assuming stream ended.`,
          );
          streamConfirmedEnded = true;
        } else {
          // Wait and try API again
          this.logger.info(
            `[DownloadOrchestrator] Will retry API check in ${STREAM_END_VERIFY_INTERVAL_MS / 60000} minutes...`,
          );
          try {
            await this.cancellableDelay(STREAM_END_VERIFY_INTERVAL_MS, signal);
          } catch { break; }
        }
      }
    }

    // Signal stream ended — chat loop exits promptly (within 500ms),
    // writes final file, and clears resume state (graceful completion).
    if (chatDl) chatDl.markStreamEnded();

    // Wait for chat to finish
    await chatPromise;
  }

  /**
   * Run segment downloaders for video/audio only (chat handled separately)
   */
  private async runSegmentDownloadersWithoutChat(
    job: Job,
    videoDl: SegmentDownloader | null,
    audioDl: SegmentDownloader | null,
    chatDl: ChatDownloader | null,
    db: Database,
  ): Promise<void> {
    let vSeq = 0;
    let aSeq = 0;
    let vTotal = 0;
    let aTotal = 0;
    let vBytes = 0;
    let aBytes = 0;
    let chatMsgCount = 0;
    let lastUpdate = 0;
    let lastBytes = 0;
    let lastBytesTime = Date.now();

    const updateProgress = () => {
      const now = Date.now();
      if (now - lastUpdate > PROGRESS_UPDATE_INTERVAL_MS) {
        lastUpdate = now;

        const totalBytes = vBytes + aBytes;
        const timeDelta = (now - lastBytesTime) / 1000;
        const bytesDelta = totalBytes - lastBytes;
        const speed = timeDelta > 0 ? bytesDelta / timeDelta : 0;

        lastBytes = totalBytes;
        lastBytesTime = now;

        let progStr = "";
        if (audioDl) {
          const vPart = vTotal > 0 ? `${vSeq}/${vTotal}` : `${vSeq}`;
          const aPart = aTotal > 0 ? `${aSeq}/${aTotal}` : `${aSeq}`;
          progStr = `(A: ${aPart} V: ${vPart}`;
          if (chatDl && chatMsgCount > 0) {
            progStr += ` C: ${chatMsgCount}`;
          }
          progStr += ")";
        } else {
          progStr = `Seq: ${vSeq}`;
          if (chatDl && chatMsgCount > 0) {
            progStr += ` C: ${chatMsgCount}`;
          }
        }

        db.updateJob(job.id, {
          progress: progStr,
          speed: formatSpeed(speed),
          lastVideoSeq: vSeq,
          lastAudioSeq: aSeq,
          totalVideoSeq: vTotal || undefined,
          totalAudioSeq: aTotal || undefined,
        }).catch((e: any) => this.logger.debug(`[DownloadOrchestrator] Progress update failed: ${e.message}`));
      }
    };

    if (videoDl) {
      videoDl.on("progress", (d) => {
        vSeq = d.seq;
        vBytes = d.bytes || 0;
        if (d.total || d.headSeq) vTotal = d.total || d.headSeq;
        updateProgress();
      });
    }

    if (audioDl) {
      audioDl.on("progress", (d) => {
        aSeq = d.seq;
        aBytes = d.bytes || 0;
        if (d.total || d.headSeq) aTotal = d.total || d.headSeq;
        updateProgress();
      });
    }

    // Chat listeners are registered once in runLiveStreamDownload to avoid
    // leaking listeners across loop iterations. Read chatDl count directly.
    if (chatDl) {
      chatMsgCount = chatDl.getMessageCount();
    }

    const chatProgressHandler = chatDl
      ? (d: ChatProgress) => { chatMsgCount = d.messageCount; updateProgress(); }
      : null;
    if (chatDl && chatProgressHandler) chatDl.on("progress", chatProgressHandler);

    const promises: Promise<void>[] = [];
    if (videoDl) promises.push(videoDl.start().finally(() => this.activeSegmentDownloaders.delete(videoDl!)));
    if (audioDl) promises.push(audioDl.start().finally(() => this.activeSegmentDownloaders.delete(audioDl!)));
    await Promise.all(promises);

    // Remove chat listener to prevent leak when called in a loop
    if (chatDl && chatProgressHandler) chatDl.removeListener("progress", chatProgressHandler);
  }

  /**
   * Mux streams and finalize job
   */
  private async muxAndFinalize(
    job: Job,
    videoPath: string,
    audioPath: string | null,
    outputDir: string,
    stagingDir: string,
    finalExtension: string,
    db: Database,
    signal?: AbortSignal,
  ): Promise<void> {
    const config = ConfigManager.getInstance().get();

    this.logger.info("[DownloadOrchestrator] Muxing...");
    await db.updateJob(job.id, { status: "Muxing" });

    // "Muxing Starting" notification
    {
      const freshJob = (await db.getJobs()).find(j => j.id === job.id);
      const muxFields: { name: string; value: string; inline?: boolean }[] = [];
      if (freshJob?.lastVideoSeq) muxFields.push({ name: "Video Segments", value: String(freshJob.lastVideoSeq), inline: true });
      if (freshJob?.lastAudioSeq) muxFields.push({ name: "Audio Segments", value: String(freshJob.lastAudioSeq), inline: true });
      if (freshJob?.totalChatMessages) muxFields.push({ name: "Chat Messages", value: String(freshJob.totalChatMessages), inline: true });
      if (freshJob?.downloadStartedAt) {
        const elapsed = Date.now() - new Date(freshJob.downloadStartedAt).getTime();
        muxFields.push({ name: "Download Time", value: formatElapsed(elapsed), inline: true });
      }
      NotificationManager.getInstance().send(
        "Muxing Starting",
        `Download complete, muxing: ${job.title}`,
        NotificationType.MUXING,
        muxFields,
        {
          url: `https://www.youtube.com/watch?v=${job.videoId}`,
          thumbnail: job.thumbnailUrl,
          event: "muxing",
        },
      );
    }

    const template = config.downloader.output_template || "${title} [${id}]";
    // Use stream start time if available, otherwise fall back to job creation time
    const streamDate = job.streamStartTime
      ? new Date(job.streamStartTime)
      : new Date(job.createdAt);
    const filenameBase = ConfigManager.resolveTemplate(template, {
      title: job.title,
      id: job.id,
      channel: job.channelName || "Unknown",
      date: streamDate,
    });

    const finalFilename = `${filenameBase}${finalExtension}`;
    const finalPath = path.join(outputDir, finalFilename);

    try {
      await Muxer.mux(videoPath, audioPath, finalPath, signal);
      this.logger.info(`[DownloadOrchestrator] Muxing complete: ${finalPath}`);

      // Save description and thumbnail
      if (job.description) {
        await this.assetDownloader.saveDescription(
          job.description,
          outputDir,
          filenameBase,
        );
      }

      if (job.thumbnailUrl) {
        await this.assetDownloader.downloadThumbnail(
          job.thumbnailUrl,
          outputDir,
          filenameBase,
        );
      }

      // Copy chat file if exists
      const chatStagingPath = path.join(stagingDir, "chat.json");
      let chatFilename: string | undefined;
      if (await fs.pathExists(chatStagingPath)) {
        chatFilename = `${filenameBase}.chat.json`;
        const chatOutputPath = path.join(outputDir, chatFilename);
        await fs.copy(chatStagingPath, chatOutputPath);
        this.logger.info(`[Chat] Saved chat to ${chatOutputPath}`);
      }

      // Get file size
      let fileSize: number | undefined;
      try {
        const stats = await fs.stat(finalPath);
        fileSize = stats.size;
      } catch {}

      await db.updateJob(job.id, {
        status: "Finished",
        filename: finalFilename,
        progress: "",
        percent: 100,
        speed: "",
        eta: "",
        chatStatus: chatFilename ? "finished" : job.chatStatus,
        chatFilename,
        fileSize,
      });

      // Build rich "Download Finished" notification
      const finishedJob = (await db.getJobs()).find(j => j.id === job.id);
      const finFields: { name: string; value: string; inline?: boolean }[] = [];
      finFields.push({ name: "File", value: finalFilename, inline: false });

      if (job.videoWidth && job.videoHeight) {
        const res = `${job.videoWidth}x${job.videoHeight}${job.videoFps ? ` @${job.videoFps}fps` : ""}`;
        finFields.push({ name: "Resolution", value: res, inline: true });
      }

      if (fileSize) {
        const sizeStr = fileSize > 1024 ** 3
          ? `${(fileSize / 1024 ** 3).toFixed(2)} GB`
          : `${(fileSize / 1024 ** 2).toFixed(1)} MB`;
        finFields.push({ name: "File Size", value: sizeStr, inline: true });
      }

      if (finishedJob?.lengthSeconds && finishedJob.lengthSeconds > 0) {
        finFields.push({ name: "Duration", value: formatElapsed(finishedJob.lengthSeconds * 1000), inline: true });
      }

      if (finishedJob?.downloadStartedAt) {
        const elapsed = Date.now() - new Date(finishedJob.downloadStartedAt).getTime();
        finFields.push({ name: "Total Time", value: formatElapsed(elapsed), inline: true });
      }

      if (finishedJob?.lastVideoSeq) {
        finFields.push({
          name: "Segments",
          value: `V: ${finishedJob.lastVideoSeq}${finishedJob.lastAudioSeq ? ` A: ${finishedJob.lastAudioSeq}` : ""}`,
          inline: true,
        });
      }

      if (finishedJob?.totalChatMessages) {
        finFields.push({ name: "Chat Messages", value: String(finishedJob.totalChatMessages), inline: true });
      }

      if (job.description) {
        const desc = job.description.length > 300 ? job.description.substring(0, 297) + "..." : job.description;
        finFields.push({ name: "Description", value: desc, inline: false });
      }

      NotificationManager.getInstance().send(
        "Download Finished",
        `Successfully archived: ${job.title}`,
        NotificationType.SUCCESS,
        finFields,
        {
          url: `https://www.youtube.com/watch?v=${job.videoId}`,
          image: job.thumbnailUrl,
          event: "finished",
        },
      );

      // Cleanup staging
      await fs.remove(stagingDir);
    } catch (e: any) {
      // Don't mark as error if cancelled — the main execute() handler deals with that
      if (e?.name === "AbortError") {
        await fs.remove(finalPath).catch((e: any) => this.logger.debug(`[DownloadOrchestrator] Cleanup failed: ${e.message}`));
        throw e;
      }
      this.logger.error(
        `[DownloadOrchestrator] Muxing failed: ${getErrorMessage(e)}`,
      );
      await db.updateJob(job.id, { status: "Error", error: "Muxing Failed" });
      throw e;
    }
  }

  /**
   * Download a file with chunked range requests
   */
  private async downloadFile(
    url: string,
    outputPath: string,
    signal?: AbortSignal,
    onProgress?: (progress: string, percent?: number) => void,
  ): Promise<void> {
    const headers: Record<string, string> = {
      "User-Agent": USER_AGENTS.ANDROID,
    };

    // Get file size
    let totalBytes = 0;
    try {
      const probeResponse = await fetch(url, {
        method: "GET",
        headers: { ...headers, Range: "bytes=0-0" },
        signal,
      });

      if (probeResponse.status === 206) {
        const contentRange = probeResponse.headers.get("content-range");
        if (contentRange) {
          const match = contentRange.match(/bytes \d+-\d+\/(\d+)/);
          if (match) {
            totalBytes = parseInt(match[1], 10);
          }
        }
      }
      await probeResponse.arrayBuffer();
    } catch (e: any) {
      if (e?.name === "AbortError" || signal?.aborted) return;
      this.logger.debug(
        "[DownloadOrchestrator] Size probe failed, downloading without size info",
      );
    }

    if (signal?.aborted) return;

    // Download in chunks
    let downloadedBytes = 0;
    let lastProgressUpdate = 0;
    const fileHandle = await fsOpen(outputPath, "w");

    try {
      while (true) {
        if (signal?.aborted) break;

        const start = downloadedBytes;
        const end =
          totalBytes > 0
            ? Math.min(start + DOWNLOAD_CHUNK_SIZE - 1, totalBytes - 1)
            : start + DOWNLOAD_CHUNK_SIZE - 1;

        let response!: Response;
        for (let chunkAttempt = 0; ; chunkAttempt++) {
          try {
            response = await fetch(url, {
              headers: { ...headers, Range: `bytes=${start}-${end}` },
              signal,
            });
            if (response.status >= 500) {
              await response.arrayBuffer();
              throw new Error(`HTTP ${response.status}: ${response.statusText}`);
            }
            break;
          } catch (e: any) {
            if (e?.name === "AbortError" || signal?.aborted) throw e;
            if (chunkAttempt >= 2) throw e;
            this.logger.debug(
              `[DownloadOrchestrator] Chunk retry ${chunkAttempt + 1}/3 for bytes ${start}-${end}`,
            );
            await new Promise((r) => setTimeout(r, 1000 * (chunkAttempt + 1)));
          }
        }

        if (response.status === 416) {
          await response.arrayBuffer();
          break;
        }

        if (!response.ok && response.status !== 206) {
          await response.arrayBuffer();
          throw new Error(`HTTP ${response.status}: ${response.statusText}`);
        }

        const data = await response.arrayBuffer();
        const buffer = Buffer.from(data);

        if (buffer.length === 0) break;

        await fileHandle.write(buffer, 0, buffer.length, downloadedBytes);
        downloadedBytes += buffer.length;

        const now = Date.now();
        if (onProgress && now - lastProgressUpdate > 500) {
          lastProgressUpdate = now;
          if (totalBytes > 0) {
            const percent = (downloadedBytes / totalBytes) * 100;
            onProgress(
              `${percent.toFixed(1)}% (${formatBytes(downloadedBytes)} / ${formatBytes(totalBytes)})`,
              percent,
            );
          } else {
            onProgress(formatBytes(downloadedBytes));
          }
        }

        if (response.status === 200) break;
        if (totalBytes > 0 && downloadedBytes >= totalBytes) break;
      }
    } catch (e: any) {
      if (e?.name === "AbortError" || signal?.aborted) {
        this.logger.debug(`[DownloadOrchestrator] Download aborted for ${outputPath}`);
        return;
      }
      throw e;
    } finally {
      await fileHandle.close();
    }

    this.logger.debug(
      `[DownloadOrchestrator] Downloaded ${formatBytes(downloadedBytes)} to ${outputPath}`,
    );
  }
}
