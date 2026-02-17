/**
 * Twitch Authentication
 *
 * Extracts Twitch auth token from Netscape cookie file.
 * The critical cookie is `auth-token` — this is the OAuth bearer token.
 */

import fs from "fs-extra";
import { TWITCH_GQL_CLIENT_ID } from "../../constants.js";

/**
 * Extract Twitch auth-token from a Netscape cookie file.
 * Scans for .twitch.tv domain cookies.
 */
export function extractTwitchAuthToken(cookieContent: string): string | null {
  for (const line of cookieContent.split("\n")) {
    if (!line.trim()) continue;
    if (line.startsWith("#") && !line.startsWith("#HttpOnly_")) continue;

    const parts = line.split("\t");
    if (parts.length < 7) continue;

    const rawDomain = parts[0].trim();
    const domain = rawDomain.startsWith("#HttpOnly_")
      ? rawDomain.slice("#HttpOnly_".length)
      : rawDomain;

    if (!domain.includes("twitch.tv")) continue;

    const name = parts[5].trim();
    if (name === "auth-token") {
      return parts[6].trim() || null;
    }
  }

  return null;
}

/**
 * Read Twitch auth token from a cookie file on disk.
 */
export async function readTwitchAuthToken(cookiePath: string): Promise<string | null> {
  if (!(await fs.pathExists(cookiePath))) return null;
  const content = await fs.readFile(cookiePath, "utf-8");
  return extractTwitchAuthToken(content);
}

/**
 * Generate standard Twitch API headers.
 */
export function generateTwitchHeaders(authToken?: string | null): Record<string, string> {
  const headers: Record<string, string> = {
    "Client-ID": TWITCH_GQL_CLIENT_ID,
    "Content-Type": "application/json",
  };

  if (authToken) {
    headers["Authorization"] = `OAuth ${authToken}`;
  }

  return headers;
}
