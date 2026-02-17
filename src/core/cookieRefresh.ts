import ms from "ms";
import { Logger } from "./logger.js";
import { ConfigManager } from "./config.js";
import { CookieJar, verifyYouTubeAuth, verifyTwitchAuth } from "./cookies.js";
import { AutoCookieService } from "./autoCookies.js";
import fs from "fs-extra";
import { fetchWithTimeout } from "./http.js";
import { USER_AGENTS, YOUTUBE_URLS, WEB_CLIENT } from "../constants.js";

export interface PlatformValidation {
  hasCookies: boolean;
  authenticated: boolean;
  lastValidated: Date | null;
}

export interface ValidationState {
  youtube: PlatformValidation;
  twitch: PlatformValidation;
}

/**
 * Cookie Refresh Service
 *
 * Keeps YouTube and Twitch session cookies alive by periodically making authenticated requests.
 * Validates authentication with real API calls (not just cookie presence checks).
 * Triggers auto-reacquisition when a configured platform loses auth.
 */
export class CookieRefreshService {
  private static instance: CookieRefreshService;
  private logger: Logger;
  private interval: NodeJS.Timeout | null = null;
  private running: boolean = false;

  // Refresh every 30 minutes (YouTube sessions typically last longer, but this is safe)
  private readonly REFRESH_INTERVAL_MS = ms("30m");

  private _validationState: ValidationState = {
    youtube: { hasCookies: false, authenticated: false, lastValidated: null },
    twitch: { hasCookies: false, authenticated: false, lastValidated: null },
  };

  private constructor() {
    this.logger = Logger.getInstance();
  }

  static getInstance(): CookieRefreshService {
    if (!CookieRefreshService.instance) {
      CookieRefreshService.instance = new CookieRefreshService();
    }
    return CookieRefreshService.instance;
  }

  /**
   * Get a copy of the current validation state
   */
  get validationState(): ValidationState {
    return {
      youtube: { ...this._validationState.youtube },
      twitch: { ...this._validationState.twitch },
    };
  }

  /**
   * Start the cookie refresh service
   */
  start(): void {
    if (this.running) return;

    const config = ConfigManager.getInstance().get();
    if (!config.downloader.cookie_file) {
      this.logger.debug(
        "[CookieRefresh] No cookie file configured, skipping refresh service",
      );
      return;
    }

    this.running = true;
    this.logger.info("[CookieRefresh] Starting cookie refresh service");

    // Do an initial refresh
    this.refresh().catch((e) => {
      this.logger.warn(`[CookieRefresh] Initial refresh failed: ${e.stack || e.message}`);
    });

    // Schedule periodic refreshes
    this.interval = setInterval(() => {
      this.refresh().catch((e) => {
        this.logger.warn(`[CookieRefresh] Refresh failed: ${e.stack || e.message}`);
      });
    }, this.REFRESH_INTERVAL_MS);
  }

  /**
   * Stop the cookie refresh service
   */
  stop(): void {
    if (this.interval) {
      clearInterval(this.interval);
      this.interval = null;
    }
    this.running = false;
    this.logger.info("[CookieRefresh] Stopped cookie refresh service");
  }

  /**
   * Perform a cookie refresh cycle:
   * 1. Reload cookies from file
   * 2. Validate YouTube auth with real API call
   * 3. Validate Twitch auth with real API call
   * 4. Refresh YouTube session cookies via Set-Cookie
   * 5. Trigger auto-reacquisition for configured platforms that lost auth
   */
  async refresh(): Promise<boolean> {
    const config = ConfigManager.getInstance().get();
    if (!config.downloader.cookie_file) {
      return false;
    }

    try {
      // Reload cookies from file to get the latest
      const cookieHeader = await CookieJar.load(
        config.downloader.cookie_file,
        true,
      );

      const now = new Date();

      // Update hasCookies flags from format checks
      this._validationState.youtube.hasCookies = CookieJar.hasAuthCookies();
      this._validationState.twitch.hasCookies = CookieJar.hasTwitchAuthCookies();

      // Validate both platforms with real API calls (in parallel)
      const [ytAuth, twAuth] = await Promise.all([
        this._validationState.youtube.hasCookies ? verifyYouTubeAuth() : Promise.resolve(false),
        this._validationState.twitch.hasCookies ? verifyTwitchAuth() : Promise.resolve(false),
      ]);

      this._validationState.youtube.authenticated = ytAuth;
      this._validationState.youtube.lastValidated = now;
      this._validationState.twitch.authenticated = twAuth;
      this._validationState.twitch.lastValidated = now;

      // Refresh YouTube session cookies via Set-Cookie if auth is valid
      if (ytAuth && cookieHeader) {
        await this.refreshYouTubeSession(config.downloader.cookie_file, cookieHeader);
      }

      // Determine which configured platforms need recovery.
      // A platform needs recovery when it's in the platforms list (meaning
      // it was set up at some point) and it now lacks cookies or failed auth.
      const autoCookiesEnabled = config.auto_cookies?.enabled === true;
      const configuredPlatforms = config.auto_cookies?.platforms ?? [];
      const platformsNeedingRecovery: ("youtube" | "twitch")[] = [];

      if (autoCookiesEnabled) {
        for (const platform of configuredPlatforms) {
          const state = this._validationState[platform];
          if (!state.hasCookies || !state.authenticated) {
            platformsNeedingRecovery.push(platform);
          }
        }

        // Also trigger if no cookies at all (file deleted) and auto_cookies is enabled,
        // even if platforms list is empty (handles pre-migration setups)
        if (platformsNeedingRecovery.length === 0
          && !this._validationState.youtube.hasCookies
          && !this._validationState.twitch.hasCookies) {
          platformsNeedingRecovery.push("youtube"); // YouTube is always the primary platform
        }
      }

      if (platformsNeedingRecovery.length > 0) {
        const reasons = platformsNeedingRecovery.map((p) => {
          const state = this._validationState[p];
          return !state.hasCookies ? `${p} missing` : `${p} auth failed`;
        });
        this.logger.info(
          `[CookieRefresh] Auto-cookie refresh needed (${reasons.join(", ")})`,
        );
        const refreshed = await this.tryAutoCookieRefresh();

        if (refreshed) {
          // Re-validate after auto-refresh
          await CookieJar.load(config.downloader.cookie_file, true);
          this._validationState.youtube.hasCookies = CookieJar.hasAuthCookies();
          this._validationState.twitch.hasCookies = CookieJar.hasTwitchAuthCookies();

          const [ytAuth2, twAuth2] = await Promise.all([
            this._validationState.youtube.hasCookies ? verifyYouTubeAuth() : Promise.resolve(false),
            this._validationState.twitch.hasCookies ? verifyTwitchAuth() : Promise.resolve(false),
          ]);

          const revalidateTime = new Date();
          this._validationState.youtube.authenticated = ytAuth2;
          this._validationState.youtube.lastValidated = revalidateTime;
          this._validationState.twitch.authenticated = twAuth2;
          this._validationState.twitch.lastValidated = revalidateTime;
        }

        // Flag re-login for configured platforms that still lack auth after refresh attempt
        const autoCookies = AutoCookieService.getInstance();
        for (const platform of platformsNeedingRecovery) {
          if (!this._validationState[platform].authenticated) {
            autoCookies.flagManualRelogin(platform);
          }
        }
      }

      const ytStatus = this._validationState.youtube;
      const twStatus = this._validationState.twitch;
      this.logger.debug(
        `[CookieRefresh] Validation: YT(cookies=${ytStatus.hasCookies}, auth=${ytStatus.authenticated}) ` +
        `TW(cookies=${twStatus.hasCookies}, auth=${twStatus.authenticated})`,
      );

      return ytAuth || twAuth;
    } catch (e: any) {
      this.logger.warn(`[CookieRefresh] Refresh error: ${e.stack || e.message}`);
      return false;
    }
  }

  /**
   * Convenience method: run a refresh cycle and return the validation state.
   */
  async recheck(): Promise<ValidationState> {
    await this.refresh();
    return this.validationState;
  }

  /**
   * Refresh YouTube session cookies by making an authenticated request
   * and updating the cookie file with any Set-Cookie headers.
   */
  private async refreshYouTubeSession(cookieFilePath: string, cookieHeader: string): Promise<void> {
    try {
      const authHeader = CookieJar.generateAuthorizationHeader();

      const response = await fetchWithTimeout(
        `${YOUTUBE_URLS.API}/guide?prettyPrint=false`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            "User-Agent": USER_AGENTS.WEB,
            Cookie: cookieHeader,
            Origin: YOUTUBE_URLS.BASE,
            Referer: `${YOUTUBE_URLS.BASE}/`,
            ...(authHeader ? { Authorization: authHeader } : {}),
            "X-Origin": YOUTUBE_URLS.BASE,
          },
          body: JSON.stringify({
            context: {
              client: {
                clientName: WEB_CLIENT.context.clientName,
                clientVersion: WEB_CLIENT.context.clientVersion,
                hl: "en",
              },
            },
          }),
        },
      );

      if (response.ok) {
        const setCookies = response.headers.getSetCookie?.();
        if (setCookies && setCookies.length > 0) {
          await this.updateCookieFile(cookieFilePath, setCookies);
        }
        this.logger.debug(
          `[CookieRefresh] YouTube session refreshed (HTTP ${response.status})`,
        );
      }
    } catch (e: any) {
      this.logger.debug(`[CookieRefresh] YouTube session refresh error: ${e.message}`);
    }
  }

  /**
   * Update the cookie file with new cookies from Set-Cookie headers
   */
  private async updateCookieFile(
    cookieFilePath: string,
    setCookies: string[],
  ): Promise<void> {
    try {
      // Read existing cookie file
      const content = await fs.readFile(cookieFilePath, "utf-8");
      const lines = content.split("\n");

      // Parse Set-Cookie headers and extract cookie name/value pairs
      const newCookies = new Map<string, { value: string; expiry: number }>();

      for (const setCookie of setCookies) {
        // Parse: NAME=VALUE; Domain=...; Path=...; Expires=...; ...
        const parts = setCookie.split(";");
        if (parts.length === 0) continue;

        const [nameValue] = parts;
        const eqIndex = nameValue.indexOf("=");
        if (eqIndex === -1) continue;

        const name = nameValue.substring(0, eqIndex).trim();
        const value = nameValue.substring(eqIndex + 1).trim();

        // Check for expiry
        let expiry = Math.floor(Date.now() / 1000) + ms("365d") / 1000; // Default 1 year
        for (const part of parts.slice(1)) {
          const trimmed = part.trim().toLowerCase();
          if (trimmed.startsWith("expires=")) {
            const dateStr = part.trim().substring(8);
            const date = new Date(dateStr);
            if (!isNaN(date.getTime())) {
              expiry = Math.floor(date.getTime() / 1000);
            }
          } else if (trimmed.startsWith("max-age=")) {
            const maxAge = parseInt(part.trim().substring(8), 10);
            if (!isNaN(maxAge)) {
              expiry = Math.floor(Date.now() / 1000) + maxAge;
            }
          }
        }

        // Only process YouTube/Google cookies
        if (
          setCookie.toLowerCase().includes("youtube.com") ||
          setCookie.toLowerCase().includes("google.com")
        ) {
          newCookies.set(name, { value, expiry });
        }
      }

      if (newCookies.size === 0) {
        return; // No relevant cookies to update
      }

      // Update existing lines with new cookie values
      const updatedLines: string[] = [];
      const updatedCookies = new Set<string>();

      for (const line of lines) {
        if (line.startsWith("#") || !line.trim()) {
          updatedLines.push(line);
          continue;
        }

        const parts = line.split("\t");
        if (parts.length >= 7) {
          const name = parts[5].trim();

          if (newCookies.has(name)) {
            // Update this cookie with new value and expiry
            const { value, expiry } = newCookies.get(name)!;
            parts[4] = expiry.toString();
            parts[6] = value;
            updatedLines.push(parts.join("\t"));
            updatedCookies.add(name);
          } else {
            updatedLines.push(line);
          }
        } else {
          updatedLines.push(line);
        }
      }

      // Add any new cookies that weren't in the file
      for (const [name, { value, expiry }] of newCookies) {
        if (!updatedCookies.has(name)) {
          // Add new cookie line in Netscape format
          // domain, include_subdomains, path, secure, expiry, name, value
          const secure = name.startsWith("__Secure-") ? "TRUE" : "FALSE";
          const domain = name.includes("GOOGLE")
            ? ".google.com"
            : ".youtube.com";
          updatedLines.push(
            `${domain}\tTRUE\t/\t${secure}\t${expiry}\t${name}\t${value}`,
          );
          this.logger.debug(`[CookieRefresh] Added new cookie: ${name}`);
        }
      }

      // Write updated cookie file
      await fs.writeFile(cookieFilePath, updatedLines.join("\n"), "utf-8");
      // Set restrictive permissions (owner read/write only)
      await fs.chmod(cookieFilePath, 0o600);
      this.logger.debug(
        `[CookieRefresh] Updated ${newCookies.size} cookies in file`,
      );
    } catch (e: any) {
      this.logger.warn(`[CookieRefresh] Failed to update cookie file: ${e.stack || e.message}`);
    }
  }

  /**
   * If AutoCookieService is enabled, attempt a headless browser cookie refresh.
   */
  private async tryAutoCookieRefresh(): Promise<boolean> {
    const autoCookies = AutoCookieService.getInstance();
    if (!autoCookies.isConfigured()) return false;

    this.logger.info("[CookieRefresh] Auth cookies missing — attempting auto-cookie refresh...");
    const refreshed = await autoCookies.refreshCookies();
    if (refreshed) {
      this.logger.info("[CookieRefresh] Auto-cookie refresh succeeded");
    } else {
      this.logger.warn("[CookieRefresh] Auto-cookie refresh failed");
    }
    return refreshed;
  }

  /**
   * Check if the service is running
   */
  isRunning(): boolean {
    return this.running;
  }
}
