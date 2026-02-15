/**
 * Job CRUD endpoints.
 */

import type { Router } from "express";
import path from "path";
import fs from "fs-extra";
import { spawn } from "child_process";
import { Logger } from "../../core/logger.js";
import { Database } from "../../core/database.js";
import { ConfigManager } from "../../core/config.js";
import { YouTubeService } from "../../engine/youtube/index.js";
import { NotificationManager, NotificationType } from "../../core/notifications.js";
import type { Job } from "../../types/jobs.js";
import { asyncHandler } from "./errorHandler.js";

export interface JobRoutesContext {
  jobLogs: Map<string, string[]>;
  filterJobsByAge: (jobs: Job[], archived: boolean) => Job[];
  broadcast: (message: { type: string; payload?: any }) => void;
  isLoopback: (ip: string) => boolean;
}

export function registerJobRoutes(router: Router, ctx: JobRoutesContext): void {
  const logger = Logger.getInstance();

  // Get all jobs
  router.get("/jobs", asyncHandler(async (req, res) => {
    const db = await Database.getInstance();
    const jobs = await db.getJobs();
    res.json(ctx.filterJobsByAge(jobs, false));
  }));

  // Get archived jobs (finished jobs older than hide_finished_age_days)
  router.get("/jobs/archived", asyncHandler(async (req, res) => {
    const db = await Database.getInstance();
    const jobs = await db.getJobs();
    const archivedJobs = ctx.filterJobsByAge(jobs, true)
      .sort((a, b) => new Date(b.updatedAt).getTime() - new Date(a.updatedAt).getTime());
    res.json(archivedJobs);
  }));

  // Get single job
  router.get("/jobs/:id", asyncHandler(async (req, res) => {
    const db = await Database.getInstance();
    const job = await db.getJob(req.params.id as string);
    if (!job) {
      return res.status(404).json({ error: "Job not found" });
    }
    res.json(job);
  }));

  // Get logs for a job
  router.get("/jobs/:id/logs", (req, res) => {
    const logs = ctx.jobLogs.get(req.params.id as string) || [];
    res.json(logs);
  });

  // Stream video file for a job
  router.get("/jobs/:id/video", asyncHandler(async (req, res) => {
    const db = await Database.getInstance();
    const config = ConfigManager.getInstance().get();
    const job = await db.getJob(req.params.id as string);

    if (!job) {
      return res.status(404).json({ error: "Job not found" });
    }

    if (!job.filename) {
      return res.status(404).json({ error: "No video file available" });
    }

    const outputDir =
      job.outputDirectory ||
      config.downloader?.output_directory ||
      "./output";
    const filePath = path.resolve(path.join(outputDir, job.filename));

    // Prevent path traversal
    const resolvedOutputDir = path.resolve(outputDir);
    if (!filePath.startsWith(resolvedOutputDir + path.sep)) {
      return res.status(403).json({ error: "Access denied" });
    }

    if (!fs.existsSync(filePath)) {
      return res.status(404).json({ error: "Video file not found" });
    }

    const stat = fs.statSync(filePath);
    const fileSize = stat.size;

    const range = req.headers.range;
    if (range) {
      const parts = range.replace(/bytes=/, "").split("-");
      const start = parseInt(parts[0], 10);
      const end = parts[1] ? parseInt(parts[1], 10) : fileSize - 1;

      if (isNaN(start) || start < 0 || isNaN(end) || end < 0 || start >= fileSize || end >= fileSize || start > end) {
        res.status(416).setHeader("Content-Range", `bytes */${fileSize}`);
        return res.end();
      }

      res.status(206);
      res.setHeader("Content-Range", `bytes ${start}-${end}/${fileSize}`);
      res.setHeader("Accept-Ranges", "bytes");
      res.setHeader("Content-Length", end - start + 1);
      res.setHeader("Content-Type", "video/mp4");
      fs.createReadStream(filePath, { start, end }).pipe(res);
    } else {
      res.setHeader("Content-Length", fileSize);
      res.setHeader("Content-Type", "video/mp4");
      res.setHeader("Accept-Ranges", "bytes");
      fs.createReadStream(filePath).pipe(res);
    }
  }));

  // Get chat data for a job
  router.get("/jobs/:id/chat", asyncHandler(async (req, res) => {
    const db = await Database.getInstance();
    const config = ConfigManager.getInstance().get();
    const job = await db.getJob(req.params.id as string);

    if (!job) {
      return res.status(404).json({ error: "Job not found" });
    }

    if (!job.chatFilename) {
      return res.status(404).json({ error: "No chat data available" });
    }

    const outputDir =
      job.outputDirectory ||
      config.downloader?.output_directory ||
      "./output";
    const chatPath = path.resolve(path.join(outputDir, job.chatFilename));

    // Prevent path traversal
    const resolvedOutputDir = path.resolve(outputDir);
    if (!chatPath.startsWith(resolvedOutputDir + path.sep)) {
      return res.status(403).json({ error: "Access denied" });
    }

    if (!fs.existsSync(chatPath)) {
      return res.status(404).json({ error: "Chat file not found" });
    }

    const chatRaw = await fs.readFile(chatPath, "utf-8");
    const chatData = JSON.parse(chatRaw);
    res.json(chatData);
  }));

  // Add new job
  router.post("/jobs", asyncHandler(async (req, res) => {
    const { videoId } = req.body;
    if (!videoId) {
      return res.status(400).json({ error: "videoId is required" });
    }
    if (!/^[a-zA-Z0-9_-]{11}$/.test(videoId)) {
      return res.status(400).json({ error: "Invalid videoId format" });
    }

    const db = await Database.getInstance();

    const exists = await db.jobExists(videoId);
    if (exists) {
      return res.status(409).json({ error: "Job already exists" });
    }

    // Fetch metadata
    let title = "Manual Add";
    let channelName = "Manual";

    try {
      const yt = YouTubeService.getInstance();
      await yt.init();
      const metadata = await yt.getMetadata(videoId);
      title = metadata.title || title;
      channelName = metadata.channelName || channelName;
    } catch (e) {
      logger.warn(`Could not fetch metadata for ${videoId}`);
    }

    const job = await db.addJob({
      videoId,
      url: `https://www.youtube.com/watch?v=${videoId}`,
      title,
      channelName,
      thumbnailUrl: `https://i.ytimg.com/vi/${videoId}/maxresdefault.jpg`,
      manuallyAdded: true,
    });

    if (job) {
      logger.info(`Added job: ${title} (${videoId})`);
      ctx.broadcast({ type: "job_added", payload: job });

      NotificationManager.getInstance().send(
        "Video Added",
        `Manually added: ${title}`,
        NotificationType.INFO,
        [
          { name: "Channel", value: channelName, inline: true },
          { name: "Video ID", value: videoId, inline: true },
        ],
        { url: `https://www.youtube.com/watch?v=${videoId}`, thumbnail: job.thumbnailUrl, event: "added" },
      );

      res.status(201).json(job);
    } else {
      res.status(500).json({ error: "Failed to create job" });
    }
  }));

  // Cancel job
  router.post("/jobs/:id/cancel", asyncHandler(async (req, res) => {
    const db = await Database.getInstance();
    const job = await db.getJob(req.params.id as string);

    if (!job) {
      return res.status(404).json({ error: "Job not found" });
    }

    if (
      !["Downloading", "Live", "Upcoming", "Muxing", "COOKIES?"].includes(
        job.status,
      )
    ) {
      return res
        .status(400)
        .json({ error: "Job cannot be cancelled in current state" });
    }

    await db.updateJob(job.id, { status: "Cancelled" });
    logger.info(`Cancelled job: ${job.title}`);

    NotificationManager.getInstance().send(
      "Job Cancelled",
      `Cancelled: ${job.title}`,
      NotificationType.CANCELLED,
      [
        { name: "Channel", value: job.channelName, inline: true },
        { name: "Video ID", value: job.videoId, inline: true },
      ],
      {
        url: `https://www.youtube.com/watch?v=${job.videoId}`,
        thumbnail: job.thumbnailUrl,
        event: "cancelled",
      },
    );

    res.json({ success: true });
  }));

  // Retry job
  router.post("/jobs/:id/retry", asyncHandler(async (req, res) => {
    const db = await Database.getInstance();
    const job = await db.getJob(req.params.id as string);

    if (!job) {
      return res.status(404).json({ error: "Job not found" });
    }

    if (!["Error", "Cancelled", "COOKIES?"].includes(job.status)) {
      return res
        .status(400)
        .json({ error: "Job cannot be retried in current state" });
    }

    await db.updateJob(job.id, {
      status: "Upcoming",
      error: undefined,
      progress: "",
    });
    logger.info(`Retrying job: ${job.title}`);
    res.json({ success: true });
  }));

  // Delete job
  router.delete("/jobs/:id", asyncHandler(async (req, res) => {
    const db = await Database.getInstance();
    const job = await db.getJob(req.params.id as string);

    if (!job) {
      return res.status(404).json({ error: "Job not found" });
    }

    if (
      !["Finished", "Error", "Cancelled", "COOKIES?"].includes(job.status)
    ) {
      return res
        .status(400)
        .json({ error: "Job cannot be deleted in current state" });
    }

    await db.deleteJob(job.id);
    ctx.jobLogs.delete(job.id);
    logger.info(`Deleted job: ${job.title}`);
    res.json({ success: true });
  }));

  // Open folder (for finished jobs)
  router.post("/jobs/:id/open-folder", asyncHandler(async (req, res) => {
    // Only allow from localhost — opening a folder on the server makes no
    // sense for remote clients.
    if (!req.ip || !ctx.isLoopback(req.ip)) {
      return res.status(403).json({ error: "Only available from localhost" });
    }

    const db = await Database.getInstance();
    const config = ConfigManager.getInstance().get();
    const job = await db.getJob(req.params.id as string);

    if (!job || !job.filename) {
      return res.status(404).json({ error: "Job or file not found" });
    }

    const outputDir =
      job.outputDirectory ||
      config.downloader?.output_directory ||
      "./output";
    const filePath = path.resolve(path.join(outputDir, job.filename));
    const resolvedOutputDir = path.resolve(outputDir);
    if (!filePath.startsWith(resolvedOutputDir + path.sep)) {
      return res.status(403).json({ error: "Access denied" });
    }
    const folderPath = path.dirname(filePath);

    const program =
      process.platform === "win32"
        ? "explorer"
        : process.platform === "darwin"
          ? "open"
          : "xdg-open";

    const child = spawn(program, [folderPath], {
      stdio: "ignore",
    });
    child.unref();
    res.json({ success: true });
  }));
}
