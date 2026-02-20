/**
 * FFprobe metadata extraction utility.
 * Consolidates 2 duplicate implementations.
 */

import { execa } from "execa";

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
  try {
    const { stdout } = await execa("ffprobe", [
      "-v",
      "quiet",
      "-print_format",
      "json",
      "-show_format",
      "-show_streams",
      filePath,
    ], { timeout: 30_000 });

    if (!stdout) {
      return null;
    }

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

    return metadata;
  } catch (error) {
    return null;
  }
}
