/**
 * FFprobe metadata extraction utility.
 * Consolidates 2 duplicate implementations.
 */

import { spawn } from "child_process";

export interface VideoMetadata {
  duration: number;
  width?: number;
  height?: number;
  size: number;
}

/**
 * Extract video metadata using ffprobe.
 * @param filePath - Path to video file
 * @returns Metadata object or null if extraction fails
 */
export async function extractVideoMetadata(
  filePath: string,
): Promise<VideoMetadata | null> {
  return new Promise((resolve) => {
    const args = [
      "-v",
      "quiet",
      "-print_format",
      "json",
      "-show_format",
      "-show_streams",
      filePath,
    ];

    const ffprobe = spawn("ffprobe", args);

    let stdout = "";
    let stderr = "";

    ffprobe.stdout.on("data", (chunk) => {
      stdout += chunk.toString();
    });

    ffprobe.stderr.on("data", (chunk) => {
      stderr += chunk.toString();
    });

    ffprobe.on("close", (code) => {
      if (code !== 0 || !stdout) {
        resolve(null);
        return;
      }

      try {
        const data = JSON.parse(stdout);
        const format = data.format || {};
        const videoStream = (data.streams || []).find(
          (s: any) => s.codec_type === "video",
        );

        const metadata: VideoMetadata = {
          duration: parseFloat(format.duration) || 0,
          width: videoStream?.width,
          height: videoStream?.height,
          size: parseInt(format.size, 10) || 0,
        };

        resolve(metadata);
      } catch (e) {
        resolve(null);
      }
    });

    ffprobe.on("error", () => {
      resolve(null);
    });
  });
}
