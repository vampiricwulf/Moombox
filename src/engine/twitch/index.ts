/**
 * Twitch Service
 *
 * Singleton facade for all Twitch operations.
 * Coordinates GQL API, authentication, HLS access, and stream status.
 */

import { Logger } from "../../core/logger.js";
import { ConfigManager } from "../../core/config.js";
import { getErrorMessage } from "../../types/errors.js";
import type {
  TwitchStreamInfo,
  TwitchVodInfo,
  TwitchHlsVariant,
} from "../../types/twitch.js";
import { readTwitchAuthToken } from "./twitchAuth.js";
import * as twitchApi from "./twitchApi.js";

// Re-export sub-modules
export { twitchApi };
export { generateTwitchHeaders, readTwitchAuthToken } from "./twitchAuth.js";
export { TwitchVodChatDownloader } from "./twitchVodChat.js";

/**
 * Twitch Service — singleton facade
 */
export class TwitchService {
  private static instance: TwitchService;

  private logger: Logger;
  private authToken: string | null = null;
  private initialized = false;

  private constructor() {
    this.logger = Logger.getInstance();
  }

  static getInstance(): TwitchService {
    if (!TwitchService.instance) {
      TwitchService.instance = new TwitchService();
    }
    return TwitchService.instance;
  }

  /**
   * Initialize the service — load auth token from cookie file
   */
  async init(): Promise<void> {
    if (this.initialized) return;

    try {
      const config = ConfigManager.getInstance().get();
      const cookiePath = config.downloader.cookie_file || "./cookies.txt";
      this.authToken = await readTwitchAuthToken(cookiePath);

      if (this.authToken) {
        this.logger.info("[Twitch] Authenticated (auth-token found in cookies)");
      } else {
        this.logger.debug("[Twitch] No auth-token found, using anonymous access");
      }
    } catch (e) {
      this.logger.warn(`[Twitch] Failed to load auth token: ${getErrorMessage(e)}`);
    }

    this.initialized = true;
  }

  /**
   * Reload auth token from cookie file
   */
  async reloadAuth(): Promise<void> {
    try {
      const config = ConfigManager.getInstance().get();
      const cookiePath = config.downloader.cookie_file || "./cookies.txt";
      this.authToken = await readTwitchAuthToken(cookiePath);
    } catch {
      // Ignore reload failures
    }
  }

  /**
   * Get stream info for a channel. Returns null if offline.
   */
  async getStreamInfo(channelLogin: string): Promise<TwitchStreamInfo | null> {
    if (!this.initialized) await this.init();
    return twitchApi.getStreamInfo(channelLogin, this.authToken);
  }

  /**
   * Get VOD info.
   */
  async getVodInfo(vodId: string): Promise<TwitchVodInfo> {
    if (!this.initialized) await this.init();
    return twitchApi.getVodInfo(vodId, this.authToken);
  }

  /**
   * Get HLS master playlist variants for a live stream.
   */
  async getHlsMasterPlaylist(channelLogin: string): Promise<TwitchHlsVariant[]> {
    if (!this.initialized) await this.init();
    return twitchApi.getHlsMasterPlaylist(channelLogin, this.authToken);
  }

  /**
   * Get HLS playlist variants for a VOD.
   */
  async getVodHlsPlaylist(vodId: string): Promise<TwitchHlsVariant[]> {
    if (!this.initialized) await this.init();
    return twitchApi.getVodHlsPlaylist(vodId, this.authToken);
  }

  /**
   * Fetch VOD chat comments (replay).
   */
  async getVodComments(vodId: string, contentOffsetSeconds: number) {
    if (!this.initialized) await this.init();
    return twitchApi.getVodComments(vodId, contentOffsetSeconds, this.authToken);
  }

  /**
   * Check if we have a Twitch auth token.
   */
  hasAuthToken(): boolean {
    return !!this.authToken;
  }

  /**
   * Get the current auth token (for external use like IRC).
   */
  getAuthToken(): string | null {
    return this.authToken;
  }

  /**
   * Select the best HLS variant based on quality preference and resolution cap.
   */
  selectBestVariant(
    variants: TwitchHlsVariant[],
    quality_preference?: string,
    maxResolution?: number,
  ): TwitchHlsVariant | null {
    if (variants.length === 0) return null;

    // Handle explicit quality preferences
    if (quality_preference === "audio_only") {
      return variants.find(v => v.name === "audio_only") || variants[variants.length - 1];
    }

    // Filter by max resolution
    let filtered = variants.filter(v => v.name !== "audio_only");
    if (maxResolution && filtered.length > 0) {
      const withinCap = filtered.filter(v => {
        if (!v.height) return true;
        // Use max of width/height for portrait streams
        const maxDim = Math.max(v.width || 0, v.height);
        return maxDim <= maxResolution;
      });
      if (withinCap.length > 0) filtered = withinCap;
    }

    // Handle specific quality targets
    if (quality_preference === "720p") {
      const match = filtered.find(v => v.height === 720);
      if (match) return match;
    }
    if (quality_preference === "480p") {
      const match = filtered.find(v => v.height === 480);
      if (match) return match;
    }

    // Default: prefer source quality (chunked), then highest bandwidth
    const source = filtered.find(v => v.isSource);
    if (source) return source;

    // Sort by bandwidth descending
    filtered.sort((a, b) => b.bandwidth - a.bandwidth);
    return filtered[0] || null;
  }

  /**
   * Get auth state for status display.
   */
  getAuthState(): { isAuthenticated: boolean } {
    return { isAuthenticated: !!this.authToken };
  }
}

export default TwitchService;
