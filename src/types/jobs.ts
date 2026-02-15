/**
 * Job-related type definitions
 */

import type { ChatStatus } from "./chat.js";

/**
 * Job status in the download pipeline
 */
export type JobStatus =
  | "Upcoming" // Waiting for stream to go live
  | "Live" // Currently downloading live stream
  | "Downloading" // Downloading VOD or post-live
  | "Muxing" // Combining video and audio streams
  | "Finished" // Download complete
  | "Error" // Download failed
  | "Cancelled" // User cancelled
  | "COOKIES?"; // Requires updated cookies (member content)

/**
 * Trimmed version record (derivative file)
 */
export interface TrimRecord {
  id: string;              // Unique trim ID (timestamp-based: "trim_1708042800000")
  startTime: number;       // Start time in seconds
  endTime: number;         // End time in seconds
  filename: string;        // Relative filename: "{base} [30s-60s].mp4"
  createdAt: string;       // ISO timestamp
  duration: number;        // Trim duration (endTime - startTime)
  fileSize?: number;       // File size in bytes (populated after creation)
}

/**
 * Job record stored in database
 */
export interface Job {
  id: string; // Video ID (primary key)
  videoId: string; // Same as id, kept for compatibility
  url: string;
  title: string;
  channelName: string;
  status: JobStatus;
  progress: string; // e.g. "(A: 10/100 V: 10/100)" for live, "45.3%" for VOD
  percent: number; // 0-100 for progress bar
  eta: string;
  speed: string;
  createdAt: string; // ISO Date
  updatedAt: string;
  error?: string;
  // Download state tracking
  lastVideoSeq?: number;
  lastAudioSeq?: number;
  totalVideoSeq?: number;
  totalAudioSeq?: number;
  lastRecheckAt?: string;
  isVod?: boolean;
  manuallyAdded?: boolean;
  allowNonStream?: boolean; // From include_non_live_content setting
  streamStartTime?: string; // ISO timestamp of when stream started (for filename)
  lengthSeconds?: number;    // Video duration in seconds (from YouTube)
  streamEndTime?: string;    // ISO timestamp — when stream ended
  // Media info
  thumbnailUrl?: string;
  description?: string;
  outputFile?: string;
  filename?: string;
  outputDirectory?: string;
  // Format metadata
  videoWidth?: number;
  videoHeight?: number;
  videoFps?: number;
  fileSize?: number;            // Final output file size in bytes
  downloadStartedAt?: string;   // ISO timestamp — when status changed to "Downloading"
  // Chat tracking
  chatStatus?: ChatStatus;
  totalChatMessages?: number;
  chatFilename?: string; // Relative path to chat.json
  // Segment gap tracking (segments lost during parallel catch-up)
  gaps?: Array<{ from: number; to: number; stream: "video" | "audio" }>;
  // Advanced download options (manual format selection)
  selectedVideoItag?: number;   // User-chosen video format itag
  selectedAudioItag?: number;   // User-chosen audio format itag
  // Timestamp selection (segment-level download range)
  startTime?: number;           // Start time in seconds (0 = beginning)
  endTime?: number;             // End time in seconds (undefined = end of stream)
  // Derivative Trimmed Versions
  trims?: TrimRecord[];         // Trimmed versions of this video
}

/**
 * Partial job update for database operations
 */
export type JobUpdate = Partial<Omit<Job, "id" | "videoId" | "createdAt">>;

/**
 * Data required to create a new job
 */
export interface NewJobData {
  videoId: string;
  url: string;
  title: string;
  channelName: string;
  thumbnailUrl?: string;
  description?: string;
  outputDirectory?: string;
  manuallyAdded?: boolean;
  allowNonStream?: boolean; // From include_non_live_content setting
  // Advanced download options
  selectedVideoItag?: number;
  selectedAudioItag?: number;
  startTime?: number;
  endTime?: number;
}

/**
 * Download progress event data
 */
export interface DownloadProgress {
  seq: number;
  bytes?: number;
  total?: number;
  speed?: number;
  eta?: number;
}
