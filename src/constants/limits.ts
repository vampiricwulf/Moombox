/**
 * Moombox Limits Constants
 *
 * Centralized limit values used throughout the application.
 */

export const LIMITS = {
  /** Maximum number of parallel segment downloads during catch-up */
  MAX_PARALLEL_SEGMENTS: 6,

  /** Maximum retry attempts for failed segment downloads */
  MAX_SEGMENT_RETRIES: 5,

  /** Number of segments behind before triggering parallel download mode */
  SEGMENT_BEHIND_THRESHOLD: 10,

  /** Maximum chat messages kept in memory before flushing to disk */
  CHAT_MESSAGES_IN_MEMORY: 5_000,

  /** Chat message count threshold that triggers flush to disk */
  CHAT_MESSAGES_FLUSH_THRESHOLD: 50_000,

  /** Maximum items per page for API pagination */
  MAX_ITEMS_PER_PAGE: 100,

  /** Default items per page for API pagination */
  DEFAULT_ITEMS_PER_PAGE: 50,
} as const;
