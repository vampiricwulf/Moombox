import fs from 'fs-extra';
import path from 'path';
import crypto from 'crypto';
import os from 'os';
import { extractPlayerId } from "./utils.js";
import { Logger } from "../../core/logger.js";
import { fetchWithTimeout } from "../../core/http.js";
import { env } from "../../core/env.js";

const ignorePlayerScriptRegion = env.IGNORE_SCRIPT_REGION;

export const CACHE_HOME = process.env.XDG_CACHE_HOME || path.join(os.homedir(), '.cache');
export const CACHE_DIR = path.join(CACHE_HOME, 'yt-cipher', 'player_cache');

export async function getPlayerFilePath(playerUrl: string): Promise<string> {
    let cacheKey: string;
    if (ignorePlayerScriptRegion) {
        cacheKey = extractPlayerId(playerUrl);
    } else {
        const hash = crypto.createHash('sha256').update(playerUrl).digest('hex');
        cacheKey = hash;
    }
    const filePath = path.join(CACHE_DIR, `${cacheKey}.js`);

    try {
        const stat = await fs.stat(filePath);
        // updated time on file mark it as recently used.
        await fs.utimes(filePath, new Date(), stat.mtime ?? new Date());
        return filePath;
    } catch (error: any) {
        if (error.code === 'ENOENT') {
            // console.log(`Cache miss for player: ${playerUrl}. Fetching...`);
            const response = await fetchWithTimeout(playerUrl);
            if (!response.ok) {
                throw new Error(`Failed to fetch player from ${playerUrl}: ${response.statusText}`);
            }
            const playerContent = await response.text();
            await fs.ensureDir(CACHE_DIR);
            await fs.writeFile(filePath, playerContent);
            
            // console.log(`Saved player to cache: ${filePath}`);
            return filePath;
        }
        throw error;
    }
}

export async function initializeCache() {
    await fs.ensureDir(CACHE_DIR);

    const fourteenDays = 14 * 24 * 60 * 60 * 1000;
    // console.log(`Cleaning up player cache directory: ${CACHE_DIR}`);
    try {
        const files = await fs.readdir(CACHE_DIR);
        for (const file of files) {
            const filePath = path.join(CACHE_DIR, file);
            const stat = await fs.stat(filePath);
            if (stat.isFile()) {
                const lastAccessed = stat.atimeMs || stat.mtimeMs || stat.birthtimeMs;
                if (Date.now() - lastAccessed > fourteenDays) {
                    // console.log(`Deleting stale player cache file: ${filePath}`);
                    await fs.remove(filePath);
                }
            }
        }
    } catch (e) {
        Logger.getInstance().warn(`[PlayerCache] Error cleaning cache: ${e}`);
    }
}
