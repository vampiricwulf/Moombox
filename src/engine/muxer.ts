import { spawn } from "child_process";
import path from "path";
import fs from "fs-extra";
import { Logger } from "../core/logger.js";

/**
 * Trim options for timestamp-based segment selection.
 * These apply FFmpeg -ss/-to flags during mux to trim segment boundaries.
 */
export interface MuxTrimOptions {
  /** Offset into the first segment where the actual start time falls (seconds) */
  trimStartOffset?: number;
  /** Duration of the output from the trim start (seconds) */
  trimDuration?: number;
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

    return new Promise((resolve, reject) => {
      // Don't use shell mode - pass args directly to avoid quoting issues
      const ffmpegArgs = ["-y"];

      // Apply trim offset before input (input-level seeking for codec copy)
      if (trimOptions?.trimStartOffset && trimOptions.trimStartOffset > 0) {
        ffmpegArgs.push("-ss", String(trimOptions.trimStartOffset));
      }

      ffmpegArgs.push("-i", normalizedVideoPath);

      if (normalizedAudioPath) {
        // Apply same seek to audio input for sync
        if (trimOptions?.trimStartOffset && trimOptions.trimStartOffset > 0) {
          ffmpegArgs.push("-ss", String(trimOptions.trimStartOffset));
        }
        ffmpegArgs.push("-i", normalizedAudioPath);
      }

      // Apply duration limit after inputs
      if (trimOptions?.trimDuration && trimOptions.trimDuration > 0) {
        ffmpegArgs.push("-t", String(trimOptions.trimDuration));
      }

      ffmpegArgs.push(
        "-c",
        "copy",
        "-movflags",
        "faststart",
        normalizedOutputPath,
      );

      logger.debug(
        `[Muxer] Running: ffmpeg ${ffmpegArgs.map((a) => `"${a}"`).join(" ")}`,
      );

      // Don't use shell mode - spawn handles arguments with spaces correctly when shell=false
      // Note: Do NOT use windowsVerbatimArguments as it bypasses proper quoting
      const ffmpeg = spawn("ffmpeg", ffmpegArgs, {
        stdio: ["ignore", "pipe", "pipe"],
      });

      let stderrOutput = "";
      let aborted = false;
      const MAX_STDERR = 10000;

      const onAbort = () => {
        aborted = true;
        ffmpeg.kill();
      };
      signal?.addEventListener("abort", onAbort, { once: true });

      ffmpeg.stderr?.on("data", (data) => {
        stderrOutput += data.toString();
        if (stderrOutput.length > MAX_STDERR) {
          stderrOutput = stderrOutput.slice(-MAX_STDERR / 2);
        }
      });

      ffmpeg.on("close", (code) => {
        signal?.removeEventListener("abort", onAbort);
        if (aborted) {
          reject(new DOMException("Aborted", "AbortError"));
        } else if (code === 0) {
          logger.debug(`[Muxer] FFmpeg completed successfully`);
          resolve();
        } else {
          logger.error(`[Muxer] FFmpeg stderr: ${stderrOutput.slice(-500)}`);
          reject(new Error(`FFmpeg exited with code ${code}`));
        }
      });

      ffmpeg.on("error", (err) => {
        signal?.removeEventListener("abort", onAbort);
        logger.error(`[Muxer] Failed to start FFmpeg: ${err.message}`);
        reject(new Error(`Failed to start FFmpeg: ${err.message}`));
      });
    });
  }
}
