/**
 * Moombox Timeout Constants
 *
 * Centralized timeout values used throughout the application (in milliseconds).
 */

export const TIMEOUTS = {
  /** Timeout for individual segment downloads (30 seconds) */
  SEGMENT_DOWNLOAD: 30000,

  /** Timeout for YouTube API requests (15 seconds) */
  API_REQUEST: 15000,

  /** Feed monitor check interval (10 minutes) */
  FEED_MONITOR: 600000,

  /** Stream probe interval when waiting for live (5 seconds) */
  STREAM_PROBE: 5000,

  /** Throttle interval for job update broadcasts (100ms) */
  JOB_UPDATE_THROTTLE: 100,

  /** Worker job queue check interval (2 seconds) */
  WORKER_CHECK_INTERVAL: 2000,
} as const;
