/**
 * Download Worker
 *
 * Main entry point for job processing. Coordinates the job queue,
 * stream processing, and download orchestration.
 */

import { Database, type Job } from "../database.js";
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

  private constructor() {
    this.logger = Logger.getInstance();
    this.jobQueue = new JobQueue();
    this.streamProcessor = new StreamProcessor();
    this.downloadOrchestrator = new DownloadOrchestrator();

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
    this.logger.info("Download Worker stopped.");
  }

  /**
   * Check if running
   */
  isRunning(): boolean {
    return this.running;
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
          url: `https://www.youtube.com/watch?v=${job.videoId}`,
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

    this.logger.info(`[Worker] Processing job ${job.title} (${job.videoId})`);

    // Step 1: Process stream status and get video info
    const result = await this.streamProcessor.process(job);

    // Check for errors
    if (result.error) {
      result.chatDownloader?.stop();
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
            url: `https://www.youtube.com/watch?v=${job.videoId}`,
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

      // Other errors
      await db.updateJob(job.id, {
        status: "Error",
        error: result.error,
      });
      return;
    }

    // Check if we should proceed with download
    if (!result.shouldDownload) {
      result.chatDownloader?.stop();
      return;
    }

    if (!this.running) {
      result.chatDownloader?.stop();
      return;
    }

    // Check if job was cancelled during stream processing
    const currentJob = (await db.getJobs()).find((j) => j.id === job.id);
    if (!currentJob || currentJob.status === "Cancelled") {
      result.chatDownloader?.stop();
      this.logger.info(`[Worker] Job ${job.id} was cancelled, skipping download`);
      return;
    }

    // Step 2: Execute the download
    await this.downloadOrchestrator.execute(job, result.videoInfo, result.isVod, result.chatDownloader);
  }
}

// Default export for convenience
export default DownloadWorker;
