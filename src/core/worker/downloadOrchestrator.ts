/**
 * Download Orchestrator
 *
 * Orchestrates the download process: manifest fetching, format selection,
 * segment downloading, and muxing.
 */

import path from "path";
import ms from "ms";
import fs from "fs-extra";
import { Database, type Job } from "../database.js";
import { ConfigManager } from "../config.js";
import { Logger } from "../logger.js";
import { YouTubeService } from "../../engine/youtube/index.js";
import { SegmentDownloader } from "../../engine/downloader.js";
import { ChatDownloader } from "../../engine/chat/index.js";
import { NotificationManager, NotificationType } from "../notifications.js";
import type { VideoInfo } from "../../types/youtube.js";
import {
  STREAM_SEGMENT_TIMEOUT_MS,
  STREAM_END_VERIFY_INTERVAL_MS,
} from "../../constants.js";
import { getErrorMessage } from "../../types/errors.js";
import type { SegmentRange } from "../../engine/manifest.js";
import type { MuxTrimOptions } from "../../engine/muxer.js";
import { downloadVod, downloadDash, downloadHls } from "./downloadStrategies.js";
import { muxAndFinalize } from "./muxFinalize.js";
import { setupChatDownloader, runSegmentDownloaders, runSegmentDownloadersWithoutChat } from "./progressTracking.js";

/**
 * Download Orchestrator
 */
export class DownloadOrchestrator {
  private logger: Logger;
  private activeSegmentDownloaders: Set<SegmentDownloader> = new Set();
  private activeChatDownloaders: Set<ChatDownloader> = new Set();
  private activeAbortControllers: Map<string, AbortController> = new Map();

  constructor() {
    this.logger = Logger.getInstance();
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

    // Build notification fields with advanced options
    const startFields: Array<{ name: string; value: string; inline?: boolean }> = [
      { name: "Channel", value: job.channelName, inline: true },
      { name: "Type", value: isVodDownload ? "VOD" : "Live Stream", inline: true },
    ];

    // Add format selection fields if specified
    if (job.selectedVideoItag != null) {
      const formatLabel = job.selectedVideoItag === -1 ? "None" : `itag ${job.selectedVideoItag}`;
      startFields.push({ name: "Video", value: formatLabel, inline: true });
    }
    if (job.selectedAudioItag != null) {
      const formatLabel = job.selectedAudioItag === -1 ? "None" : `itag ${job.selectedAudioItag}`;
      startFields.push({ name: "Audio", value: formatLabel, inline: true });
    }

    // Add time range if specified (post-download trim)
    if (job.startTime != null || job.endTime != null) {
      const startMin = Math.floor((job.startTime || 0) / 60);
      const startSec = (job.startTime || 0) % 60;
      const startStr = `${startMin}:${String(startSec).padStart(2, "0")}`;
      const endStr = job.endTime != null
        ? `${Math.floor(job.endTime / 60)}:${String(job.endTime % 60).padStart(2, "0")}`
        : "end";
      startFields.push({ name: "Post-Download Trim", value: `${startStr} - ${endStr}`, inline: false });
    }

    NotificationManager.getInstance().send(
      "Download Starting",
      `Beginning download: ${job.title}`,
      NotificationType.DOWNLOAD,
      startFields,
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
    let segmentRange: SegmentRange | null = null;
    let selectedVideoFormat: any = null;
    let selectedAudioFormat: any = null;

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
        chatDl = await setupChatDownloader(
          job,
          videoInfo,
          yt,
          stagingDir,
          db,
          this.activeChatDownloaders,
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
      const { chatPromise } = this.startChatInBackground(chatDl, "VOD");

      const vodResult = await downloadVod(job, videoInfo, yt, stagingDir, db, this.activeSegmentDownloaders, chatDl, signal);

      // Check abort immediately after download — avoid false 100% and chat timeout wait
      if (signal?.aborted) return;

      // Capture selected formats for trim bitrate info
      selectedVideoFormat = vodResult.selectedVideoFormat;
      selectedAudioFormat = vodResult.selectedAudioFormat;

      if (vodResult.hasVideo) {
        videoPath = path.join(stagingDir, "video_stream");
        audioPath = vodResult.hasAudio ? path.join(stagingDir, "audio_stream") : null;
      } else if (vodResult.hasAudio) {
        // Audio-only download: use audio as primary input for muxer
        videoPath = path.join(stagingDir, "audio_stream");
        audioPath = null;
      }

      // Set progress to 100% and wait for chat with timeout
      await this.finishVodWithChat(job.id, chatDl, chatPromise, db);
    } else if (videoInfo.dashManifestUrl) {
      // DASH: Live streams and post-live VODs
      const result = await downloadDash(
        job,
        videoInfo,
        yt,
        stagingDir,
        db,
        this.activeSegmentDownloaders,
      );
      videoDl = result.videoDl;
      audioDl = result.audioDl;
      videoPath = result.videoPath;
      audioPath = result.audioPath;
      segmentRange = result.segmentRange;
      selectedVideoFormat = result.selectedVideoFormat;
      selectedAudioFormat = result.selectedAudioFormat;
    } else if (videoInfo.hlsManifestUrl) {
      // HLS: Fallback for some streams
      const result = await downloadHls(job, videoInfo, yt, stagingDir, db, this.activeSegmentDownloaders);
      videoDl = result.videoDl;
      videoPath = result.videoPath;
    } else if (videoInfo.formats && videoInfo.formats.length > 0) {
      // Fallback: Direct format download
      const { chatPromise } = this.startChatInBackground(chatDl, "Fallback");

      const vodResult2 = await downloadVod(job, videoInfo, yt, stagingDir, db, this.activeSegmentDownloaders, chatDl, signal);

      // Check abort immediately after download
      if (signal?.aborted) return;

      // Capture selected formats for trim bitrate info
      selectedVideoFormat = vodResult2.selectedVideoFormat;
      selectedAudioFormat = vodResult2.selectedAudioFormat;

      videoPath = path.join(stagingDir, "video_stream");
      audioPath = vodResult2.hasAudio ? path.join(stagingDir, "audio_stream") : null;

      // Set progress to 100% and wait for chat with timeout
      await this.finishVodWithChat(job.id, chatDl, chatPromise, db);
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
        await runSegmentDownloaders(job, videoDl, audioDl, chatDl, db, this.activeSegmentDownloaders);
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

    // Mux the streams (always download full file, no trimming during download)
    await muxAndFinalize(
      job,
      videoPath,
      audioPath,
      outputDir,
      stagingDir,
      finalExtension,
      db,
      signal,
      undefined, // No trim options - download full file
    );

    // Post-download trim: If timestamps were specified, create a trimmed version
    if ((job.startTime != null || job.endTime != null) && !signal?.aborted) {
      this.logger.info(
        `[DownloadOrchestrator] Creating post-download trim: ${job.startTime || 0}s - ${job.endTime ? job.endTime + "s" : "end"}`
      );

      try {
        // Import TrimService (dynamic to avoid circular dependency)
        const { TrimService } = await import("./trimService.js");

        // Flush database to ensure "Finished" status is written
        await db.flush();

        // Get the finished job with updated metadata
        const finishedJob = (await db.getJobs()).find((j) => j.id === job.id);
        if (!finishedJob || finishedJob.status !== "Finished") {
          this.logger.warn(
            `[DownloadOrchestrator] Cannot create trim: job not in Finished state (${finishedJob?.status})`
          );
        } else {
          // Create trim with the specified time range
          const trimRecord = await TrimService.createTrim(
            finishedJob,
            job.startTime || 0,
            job.endTime || finishedJob.lengthSeconds || 0,
            signal,
          );

          this.logger.info(
            `[DownloadOrchestrator] Post-download trim created: ${trimRecord.filename}`
          );
        }
      } catch (e: any) {
        // Don't fail the entire job if trim fails - the full file is still available
        this.logger.error(
          `[DownloadOrchestrator] Failed to create post-download trim: ${e.message}`
        );
        // Optionally send a notification about trim failure
        NotificationManager.getInstance().send(
          "Trim Failed",
          `Failed to create trim for "${job.title}": ${e.message}`,
          NotificationType.ERROR,
          [],
          {
            url: `https://www.youtube.com/watch?v=${job.videoId}`,
            event: "trim_error",
          },
        );
      }
    }
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

  private static readonly CHAT_TIMEOUT_MS = ms("2m");

  /**
   * Start a chat downloader in the background, returning a tracked promise.
   */
  private startChatInBackground(chatDl: ChatDownloader | null, label: string) {
    let chatFinished = false;
    const chatPromise = chatDl
      ? chatDl.start().then(() => { chatFinished = true; }).catch((e) => {
          chatFinished = true;
          this.logger.warn(`[Chat] ${label} chat download failed: ${e.message}`);
        })
      : Promise.resolve();
    return { chatPromise, isChatFinished: () => chatFinished };
  }

  /**
   * Set 100% progress and wait for chat to finish with a timeout.
   * Used after VOD/fallback downloads complete.
   */
  private async finishVodWithChat(
    jobId: string,
    chatDl: ChatDownloader | null,
    chatPromise: Promise<void>,
    db: Database,
  ): Promise<void> {
    const chatCount = chatDl ? chatDl.getMessageCount() : 0;
    const chatSuffix = chatCount > 0 ? ` C:${chatCount}` : "";
    await db.updateJob(jobId, { progress: `V:100% A:100%${chatSuffix}`, percent: 100 });

    // Wait for chat to finish (with timeout so we don't block muxing forever)
    const timeoutMs = DownloadOrchestrator.CHAT_TIMEOUT_MS;
    let timer: ReturnType<typeof setTimeout>;
    const chatTimeout = new Promise<void>((resolve) => {
      timer = setTimeout(() => {
        if (chatDl && chatDl.isRunning()) {
          this.logger.warn(
            `[Chat] Chat download timed out after ${timeoutMs / 1000}s, proceeding to mux`,
          );
          chatDl.stop();
        }
        resolve();
      }, timeoutMs);
    });
    await Promise.race([chatPromise, chatTimeout]);
    clearTimeout(timer!);
  }

  /**
   * Cancellable delay — resolves after `delayMs` or rejects immediately if signal fires.
   */
  private cancellableDelay(delayMs: number, signal?: AbortSignal): Promise<void> {
    if (signal?.aborted) return Promise.reject(new DOMException("Aborted", "AbortError"));
    return new Promise((resolve, reject) => {
      const onAbort = () => {
        clearTimeout(timer);
        reject(new DOMException("Aborted", "AbortError"));
      };
      const timer = setTimeout(() => {
        signal?.removeEventListener("abort", onAbort);
        resolve();
      }, delayMs);
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
    let consecutiveLiveChecks = 0; // How many times YouTube said "live" with no new segments
    const MAX_LIVE_CHECKS = 6; // Force-end after 6 checks (~30min with 5min intervals)

    // Track segment activity
    const onSegmentProgress = () => {
      lastSegmentTime = Date.now();
      consecutiveLiveChecks = 0; // Reset when new segments arrive
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
      await runSegmentDownloadersWithoutChat(job, videoDl, audioDl, chatDl, db, this.activeSegmentDownloaders);

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
          consecutiveLiveChecks++;

          // Force-end if YouTube keeps saying "live" but no segments are arriving
          if (consecutiveLiveChecks >= MAX_LIVE_CHECKS) {
            this.logger.warn(
              `[DownloadOrchestrator] YouTube reported "live" ${consecutiveLiveChecks} times ` +
                `with no new segments. Forcing stream end.`,
            );
            streamConfirmedEnded = true;
            break;
          }

          // Stream is still live - wait and try again
          this.logger.info(
            `[DownloadOrchestrator] Stream still reports as live (check ${consecutiveLiveChecks}/${MAX_LIVE_CHECKS}). ` +
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
              const refreshedResult = await downloadHls(
                job,
                freshInfo,
                yt,
                stagingDir,
                db,
                this.activeSegmentDownloaders,
              );
              videoDl = refreshedResult.videoDl;
              audioDl = null;
            } else {
              const refreshedResult = await downloadDash(
                job,
                freshInfo,
                yt,
                stagingDir,
                db,
                this.activeSegmentDownloaders,
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
}
