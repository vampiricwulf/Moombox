/**
 * YouTube Player API
 *
 * Handles interactions with YouTube's Innertube player API.
 */

import { Logger } from "../../core/logger.js";
import { getSts } from "../../cipher/src/handlers/getSts.js";
import { decryptSignature } from "../../cipher/src/handlers/decryptSignature.js";
import type {
  Format,
  VideoInfo,
  StreamStatus,
  PlayabilityError,
  YouTubeClientConfig,
  YtcfgData,
} from "../../types/youtube.js";
import {
  TV_DOWNGRADED_CLIENT,
  WEB_CREATOR_CLIENT,
  ANDROID_VR_CLIENT,
  DEFAULT_API_KEY,
  YOUTUBE_URLS,
} from "../../constants.js";
import { YouTubeAuth } from "./auth.js";
import { WatchPageParser } from "./watchPage.js";
import { fetchWithTimeout } from "../../core/http.js";

/**
 * Authentication cost levels — lower = less auth needed.
 * Used as tiebreaker when deduplicating formats by itag.
 */
const AUTH_LEVELS = {
  ANDROID_VR: 0,    // No cookies, no cipher, no watch page
  WATCH_PAGE: 1,    // Embedded in HTML (no separate API call)
  TV_PUBLIC: 2,     // TV_DOWNGRADED without cookies
  TV_AUTH: 3,       // TV_DOWNGRADED with cookies
  WEB_CREATOR: 4,   // WEB_CREATOR with cookies
} as const;

/**
 * Fallback metadata for when API doesn't return it
 */
interface FallbackMetadata {
  title: string | null;
  author: string | null;
  channelId: string | null;
  description: string | null;
  thumbnailUrl: string | null;
}

/**
 * YouTube Player API Client
 */
export class PlayerApi {
  private logger: Logger;
  private auth: YouTubeAuth;
  private innertubeApiKey: string = DEFAULT_API_KEY;

  constructor(auth: YouTubeAuth) {
    this.logger = Logger.getInstance();
    this.auth = auth;
  }

  /**
   * Set the API key (extracted from watch page)
   */
  setApiKey(apiKey: string): void {
    this.innertubeApiKey = apiKey;
  }

  /**
   * Get video info using authenticated clients
   */
  async getVideoInfoAuthenticated(videoId: string): Promise<VideoInfo> {
    const watchParser = new WatchPageParser();

    // Fetch watch page to extract ytcfg and embedded player response
    let ytcfg = WatchPageParser.createDefaultYtcfg();
    let watchPagePlayerResponse: any | null = null;
    let signatureTimestamp = 0;

    try {
      await this.auth.syncCookies();
      const { ytcfg: extractedYtcfg, playerResponse } = await watchParser.fetch(
        videoId,
        this.auth.getCookieHeader(),
      );
      ytcfg = extractedYtcfg;
      watchPagePlayerResponse = playerResponse;

      if (ytcfg.playerUrl) {
        try {
          const stsResult = await getSts({ player_url: ytcfg.playerUrl });
          signatureTimestamp = parseInt(stsResult.sts, 10);
          this.logger.debug(
            `[PlayerApi] Got signature timestamp: ${signatureTimestamp}`,
          );
        } catch (e) {
          this.logger.debug(`[PlayerApi] Could not get STS: ${e}`);
        }
      }
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      this.logger.warn(`[PlayerApi] Could not fetch watch page: ${msg}`);
    }

    // Format pool: collect formats from all clients tried, deduplicate at the end
    const formatPool: Format[] = [];

    // Parse watch page player response once upfront
    const wpFallback: FallbackMetadata = {
      title: ytcfg.title,
      author: ytcfg.author,
      channelId: ytcfg.channelId,
      description: ytcfg.description,
      thumbnailUrl: ytcfg.thumbnailUrl,
    };
    let wpParsed: VideoInfo | null = null;
    if (watchPagePlayerResponse) {
      wpParsed = await this.parsePlayerResponse(
        watchPagePlayerResponse,
        ytcfg.playerUrl,
        wpFallback,
      );
      this.collectFormats(formatPool, wpParsed.formats, "watch_page", AUTH_LEVELS.WATCH_PAGE);
    }

    // Try TV_DOWNGRADED client first
    const result = await this.fetchWithClient(
      videoId,
      TV_DOWNGRADED_CLIENT,
      ytcfg,
      signatureTimestamp,
    );
    this.collectFormats(formatPool, result.formats, "tv_auth", AUTH_LEVELS.TV_AUTH);

    // If TV_DOWNGRADED fails, try WEB_CREATOR
    if (
      result.playabilityError === "members_only" ||
      result.playabilityError === "login_required" ||
      result.formats.length === 0
    ) {
      this.logger.debug(
        `[PlayerApi] TV_DOWNGRADED client failed (${result.playabilityError || "no formats"}), trying WEB_CREATOR for ${videoId}`,
      );
      const webResult = await this.fetchWithClient(
        videoId,
        WEB_CREATOR_CLIENT,
        ytcfg,
        signatureTimestamp,
      );
      this.collectFormats(formatPool, webResult.formats, "web_creator", AUTH_LEVELS.WEB_CREATOR);

      // If WEB_CREATOR also fails, try ANDROID_VR (unless members_only — auth is required)
      if (
        webResult.playabilityError === "login_required" ||
        webResult.streamStatus === "not_a_stream" ||
        webResult.formats.length === 0 ||
        !this.hasAdequateFormats(webResult)
      ) {
        if (webResult.playabilityError !== "members_only") {
          this.logger.debug(
            `[PlayerApi] WEB_CREATOR failed (${webResult.playabilityError || "inadequate formats"}), trying ANDROID_VR for ${videoId}`,
          );
          try {
            const vrResult = await this.fetchWithAndroidVR(videoId, ytcfg.visitorData || undefined);
            this.collectFormats(formatPool, vrResult.formats, "android_vr", AUTH_LEVELS.ANDROID_VR);
            if (vrResult.playabilityError === "ok" && this.hasAdequateFormats(vrResult)) {
              if (wpParsed) this.mergeWatchPageMetadata(vrResult, wpParsed);
              vrResult.formats = this.deduplicateFormats(formatPool);
              return vrResult;
            }
            this.logger.debug(
              `[PlayerApi] ANDROID_VR returned ${vrResult.playabilityError || "inadequate formats"} for ${videoId}`,
            );
          } catch (e) {
            const msg = e instanceof Error ? e.message : String(e);
            this.logger.debug(`[PlayerApi] ANDROID_VR failed: ${msg}`);
          }
        }

        // Fall back to watch page embedded player response
        if (wpParsed) {
          this.logger.debug(
            `[PlayerApi] Falling back to watch page player response for ${videoId}`,
          );
          wpParsed.formats = this.deduplicateFormats(formatPool);
          return wpParsed;
        }
      }

      if (wpParsed) this.mergeWatchPageMetadata(webResult, wpParsed);
      webResult.formats = this.deduplicateFormats(formatPool);
      return webResult;
    }

    // If TV result looks like not_a_stream but watch page says otherwise,
    // use watch page's stream classification but keep TV's formats if adequate
    if (result.streamStatus === "not_a_stream" && wpParsed) {
      if (wpParsed.streamStatus !== "not_a_stream") {
        if (this.hasAdequateFormats(result)) {
          // TV has good formats — keep them, just override stream classification
          this.logger.debug(
            `[PlayerApi] TV client said not_a_stream but watch page says ${wpParsed.streamStatus} — keeping TV formats for ${videoId}`,
          );
          result.streamStatus = wpParsed.streamStatus;
          result.isLive = wpParsed.isLive;
          result.isUpcoming = wpParsed.isUpcoming;
          result.isPostLiveDVR = wpParsed.isPostLiveDVR;
          this.mergeWatchPageMetadata(result, wpParsed);
          result.formats = this.deduplicateFormats(formatPool);
          return result;
        }
        // TV formats inadequate — fall back to watch page entirely
        this.logger.debug(
          `[PlayerApi] TV client said not_a_stream but watch page says ${wpParsed.streamStatus} — using watch page for ${videoId}`,
        );
        wpParsed.formats = this.deduplicateFormats(formatPool);
        return wpParsed;
      }
    }

    // Merge watch page metadata into result (backfills scheduledStartTime, description, etc.)
    if (wpParsed) this.mergeWatchPageMetadata(result, wpParsed);
    result.formats = this.deduplicateFormats(formatPool);
    return result;
  }

  /**
   * Get video info using public client (no auth)
   */
  async getVideoInfoPublic(videoId: string): Promise<VideoInfo> {
    // For public content without cookies, use a simpler approach
    const watchParser = new WatchPageParser();
    const { ytcfg, playerResponse } = await watchParser.fetch(videoId);

    // Format pool: collect formats from all clients tried, deduplicate at the end
    const formatPool: Format[] = [];

    // Parse watch page player response once upfront
    let wpParsed: VideoInfo | null = null;
    if (playerResponse) {
      wpParsed = await this.parsePlayerResponse(playerResponse, ytcfg.playerUrl, {
        title: ytcfg.title,
        author: ytcfg.author,
        channelId: ytcfg.channelId,
        description: ytcfg.description,
        thumbnailUrl: ytcfg.thumbnailUrl,
      });
      this.collectFormats(formatPool, wpParsed.formats, "watch_page", AUTH_LEVELS.WATCH_PAGE);
    }

    const result = await this.fetchWithClient(videoId, TV_DOWNGRADED_CLIENT, ytcfg, 0);
    this.collectFormats(formatPool, result.formats, "tv_public", AUTH_LEVELS.TV_PUBLIC);

    // If TV_DOWNGRADED fails or returns inadequate formats, try ANDROID_VR
    if (
      result.playabilityError === "login_required" ||
      result.formats.length === 0 ||
      !this.hasAdequateFormats(result)
    ) {
      this.logger.debug(
        `[PlayerApi] TV_DOWNGRADED public failed (${result.playabilityError || "inadequate formats"}), trying ANDROID_VR for ${videoId}`,
      );

      try {
        const vrResult = await this.fetchWithAndroidVR(videoId, ytcfg.visitorData || undefined);
        this.collectFormats(formatPool, vrResult.formats, "android_vr", AUTH_LEVELS.ANDROID_VR);
        if (vrResult.playabilityError === "ok" && this.hasAdequateFormats(vrResult)) {
          if (wpParsed) this.mergeWatchPageMetadata(vrResult, wpParsed);
          vrResult.formats = this.deduplicateFormats(formatPool);
          return vrResult;
        }
        this.logger.debug(
          `[PlayerApi] ANDROID_VR returned ${vrResult.playabilityError || "inadequate formats"} for ${videoId}`,
        );
      } catch (e) {
        const msg = e instanceof Error ? e.message : String(e);
        this.logger.debug(`[PlayerApi] ANDROID_VR failed: ${msg}`);
      }

      // Fall back to watch page embedded response
      if (wpParsed) {
        this.logger.debug(
          `[PlayerApi] Falling back to watch page player response for ${videoId}`,
        );
        wpParsed.formats = this.deduplicateFormats(formatPool);
        return wpParsed;
      }
    }

    // If TV result looks like not_a_stream but the watch page says otherwise,
    // use watch page's stream classification but keep TV's formats if adequate
    if (result.streamStatus === "not_a_stream" && wpParsed) {
      if (wpParsed.streamStatus !== "not_a_stream") {
        if (this.hasAdequateFormats(result)) {
          // TV has good formats — keep them, just override stream classification
          this.logger.debug(
            `[PlayerApi] Public TV said not_a_stream but watch page says ${wpParsed.streamStatus} — keeping TV formats for ${videoId}`,
          );
          result.streamStatus = wpParsed.streamStatus;
          result.isLive = wpParsed.isLive;
          result.isUpcoming = wpParsed.isUpcoming;
          result.isPostLiveDVR = wpParsed.isPostLiveDVR;
          this.mergeWatchPageMetadata(result, wpParsed);
          result.formats = this.deduplicateFormats(formatPool);
          return result;
        }
        // TV formats inadequate — fall back to watch page entirely
        this.logger.debug(
          `[PlayerApi] Public TV said not_a_stream, falling back to watch page for ${videoId}`,
        );
        wpParsed.formats = this.deduplicateFormats(formatPool);
        return wpParsed;
      }
      // Both agree it's not_a_stream — return TV result (has formats)
    }

    // Merge watch page metadata into result (backfills scheduledStartTime, description, etc.)
    if (wpParsed) this.mergeWatchPageMetadata(result, wpParsed);
    result.formats = this.deduplicateFormats(formatPool);
    return result;
  }

  /**
   * Lightweight probe using ANDROID_VR — no cookies, no watch page, no cipher.
   * Returns status + metadata only. Used for polling upcoming streams.
   */
  async probeVideoStatus(videoId: string, visitorData?: string): Promise<VideoInfo> {
    return this.fetchWithAndroidVR(videoId, visitorData);
  }

  /**
   * Lightweight authenticated probe using TV_DOWNGRADED with cookies.
   * Single API call — no watch page, no STS, no cipher.
   * Used for polling members-only upcoming streams.
   */
  async probeVideoStatusAuthenticated(videoId: string, visitorData?: string): Promise<VideoInfo> {
    await this.auth.syncCookies();
    const ytcfg = WatchPageParser.createDefaultYtcfg();
    if (visitorData) {
      ytcfg.visitorData = visitorData;
    }
    return this.fetchWithClient(videoId, TV_DOWNGRADED_CLIENT, ytcfg, 0);
  }

  /**
   * Fetch video info with a specific client
   */
  private async fetchWithClient(
    videoId: string,
    client: YouTubeClientConfig,
    ytcfg: YtcfgData,
    signatureTimestamp: number,
  ): Promise<VideoInfo> {
    const apiUrl = `${YOUTUBE_URLS.API}/player?key=${this.innertubeApiKey}`;
    const headers = this.auth.generateApiHeaders(client, ytcfg);

    const postData = {
      context: {
        client: {
          ...client.context,
          visitorData: ytcfg.visitorData || undefined,
        },
      },
      videoId,
      playbackContext: {
        contentPlaybackContext: {
          html5Preference: "HTML5_PREF_WANTS",
          signatureTimestamp: signatureTimestamp || undefined,
        },
      },
      contentCheckOk: true,
      racyCheckOk: true,
    };

    this.logger.debug(
      `[PlayerApi] Fetching video info via ${client.clientName} client for ${videoId}`,
    );

    const response = await fetchWithTimeout(apiUrl, {
      method: "POST",
      headers,
      body: JSON.stringify(postData),
    });

    if (!response.ok) {
      throw new Error(`Innertube API error: HTTP ${response.status}`);
    }

    const data = await response.json();

    // Log playability status
    const playabilityStatus = data.playabilityStatus?.status;
    const playabilityReason =
      data.playabilityStatus?.reason || data.playabilityStatus?.messages?.[0];
    this.logger.debug(
      `[PlayerApi] ${client.clientName} response - status: ${playabilityStatus}, reason: ${playabilityReason || "none"}`,
    );

    return this.parsePlayerResponse(data, ytcfg.playerUrl, {
      title: ytcfg.title,
      author: ytcfg.author,
      channelId: ytcfg.channelId,
      description: ytcfg.description,
      thumbnailUrl: ytcfg.thumbnailUrl,
    });
  }

  /**
   * Tag formats with source/authLevel and add them to the pool.
   */
  private collectFormats(pool: Format[], formats: Format[], source: string, authLevel: number): void {
    for (const f of formats) {
      f.source = source;
      f.authLevel = authLevel;
      pool.push(f);
    }
  }

  /**
   * Deduplicate formats by itag, preferring the lowest authLevel when duplicates exist.
   */
  private deduplicateFormats(pool: Format[]): Format[] {
    const byItag = new Map<number, Format>();
    for (const f of pool) {
      if (!f.url) continue;
      const existing = byItag.get(f.itag);
      if (!existing || (f.authLevel ?? 999) < (existing.authLevel ?? 999)) {
        byItag.set(f.itag, f);
      }
    }
    const deduped = Array.from(byItag.values());
    this.logger.debug(`[PlayerApi] Format pool: ${pool.length} total → ${deduped.length} unique (by itag, preferring lowest auth)`);
    return deduped;
  }

  /**
   * Check if formats include both video and separate audio (adaptive).
   * Returns false for progressive-only results (e.g. itag 18 at 360p).
   */
  private hasAdequateFormats(info: VideoInfo): boolean {
    const hasVideo = info.formats.some((f) => f.width || f.height);
    const hasAudio = info.formats.some((f) => f.audioQuality && !f.width);
    return hasVideo && hasAudio;
  }

  /**
   * Backfill missing metadata from watch page VideoInfo into an API client VideoInfo.
   * Only fills fields that are empty/missing in the target — does not override existing values.
   * Does NOT touch: formats, stream classification, or playability fields.
   */
  private mergeWatchPageMetadata(target: VideoInfo, wpSource: VideoInfo): void {
    target.scheduledStartTime = target.scheduledStartTime || wpSource.scheduledStartTime;
    target.description = target.description || wpSource.description;
    target.thumbnailUrl = target.thumbnailUrl || wpSource.thumbnailUrl;
    if (target.title === "Unknown Title" && wpSource.title !== "Unknown Title") {
      target.title = wpSource.title;
    }
    if (target.channelName === "Unknown Channel" && wpSource.channelName !== "Unknown Channel") {
      target.channelName = wpSource.channelName;
    }
    target.channelId = target.channelId || wpSource.channelId;
    target.lengthSeconds = target.lengthSeconds || wpSource.lengthSeconds;
    target.endTimestamp = target.endTimestamp || wpSource.endTimestamp;
    target.dashManifestUrl = target.dashManifestUrl || wpSource.dashManifestUrl;
    target.hlsManifestUrl = target.hlsManifestUrl || wpSource.hlsManifestUrl;
    target.playerUrl = target.playerUrl || wpSource.playerUrl;
  }

  /**
   * Fetch video info using the ANDROID_VR client.
   * This client requires no cookies, no JS player, no signature decryption, and no PO token.
   * Formats have direct URLs so n-param decryption is skipped (may be throttled).
   */
  private async fetchWithAndroidVR(
    videoId: string,
    visitorData?: string,
  ): Promise<VideoInfo> {
    this.logger.info(
      `[PlayerApi] Trying ANDROID_VR client for ${videoId}`,
    );

    const apiUrl = `${YOUTUBE_URLS.API}/player?key=${this.innertubeApiKey}`;
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      "User-Agent": ANDROID_VR_CLIENT.userAgent,
      "X-YouTube-Client-Name": ANDROID_VR_CLIENT.clientId,
      "X-YouTube-Client-Version": ANDROID_VR_CLIENT.context.clientVersion,
      Origin: "https://www.youtube.com",
    };
    if (visitorData) {
      headers["X-Goog-Visitor-Id"] = visitorData;
    }

    const postData = {
      context: {
        client: {
          ...ANDROID_VR_CLIENT.context,
          visitorData: visitorData || undefined,
        },
      },
      videoId,
      playbackContext: {
        contentPlaybackContext: { html5Preference: "HTML5_PREF_WANTS" },
      },
      contentCheckOk: true,
      racyCheckOk: true,
    };

    const response = await fetchWithTimeout(apiUrl, {
      method: "POST",
      headers,
      body: JSON.stringify(postData),
    });

    if (!response.ok) {
      throw new Error(`ANDROID_VR API error: HTTP ${response.status}`);
    }

    const data = await response.json();

    const playabilityStatus = data.playabilityStatus?.status;
    const playabilityReason =
      data.playabilityStatus?.reason || data.playabilityStatus?.messages?.[0];
    this.logger.debug(
      `[PlayerApi] ANDROID_VR response - status: ${playabilityStatus}, reason: ${playabilityReason || "none"}`,
    );

    // Empty playerUrl — n-param decryption is skipped (formats still work, possibly throttled)
    return this.parsePlayerResponse(data, "", undefined);
  }

  /**
   * Parse player API response into VideoInfo
   */
  private async parsePlayerResponse(
    data: any,
    playerUrl: string,
    fallback?: FallbackMetadata,
  ): Promise<VideoInfo> {
    const videoDetails = data.videoDetails || {};
    const streamingData = data.streamingData || {};
    const playabilityStatus = data.playabilityStatus || {};
    const microformat = data.microformat?.playerMicroformatRenderer;

    // Parse playability error
    const { error: playabilityError, reason: playabilityReason } =
      this.parsePlayabilityStatus(playabilityStatus);

    // Parse formats
    const formats = await this.parseFormats(streamingData, playerUrl);

    // Determine stream status
    const liveBroadcastDetails = microformat?.liveBroadcastDetails;
    const isLiveContent = videoDetails.isLiveContent;
    const isPostLiveDvr =
      liveBroadcastDetails?.endTimestamp && !videoDetails.isLive;
    const isLiveNow = liveBroadcastDetails?.isLiveNow || videoDetails.isLive;
    const hasStreamingData = formats.length > 0;

    // Check playability status for additional context
    const status = playabilityStatus.status;
    const reason = (playabilityStatus.reason || "").toLowerCase();

    // Detect upcoming streams from playability status
    // "LIVE_STREAM_OFFLINE" or "UNPLAYABLE" with "live event will begin" indicates upcoming
    const isUpcomingFromPlayability =
      status === "LIVE_STREAM_OFFLINE" ||
      (status === "UNPLAYABLE" && reason.includes("live event will begin"));

    // Detect premieres
    const isPremiere =
      microformat?.liveBroadcastDetails?.startTimestamp &&
      !isLiveContent &&
      (reason.includes("premiere") || videoDetails.isUpcoming);
    const isPremiereNow = isPremiere && isLiveNow;
    const isUpcomingPremiere = isPremiere && !isLiveNow && !hasStreamingData;

    let streamStatus: StreamStatus;

    // Priority order:
    // 1. Upcoming from playability status (LIVE_STREAM_OFFLINE etc.)
    // 2. Upcoming premiere (has start timestamp, not live yet)
    // 3. Currently live (videoDetails.isLive or liveBroadcastDetails.isLiveNow)
    // 4. Not a stream (no live indicators at all)
    // 5. Upcoming (has live metadata but no streaming data yet)
    // 6. Post-live DVR
    // 7. VOD fallback
    if (isUpcomingFromPlayability) {
      streamStatus = "upcoming";
    } else if (isUpcomingPremiere) {
      streamStatus = "upcoming";
    } else if (isLiveNow || isPremiereNow) {
      streamStatus = "live";
    } else if (
      !liveBroadcastDetails &&
      !isLiveContent &&
      !isPremiere
    ) {
      streamStatus = "not_a_stream";
    } else if (!hasStreamingData && (liveBroadcastDetails || isLiveContent)) {
      streamStatus = "upcoming";
    } else if (isPostLiveDvr) {
      streamStatus = "post_live";
    } else {
      streamStatus = "vod";
    }

    this.logger.debug(
      `[PlayerApi] Stream classification for ${videoDetails.videoId}: ${streamStatus} ` +
      `(isLive=${videoDetails.isLive}, isLiveContent=${isLiveContent}, ` +
      `liveBroadcastDetails=${!!liveBroadcastDetails}, isLiveNow=${isLiveNow}, ` +
      `hasStreamingData=${hasStreamingData})`,
    );

    // Use fallback metadata if API doesn't return it
    const title = videoDetails.title || fallback?.title || "Unknown Title";
    const channelName =
      videoDetails.author || fallback?.author || "Unknown Channel";
    const channelId = videoDetails.channelId || fallback?.channelId || "";
    const description =
      videoDetails.shortDescription || fallback?.description || "";

    let thumbnailUrl = fallback?.thumbnailUrl || undefined;
    if (videoDetails.thumbnail?.thumbnails?.length > 0) {
      const thumbnails = videoDetails.thumbnail.thumbnails;
      thumbnailUrl = thumbnails[thumbnails.length - 1].url;
    }

    // Extract scheduled start time for upcoming streams, or upload date for VODs
    // For streams: use liveBroadcastDetails.startTimestamp
    // For regular videos: use microformat uploadDate or publishDate
    const scheduledStartTime =
      liveBroadcastDetails?.startTimestamp ||
      microformat?.uploadDate ||
      microformat?.publishDate ||
      undefined;

    // Video duration — "0" while live, accurate after stream ends
    const rawLength = parseInt(videoDetails.lengthSeconds, 10);
    const lengthSeconds = rawLength > 0 ? rawLength : undefined;

    // Stream end timestamp — only in microformat liveBroadcastDetails (watch page)
    const endTimestamp = liveBroadcastDetails?.endTimestamp || undefined;

    return {
      title,
      channelName,
      channelId,
      description,
      thumbnailUrl,
      formats,
      playerUrl,
      streamStatus,
      isLive: streamStatus === "live",
      isUpcoming: streamStatus === "upcoming",
      isPostLiveDVR: streamStatus === "post_live",
      lengthSeconds,
      endTimestamp,
      scheduledStartTime,
      dashManifestUrl: streamingData.dashManifestUrl,
      hlsManifestUrl: streamingData.hlsManifestUrl,
      playabilityError,
      playabilityReason,
    };
  }

  /**
   * Parse formats from streaming data
   */
  private async parseFormats(
    streamingData: any,
    playerUrl: string,
  ): Promise<Format[]> {
    const formats: Format[] = [];
    const allFormats = [
      ...(streamingData.formats || []),
      ...(streamingData.adaptiveFormats || []),
    ];

    let nParamDecrypted = 0;
    for (const f of allFormats) {
      if (f.url) {
        let finalUrl = f.url;

        // Decrypt n parameter if needed
        if (playerUrl) {
          try {
            const urlObj = new URL(f.url);
            const nParam = urlObj.searchParams.get("n");
            if (nParam) {
              const result = await decryptSignature({
                n_param: nParam,
                player_url: playerUrl,
              });
              if (result.decrypted_n_sig) {
                urlObj.searchParams.set("n", result.decrypted_n_sig);
                finalUrl = urlObj.toString();
                nParamDecrypted++;
              }
            }
          } catch (e) {
            const msg = e instanceof Error ? e.message : String(e);
            this.logger.debug(
              `[PlayerApi] Failed to decrypt n param for format ${f.itag}: ${msg}`,
            );
          }
        }

        formats.push({
          itag: f.itag,
          url: finalUrl,
          mimeType: f.mimeType,
          bitrate: f.bitrate,
          width: f.width,
          height: f.height,
          contentLength: f.contentLength,
          qualityLabel: f.qualityLabel,
          audioQuality: f.audioQuality,
          audioSampleRate: f.audioSampleRate,
          fps: f.fps,
        });
      } else if (f.signatureCipher && playerUrl) {
        try {
          const decrypted = await this.decryptSignatureCipher(
            f.signatureCipher,
            playerUrl,
          );
          if (decrypted) {
            formats.push({
              itag: f.itag,
              url: decrypted,
              mimeType: f.mimeType,
              bitrate: f.bitrate,
              width: f.width,
              height: f.height,
              contentLength: f.contentLength,
              qualityLabel: f.qualityLabel,
              audioQuality: f.audioQuality,
              audioSampleRate: f.audioSampleRate,
              fps: f.fps,
            });
          }
        } catch (e) {
          const msg = e instanceof Error ? e.message : String(e);
          this.logger.debug(
            `[PlayerApi] Failed to decrypt signature cipher: ${msg}`,
          );
        }
      }
    }

    if (nParamDecrypted > 0) {
      this.logger.debug(
        `[PlayerApi] Decrypted n parameter for ${nParamDecrypted} formats`,
      );
    }
    this.logger.debug(
      `[PlayerApi] Extracted ${formats.length} formats with URLs`,
    );

    return formats;
  }

  /**
   * Decrypt signature cipher
   */
  private async decryptSignatureCipher(
    signatureCipher: string,
    playerUrl: string,
  ): Promise<string | null> {
    const params = new URLSearchParams(signatureCipher);
    const url = params.get("url");
    const s = params.get("s");
    const sp = params.get("sp") || "signature";

    if (!url || !s) return null;

    const result = await decryptSignature({
      encrypted_signature: s,
      player_url: playerUrl,
    });

    if (!result.decrypted_signature) return null;

    const finalUrl = new URL(url);
    finalUrl.searchParams.set(sp, result.decrypted_signature);

    // Also decrypt n parameter
    const nParam = finalUrl.searchParams.get("n");
    if (nParam) {
      const nResult = await decryptSignature({
        n_param: nParam,
        player_url: playerUrl,
      });
      if (nResult.decrypted_n_sig) {
        finalUrl.searchParams.set("n", nResult.decrypted_n_sig);
      }
    }

    return finalUrl.toString();
  }

  /**
   * Parse playability status
   */
  private parsePlayabilityStatus(status: any): {
    error: PlayabilityError;
    reason: string;
  } {
    const statusCode = status.status;
    const reason = status.reason || status.messages?.[0] || "";

    const reasonLower = reason.toLowerCase();

    // Check if this is an upcoming stream indicator (not a real error)
    const isUpcomingIndicator =
      statusCode === "LIVE_STREAM_OFFLINE" ||
      (statusCode === "UNPLAYABLE" &&
        reasonLower.includes("live event will begin"));

    if (isUpcomingIndicator) {
      // Not an error - this is a normal upcoming stream
      return { error: "ok", reason: "" };
    }

    switch (statusCode) {
      case "OK":
        return { error: "ok", reason: "" };

      case "LOGIN_REQUIRED":
        if (reasonLower.includes("member") || reasonLower.includes("join")) {
          return { error: "members_only", reason };
        }
        return { error: "login_required", reason };

      case "UNPLAYABLE":
        if (reasonLower.includes("member")) {
          return { error: "members_only", reason };
        }
        if (reasonLower.includes("private")) {
          return { error: "private", reason };
        }
        if (reasonLower.includes("unavailable")) {
          return { error: "unavailable", reason };
        }
        return { error: "unknown", reason };

      case "AGE_VERIFICATION_REQUIRED":
        return { error: "age_restricted", reason };

      case "ERROR":
        if (
          reasonLower.includes("private") ||
          reasonLower.includes("unavailable")
        ) {
          return { error: "unavailable", reason };
        }
        return { error: "unknown", reason };

      default:
        return { error: "unknown", reason };
    }
  }

  /**
   * Decrypt n parameter in DASH manifest URL
   */
  async decryptDashManifestUrl(
    dashUrl: string,
    playerUrl: string,
  ): Promise<string> {
    try {
      const urlObj = new URL(dashUrl);
      const nParam = urlObj.searchParams.get("n");
      if (nParam && playerUrl) {
        const result = await decryptSignature({
          n_param: nParam,
          player_url: playerUrl,
        });
        if (result.decrypted_n_sig) {
          urlObj.searchParams.set("n", result.decrypted_n_sig);
          return urlObj.toString();
        }
      }
    } catch (e) {
      this.logger.debug(`[PlayerApi] Failed to decrypt DASH URL n param: ${e}`);
    }
    return dashUrl;
  }

  /**
   * Decrypt n parameter in any URL (used for segment BaseURLs)
   * YouTube URLs encode n param as /n/{value}/ in the path
   */
  async decryptNParamInUrl(url: string, playerUrl: string): Promise<string> {
    if (!playerUrl) {
      this.logger.debug(
        `[PlayerApi] No player URL provided for n param decryption`,
      );
      return url;
    }

    try {
      // Check for n parameter in path: /n/{encrypted_value}/
      // Match values that look like encrypted n-params (10+ alphanumeric/special chars)
      const pathMatch = url.match(/\/n\/([a-zA-Z0-9_-]{10,})\//);
      if (pathMatch) {
        const encryptedN = pathMatch[1];
        this.logger.debug(
          `[PlayerApi] Found n param in path: ${encryptedN.substring(0, 10)}...`,
        );
        const result = await decryptSignature({
          n_param: encryptedN,
          player_url: playerUrl,
        });
        if (result.decrypted_n_sig && result.decrypted_n_sig !== encryptedN) {
          this.logger.debug(
            `[PlayerApi] Decrypted n: ${encryptedN.substring(0, 8)}... -> ${result.decrypted_n_sig.substring(0, 8)}...`,
          );
          const decryptedUrl = url.replace(
            `/n/${encryptedN}/`,
            `/n/${result.decrypted_n_sig}/`,
          );
          return decryptedUrl;
        } else {
          this.logger.debug(
            `[PlayerApi] n param decryption returned same value or empty`,
          );
        }
      }

      // Also check query string n param
      const urlObj = new URL(url);
      const nParam = urlObj.searchParams.get("n");
      if (nParam) {
        this.logger.debug(
          `[PlayerApi] Found n param in query: ${nParam.substring(0, 10)}...`,
        );
        const result = await decryptSignature({
          n_param: nParam,
          player_url: playerUrl,
        });
        if (result.decrypted_n_sig && result.decrypted_n_sig !== nParam) {
          this.logger.debug(
            `[PlayerApi] Decrypted query n: ${nParam.substring(0, 8)}... -> ${result.decrypted_n_sig.substring(0, 8)}...`,
          );
          urlObj.searchParams.set("n", result.decrypted_n_sig);
          return urlObj.toString();
        }
      }
    } catch (e) {
      this.logger.warn(`[PlayerApi] Failed to decrypt n param in URL: ${e}`);
    }
    return url;
  }
}
