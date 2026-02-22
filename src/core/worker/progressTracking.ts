/**
 * Segment progress tracking and chat downloader setup.
 */

import path from "path";
import { Database, type Job } from "../database.js";
import { Logger } from "../logger.js";
import { YouTubeService } from "../../engine/youtube/index.js";
import { SegmentDownloader } from "../../engine/downloader.js";
import { ChatDownloader, ChatApi } from "../../engine/chat/index.js";
import type { VideoInfo } from "../../types/youtube.js";
import type { ChatProgress } from "../../types/chat.js";
import { PROGRESS_UPDATE_INTERVAL_MS, PROGRESS_PERSIST_INTERVAL_MS } from "../../constants.js";
import { formatSpeed } from "../../utils/timeFormat.js";
import { SmoothValue } from "../../utils/SmoothValue.js";
import { getErrorMessage } from "../../types/errors.js";

/**
 * Setup chat downloader if chat is available
 */
export async function setupChatDownloader(
  job: Job,
  videoInfo: VideoInfo,
  yt: YouTubeService,
  stagingDir: string,
  db: Database,
  activeChatDownloaders: Set<{ stop(): void }>,
): Promise<ChatDownloader | null> {
  const logger = Logger.getInstance();

  try {
    // Fetch watch page HTML to extract chat continuation
    const html = await yt.fetchWatchPageHtml(job.videoId);
    const chatApi = new ChatApi();
    const { continuation, isReplay } = chatApi.extractChatContinuation(html);

    if (!continuation) {
      logger.debug(`[Chat] No chat available for ${job.videoId}`);
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

    activeChatDownloaders.add(chatDl);

    chatDl.on("start", () => {
      db.updateJob(job.id, { chatStatus: "downloading" }).catch((e: unknown) => logger.debug(`[Chat] DB update failed: ${getErrorMessage(e)}`));
      logger.info(`[Chat] Started downloading chat for ${job.videoId}`);
    });

    chatDl.on("progress", (data) => {
      db.updateJobLive(job.id, { totalChatMessages: data.messageCount });
    });

    chatDl.on("error", (err) => {
      logger.warn(`[Chat] Error: ${err.message}`);
      db.updateJob(job.id, { chatStatus: "error" }).catch((e: unknown) => logger.debug(`[Chat] DB update failed: ${getErrorMessage(e)}`));
    });

    chatDl.on("finish", () => {
      activeChatDownloaders.delete(chatDl);
      logger.info(`[Chat] Finished downloading chat for ${job.videoId}`);
      db.updateJob(job.id, { chatStatus: "finished" }).catch((e: unknown) => logger.debug(`[Chat] DB update failed: ${getErrorMessage(e)}`));
    });

    return chatDl;
  } catch (e: unknown) {
    logger.warn(`[Chat] Failed to setup chat downloader: ${getErrorMessage(e)}`);
    await db.updateJob(job.id, { chatStatus: "error" });
    return null;
  }
}

/**
 * Shared implementation for segment downloader progress tracking.
 * Attaches progress/gap listeners, optionally starts chat, waits for completion.
 *
 * @param startChat - If true, starts chatDl in Promise.all alongside segment downloaders.
 *   If false, only reads chatDl count and tracks progress (chat started separately).
 */
async function runSegmentDownloadersImpl(
  job: Job,
  videoDl: SegmentDownloader | null,
  audioDl: SegmentDownloader | null,
  chatDl: ChatDownloader | null,
  db: Database,
  activeSegmentDownloaders: Set<SegmentDownloader>,
  startChat: boolean,
): Promise<void> {
  const logger = Logger.getInstance();
  let vSeq = 0;
  let aSeq = 0;
  let vTotal = 0;
  let aTotal = 0;
  let vBytes = 0;
  let aBytes = 0;
  let chatMsgCount = chatDl && !startChat ? chatDl.getMessageCount() : 0;
  let lastUpdate = 0;
  let lastBytes = 0;
  let lastBytesTime = Date.now();
  const speedSmoother = new SmoothValue(0.7); // Quick response to changes

  const updateProgress = () => {
    const now = Date.now();
    if (now - lastUpdate > PROGRESS_UPDATE_INTERVAL_MS) {
      lastUpdate = now;

      const totalBytes = vBytes + aBytes;
      const timeDelta = (now - lastBytesTime) / 1000;
      const bytesDelta = totalBytes - lastBytes;
      const instantSpeed = timeDelta > 0 ? bytesDelta / timeDelta : 0;
      const speed = speedSmoother.update(instantSpeed);

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

      // In-memory update — notifies TUI/WebSocket instantly, no disk I/O
      db.updateJobLive(job.id, {
        progress: progStr,
        speed: formatSpeed(speed),
        lastVideoSeq: vSeq,
        lastAudioSeq: aSeq,
        totalVideoSeq: vTotal || undefined,
        totalAudioSeq: aTotal || undefined,
        totalChatMessages: chatMsgCount || undefined,
      });
    }
  };

  // Periodic disk persistence for crash recovery (much less frequent than display updates)
  const persistTimer = setInterval(() => {
    db.persistToDisk().catch((e: unknown) =>
      logger.debug(`[DownloadOrchestrator] Persist failed: ${getErrorMessage(e)}`),
    );
  }, PROGRESS_PERSIST_INTERVAL_MS);

  // Track segment gaps — accumulate locally to avoid read-modify-write races
  const localGaps: Array<{ from: number; to: number; stream: "video" | "audio" }> = [];
  const gapHandler = (stream: "video" | "audio") => (gap: { from: number; to: number }) => {
    logger.warn(`[DownloadOrchestrator] ${stream} segment gap: seq ${gap.from}–${gap.to}`);
    localGaps.push({ ...gap, stream });
    db.updateJob(job.id, { gaps: [...localGaps] }).catch((e: unknown) =>
      logger.debug(`[DownloadOrchestrator] Gap update failed: ${getErrorMessage(e)}`),
    );
  };

  if (videoDl) {
    videoDl.on("progress", (d) => {
      vSeq = d.seq;
      vBytes = d.bytes || 0;
      if (d.total || d.headSeq) vTotal = d.total || d.headSeq;
      updateProgress();
    });
    videoDl.on("gap", gapHandler("video"));
  }

  if (audioDl) {
    audioDl.on("progress", (d) => {
      aSeq = d.seq;
      aBytes = d.bytes || 0;
      if (d.total || d.headSeq) aTotal = d.total || d.headSeq;
      updateProgress();
    });
    audioDl.on("gap", gapHandler("audio"));
  }

  const chatProgressHandler = chatDl
    ? (d: ChatProgress) => { chatMsgCount = d.messageCount; updateProgress(); }
    : null;
  if (chatDl && chatProgressHandler) chatDl.on("progress", chatProgressHandler);

  const promises: Promise<void>[] = [];
  if (videoDl) promises.push(videoDl.start().finally(() => activeSegmentDownloaders.delete(videoDl!)));
  if (audioDl) promises.push(audioDl.start().finally(() => activeSegmentDownloaders.delete(audioDl!)));
  if (startChat && chatDl)
    promises.push(
      chatDl.start().catch((e: unknown) => {
        logger.warn(`[Chat] Chat download error: ${getErrorMessage(e)}`);
      }),
    );
  await Promise.all(promises);

  // Clean up persist timer and flush final state to disk
  clearInterval(persistTimer);
  await db.persistToDisk().catch((e: unknown) =>
    logger.debug(`[DownloadOrchestrator] Final persist failed: ${getErrorMessage(e)}`),
  );

  // Remove chat listener to prevent leak
  if (chatDl && chatProgressHandler) chatDl.removeListener("progress", chatProgressHandler);
}

/**
 * Run segment downloaders with progress tracking (starts chat alongside segments)
 */
export async function runSegmentDownloaders(
  job: Job,
  videoDl: SegmentDownloader | null,
  audioDl: SegmentDownloader | null,
  chatDl: ChatDownloader | null,
  db: Database,
  activeSegmentDownloaders: Set<SegmentDownloader>,
): Promise<void> {
  return runSegmentDownloadersImpl(job, videoDl, audioDl, chatDl, db, activeSegmentDownloaders, true);
}

/**
 * Run segment downloaders for video/audio only (chat handled separately in live stream loop)
 */
export async function runSegmentDownloadersWithoutChat(
  job: Job,
  videoDl: SegmentDownloader | null,
  audioDl: SegmentDownloader | null,
  chatDl: ChatDownloader | null,
  db: Database,
  activeSegmentDownloaders: Set<SegmentDownloader>,
): Promise<void> {
  return runSegmentDownloadersImpl(job, videoDl, audioDl, chatDl, db, activeSegmentDownloaders, false);
}
