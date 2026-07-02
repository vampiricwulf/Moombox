// Cipher solving for YouTube sig + n params, backed by ejs.
//
// ejs's main() takes a player JS source (or already-preprocessed source)
// and a list of challenges. Preprocessing parses the player and extracts
// the URL-builder constructor; solving constructs that URL builder with
// each challenge as a parameter and reads back the decoded value.
//
// We cache at two levels:
//   1. Per-player solver closures, so we only preprocess each player once.
//   2. Per-challenge results, so segment URLs that share an encrypted n
//      don't repeat the V8 round-trip.

import ejsMain from "../vendor/ejs.bundle.js";

const MAX_PLAYERS = 2;
const MAX_CHALLENGES_PER_PLAYER = 1000;

// playerID -> { preprocessed, sigCache, nCache, lastUsed }
const playerCache = new Map();

function evictIfNeeded() {
    if (playerCache.size <= MAX_PLAYERS) return;
    let oldestID = null;
    let oldestTime = Infinity;
    for (const [id, entry] of playerCache.entries()) {
        if (entry.lastUsed < oldestTime) {
            oldestTime = entry.lastUsed;
            oldestID = id;
        }
    }
    if (oldestID !== null) playerCache.delete(oldestID);
}

function loadPlayer(playerID, playerJS) {
    // Preprocess via ejs with empty requests to get back just the
    // preprocessed source. Solving happens in solveMany via the
    // "preprocessed" call shape so we control the cache.
    //
    // ejs THROWS on preprocess failures (meriyah SyntaxError on truncated
    // JS, "unexpected structure" on a player-layout rotation) rather than
    // returning {type:"error"}. The label below is load-bearing: the Go
    // side's isStaleJSError matches "ejs preprocess" to trigger its
    // remove-cached-player + retry-with-fresh-JS recovery — an unlabeled
    // throw would fail permanently instead of self-healing.
    let result;
    try {
        result = ejsMain({
            type: "player",
            player: playerJS,
            requests: [],
            output_preprocessed: true,
        });
    } catch (e) {
        throw new Error(`ejs preprocess: ${e?.message ?? e}`);
    }
    if (result.type === "error") {
        throw new Error(`ejs preprocess: ${result.error}`);
    }
    if (!result.preprocessed_player) {
        throw new Error("ejs preprocess: no preprocessed_player in result");
    }
    const entry = {
        preprocessed: result.preprocessed_player,
        sigCache: new Map(),
        nCache: new Map(),
        lastUsed: Date.now(),
    };
    playerCache.set(playerID, entry);
    evictIfNeeded();
    return entry;
}

function solveMany(entry, type, challenges) {
    const cache = type === "sig" ? entry.sigCache : entry.nCache;
    const results = {};
    const misses = [];
    const seen = new Set(); // O(1) miss-dedup; misses.includes() was O(n²)
    for (const challenge of challenges) {
        const hit = cache.get(challenge);
        if (hit !== undefined) {
            results[challenge] = hit;
        } else if (!seen.has(challenge)) {
            seen.add(challenge);
            misses.push(challenge);
        }
    }
    if (misses.length === 0) return results;

    // One ejsMain call for ALL uncached challenges of this type: each call
    // re-evaluates the preprocessed solver source, so per-challenge calls
    // would redo that work N times back-to-back on the single-threaded
    // event loop, starving queued PO-token RPCs of the request budget.
    let result;
    try {
        result = ejsMain({
            type: "preprocessed",
            preprocessed_player: entry.preprocessed,
            requests: [{ type, challenges: misses }],
        });
    } catch (e) {
        // Same labeling contract as loadPlayer — see comment there.
        throw new Error(`ejs solve ${type}: ${e?.message ?? e}`);
    }
    if (result.type === "error") {
        throw new Error(`ejs solve ${type}: ${result.error}`);
    }
    const response = result.responses?.[0];
    if (!response) {
        throw new Error(`ejs solve ${type}: no response for challenges`);
    }
    if (response.type === "error") {
        throw new Error(`ejs solve ${type}: ${response.error}`);
    }
    for (const challenge of misses) {
        const solved = response.data[challenge];
        if (solved === undefined) {
            throw new Error(`ejs solve ${type}: no result for challenge`);
        }
        if (cache.size >= MAX_CHALLENGES_PER_PLAYER) {
            // Cheap FIFO: evict the oldest-inserted entry. Acceptable for this workload —
            // challenges are mostly one-shot, so true LRU's bookkeeping cost would not pay back.
            const firstKey = cache.keys().next().value;
            cache.delete(firstKey);
        }
        cache.set(challenge, solved);
        results[challenge] = solved;
    }
    return results;
}

export function solveCipher({ playerID, playerJS, sigChallenges, nChallenges, forceReload }) {
    // forceReload: caller has detected stale-JS errors against a cached
    // entry (Moombox cipher.ErrPlayerJSStale path). Drop our cache and
    // re-load from the attached playerJS so the next solve runs against
    // the rotated player.
    if (forceReload && playerCache.has(playerID)) {
        playerCache.delete(playerID);
    }
    let entry = playerCache.get(playerID);
    if (!entry) {
        if (!playerJS) {
            const err = new Error("player not loaded");
            err.code = "PLAYER_NOT_LOADED";
            throw err;
        }
        entry = loadPlayer(playerID, playerJS);
    }
    entry.lastUsed = Date.now();

    const sigResults = solveMany(entry, "sig", sigChallenges || []);
    const nResults = solveMany(entry, "n", nChallenges || []);
    return { sigResults, nResults };
}

// For tests / diagnostics.
export function _cacheStats() {
    return {
        players: playerCache.size,
        bindings: [...playerCache.entries()].map(([id, e]) => ({
            playerID: id,
            sigCacheSize: e.sigCache.size,
            nCacheSize: e.nCache.size,
            lastUsed: e.lastUsed,
        })),
    };
}

export function _resetCacheForTest() {
    playerCache.clear();
}
