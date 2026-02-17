/**
 * Configuration type definitions
 */

/**
 * Channel-specific configuration
 */
export interface ChannelConfig {
  id: string;
  name?: string;
  platform?: "youtube" | "twitch"; // defaults to "youtube"
  /** Whether this channel is actively monitored (defaults to true when undefined) */
  enabled?: boolean;
  /** Filter pattern - can be a simple string (regex) or object with named patterns for backwards compatibility */
  terms?: string | Record<string, string>;
  num_desc_lookbehind?: number;
  output_directory?: string;
  include_non_live_content?: boolean;
  max_feed_items?: number;
  /** Twitch-specific: quality preference for HLS recording */
  qualityPreference?: "best" | "720p" | "480p" | "audio_only";
}

/**
 * Downloader settings
 */
export interface DownloaderConfig {
  max_video_resolution?: number;
  ffmpeg_path?: string;
  staging_directory?: string;
  output_directory?: string;
  output_template?: string;
  num_parallel_downloads?: number;
  po_token?: string;
  visitor_data?: string;
  cookie_file?: string;
  pot_provider_url?: string;
  download_chat?: boolean;
  prefer_60fps?: boolean;
  segment_retry_delay_cap?: number;
  segment_live_check_retries?: number;
}

/**
 * Task list display settings
 */
export interface TasklistConfig {
  /** Age in days (or ms format like "30d") after which finished jobs are archived */
  hide_finished_age_days?: number | string;
}

/**
 * Notification endpoint configuration
 */
export interface NotificationConfig {
  url?: string;
  tags?: string[];
  events?: string[];  // Optional event filter, e.g. ["found", "live", "finished", "error"]
}

/**
 * Automatic cookie acquisition settings
 */
export interface AutoCookiesConfig {
  enabled?: boolean;
  browser_profile_dir?: string;
}

/**
 * Complete application configuration
 */
export interface MoomboxConfig {
  port?: number;
  network_access?: string; // "localhost" | "lan" | "external"
  log_level?: string;
  log_file_path?: string;
  log_max_file_size?: number;
  log_max_files?: number;
  database_path?: string;
  max_feed_items?: number;
  /** Feed check interval in minutes (or ms format like "10m") */
  feed_check_interval?: number | string;
  downloader: DownloaderConfig;
  tasklist?: TasklistConfig;
  auto_cookies?: AutoCookiesConfig;
  notifications?: NotificationConfig[];
  channels?: ChannelConfig[];
}

/**
 * Template variables for output filename
 */
export interface TemplateVariables {
  title: string;
  id: string;
  channel: string;
  date?: Date;
}
