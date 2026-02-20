/**
 * Twitch-related type definitions
 */

/**
 * Live stream information from Twitch GQL API
 */
export interface TwitchStreamInfo {
  streamId: string;           // Twitch stream session ID (numeric)
  channelLogin: string;       // Lowercase login name
  channelDisplayName: string; // Display name with capitalization
  channelId: string;          // Numeric Twitch user ID
  title: string;              // Stream title
  gameCategory?: string;      // Current game/category
  thumbnailUrl?: string;      // Preview image URL
  viewerCount?: number;
  startedAt?: string;         // ISO timestamp
  profileImageUrl?: string;   // Channel avatar URL
  isLive: boolean;
  streamType?: "live" | "rerun";
  // HLS access
  accessToken?: { value: string; signature: string };
  hlsMasterUrl?: string;      // Usher HLS master playlist URL
}

/**
 * HLS variant stream from Twitch master playlist
 */
export interface TwitchHlsVariant {
  url: string;
  name: string;           // "720p60", "480p30", "audio_only", "chunked" (Source)
  bandwidth: number;
  width?: number;
  height?: number;
  fps?: number;
  isSource: boolean;
}

/**
 * VOD information from Twitch GQL API
 */
export interface TwitchVodInfo {
  vodId: string;              // Numeric VOD ID
  title: string;
  channelLogin: string;
  channelDisplayName: string;
  duration: number;           // seconds
  thumbnailUrl?: string;
  createdAt: string;          // ISO timestamp
  viewCount?: number;
  gameCategory?: string;
}

/**
 * Twitch chat message (IRC-based)
 */
export interface TwitchChatMessage {
  id: string;                    // IRC message UUID
  timestampMs: number;           // tmi-sent-ts
  offsetMs: number;              // relative to recording start (when Moombox began downloading)
  authorName: string;            // display-name
  authorId: string;              // user-id (numeric)
  authorBadges: string[];        // ["subscriber/12", "moderator/1"]
  authorColor?: string;          // hex color
  message: string;               // raw text
  emotes?: Array<{ id: string; name: string; start: number; end: number }>;
  bits?: number;                 // cheer amount
  messageType: TwitchMessageType;
  raw: string;                   // lossless raw IRC line
}

export type TwitchMessageType =
  | "chat"
  | "sub"
  | "resub"
  | "subgift"
  | "raid"
  | "bits"
  | "system";

/**
 * Twitch chat data stored alongside video
 */
export interface TwitchChatData {
  platform: "twitch";
  channelLogin: string;
  channelDisplayName: string;
  streamId: string;
  streamStartTime?: string;      // ISO timestamp — when the Twitch broadcast began
  recordingStartTime?: string;   // ISO timestamp — when Moombox began downloading (video start)
  downloadedAt: string;          // ISO timestamp
  messageCount: number;
  messages: TwitchChatMessage[];
  emotes?: TwitchEmoteData;
}

/**
 * Cached emote data for offline rendering
 */
export interface TwitchEmoteData {
  bttv?: Array<{ id: string; code: string; url: string }>;
  ffz?: Array<{ id: number; code: string; url: string }>;
  seventv?: Array<{ id: string; code: string; url: string }>;
}

/**
 * GQL access token response
 */
export interface TwitchAccessToken {
  value: string;
  signature: string;
}
