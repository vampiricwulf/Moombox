/**
 * Zod validation schemas for API request validation
 * Provides type-safe runtime validation with automatic TypeScript inference
 */

import { z } from "zod";

// ============================================================================
// Common Schemas
// ============================================================================

/**
 * YouTube video ID: exactly 11 characters, alphanumeric + underscore + hyphen
 */
export const videoIdSchema = z
  .string()
  .length(11)
  .regex(/^[a-zA-Z0-9_-]{11}$/, "Invalid video ID format");

/**
 * YouTube channel ID: starts with UC, 24 characters total
 */
export const channelIdSchema = z
  .string()
  .regex(/^UC[a-zA-Z0-9_-]{22}$/, "Invalid channel ID format");

/**
 * Twitch channel login: 1-25 alphanumeric + underscore
 */
export const twitchLoginSchema = z
  .string()
  .min(1).max(25)
  .regex(/^[a-zA-Z0-9_]+$/, "Invalid Twitch username");

/**
 * Twitch VOD ID: numeric, variable length
 */
export const twitchVodIdSchema = z
  .string()
  .regex(/^\d{1,12}$/, "Invalid Twitch VOD ID");

/**
 * Twitch quality preference
 */
export const twitchQualitySchema = z.enum(["best", "720p", "480p", "audio_only"]);

/**
 * Platform identifier
 */
export const platformSchema = z.enum(["youtube", "twitch"]).default("youtube");

/**
 * YouTube format itag: -1 (none) or 1-999
 */
export const itagSchema = z
  .number()
  .int()
  .min(-1, "Itag must be -1 (none) or positive")
  .max(999, "Itag must be <= 999");

/**
 * Timestamp in seconds: 0 to 1 year (31536000 seconds)
 */
export const timestampSchema = z
  .number()
  .min(0, "Timestamp cannot be negative")
  .max(31536000, "Timestamp cannot exceed 1 year");

/**
 * File path validation (prevents traversal, absolute paths)
 */
export const safePathSchema = z
  .string()
  .min(1)
  .refine(
    (path) => !path.includes("..") && !path.startsWith("/") && !path.startsWith("\\") && !/^[A-Za-z]:/.test(path),
    "Path cannot contain .. or be absolute"
  );

// ============================================================================
// Job Routes Schemas
// ============================================================================

/**
 * POST /api/jobs - Add new YouTube job
 */
export const addYouTubeJobSchema = z
  .object({
    platform: z.literal("youtube").default("youtube"),
    videoId: videoIdSchema,
    selectedVideoItag: itagSchema.optional(),
    selectedAudioItag: itagSchema.optional(),
    startTime: timestampSchema.optional(),
    endTime: timestampSchema.optional(),
  })
  .refine(
    (data) => {
      // Both formats cannot be -1 (would download nothing)
      if (data.selectedVideoItag === -1 && data.selectedAudioItag === -1) {
        return false;
      }
      return true;
    },
    { message: "Cannot select both video-only and audio-only (both -1)" }
  )
  .refine(
    (data) => {
      // End time must be after start time
      if (data.startTime !== undefined && data.endTime !== undefined) {
        return data.endTime > data.startTime;
      }
      return true;
    },
    { message: "End time must be after start time", path: ["endTime"] }
  );

/**
 * POST /api/jobs - Add new Twitch job
 */
export const addTwitchJobSchema = z.object({
  platform: z.literal("twitch"),
  videoId: z.string().min(1),  // channel login or VOD ID
  twitchType: z.enum(["channel", "vod"]).default("channel"),
  quality_preference: twitchQualitySchema.optional(),
});

/**
 * POST /api/jobs - Add new job (YouTube or Twitch)
 *
 * Tries Twitch schema first (requires explicit platform: "twitch"),
 * falls back to YouTube schema (platform defaults to "youtube").
 */
export const addJobSchema = z.union([addTwitchJobSchema, addYouTubeJobSchema]);

export type AddJobRequest = z.infer<typeof addJobSchema>;
export type AddYouTubeJobRequest = z.infer<typeof addYouTubeJobSchema>;
export type AddTwitchJobRequest = z.infer<typeof addTwitchJobSchema>;

/**
 * GET /api/jobs - List jobs with pagination
 */
// ============================================================================
// Trim Routes Schemas
// ============================================================================

/**
 * POST /api/jobs/:id/trims - Create trim
 */
export const createTrimSchema = z
  .object({
    startTime: z.number().finite(),
    endTime: z.number().finite(),
  })
  .refine((data) => data.startTime >= 0, {
    message: "Start time cannot be negative",
    path: ["startTime"],
  })
  .refine((data) => data.endTime > data.startTime, {
    message: "End time must be after start time",
    path: ["endTime"],
  })
  .refine((data) => data.endTime - data.startTime >= 1, {
    message: "Trim must be at least 1 second",
    path: ["endTime"],
  });

export type CreateTrimRequest = z.infer<typeof createTrimSchema>;

// ============================================================================
// Config Routes Schemas
// ============================================================================

/**
 * PUT /api/config - Update configuration
 */
export const updateConfigSchema = z.object({
  port: z.number().int().min(1).max(65535).optional(),
  network_access: z.enum(["localhost", "lan", "external"]).optional(),
  log_level: z.enum(["DEBUG", "INFO", "WARN", "ERROR"]).optional(),
  log_file_path: safePathSchema.optional(),
  log_max_file_size: z.number().int().min(1024).max(1073741824).optional(), // 1KB to 1GB
  log_max_files: z.number().int().min(1).max(100).optional(),
  database_path: safePathSchema.optional(),
  max_feed_items: z.number().int().min(1).max(100).optional(),
  feed_check_interval: z.union([z.number().min(1), z.string()]).optional(),
  decapi_check_interval: z.number().int().min(15).max(3600).optional(),
  twitch_check_interval: z.number().int().min(5).max(3600).optional(),

  downloader: z
    .object({
      output_directory: safePathSchema.optional(),
      output_template: z.string().min(1).max(500).optional(),
      staging_directory: safePathSchema.optional(),
      num_parallel_downloads: z.number().int().min(1).max(10).optional(),
      max_video_resolution: z.number().int().min(1).optional(),
      prefer_60fps: z.boolean().optional(),
      download_chat: z.boolean().optional(),
      cookie_file: safePathSchema.optional(),
      ffmpeg_path: z.string().optional(),
      po_token: z.string().optional(),
      visitor_data: z.string().optional(),
      pot_provider_url: z.string().optional(),
      segment_retry_delay_cap: z.number().min(0).optional(),
      segment_live_check_retries: z.number().int().min(0).optional(),
    })
    .optional(),

  tasklist: z
    .object({
      hide_finished_age_days: z.union([z.number().int().min(0).max(3650), z.string()]).optional(),
    })
    .optional(),

  auto_cookies: z
    .object({
      enabled: z.boolean().optional(),
      browser_profile_dir: safePathSchema.optional(),
      platforms: z.array(z.enum(["youtube", "twitch"])).optional(),
    })
    .optional(),

  notifications: z.array(z.object({
    url: z.string().optional(),
    tags: z.array(z.string()).optional(),
    events: z.array(z.string()).optional(),
  })).optional(),

  channels: z.array(z.object({
    id: z.string().min(1),
    name: z.string().optional(),
    platform: z.enum(["youtube", "twitch"]).optional(),
    enabled: z.boolean().optional(),
    terms: z.union([z.string(), z.record(z.string(), z.string())]).optional(),
    num_desc_lookbehind: z.number().int().min(0).max(100).optional(),
    output_directory: z.string().optional(),
    include_non_live_content: z.boolean().optional(),
    max_feed_items: z.number().int().min(1).max(100).optional(),
    quality_preference: twitchQualitySchema.optional(),
  })).optional(),
});

export type UpdateConfigRequest = z.infer<typeof updateConfigSchema>;

/**
 * POST /api/config/channels - Add or update YouTube channel
 */
export const addYouTubeChannelSchema = z.object({
  platform: z.literal("youtube").default("youtube"),
  id: channelIdSchema,
  name: z.string().optional(),
  terms: z.union([z.string(), z.record(z.string(), z.string())]).optional(),
  include_non_live_content: z.boolean().optional(),
  num_desc_lookbehind: z.number().int().min(0).max(100).optional(),
  output_directory: z.string().optional(),
  max_feed_items: z.number().int().min(1).max(100).optional(),
});

/**
 * POST /api/config/channels - Add or update Twitch channel
 */
export const addTwitchChannelSchema = z.object({
  platform: z.literal("twitch"),
  id: twitchLoginSchema,         // Twitch login name (e.g., "shroud")
  name: z.string().optional(),    // Display name override
  terms: z.union([z.string(), z.record(z.string(), z.string())]).optional(),
  output_directory: z.string().optional(),
  quality_preference: twitchQualitySchema.optional(),
});

/**
 * POST /api/config/channels - Add or update channel (YouTube or Twitch)
 */
export const addChannelSchema = z.union([addTwitchChannelSchema, addYouTubeChannelSchema]);

export type AddChannelRequest = z.infer<typeof addChannelSchema>;

// ============================================================================
// Auth Routes Schemas
// ============================================================================

/**
 * POST /api/auth/login - Login request
 */
export const loginSchema = z.object({
  password: z.string().min(1).max(128),
});

export type LoginRequest = z.infer<typeof loginSchema>;

/**
 * POST /api/auth/set-password - Set or change password
 */
export const setPasswordSchema = z.object({
  currentPassword: z.string().max(128).optional(),
  newPassword: z.string().min(8).max(128),
});

export type SetPasswordRequest = z.infer<typeof setPasswordSchema>;

/**
 * POST /api/auth/remove-password - Remove password
 */
export const removePasswordSchema = z.object({
  currentPassword: z.string().min(1).max(128),
});

export type RemovePasswordRequest = z.infer<typeof removePasswordSchema>;

// ============================================================================
// Validation Helper Functions
// ============================================================================

/**
 * Formats Zod validation errors into user-friendly messages
 * @param error Zod validation error
 * @returns Object mapping field paths to error messages
 */
export function formatValidationErrors(error: z.ZodError): Record<string, string> {
  const formatted: Record<string, string> = {};
  for (const issue of error.issues) {
    const path = issue.path.join(".");
    formatted[path || "_global"] = issue.message;
  }
  return formatted;
}
