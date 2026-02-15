/**
 * YouTube Service
 *
 * Main entry point for YouTube interactions.
 * Coordinates authentication, video info fetching, and format selection.
 */

import { Logger } from "../../core/logger.js";
import { ConfigManager } from "../../core/config.js";
import type { VideoInfo, VideoMetadata, Format } from "../../types/youtube.js";
import { YOUTUBE_URLS, USER_AGENTS, DEFAULT_API_KEY } from "../../constants.js";
import { YouTubeAuth } from "./auth.js";
import { WatchPageParser } from "./watchPage.js";
import { PlayerApi } from "./playerApi.js";
import { FormatSelector, type SelectedFormats, type FormatSelectionOptions } from "./formatSelector.js";
import { PoTokenGenerator } from "./poToken.js";
import { fetchWithTimeout } from "../../core/http.js";
import { getErrorMessage } from "../../types/errors.js";

// Re-export sub-modules for direct access if needed
export { YouTubeAuth } from "./auth.js";
export { WatchPageParser } from "./watchPage.js";
export { PlayerApi } from "./playerApi.js";
export { FormatSelector } from "./formatSelector.js";
export { PoTokenGenerator } from "./poToken.js";

// Re-export types
export type { SelectedFormats, FormatSelectionOptions } from "./formatSelector.js";

/**
 * YouTube Service - Main facade for all YouTube operations
 */
export class YouTubeService {
  private static instance: YouTubeService;

  private logger: Logger;
  private auth: YouTubeAuth;
  private playerApi: PlayerApi;
  private formatSelector: FormatSelector;
  private poTokenGen: PoTokenGenerator;
  private watchPageParser: WatchPageParser;

  private initialized: boolean = false;
  private initPromise: Promise<void> | null = null;
  private visitorData: string = "";
  private innertubeApiKey: string = DEFAULT_API_KEY;

  constructor() {
    this.logger = Logger.getInstance();
    this.auth = new YouTubeAuth();
    this.playerApi = new PlayerApi(this.auth);
    this.formatSelector = new FormatSelector();
    this.poTokenGen = new PoTokenGenerator();
    this.watchPageParser = new WatchPageParser();
  }

  /**
   * Get the singleton instance
   */
  static getInstance(): YouTubeService {
    if (!YouTubeService.instance) {
      YouTubeService.instance = new YouTubeService();
    }
    return YouTubeService.instance;
  }

  /**
   * Initialize the service
   */
  async init(): Promise<void> {
    if (this.initialized) return;
    if (this.initPromise) return this.initPromise;
    this.initPromise = this.doInit().catch((e) => {
      this.initPromise = null;
      throw e;
    });
    return this.initPromise;
  }

  private async doInit(): Promise<void> {

    // Load cookies
    await this.auth.loadCookies();

    // Fetch visitor data and API key from YouTube homepage
    try {
      const response = await fetchWithTimeout(YOUTUBE_URLS.BASE, {
        headers: {
          "User-Agent": USER_AGENTS.WEB,
          ...(this.auth.getCookieHeader()
            ? { Cookie: this.auth.getCookieHeader() }
            : {}),
        },
      });
      const html = await response.text();

      // Extract VISITOR_DATA
      const visitorMatch =
        html.match(/"visitorData":"([^"]+)"/) ||
        html.match(/"visitor_data":"([^"]+)"/);
      if (visitorMatch) {
        this.visitorData = visitorMatch[1];
        this.poTokenGen.setVisitorData(this.visitorData);
        this.logger.debug(
          `[YouTube] Visitor data: ${this.visitorData.substring(0, 30)}...`,
        );
      }

      // Extract INNERTUBE_API_KEY
      const apiKeyMatch = html.match(/"INNERTUBE_API_KEY":"([^"]+)"/);
      if (apiKeyMatch) {
        this.innertubeApiKey = apiKeyMatch[1];
        this.playerApi.setApiKey(this.innertubeApiKey);
        this.logger.debug(`[YouTube] API key: ${this.innertubeApiKey}`);
      }
    } catch (e) {
      const msg = getErrorMessage(e);
      this.logger.warn(`[YouTube] Failed to fetch homepage: ${msg}`);
    }

    // Generate PO Token
    await this.poTokenGen.generate();

    this.initialized = true;
  }

  /**
   * Get complete video information
   */
  async getVideoInfo(videoId: string): Promise<VideoInfo> {
    if (!this.initialized) await this.init();

    // Sync cookies (may have been refreshed)
    await this.auth.syncCookies();

    // Use authenticated path if cookies available
    if (this.auth.getCookieHeader()) {
      this.logger.debug(
        `[YouTube] Using authenticated clients for ${videoId} (cookies available)`,
      );
      return this.playerApi.getVideoInfoAuthenticated(videoId);
    }

    // No cookies - use public client
    this.logger.debug(
      `[YouTube] Using public client for ${videoId} (no cookies)`,
    );
    return this.playerApi.getVideoInfoPublic(videoId);
  }

  /**
   * Lightweight probe using ANDROID_VR — no cookies, no watch page, no cipher.
   * Returns status + metadata. Used for polling upcoming streams.
   */
  async probeVideoStatus(videoId: string): Promise<VideoInfo> {
    if (!this.initialized) await this.init();
    return this.playerApi.probeVideoStatus(videoId, this.visitorData);
  }

  /**
   * Lightweight authenticated probe using TV_DOWNGRADED with cookies.
   * Single API call — no watch page, no STS, no cipher.
   * Used for polling members-only upcoming streams.
   */
  async probeVideoStatusAuthenticated(videoId: string): Promise<VideoInfo> {
    if (!this.initialized) await this.init();
    return this.playerApi.probeVideoStatusAuthenticated(videoId, this.visitorData);
  }

  /**
   * Get basic video metadata (lighter than full VideoInfo)
   */
  async getMetadata(videoId: string): Promise<VideoMetadata> {
    const info = await this.getVideoInfo(videoId);
    return {
      title: info.title,
      channelName: info.channelName,
      channelId: info.channelId,
      thumbnailUrl: info.thumbnailUrl,
      streamStatus: info.streamStatus,
      isLive: info.isLive,
      isUpcoming: info.isUpcoming,
    };
  }

  /**
   * Select best formats from available options
   */
  getBestFormats(formats: Format[], options?: FormatSelectionOptions): SelectedFormats {
    if (options?.selectedVideoItag != null || options?.selectedAudioItag != null) {
      return this.formatSelector.selectWithOptions(formats, options);
    }
    return this.formatSelector.selectBest(formats);
  }

  /**
   * Decrypt n parameter in DASH manifest URL
   */
  async decryptDashManifestUrl(
    dashUrl: string,
    playerUrl: string,
  ): Promise<string> {
    return this.playerApi.decryptDashManifestUrl(dashUrl, playerUrl);
  }

  /**
   * Decrypt n parameter in any URL (used for segment BaseURLs)
   */
  async decryptNParamInUrl(url: string, playerUrl: string): Promise<string> {
    return this.playerApi.decryptNParamInUrl(url, playerUrl);
  }

  /**
   * Force reload cookies from file
   */
  async reloadCookies(): Promise<void> {
    await this.auth.reloadCookies();
  }

  /**
   * Check if authenticated
   */
  hasAuthCookies(): boolean {
    return this.auth.hasAuthCookies();
  }

  /**
   * Get the current PO token
   */
  getPoToken(): string {
    return this.poTokenGen.getPoToken();
  }

  /**
   * Get the current visitor data
   */
  getVisitorData(): string {
    return this.visitorData;
  }

  /**
   * Get authentication state for status display
   */
  getAuthState(): { isLoggedIn: boolean; hasCookies: boolean } {
    return {
      isLoggedIn: this.auth.hasAuthCookies(),
      hasCookies: !!this.auth.getCookieHeader(),
    };
  }

  /**
   * Fetch watch page HTML for a video (used for chat continuation extraction)
   */
  async fetchWatchPageHtml(videoId: string): Promise<string> {
    if (!this.initialized) await this.init();

    const { html } = await this.watchPageParser.fetch(
      videoId,
      this.auth.getCookieHeader() || undefined,
    );
    return html;
  }

  /**
   * Get the current API key
   */
  getApiKey(): string {
    return this.innertubeApiKey;
  }

  /**
   * Get the current cookie header (for authenticated requests)
   */
  getCookieHeader(): string | undefined {
    return this.auth.getCookieHeader() || undefined;
  }
}

// Default export for convenience
export default YouTubeService;
