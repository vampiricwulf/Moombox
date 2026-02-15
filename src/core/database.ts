import { Low } from "lowdb";
import { JSONFile } from "lowdb/node";
import path from "path";
import fs from "fs-extra";
import { ConfigManager } from "./config.js";
import { Logger } from "./logger.js";
import type { Job, JobUpdate } from "../types/jobs.js";

// Re-export Job type for backwards compatibility
export type { Job, JobUpdate } from "../types/jobs.js";

interface Data {
  jobs: Job[];
  history: string[]; // List of processed video IDs
  lastVideos?: Record<string, string>; // channelId → most recent videoId (for DECAPI fallback)
}

type JobUpdateListener = (job: Job) => void;
type JobsChangeListener = (jobs: Job[]) => void;

export class Database {
  private static instance: Database;
  private static initPromise: Promise<Database> | null = null;
  private db: Low<Data>;
  private historySet: Set<string> = new Set();
  private jobsMap: Map<string, Job> = new Map(); // O(1) lookup index
  private jobUpdateListeners: JobUpdateListener[] = [];
  private jobsChangeListeners: JobsChangeListener[] = [];
  private writeQueue: Promise<void> = Promise.resolve();

  private constructor() {
    let file = path.join(process.cwd(), "moombox.json");
    try {
      const config = ConfigManager.getInstance().get();
      if (config.database_path) {
        file = config.database_path;
      }
    } catch (e) {
      // Config might not be loaded yet if testing separately, fallback to default
    }

    const adapter = new JSONFile<Data>(file);
    this.db = new Low(adapter, { jobs: [], history: [] });
  }

  static async getInstance(): Promise<Database> {
    if (!Database.initPromise) {
      Database.initPromise = (async () => {
        const db = new Database();
        await db.init();
        Database.instance = db;
        return db;
      })();
    }
    return Database.initPromise;
  }

  private async init() {
    try {
      await this.db.read();
    } catch (e) {
      // Database file is corrupted — back it up and start fresh
      const logger = Logger.getInstance();
      const msg = e instanceof Error ? e.message : String(e);
      logger.error(`[Database] Failed to read database: ${msg}`);

      let file = path.join(process.cwd(), "moombox.json");
      try {
        const config = ConfigManager.getInstance().get();
        if (config.database_path) file = config.database_path;
      } catch {}

      const backupPath = `${file}.corrupt.${Date.now()}`;
      try {
        await fs.copy(file, backupPath);
        logger.warn(`[Database] Backed up corrupt database to ${backupPath}`);
      } catch {}
    }
    // Initialize with defaults if data was missing or corrupt
    if (!this.db.data) {
      this.db.data = { jobs: [], history: [] };
      await this.writeWithPermissions();
    }
    // Validate data structure
    if (!Array.isArray(this.db.data.jobs)) {
      Logger.getInstance().warn("[Database] jobs is not an array, resetting");
      this.db.data.jobs = [];
    }
    if (!Array.isArray(this.db.data.history)) {
      Logger.getInstance().warn("[Database] history is not an array, resetting");
      this.db.data.history = [];
    }
    // Build history Set for O(1) lookups
    this.historySet = new Set(this.db.data.history);
    // Build jobs Map for O(1) lookups
    this.jobsMap.clear();
    for (const job of this.db.data.jobs) {
      this.jobsMap.set(job.id, job);
    }
  }

  /**
   * Write database to disk and set restrictive permissions
   */
  private async writeWithPermissions(): Promise<void> {
    await this.writeWithPermissions();
    // Set owner-only permissions (0o600) for security
    const filename = (this.db.adapter as any).filename;
    if (filename) {
      await fs.chmod(filename, 0o600);
    }
  }

  /**
   * Serialize a mutation through the write queue.
   * Ensures only one read-modify-write cycle runs at a time,
   * preventing concurrent callers from overwriting each other's changes.
   */
  private enqueueMutation<T>(fn: () => Promise<T>): Promise<T> {
    const result = this.writeQueue.then(fn);
    // Keep the chain going regardless of success/failure (log errors for visibility)
    this.writeQueue = result.then(() => {}, (e) => {
      Logger.getInstance().error(`[Database] Write failed: ${e instanceof Error ? e.message : String(e)}`);
    });
    return result;
  }

  async addJob(
    job: Omit<
      Job,
      | "id"
      | "createdAt"
      | "updatedAt"
      | "status"
      | "percent"
      | "progress"
      | "eta"
      | "speed"
    >,
  ): Promise<Job | null> {
    return this.enqueueMutation(async () => {
      // Use videoId as the job ID to prevent duplicate downloads
      const jobId = job.videoId;

      // Check if job already exists (O(1) lookup)
      if (this.jobsMap.has(jobId)) {
        // Job already exists, don't add duplicate
        return null;
      }

      const newJob: Job = {
        ...job,
        id: jobId,
        status: "Upcoming", // Default
        percent: 0,
        progress: "",
        eta: "",
        speed: "",
        createdAt: new Date().toISOString(),
        updatedAt: new Date().toISOString(),
      };
      this.db.data.jobs.push(newJob);
      this.jobsMap.set(newJob.id, newJob); // Update index
      await this.writeWithPermissions();
      this.notifyJobsChange();
      return newJob;
    });
  }

  async getJob(id: string): Promise<Job | undefined> {
    const job = this.jobsMap.get(id);
    return job ? { ...job } : undefined;
  }

  async jobExists(videoId: string): Promise<boolean> {
    return this.db.data.jobs.some((j) => j.id === videoId);
  }

  async updateJob(id: string, updates: Partial<Job>) {
    return this.enqueueMutation(async () => {
      const job = this.jobsMap.get(id); // O(1) lookup
      if (job) {
        Object.assign(job, { ...updates, updatedAt: new Date().toISOString() });
        // Map points to the same object reference, so it's automatically updated
        await this.writeWithPermissions();
        // Notify listeners of the update
        this.notifyJobUpdate(job);
      }
    });
  }

  /**
   * Subscribe to individual job updates (for real-time progress)
   */
  onJobUpdate(listener: JobUpdateListener): () => void {
    this.jobUpdateListeners.push(listener);
    return () => {
      const idx = this.jobUpdateListeners.indexOf(listener);
      if (idx !== -1) this.jobUpdateListeners.splice(idx, 1);
    };
  }

  /**
   * Subscribe to jobs list changes (add/delete)
   */
  onJobsChange(listener: JobsChangeListener): () => void {
    this.jobsChangeListeners.push(listener);
    return () => {
      const idx = this.jobsChangeListeners.indexOf(listener);
      if (idx !== -1) this.jobsChangeListeners.splice(idx, 1);
    };
  }

  private notifyJobUpdate(job: Job) {
    for (const listener of this.jobUpdateListeners) {
      try {
        listener(job);
      } catch {
        // Ignore listener errors
      }
    }
  }

  private notifyJobsChange() {
    const jobs = this.db.data.jobs;
    for (const listener of this.jobsChangeListeners) {
      try {
        listener(jobs);
      } catch {
        // Ignore listener errors
      }
    }
  }

  async getJobs(): Promise<Job[]> {
    return this.db.data.jobs.map((j) => ({ ...j }));
  }

  private static readonly MAX_HISTORY_SIZE = 10000;

  async addToHistory(videoId: string) {
    return this.enqueueMutation(async () => {
      if (!this.historySet.has(videoId)) {
        this.historySet.add(videoId);
        this.db.data.history.push(videoId);
        // Prune oldest entries if history grows too large
        if (this.db.data.history.length > Database.MAX_HISTORY_SIZE) {
          const pruned = this.db.data.history.slice(-Database.MAX_HISTORY_SIZE);
          this.db.data.history = pruned;
          this.historySet = new Set(pruned);
        }
        await this.writeWithPermissions();
      }
    });
  }

  async hasProcessed(videoId: string): Promise<boolean> {
    return this.historySet.has(videoId);
  }

  async deleteJob(id: string): Promise<boolean> {
    return this.enqueueMutation(async () => {
      const idx = this.db.data.jobs.findIndex((j) => j.id === id);
      if (idx !== -1) {
        this.db.data.jobs.splice(idx, 1);
        this.jobsMap.delete(id); // Remove from index
        await this.writeWithPermissions();
        this.notifyJobsChange();
        return true;
      }
      return false;
    });
  }

  async getLastVideo(channelId: string): Promise<string | undefined> {
    return this.db.data.lastVideos?.[channelId];
  }

  async setLastVideo(channelId: string, videoId: string): Promise<void> {
    return this.enqueueMutation(async () => {
      if (!this.db.data.lastVideos) {
        this.db.data.lastVideos = {};
      }
      this.db.data.lastVideos[channelId] = videoId;
      await this.writeWithPermissions();
    });
  }

  async read(): Promise<void> {
    await this.db.read();
  }
}
