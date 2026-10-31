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

// ejs is the solver entry point loadPlayer and solveMany call: ejsMain, save
// under the sidecar's own tests, which stand a stub in for it
// (_setEjsForTest). What the caches do with an answer is this file's logic,
// and only a stub can hand it the exception sentinel on demand.
let ejs = ejsMain;

// Matches solverCacheSize in internal/cipher/solver.go, and for the same
// reason: multi-channel monitoring routinely has 4+ active players, and a
// smaller cap thrashed — every eviction cost the Go side an ErrPlayerNotLoaded
// round-trip, a ~3 MB player-JS re-send and a full ejs preprocess here.
const MAX_PLAYERS = 10;
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
        result = ejs({
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
        result = ejs({
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
    // Every answer is checked before any is cached: a throw part-way through
    // used to leave the answers before it cached, from a response that as a
    // whole was not to be trusted.
    for (const challenge of misses) {
        const solved = response.data[challenge];
        if (solved === undefined) {
            throw new Error(`ejs solve ${type}: no result for challenge`);
        }
        // An answer of either type is a string, or the response is not one:
        // yt-dlp's validate_nsig_challenge_output and
        // validate_sig_challenge_output both require `isinstance(v, str)`
        // ("Invalid NChallengeOutput", jsc/_director.py:265-287), and its
        // bulk_solve records nothing from such a response. A null used to be
        // cached here and replayed, and the Go side read it as "": an empty n
        // falls through to goja, but an empty signature was appended as the
        // answer. Not labelled as a stale player: upstream moves to its next
        // provider and refetches nothing, and so does this.
        if (typeof solved !== "string") {
            throw new Error(`ejs solve ${type}: non-string result for challenge`);
        }
        // The player's exception sentinel: an n function that throws returns
        // from its catch block a string ending with its own input, and ejs
        // hands that back as the answer (its main() returns solver(challenge)
        // as is). yt-dlp's validate_nsig_challenge_output rejects any n result
        // that ends with its challenge — "the js function returned with an
        // exception" (jsc/_director.py:265-278, 6224a3898) — and its
        // bulk_solve records nothing for it. Not cached here either: it used
        // to sit in nCache, every URL of the player built with it and 403ing,
        // until the player was evicted. The challenge is not quoted: an n is
        // one of the media-URL values the Go side redacts.
        //
        // The label is load-bearing, as loadPlayer's is: "ejs solve n: no
        // solutions" is what the Go side's isStaleJSError matches to drop the
        // cached player and retry once with fresh JS and a forced reload here.
        // That retry is Moombox's own: yt-dlp's director only asks its next
        // provider (jsc/_director.py:117-124), and its ejs provider never
        // refetches a player (jsc/_builtin/ejs.py:148-183). A player id names
        // one script, so the refetch mostly brings back the same JS; what it
        // buys is a cached copy gone bad, and for a player whose n function
        // throws whatever the copy, it costs a refetch and a preprocess per
        // solve — the cost ejs's own "no solutions" already pays.
        if (type === "n" && solved.endsWith(challenge)) {
            throw new Error("ejs solve n: no solutions (exception sentinel)");
        }
    }
    for (const challenge of misses) {
        const solved = response.data[challenge];
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

// _setEjsForTest stands fn in for ejsMain (null restores it); _ejsForTest is
// what solves now, which the tests pin to ejsMain by default.
export function _setEjsForTest(fn) {
    ejs = fn ?? ejsMain;
}

export function _ejsForTest() {
    return ejs;
}
