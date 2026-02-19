import os from "node:os";
import ms from "ms";
import { execa } from "execa";
import path from "path";
import fs from "fs-extra";
import { Logger } from "../core/logger.js";
import { getErrorMessage } from "../types/errors.js";

/**
 * Trim options for timestamp-based segment selection.
 * These apply FFmpeg -ss/-to flags during mux to trim segment boundaries.
 */
export interface MuxTrimOptions {
  /** Offset into the first segment where the actual start time falls (seconds) */
  trimStartOffset?: number;
  /** Duration of the output from the trim start (seconds) */
  trimDuration?: number;
  /** Video bitrate for precise re-encode (kbps) — used for ABR modes */
  videoBitrate?: number;
  /** Audio bitrate for precise re-encode (kbps) */
  audioBitrate?: number;
  /** CRF value for quality-based encoding (0-51, lower = better, 18 = perceptually lossless) */
  crf?: number;
  /** Use precise trim (re-encode for exact duration) - default true */
  usePreciseTrim?: boolean;
  /** Use 2-pass encoding (only for ABR mode, not CRF) */
  twoPass?: boolean;
}

export class Muxer {
  static async mux(
    videoPath: string,
    audioPath: string | null,
    outputPath: string,
    signal?: AbortSignal,
    trimOptions?: MuxTrimOptions,
  ): Promise<void> {
    const logger = Logger.getInstance();

    if (signal?.aborted) {
      throw new DOMException("Aborted", "AbortError");
    }

    // Normalize paths for the current platform
    const normalizedVideoPath = path.resolve(videoPath);
    const normalizedAudioPath = audioPath ? path.resolve(audioPath) : null;
    const normalizedOutputPath = path.resolve(outputPath);

    // Verify input files exist
    if (!(await fs.pathExists(normalizedVideoPath))) {
      throw new Error(`Video file not found: ${normalizedVideoPath}`);
    }
    if (normalizedAudioPath && !(await fs.pathExists(normalizedAudioPath))) {
      throw new Error(`Audio file not found: ${normalizedAudioPath}`);
    }

    // Ensure output directory exists
    await fs.ensureDir(path.dirname(normalizedOutputPath));

    // Decide: codec copy (fast but imprecise) vs re-encode (precise)
    const hasTrim = trimOptions?.trimStartOffset || trimOptions?.trimDuration;
    const usePrecise =
      trimOptions?.usePreciseTrim !== false && hasTrim &&
      (trimOptions?.crf != null || trimOptions?.videoBitrate || trimOptions?.audioBitrate);

    if (usePrecise && trimOptions?.twoPass && trimOptions.videoBitrate) {
      // 2-pass ABR re-encode (for target bitrate with optimal distribution)
      await this.runTwoPassEncode(
        normalizedVideoPath,
        normalizedAudioPath,
        normalizedOutputPath,
        trimOptions,
        signal,
      );
    } else {
      // Build FFmpeg arguments for single-pass paths
      const ffmpegArgs = this.buildInputArgs(
        normalizedVideoPath,
        normalizedAudioPath,
        trimOptions ?? {},
      );

      if (usePrecise) {
        // Single-pass re-encode for exact duration
        this.appendEncodeArgs(ffmpegArgs, normalizedAudioPath, trimOptions!);
        ffmpegArgs.push("-movflags", "faststart", normalizedOutputPath);
      } else {
        // Standard codec copy (fast)
        ffmpegArgs.push(
          "-c",
          "copy",
          "-movflags",
          "faststart",
          normalizedOutputPath,
        );
      }

      await this.runFFmpeg(ffmpegArgs, signal);
    }
  }

  /**
   * Run a 2-pass x264 encode for better bitrate distribution
   */
  private static async runTwoPassEncode(
    videoPath: string,
    audioPath: string | null,
    outputPath: string,
    trimOptions: MuxTrimOptions,
    signal?: AbortSignal,
  ): Promise<void> {
    const logger = Logger.getInstance();
    const passlogPrefix = path.join(
      os.tmpdir(),
      `moombox-2pass-${Date.now()}`,
    );

    try {
      // === Pass 1: Analysis (no audio, output to null) ===
      logger.info("[Muxer] 2-pass encode: starting pass 1 (analysis)");

      const pass1Args = this.buildInputArgs(videoPath, audioPath, trimOptions);
      pass1Args.push(
        "-c:v", "libx264",
        "-b:v", `${trimOptions.videoBitrate}k`,
        "-preset", "fast",
        "-pass", "1",
        "-passlogfile", passlogPrefix,
        "-an",
        "-f", "null",
        os.devNull,
      );

      await this.runFFmpeg(pass1Args, signal);

      // Check abort between passes
      if (signal?.aborted) {
        throw new DOMException("Aborted", "AbortError");
      }

      // === Pass 2: Encode with optimized bitrate distribution ===
      logger.info("[Muxer] 2-pass encode: starting pass 2 (encode)");

      const pass2Args = this.buildInputArgs(videoPath, audioPath, trimOptions);
      pass2Args.push(
        "-c:v", "libx264",
        "-b:v", `${trimOptions.videoBitrate}k`,
        "-preset", "fast",
        "-pass", "2",
        "-passlogfile", passlogPrefix,
      );

      // Audio encoding (same logic as single-pass)
      if (audioPath && trimOptions.audioBitrate) {
        pass2Args.push("-c:a", "aac", "-b:a", `${trimOptions.audioBitrate}k`);
      } else if (audioPath) {
        pass2Args.push("-c:a", "copy");
      }

      pass2Args.push("-movflags", "faststart", outputPath);

      await this.runFFmpeg(pass2Args, signal);

      logger.info("[Muxer] 2-pass encode: complete");
    } finally {
      // Clean up passlog files
      for (const suffix of ["-0.log", "-0.log.mbtree"]) {
        try {
          await fs.remove(passlogPrefix + suffix);
        } catch {
          // Ignore cleanup errors
        }
      }
    }
  }

  /**
   * Build common input arguments (seek, inputs, duration)
   */
  private static buildInputArgs(
    videoPath: string,
    audioPath: string | null,
    trimOptions: MuxTrimOptions,
  ): string[] {
    const args = ["-y"];

    if (trimOptions.trimStartOffset && trimOptions.trimStartOffset > 0) {
      args.push("-ss", String(trimOptions.trimStartOffset));
    }

    args.push("-i", videoPath);

    if (audioPath) {
      if (trimOptions.trimStartOffset && trimOptions.trimStartOffset > 0) {
        args.push("-ss", String(trimOptions.trimStartOffset));
      }
      args.push("-i", audioPath);
    }

    if (trimOptions.trimDuration && trimOptions.trimDuration > 0) {
      args.push("-t", String(trimOptions.trimDuration));
    }

    return args;
  }

  /**
   * Append single-pass video/audio encode arguments
   */
  private static appendEncodeArgs(
    args: string[],
    audioPath: string | null,
    trimOptions: MuxTrimOptions,
  ): void {
    const logger = Logger.getInstance();

    if (trimOptions.crf != null) {
      // CRF mode: quality-based encoding — x264 decides bitrate per-frame
      logger.info(`[Muxer] Using CRF ${trimOptions.crf} encode (quality-based, slow preset)`);
      args.push(
        "-c:v", "libx264",
        "-crf", String(trimOptions.crf),
        "-preset", "slow",
      );
      // Re-encode audio at source bitrate to keep sync with re-encoded video
      if (trimOptions.audioBitrate) {
        args.push("-c:a", "aac", "-b:a", `${trimOptions.audioBitrate}k`);
      } else {
        args.push("-c:a", "aac");
      }
    } else {
      // ABR mode: target bitrate encoding
      logger.info("[Muxer] Using precise trim (re-encode for exact duration)");

      if (trimOptions.videoBitrate) {
        args.push(
          "-c:v", "libx264",
          "-b:v", `${trimOptions.videoBitrate}k`,
          "-preset", "fast",
        );
      } else {
        args.push("-c:v", "copy");
      }

      if (audioPath && trimOptions.audioBitrate) {
        args.push("-c:a", "aac", "-b:a", `${trimOptions.audioBitrate}k`);
      } else if (audioPath) {
        args.push("-c:a", "copy");
      }
    }
  }

  /**
   * Execute FFmpeg with standard error handling
   */
  private static async runFFmpeg(
    args: string[],
    signal?: AbortSignal,
  ): Promise<void> {
    const logger = Logger.getInstance();
    logger.debug(
      `[Muxer] Running: ffmpeg ${args.map((a) => `"${a}"`).join(" ")}`,
    );

    try {
      await execa("ffmpeg", args, {
        stdin: "ignore",
        cancelSignal: signal,
        timeout: ms("10m"),
        cleanup: true,
      });

      logger.debug(`[Muxer] FFmpeg completed successfully`);
    } catch (error: unknown) {
      const execaErr = error as { isCanceled?: boolean; name?: string; stderr?: string; exitCode?: number; message?: string };
      if (execaErr.isCanceled || execaErr.name === 'AbortError') {
        throw new DOMException("Aborted", "AbortError");
      }

      const stderr = execaErr.stderr || getErrorMessage(error);
      logger.error(`[Muxer] FFmpeg failed: ${stderr.slice(-500)}`);
      throw new Error(`FFmpeg exited with code ${execaErr.exitCode || 'unknown'}: ${stderr.slice(-200)}`);
    }
  }
}
