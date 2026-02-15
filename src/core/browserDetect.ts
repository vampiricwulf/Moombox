/**
 * Browser Detection (Windows)
 *
 * Finds installed browsers for automatic cookie acquisition.
 * Priority: Firefox → Edge → Chrome
 */

import fs from "fs-extra";
import path from "path";
import { env } from "./env.js";

export interface DetectedBrowser {
  type: "firefox" | "edge" | "chrome";
  path: string;
}

interface BrowserCandidate {
  type: DetectedBrowser["type"];
  relativePath: string;
}

const CANDIDATES: BrowserCandidate[] = [
  { type: "firefox", relativePath: "Mozilla Firefox\\firefox.exe" },
  { type: "edge", relativePath: "Microsoft\\Edge\\Application\\msedge.exe" },
  { type: "chrome", relativePath: "Google\\Chrome\\Application\\chrome.exe" },
];

function getSearchRoots(): string[] {
  const roots: string[] = [];
  const pf = env.PROGRAMFILES;
  const pf86 = process.env["PROGRAMFILES(X86)"];  // Not in env.ts yet, keep as-is
  const localApp = env.LOCALAPPDATA;

  if (pf) roots.push(pf);
  if (pf86) roots.push(pf86);
  if (localApp) roots.push(localApp);

  return roots;
}

/**
 * Detect the first available browser (Firefox → Edge → Chrome)
 */
export function detectBrowser(): DetectedBrowser | null {
  const all = detectAllBrowsers();
  return all.length > 0 ? all[0] : null;
}

/**
 * Detect all available browsers, ordered by preference
 */
export function detectAllBrowsers(): DetectedBrowser[] {
  const roots = getSearchRoots();
  const found: DetectedBrowser[] = [];
  const seenTypes = new Set<string>();

  for (const candidate of CANDIDATES) {
    if (seenTypes.has(candidate.type)) continue;
    for (const root of roots) {
      const fullPath = path.join(root, candidate.relativePath);
      if (fs.existsSync(fullPath)) {
        found.push({ type: candidate.type, path: fullPath });
        seenTypes.add(candidate.type);
        break;
      }
    }
  }

  return found;
}
