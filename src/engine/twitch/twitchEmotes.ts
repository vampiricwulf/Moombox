/**
 * Twitch emote resolver — fetches emote data from native Twitch, BTTV, FFZ, and 7TV APIs.
 *
 * Fetched once per channel, cached in memory. Emote data is stored alongside chat JSON
 * so the frontend can render them at playback time without live API calls.
 */

import { fetchWithTimeout } from "../../core/http.js";
import { Logger } from "../../core/logger.js";
import { TWITCH_EMOTE_APIS } from "../../constants.js";
import type { TwitchEmoteData } from "../../types/twitch.js";

/** Per-channel emote cache (login → resolved emotes). Bounded to prevent unbounded growth. */
const MAX_EMOTE_CACHE_SIZE = 200;
const emoteCache = new Map<string, TwitchEmoteData>();

/**
 * Resolve all third-party emotes for a Twitch channel.
 * Results are cached in memory, bounded to MAX_EMOTE_CACHE_SIZE entries (LRU eviction).
 */
export async function resolveChannelEmotes(
  channelId: string,
  channelLogin: string,
): Promise<TwitchEmoteData> {
  const cacheKey = channelLogin.toLowerCase();
  const cached = emoteCache.get(cacheKey);
  if (cached) {
    // Move to end for LRU behavior (Map preserves insertion order)
    emoteCache.delete(cacheKey);
    emoteCache.set(cacheKey, cached);
    return cached;
  }

  const [bttv, ffz, seventv] = await Promise.allSettled([
    fetchBttvEmotes(channelId),
    fetchFfzEmotes(channelId),
    fetchSevenTvEmotes(channelId),
  ]);

  const emotes: TwitchEmoteData = {
    bttv: bttv.status === "fulfilled" ? bttv.value : [],
    ffz: ffz.status === "fulfilled" ? ffz.value : [],
    seventv: seventv.status === "fulfilled" ? seventv.value : [],
  };

  // Evict oldest entry if at capacity
  if (emoteCache.size >= MAX_EMOTE_CACHE_SIZE) {
    const oldest = emoteCache.keys().next().value;
    if (oldest !== undefined) emoteCache.delete(oldest);
  }
  emoteCache.set(cacheKey, emotes);
  const total = (emotes.bttv?.length || 0) + (emotes.ffz?.length || 0) + (emotes.seventv?.length || 0);
  if (total > 0) {
    Logger.getInstance().debug(
      `[TwitchEmotes] Resolved ${total} third-party emotes for ${channelLogin} ` +
        `(BTTV: ${emotes.bttv?.length || 0}, FFZ: ${emotes.ffz?.length || 0}, 7TV: ${emotes.seventv?.length || 0})`,
    );
  }

  return emotes;
}

// ===== BTTV =====

interface BttvEmote {
  id: string;
  code: string;
}

interface BttvChannelResponse {
  channelEmotes?: BttvEmote[];
  sharedEmotes?: BttvEmote[];
}

async function fetchBttvEmotes(
  channelId: string,
): Promise<Array<{ id: string; code: string; url: string }>> {
  try {
    const res = await fetchWithTimeout(
      `${TWITCH_EMOTE_APIS.BTTV_CHANNEL}/${channelId}`,
      undefined,
      8_000,
    );
    if (!res.ok) { await res.body?.cancel(); return []; }

    const data = (await res.json()) as BttvChannelResponse;
    const emotes: Array<{ id: string; code: string; url: string }> = [];

    for (const e of data.channelEmotes || []) {
      emotes.push({
        id: e.id,
        code: e.code,
        url: `https://cdn.betterttv.net/emote/${e.id}/2x.webp`,
      });
    }
    for (const e of data.sharedEmotes || []) {
      emotes.push({
        id: e.id,
        code: e.code,
        url: `https://cdn.betterttv.net/emote/${e.id}/2x.webp`,
      });
    }

    return emotes;
  } catch {
    return [];
  }
}

// ===== FFZ =====

interface FfzResponse {
  sets?: Record<
    string,
    {
      emoticons?: Array<{
        id: number;
        name: string;
        urls?: Record<string, string>;
      }>;
    }
  >;
}

async function fetchFfzEmotes(
  channelId: string,
): Promise<Array<{ id: number; code: string; url: string }>> {
  try {
    const res = await fetchWithTimeout(
      `${TWITCH_EMOTE_APIS.FFZ_CHANNEL}/${channelId}`,
      undefined,
      8_000,
    );
    if (!res.ok) { await res.body?.cancel(); return []; }

    const data = (await res.json()) as FfzResponse;
    const emotes: Array<{ id: number; code: string; url: string }> = [];

    for (const set of Object.values(data.sets || {})) {
      for (const e of set.emoticons || []) {
        const url = e.urls?.["2"] || e.urls?.["1"] || "";
        if (url) {
          emotes.push({
            id: e.id,
            code: e.name,
            url: url.startsWith("//") ? `https:${url}` : url,
          });
        }
      }
    }

    return emotes;
  } catch {
    return [];
  }
}

// ===== 7TV =====

interface SevenTvResponse {
  emote_set?: {
    emotes?: Array<{
      id: string;
      name: string;
      data?: {
        host?: {
          url?: string;
          files?: Array<{ name: string; format: string }>;
        };
      };
    }>;
  };
}

async function fetchSevenTvEmotes(
  channelId: string,
): Promise<Array<{ id: string; code: string; url: string }>> {
  try {
    const res = await fetchWithTimeout(
      `${TWITCH_EMOTE_APIS.SEVENTV_USER}/${channelId}`,
      undefined,
      8_000,
    );
    if (!res.ok) { await res.body?.cancel(); return []; }

    const data = (await res.json()) as SevenTvResponse;
    const emotes: Array<{ id: string; code: string; url: string }> = [];

    for (const e of data.emote_set?.emotes || []) {
      const host = e.data?.host;
      if (host?.url && host.files?.length) {
        // Prefer 2x webp
        const file =
          host.files.find((f) => f.name === "2x.webp") ||
          host.files.find((f) => f.name === "1x.webp") ||
          host.files[0];
        if (file) {
          emotes.push({
            id: e.id,
            code: e.name,
            url: `https:${host.url}/${file.name}`,
          });
        }
      }
    }

    return emotes;
  } catch {
    return [];
  }
}

/** Clear the emote cache (e.g., on shutdown). */
export function clearEmoteCache(): void {
  emoteCache.clear();
}
