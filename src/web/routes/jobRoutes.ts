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
import { TwitchService } from "../../engine/twitch/index.js";
import { NotificationManager, NotificationType } from "../../core/notifications.js";
import { getErrorMessage } from "../../types/errors.js";
import type { Job } from "../../types/jobs.js";
import { asyncHandler } from "./errorHandler.js";
import { createRateLimiter } from "./rateLimiter.js";
import { addJobSchema, formatValidationErrors } from "../../types/schemas.js";
import { validateOffsetPagination } from "./validators.js";
import { TWITCH_URLS } from "../../constants.js";

export interface JobRoutesContext {
  jobLogs: Map<string, string[]>;
  filterJobsByAge: (jobs: Job[], archived: boolean) => Job[];
  broadcast: (message: { type: string; payload?: unknown }) => void;
  isLoopback: (ip: string) => boolean;
}

/** Apply optional offset/limit pagination to an array and send response */
function sendPaginated<T>(
  items: T[],
  req: { query: Record<string, unknown> },
  res: { status: (code: number) => { json: (body: unknown) => void }; json: (body: unknown) => void },
): void {
  let offset: number;
  let limit: number | undefined;
  try {
    ({ offset, limit } = validateOffsetPagination(
      req.query.offset as string | undefined,
      req.query.limit as string | undefined,
    ));
  } catch (e: unknown) {
    res.status(400).json({ error: e instanceof Error ? e.message : String(e) });
    return;
  }

  if (limit !== undefined || offset > 0) {
    const paginated = limit !== undefined
      ? items.slice(offset, offset + limit)
      : items.slice(offset);

    res.json({
      data: paginated,
      pagination: {
        total: items.length,
        offset,
        limit: limit || (items.length - offset),
        hasMore: limit !== undefined && (offset + limit) < items.length,
      },
    });
    return;
  }

  // Backward compatibility: return plain array if no pagination params
  res.json(items);
}

export function registerJobRoutes(router: Router, ctx: JobRoutesContext): void {
  const logger = Logger.getInstance();

  // Get all jobs (with optional pagination)
  router.get("/jobs", asyncHandler(async (req, res) => {
    const db = await Database.getInstance();
    const jobs = await db.getJobs();
    const filtered = ctx.filterJobsByAge(jobs, false);
    sendPaginated(filtered, req, res);
  }));

  // Get archived jobs (finished jobs older than hide_finished_age_days, with optional pagination)
  router.get("/jobs/archived", asyncHandler(async (req, res) => {
    const db = await Database.getInstance();
    const jobs = await db.getJobs();
    const archivedJobs = ctx.filterJobsByAge(jobs, true)
      .sort((a, b) => new Date(b.updatedAt).getTime() - new Date(a.updatedAt).getTime());
    sendPaginated(archivedJobs, req, res);
  }));

  // Get single job
  router.get("/jobs/:id", asyncHandler(async (req, res) => {
    const db = await Database.getInstance();
    const job = await db.getJob(req.params.id as string);
    if (!job) {
      return res.status(404).json({ error: "Job not found" });
    }

    // Cache finished jobs (immutable), but not active ones
    if (job.status === "Finished") {
      res.setHeader("Cache-Control", "public, max-age=31536000, immutable");
    } else {
      res.setHeader("Cache-Control", "no-cache, must-revalidate");
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

    if (!(await fs.pathExists(filePath))) {
      return res.status(404).json({ error: "Video file not found" });
    }

    const stat = await fs.stat(filePath);
    const fileSize = stat.size;

    // Derive Content-Type from file extension
    const ext = path.extname(filePath).toLowerCase();
    const contentType = ext === ".webm" ? "video/webm"
      : ext === ".mkv" ? "video/x-matroska"
      : "video/mp4";

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
      res.setHeader("Content-Type", contentType);
      const stream = fs.createReadStream(filePath, { start, end });
      stream.on("error", () => { if (!res.headersSent) res.status(500).end(); });
      res.on("close", () => { stream.destroy(); });
      stream.pipe(res);
    } else {
      res.setHeader("Content-Length", fileSize);
      res.setHeader("Content-Type", contentType);
      res.setHeader("Accept-Ranges", "bytes");
      const stream = fs.createReadStream(filePath);
      stream.on("error", () => { if (!res.headersSent) res.status(500).end(); });
      res.on("close", () => { stream.destroy(); });
      stream.pipe(res);
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

    if (!(await fs.pathExists(chatPath))) {
      return res.status(404).json({ error: "Chat file not found" });
    }

    const chatRaw = await fs.readFile(chatPath, "utf-8");
    let chatData: unknown;
    try {
      chatData = JSON.parse(chatRaw);
    } catch {
      return res.status(422).json({ error: "Chat file is corrupt or unreadable" });
    }
    res.json(chatData);
  }));

  // Get available formats for a video (for Advanced Options)
  router.get("/formats/:videoId", asyncHandler(async (req, res) => {
    const { videoId } = req.params;
    if (!videoId || !/^[a-zA-Z0-9_-]{11}$/.test(videoId as string)) {
      return res.status(400).json({ error: "Invalid videoId format" });
    }

    try {
      const yt = YouTubeService.getInstance();
      await yt.init();
      const videoInfo = await yt.getVideoInfo(videoId as string);

      const videoFormats = (videoInfo.formats || [])
        .filter((f) => f.mimeType?.includes("video") && f.url)
        .map((f) => ({
          itag: f.itag,
          mimeType: f.mimeType,
          width: f.width,
          height: f.height,
          fps: f.fps,
          bitrate: f.bitrate,
          qualityLabel: f.qualityLabel,
          audioQuality: f.audioQuality,
          contentLength: f.contentLength,
        }))
        .sort((a, b) => {
          const aMaxDim = Math.max(a.width || 0, a.height || 0);
          const bMaxDim = Math.max(b.width || 0, b.height || 0);
          // Sort by resolution (higher first)
          if (aMaxDim !== bMaxDim) return bMaxDim - aMaxDim;
          // Same resolution: sort by FPS (60fps first for modern videos)
          const aFps = a.fps || 30;
          const bFps = b.fps || 30;
          if (aFps !== bFps) return bFps - aFps;
          // Same resolution and FPS: prefer LOWER bitrate (better compression)
          return a.bitrate - b.bitrate;
        });

      const audioFormats = (videoInfo.formats || [])
        .filter((f) => f.mimeType?.includes("audio") && f.url)
        .map((f) => ({
          itag: f.itag,
          mimeType: f.mimeType,
          bitrate: f.bitrate,
          audioQuality: f.audioQuality,
          audioSampleRate: f.audioSampleRate,
          contentLength: f.contentLength,
        }))
        .sort((a, b) => b.bitrate - a.bitrate);

      // Mark best formats per container type
      const markBest = <T extends { itag: number; mimeType?: string }>(
        formats: T[],
        typeFilter: string,
      ): number | null => {
        const match = formats.find((f) => f.mimeType?.includes(typeFilter));
        return match ? match.itag : null;
      };

      res.json({
        videoId,
        title: videoInfo.title,
        channelName: videoInfo.channelName,
        lengthSeconds: videoInfo.lengthSeconds,
        streamStatus: videoInfo.streamStatus,
        videoFormats,
        audioFormats,
        bestItags: {
          bestWebmVideo: markBest(videoFormats, "webm"),
          bestMp4Video: markBest(videoFormats, "mp4"),
          bestOpusAudio: markBest(audioFormats, "opus") ?? markBest(audioFormats, "webm"),
          bestAacAudio: markBest(audioFormats, "mp4"),
        },
      });
    } catch (e: unknown) {
      logger.warn(`Failed to fetch formats for ${videoId}: ${getErrorMessage(e)}`);
      res.status(500).json({ error: "Failed to fetch video formats" });
    }
  }));

  // Add new job (rate limited to 20 requests per minute)
  const addJobRateLimiter = createRateLimiter(20, 60_000);
  router.post("/jobs", addJobRateLimiter, asyncHandler(async (req, res) => {
    // Validate request body with Zod schema
    const validation = addJobSchema.safeParse(req.body);
    if (!validation.success) {
      const errors = formatValidationErrors(validation.error);
      return res.status(400).json({
        error: "Validation failed",
        details: errors
      });
    }

    const data = validation.data;
    const db = await Database.getInstance();

    // Branch on platform
    if ("platform" in data && data.platform === "twitch") {
      // --- Twitch job ---
      const twitchData = data as { platform: "twitch"; videoId: string; twitchType?: string; quality_preference?: string };
      const login = twitchData.videoId.toLowerCase();
      const isVod = twitchData.twitchType === "vod";

      // Build job ID
      let jobId: string;
      let url: string;
      let title = "Manual Add";
      let channelName = login;
      let thumbnailUrl: string | undefined;
      let channelAvatarUrl: string | undefined;
      let streamStartTime: string | undefined;
      let twitchCategory: string | undefined;

      const twitch = TwitchService.getInstance();
      await twitch.init();

      if (isVod) {
        jobId = `tw_v${login}`;
        url = `${TWITCH_URLS.BASE}/videos/${login}`;
        try {
          const vodInfo = await twitch.getVodInfo(login);
          title = `${vodInfo.channelDisplayName} — ${vodInfo.title}`;
          channelName = vodInfo.channelDisplayName;
          thumbnailUrl = vodInfo.thumbnailUrl;
          streamStartTime = vodInfo.createdAt;
          twitchCategory = vodInfo.gameCategory;
        } catch {
          logger.warn(`Could not fetch Twitch VOD metadata for ${login}`);
        }
      } else {
        // Live channel: check if live and get stream ID
        const streamInfo = await twitch.getStreamInfo(login);
        if (streamInfo && streamInfo.isLive) {
          jobId = `tw_${streamInfo.streamId}`;
          title = `${streamInfo.channelDisplayName} — ${streamInfo.title || "Live"}`;
          channelName = streamInfo.channelDisplayName;
          thumbnailUrl = streamInfo.thumbnailUrl;
          channelAvatarUrl = streamInfo.profileImageUrl;
          streamStartTime = streamInfo.startedAt;
          twitchCategory = streamInfo.gameCategory;
        } else {
          jobId = `tw_manual_${login}_${Date.now()}`;
          title = `${login} — Manual Add`;
        }
        url = `${TWITCH_URLS.BASE}/${login}`;
      }

      const exists = await db.jobExists(jobId);
      if (exists) {
        return res.status(409).json({ error: "Job already exists" });
      }

      const job = await db.addJob({
        videoId: jobId,
        url,
        title,
        channelName,
        platform: "twitch",
        thumbnailUrl,
        channelAvatarUrl,
        manuallyAdded: true,
        isVod: isVod || undefined,
        streamStartTime,
        twitchCategory,
        twitchQuality: twitchData.quality_preference,
      });

      if (job) {
        logger.info(`Added Twitch job: ${title} (${jobId})`);
        // job_added not needed — the DB's notifyJobsChange triggers a jobs_update broadcast
        NotificationManager.getInstance().send(
          "Twitch Video Added",
          `Manually added: ${title}`,
          NotificationType.INFO,
          [
            { name: "Channel", value: channelName, inline: true },
            { name: "Stream ID", value: jobId, inline: true },
          ],
          { url, thumbnail: thumbnailUrl, event: "added" },
        );
        res.status(201).json(job);
      } else {
        res.status(500).json({ error: "Failed to create job" });
      }
    } else {
      // --- YouTube job ---
      const ytData = data as { videoId: string; selectedVideoItag?: number; selectedAudioItag?: number; startTime?: number; endTime?: number };
      const { videoId, selectedVideoItag, selectedAudioItag, startTime, endTime } = ytData;

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

      // Build advanced options (already validated by schema)
      const advancedOpts: Record<string, number | undefined> = {
        selectedVideoItag,
        selectedAudioItag,
        startTime,
        endTime,
      };

      const job = await db.addJob({
        videoId,
        url: `https://www.youtube.com/watch?v=${videoId}`,
        title,
        channelName,
        thumbnailUrl: `https://i.ytimg.com/vi/${videoId}/maxresdefault.jpg`,
        manuallyAdded: true,
        ...advancedOpts,
      });

      if (job) {
        logger.info(`Added job: ${title} (${videoId})`);
        // job_added not needed — the DB's notifyJobsChange triggers a jobs_update broadcast

        // Build notification fields with advanced options
        const fields: Array<{ name: string; value: string; inline?: boolean }> = [
          { name: "Channel", value: channelName, inline: true },
          { name: "Video ID", value: videoId, inline: true },
        ];

        // Add format selection fields if specified
        if (advancedOpts.selectedVideoItag != null) {
          const formatLabel = advancedOpts.selectedVideoItag === -1
            ? "None (audio only)"
            : `itag ${advancedOpts.selectedVideoItag}`;
          fields.push({ name: "Video Format", value: formatLabel, inline: true });
        }
        if (advancedOpts.selectedAudioItag != null) {
          const formatLabel = advancedOpts.selectedAudioItag === -1
            ? "None (video only)"
            : `itag ${advancedOpts.selectedAudioItag}`;
          fields.push({ name: "Audio Format", value: formatLabel, inline: true });
        }

        // Add time range if specified
        if (advancedOpts.startTime != null || advancedOpts.endTime != null) {
          const startMin = Math.floor((advancedOpts.startTime || 0) / 60);
          const startSec = (advancedOpts.startTime || 0) % 60;
          const startStr = `${startMin}:${String(startSec).padStart(2, "0")}`;
          const endStr = advancedOpts.endTime != null
            ? `${Math.floor(advancedOpts.endTime / 60)}:${String(advancedOpts.endTime % 60).padStart(2, "0")}`
            : "end";
          const duration = advancedOpts.endTime != null
            ? advancedOpts.endTime - (advancedOpts.startTime || 0)
            : null;
          const rangeValue = duration != null
            ? `${startStr} - ${endStr} (${Math.floor(duration / 60)}:${String(duration % 60).padStart(2, "0")})`
            : `${startStr} - ${endStr}`;
          fields.push({ name: "Time Range", value: rangeValue, inline: false });
        }

        NotificationManager.getInstance().send(
          "Video Added",
          `Manually added: ${title}`,
          NotificationType.INFO,
          fields,
          { url: `https://www.youtube.com/watch?v=${videoId}`, thumbnail: job.thumbnailUrl, event: "added" },
        );

        res.status(201).json(job);
      } else {
        res.status(500).json({ error: "Failed to create job" });
      }
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
        { name: job.platform === "twitch" ? "Stream ID" : "Video ID", value: job.videoId, inline: true },
      ],
      {
        url: job.url || `https://www.youtube.com/watch?v=${job.videoId}`,
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

    // Fire and forget — spawn passes path as direct arg (no shell metachar issues)
    const [program, args] = process.platform === "win32"
      ? ["explorer.exe", [folderPath]]
      : [process.platform === "darwin" ? "open" : "xdg-open", [folderPath]];
    spawn(program, args, { stdio: "ignore", detached: true }).unref();
    res.json({ success: true });
  }));
}
