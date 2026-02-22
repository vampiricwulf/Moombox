/**
 * Chat-related type definitions for live chat downloading
 */

/**
 * A single part of a chat message (text or emoji)
 */
export interface MessagePart {
  type: "text" | "emoji";
  text?: string;
  emojiId?: string;
  emojiUrl?: string;
  isCustomEmoji?: boolean;
}

/**
 * Super Chat / Super Sticker information
 */
export interface SuperchatInfo {
  amount: string;
  currency: string;
  color: string;
  tier: number;
}

/**
 * A single chat message
 */
export interface ChatMessage {
  id: string;
  timestampUsec: string; // Microseconds since epoch
  timestampText: string; // Human readable, e.g., "1:23:45"
  offsetMs: number; // Offset from stream start (for sync)
  authorName: string;
  authorChannelId: string;
  authorBadges: string[]; // ["moderator", "member", "owner", "verified"]
  message: MessagePart[];
  superchat?: SuperchatInfo;
  isMembership?: boolean; // Membership join/milestone message
}

/**
 * Complete chat data for a video
 */
export interface ChatData {
  videoId: string;
  videoTitle: string;
  channelName: string;
  streamStartTime?: string; // ISO timestamp
  downloadedAt: string; // ISO timestamp
  messageCount: number;
  messages: ChatMessage[];
}

/**
 * Resume state for chat downloading
 */
export interface ChatResumeState {
  lastTimestampUsec: string;
  messageCount: number;
  continuation: string;
  timestamp: number; // When state was saved
  videoId: string; // For validation
  recentIds?: string[]; // Bounded dedup IDs — avoids reloading chat file on resume
}

/**
 * Chat downloader options
 */
export interface ChatDownloaderOptions {
  videoId: string;
  videoTitle: string;
  channelName: string;
  outputFile: string; // Path to chat.json in staging
  initialContinuation: string;
  apiKey: string;
  visitorData?: string;
  cookieHeader?: string;
  isReplay?: boolean; // VOD replay vs live
  isLiveOrUpcoming?: boolean; // Stream is live/upcoming — keep retrying on "complete"
  streamStartTime?: string; // ISO timestamp for calculating offsetMs
  resumeFile?: string;
}

/**
 * Chat API response structure
 */
export interface ChatApiResponse {
  messages: ChatMessage[];
  nextContinuation: string | null;
  timeoutMs: number;
  isComplete: boolean;
}

/**
 * Chat progress event data
 */
export interface ChatProgress {
  messageCount: number;
  lastTimestamp?: string;
}

/**
 * Chat status for job tracking
 */
export type ChatStatus =
  | "pending"
  | "downloading"
  | "finished"
  | "error"
  | "unavailable";
