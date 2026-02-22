/**
 * Asset Downloader
 *
 * Downloads supplementary assets like thumbnails and descriptions.
 */

import fs from "fs-extra";
import path from "path";
import { Logger } from "../logger.js";
import { THUMBNAIL_QUALITIES, YOUTUBE_URLS } from "../../constants.js";
import { fetchWithTimeout } from "../http.js";
import { getErrorMessage } from "../../types/errors.js";

/**
 * Asset Downloader
 */
export class AssetDownloader {
  private logger: Logger;

  constructor() {
    this.logger = Logger.getInstance();
  }

  /**
   * Save video description to file
   */
  async saveDescription(
    description: string,
    outputDir: string,
    filenameBase: string,
  ): Promise<boolean> {
    if (!description) return false;

    const descriptionPath = path.join(outputDir, `${filenameBase}.description`);

    try {
      await fs.writeFile(descriptionPath, description, "utf-8");
      this.logger.info(`[AssetDownloader] Saved description: ${descriptionPath}`);
      return true;
    } catch (e) {
      const msg = getErrorMessage(e);
      this.logger.warn(`[AssetDownloader] Failed to save description: ${msg}`);
      return false;
    }
  }

  /**
   * Download and save thumbnail image
   */
  async downloadThumbnail(
    thumbnailUrl: string,
    outputDir: string,
    filenameBase: string,
  ): Promise<boolean> {
    if (!thumbnailUrl) return false;

    try {
      // Extract video ID from thumbnail URL to try different quality levels
      const match = thumbnailUrl.match(/\/vi\/([a-zA-Z0-9_-]+)\//);

      if (!match) {
        // Just try the original URL
        return this.downloadThumbnailFromUrl(
          thumbnailUrl,
          outputDir,
          filenameBase,
        );
      }

      const videoId = match[1];

      // Try different quality levels in order of preference
      for (const quality of THUMBNAIL_QUALITIES) {
        const url = `${YOUTUBE_URLS.THUMBNAIL}/${videoId}/${quality}.jpg`;

        try {
          const response = await fetchWithTimeout(url, undefined, 15_000);
          if (response.ok) {
            const data = await response.arrayBuffer();
            // Check if we got a valid image (YouTube returns a tiny placeholder for missing thumbnails)
            if (data.byteLength > 1000) {
              const outputPath = path.join(outputDir, `${filenameBase}.jpg`);
              await fs.writeFile(outputPath, Buffer.from(data));
              this.logger.debug(
                `[AssetDownloader] Downloaded thumbnail (${quality}): ${outputPath}`,
              );
              return true;
            }
          }
        } catch {
          // Try next quality
        }
      }

      this.logger.warn(
        "[AssetDownloader] No valid thumbnail found at any quality level",
      );
      return false;
    } catch (e) {
      const msg = getErrorMessage(e);
      this.logger.warn(`[AssetDownloader] Failed to download thumbnail: ${msg}`);
      return false;
    }
  }

  /**
   * Download thumbnail from a specific URL
   */
  private async downloadThumbnailFromUrl(
    url: string,
    outputDir: string,
    filenameBase: string,
  ): Promise<boolean> {
    const response = await fetchWithTimeout(url, undefined, 15_000);
    if (!response.ok) {
      await response.body?.cancel();
      throw new Error(`HTTP ${response.status}`);
    }

    const data = await response.arrayBuffer();
    const ext = url.includes(".webp") ? ".webp" : ".jpg";
    const outputPath = path.join(outputDir, `${filenameBase}${ext}`);
    await fs.writeFile(outputPath, Buffer.from(data));

    this.logger.debug(`[AssetDownloader] Downloaded thumbnail: ${outputPath}`);
    return true;
  }
}
