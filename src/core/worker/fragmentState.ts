/**
 * Fragment State Manager
 *
 * Manages download state persistence for crash recovery.
 * Based on yt-dlp's fragment state tracking pattern.
 */

import fs from "fs-extra";
import path from "path";

/**
 * Information about a single fragment
 */
export interface FragmentInfo {
  index: number;
  url: string;
  downloaded: boolean;
  size?: number;
  timestamp?: number;
}

/**
 * Fragment State Manager - persists download progress for resume capability
 */
export class FragmentStateManager {
  private stateFilePath: string;
  private fragments: Map<number, FragmentInfo> = new Map();

  constructor(jobId: string, stagingDir: string) {
    this.stateFilePath = path.join(stagingDir, `${jobId}.ytdl`);
    this.load();
  }

  /**
   * Load fragment state from disk
   */
  private load(): void {
    if (!fs.existsSync(this.stateFilePath)) {
      return;
    }

    try {
      const data = fs.readFileSync(this.stateFilePath, "utf-8");
      const parsed = JSON.parse(data) as Record<string, FragmentInfo>;

      this.fragments = new Map(
        Object.entries(parsed).map(([k, v]) => [parseInt(k, 10), v])
      );
    } catch {
      // Corrupted state file - ignore and start fresh
      this.fragments.clear();
    }
  }

  /**
   * Save fragment state to disk
   */
  save(): void {
    const data = Object.fromEntries(this.fragments);
    fs.writeFileSync(this.stateFilePath, JSON.stringify(data, null, 2));
  }

  /**
   * Mark a fragment as downloaded
   */
  markDownloaded(index: number, url: string, size: number): void {
    this.fragments.set(index, {
      index,
      url,
      downloaded: true,
      size,
      timestamp: Date.now(),
    });
  }

  /**
   * Check if a fragment is already downloaded
   */
  isDownloaded(index: number): boolean {
    return this.fragments.get(index)?.downloaded ?? false;
  }

  /**
   * Get all downloaded fragment indices
   */
  getDownloadedIndices(): number[] {
    return Array.from(this.fragments.values())
      .filter(f => f.downloaded)
      .map(f => f.index)
      .sort((a, b) => a - b);
  }

  /**
   * Get total downloaded fragments count
   */
  getDownloadedCount(): number {
    return Array.from(this.fragments.values()).filter(f => f.downloaded).length;
  }

  /**
   * Clean up state file (call on successful completion)
   */
  cleanup(): void {
    if (fs.existsSync(this.stateFilePath)) {
      fs.unlinkSync(this.stateFilePath);
    }
  }
}
