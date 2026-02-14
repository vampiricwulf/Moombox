/**
 * Cookie Format Converter
 *
 * Converts cookies from CDP (Edge/Chrome) and Firefox SQLite formats
 * to Netscape cookie file format for use with YouTube authentication.
 */

import { ESSENTIAL_COOKIES } from "./cookies.js";

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
  isSecure: number; // 0 or 1
}

const RELEVANT_DOMAINS = [".youtube.com", ".google.com", "youtube.com", "google.com"];

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
    if (essentialOnly && !ESSENTIAL_COOKIES.has(cookie.name)) continue;

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
    if (essentialOnly && !ESSENTIAL_COOKIES.has(row.name)) continue;

    // Firefox stores expiry in milliseconds; Netscape format uses seconds
    const expiry = row.expiry > 1e12 ? Math.floor(row.expiry / 1000) : row.expiry;

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
