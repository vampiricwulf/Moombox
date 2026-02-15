/**
 * Job Queue Manager
 *
 * Manages the queue of download jobs and their lifecycle.
 */

import { Database, type Job } from "../database.js";
import { ConfigManager } from "../config.js";
import { Logger } from "../logger.js";
import { WORKER_CHECK_INTERVAL_MS } from "../../constants.js";
import { getErrorMessage } from "../../types/errors.js";

/**
 * Job filter function type
 */
type JobFilter = (job: Job) => boolean;

/**
 * Job Queue Manager
 */
export class JobQueue {
  private logger: Logger;
  private activeJobs: Map<string, boolean> = new Map();
  private running: boolean = false;
  private interval: NodeJS.Timeout | null = null;
  private onJobReady?: (job: Job) => void;

  constructor() {
    this.logger = Logger.getInstance();
  }

  /**
   * Set the callback for when a job is ready to process
   */
  setJobReadyCallback(callback: (job: Job) => void): void {
    this.onJobReady = callback;
  }

  /**
   * Start the job queue monitor
   */
  start(): void {
    if (this.running) return;
    this.running = true;

    this.logger.info("[JobQueue] Started");
    this.checkQueue();
    this.interval = setInterval(
      () => this.checkQueue(),
      WORKER_CHECK_INTERVAL_MS,
    );
  }

  /**
   * Stop the job queue monitor
   */
  stop(): void {
    if (this.interval) {
      clearInterval(this.interval);
      this.interval = null;
    }
    this.running = false;
    this.logger.info("[JobQueue] Stopped");
  }

  /**
   * Check if running
   */
  isRunning(): boolean {
    return this.running;
  }

  /**
   * Check the queue for pending jobs
   */
  private async checkQueue(): Promise<void> {
    try {
      const db = await Database.getInstance();
      const jobs = await db.getJobs();

      // Filter for processable jobs
      const pendingJobs = jobs.filter(
        (j) => this.isProcessableStatus(j.status) && !this.isActive(j.id),
      );

      const config = ConfigManager.getInstance().get();
      const maxParallel = config.downloader?.num_parallel_downloads ?? 2;
      const availableSlots = maxParallel - this.getActiveCount();

      for (const job of pendingJobs) {
        if (availableSlots <= 0 || this.getActiveCount() >= maxParallel) break;
        if (this.onJobReady) {
          this.markActive(job.id);
          this.onJobReady(job);
        }
      }
    } catch (e) {
      const msg = getErrorMessage(e);
      this.logger.error(`[JobQueue] Error checking queue: ${msg}`);
    }
  }

  /**
   * Check if a job status is processable
   */
  private isProcessableStatus(status: string): boolean {
    return (
      status === "Upcoming" || status === "Live" || status === "Downloading"
    );
  }

  /**
   * Check if a job is currently active
   */
  isActive(jobId: string): boolean {
    return this.activeJobs.has(jobId);
  }

  /**
   * Mark a job as active
   */
  markActive(jobId: string): void {
    this.activeJobs.set(jobId, true);
  }

  /**
   * Mark a job as inactive
   */
  markInactive(jobId: string): void {
    this.activeJobs.delete(jobId);
  }

  /**
   * Get count of active jobs
   */
  getActiveCount(): number {
    return this.activeJobs.size;
  }

  /**
   * Get all active job IDs
   */
  getActiveJobIds(): string[] {
    return Array.from(this.activeJobs.keys());
  }
}
