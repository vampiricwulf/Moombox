/**
 * Twitch URL/ID extraction utilities
 */

export type TwitchTarget =
  | { type: "channel"; login: string }
  | { type: "vod"; vodId: string }
  | { type: "clip"; clipId: string };

/**
 * Extract Twitch target from various URL formats or raw input.
 * Supports:
 * - twitch.tv/{login}
 * - twitch.tv/videos/{id}
 * - twitch.tv/{login}/video/{id}
 * - twitch.tv/{login}/clip/{slug}
 * - clips.twitch.tv/{slug}
 * - Raw username (1-25 chars, alphanumeric + underscore)
 * - Raw numeric ID prefixed with "v" → VOD
 */
export function extractTwitchTarget(input: string): TwitchTarget | null {
  const trimmed = input.trim();
  if (!trimmed) return null;

  // Try URL patterns first
  try {
    // Normalize: add protocol if missing
    let urlStr = trimmed;
    if (!urlStr.startsWith("http://") && !urlStr.startsWith("https://")) {
      if (urlStr.startsWith("twitch.tv") || urlStr.startsWith("www.twitch.tv") || urlStr.startsWith("clips.twitch.tv")) {
        urlStr = "https://" + urlStr;
      }
    }

    if (urlStr.startsWith("http")) {
      const url = new URL(urlStr);
      const host = url.hostname.replace("www.", "");

      // clips.twitch.tv/{slug}
      if (host === "clips.twitch.tv") {
        const slug = url.pathname.split("/").filter(Boolean)[0];
        if (slug) return { type: "clip", clipId: slug };
      }

      if (host === "twitch.tv") {
        const parts = url.pathname.split("/").filter(Boolean);

        // /videos/{id}
        if (parts[0] === "videos" && parts[1] && /^\d+$/.test(parts[1])) {
          return { type: "vod", vodId: parts[1] };
        }

        // /{login}/video/{id}
        if (parts.length >= 3 && parts[1] === "video" && /^\d+$/.test(parts[2])) {
          return { type: "vod", vodId: parts[2] };
        }

        // /{login}/clip/{slug}
        if (parts.length >= 3 && parts[1] === "clip" && parts[2]) {
          return { type: "clip", clipId: parts[2] };
        }

        // /{login} (but not reserved paths)
        const reserved = new Set(["directory", "downloads", "jobs", "settings", "videos", "search", "p"]);
        if (parts.length >= 1 && parts[0] && !reserved.has(parts[0].toLowerCase())) {
          const login = parts[0].toLowerCase();
          if (/^[a-zA-Z0-9_]{1,25}$/.test(login)) {
            return { type: "channel", login };
          }
        }
      }
    }
  } catch {
    // Not a valid URL, try raw patterns below
  }

  // Raw VOD ID with "v" prefix (e.g., "v2345678901")
  if (/^v\d{1,12}$/.test(trimmed)) {
    return { type: "vod", vodId: trimmed.slice(1) };
  }

  // Raw numeric ID → VOD (only if clearly numeric and long enough to be a VOD)
  if (/^\d{7,12}$/.test(trimmed)) {
    return { type: "vod", vodId: trimmed };
  }

  // Raw username (1-25 alphanumeric + underscore, must start with letter)
  if (/^[a-zA-Z][a-zA-Z0-9_]{0,24}$/.test(trimmed)) {
    return { type: "channel", login: trimmed.toLowerCase() };
  }

  return null;
}

/**
 * Extract Twitch channel login from a Job object.
 * Tries job.url first, then falls back to channelName.
 */
export function extractTwitchLoginFromJob(job: { url?: string; channelName?: string }): string | null {
  if (job.url) {
    const match = job.url.match(/twitch\.tv\/([a-zA-Z0-9_]+)/);
    if (match) return match[1].toLowerCase();
  }
  if (job.channelName && job.channelName !== "Unknown") {
    return job.channelName.toLowerCase();
  }
  return null;
}
