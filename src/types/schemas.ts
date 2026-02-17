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
    (path) => !path.includes("..") && !path.startsWith("/") && !path.startsWith("\\"),
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
  qualityPreference: twitchQualitySchema.optional(),
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
export const listJobsQuerySchema = z.object({
  limit: z.coerce.number().int().min(1).max(1000).default(100),
  offset: z.coerce.number().int().min(0).default(0),
  status: z.string().optional(), // Comma-separated status filters
});

export type ListJobsQuery = z.infer<typeof listJobsQuerySchema>;

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
  log_level: z.enum(["DEBUG", "INFO", "WARN", "ERROR"]).optional(),
  log_file_path: safePathSchema.optional(),
  log_max_file_size: z.number().int().min(1024).max(1073741824).optional(), // 1KB to 1GB
  log_max_files: z.number().int().min(1).max(100).optional(),
  database_path: safePathSchema.optional(),
  max_feed_items: z.number().int().min(1).max(100).optional(),

  downloader: z
    .object({
      output_directory: safePathSchema,
      output_template: z.string().min(1).max(500),
      staging_directory: safePathSchema.optional(),
      num_parallel_downloads: z.number().int().min(1).max(10).optional(),
      max_video_resolution: z.number().int().min(144).max(8192).optional(),
      prefer_60fps: z.boolean().optional(),
      download_chat: z.boolean().optional(),
      cookie_file: safePathSchema.optional(),
    })
    .strict(),

  tasklist: z
    .object({
      hide_finished_age_days: z.number().int().min(0).max(3650).optional(), // 0 to 10 years
    })
    .optional(),

  network_access: z.enum(["loopback", "lan", "external"]).optional(),

  auto_cookies: z
    .object({
      enabled: z.boolean().optional(),
      browser_profile_dir: safePathSchema.optional(),
    })
    .optional(),
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
  qualityPreference: twitchQualitySchema.optional(),
});

/**
 * POST /api/config/channels - Add or update channel (YouTube or Twitch)
 */
export const addChannelSchema = z.union([addTwitchChannelSchema, addYouTubeChannelSchema]);

export type AddChannelRequest = z.infer<typeof addChannelSchema>;

// ============================================================================
// Import Routes Schemas
// ============================================================================

/**
 * POST /api/import - Import archive headers
 */
export const importHeadersSchema = z.object({
  "X-Import-Title": z.string().max(500).optional(),
  "X-Import-Channel": z.string().max(500).optional(),
});

export type ImportHeaders = z.infer<typeof importHeadersSchema>;

// ============================================================================
// POT Routes Schemas
// ============================================================================

/**
 * POST /get_pot - Generate PO token
 */
export const getPotSchema = z.object({
  content_binding: z.string().optional(),
  // Deprecated parameters that should be rejected
  data_sync_id: z.never().optional(),
  visitor_data: z.never().optional(),
});

export type GetPotRequest = z.infer<typeof getPotSchema>;

// ============================================================================
// Validation Helper Functions
// ============================================================================

/**
 * Validates request body against schema and returns result
 * @param schema Zod schema to validate against
 * @param data Data to validate
 * @returns Success with parsed data or error with validation errors
 */
export function validateRequest<T>(
  schema: z.ZodSchema<T>,
  data: unknown
): { success: true; data: T } | { success: false; errors: z.ZodIssue[] } {
  const result = schema.safeParse(data);
  if (result.success) {
    return { success: true, data: result.data };
  }
  return { success: false, errors: result.error.issues };
}

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
