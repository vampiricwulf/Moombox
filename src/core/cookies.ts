import fs from "fs-extra";
import crypto from "crypto";
import { fetchWithTimeout } from "./http.js";
import { USER_AGENTS, YOUTUBE_URLS, TWITCH_URLS, WEB_CLIENT } from "../constants.js";

export interface ParsedCookies {
  cookieHeader: string;
  cookies: Map<string, string>;
}

/** CDP cookie as returned by Network.getAllCookies */
export interface CdpCookie {
  name: string;
  value: string;
  domain: string;
  path: string;
  expires: number; // -1 for session cookies
  size: number;
  httpOnly: boolean;
  secure: boolean;
  session: boolean;
  sameSite?: string;
}

/** Firefox cookies.sqlite row */
export interface FirefoxCookieRow {
  name: string;
  value: string;
  host: string;
  path: string;
  expiry: number; // 0 for session cookies
  isHttpOnly: number; // 0 or 1
  isSecure: number; // 0 or 1;
}

// Essential Twitch cookies
export const TWITCH_ESSENTIAL_COOKIES = new Set([
  "auth-token",           // OAuth bearer token (the main one)
  "twilight-user",        // User preference data
  "login",               // Username
  "name",                // Display name
]);

// Essential YouTube cookies needed for authentication
// Based on yt-dlp's cookie handling
export const ESSENTIAL_COOKIES = new Set([
  // Auth cookies for SAPISIDHASH generation
  "SAPISID",
  "__Secure-1PAPISID",
  "__Secure-3PAPISID",
  // Session cookies
  "SID",
  "HSID",
  "SSID",
  "APISID",
  "__Secure-1PSID",
  "__Secure-3PSID",
  // Session ID tokens (important for auth)
  "__Secure-1PSIDTS",
  "__Secure-3PSIDTS",
  "__Secure-1PSIDCC",
  "__Secure-3PSIDCC",
  // Login verification
  "LOGIN_INFO",
  // Visitor data
  "VISITOR_INFO1_LIVE",
  "VISITOR_PRIVACY_METADATA",
  // Session state
  "YSC",
  // Rollout token (may be needed)
  "__Secure-ROLLOUT_TOKEN",
  // Consent
  "CONSENT",
  // Preferences
  "PREF",
]);

export class CookieJar {
  private static parsedCookies: ParsedCookies | null = null;

  static async load(
    filePath: string,
    forceReload: boolean = false,
  ): Promise<string> {
    const parsed = await this.parse(filePath, forceReload);
    return parsed.cookieHeader;
  }

  static async parse(
    filePath: string,
    forceReload: boolean = false,
  ): Promise<ParsedCookies> {
    if (this.parsedCookies && !forceReload) return this.parsedCookies;

    if (!(await fs.pathExists(filePath))) {
      this.parsedCookies = { cookieHeader: "", cookies: new Map() };
      return this.parsedCookies;
    }

    const content = await fs.readFile(filePath, "utf-8");
    const lines = content.split("\n");
    const cookies = new Map<string, string>();
    // Track which domain each cookie came from so youtube.com takes priority
    const cookieDomains = new Map<string, string>();

    for (const line of lines) {
      // Skip empty lines and comments, but NOT #HttpOnly_ lines (those are data)
      if (!line.trim()) continue;
      if (line.startsWith("#") && !line.startsWith("#HttpOnly_")) continue;

      const parts = line.split("\t");
      if (parts.length >= 7) {
        // domain, flag, path, secure, expiration, name, value
        // Strip #HttpOnly_ prefix to get the actual domain
        const rawDomain = parts[0].trim();
        const domain = rawDomain.startsWith("#HttpOnly_")
          ? rawDomain.slice("#HttpOnly_".length)
          : rawDomain;
        const name = parts[5].trim();
        const value = parts[6].trim();

        // Only include YouTube, Google, and Twitch domain cookies
        const isYouTubeGoogle =
          domain.includes("youtube.com") ||
          domain.includes(".youtube.com") ||
          domain.includes("google.com") ||
          domain.includes(".google.com");
        const isTwitch = domain.includes("twitch.tv") || domain.includes(".twitch.tv");

        if (!isYouTubeGoogle && !isTwitch) {
          continue;
        }

        // Filter to only essential cookies to reduce payload size
        // But include all Google domain auth cookies
        const isGoogleAuthCookie =
          domain.includes("google.com") &&
          (name === "SID" ||
            name === "HSID" ||
            name === "SSID" ||
            name === "APISID" ||
            name === "SAPISID" ||
            name.startsWith("__Secure-1P") ||
            name.startsWith("__Secure-3P"));

        const isTwitchEssential = isTwitch && TWITCH_ESSENTIAL_COOKIES.has(name);

        if (!ESSENTIAL_COOKIES.has(name) && !isGoogleAuthCookie && !isTwitchEssential) {
          continue;
        }

        // Prefer youtube.com cookies over google.com when both exist
        // (they can have different values and duplicates confuse YouTube)
        const existingDomain = cookieDomains.get(name);
        if (existingDomain && existingDomain.includes("youtube.com") && !domain.includes("youtube.com")) {
          continue; // Already have youtube.com version, skip google.com version
        }

        cookies.set(name, value);
        cookieDomains.set(name, domain);
      }
    }

    // Build cookie header from deduplicated Map (no duplicate names)
    const cookiePairs: string[] = [];
    for (const [name, value] of cookies) {
      cookiePairs.push(`${name}=${value}`);
    }

    this.parsedCookies = {
      cookieHeader: cookiePairs.join("; "),
      cookies,
    };
    return this.parsedCookies;
  }

  /**
   * Get the current cookie header string without reloading from file.
   */
  static getCookieHeader(): string {
    return this.parsedCookies?.cookieHeader ?? "";
  }

  /**
   * Get all SAPISID cookie variants for YouTube authentication.
   * Returns { sapisid, sapisid1p, sapisid3p }
   */
  static getSapisidCookies(): {
    sapisid: string | null;
    sapisid1p: string | null;
    sapisid3p: string | null;
  } {
    if (!this.parsedCookies)
      return { sapisid: null, sapisid1p: null, sapisid3p: null };

    const sapisid = this.parsedCookies.cookies.get("SAPISID") || null;
    const sapisid3p =
      this.parsedCookies.cookies.get("__Secure-3PAPISID") || null;
    const sapisid1p =
      this.parsedCookies.cookies.get("__Secure-1PAPISID") || null;

    return {
      // yt-dlp falls back to 3PAPISID if SAPISID is missing
      sapisid: sapisid || sapisid3p,
      sapisid1p,
      sapisid3p,
    };
  }

  /**
   * Get SAPISID cookie value for YouTube authentication.
   * Falls back to __Secure-3PAPISID if SAPISID is not present.
   */
  static getSapisid(): string | null {
    return this.getSapisidCookies().sapisid;
  }

  /**
   * Generate a single SAPISIDHASH value
   */
  private static makeSidAuthorization(
    scheme: string,
    sid: string,
    origin: string,
  ): string {
    const timestamp = Math.round(Date.now() / 1000).toString();
    const hashInput = `${timestamp} ${sid} ${origin}`;
    const hash = crypto.createHash("sha1").update(hashInput).digest("hex");
    return `${scheme} ${timestamp}_${hash}`;
  }

  /**
   * Generate full Authorization header for YouTube API authentication.
   * Matches yt-dlp's approach: includes SAPISIDHASH, SAPISID1PHASH, SAPISID3PHASH
   */
  static generateAuthorizationHeader(
    origin: string = "https://www.youtube.com",
  ): string | null {
    const { sapisid, sapisid1p, sapisid3p } = this.getSapisidCookies();

    const authorizations: string[] = [];

    if (sapisid) {
      authorizations.push(
        this.makeSidAuthorization("SAPISIDHASH", sapisid, origin),
      );
    }
    if (sapisid1p) {
      authorizations.push(
        this.makeSidAuthorization("SAPISID1PHASH", sapisid1p, origin),
      );
    }
    if (sapisid3p) {
      authorizations.push(
        this.makeSidAuthorization("SAPISID3PHASH", sapisid3p, origin),
      );
    }

    if (authorizations.length === 0) {
      return null;
    }

    return authorizations.join(" ");
  }

  /**
   * Generate SAPISIDHASH for YouTube API authentication (legacy single hash).
   * Format: SAPISIDHASH timestamp_sha1(timestamp SAPISID origin)
   */
  static generateSapisidHash(
    origin: string = "https://www.youtube.com",
  ): string | null {
    const sapisid = this.getSapisid();
    if (!sapisid) return null;

    const timestamp = Math.floor(Date.now() / 1000);
    const hashInput = `${timestamp} ${sapisid} ${origin}`;
    const hash = crypto.createHash("sha1").update(hashInput).digest("hex");

    return `SAPISIDHASH ${timestamp}_${hash}`;
  }

  /**
   * Check if we have authentication cookies for YouTube.
   */
  static hasAuthCookies(): boolean {
    if (!this.parsedCookies) return false;
    const hasSapisid = this.getSapisid() !== null;
    const hasLoginInfo = this.parsedCookies.cookies.has("LOGIN_INFO");
    return hasSapisid && hasLoginInfo;
  }

  /**
   * Get Twitch auth-token from parsed cookies.
   * The auth-token is the OAuth bearer token for Twitch API access.
   */
  static getTwitchAuthToken(): string | null {
    if (!this.parsedCookies) return null;
    return this.parsedCookies.cookies.get("auth-token") || null;
  }

  /**
   * Check if we have Twitch authentication cookies.
   */
  static hasTwitchAuthCookies(): boolean {
    return !!this.getTwitchAuthToken();
  }

  /**
   * Reset parsed cookies (for testing)
   */
  static reset(): void {
    this.parsedCookies = null;
  }
}

// ============================================================================
// Auth Verification (real API calls, not format-only checks)
// ============================================================================

/**
 * Verify YouTube authentication by making a real API call.
 * Uses the guide endpoint which is lightweight and requires valid auth cookies + SAPISIDHASH.
 * CookieJar must already be loaded before calling this.
 */
export async function verifyYouTubeAuth(): Promise<boolean> {
  try {
    if (!CookieJar.hasAuthCookies()) return false;

    const cookieHeader = CookieJar.getCookieHeader();
    if (!cookieHeader) return false;

    const authHeader = CookieJar.generateAuthorizationHeader();
    if (!authHeader) return false;

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
          Authorization: authHeader,
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
      10_000, // 10s timeout — quick yes/no
    );

    if (!response.ok) {
      await response.body?.cancel();
      return false;
    }

    // YouTube always returns 200 OK even with invalid/garbage cookies —
    // it just serves an unauthenticated guide response. Parse the body
    // and check for authentication indicators.
    const data = await response.json();

    // Primary: serviceTrackingParams contains logged_in across all Innertube responses
    const trackingParams = data?.responseContext?.serviceTrackingParams;
    if (Array.isArray(trackingParams)) {
      for (const service of trackingParams) {
        if (Array.isArray(service?.params)) {
          for (const param of service.params) {
            if (param?.key === "logged_in" && param?.value === "1") {
              return true;
            }
          }
        }
      }
    }

    // Fallback: mainAppWebResponseContext.loggedIn (may not always be present)
    if (data?.responseContext?.mainAppWebResponseContext?.loggedIn === true) {
      return true;
    }

    return false;
  } catch {
    return false;
  }
}

/**
 * Verify Twitch authentication by validating the OAuth token.
 * Uses Twitch's OAuth2 validation endpoint — returns 200 if valid, 401 if expired.
 * CookieJar must already be loaded before calling this.
 */
export async function verifyTwitchAuth(): Promise<boolean> {
  try {
    const token = CookieJar.getTwitchAuthToken();
    if (!token) return false;

    const response = await fetchWithTimeout(
      TWITCH_URLS.OAUTH_VALIDATE,
      {
        headers: {
          Authorization: `OAuth ${token}`,
        },
      },
      10_000,
    );

    const ok = response.ok;
    // Drain body — we only need the HTTP status
    await response.body?.cancel();
    return ok;
  } catch {
    return false;
  }
}

// ============================================================================
// Cookie Format Conversion Utilities
// ============================================================================

const RELEVANT_DOMAINS = [".youtube.com", ".google.com", "youtube.com", "google.com", ".twitch.tv", "twitch.tv"];

function isRelevantDomain(domain: string): boolean {
  return RELEVANT_DOMAINS.some(
    (d) => domain === d || domain.endsWith(d),
  );
}

function toNetscapeLine(
  domain: string,
  httpOnly: boolean,
  path: string,
  secure: boolean,
  expiry: number,
  name: string,
  value: string,
): string {
  const domainField = httpOnly ? `#HttpOnly_${domain}` : domain;
  const subdomains = domain.startsWith(".") ? "TRUE" : "FALSE";
  const secureFlag = secure ? "TRUE" : "FALSE";
  return `${domainField}\t${subdomains}\t${path}\t${secureFlag}\t${expiry}\t${name}\t${value}`;
}

function isYouTubeDomain(domain: string): boolean {
  return domain.includes("youtube.com");
}

interface DeduplicatedCookie {
  domain: string;
  httpOnly: boolean;
  path: string;
  secure: boolean;
  expiry: number;
  name: string;
  value: string;
}

/**
 * Deduplicate cookies by name, preferring youtube.com over google.com.
 * Duplicate cookie names with different domains cause auth failures when
 * the Cookie header contains conflicting values for the same name.
 */
function deduplicateCookies(cookies: DeduplicatedCookie[]): DeduplicatedCookie[] {
  const byName = new Map<string, DeduplicatedCookie>();
  const domainByName = new Map<string, string>();

  for (const cookie of cookies) {
    const existing = domainByName.get(cookie.name);
    if (existing && isYouTubeDomain(existing) && !isYouTubeDomain(cookie.domain)) {
      continue; // Already have youtube.com version, skip google.com
    }
    byName.set(cookie.name, cookie);
    domainByName.set(cookie.name, cookie.domain);
  }

  return Array.from(byName.values());
}

/**
 * Convert CDP cookies to Netscape format
 */
export function cdpCookiesToNetscape(
  cookies: CdpCookie[],
  essentialOnly: boolean = true,
): string {
  const collected: DeduplicatedCookie[] = [];

  for (const cookie of cookies) {
    if (!isRelevantDomain(cookie.domain)) continue;
    if (essentialOnly) {
      const isTwitchDomain = cookie.domain.includes("twitch.tv");
      if (isTwitchDomain ? !TWITCH_ESSENTIAL_COOKIES.has(cookie.name) : !ESSENTIAL_COOKIES.has(cookie.name)) continue;
    }

    collected.push({
      domain: cookie.domain,
      httpOnly: cookie.httpOnly,
      path: cookie.path,
      secure: cookie.secure,
      expiry: cookie.expires === -1 ? 0 : Math.floor(cookie.expires),
      name: cookie.name,
      value: cookie.value,
    });
  }

  const lines = ["# Netscape HTTP Cookie File"];
  for (const c of deduplicateCookies(collected)) {
    lines.push(toNetscapeLine(c.domain, c.httpOnly, c.path, c.secure, c.expiry, c.name, c.value));
  }
  return lines.join("\n") + "\n";
}

/**
 * Convert Firefox SQLite cookie rows to Netscape format
 */
export function firefoxCookiesToNetscape(
  rows: FirefoxCookieRow[],
  essentialOnly: boolean = true,
): string {
  const collected: DeduplicatedCookie[] = [];

  for (const row of rows) {
    if (!isRelevantDomain(row.host)) continue;
    if (essentialOnly) {
      const isTwitchDomain = row.host.includes("twitch.tv");
      if (isTwitchDomain ? !TWITCH_ESSENTIAL_COOKIES.has(row.name) : !ESSENTIAL_COOKIES.has(row.name)) continue;
    }

    // Firefox moz_cookies.expiry is already in seconds (unix epoch)
    const expiry = row.expiry;

    collected.push({
      domain: row.host,
      httpOnly: row.isHttpOnly === 1,
      path: row.path,
      secure: row.isSecure === 1,
      expiry,
      name: row.name,
      value: row.value,
    });
  }

  const lines = ["# Netscape HTTP Cookie File"];
  for (const c of deduplicateCookies(collected)) {
    lines.push(toNetscapeLine(c.domain, c.httpOnly, c.path, c.secure, c.expiry, c.name, c.value));
  }
  return lines.join("\n") + "\n";
}
