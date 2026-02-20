/**
 * Job Queue Manager
 *
 * Manages the queue of download jobs and their lifecycle.
 * Uses p-queue for priority-based job scheduling and concurrency control.
 */

import PQueue from "p-queue";
import { Database, type Job } from "../database.js";
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
  private queue: PQueue;
  private activeJobs: Map<string, boolean> = new Map();
  private queuedJobs: Set<string> = new Set(); // Track jobs already in p-queue
  private running: boolean = false;
  private interval: NodeJS.Timeout | null = null;
  private onJobReady?: (job: Job) => Promise<void>;

  constructor() {
    this.logger = Logger.getInstance();

    // Create p-queue for job lifecycle management.
    // Concurrency is generous — actual download throughput is controlled
    // by DownloadWorker's download slot semaphore (num_parallel_downloads).
    this.queue = new PQueue({
      concurrency: 100,
      autoStart: true,
      interval: 1_000, // Rate limit: max N job starts per second
      intervalCap: 10, // Max 10 job starts per second
    });
  }

  /**
   * Set the callback for when a job is ready to process
   */
  setJobReadyCallback(callback: (job: Job) => Promise<void>): void {
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
    this.queue.pause();
    this.running = false;
    this.logger.info("[JobQueue] Stopped");
  }

  /**
   * Pause job processing (jobs stay in queue)
   */
  pause(): void {
    this.queue.pause();
    this.logger.info("[JobQueue] Paused");
  }

  /**
   * Resume job processing
   */
  resume(): void {
    this.queue.start();
    this.logger.info("[JobQueue] Resumed");
  }

  /**
   * Wait for all queued jobs to complete
   */
  async waitUntilIdle(): Promise<void> {
    await this.queue.onIdle();
  }

  /**
   * Check if running
   */
  isRunning(): boolean {
    return this.running;
  }

  /**
   * Check the queue for pending jobs and add them to p-queue
   */
  private async checkQueue(): Promise<void> {
    try {
      const db = await Database.getInstance();
      const jobs = await db.getJobs();

      // Filter for processable jobs not already queued or active
      const pendingJobs = jobs.filter(
        (j) =>
          this.isProcessableStatus(j.status) &&
          !this.isActive(j.id) &&
          !this.queuedJobs.has(j.id),
      );

      // Add jobs to p-queue with priority
      for (const job of pendingJobs) {
        // Limit queue backlog to prevent memory issues
        if (this.queue.size >= 100) {
          this.logger.debug(
            `[JobQueue] Queue backlog limit reached (${this.queue.size} jobs), skipping new additions`,
          );
          break;
        }

        await this.addJobToQueue(job);
      }
    } catch (e) {
      const msg = getErrorMessage(e);
      this.logger.error(`[JobQueue] Error checking queue: ${msg}`);
    }
  }

  /**
   * Add a job to the p-queue with priority
   */
  private async addJobToQueue(job: Job): Promise<void> {
    if (!this.onJobReady) return;

    const priority = this.calculatePriority(job);
    this.queuedJobs.add(job.id);

    await this.queue.add(
      async () => {
        this.markActive(job.id);
        this.queuedJobs.delete(job.id);

        try {
          await this.onJobReady!(job);
        } catch (e) {
          const msg = getErrorMessage(e);
          this.logger.error(`[JobQueue] Job ${job.id} failed: ${msg}`);
        } finally {
          this.markInactive(job.id);
        }
      },
      { priority },
    );
  }

  /**
   * Calculate job priority for p-queue
   * Higher number = higher priority
   */
  private calculatePriority(job: Job): number {
    // Live streams = highest priority (1)
    // These are actively streaming and need immediate processing
    if (job.status === "Live") return 1;

    // Upcoming streams = medium priority (0)
    // These need monitoring but aren't urgent yet
    if (job.status === "Upcoming") return 0;

    // Downloading = normal priority (0)
    // Already in progress, continue at normal priority
    if (job.status === "Downloading") return 0;

    // Error status = lower priority (-1)
    // Give precedence to fresh jobs over retries
    if (job.error) return -1;

    return 0;
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
