/**
 * Automatic Cookie Acquisition Service
 *
 * Automates the cookie lifecycle for YouTube authentication:
 * - Initial setup: launches a browser for user to log in to Google
 * - Silent refresh: headless browser visit to refresh cookies on auth failure
 *
 * Supports Firefox (primary, SQLite cookies) and Edge/Chrome (fallback, CDP).
 */

import { spawn, exec } from "child_process";
import type { ChildProcess } from "child_process";
import net from "net";
import fs from "fs-extra";
import path from "path";
import { Logger } from "./logger.js";
import { ConfigManager } from "./config.js";
import { CookieJar } from "./cookies.js";
import { fetchWithTimeout } from "./http.js";
import { detectBrowser, type DetectedBrowser } from "./browserDetect.js";
import { CdpClient } from "./cdpClient.js";
import {
  cdpCookiesToNetscape,
  firefoxCookiesToNetscape,
  type CdpCookie,
  type FirefoxCookieRow,
} from "./cookieConverter.js";

export interface AutoCookieStatus {
  configured: boolean;
  setupInProgress: boolean;
  browser: DetectedBrowser | null;
  lastRefresh: string | null;
  lastError: string | null;
}

// Firefox user.js preferences to suppress first-run dialogs
const FIREFOX_USER_JS = `user_pref("browser.aboutwelcome.enabled", false);
user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("browser.startup.homepage_override.mstone", "ignore");
user_pref("datareporting.policy.dataSubmissionPolicyBypassNotification", true);
user_pref("toolkit.telemetry.reportingpolicy.firstRun", false);
user_pref("browser.rights.3.shown", true);
`;

const LOGIN_URL = "https://accounts.google.com/ServiceLogin?service=youtube";
const REFRESH_URL = "https://www.youtube.com";
const PROCESS_TIMEOUT = 30000;

// Chromium lock files that prevent headless launch when a headed session was killed
const CHROMIUM_LOCK_FILES = ["lockfile", "SingletonLock", "SingletonSocket", "SingletonCookie"];

export class AutoCookieService {
  private static instance: AutoCookieService;

  private logger: Logger;
  private setupProcess: ChildProcess | null = null;
  private setupBrowser: DetectedBrowser | null = null;
  private browserExited: boolean = false;
  private cdpPort: number | null = null;
  private cancelled: boolean = false;
  private lastRefresh: Date | null = null;
  private lastError: string | null = null;

  private constructor() {
    this.logger = Logger.getInstance();
  }

  static getInstance(): AutoCookieService {
    if (!AutoCookieService.instance) {
      AutoCookieService.instance = new AutoCookieService();
    }
    return AutoCookieService.instance;
  }

  /**
   * Check if auto-cookies is configured and enabled
   */
  isConfigured(): boolean {
    try {
      const config = ConfigManager.getInstance().get();
      return config.auto_cookies?.enabled === true;
    } catch {
      return false;
    }
  }

  isSetupInProgress(): boolean {
    return this.setupProcess !== null;
  }

  getStatus(): AutoCookieStatus {
    return {
      configured: this.isConfigured(),
      setupInProgress: this.isSetupInProgress(),
      browser: detectBrowser(),
      lastRefresh: this.lastRefresh?.toISOString() ?? null,
      lastError: this.lastError,
    };
  }

  // ─── Headed Setup ────────────────────────────────────────────────

  /**
   * Step 1: Launch a browser for the user to log in to Google.
   * Returns immediately — the user logs in at their own pace.
   */
  async startSetup(): Promise<{ success: boolean; error?: string }> {
    if (this.setupProcess) {
      return { success: false, error: "Setup already in progress" };
    }

    const browser = detectBrowser();
    if (!browser) {
      return {
        success: false,
        error: "No supported browser found (Firefox, Edge, or Chrome required)",
      };
    }

    const profileDir = this.getProfileDir();
    await fs.ensureDir(profileDir);

    this.setupBrowser = browser;
    this.lastError = null;
    this.cancelled = false;

    try {
      if (browser.type === "firefox") {
        await this.startFirefoxSetup(browser, profileDir);
      } else {
        await this.startChromiumSetup(browser, profileDir);
      }
      return { success: true };
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      this.lastError = msg;
      this.cleanup();
      return { success: false, error: msg };
    }
  }

  /**
   * Step 2: User confirms they are logged in.
   * Extracts cookies, writes to cookie file, validates.
   */
  async finishSetup(): Promise<{
    success: boolean;
    authenticated: boolean;
    error?: string;
  }> {
    if (!this.setupProcess || !this.setupBrowser) {
      return { success: false, authenticated: false, error: "No setup in progress" };
    }

    if (this.cancelled) {
      return { success: false, authenticated: false, error: "Setup was cancelled" };
    }

    const browser = this.setupBrowser;
    const profileDir = this.getProfileDir();

    try {
      let netscapeCookies: string;

      if (browser.type === "firefox") {
        // Firefox cookies live in SQLite with WAL — we need a CLEAN exit
        // so Firefox checkpoints the WAL into cookies.sqlite before we read it.
        await this.closeFirefoxGracefully();
        netscapeCookies = await this.readFirefoxCookies(profileDir);
      } else {
        // Chromium: extract cookies via CDP from the running browser, then close it
        netscapeCookies = await this.extractChromiumCookies();
        await killProcess(this.setupProcess);
      }

      // Write cookies
      const cookiePath = this.getCookiePath();
      await fs.ensureDir(path.dirname(cookiePath));
      await fs.writeFile(cookiePath, netscapeCookies, "utf-8");
      // Set restrictive permissions (owner read/write only)
      await fs.chmod(cookiePath, 0o600);

      // Validate
      CookieJar.reset();
      await CookieJar.parse(cookiePath, true);
      const authenticated = CookieJar.hasAuthCookies();

      this.lastRefresh = new Date();
      this.cleanup();

      if (!authenticated) {
        return {
          success: true,
          authenticated: false,
          error: "Login not detected. Please try again and make sure you complete the Google login.",
        };
      }

      this.logger.info("[AutoCookies] Setup complete — authenticated successfully");
      return { success: true, authenticated: true };
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      this.lastError = msg;
      this.cleanup();
      return { success: false, authenticated: false, error: msg };
    }
  }

  /**
   * Cancel an in-progress setup
   */
  async cancelSetup(): Promise<void> {
    if (!this.setupProcess) return;

    this.logger.info("[AutoCookies] Setup cancelled");
    this.cancelled = true;

    // Try graceful CDP close for Chromium
    if (this.cdpPort) {
      try {
        const client = await CdpClient.connect(this.cdpPort);
        await client.closeBrowser();
        client.disconnect();
      } catch { /* already closed */ }
    }

    await killProcess(this.setupProcess);
    this.cleanup();
  }

  // ─── Headless Refresh ────────────────────────────────────────────

  /**
   * Silently refresh cookies using a headless browser visit.
   * Called automatically when a job hits COOKIES? status.
   */
  async refreshCookies(): Promise<boolean> {
    const browser = detectBrowser();
    if (!browser) {
      this.lastError = "No browser found for refresh";
      return false;
    }

    const profileDir = this.getProfileDir();
    if (!await fs.pathExists(profileDir)) {
      this.lastError = "Browser profile not found — run setup first";
      return false;
    }

    this.logger.info(`[AutoCookies] Refreshing cookies via ${browser.type}...`);

    try {
      let netscapeCookies: string;

      if (browser.type === "firefox") {
        netscapeCookies = await this.refreshFirefox(browser, profileDir);
      } else {
        netscapeCookies = await this.refreshChromium(browser, profileDir);
      }

      // Write cookies
      const cookiePath = this.getCookiePath();
      await fs.ensureDir(path.dirname(cookiePath));
      await fs.writeFile(cookiePath, netscapeCookies, "utf-8");
      // Set restrictive permissions (owner read/write only)
      await fs.chmod(cookiePath, 0o600);

      // Validate
      CookieJar.reset();
      await CookieJar.parse(cookiePath, true);
      const authenticated = CookieJar.hasAuthCookies();

      if (authenticated) {
        this.lastRefresh = new Date();
        this.lastError = null;
        this.logger.info("[AutoCookies] Cookie refresh succeeded");
        return true;
      }

      this.lastError = "Refresh completed but auth cookies not found";
      this.logger.warn("[AutoCookies] Refresh completed but auth cookies not found");
      return false;
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      this.lastError = msg;
      this.logger.warn(`[AutoCookies] Refresh failed: ${msg}`);
      return false;
    }
  }

  // ─── Firefox Flows ───────────────────────────────────────────────

  private async startFirefoxSetup(
    browser: DetectedBrowser,
    profileDir: string,
  ): Promise<void> {
    // Write user.js to suppress first-run dialogs
    await fs.writeFile(
      path.join(profileDir, "user.js"),
      FIREFOX_USER_JS,
      "utf-8",
    );

    this.browserExited = false;

    this.setupProcess = spawn(
      browser.path,
      ["-profile", profileDir, "-no-remote", LOGIN_URL],
      { stdio: "ignore", detached: false },
    );

    this.setupProcess.on("error", (err) => {
      this.logger.error(`[AutoCookies] Firefox process error: ${err.message}`);
      this.lastError = err.message;
    });

    this.setupProcess.on("close", () => {
      this.browserExited = true;
    });
  }

  private async refreshFirefox(
    browser: DetectedBrowser,
    profileDir: string,
  ): Promise<string> {
    // Use --screenshot to make headless Firefox load a page and exit
    const tempScreenshot = path.join(profileDir, "refresh-screenshot.png");

    const proc = spawn(
      browser.path,
      ["--screenshot", tempScreenshot, "-no-remote", "-profile", profileDir, REFRESH_URL],
      { stdio: "ignore", detached: false },
    );

    await waitForExit(proc, PROCESS_TIMEOUT);

    // Clean up screenshot
    await fs.remove(tempScreenshot).catch(() => {});

    return this.readFirefoxCookies(profileDir);
  }

  private async readFirefoxCookies(profileDir: string): Promise<string> {
    const dbPath = path.join(profileDir, "cookies.sqlite");

    if (!await fs.pathExists(dbPath)) {
      throw new Error("Firefox cookies.sqlite not found — browser may not have been used yet");
    }

    // Load sql.js with WASM
    const initSqlJs = (await import("sql.js")).default;
    const wasmBinary = await this.loadSqlWasm();
    const SQL = await initSqlJs(wasmBinary ? { wasmBinary } : undefined);

    // Read database with retries (file may be briefly locked)
    let dbBuffer: Buffer | null = null;
    for (let attempt = 0; attempt < 3; attempt++) {
      try {
        dbBuffer = await fs.readFile(dbPath);
        break;
      } catch {
        if (attempt < 2) await sleep(1000);
      }
    }

    if (!dbBuffer) {
      throw new Error("Could not read cookies.sqlite (file may be locked)");
    }

    const db = new SQL.Database(dbBuffer);
    try {
      const results = db.exec(
        "SELECT name, value, host, path, expiry, isHttpOnly, isSecure FROM moz_cookies",
      );

      if (!results.length || !results[0].values.length) {
        throw new Error("No cookies found in Firefox profile");
      }

      const rows: FirefoxCookieRow[] = results[0].values.map((row: any[]) => ({
        name: String(row[0]),
        value: String(row[1]),
        host: String(row[2]),
        path: String(row[3]),
        expiry: Number(row[4]),
        isHttpOnly: Number(row[5]),
        isSecure: Number(row[6]),
      }));

      return firefoxCookiesToNetscape(rows);
    } finally {
      db.close();
    }
  }

  private async loadSqlWasm(): Promise<Buffer | null> {
    // Try embedded assets first (SEA build)
    const assets = (globalThis as any).__MOOMBOX_ASSETS__;
    if (assets?.["sql-wasm.wasm"]) {
      return assets["sql-wasm.wasm"];
    }

    // Development: load from node_modules
    try {
      const wasmPath = require.resolve("sql.js/dist/sql-wasm.wasm");
      return await fs.readFile(wasmPath);
    } catch {
      // sql.js will try to fetch WASM itself
      return null;
    }
  }

  // ─── Chromium Flows (Edge/Chrome) ────────────────────────────────

  private async startChromiumSetup(
    browser: DetectedBrowser,
    profileDir: string,
  ): Promise<void> {
    // Launch with --remote-debugging-port so we can extract cookies via CDP.
    // --disable-blink-features=AutomationControlled removes navigator.webdriver
    // which is what triggers Google's "This browser or app may not be secure" block.
    //
    // We use a known port instead of 0 (random) because Edge's launcher process
    // may exit immediately after spawning the real browser process. This means
    // our child process closes before printing the CDP URL to stderr. With a
    // known port we can poll for CDP availability directly.
    this.browserExited = false;
    this.cdpPort = await getFreePort();

    await cleanChromiumLockFiles(profileDir);

    this.setupProcess = spawn(
      browser.path,
      [
        `--user-data-dir=${profileDir}`,
        "--no-first-run",
        "--no-default-browser-check",
        "--disable-blink-features=AutomationControlled",
        `--remote-debugging-port=${this.cdpPort}`,
        LOGIN_URL,
      ],
      { stdio: "ignore", detached: false },
    );

    this.setupProcess.on("error", (err) => {
      this.logger.error(`[AutoCookies] Browser process error: ${err.message}`);
      this.lastError = err.message;
    });

    this.setupProcess.on("close", () => {
      this.browserExited = true;
    });

    // Poll until CDP endpoint is available (the actual browser process
    // may take a moment to start listening after the launcher exits)
    await this.waitForCdp(this.cdpPort, 15000);
  }

  /**
   * Extract cookies from the running Chromium browser via CDP.
   */
  private async extractChromiumCookies(): Promise<string> {
    if (!this.cdpPort) {
      throw new Error("CDP port not available — browser may have closed");
    }

    const cookies = await this.cdpExtractCookies(this.cdpPort);
    return cdpCookiesToNetscape(cookies);
  }

  private async refreshChromium(
    browser: DetectedBrowser,
    profileDir: string,
  ): Promise<string> {
    await cleanChromiumLockFiles(profileDir);

    const port = await getFreePort();

    const proc = spawn(
      browser.path,
      [
        "--headless=new",
        `--user-data-dir=${profileDir}`,
        "--no-first-run",
        "--no-default-browser-check",
        "--disable-gpu",
        "--disable-session-crashed-bubble",
        "--disable-features=InfiniteSessionRestore",
        `--remote-debugging-port=${port}`,
      ],
      { stdio: "ignore", detached: false },
    );

    try {
      await this.waitForCdp(port, 15000);

      // Navigate to YouTube to refresh cookies
      let pageClient: CdpClient | null = null;
      try {
        pageClient = await CdpClient.connectToPage(port);
        await pageClient.navigate(REFRESH_URL);
        pageClient.disconnect();
      } catch {
        pageClient?.disconnect();
      }

      const cookies = await this.cdpExtractCookies(port);

      // Close browser and wait for exit
      const browserClient = await CdpClient.connect(port).catch(() => null);
      if (browserClient) {
        await browserClient.closeBrowser();
        browserClient.disconnect();
      }
      await waitForExit(proc, 5000).catch(() => {});

      return cdpCookiesToNetscape(cookies);
    } catch (e) {
      await killProcess(proc);
      throw e;
    }
  }

  /**
   * Extract cookies via CDP, trying browser-level then page-level connections.
   * The fallback chain in getAllCookies() handles Storage vs Network methods.
   * But Storage needs browser-level and Network needs page-level targets.
   */
  private async cdpExtractCookies(port: number): Promise<CdpCookie[]> {
    if (this.cancelled) throw new Error("Setup was cancelled");

    // Try browser-level first (Storage.getCookies works here)
    let client: CdpClient | null = null;
    try {
      client = await CdpClient.connect(port);
      const cookies = await client.getAllCookies();
      client.disconnect();
      if (cookies.length > 0) return cookies;
    } catch {
      client?.disconnect();
    }

    // Try page-level target (Network.getCookies works here)
    let pageClient: CdpClient | null = null;
    try {
      pageClient = await CdpClient.connectToPage(port);
      const cookies = await pageClient.getAllCookies();
      pageClient.disconnect();
      if (cookies.length > 0) return cookies;
    } catch {
      pageClient?.disconnect();
    }

    throw new Error("Failed to extract cookies via CDP — no working method found");
  }

  /**
   * Poll the CDP HTTP endpoint until it responds.
   * Edge's launcher process exits immediately after spawning the real browser,
   * so we can't rely on stderr parsing. Instead we poll the known port.
   */
  private async waitForCdp(port: number, timeoutMs: number): Promise<void> {
    const start = Date.now();
    while (Date.now() - start < timeoutMs) {
      if (this.cancelled) throw new Error("Setup was cancelled");
      try {
        const resp = await fetchWithTimeout(
          `http://127.0.0.1:${port}/json/version`,
          undefined,
          2000,
        );
        if (resp.ok) return; // CDP is ready
      } catch {
        // Not ready yet
      }
      await sleep(500);
    }
    throw new Error("Timeout waiting for CDP endpoint to become available");
  }

  // ─── Process Management ──────────────────────────────────────────

  /**
   * Gracefully close Firefox so it checkpoints WAL into cookies.sqlite.
   *
   * sql.js reads the database as a flat buffer and has NO access to the
   * WAL/SHM sidecar files. If Firefox is force-killed, uncommitted WAL
   * data (which typically includes all recent cookies) is invisible.
   *
   * Flow:
   * 1. If user already closed the browser → WAL is checkpointed, proceed
   * 2. Send graceful WM_CLOSE via `taskkill` (no /F) → Firefox shuts down
   *    cleanly, checkpointing WAL as part of its exit routine
   * 3. Wait up to 8 seconds for clean exit
   * 4. If still alive, force kill (cookies may be incomplete)
   */
  private async closeFirefoxGracefully(): Promise<void> {
    if (!this.setupProcess) return;

    // Already exited (user closed it) — WAL was checkpointed on clean exit
    if (this.browserExited) {
      this.logger.debug("[AutoCookies] Firefox already exited cleanly");
      await sleep(300); // Brief FS flush delay
      return;
    }

    const pid = this.setupProcess.pid;
    if (!pid) return;

    // Step 1: Send graceful close (WM_CLOSE) — no /F flag
    this.logger.debug("[AutoCookies] Sending graceful close to Firefox...");
    await new Promise<void>((resolve) => {
      exec(`taskkill /T /PID ${pid}`, () => resolve());
    });

    // Step 2: Wait for clean exit (up to 8 seconds)
    const exitedCleanly = await this.waitForProcessExit(8000);

    if (exitedCleanly) {
      this.logger.debug("[AutoCookies] Firefox exited cleanly (WAL checkpointed)");
      await sleep(300); // Brief FS flush delay
      return;
    }

    // Step 3: Graceful close didn't work — force kill as last resort
    this.logger.warn(
      "[AutoCookies] Firefox did not exit gracefully within 8s, force killing. " +
      "Some cookies in the WAL may not be readable.",
    );
    await new Promise<void>((resolve) => {
      exec(`taskkill /F /T /PID ${pid}`, () => resolve());
    });
    await sleep(500);
  }

  /**
   * Wait for the setup process to exit.
   * Returns true if it exited within the timeout, false otherwise.
   */
  private waitForProcessExit(timeoutMs: number): Promise<boolean> {
    return new Promise((resolve) => {
      if (this.browserExited) {
        resolve(true);
        return;
      }

      const start = Date.now();
      const interval = setInterval(() => {
        if (this.browserExited) {
          clearInterval(interval);
          resolve(true);
        } else if (Date.now() - start >= timeoutMs) {
          clearInterval(interval);
          resolve(false);
        }
      }, 200);
    });
  }

  private cleanup(): void {
    this.setupProcess = null;
    this.setupBrowser = null;
    this.browserExited = false;
    this.cdpPort = null;
    this.cancelled = false;
  }

  private getProfileDir(): string {
    const config = ConfigManager.getInstance().get();
    return path.resolve(config.auto_cookies?.browser_profile_dir || "./browser-profile");
  }

  private getCookiePath(): string {
    const config = ConfigManager.getInstance().get();
    return path.resolve(config.downloader.cookie_file || "./cookies.txt");
  }

  /**
   * Stop service on shutdown
   */
  async stop(): Promise<void> {
    if (this.setupProcess) {
      this.cancelled = true;
      await killProcess(this.setupProcess);
      this.cleanup();
    }
  }
}

// ─── Helpers ─────────────────────────────────────────────────────

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function killProcess(proc: ChildProcess | null): Promise<void> {
  if (!proc) return;
  const pid = proc.pid;
  if (pid) {
    await new Promise<void>((resolve) => {
      exec(`taskkill /F /T /PID ${pid}`, () => resolve());
    });
  }
  await sleep(300);
}

async function cleanChromiumLockFiles(profileDir: string): Promise<void> {
  for (const lock of CHROMIUM_LOCK_FILES) {
    await fs.remove(path.join(profileDir, lock)).catch(() => {});
  }
}

function getFreePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.listen(0, "127.0.0.1", () => {
      const port = (server.address() as net.AddressInfo).port;
      server.close(() => resolve(port));
    });
    server.on("error", reject);
  });
}

function waitForExit(
  proc: ChildProcess,
  timeoutMs: number,
): Promise<number | null> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(async () => {
      await killProcess(proc);
      reject(new Error("Process timed out"));
    }, timeoutMs);

    proc.on("close", (code) => {
      clearTimeout(timer);
      resolve(code);
    });

    proc.on("error", (err) => {
      clearTimeout(timer);
      reject(err);
    });
  });
}
