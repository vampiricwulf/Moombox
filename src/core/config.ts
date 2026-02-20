import fs from "fs-extra";
import path from "path";
import { parse as parseToml, stringify as stringifyToml } from "smol-toml";
import os from "os";
import type {
  ChannelConfig,
  MoomboxConfig,
} from "../types/config.js";
import { Logger } from "./logger.js";
import { isScryptHash, hashPassword } from "./auth.js";

export type { ChannelConfig, MoomboxConfig, AutoCookiesConfig } from "../types/config.js";

/**
 * Normalized config type where time values are always numbers (not strings).
 * Used internally after applyDefaults() normalizes the values.
 */
export type NormalizedConfig = Omit<MoomboxConfig, 'feed_check_interval' | 'tasklist'> & {
  feed_check_interval: number;
  password_hash?: string;
  tasklist?: {
    hide_finished_age_days: number;
  };
};

export class ConfigManager {
  private static instance: ConfigManager;
  private config: NormalizedConfig | null = null;
  private configPath: string = "";
  private configLoaded: boolean = false;
  private saveQueue: Promise<void> = Promise.resolve();

  // Default configuration values (normalized with numeric time values)
  private static readonly DEFAULTS: NormalizedConfig = {
    port: 774,
    network_access: "localhost",
    log_level: "INFO",
    log_file_path: "./moombox.log",
    log_max_file_size: 10485760, // 10MB
    log_max_files: 5,
    database_path: "./moombox.json",
    max_feed_items: 15,
    feed_check_interval: 10, // minutes
    downloader: {
      output_directory: "./output",
      output_template: "${channel}/${start_date} ${title} [${id}]",
      staging_directory: "./staging",
      num_parallel_downloads: 2,
      max_video_resolution: 1080, // Based on max(width, height)
      cookie_file: "./cookies.txt",
      download_chat: true, // Download live chat alongside streams
      prefer_60fps: true, // Prefer 60fps when same resolution available
      segment_retry_delay_cap: 60, // Max backoff delay in seconds
      segment_live_check_retries: 16, // Retries before first live status check
    },
    tasklist: {
      hide_finished_age_days: 30, // days
    },
    auto_cookies: {
      enabled: false,
      browser_profile_dir: "./browser-profile",
      platforms: [],
    },
  };

  private constructor() {}

  /** Log helper that falls back to console if Logger isn't ready */
  private static log(level: "info" | "warn" | "error", msg: string): void {
    try {
      const logger = Logger.getInstance();
      logger[level](msg);
    } catch {
      console[level](msg);
    }
  }

  /**
   * Parse a time value that can be a number (original unit), a string (ms format like "10m"),
   * or undefined (use default).
   *
   * For backward compatibility:
   * - If value is a number, it's returned as-is (in the original unit: minutes or days)
   * - If value is a string, it's parsed as a duration (e.g. "10m") and converted back to the original unit
   * - If value is undefined, the default is returned
   *
   * @param value - The value from config (can be string, number, or undefined)
   * @param defaultValue - The default value in original unit
   * @param unit - The unit for the return value ('minutes' | 'days')
   * @returns Value in the original unit (minutes or days)
   */
  private static parseTimeValue(
    value: string | number | undefined,
    defaultValue: number,
    unit: 'minutes' | 'days'
  ): number {
    if (value === undefined) {
      return defaultValue;
    }

    if (typeof value === 'number') {
      // Numeric value is already in the correct unit
      return value;
    }

    // String value - parse duration string (e.g. "10m", "7d") and convert to target unit
    const match = value.trim().match(/^(\d+(?:\.\d+)?)\s*(ms|s|m|h|d|w)$/i);
    if (!match) {
      ConfigManager.log('warn', `Invalid time value: ${value}, using default`);
      return defaultValue;
    }
    const num = parseFloat(match[1]);
    const multipliers: Record<string, number> = { ms: 1, s: 1_000, m: 60_000, h: 3_600_000, d: 86_400_000, w: 604_800_000 };
    const parsed = num * (multipliers[match[2].toLowerCase()] ?? 1);

    // Convert milliseconds to target unit
    if (unit === 'minutes') {
      return parsed / 60_000;
    } else if (unit === 'days') {
      return parsed / 86_400_000;
    }

    return defaultValue;
  }

  static getInstance(): ConfigManager {
    if (!ConfigManager.instance) {
      ConfigManager.instance = new ConfigManager();
    }
    return ConfigManager.instance;
  }

  /**
   * Get the default configuration values
   */
  static getDefaults(): NormalizedConfig {
    return JSON.parse(JSON.stringify(ConfigManager.DEFAULTS));
  }

  /**
   * Merge loaded config with defaults, using defaults for any missing values
   */
  private applyDefaults(config: Partial<MoomboxConfig>): NormalizedConfig {
    const defaults = ConfigManager.getDefaults();

    // Migrate old allow_lan / allow_external booleans → network_access string
    let networkAccess = config.network_access;
    if (!networkAccess) {
      const raw = config as Record<string, unknown>;
      if (raw.allow_lan !== undefined || raw.allow_external !== undefined) {
        const lan = raw.allow_lan === true;
        const ext = lan && raw.allow_external === true;
        networkAccess = ext ? "external" : lan ? "lan" : "localhost";
      }
    }

    return {
      port: config.port ?? defaults.port,
      network_access: networkAccess ?? defaults.network_access,
      password_hash: config.password_hash,
      log_level: config.log_level ?? defaults.log_level,
      log_file_path: config.log_file_path ?? defaults.log_file_path,
      log_max_file_size: config.log_max_file_size ?? defaults.log_max_file_size,
      log_max_files: config.log_max_files ?? defaults.log_max_files,
      database_path: config.database_path ?? defaults.database_path,
      max_feed_items: config.max_feed_items ?? defaults.max_feed_items,
      feed_check_interval: ConfigManager.parseTimeValue(
        config.feed_check_interval,
        defaults.feed_check_interval as number,
        'minutes'
      ),
      decapi_check_interval: config.decapi_check_interval,
      twitch_check_interval: config.twitch_check_interval,
      downloader: {
        output_directory:
          config.downloader?.output_directory ??
          defaults.downloader.output_directory,
        output_template:
          config.downloader?.output_template ??
          defaults.downloader.output_template,
        staging_directory:
          config.downloader?.staging_directory ??
          defaults.downloader.staging_directory,
        num_parallel_downloads:
          config.downloader?.num_parallel_downloads ??
          defaults.downloader.num_parallel_downloads,
        max_video_resolution:
          config.downloader?.max_video_resolution ??
          defaults.downloader.max_video_resolution,
        cookie_file:
          config.downloader?.cookie_file ?? defaults.downloader.cookie_file,
        download_chat:
          config.downloader?.download_chat !== undefined
            ? config.downloader.download_chat
            : defaults.downloader.download_chat,
        prefer_60fps:
          config.downloader?.prefer_60fps !== undefined
            ? config.downloader.prefer_60fps
            : defaults.downloader.prefer_60fps,
        segment_retry_delay_cap:
          config.downloader?.segment_retry_delay_cap ?? defaults.downloader.segment_retry_delay_cap,
        segment_live_check_retries:
          config.downloader?.segment_live_check_retries ?? defaults.downloader.segment_live_check_retries,
        // These have no defaults - only use if explicitly set
        ffmpeg_path: config.downloader?.ffmpeg_path,
        po_token: config.downloader?.po_token,
        visitor_data: config.downloader?.visitor_data,
        pot_provider_url: config.downloader?.pot_provider_url,
      },
      tasklist: {
        hide_finished_age_days: ConfigManager.parseTimeValue(
          config.tasklist?.hide_finished_age_days,
          defaults.tasklist?.hide_finished_age_days as number,
          'days'
        ),
      },
      auto_cookies: {
        enabled:
          config.auto_cookies?.enabled !== undefined
            ? config.auto_cookies.enabled
            : defaults.auto_cookies?.enabled,
        browser_profile_dir:
          config.auto_cookies?.browser_profile_dir ??
          defaults.auto_cookies?.browser_profile_dir,
        platforms:
          config.auto_cookies?.platforms ??
          defaults.auto_cookies?.platforms,
      },
      notifications: config.notifications,
      channels: config.channels,
    };
  }

  /**
   * Validate config values and replace invalid ones with defaults.
   */
  private validate(config: NormalizedConfig): NormalizedConfig {
    const defaults = ConfigManager.DEFAULTS;
    const warn = (field: string, val: unknown) =>
      ConfigManager.log("warn", `[Config] Invalid ${field} (${val}), using default`);

    if (!Number.isInteger(config.port) || config.port! < 1 || config.port! > 65535) {
      warn("port", config.port); config.port = defaults.port;
    }
    if (!["localhost", "lan", "external"].includes(config.network_access!)) {
      warn("network_access", config.network_access); config.network_access = defaults.network_access;
    }
    if (typeof config.max_feed_items !== "number" || config.max_feed_items < 1) {
      warn("max_feed_items", config.max_feed_items); config.max_feed_items = defaults.max_feed_items;
    }
    if (typeof config.feed_check_interval !== "number" || config.feed_check_interval < 1) {
      warn("feed_check_interval", config.feed_check_interval); config.feed_check_interval = defaults.feed_check_interval;
    }
    if (config.decapi_check_interval !== undefined && (typeof config.decapi_check_interval !== "number" || config.decapi_check_interval < 15 || config.decapi_check_interval > 3600)) {
      warn("decapi_check_interval", config.decapi_check_interval); config.decapi_check_interval = undefined;
    }
    if (config.twitch_check_interval !== undefined && (typeof config.twitch_check_interval !== "number" || config.twitch_check_interval < 5 || config.twitch_check_interval > 3600)) {
      warn("twitch_check_interval", config.twitch_check_interval); config.twitch_check_interval = undefined;
    }
    if (typeof config.log_max_file_size !== "number" || config.log_max_file_size! < 1) {
      warn("log_max_file_size", config.log_max_file_size); config.log_max_file_size = defaults.log_max_file_size;
    }
    if (typeof config.log_max_files !== "number" || config.log_max_files! < 1) {
      warn("log_max_files", config.log_max_files); config.log_max_files = defaults.log_max_files;
    }
    const d = config.downloader;
    if (typeof d.num_parallel_downloads !== "number" || d.num_parallel_downloads < 1) {
      warn("downloader.num_parallel_downloads", d.num_parallel_downloads);
      d.num_parallel_downloads = defaults.downloader.num_parallel_downloads;
    }
    if (typeof d.max_video_resolution !== "number" || d.max_video_resolution! < 1) {
      warn("downloader.max_video_resolution", d.max_video_resolution);
      d.max_video_resolution = defaults.downloader.max_video_resolution;
    }
    return config;
  }

  async load(customPath?: string): Promise<MoomboxConfig> {
    // Search paths: custom -> ./config.toml -> ./config/config.toml -> ~/.config/moombox/config.toml
    const paths = [
      customPath,
      path.join(process.cwd(), "config.toml"),
      path.join(process.cwd(), "config", "config.toml"),
      path.join(os.homedir(), ".config", "moombox", "config.toml"),
    ].filter((p): p is string => !!p);

    for (const p of paths) {
      if (await fs.pathExists(p)) {
        ConfigManager.log("info", `[Config] Loading from ${p}`);
        const content = await fs.readFile(p, "utf-8");
        const loadedConfig = parseToml(content) as unknown as Partial<MoomboxConfig>;
        this.config = this.validate(this.applyDefaults(loadedConfig));
        this.configPath = p;
        this.configLoaded = true;

        // Auto-convert plaintext password to scrypt hash
        if (this.config.password_hash && !isScryptHash(this.config.password_hash)) {
          ConfigManager.log("info", "[Config] Plaintext password detected, converting to secure hash");
          this.config.password_hash = hashPassword(this.config.password_hash);
        }

        // Write back to persist any missing config entries added by defaults
        const updatedToml = this.configToToml(this.config);
        if (updatedToml.trim() !== content.trim()) {
          await fs.writeFile(p, updatedToml, "utf-8");
        }

        return this.config;
      }
    }

    ConfigManager.log("warn", "[Config] No config file found. Using defaults.");
    this.configPath = path.join(process.cwd(), "config.toml");
    this.config = ConfigManager.getDefaults();
    return this.config;
  }

  get(): NormalizedConfig {
    if (!this.config) {
      throw new Error("Config not loaded");
    }
    return this.config;
  }

  hasConfig(): boolean {
    return this.configLoaded;
  }

  getConfigPath(): string {
    return this.configPath;
  }

  async save(config: MoomboxConfig): Promise<void> {
    const result = this.saveQueue.then(async () => {
      // Apply defaults to ensure all settings are written to TOML
      this.config = this.applyDefaults(config);

      // Ensure we have a config path
      if (!this.configPath) {
        this.configPath = path.join(process.cwd(), "config.toml");
      }

      // Convert config to TOML format
      const tomlContent = this.configToToml(this.config);

      // Ensure directory exists
      const dir = path.dirname(this.configPath);
      await fs.ensureDir(dir);

      // Write to file
      await fs.writeFile(this.configPath, tomlContent, "utf-8");
      // Set restrictive permissions (owner read/write only)
      await fs.chmod(this.configPath, 0o600);
      this.configLoaded = true;
      ConfigManager.log("info", `[Config] Saved configuration to ${this.configPath}`);
    });
    this.saveQueue = result.then(() => {}, (e) => {
      ConfigManager.log("error", `[Config] Save failed: ${e instanceof Error ? e.message : String(e)}`);
    });
    return result;
  }

  private configToToml(config: MoomboxConfig): string {
    // Strip undefined values (TOML has no null/undefined)
    const clean = JSON.parse(JSON.stringify(config));

    return stringifyToml(clean) + "\n";
  }

  static resolveTemplate(
    template: string,
    data: { title: string; id: string; channel: string; date?: Date },
  ): string {
    const safeTitle = data.title
      .replace(
        /[^\w\s\-\u3000-\u303F\u3040-\u309F\u30A0-\u30FF\uFF00-\uFFEF\u4E00-\u9FAF]/g,
        "",
      )
      .trim();
    const safeChannel = data.channel
      .replace(
        /[^\w\s\-\u3000-\u303F\u3040-\u309F\u30A0-\u30FF\uFF00-\uFFEF\u4E00-\u9FAF]/g,
        "",
      )
      .trim();

    let result = template;

    // Simple Replace
    result = result.replace(/\$\{title\}/g, safeTitle);
    result = result.replace(/\$\{id\}/g, data.id);
    result = result.replace(/\$\{channel\}/g, safeChannel);

    const d = data.date || new Date();
    const yyyy = d.getFullYear();
    const mm = String(d.getMonth() + 1).padStart(2, "0");
    const dd = String(d.getDate()).padStart(2, "0");
    const HH = String(d.getHours()).padStart(2, "0");
    const MM = String(d.getMinutes()).padStart(2, "0");

    result = result.replace(/\$\{start_date\}/g, `${yyyy}${mm}${dd}`);
    result = result.replace(/\$\{start_time\}/g, `${HH}${MM}`);

    return result;
  }
}
