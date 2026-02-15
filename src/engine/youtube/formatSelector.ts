/**
 * YouTube Format Selector
 *
 * Selects the best video and audio formats from available options.
 */

import { Logger } from "../../core/logger.js";
import { ConfigManager } from "../../core/config.js";
import type { Format } from "../../types/youtube.js";

/**
 * Selected formats for download
 */
export interface SelectedFormats {
  video: Format | null;
  audio: Format | null;
}

/**
 * Options for manual format selection
 */
export interface FormatSelectionOptions {
  selectedVideoItag?: number;   // User-chosen video format itag
  selectedAudioItag?: number;   // User-chosen audio format itag
}

/**
 * Format Selector
 */
export class FormatSelector {
  private logger: Logger;

  constructor() {
    this.logger = Logger.getInstance();
  }

  /**
   * Select formats with optional manual itag selection.
   * If a selected itag is found in the available formats, it is used.
   * Otherwise falls back to automatic best-quality selection.
   */
  selectWithOptions(formats: Format[], options?: FormatSelectionOptions): SelectedFormats {
    let video: Format | null = null;
    let audio: Format | null = null;

    // Try manual video selection
    if (options?.selectedVideoItag != null) {
      video = formats.find(
        (f) => f.itag === options.selectedVideoItag && f.mimeType?.includes("video") && f.url,
      ) || null;
      if (video) {
        this.logger.debug(
          `[FormatSelector] Manual video selection: itag ${video.itag} ${video.width}x${video.height}@${video.fps || "?"}fps`,
        );
      } else {
        this.logger.warn(
          `[FormatSelector] Manual video itag ${options.selectedVideoItag} not found, falling back to auto`,
        );
      }
    }

    // Try manual audio selection
    if (options?.selectedAudioItag != null) {
      audio = formats.find(
        (f) => f.itag === options.selectedAudioItag && f.mimeType?.includes("audio") && f.url,
      ) || null;
      if (audio) {
        this.logger.debug(
          `[FormatSelector] Manual audio selection: itag ${audio.itag} ${audio.bitrate}bps`,
        );
      } else {
        this.logger.warn(
          `[FormatSelector] Manual audio itag ${options.selectedAudioItag} not found, falling back to auto`,
        );
      }
    }

    // Fall back to auto for any unresolved selections
    // A selectedItag of -1 means explicitly "none" (video-only or audio-only)
    if (options?.selectedVideoItag === -1) {
      video = null; // Explicitly no video
    }
    if (options?.selectedAudioItag === -1) {
      audio = null; // Explicitly no audio
    }

    const auto = this.selectBest(formats);
    if (!video && options?.selectedVideoItag !== -1) video = auto.video;
    if (!audio && options?.selectedAudioItag !== -1) audio = auto.audio;

    return { video, audio };
  }

  /**
   * Select the best video and audio formats
   */
  selectBest(formats: Format[]): SelectedFormats {
    const config = ConfigManager.getInstance().get();
    const maxRes = config.downloader.max_video_resolution || 9999;
    const prefer60 = config.downloader.prefer_60fps ?? true;

    let bestVideo: Format | null = null;
    let bestAudio: Format | null = null;

    // Debug: log all available video formats with URLs
    const videoFormatsWithUrls = formats.filter(f => f.mimeType?.includes("video") && f.url);
    if (videoFormatsWithUrls.length > 0) {
      this.logger.debug(
        `[FormatSelector] Available video formats with URLs: ${videoFormatsWithUrls.map(f => `itag ${f.itag} (${f.width}x${f.height}@${f.fps || "?"}fps, ${f.bitrate}bps)`).join(", ")}`
      );
    }

    for (const f of formats) {
      const mimeType = f.mimeType || "";

      if (mimeType.includes("video")) {
        // Only consider formats with URLs
        if (!f.url) continue;

        // Check resolution limit using max(width, height) to handle portrait videos
        const maxDimension = Math.max(f.width || 0, f.height || 0);
        if (maxDimension > maxRes) continue;

        if (!bestVideo) {
          bestVideo = f;
          continue;
        }

        // Prefer higher resolution within limit
        const currentMaxDim = Math.max(
          bestVideo.width || 0,
          bestVideo.height || 0,
        );

        if (maxDimension > currentMaxDim) {
          bestVideo = f;
          continue;
        }

        if (maxDimension === currentMaxDim) {
          // Same resolution — fps preference is tiebreaker
          const fFps = f.fps || 30;
          const bestFps = bestVideo.fps || 30;

          if (fFps !== bestFps) {
            if (prefer60 && fFps > bestFps) bestVideo = f;
            if (!prefer60 && fFps < bestFps) bestVideo = f;
            continue;
          }

          // Same fps — fall through to bitrate
          if (f.bitrate > bestVideo.bitrate) {
            bestVideo = f;
          } else if (f.bitrate === bestVideo.bitrate) {
            // Same bitrate — prefer lower auth level
            if ((f.authLevel ?? 999) < (bestVideo.authLevel ?? 999)) {
              bestVideo = f;
            }
          }
        }
      } else if (mimeType.includes("audio")) {
        // Only consider formats with URLs
        if (!f.url) continue;

        // Prefer higher bitrate audio, with authLevel as tiebreaker
        if (!bestAudio || f.bitrate > bestAudio.bitrate) {
          bestAudio = f;
        } else if (f.bitrate === bestAudio.bitrate && (f.authLevel ?? 999) < (bestAudio.authLevel ?? 999)) {
          bestAudio = f;
        }
      }
    }

    if (bestVideo) {
      this.logger.debug(
        `[FormatSelector] Best video: ${bestVideo.width}x${bestVideo.height}@${bestVideo.fps || "?"}fps @ ${bestVideo.bitrate} (itag ${bestVideo.itag}, source: ${bestVideo.source || "unknown"})`,
      );
    }
    if (bestAudio) {
      this.logger.debug(
        `[FormatSelector] Best audio: ${bestAudio.bitrate} (itag ${bestAudio.itag}, source: ${bestAudio.source || "unknown"})`,
      );
    }

    return { video: bestVideo, audio: bestAudio };
  }

  /**
   * Filter formats to only those with direct URLs
   */
  filterWithUrls(formats: Format[]): Format[] {
    return formats.filter((f) => !!f.url);
  }

  /**
   * Get video formats only
   */
  getVideoFormats(formats: Format[]): Format[] {
    return formats.filter((f) => f.mimeType?.includes("video") && f.url);
  }

  /**
   * Get audio formats only
   */
  getAudioFormats(formats: Format[]): Format[] {
    return formats.filter((f) => f.mimeType?.includes("audio") && f.url);
  }

  /**
   * Sort formats by quality (best first)
   */
  sortByQuality(formats: Format[]): Format[] {
    return [...formats].sort((a, b) => {
      // Video: sort by resolution then bitrate
      if (a.height && b.height) {
        if (a.height !== b.height) return b.height - a.height;
      }
      // Audio or same resolution: sort by bitrate
      return b.bitrate - a.bitrate;
    });
  }
}
