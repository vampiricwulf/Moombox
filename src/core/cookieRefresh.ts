import ms from "ms";
import { Logger } from "./logger.js";
import { createComponentLogger } from "./structuredLogger.js";
import { ConfigManager } from "./config.js";
import { CookieJar } from "./cookies.js";
import { AutoCookieService } from "./autoCookies.js";
import fs from "fs-extra";
import { fetchWithTimeout } from "./http.js";

/**
 * Cookie Refresh Service
 *
 * Keeps YouTube session cookies alive by periodically making authenticated requests.
 * This prevents cookies from expiring due to inactivity.
 *
 * YouTube session cookies typically expire after extended periods of inactivity.
 * By making periodic requests with the cookies, we refresh the session.
 */
export class CookieRefreshService {
  private static instance: CookieRefreshService;
  private logger: Logger;
  private interval: NodeJS.Timeout | null = null;
  private running: boolean = false;

  // Refresh every 30 minutes (YouTube sessions typically last longer, but this is safe)
  private readonly REFRESH_INTERVAL_MS = ms("30m");

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
   * Perform a cookie refresh by making an authenticated request to YouTube
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

      if (!cookieHeader || !CookieJar.hasAuthCookies()) {
        this.logger.debug("[CookieRefresh] No auth cookies present");
        return this.tryAutoCookieRefresh();
      }

      // Generate authorization header
      const authHeader = CookieJar.generateAuthorizationHeader();

      // Make an authenticated request to YouTube
      // Using the /youtubei/v1/guide endpoint which is lightweight and requires auth
      const response = await fetchWithTimeout(
        "https://www.youtube.com/youtubei/v1/guide?prettyPrint=false",
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            "User-Agent":
              "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
            Cookie: cookieHeader,
            Origin: "https://www.youtube.com",
            Referer: "https://www.youtube.com/",
            ...(authHeader ? { Authorization: authHeader } : {}),
            "X-Origin": "https://www.youtube.com",
          },
          body: JSON.stringify({
            context: {
              client: {
                clientName: "WEB",
                clientVersion: "2.20241212.01.00",
                hl: "en",
                gl: "US",
              },
            },
          }),
        },
      );

      const slog = createComponentLogger("CookieRefresh");

      if (response.ok) {
        // Check for Set-Cookie headers (new cookies from server)
        const setCookies = response.headers.getSetCookie?.();
        if (setCookies && setCookies.length > 0) {
          await this.updateCookieFile(
            config.downloader.cookie_file,
            setCookies,
          );
        }

        this.logger.debug(
          `[CookieRefresh] Session refreshed successfully (HTTP ${response.status})`,
        );
        slog.info({
          event: "cookie_refresh_success",
          httpStatus: response.status,
          cookiesUpdated: setCookies?.length || 0,
        });
        return true;
      } else {
        this.logger.warn(
          `[CookieRefresh] Refresh request failed: HTTP ${response.status}`,
        );
        slog.warn({
          event: "cookie_refresh_failed",
          httpStatus: response.status,
          reason: "http_error",
        });
        return false;
      }
    } catch (e: any) {
      this.logger.warn(`[CookieRefresh] Refresh error: ${e.stack || e.message}`);
      createComponentLogger("CookieRefresh").error({
        event: "cookie_refresh_failed",
        reason: "exception",
        error: e.message,
      });
      return false;
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
