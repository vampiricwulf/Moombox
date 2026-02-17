/**
 * Twitch GQL API Client
 *
 * Communicates with Twitch's internal GraphQL API for stream metadata,
 * access tokens, and VOD information. Uses the same approach as yt-dlp
 * and streamlink.
 */

import { fetchWithTimeout } from "../../core/http.js";
import { TWITCH_URLS, TWITCH_GQL_CLIENT_ID, TWITCH_GQL_HASHES } from "../../constants.js";
import type {
  TwitchStreamInfo,
  TwitchVodInfo,
  TwitchAccessToken,
  TwitchHlsVariant,
} from "../../types/twitch.js";
import { generateTwitchHeaders } from "./twitchAuth.js";

/**
 * Send a GQL request to Twitch.
 */
async function gqlRequest(
  body: unknown,
  authToken?: string | null,
): Promise<any> {
  const response = await fetchWithTimeout(
    TWITCH_URLS.GQL,
    {
      method: "POST",
      headers: generateTwitchHeaders(authToken),
      body: JSON.stringify(body),
    },
  );

  if (!response.ok) {
    throw new Error(`Twitch GQL error: HTTP ${response.status}`);
  }

  const result = await response.json();

  // Check for GraphQL-level errors (HTTP 200 with error payload)
  if (result?.errors?.length) {
    const msg = result.errors[0]?.message || "Unknown GQL error";
    throw new Error(`Twitch GQL error: ${msg}`);
  }

  return result;
}

/**
 * Send a batched GQL request (multiple operations in one call).
 */
async function gqlBatchRequest(
  operations: unknown[],
  authToken?: string | null,
): Promise<any[]> {
  const response = await fetchWithTimeout(
    TWITCH_URLS.GQL,
    {
      method: "POST",
      headers: generateTwitchHeaders(authToken),
      body: JSON.stringify(operations),
    },
  );

  if (!response.ok) {
    throw new Error(`Twitch GQL batch error: HTTP ${response.status}`);
  }

  const results = await response.json();

  // Check for GraphQL-level errors in batch results
  if (Array.isArray(results)) {
    for (const result of results) {
      if (result?.errors?.length) {
        const msg = result.errors[0]?.message || "Unknown GQL error";
        throw new Error(`Twitch GQL error: ${msg}`);
      }
    }
  }

  return results;
}

/**
 * Get stream metadata for a channel.
 * Returns null if the channel is offline.
 */
export async function getStreamInfo(
  channelLogin: string,
  authToken?: string | null,
): Promise<TwitchStreamInfo | null> {
  // Batch request: StreamMetadata + ComscoreStreamingQuery for title
  const results = await gqlBatchRequest([
    {
      operationName: "StreamMetadata",
      variables: { channelLogin, includeIsDJ: true },
      extensions: {
        persistedQuery: {
          version: 1,
          sha256Hash: TWITCH_GQL_HASHES.StreamMetadata,
        },
      },
    },
    {
      operationName: "ComscoreStreamingQuery",
      variables: { channel: channelLogin, clipSlug: "", isClip: false, isLive: true, isVodOrCollection: false, vodID: "" },
      extensions: {
        persistedQuery: {
          version: 1,
          sha256Hash: TWITCH_GQL_HASHES.ComscoreStreamingQuery,
        },
      },
    },
  ], authToken);

  const metadataResult = results[0]?.data?.user;
  if (!metadataResult) return null;

  const stream = metadataResult.stream;
  if (!stream) return null;

  // Extract title from ComscoreStreamingQuery (more reliable)
  const comscoreStream = results[1]?.data?.user?.stream;
  const title = stream.title || comscoreStream?.broadcaster?.broadcastSettings?.title || "";

  const info: TwitchStreamInfo = {
    streamId: stream.id,
    channelLogin: metadataResult.login || channelLogin,
    channelDisplayName: metadataResult.displayName || channelLogin,
    channelId: metadataResult.id || "",
    title,
    gameCategory: stream.game?.displayName,
    viewerCount: stream.viewersCount,
    startedAt: stream.createdAt,
    isLive: true,
    streamType: stream.type === "rerun" ? "rerun" : "live",
  };

  // Build thumbnail URL
  if (metadataResult.login) {
    info.thumbnailUrl = `${TWITCH_URLS.PREVIEW_CDN}/live_user_${metadataResult.login}-640x360.jpg`;
  }

  // Extract channel profile image (stable fallback when live preview 404s)
  if (metadataResult.profileImageURL) {
    info.profileImageUrl = metadataResult.profileImageURL;
  }

  return info;
}

/**
 * Get a playback access token for a live stream.
 * Required to access HLS playlists.
 * Uses raw GQL query (not persisted query) — Twitch removed the persisted hash.
 */
export async function getStreamAccessToken(
  channelLogin: string,
  authToken?: string | null,
): Promise<TwitchAccessToken> {
  // Sanitize input — Twitch logins are alphanumeric + underscores
  const safeLogin = channelLogin.replace(/[^a-zA-Z0-9_]/g, "");
  const result = await gqlRequest(
    {
      query: `{
        streamPlaybackAccessToken(
          channelName: "${safeLogin}",
          params: {
            platform: "web",
            playerBackend: "mediaplayer",
            playerType: "site"
          }
        )
        {
          value
          signature
        }
      }`,
    },
    authToken,
  );

  const token = result?.data?.streamPlaybackAccessToken;
  if (!token) {
    throw new Error("Failed to get stream access token");
  }

  return { value: token.value, signature: token.signature };
}

/**
 * Get a playback access token for a VOD.
 * Uses raw GQL query (not persisted query) — Twitch removed the persisted hash.
 */
export async function getVodAccessToken(
  vodId: string,
  authToken?: string | null,
): Promise<TwitchAccessToken> {
  // Sanitize input — VOD IDs are numeric
  const safeVodId = vodId.replace(/[^0-9]/g, "");
  const result = await gqlRequest(
    {
      query: `{
        videoPlaybackAccessToken(
          id: "${safeVodId}",
          params: {
            platform: "web",
            playerBackend: "mediaplayer",
            playerType: "site"
          }
        )
        {
          value
          signature
        }
      }`,
    },
    authToken,
  );

  const token = result?.data?.videoPlaybackAccessToken;
  if (!token) {
    throw new Error("Failed to get VOD access token");
  }

  return { value: token.value, signature: token.signature };
}

/**
 * Get VOD metadata.
 */
export async function getVodInfo(
  vodId: string,
  authToken?: string | null,
): Promise<TwitchVodInfo> {
  const result = await gqlRequest(
    {
      operationName: "VideoMetadata",
      variables: { channelLogin: "", videoID: vodId },
      extensions: {
        persistedQuery: {
          version: 1,
          sha256Hash: TWITCH_GQL_HASHES.VideoMetadata,
        },
      },
    },
    authToken,
  );

  const video = result?.data?.video;
  if (!video) {
    throw new Error(`VOD ${vodId} not found`);
  }

  // Parse duration from ISO 8601 or seconds
  let duration = 0;
  if (typeof video.lengthSeconds === "number") {
    duration = video.lengthSeconds;
  }

  return {
    vodId,
    title: video.title || "",
    channelLogin: video.owner?.login || "",
    channelDisplayName: video.owner?.displayName || "",
    duration,
    thumbnailUrl: video.previewThumbnailURL,
    createdAt: video.createdAt || new Date().toISOString(),
    viewCount: video.viewCount,
    gameCategory: video.game?.displayName,
  };
}

/**
 * Build Usher HLS master playlist URL for a live stream.
 */
function buildUsherLiveUrl(channelLogin: string, token: TwitchAccessToken): string {
  const params = new URLSearchParams({
    allow_source: "true",
    allow_audio_only: "true",
    allow_spectre: "true",
    fast_bread: "true",
    p: String(Math.floor(Math.random() * 10000000)),
    player: "twitchweb",
    playlist_include_framerate: "true",
    sig: token.signature,
    token: token.value,
    type: "any",
  });
  return `${TWITCH_URLS.USHER_LIVE}/${channelLogin}.m3u8?${params}`;
}

/**
 * Build Usher HLS playlist URL for a VOD.
 */
function buildUsherVodUrl(vodId: string, token: TwitchAccessToken): string {
  const params = new URLSearchParams({
    allow_source: "true",
    allow_audio_only: "true",
    allow_spectre: "true",
    p: String(Math.floor(Math.random() * 10000000)),
    player: "twitchweb",
    playlist_include_framerate: "true",
    sig: token.signature,
    token: token.value,
    type: "any",
  });
  return `${TWITCH_URLS.USHER_VOD}/${vodId}.m3u8?${params}`;
}

/**
 * Parse an HLS master playlist into variant streams.
 */
function parseHlsMasterPlaylist(content: string): TwitchHlsVariant[] {
  const variants: TwitchHlsVariant[] = [];
  const lines = content.split("\n");

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].trim();

    // #EXT-X-MEDIA:TYPE=VIDEO,GROUP-ID="chunked",NAME="1080p60 (source)",...
    // #EXT-X-STREAM-INF:BANDWIDTH=...,RESOLUTION=...,VIDEO="chunked",...
    if (!line.startsWith("#EXT-X-STREAM-INF:")) continue;

    const attrs = line.substring("#EXT-X-STREAM-INF:".length);
    const url = lines[i + 1]?.trim();
    if (!url || url.startsWith("#")) continue;

    // Parse attributes
    const bandwidth = parseInt(attrs.match(/BANDWIDTH=(\d+)/)?.[1] || "0", 10);
    const resolution = attrs.match(/RESOLUTION=(\d+)x(\d+)/);
    const width = resolution ? parseInt(resolution[1], 10) : undefined;
    const height = resolution ? parseInt(resolution[2], 10) : undefined;
    const frameRate = parseFloat(attrs.match(/FRAME-RATE=([\d.]+)/)?.[1] || "0");
    const videoGroup = attrs.match(/VIDEO="([^"]+)"/)?.[1] || "";

    // Derive human-readable name from GROUP-ID
    const name = videoGroup || (height ? `${height}p${frameRate >= 59 ? "60" : "30"}` : "unknown");

    variants.push({
      url,
      name,
      bandwidth,
      width,
      height,
      fps: frameRate || undefined,
      isSource: videoGroup === "chunked" || name.includes("source"),
    });
  }

  return variants;
}

/**
 * Fetch HLS variants for a live stream.
 */
export async function getHlsMasterPlaylist(
  channelLogin: string,
  authToken?: string | null,
): Promise<TwitchHlsVariant[]> {
  const token = await getStreamAccessToken(channelLogin, authToken);
  const url = buildUsherLiveUrl(channelLogin, token);

  const response = await fetchWithTimeout(url, {
    headers: { "Client-ID": TWITCH_GQL_CLIENT_ID },
  });

  if (!response.ok) {
    throw new Error(`Usher returned HTTP ${response.status}`);
  }

  const content = await response.text();
  return parseHlsMasterPlaylist(content);
}

/**
 * Fetch HLS variants for a VOD.
 */
export async function getVodHlsPlaylist(
  vodId: string,
  authToken?: string | null,
): Promise<TwitchHlsVariant[]> {
  const token = await getVodAccessToken(vodId, authToken);
  const url = buildUsherVodUrl(vodId, token);

  const response = await fetchWithTimeout(url, {
    headers: { "Client-ID": TWITCH_GQL_CLIENT_ID },
  });

  if (!response.ok) {
    throw new Error(`Usher VOD returned HTTP ${response.status}`);
  }

  const content = await response.text();
  return parseHlsMasterPlaylist(content);
}

/**
 * VOD comment edge from GQL response.
 */
export interface VodCommentEdge {
  node: {
    id: string;
    contentOffsetSeconds: number;
    commenter: {
      displayName: string;
      id: string;
      login: string;
    } | null;
    message: {
      fragments: Array<{
        text: string;
        emote: { emoteID: string } | null;
      }>;
      userBadges: Array<{ setID: string; version: string }>;
      userColor: string | null;
    };
  };
}

/**
 * Fetch VOD chat comments (replay) via GQL persisted query.
 * Always uses contentOffsetSeconds for pagination (not cursor) because
 * cursor-based pagination triggers Twitch's integrity check.
 * This matches TwitchDownloader's approach.
 */
export async function getVodComments(
  vodId: string,
  contentOffsetSeconds: number,
  authToken?: string | null,
): Promise<{ edges: VodCommentEdge[]; hasNextPage: boolean }> {
  const result = await gqlRequest(
    {
      operationName: "VideoCommentsByOffsetOrCursor",
      variables: {
        videoID: vodId,
        contentOffsetSeconds,
      },
      extensions: {
        persistedQuery: {
          version: 1,
          sha256Hash: TWITCH_GQL_HASHES.VideoCommentsByOffsetOrCursor,
        },
      },
    },
    authToken,
  );

  const comments = result?.data?.video?.comments;
  if (!comments) {
    return { edges: [], hasNextPage: false };
  }

  return {
    edges: comments.edges || [],
    hasNextPage: comments.pageInfo?.hasNextPage ?? false,
  };
}

/**
 * Lightweight channel-live check using DECAPI.
 * Returns true if the channel is currently live.
 */
export async function isChannelLive(channelLogin: string): Promise<boolean> {
  try {
    const response = await fetchWithTimeout(
      `${TWITCH_URLS.DECAPI_UPTIME}/${channelLogin}`,
      { headers: { "User-Agent": "Moombox/1.0" } },
    );
    if (!response.ok) return false;
    const text = await response.text();
    const lower = text.toLowerCase().trim();
    // DECAPI returns the channel name followed by " is offline" when not live,
    // or error messages like "user not found" / "could not be found"
    if (lower.includes("is offline")) return false;
    if (lower.includes("not found") || lower.includes("error")) return false;
    // A valid uptime response is a duration string like "2 hours, 15 minutes"
    // containing at least one time unit word
    return /\d/.test(text) && /(second|minute|hour|day|week)/i.test(text);
  } catch {
    return false;
  }
}
