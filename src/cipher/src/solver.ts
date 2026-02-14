import fs from 'fs-extra';
import { execInPool } from "./workerPool.js";
import { getPlayerFilePath } from "./playerCache.js";
import { preprocessedCache } from "./preprocessedCache.js";
import { solverCache } from "./solverCache.js";
import { getFromPrepared } from "../../ejs/yt/solver/solvers.js";
import type { Solvers } from "./types.js";
import { extractPlayerId } from "./utils.js";

export async function getSolvers(player_url: string): Promise<Solvers | null> {
    const playerCacheKey = await getPlayerFilePath(player_url);

    let solvers = solverCache.get(playerCacheKey);

    if (solvers) {
        return solvers;
    }

    let preprocessedPlayer = preprocessedCache.get(playerCacheKey);
    if (!preprocessedPlayer) {
        const rawPlayer = await fs.readFile(playerCacheKey, 'utf-8');
        try {
            preprocessedPlayer = await execInPool(rawPlayer);
        } catch (e) {
            const playerId = extractPlayerId(player_url);
            // const message = e instanceof Error ? e.message : String(e);
            // workerErrors.labels({ player_id: playerId, message }).inc();
            throw e;
        }
        preprocessedCache.set(playerCacheKey, preprocessedPlayer);
    }
    
    solvers = getFromPrepared(preprocessedPlayer);
    if (solvers) {
        solverCache.set(playerCacheKey, solvers);
        return solvers;
    }

    return null;
}