/**
 * YouTube Authentication Module
 *
 * Handles cookie management, SAPISIDHASH generation, and auth headers.
 */

import { CookieJar } from "../../core/cookies.js";
import { ConfigManager } from "../../core/config.js";
import { Logger } from "../../core/logger.js";
import type { YouTubeClientConfig, YtcfgData } from "../../types/youtube.js";

/**
 * YouTube Authentication Handler
 */
export class YouTubeAuth {
  private logger: Logger;
  private cookieHeader: string = "";

  constructor() {
    this.logger = Logger.getInstance();
  }

  /**
   * Load cookies from file
   */
  async loadCookies(): Promise<void> {
    const config = ConfigManager.getInstance().get();

    if (config.downloader.cookie_file) {
      try {
        this.cookieHeader = await CookieJar.load(config.downloader.cookie_file);
        this.logger.info("[YouTubeAuth] Loaded cookies from file");
      } catch (e) {
        const msg = e instanceof Error ? e.message : String(e);
        this.logger.warn(`[YouTubeAuth] Failed to load cookies: ${msg}`);
      }
    }
  }

  /**
   * Reload cookies from file (for retry scenarios)
   */
  async reloadCookies(): Promise<void> {
    const config = ConfigManager.getInstance().get();

    if (config.downloader.cookie_file) {
      try {
        this.cookieHeader = await CookieJar.load(
          config.downloader.cookie_file,
          true,
        );
        this.logger.info("[YouTubeAuth] Reloaded cookies from file");
      } catch (e) {
        const msg = e instanceof Error ? e.message : String(e);
        this.logger.warn(`[YouTubeAuth] Failed to reload cookies: ${msg}`);
      }
    }
  }

  /**
   * Sync cookies from CookieJar (picks up refreshed cookies)
   */
  async syncCookies(): Promise<void> {
    const config = ConfigManager.getInstance().get();
    if (config.downloader.cookie_file) {
      const newCookieHeader = await CookieJar.load(config.downloader.cookie_file);
      if (newCookieHeader !== this.cookieHeader) {
        this.cookieHeader = newCookieHeader;
        this.logger.debug("[YouTubeAuth] Synced cookies from CookieJar");
      }
    }
  }

  /**
   * Check if we have valid authentication cookies
   */
  hasAuthCookies(): boolean {
    return !!this.cookieHeader && CookieJar.hasAuthCookies();
  }

  /**
   * Get the cookie header string
   */
  getCookieHeader(): string {
    return this.cookieHeader;
  }

  /**
   * Generate API headers for YouTube requests
   */
  generateApiHeaders(
    client: YouTubeClientConfig,
    ytcfg: YtcfgData,
  ): Record<string, string> {
    const origin = "https://www.youtube.com";

    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      "User-Agent": client.userAgent,
      "X-YouTube-Client-Name": client.clientId,
      "X-YouTube-Client-Version": client.context.clientVersion,
      Origin: origin,
      Cookie: this.cookieHeader,
    };

    // Add visitor ID if available
    if (ytcfg.visitorData) {
      headers["X-Goog-Visitor-Id"] = ytcfg.visitorData;
    }

    // Add delegated session ID (for secondary channels)
    if (ytcfg.delegatedSessionId) {
      headers["X-Goog-PageId"] = ytcfg.delegatedSessionId;
    }

    // Add auth user (session index)
    if (ytcfg.delegatedSessionId || ytcfg.sessionIndex !== null) {
      headers["X-Goog-AuthUser"] = String(ytcfg.sessionIndex ?? 0);
    }

    // Add authorization header with SAPISIDHASH
    const authHeader = CookieJar.generateAuthorizationHeader(origin);
    if (authHeader) {
      headers["Authorization"] = authHeader;
      headers["X-Origin"] = origin;
    }

    // Indicate logged in status
    if (CookieJar.hasAuthCookies()) {
      headers["X-Youtube-Bootstrap-Logged-In"] = "true";
    }

    this.logger.debug(
      `[YouTubeAuth] API headers - Auth: ${authHeader ? "yes" : "no"}, ` +
        `VisitorId: ${headers["X-Goog-Visitor-Id"] ? "yes" : "no"}, ` +
        `AuthUser: ${headers["X-Goog-AuthUser"] ?? "none"}, ` +
        `PageId: ${headers["X-Goog-PageId"] ? "yes" : "no"}, ` +
        `LoggedIn: ${headers["X-Youtube-Bootstrap-Logged-In"] || "no"}`,
    );

    return headers;
  }
}
