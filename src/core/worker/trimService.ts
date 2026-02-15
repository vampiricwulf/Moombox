/**
 * Trim service for creating derivative trimmed versions of finished videos
 */

import path from "path";
import ms from "ms";
import fs from "fs-extra";
import { execa } from "execa";
import { Database, type Job } from "../database.js";
import { ConfigManager } from "../config.js";
import { Logger } from "../logger.js";
import { Muxer } from "../../engine/muxer.js";
import { NotificationManager, NotificationType } from "../notifications.js";
import type { TrimRecord } from "../../types/jobs.js";

export class TrimService {
  private static readonly activeTrimOps = new Set<string>();

  /**
   * Create a trimmed version of a finished job
   */
  static async createTrim(
    sourceJob: Job,
    startTime: number,
    endTime: number,
    signal?: AbortSignal,
  ): Promise<TrimRecord> {
    const logger = Logger.getInstance();
    const db = await Database.getInstance();

    // 1. Validate source job (must be Finished with file)
    if (sourceJob.status !== "Finished") {
      throw new Error("Can only trim finished jobs");
    }
    if (!sourceJob.filename) {
      throw new Error("Job has no output file");
    }

    // Get output directory from job or fall back to config default
    const config = ConfigManager.getInstance().get();
    const outputDirectory = sourceJob.outputDirectory || config.downloader?.output_directory || "./output";

    // 2. Validate time range
    this.validateTrimRange(startTime, endTime, sourceJob.lengthSeconds);

    // 3. Check for concurrent operations (lock via activeTrimOps Set)
    const lockKey = sourceJob.id;
    if (this.activeTrimOps.has(lockKey)) {
      throw new Error("Another trim operation is in progress for this job");
    }

    this.activeTrimOps.add(lockKey);

    try {
      // 4. Verify source file exists
      const sourceFilePath = path.join(
        outputDirectory,
        sourceJob.filename,
      );
      if (!(await fs.pathExists(sourceFilePath))) {
        throw new Error(`Source file not found: ${sourceFilePath}`);
      }

      // 5. Generate trim filename with time suffix
      // Store in trim/ subdirectory under the same channel/directory as source
      const trimFilename = this.generateTrimFilename(
        sourceJob.videoId,
        startTime,
        endTime,
      );
      // Extract parent directory from source filename (e.g., "Channel Name" from "Channel Name/video.mp4")
      const sourceParentDir = path.dirname(sourceJob.filename);
      const trimRelativePath = path.join(sourceParentDir, "trim", trimFilename);

      // 6. Check if trim already exists (throw if duplicate)
      if (sourceJob.trims?.some((t) => t.filename === trimRelativePath)) {
        throw new Error(
          `A trim with this time range already exists: ${trimFilename}`,
        );
      }

      const trimOutputPath = path.join(
        outputDirectory,
        trimRelativePath,
      );

      // Ensure trim directory exists
      await fs.ensureDir(path.dirname(trimOutputPath));

      if (await fs.pathExists(trimOutputPath)) {
        throw new Error(`Trim file already exists: ${trimFilename}`);
      }

      // 7. Estimate size (disk space check skipped - not critical)
      const duration = endTime - startTime;
      const estimatedSize = sourceJob.fileSize
        ? (sourceJob.fileSize / (sourceJob.lengthSeconds || 1)) * duration
        : 0;

      logger.info(
        `[TrimService] Creating trim: ${startTime}s - ${endTime}s (${duration}s) -> ${trimFilename}`,
      );

      // 8. Execute FFmpeg trim with re-encode (Muxer.mux)
      await Muxer.mux(
        sourceFilePath, // Already-muxed video file
        null, // No separate audio (single file)
        trimOutputPath,
        signal,
        {
          trimStartOffset: startTime,
          trimDuration: duration,
          videoBitrate: sourceJob.videoWidth ? 5000 : undefined, // kbps (estimate)
          audioBitrate: 128, // kbps (standard)
          usePreciseTrim: true, // Force re-encode for accuracy
        },
      );

      // 9. Extract metadata (duration, size) via ffprobe
      const metadata = await this.extractVideoMetadata(trimOutputPath);

      if (!metadata) {
        logger.warn(
          `[TrimService] Could not extract metadata for trim ${trimFilename}`,
        );
      }

      // 10. Create TrimRecord and update parent job
      const trimRecord: TrimRecord = {
        id: `trim_${Date.now()}`,
        startTime,
        endTime,
        filename: trimRelativePath, // Store relative path including trim/ subdirectory
        createdAt: new Date().toISOString(),
        duration: metadata?.duration || duration,
        fileSize: metadata?.size,
      };

      const updatedTrims = [...(sourceJob.trims || []), trimRecord];
      await db.updateJob(sourceJob.id, { trims: updatedTrims });

      // Flush batch immediately so frontend can fetch updated job data
      await db.flush();

      logger.info(
        `[TrimService] Trim created successfully: ${trimFilename} (${this.formatBytes(metadata?.size || 0)})`,
      );

      // Send notification
      NotificationManager.getInstance().send(
        "Trim Created",
        `Created ${this.formatDuration(trimRecord.duration)} trim from "${sourceJob.title}"`,
        NotificationType.INFO,
        [
          { name: "Source Video", value: sourceJob.title, inline: false },
          {
            name: "Time Range",
            value: `${this.formatTime(trimRecord.startTime)} - ${this.formatTime(trimRecord.endTime)}`,
            inline: true,
          },
          {
            name: "Duration",
            value: this.formatDuration(trimRecord.duration),
            inline: true,
          },
          {
            name: "File Size",
            value: this.formatBytes(trimRecord.fileSize || 0),
            inline: true,
          },
        ],
        {
          url: `https://www.youtube.com/watch?v=${sourceJob.videoId}`,
          thumbnail: sourceJob.thumbnailUrl,
          event: "trim_created",
        },
      );

      // 11. Return TrimRecord
      return trimRecord;
    } finally {
      this.activeTrimOps.delete(lockKey);
    }
  }

  /**
   * Delete a trim file and remove from job record
   */
  static async deleteTrim(jobId: string, trimId: string): Promise<void> {
    const logger = Logger.getInstance();
    const db = await Database.getInstance();

    // 1. Load job from database
    const job = await db.getJob(jobId);
    if (!job) {
      throw new Error("Job not found");
    }

    // 2. Find trim in job.trims array
    const trimIndex = job.trims?.findIndex((t) => t.id === trimId);
    if (trimIndex === undefined || trimIndex === -1 || !job.trims) {
      throw new Error("Trim not found");
    }

    const trim = job.trims[trimIndex];

    // 3. Delete file from disk (gracefully handle missing files)
    const config = ConfigManager.getInstance().get();
    const outputDirectory = job.outputDirectory || config.downloader?.output_directory || "./output";
    const trimPath = path.join(outputDirectory, trim.filename);
    try {
      if (await fs.pathExists(trimPath)) {
        await fs.remove(trimPath);
        logger.info(`[TrimService] Deleted trim file: ${trim.filename}`);
      } else {
        logger.warn(
          `[TrimService] Trim file not found (already deleted?): ${trim.filename}`,
        );
      }
    } catch (e: any) {
      logger.error(
        `[TrimService] Failed to delete trim file: ${e.message}`,
      );
      // Continue anyway to clean up database record
    }

    // 4. Remove trim record from job.trims array
    const updatedTrims = job.trims.filter((t) => t.id !== trimId);

    // 5. Update job in database
    await db.updateJob(jobId, { trims: updatedTrims });

    // Flush batch immediately so frontend can fetch updated job data
    await db.flush();

    logger.info(`[TrimService] Trim deleted: ${trim.filename}`);

    // Send notification
    NotificationManager.getInstance().send(
      "Trim Deleted",
      `Deleted trim from "${job.title}"`,
      NotificationType.INFO,
      [
        { name: "Source Video", value: job.title, inline: false },
        {
          name: "Time Range",
          value: `${this.formatTime(trim.startTime)} - ${this.formatTime(trim.endTime)}`,
          inline: true,
        },
        {
          name: "Duration",
          value: this.formatDuration(trim.duration),
          inline: true,
        },
      ],
      {
        event: "trim_deleted",
      },
    );
  }

  /**
   * Generate trim filename with time-range suffix
   */
  private static generateTrimFilename(
    videoId: string,
    startTime: number,
    endTime: number,
  ): string {
    // Simple format: "{videoId} [30s-60s].mp4"
    // Stored in trim/ subdirectory
    return `${videoId} [${startTime}s-${endTime}s].mp4`;
  }

  /**
   * Validate trim parameters
   */
  private static validateTrimRange(
    start: number,
    end: number,
    maxDuration?: number,
  ): void {
    if (start < 0) {
      throw new Error("Start time cannot be negative");
    }
    if (end <= start) {
      throw new Error("End time must be after start time");
    }
    if (maxDuration && end > maxDuration) {
      throw new Error(
        `End time exceeds video duration (${this.formatTime(maxDuration)})`,
      );
    }
    if (end - start < 1) {
      throw new Error("Trim must be at least 1 second");
    }
  }

  /**
   * Extract video metadata using ffprobe
   */
  private static async extractVideoMetadata(
    filePath: string,
  ): Promise<{ duration: number; size: number } | null> {
    try {
      const { stdout } = await execa("ffprobe", [
        "-v",
        "quiet",
        "-print_format",
        "json",
        "-show_format",
        filePath,
      ], { timeout: ms("30s") });

      if (!stdout) {
        return null;
      }

      const data = JSON.parse(stdout);
      const format = data.format;

      if (!format) {
        return null;
      }

      return {
        duration: Math.floor(parseFloat(format.duration) || 0),
        size: parseInt(format.size) || 0,
      };
    } catch (error: any) {
      Logger.getInstance().warn(
        `[TrimService] ffprobe failed: ${error.message}`,
      );
      return null;
    }
  }

  /**
   * Format duration as human-readable string
   */
  private static formatDuration(seconds: number): string {
    const h = Math.floor(seconds / 3600);
    const m = Math.floor((seconds % 3600) / 60);
    const s = Math.floor(seconds % 60);
    if (h > 0) return `${h}h ${m}m ${s}s`;
    if (m > 0) return `${m}m ${s}s`;
    return `${s}s`;
  }

  /**
   * Format time as HH:MM:SS or MM:SS
   */
  private static formatTime(seconds: number): string {
    const h = Math.floor(seconds / 3600);
    const m = Math.floor((seconds % 3600) / 60);
    const s = Math.floor(seconds % 60);
    if (h > 0)
      return `${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
    return `${m}:${String(s).padStart(2, "0")}`;
  }

  /**
   * Format bytes as human-readable string
   */
  private static formatBytes(bytes: number): string {
    if (bytes === 0) return "0 B";
    const k = 1024;
    const sizes = ["B", "KB", "MB", "GB"];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return `${(bytes / Math.pow(k, i)).toFixed(1)} ${sizes[i]}`;
  }
}
