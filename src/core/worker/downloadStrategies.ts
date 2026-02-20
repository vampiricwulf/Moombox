/**
 * Download strategy functions for VOD, DASH, and HLS downloads.
 */

import path from "path";
import pLimit from "p-limit";
import { Database, type Job } from "../database.js";
import { ConfigManager } from "../config.js";
import { Logger } from "../logger.js";
import { YouTubeService } from "../../engine/youtube/index.js";
import { SegmentDownloader } from "../../engine/downloader.js";
import { ChatDownloader } from "../../engine/chat/index.js";
import { ManifestParser } from "../../engine/manifest.js";
import { fetchWithTimeout } from "../http.js";
import type { VideoInfo, HlsVariant } from "../../types/youtube.js";
import type { DashStream } from "../../engine/manifest.js";
import type { ChatProgress } from "../../types/chat.js";
import {
  USER_AGENTS,
} from "../../constants.js";
import { getErrorMessage } from "../../types/errors.js";
import { formatBytes } from "../../utils/timeFormat.js";
import { downloadFile } from "./muxFinalize.js";

/**
 * Download VOD using direct format URLs
 */
export async function downloadVod(
  job: Job,
  videoInfo: VideoInfo,
  yt: YouTubeService,
  stagingDir: string,
  db: Database,
  activeSegmentDownloaders: Set<SegmentDownloader>,
  chatDl?: ChatDownloader | null,
  signal?: AbortSignal,
): Promise<{
  hasAudio: boolean;
  hasVideo: boolean;
  selectedVideoFormat?: import("../../types/youtube.js").Format | null;
  selectedAudioFormat?: import("../../types/youtube.js").Format | null;
}> {
  const logger = Logger.getInstance();

  logger.info(
    "[DownloadOrchestrator] VOD download - using direct format URLs",
  );

  const { video: bestVideo, audio: bestAudio } = yt.getBestFormats(
    videoInfo.formats,
    {
      selectedVideoItag: job.selectedVideoItag,
      selectedAudioItag: job.selectedAudioItag,
    },
  );

  // Allow video-only (no audio) and audio-only (no video) downloads
  if (!bestVideo && !bestAudio) {
    throw new Error("No format with URL found for VOD");
  }
  if (!bestVideo?.url && !bestAudio?.url) {
    throw new Error("No format with URL found for VOD");
  }

  // Store format metadata for notifications
  if (bestVideo && (bestVideo.width || bestVideo.height)) {
    const metaUpdate: Partial<Job> = {
      videoWidth: bestVideo.width,
      videoHeight: bestVideo.height,
      videoFps: bestVideo.fps,
    };
    await db.updateJob(job.id, metaUpdate);
    Object.assign(job, metaUpdate);
  }

  // Detect progressive format (video+audio pre-muxed, e.g. itag 18 at 360p)
  const isProgressive = !!(bestVideo?.audioQuality && (bestVideo.width || bestVideo.height));
  if (isProgressive) {
    logger.warn(
      `[DownloadOrchestrator] Progressive format selected (itag ${bestVideo!.itag}, ` +
      `${bestVideo!.qualityLabel || (bestVideo!.width + "x" + bestVideo!.height)}) — ` +
      `video and audio are pre-muxed, no separate audio download needed`,
    );
  }

  if (bestVideo) {
    logger.info(
      `[DownloadOrchestrator] Selected Video: ${bestVideo.width}x${bestVideo.height} @ ${bestVideo.bitrate}`,
    );
  }
  if (bestAudio && !isProgressive) {
    logger.info(
      `[DownloadOrchestrator] Selected Audio: ${bestAudio.bitrate}`,
    );
  }

  const videoPath = bestVideo?.url ? path.join(stagingDir, "video_stream") : null;
  const audioPath = (!isProgressive && bestAudio?.url)
    ? path.join(stagingDir, "audio_stream")
    : null;

  // Track progress
  let videoProgress = videoPath ? "0%" : "N/A";
  let audioProgress = "0%";
  let videoPercent = 0;
  let audioPercent = 0;
  let chatMsgCount = 0;

  // Track chat progress if available
  const chatProgressHandler = chatDl
    ? (data: ChatProgress) => { chatMsgCount = data.messageCount; updateCombinedProgress(); }
    : null;
  if (chatDl && chatProgressHandler) chatDl.on("progress", chatProgressHandler);

  const updateCombinedProgress = () => {
    const combinedPercent = audioPath && videoPath
      ? (videoPercent + audioPercent) / 2
      : videoPath ? videoPercent : audioPercent;
    const chatStr = chatMsgCount > 0 ? ` C:${chatMsgCount}` : "";
    db.updateJob(job.id, {
      progress: `V:${videoProgress} A:${audioProgress}${chatStr}`,
      percent: combinedPercent,
    }).catch((e: unknown) => logger.debug(`[DownloadOrchestrator] Progress update failed: ${getErrorMessage(e)}`));
  };

  // Download in parallel
  logger.info(
    "[DownloadOrchestrator] Downloading video and audio streams in parallel...",
  );

  // Use p-limit to ensure max 2 concurrent downloads (video + audio)
  const limit = pLimit(2);
  const downloadPromises: (Promise<void> | false)[] = [];

  if (bestVideo?.url && videoPath) {
    downloadPromises.push(
      limit(() =>
        downloadFile(bestVideo.url!, videoPath!, activeSegmentDownloaders, signal, (progress, percent) => {
          videoProgress =
            percent !== undefined ? `${percent.toFixed(1)}%` : progress;
          videoPercent = percent || 0;
          updateCombinedProgress();
        }),
      ),
    );
  } else {
    videoProgress = "N/A";
    downloadPromises.push(false);
  }

  if (bestAudio?.url && audioPath) {
    downloadPromises.push(
      limit(() =>
        downloadFile(bestAudio.url!, audioPath!, activeSegmentDownloaders, signal, (progress, percent) => {
          audioProgress =
            percent !== undefined ? `${percent.toFixed(1)}%` : progress;
          audioPercent = percent || 0;
          updateCombinedProgress();
        }),
      ),
    );
  } else {
    audioProgress = "N/A";
    downloadPromises.push(false);
  }

  await Promise.all(downloadPromises.filter(Boolean));
  if (chatDl && chatProgressHandler) chatDl.removeListener("progress", chatProgressHandler);
  logger.info("[DownloadOrchestrator] Parallel download complete");
  return {
    hasAudio: !isProgressive && !!audioPath,
    hasVideo: !!videoPath,
    selectedVideoFormat: bestVideo,
    selectedAudioFormat: bestAudio,
  };
}

/**
 * Download DASH stream
 */
export async function downloadDash(
  job: Job,
  videoInfo: VideoInfo,
  yt: YouTubeService,
  stagingDir: string,
  db: Database,
  activeSegmentDownloaders: Set<SegmentDownloader>,
): Promise<{
  videoDl: SegmentDownloader;
  audioDl: SegmentDownloader;
  videoPath: string;
  audioPath: string;
  selectedVideoFormat?: DashStream;
  selectedAudioFormat?: DashStream;
}> {
  const logger = Logger.getInstance();
  logger.info("[DownloadOrchestrator] Fetching DASH Manifest...");

  let decryptedDashUrl = await yt.decryptDashManifestUrl(
    videoInfo.dashManifestUrl!,
    videoInfo.playerUrl,
  );

  // Add PO token to manifest URL (required for live streams)
  // yt-dlp appends /pot/{token} to the manifest path
  const poToken = yt.getPoToken();
  if (poToken) {
    // Remove trailing slash if present, then append /pot/{token}
    decryptedDashUrl = decryptedDashUrl.replace(/\/?$/, `/pot/${poToken}`);
    logger.debug(
      `[DownloadOrchestrator] Added PO token to manifest URL`,
    );
  } else {
    logger.warn(
      "[DownloadOrchestrator] No PO token available for manifest URL - may get 403 errors",
    );
  }

  const manifestResp = await fetchWithTimeout(decryptedDashUrl);
  if (!manifestResp.ok) {
    throw new Error(
      `Failed to fetch DASH manifest: HTTP ${manifestResp.status}`,
    );
  }

  const manifestXml = await manifestResp.text();
  const dashStreams = ManifestParser.parseDash(
    manifestXml,
    videoInfo.dashManifestUrl!,
  );

  logger.debug(
    `[DownloadOrchestrator] DASH manifest parsed, found ${dashStreams.length} streams`,
  );

  // Decrypt n parameter in segment BaseURLs (required to avoid throttling/403)
  if (videoInfo.playerUrl) {
    logger.debug(
      `[DownloadOrchestrator] Decrypting n parameter in segment BaseURLs...`,
    );
    // Limit concurrent n-parameter decryption to avoid overwhelming the cipher
    const decryptLimit = pLimit(10);
    await Promise.all(dashStreams.map(stream => decryptLimit(async () => {
      if (stream.baseUrl) {
        const originalUrl = stream.baseUrl;
        stream.baseUrl = await yt.decryptNParamInUrl(
          stream.baseUrl,
          videoInfo.playerUrl!,
        );
        if (stream.baseUrl !== originalUrl) {
          logger.debug(
            `[DownloadOrchestrator] Decrypted n param for itag=${stream.itag}`,
          );
        }
      }
    })));
  } else {
    logger.warn(
      `[DownloadOrchestrator] No player URL available - cannot decrypt n params`,
    );
  }

  // Select best streams (respecting manual itag selection from job)
  let bestVideo: DashStream | null = null;
  let bestAudio: DashStream | null = null;

  const config = ConfigManager.getInstance().get();
  const maxRes = config.downloader?.max_video_resolution || 9999;

  // Try manual selection first
  if (job.selectedVideoItag != null && job.selectedVideoItag !== -1) {
    bestVideo = dashStreams.find(
      (s) => s.itag === job.selectedVideoItag && s.mimeType?.includes("video"),
    ) || null;
    if (bestVideo) {
      logger.info(`[DownloadOrchestrator] Manual DASH video selection: itag ${bestVideo.itag}`);
    } else {
      logger.warn(`[DownloadOrchestrator] Manual video itag ${job.selectedVideoItag} not in DASH manifest, falling back to auto`);
    }
  }
  if (job.selectedAudioItag != null && job.selectedAudioItag !== -1) {
    bestAudio = dashStreams.find(
      (s) => s.itag === job.selectedAudioItag && s.mimeType?.includes("audio"),
    ) || null;
    if (bestAudio) {
      logger.info(`[DownloadOrchestrator] Manual DASH audio selection: itag ${bestAudio.itag}`);
    } else {
      logger.warn(`[DownloadOrchestrator] Manual audio itag ${job.selectedAudioItag} not in DASH manifest, falling back to auto`);
    }
  }

  // Auto-select remaining (skip if user explicitly chose "none" with itag -1)
  for (const stream of dashStreams) {
    logger.debug(
      `[DownloadOrchestrator] Stream: itag=${stream.itag} mime=${stream.mimeType} ${stream.width || 0}x${stream.height || 0} bw=${stream.bandwidth}`,
    );
    if (stream.mimeType?.includes("video") && job.selectedVideoItag !== -1) {
      const streamMaxDim = Math.max(stream.width || 0, stream.height || 0);
      if (streamMaxDim > maxRes) continue;
      if (!bestVideo || stream.bandwidth > bestVideo.bandwidth) {
        bestVideo = stream;
      }
    } else if (stream.mimeType?.includes("audio") && job.selectedAudioItag !== -1) {
      if (!bestAudio || stream.bandwidth > bestAudio.bandwidth) {
        bestAudio = stream;
      }
    }
  }

  if (!bestVideo || !bestAudio) {
    // For DASH streams, both video and audio are required
    // (audio-only / video-only not supported for live segment downloads)
    logger.error(
      `[DownloadOrchestrator] Stream selection failed: video=${!!bestVideo} audio=${!!bestAudio}`,
    );
    logger.debug(
      `[DownloadOrchestrator] Raw manifest (first 2000 chars): ${manifestXml.substring(0, 2000)}`,
    );
    throw new Error(
      "Could not find suitable Video/Audio streams in DASH Manifest",
    );
  }

  logger.info(
    `[DownloadOrchestrator] Selected DASH Video: ${bestVideo.width}x${bestVideo.height} @ ${bestVideo.bandwidth}`,
  );
  logger.info(
    `[DownloadOrchestrator] Selected DASH Audio: ${bestAudio.bandwidth}`,
  );

  // Store format metadata for notifications
  if (bestVideo.width || bestVideo.height) {
    const metaUpdate: Partial<Job> = {
      videoWidth: bestVideo.width,
      videoHeight: bestVideo.height,
    };
    await db.updateJob(job.id, metaUpdate);
    Object.assign(job, metaUpdate);
  }

  const videoPath = path.join(stagingDir, "video_stream");
  const audioPath = path.join(stagingDir, "audio_stream");

  // Always start from segment 0 to capture the entire stream
  // (the manifest's startNumber reflects the current live position, not the beginning)
  logger.info(
    `[DownloadOrchestrator] Starting from segment 0 (manifest startNumber: ${bestVideo.startNumber})`
  );

  // Callback to check if stream has ended via YouTube API
  const checkStreamStatus = async (): Promise<boolean> => {
    try {
      const freshInfo = await yt.getVideoInfo(job.videoId);
      logger.info(`[DownloadOrchestrator] Status check: ${freshInfo.streamStatus}`);
      return freshInfo.streamStatus === "post_live"
          || freshInfo.streamStatus === "vod"
          || freshInfo.streamStatus === "not_a_stream";
    } catch (e) {
      logger.warn(`[DownloadOrchestrator] Status check failed: ${getErrorMessage(e)}`);
      return false;
    }
  };

  const videoDl = new SegmentDownloader({
    baseUrl: bestVideo.baseUrl,
    outputFile: videoPath,
    startSeq: 0,
    initUrl: bestVideo.initialization,
    onCheckStreamStatus: checkStreamStatus,
    retryDelayCap: config.downloader.segment_retry_delay_cap,
    liveCheckRetries: config.downloader.segment_live_check_retries,
  });

  const audioDl = new SegmentDownloader({
    baseUrl: bestAudio.baseUrl,
    outputFile: audioPath,
    startSeq: 0,
    initUrl: bestAudio.initialization,
    onCheckStreamStatus: checkStreamStatus,
    retryDelayCap: config.downloader.segment_retry_delay_cap,
    liveCheckRetries: config.downloader.segment_live_check_retries,
  });

  activeSegmentDownloaders.add(videoDl);
  activeSegmentDownloaders.add(audioDl);

  logger.info(
    "[DownloadOrchestrator] Starting Parallel DASH Download...",
  );

  return {
    videoDl,
    audioDl,
    videoPath,
    audioPath,
    selectedVideoFormat: bestVideo ?? undefined,
    selectedAudioFormat: bestAudio,
  };
}

/**
 * Download HLS stream
 */
export async function downloadHls(
  job: Job,
  videoInfo: VideoInfo,
  yt: YouTubeService,
  stagingDir: string,
  db: Database,
  activeSegmentDownloaders: Set<SegmentDownloader>,
): Promise<{ videoDl: SegmentDownloader; videoPath: string }> {
  const logger = Logger.getInstance();
  logger.info("[DownloadOrchestrator] Fetching HLS Master Playlist...");

  const manifestResp = await fetchWithTimeout(videoInfo.hlsManifestUrl!);
  if (!manifestResp.ok) {
    throw new Error(
      `Failed to fetch HLS manifest: HTTP ${manifestResp.status}`,
    );
  }
  const manifestText = await manifestResp.text();
  const masterPlaylist = ManifestParser.parseHls(
    manifestText,
    videoInfo.hlsManifestUrl!,
  );

  if (masterPlaylist.type !== "master" || !masterPlaylist.variants) {
    throw new Error("Invalid HLS Master Playlist");
  }

  // Select best variant (respecting max_video_resolution)
  const config = ConfigManager.getInstance().get();
  const maxRes = config.downloader?.max_video_resolution || 9999;
  let bestVariant: HlsVariant | null = null;
  for (const variant of masterPlaylist.variants) {
    const varMaxDim = Math.max(variant.width || 0, variant.height || 0);
    if (varMaxDim > maxRes) continue;
    if (!bestVariant || variant.bandwidth > bestVariant.bandwidth) {
      bestVariant = variant;
    }
  }

  if (!bestVariant) {
    throw new Error("No variants found in HLS Playlist (within resolution limit)");
  }

  logger.info(
    `[DownloadOrchestrator] Selected HLS Variant: ${bestVariant.width}x${bestVariant.height} @ ${bestVariant.bandwidth}`,
  );

  // Store format metadata for notifications
  if (bestVariant.width || bestVariant.height) {
    const metaUpdate: Partial<Job> = {
      videoWidth: bestVariant.width,
      videoHeight: bestVariant.height,
    };
    await db.updateJob(job.id, metaUpdate);
    Object.assign(job, metaUpdate);
  }

  const videoPath = path.join(stagingDir, "stream.ts");

  // Callback to check if stream has ended via YouTube API
  const checkStreamStatus = async (): Promise<boolean> => {
    try {
      const freshInfo = await yt.getVideoInfo(job.videoId);
      logger.info(`[DownloadOrchestrator] HLS status check: ${freshInfo.streamStatus}`);
      return freshInfo.streamStatus === "post_live"
          || freshInfo.streamStatus === "vod"
          || freshInfo.streamStatus === "not_a_stream";
    } catch (e) {
      logger.warn(`[DownloadOrchestrator] HLS status check failed: ${getErrorMessage(e)}`);
      return false;
    }
  };

  const videoDl = new SegmentDownloader({
    baseUrl: bestVariant.url,
    outputFile: videoPath,
    startSeq: -1,
    isHls: true,
    onCheckStreamStatus: checkStreamStatus,
    retryDelayCap: config.downloader.segment_retry_delay_cap,
    liveCheckRetries: config.downloader.segment_live_check_retries,
  });

  activeSegmentDownloaders.add(videoDl);

  logger.info("[DownloadOrchestrator] Starting HLS Download...");

  return { videoDl, videoPath };
}
