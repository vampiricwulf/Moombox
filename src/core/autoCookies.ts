/**
 * Automatic Cookie Acquisition Service
 *
 * Automates the cookie lifecycle for YouTube and Twitch authentication:
 * - Initial setup: launches a browser for user to log in to Google and/or Twitch
 * - Silent refresh: headless browser visits to both sites to refresh cookies on auth failure
 *
 * Supports Firefox (primary, SQLite cookies) and Edge/Chrome (fallback, CDP).
 */

import { spawn } from "child_process";
import type { ChildProcess } from "child_process";
import ms from "ms";
import { execa } from "execa";
import net from "net";
import fs from "fs-extra";
import path from "path";
import { Logger } from "./logger.js";
import { ConfigManager } from "./config.js";
import { CookieJar, verifyYouTubeAuth, verifyTwitchAuth } from "./cookies.js";
import { fetchWithTimeout } from "./http.js";
import { detectBrowser, type DetectedBrowser } from "./browserDetect.js";
import { getErrorMessage } from "../types/errors.js";
import { sleep } from "../utils/async.js";
import { TWITCH_URLS } from "../constants.js";
import { CdpClient } from "./cdpClient.js";
import {
  cdpCookiesToNetscape,
  firefoxCookiesToNetscape,
  type CdpCookie,
  type FirefoxCookieRow,
} from "./cookies.js";

export interface AutoCookieReloginRequired {
  youtube: boolean;
  twitch: boolean;
}

export interface AutoCookieStatus {
  configured: boolean;
  setupInProgress: boolean;
  browser: DetectedBrowser | null;
  lastRefresh: string | null;
  lastError: string | null;
  needsManualRelogin: AutoCookieReloginRequired;
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
const TWITCH_LOGIN_URL = "https://www.twitch.tv/login";
const TWITCH_REFRESH_URL = "https://www.twitch.tv";
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
  private _needsManualRelogin: AutoCookieReloginRequired = { youtube: false, twitch: false };
  private targetPlatform: "youtube" | "twitch" | null = null;

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
      needsManualRelogin: this.needsManualRelogin,
    };
  }

  get needsManualRelogin(): AutoCookieReloginRequired {
    return { ...this._needsManualRelogin };
  }

  /**
   * Flag a platform as needing manual re-login.
   * Called by CookieRefreshService when a configured platform fails validation
   * and auto-refresh either isn't possible or didn't fix it.
   */
  flagManualRelogin(platform: "youtube" | "twitch"): void {
    this._needsManualRelogin[platform] = true;
  }

  getTargetPlatform(): "youtube" | "twitch" | null {
    return this.targetPlatform;
  }

  // ─── Headed Setup ────────────────────────────────────────────────

  /**
   * Step 1: Launch a browser for the user to log in to Google.
   * Returns immediately — the user logs in at their own pace.
   */
  async startSetup(platform?: "youtube" | "twitch"): Promise<{ success: boolean; error?: string }> {
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
    this.targetPlatform = platform ?? null;

    // Select login URL based on platform
    const loginUrl = platform === "twitch" ? TWITCH_LOGIN_URL : LOGIN_URL;

    try {
      if (browser.type === "firefox") {
        await this.startFirefoxSetup(browser, profileDir, loginUrl);
      } else {
        await this.startChromiumSetup(browser, profileDir, loginUrl);
      }
      return { success: true };
    } catch (e) {
      const msg = getErrorMessage(e);
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
    twitchAuthenticated: boolean;
    error?: string;
  }> {
    if (!this.setupProcess || !this.setupBrowser) {
      return { success: false, authenticated: false, twitchAuthenticated: false, error: "No setup in progress" };
    }

    if (this.cancelled) {
      return { success: false, authenticated: false, twitchAuthenticated: false, error: "Setup was cancelled" };
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

      // Validate by making real API calls (not just cookie presence checks)
      CookieJar.reset();
      await CookieJar.parse(cookiePath, true);

      // Run both verifications in parallel for speed
      const [authenticated, twitchAuthenticated] = await Promise.all([
        CookieJar.hasAuthCookies() ? verifyYouTubeAuth() : Promise.resolve(false),
        CookieJar.hasTwitchAuthCookies() ? verifyTwitchAuth() : Promise.resolve(false),
      ]);

      this.lastRefresh = new Date();
      this.cleanup();

      if (!authenticated && !twitchAuthenticated) {
        // Cookies are present but auth failed — still report as setup complete
        // so the user knows cookies were extracted, but flag that auth didn't pass
        const hasCookies = CookieJar.hasAuthCookies() || CookieJar.hasTwitchAuthCookies();
        return {
          success: true,
          authenticated: false,
          twitchAuthenticated: false,
          error: hasCookies
            ? "Cookies were extracted but authentication verification failed. The login session may not have completed."
            : "No login detected for YouTube or Twitch. Make sure you signed in to at least one service.",
        };
      }

      // Clear re-login flags for verified platforms
      if (authenticated) this._needsManualRelogin.youtube = false;
      if (twitchAuthenticated) this._needsManualRelogin.twitch = false;

      // Persist verified platforms to config so we can detect auth loss after restart
      await this.persistPlatforms(authenticated, twitchAuthenticated);

      const platforms = [
        authenticated ? "YouTube" : null,
        twitchAuthenticated ? "Twitch" : null,
      ].filter(Boolean).join(" + ");
      this.logger.info(`[AutoCookies] Setup complete — verified: ${platforms}`);
      return { success: true, authenticated, twitchAuthenticated };
    } catch (e) {
      const msg = getErrorMessage(e);
      this.lastError = msg;
      this.cleanup();
      return { success: false, authenticated: false, twitchAuthenticated: false, error: msg };
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

  // ─── Twitch Navigation ──────────────────────────────────────────

  /**
   * Navigate the setup browser to Twitch login page.
   * For Chromium: uses CDP to navigate the active tab.
   * For Firefox: no-op (user must navigate manually).
   */
  async navigateToTwitch(): Promise<{ success: boolean; manual?: boolean; error?: string }> {
    if (!this.setupProcess || !this.setupBrowser) {
      return { success: false, error: "No setup in progress" };
    }

    if (this.setupBrowser.type === "firefox") {
      // Firefox doesn't support CDP navigation — user navigates manually
      return { success: true, manual: true };
    }

    // Chromium: navigate via CDP
    if (!this.cdpPort) {
      return { success: false, error: "CDP port not available" };
    }

    let pageClient: CdpClient | null = null;
    try {
      pageClient = await CdpClient.connectToPage(this.cdpPort);
      await pageClient.navigate(TWITCH_LOGIN_URL);
      pageClient.disconnect();
      return { success: true };
    } catch (e) {
      pageClient?.disconnect();
      return { success: false, error: getErrorMessage(e) };
    }
  }

  // ─── Platform Persistence ───────────────────────────────────────

  /**
   * Persist verified platforms to config so we can detect auth loss after restart.
   */
  private async persistPlatforms(youtubeVerified: boolean, twitchVerified: boolean): Promise<void> {
    try {
      const configManager = ConfigManager.getInstance();
      const config = configManager.get();
      const existing = new Set(config.auto_cookies?.platforms ?? []);

      if (youtubeVerified) existing.add("youtube");
      if (twitchVerified) existing.add("twitch");

      const platforms = Array.from(existing) as ("youtube" | "twitch")[];
      if (!config.auto_cookies) {
        (config as any).auto_cookies = {};
      }
      config.auto_cookies!.platforms = platforms;

      await configManager.save(config);
      this.logger.debug(`[AutoCookies] Persisted configured platforms: ${platforms.join(", ")}`);
    } catch (e) {
      this.logger.warn(`[AutoCookies] Failed to persist platforms: ${getErrorMessage(e)}`);
    }
  }

  // ─── Headless Refresh ────────────────────────────────────────────

  /**
   * Silently refresh cookies using a headless browser visit.
   * Called automatically when a job hits COOKIES? status.
   */
  async refreshCookies(): Promise<boolean> {
    // If both platforms need manual re-login, skip the headless refresh entirely
    if (this._needsManualRelogin.youtube && this._needsManualRelogin.twitch) {
      this.logger.debug("[AutoCookies] Skipping headless refresh — manual re-login required for all platforms");
      return false;
    }

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

    const cookiePath = this.getCookiePath();

    // Determine which platforms we expect — configured platforms, or infer from existing cookies
    const config = ConfigManager.getInstance().get();
    const configuredPlatforms = new Set(config.auto_cookies?.platforms ?? []);
    try {
      if (await fs.pathExists(cookiePath)) {
        await CookieJar.load(cookiePath, true);
        if (CookieJar.hasTwitchAuthCookies()) configuredPlatforms.add("twitch");
        if (CookieJar.hasAuthCookies()) configuredPlatforms.add("youtube");
      }
    } catch {
      // Ignore errors checking pre-refresh state
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
      await fs.ensureDir(path.dirname(cookiePath));
      await fs.writeFile(cookiePath, netscapeCookies, "utf-8");
      // Set restrictive permissions (owner read/write only)
      await fs.chmod(cookiePath, 0o600);

      // Validate by making real API calls (not just cookie presence checks)
      CookieJar.reset();
      await CookieJar.parse(cookiePath, true);

      // Run both verifications in parallel — verify any platform that has cookies
      const [authenticated, twitchAuthenticated] = await Promise.all([
        CookieJar.hasAuthCookies() ? verifyYouTubeAuth() : Promise.resolve(false),
        CookieJar.hasTwitchAuthCookies() ? verifyTwitchAuth() : Promise.resolve(false),
      ]);

      // Set re-login flags for configured platforms that lost authentication
      if (!authenticated && configuredPlatforms.has("youtube")) {
        this._needsManualRelogin.youtube = true;
        this.logger.warn("[AutoCookies] YouTube auth verification failed — manual re-login required");
      }
      if (!twitchAuthenticated && configuredPlatforms.has("twitch")) {
        this._needsManualRelogin.twitch = true;
        this.logger.warn("[AutoCookies] Twitch auth verification failed — manual re-login required");
      }

      // Consider refresh successful if any platform verified
      if (authenticated || twitchAuthenticated) {
        this.lastRefresh = new Date();
        this.lastError = null;
        const verified = [
          authenticated ? "YouTube" : null,
          twitchAuthenticated ? "Twitch" : null,
        ].filter(Boolean).join(" + ");
        this.logger.info(`[AutoCookies] Cookie refresh succeeded (verified: ${verified})`);
        return true;
      }

      this.lastError = "Refresh completed but auth verification failed — manual re-login required";
      this.logger.warn("[AutoCookies] Refresh completed but auth verification failed");
      return false;
    } catch (e) {
      const msg = getErrorMessage(e);
      this.lastError = msg;
      this.logger.warn(`[AutoCookies] Refresh failed: ${msg}`);
      return false;
    }
  }

  // ─── Firefox Flows ───────────────────────────────────────────────

  private async startFirefoxSetup(
    browser: DetectedBrowser,
    profileDir: string,
    url: string = LOGIN_URL,
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
      ["-profile", profileDir, "-no-remote", url],
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
    // Use --screenshot to make headless Firefox load a page and exit.
    // Visit both YouTube and Twitch to refresh session cookies for each.
    const tempScreenshot = path.join(profileDir, "refresh-screenshot.png");

    // Visit YouTube
    const proc1 = spawn(
      browser.path,
      ["--screenshot", tempScreenshot, "-no-remote", "-profile", profileDir, REFRESH_URL],
      { stdio: "ignore", detached: false },
    );
    await waitForExit(proc1, PROCESS_TIMEOUT);

    // Visit Twitch
    const proc2 = spawn(
      browser.path,
      ["--screenshot", tempScreenshot, "-no-remote", "-profile", profileDir, TWITCH_REFRESH_URL],
      { stdio: "ignore", detached: false },
    );
    await waitForExit(proc2, PROCESS_TIMEOUT);

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
        if (attempt < 2) await sleep(ms("1s"));
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
    const assets = globalThis.__MOOMBOX_ASSETS__;
    if (assets?.["sql-wasm.wasm"]) {
      return assets["sql-wasm.wasm"] as Buffer;
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
    url: string = LOGIN_URL,
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
        url,
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

      // Navigate to YouTube and Twitch to refresh cookies for both
      let pageClient: CdpClient | null = null;
      try {
        pageClient = await CdpClient.connectToPage(port);
        await pageClient.navigate(REFRESH_URL);
        await pageClient.navigate(TWITCH_REFRESH_URL);
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
      await sleep(ms("500ms"));
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
      await sleep(ms("300ms")); // Brief FS flush delay
      return;
    }

    const pid = this.setupProcess.pid;
    if (!pid) return;

    // Step 1: Send graceful close (WM_CLOSE) — no /F flag
    this.logger.debug("[AutoCookies] Sending graceful close to Firefox...");
    try {
      if (process.platform === 'win32') {
        await execa('taskkill', ['/T', '/PID', String(pid)]);
      } else {
        await execa('kill', ['-TERM', String(pid)]);
      }
    } catch (error) {
      // Ignore errors during graceful kill (process may already be gone)
    }

    // Step 2: Wait for clean exit (up to 8 seconds)
    const exitedCleanly = await this.waitForProcessExit(8000);

    if (exitedCleanly) {
      this.logger.debug("[AutoCookies] Firefox exited cleanly (WAL checkpointed)");
      await sleep(ms("300ms")); // Brief FS flush delay
      return;
    }

    // Step 3: Graceful close didn't work — force kill as last resort
    this.logger.warn(
      "[AutoCookies] Firefox did not exit gracefully within 8s, force killing. " +
      "Some cookies in the WAL may not be readable.",
    );
    try {
      if (process.platform === 'win32') {
        await execa('taskkill', ['/F', '/T', '/PID', String(pid)]);
      } else {
        await execa('kill', ['-KILL', String(pid)]);
      }
    } catch (error: unknown) {
      this.logger.error(`[AutoCookies] Failed to force kill process: ${getErrorMessage(error)}`);
    }
    await sleep(ms("500ms"));
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
    this.targetPlatform = null;
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

async function killProcess(proc: ChildProcess | null): Promise<void> {
  if (!proc) return;
  const pid = proc.pid;
  if (pid) {
    try {
      if (process.platform === 'win32') {
        await execa('taskkill', ['/F', '/T', '/PID', String(pid)]);
      } else {
        await execa('kill', ['-KILL', String(pid)]);
      }
    } catch (error) {
      // Ignore errors (process may already be gone)
    }
  }
  await sleep(ms("300ms"));
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
