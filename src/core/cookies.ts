import fs from "fs-extra";
import crypto from "crypto";

export interface ParsedCookies {
  cookieHeader: string;
  cookies: Map<string, string>;
}

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

        // Only include YouTube and Google domain cookies
        // Google cookies (SID, HSID, etc.) are needed for YouTube auth
        if (
          !domain.includes("youtube.com") &&
          !domain.includes(".youtube.com") &&
          !domain.includes("google.com") &&
          !domain.includes(".google.com")
        ) {
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

        if (!ESSENTIAL_COOKIES.has(name) && !isGoogleAuthCookie) {
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
   * Reset parsed cookies (for testing)
   */
  static reset(): void {
    this.parsedCookies = null;
  }
}
