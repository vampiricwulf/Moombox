/**
 * YouTube-related type definitions
 */

/**
 * Stream status indicating the current state of a YouTube video
 */
export type StreamStatus =
  | "live" // Currently live streaming
  | "upcoming" // Scheduled but not started
  | "vod" // Video on demand (finished stream or regular video)
  | "post_live" // Post-live DVR available
  | "not_a_stream"; // Regular video, not a stream

/**
 * Playability error types from YouTube API
 */
export type PlayabilityError =
  | "ok" // Playable
  | "members_only" // Requires membership
  | "login_required" // Requires login
  | "age_restricted" // Age verification needed
  | "unavailable" // Video unavailable
  | "private" // Private video
  | "region_blocked" // Geographic restriction
  | "unknown"; // Unknown error

/**
 * Video format information from YouTube API
 */
export interface Format {
  itag: number;
  url?: string;
  mimeType: string;
  bitrate: number;
  width?: number;
  height?: number;
  contentLength?: string;
  qualityLabel?: string;
  audioQuality?: string;
  audioSampleRate?: string;
  fps?: number;
  source?: string;     // Client that produced this format (e.g., "android_vr", "tv_auth")
  authLevel?: number;  // Authentication cost — lower = less auth, used as tiebreaker
}

/**
 * Complete video information from YouTube
 */
export interface VideoInfo {
  title: string;
  channelName: string;
  channelId: string;
  description: string;
  thumbnailUrl?: string;
  formats: Format[];
  playerUrl: string;
  streamStatus: StreamStatus;
  isLive: boolean;
  isUpcoming: boolean;
  isPostLiveDVR: boolean;
  lengthSeconds?: number;      // Video duration in seconds (from videoDetails.lengthSeconds)
  endTimestamp?: string;        // ISO timestamp — when stream ended (from liveBroadcastDetails.endTimestamp)
  scheduledStartTime?: string; // ISO timestamp for upcoming streams
  dashManifestUrl?: string;
  hlsManifestUrl?: string;
  playabilityError?: PlayabilityError;
  playabilityReason?: string;
}

/**
 * Basic video metadata (lighter than full VideoInfo)
 */
export interface VideoMetadata {
  title: string;
  channelName: string;
  channelId: string;
  thumbnailUrl?: string;
  streamStatus: StreamStatus;
  isLive: boolean;
  isUpcoming: boolean;
}

/**
 * YouTube client configuration for API requests
 */
export interface YouTubeClientConfig {
  clientName: string;
  clientVersion: string;
  clientId: string;
  userAgent: string;
  context: {
    clientName: string;
    clientVersion: string;
    hl?: string;
    platform?: string;
    clientScreen?: string;
    deviceModel?: string;
    osVersion?: string;
    androidSdkVersion?: number;
    deviceMake?: string;
  };
}

/**
 * Extracted configuration from YouTube watch page
 */
export interface YtcfgData {
  playerUrl: string;
  visitorData: string;
  sessionIndex: number | null;
  delegatedSessionId: string | null;
  dataSyncId: string | null;
  title: string | null;
  author: string | null;
  channelId: string | null;
  description: string | null;
  thumbnailUrl: string | null;
}

/**
 * DASH stream representation
 */
export interface DashStream {
  id: string;
  mimeType: string;
  codecs: string;
  bandwidth: number;
  width?: number;
  height?: number;
  baseUrl: string;
  initialization?: string;
  startNumber: number;
  segmentTemplate?: string;
  segmentTimeline?: { d: number; r?: number }[];
}

/**
 * HLS playlist types
 */
export interface HlsVariant {
  bandwidth: number;
  width?: number;
  height?: number;
  url: string;
  codecs?: string;
}

export interface HlsPlaylist {
  type: "master" | "media";
  variants?: HlsVariant[];
  segments?: HlsSegment[];
  targetDuration?: number;
  mediaSequence?: number;
  endList?: boolean;
}

export interface HlsSegment {
  uri: string;
  duration: number;
  sequence: number;
}

/**
 * Create a minimal valid VideoInfo for error/cancellation paths.
 * Prevents NPEs when callers access fields on error-path returns.
 */
export function createEmptyVideoInfo(videoId?: string): VideoInfo {
  return {
    title: "Unknown",
    channelName: "Unknown",
    channelId: "",
    description: "",
    formats: [],
    playerUrl: "",
    streamStatus: "not_a_stream",
    isLive: false,
    isUpcoming: false,
    isPostLiveDVR: false,
  };
}
