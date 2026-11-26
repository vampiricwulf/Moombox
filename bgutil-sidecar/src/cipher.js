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
import { rawHash, readPrepared, removePrepared, sweepStore, writePrepared } from "./player-store.js";

// ejs is the solver entry point loadPlayer and solveMany call: ejsMain, save
// under the sidecar's own tests, which stand a stub in for it
// (_setEjsForTest). What the caches do with an answer is this file's logic,
// and only a stub can hand it the exception sentinel on demand.
let ejs = ejsMain;

// The globals ejs's setupNodes writes (vendor/ejs/src/yt/solver/setup.ts),
// which every preprocessed player runs first (preprocessPlayer prepends it,
// solvers.ts): location unconditionally, the rest only when absent. ejs is
// built to run in a throwaway process of its own; yt-dlp starts a fresh,
// --permission-sandboxed Node or Deno for every bulk solve
// (yt_dlp/extractor/youtube/jsc/_builtin/node.py:26-45, deno.py:38-39), and
// bgutil's BotGuard server never loads ejs at all. Here it shares a realm with
// BotGuard, whose snapshot reads its environment off globalThis (server.js
// passes globalObject: globalThis): after the first solve globalThis.location
// was a URL for watch?v=yt-dlp-wins instead of JSDOM's Location, for every
// minter built afterwards (audit G23). Whether BotGuard's attestation reads it
// is unproven; that it should see the page server.js set up is not.
//
// Owner decision D21: save and restore these around every ejs call now; run
// ejs in a node:vm context later.
const EJS_GLOBALS = ["location", "XMLHttpRequest", "self", "document", "navigator", "window"];

// withEjsGlobalsRestored runs fn and then puts each of EJS_GLOBALS back as it
// was, by property descriptor, so an accessor stays an accessor and a
// non-writable property keeps its flags; one that was absent is deleted. The
// player's sig and n functions run inside the ejs call (main evaluates the
// preprocessed source and calls them before it returns), so nothing ejs
// returns still needs its own globals afterwards. A restore that throws is
// logged and the rest still restored: one stuck property must not leave the
// others polluted, nor replace fn's own error.
//
// globalThis.window is taken away for the length of the call. Since ejs
// 1c493d6 (#77) setupNodes ends with `globalThis.window.location =
// globalThis.location`; with the JSDOM window still there that assignment is
// a navigation of BotGuard's page to watch?v=yt-dlp-wins, which JSDOM refuses
// with a "Not implemented: navigation" error on stderr at every solve. With
// no window, setupNodes makes `window` the global object itself, the shape
// ejs has in the process of its own upstream gives it, and the assignment
// lands on a global this function restores.
function withEjsGlobalsRestored(fn) {
    const saved = EJS_GLOBALS.map((name) => [name, Object.getOwnPropertyDescriptor(globalThis, name)]);
    try {
        delete globalThis.window;
    } catch (e) {
        process.stderr.write(`[bgutil-sidecar:warn] ejs: could not hide globalThis.window: ${e?.message ?? e}\n`);
    }
    try {
        return fn();
    } finally {
        for (const [name, desc] of saved) {
            try {
                if (desc) {
                    Object.defineProperty(globalThis, name, desc);
                } else {
                    delete globalThis[name];
                }
            } catch (e) {
                process.stderr.write(`[bgutil-sidecar:warn] ejs: could not restore globalThis.${name}: ${e?.message ?? e}\n`);
            }
        }
    }
}

// runEjs is the one way this file calls ejs.
function runEjs(input) {
    return withEjsGlobalsRestored(() => ejs(input));
}

// Matches solverCacheSize in internal/cipher/solver.go, and for the same
// reason: multi-channel monitoring routinely has 4+ active players, and a
// smaller cap thrashed — every eviction cost the Go side an ErrPlayerNotLoaded
// round-trip, a ~3 MB player-JS re-send and a full ejs preprocess here.
const MAX_PLAYERS = 10;
const MAX_CHALLENGES_PER_PLAYER = 1000;

// playerID -> { preprocessed, sigCache, nCache, lastUsed, fromDisk, proven }
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
        result = runEjs({
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
    return addEntry(playerID, flatCopy(result.preprocessed_player), false);
}

// flatCopy is the form a prepared player is held in. ejs hands back a rope of
// the player's pieces, and each solve flattened a fresh copy of it on V8's
// heap; the store hands back a slice of the file's text, on the heap and
// holding the whole of it. The round trip through a Buffer is === the
// original for every JS string, always flat, and held off V8's heap.
// (Flattening in place was measured to fail once the function is optimized.)
function flatCopy(s) {
    return Buffer.from(s, "utf16le").toString("utf16le");
}

// addEntry caches a prepared player. fromDisk marks one read back from the
// prepared-player store; proven is set once an ejs call on it has succeeded.
function addEntry(playerID, preprocessed, fromDisk) {
    const entry = {
        preprocessed,
        sigCache: new Map(),
        nCache: new Map(),
        lastUsed: Date.now(),
        fromDisk,
        proven: false,
    };
    playerCache.set(playerID, entry);
    evictIfNeeded();
    return entry;
}

// loadAndStore preprocesses playerJS and writes the prepared copy to the store.
function loadAndStore(playerID, playerJS) {
    const entry = loadPlayer(playerID, playerJS);
    writePrepared(playerID, rawHash(playerJS), entry.preprocessed);
    return entry;
}

function playerNotLoaded() {
    const err = new Error("player not loaded");
    err.code = "PLAYER_NOT_LOADED";
    return err;
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
        result = runEjs({
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
    entry.proven = true;
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
    // the rotated player. The prepared copy on disk goes too.
    if (forceReload) {
        playerCache.delete(playerID);
        removePrepared(playerID);
    }
    // Load order: RAM; the prepared copy on disk (used even with playerJS
    // attached, since a player ID names one script, unless that JS hashes
    // differently from what the copy was prepared from); a preprocess of the
    // attached JS; else PLAYER_NOT_LOADED, and the Go side re-sends the JS.
    let entry = playerCache.get(playerID);
    if (!entry) {
        const rec = readPrepared(playerID);
        if (rec && playerJS && rawHash(playerJS) !== rec.rawHash) {
            removePrepared(playerID);
        } else if (rec) {
            entry = addEntry(playerID, flatCopy(rec.source), true);
        }
    }
    if (!entry) {
        if (!playerJS) throw playerNotLoaded();
        entry = loadAndStore(playerID, playerJS);
    }
    entry.lastUsed = Date.now();

    const solveBoth = (e) => ({
        sigResults: solveMany(e, "sig", sigChallenges || []),
        nResults: solveMany(e, "n", nChallenges || []),
    });
    try {
        return solveBoth(entry);
    } catch (e) {
        // Self-heal: a copy read from disk that fails its first ejs call in
        // any way (a throw, or an error ejs returns) is discarded, so a bad
        // file never becomes a permanent solve failure. A genuine failure
        // then costs one extra preprocess before the Go side's stale-JS path
        // runs. Once a copy has answered, its failures are the player's.
        if (!entry.fromDisk || entry.proven) throw e;
        if (playerCache.get(playerID) === entry) playerCache.delete(playerID);
        removePrepared(playerID);
        process.stderr.write(`[bgutil-sidecar:warn] cipher: discarded a bad prepared copy of ${playerID}\n`);
        if (!playerJS) throw playerNotLoaded();
        entry = loadAndStore(playerID, playerJS);
        return solveBoth(entry);
    }
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

// An entry unused this long leaves RAM; its prepared copy stays on disk, so
// its next solve is a file read, not a re-send and a preprocess.
export const IDLE_EVICT_MS = 15 * 60 * 1000;
const SWEEP_INTERVAL_MS = 60 * 1000;
const STORE_SWEEP_EVERY_TICKS = 60;

// sweepIdle drops every entry idle longer than IDLE_EVICT_MS and returns how
// many it dropped. Solving is synchronous, so a sweep never runs mid-solve,
// and solveCipher refreshes the requested entry's lastUsed before solving.
// After a drop it runs one last-resort major GC, when gc is exposed, so the
// compiled code of the dropped players goes too.
export function sweepIdle(now = Date.now()) {
    let dropped = 0;
    for (const [id, entry] of playerCache) {
        if (now - entry.lastUsed > IDLE_EVICT_MS) {
            playerCache.delete(id);
            dropped++;
        }
    }
    if (dropped > 0 && typeof globalThis.gc === "function") {
        try {
            globalThis.gc({ type: "major", execution: "sync", flavor: "last-resort" });
        } catch {
            globalThis.gc();
        }
    }
    return dropped;
}

let sweepTimer = null;

// startSweepTimer sweeps the prepared-player store now and every hour, and
// idle players every minute. The timer is unref'd and a second call is a
// no-op.
export function startSweepTimer() {
    if (sweepTimer) return;
    sweepStore();
    let ticks = 0;
    sweepTimer = setInterval(() => {
        sweepIdle();
        ticks++;
        if (ticks % STORE_SWEEP_EVERY_TICKS === 0) sweepStore();
    }, SWEEP_INTERVAL_MS);
    sweepTimer.unref();
}

export function playersLoaded() {
    return playerCache.size;
}

export function _entryForTest(playerID) {
    return playerCache.get(playerID);
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
