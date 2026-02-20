/**
 * Download Worker
 *
 * Main entry point for job processing. Coordinates the job queue,
 * stream processing, and download orchestration.
 */

import { Database, type Job } from "../database.js";
import { ConfigManager } from "../config.js";
import { Logger } from "../logger.js";
import { NotificationManager, NotificationType } from "../notifications.js";
import { AutoCookieService } from "../autoCookies.js";
import { getErrorMessage } from "../../types/errors.js";
import { JobQueue } from "./jobQueue.js";
import { StreamProcessor } from "./streamProcessor.js";
import { DownloadOrchestrator } from "./downloadOrchestrator.js";

// Re-export sub-modules
export { JobQueue } from "./jobQueue.js";
export { StreamProcessor } from "./streamProcessor.js";
export { DownloadOrchestrator } from "./downloadOrchestrator.js";
export { AssetDownloader } from "./assetDownloader.js";

/**
 * Download Worker - Main job processing service
 */
export class DownloadWorker {
  private static instance: DownloadWorker;

  private logger: Logger;
  private jobQueue: JobQueue;
  private streamProcessor: StreamProcessor;
  private downloadOrchestrator: DownloadOrchestrator;
  private running: boolean = false;

  // Download slot semaphore — limits concurrent active downloads
  // (separate from job processing concurrency in p-queue)
  private maxDownloadSlots: number;
  private activeDownloads: number = 0;
  private downloadWaiters: Array<() => void> = [];

  private constructor() {
    this.logger = Logger.getInstance();
    this.jobQueue = new JobQueue();
    this.streamProcessor = new StreamProcessor();
    this.downloadOrchestrator = new DownloadOrchestrator();

    const config = ConfigManager.getInstance().get();
    this.maxDownloadSlots = config.downloader?.num_parallel_downloads ?? 2;

    // Setup job ready callback
    this.jobQueue.setJobReadyCallback((job) => this.startJob(job));
  }

  /**
   * Get the singleton instance
   */
  static getInstance(): DownloadWorker {
    if (!DownloadWorker.instance) {
      DownloadWorker.instance = new DownloadWorker();
    }
    return DownloadWorker.instance;
  }

  /**
   * Start the worker
   */
  start(): void {
    if (this.running) return;
    this.running = true;
    this.logger.info("Download Worker started.");
    this.jobQueue.start();
  }

  /**
   * Stop the worker
   */
  stop(): void {
    this.running = false;
    this.jobQueue.stop();
    this.streamProcessor.stop();
    this.downloadOrchestrator.stop();
    // Resolve all pending download slot waiters so they can exit
    const waiters = this.downloadWaiters.splice(0);
    for (const resolve of waiters) resolve();
    this.logger.info("Download Worker stopped.");
  }

  /**
   * Check if running
   */
  isRunning(): boolean {
    return this.running;
  }

  /**
   * Update the maximum number of concurrent download slots at runtime.
   * Existing downloads continue; the new limit applies to future slot acquisitions.
   */
  setMaxDownloadSlots(n: number): void {
    if (n >= 1) {
      this.maxDownloadSlots = n;
      this.logger.info(`[Worker] Download slots updated to ${n}`);
      // Wake up waiters if we now have spare capacity
      while (this.activeDownloads < this.maxDownloadSlots && this.downloadWaiters.length > 0) {
        this.downloadWaiters.shift()!();
      }
    }
  }

  /**
   * Acquire a download slot. Resolves immediately if a slot is free,
   * otherwise waits until one is released.
   */
  private acquireDownloadSlot(): Promise<void> {
    if (this.activeDownloads < this.maxDownloadSlots) {
      this.activeDownloads++;
      return Promise.resolve();
    }
    return new Promise(resolve => {
      this.downloadWaiters.push(() => {
        this.activeDownloads++;
        resolve();
      });
    });
  }

  /**
   * Release a download slot, unblocking the next waiter if any.
   */
  private releaseDownloadSlot(): void {
    this.activeDownloads--;
    if (this.downloadWaiters.length > 0) {
      this.downloadWaiters.shift()!();
    }
  }

  /**
   * Start processing a job
   */
  private async startJob(job: Job): Promise<void> {
    try {
      await this.processJob(job);
    } catch (e) {
      const msg = getErrorMessage(e);
      this.logger.error(`[Worker] Job ${job.id} failed: ${msg}`);

      try {
        const db = await Database.getInstance();
        await db.updateJob(job.id, { status: "Error", error: msg });
      } catch (dbErr) {
        this.logger.error(`[Worker] Failed to update job error status: ${getErrorMessage(dbErr)}`);
      }

      NotificationManager.getInstance().send(
        "Job Failed",
        `Job failed for: ${job.title}`,
        NotificationType.ERROR,
        [
          { name: "Error", value: msg },
          { name: "Channel", value: job.channelName, inline: true },
          { name: "Video ID", value: job.videoId, inline: true },
        ],
        {
          url: job.url || `https://www.youtube.com/watch?v=${job.videoId}`,
          thumbnail: job.thumbnailUrl,
          event: "error",
        },
      );
    } finally {
      this.jobQueue.markInactive(job.id);
    }
  }

  /**
   * Process a single job through the full pipeline
   */
  private async processJob(job: Job): Promise<void> {
    const db = await Database.getInstance();

    // Guard: re-read job from DB to ensure it's still in a processable state.
    // The job object may be stale if checkQueue's snapshot was taken long ago.
    const freshJob = await db.getJob(job.id);
    if (!freshJob) return;
    const terminal = ["Finished", "Error", "Cancelled", "Muxing"];
    if (terminal.includes(freshJob.status)) {
      this.logger.debug(`[Worker] Skipping job ${job.id} — already ${freshJob.status}`);
      return;
    }

    this.logger.info(`[Worker] Processing job ${job.title} (${job.videoId})`);

    // Step 1: Process stream status and get video info
    const result = await this.streamProcessor.process(job);

    // Helper: stop Twitch chat and reset chatStatus if it was started
    const stopTwitchChat = async () => {
      if (result.twitchChatDownloader) {
        result.twitchChatDownloader.stop();
        await db.updateJob(job.id, { chatStatus: "unavailable" }).catch(() => {});
      }
    };

    // Check for errors
    if (result.error) {
      result.chatDownloader?.stop();
      await stopTwitchChat();
      const error = result.videoInfo.playabilityError;

      if (error === "members_only" || error === "login_required") {
        this.logger.warn(
          `[Worker] ${job.videoId} requires authentication: ${result.error}`,
        );
        await db.updateJob(job.id, {
          status: "COOKIES?",
          error: result.error,
        });

        NotificationManager.getInstance().send(
          "Authentication Required",
          `Cookies needed for: ${job.title}`,
          NotificationType.WARNING,
          [
            { name: "Reason", value: result.error || "Members-only content" },
            { name: "Channel", value: job.channelName, inline: true },
            { name: "Video ID", value: job.videoId, inline: true },
          ],
          {
            url: job.url || `https://www.youtube.com/watch?v=${job.videoId}`,
            thumbnail: job.thumbnailUrl,
            event: "auth",
          },
        );

        // Attempt automatic cookie refresh if configured
        const autoCookies = AutoCookieService.getInstance();
        if (autoCookies.isConfigured()) {
          this.logger.info("[Worker] Attempting automatic cookie refresh...");
          const refreshed = await autoCookies.refreshCookies();
          if (refreshed) {
            this.logger.info("[Worker] Cookie refresh succeeded, retrying job");
            await db.updateJob(job.id, { status: "Live", error: undefined });
            return;
          }
          this.logger.warn("[Worker] Auto cookie refresh failed — re-run setup from Settings");
        }

        return;
      }

      if (error === "age_restricted") {
        await db.updateJob(job.id, {
          status: "Error",
          error: result.error,
        });
        return;
      }

      // Other errors (including Twitch errors)
      await db.updateJob(job.id, {
        status: "Error",
        error: result.error,
      });
      return;
    }

    // Check if we should proceed with download
    if (!result.shouldDownload) {
      result.chatDownloader?.stop();
      await stopTwitchChat();
      return;
    }

    if (!this.running) {
      result.chatDownloader?.stop();
      await stopTwitchChat();
      return;
    }

    // Check if job was cancelled during stream processing
    const currentJob = await db.getJob(job.id);
    if (!currentJob || currentJob.status === "Cancelled") {
      result.chatDownloader?.stop();
      await stopTwitchChat();
      this.logger.info(`[Worker] Job ${job.id} was cancelled, skipping download`);
      return;
    }

    // Step 2: Acquire a download slot (num_parallel_downloads concurrency gate)
    // Stream processing above runs freely; only active downloads are limited.
    await this.acquireDownloadSlot();
    let slotReleased = false;
    const releaseSlot = () => {
      if (!slotReleased) {
        slotReleased = true;
        this.releaseDownloadSlot();
      }
    };

    try {
      // Step 3: Execute the download (slot released via callback before muxing)
      if (job.platform === "twitch" && result.twitchVariant) {
        await this.downloadOrchestrator.executeTwitch(
          job,
          result.twitchVariant,
          result.isVod,
          result.twitchChatDownloader,
          result.twitchStreamInfo,
          releaseSlot,
        );
      } else {
        await this.downloadOrchestrator.execute(job, result.videoInfo, result.isVod, result.chatDownloader, releaseSlot);
      }
    } finally {
      releaseSlot(); // Ensure slot is released on any exit path
    }
  }
}

// Default export for convenience
export default DownloadWorker;
