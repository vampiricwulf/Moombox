/**
 * YouTube Watch Page Parser
 *
 * Extracts configuration and metadata from YouTube watch pages.
 */

import { Logger } from "../../core/logger.js";
import type { YtcfgData } from "../../types/youtube.js";
import { USER_AGENTS, YOUTUBE_URLS } from "../../constants.js";
import { fetchWithTimeout } from "../../core/http.js";

/**
 * Watch Page Parser
 */
export class WatchPageParser {
  private logger: Logger;

  constructor() {
    this.logger = Logger.getInstance();
  }

  /**
   * Fetch and parse a YouTube watch page
   */
  async fetch(
    videoId: string,
    cookieHeader?: string,
  ): Promise<{ html: string; ytcfg: YtcfgData; isLoggedIn: boolean; playerResponse: any | null }> {
    const url = `${YOUTUBE_URLS.WATCH}?v=${videoId}`;

    this.logger.debug(
      `[WatchPage] Fetching watch page for ${videoId}${cookieHeader ? " with cookies" : ""}`,
    );

    const response = await fetchWithTimeout(url, {
      headers: {
        "User-Agent": USER_AGENTS.WEB,
        Accept:
          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
        "Accept-Language": "en-US,en;q=0.5",
        ...(cookieHeader ? { Cookie: cookieHeader } : {}),
      },
    });

    if (!response.ok) {
      throw new Error(`Failed to fetch watch page: HTTP ${response.status}`);
    }

    const html = await response.text();
    const isLoggedIn =
      html.includes('"LOGGED_IN":true') || html.includes('"isLoggedIn":true');
    const { ytcfg, playerResponse } = this.extractYtcfgAndPlayerResponse(html);

    this.logger.debug(`[WatchPage] Watch page logged in status: ${isLoggedIn}`);
    this.logger.debug(
      `[WatchPage] Extracted ytcfg - playerUrl: ${ytcfg.playerUrl ? "yes" : "no"}, ` +
        `visitorData: ${ytcfg.visitorData ? "yes" : "no"}, ` +
        `sessionIndex: ${ytcfg.sessionIndex}, ` +
        `delegatedSessionId: ${ytcfg.delegatedSessionId ? "yes" : "no"}, ` +
        `dataSyncId: ${ytcfg.dataSyncId ? "yes" : "no"}, ` +
        `playerResponse: ${playerResponse ? "yes" : "no"}`,
    );

    return { html, ytcfg, isLoggedIn, playerResponse };
  }

  /**
   * Extract ytcfg configuration from watch page HTML
   */
  extractYtcfg(html: string): YtcfgData {
    return this.extractYtcfgAndPlayerResponse(html).ytcfg;
  }

  /**
   * Extract ytcfg configuration and the raw ytInitialPlayerResponse from watch page HTML
   */
  extractYtcfgAndPlayerResponse(html: string): { ytcfg: YtcfgData; playerResponse: any | null } {
    let playerUrl = "";
    let visitorData = "";
    let sessionIndex: number | null = null;
    let delegatedSessionId: string | null = null;
    let dataSyncId: string | null = null;
    let title: string | null = null;
    let author: string | null = null;
    let channelId: string | null = null;
    let description: string | null = null;
    let thumbnailUrl: string | null = null;
    let rawPlayerResponse: any | null = null;

    // Extract player URL
    const jsUrlMatch = html.match(/"(?:jsUrl|PLAYER_JS_URL)":"([^"]+)"/);
    if (jsUrlMatch) {
      playerUrl = jsUrlMatch[1];
      if (playerUrl.startsWith("/")) {
        playerUrl = `${YOUTUBE_URLS.BASE}${playerUrl}`;
      }
    }

    // Extract visitor data
    const visitorMatch = html.match(/"visitorData":"([^"]+)"/);
    if (visitorMatch) {
      visitorData = visitorMatch[1];
    }

    // Extract SESSION_INDEX
    const sessionIndexMatch = html.match(/"SESSION_INDEX":"?(\d+)"?/);
    if (sessionIndexMatch) {
      sessionIndex = parseInt(sessionIndexMatch[1], 10);
    }

    // Extract DELEGATED_SESSION_ID
    const delegatedMatch = html.match(/"DELEGATED_SESSION_ID":"([^"]+)"/);
    if (delegatedMatch) {
      delegatedSessionId = delegatedMatch[1];
    }

    // Extract datasyncId (for secondary channels)
    const dataSyncMatch = html.match(/"datasyncId":"([^"]+)"/);
    if (dataSyncMatch) {
      dataSyncId = dataSyncMatch[1];
    }

    // Extract video metadata from ytInitialPlayerResponse
    const playerResponseMatch = html.match(
      /var ytInitialPlayerResponse = ({.+?});/s,
    );
    if (playerResponseMatch) {
      try {
        const playerResponse = JSON.parse(playerResponseMatch[1]);
        rawPlayerResponse = playerResponse;
        const videoDetails = playerResponse.videoDetails;
        if (videoDetails) {
          title = videoDetails.title || null;
          author = videoDetails.author || null;
          channelId = videoDetails.channelId || null;
          description = videoDetails.shortDescription || null;
          // Get best thumbnail
          const thumbnails = videoDetails.thumbnail?.thumbnails;
          if (thumbnails && thumbnails.length > 0) {
            thumbnailUrl = thumbnails[thumbnails.length - 1].url || null;
          }
        }
      } catch {
        // Ignore JSON parse errors
      }
    }

    return {
      ytcfg: {
        playerUrl,
        visitorData,
        sessionIndex,
        delegatedSessionId,
        dataSyncId,
        title,
        author,
        channelId,
        description,
        thumbnailUrl,
      },
      playerResponse: rawPlayerResponse,
    };
  }

  /**
   * Create a default (empty) ytcfg object
   */
  static createDefaultYtcfg(): YtcfgData {
    return {
      playerUrl: "",
      visitorData: "",
      sessionIndex: null,
      delegatedSessionId: null,
      dataSyncId: null,
      title: null,
      author: null,
      channelId: null,
      description: null,
      thumbnailUrl: null,
    };
  }
}
