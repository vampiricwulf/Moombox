/**
 * Unified media ID extraction — dispatches to YouTube or Twitch extractors
 */

import { extractVideoId } from "./youtube.js";
import { extractTwitchTarget, type TwitchTarget } from "./twitch.js";

export type MediaTarget =
  | { platform: "youtube"; videoId: string }
  | { platform: "twitch"; target: TwitchTarget };

/**
 * Extract a media target from user input (URL or ID).
 * Tries YouTube extraction first (more specific patterns),
 * then falls back to Twitch.
 */
export function extractMediaId(input: string): MediaTarget | null {
  const trimmed = input.trim();
  if (!trimmed) return null;

  // Check for explicit Twitch URLs first (they'd fail YouTube extraction anyway)
  if (trimmed.includes("twitch.tv") || trimmed.includes("clips.twitch.tv")) {
    const twitchTarget = extractTwitchTarget(trimmed);
    if (twitchTarget) return { platform: "twitch", target: twitchTarget };
  }

  // Try YouTube
  const videoId = extractVideoId(trimmed);
  if (videoId) return { platform: "youtube", videoId };

  // Try Twitch (raw usernames, VOD IDs, etc.)
  const twitchTarget = extractTwitchTarget(trimmed);
  if (twitchTarget) return { platform: "twitch", target: twitchTarget };

  return null;
}
