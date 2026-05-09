// Moombox BotGuard sidecar.
//
// Reads JSON-RPC requests from stdin (one object per line), dispatches to
// bgutils-js running under a JSDOM window, writes JSON-RPC responses to
// stdout. Errors and trace go to stderr (Moombox routes them to the Debug
// log).
//
// Wire protocol matches docs/investigations/botguard-sidecar-design.md §3.4:
//   Request:  {"id":<int>,"method":<str>,"params":<obj>}
//   Response: {"id":<int>,"result":<any>}    | {"id":<int>,"error":<str>}
//
// Methods: getMemoryStats, generatePoToken, getStats, invalidateCaches,
// invalidateIT, ping, solveCipher, shutdown, triggerGC.

import { JSDOM } from "jsdom";
import { BG, USER_AGENT, buildURL, getHeaders } from "bgutils-js";
import { createInterface } from "node:readline";
import { solveCipher } from "./cipher.js";

// ---------------------------------------------------------------------------
// One-time DOM bootstrap. Must run before any module that inspects globalThis
// for a window/document. bgutils-js itself reads globalThis at create() time
// (via its `globalObj` arg), but JSDOM's own internals (mutationobserver,
// classList Symbol.toStringTag, etc.) want to be the first to touch the
// global namespace.
// ---------------------------------------------------------------------------
const dom = new JSDOM(
    '<!DOCTYPE html><html lang="en"><head><title></title></head><body></body></html>',
    {
        url: "https://www.youtube.com/",
        referrer: "https://www.youtube.com/",
        userAgent: USER_AGENT,
    },
);
Object.assign(globalThis, {
    window: dom.window,
    document: dom.window.document,
    location: dom.window.location,
    origin: dom.window.origin,
});
if (!Reflect.has(globalThis, "navigator")) {
    Object.defineProperty(globalThis, "navigator", { value: dom.window.navigator });
}

// ---------------------------------------------------------------------------
// Constants. REQUEST_KEY is the long-stable API key YouTube uses for the WAA
// (BotGuard) endpoints. clientVersion mirrors what upstream's session_manager
// hard-codes; safe to bump if YouTube starts rejecting older versions.
// ---------------------------------------------------------------------------
const REQUEST_KEY = "O43z0dpjhgX20SCx4KAo";
const CLIENT_VERSION = "2.20260227.01.00";
const ATT_GET_URL =
    "https://www.youtube.com/youtubei/v1/att/get?prettyPrint=false";

// ---------------------------------------------------------------------------
// State. Single-minter design (matches Moombox's PotProvider CRIT-2 fix): the
// sidecar caches at most one minter; that minter mints PoTokens for every
// content binding until it expires.
// ---------------------------------------------------------------------------
let cachedMinter = null; // { minter, expiresAt, webPoSignalOutput }
const stats = {
    cachedMinters: 0,
    cachedSessions: 0,
    mintsTotal: 0,
    mintsErrored: 0,
};

function logErr(msg) {
    // Errors from server.js own logic — distinct from raw JSDOM stderr
    // chatter, which doesn't get our prefix. Go-side stderrPump routes
    // these to Warn level.
    process.stderr.write(`[bgutil-sidecar:error] ${msg}\n`);
}

function logWarn(msg) {
    // Server-recoverable warnings from server.js. Go-side routes these
    // to Debug.
    process.stderr.write(`[bgutil-sidecar:warn] ${msg}\n`);
}

// ---------------------------------------------------------------------------
// Core: full BotGuard fetch + interpreter + minter mint flow. Mirrors the
// upstream session_manager.generateTokenMinter pattern, but stripped of
// proxy / axios / Innertube fallbacks (Moombox always supplies a binding).
// ---------------------------------------------------------------------------
async function generateMinter() {
    // 1. Fetch the BotGuard challenge from YouTube's /att/get endpoint.
    const attResp = await fetch(ATT_GET_URL, {
        method: "POST",
        headers: {
            "Content-Type": "application/json",
            "User-Agent": USER_AGENT,
        },
        body: JSON.stringify({
            context: { client: { clientName: "WEB", clientVersion: CLIENT_VERSION } },
            engagementType: "ENGAGEMENT_TYPE_UNBOUND",
        }),
    });
    if (!attResp.ok) {
        throw new Error(`att/get HTTP ${attResp.status}`);
    }
    const attBody = await attResp.json();
    const challenge = attBody && attBody.bgChallenge;
    if (!challenge) {
        throw new Error("att/get response missing bgChallenge");
    }

    // 2. Fetch the BotGuard interpreter JS by URL given in the challenge.
    const interpUrl = `https:${challenge.interpreterUrl.privateDoNotAccessOrElseTrustedResourceUrlWrappedValue}`;
    const interpResp = await fetch(interpUrl, {
        headers: { "User-Agent": USER_AGENT },
    });
    if (!interpResp.ok) {
        throw new Error(`interpreter fetch HTTP ${interpResp.status}`);
    }
    const interpJS = await interpResp.text();

    // 3. Install the interpreter into globalThis. `new Function(...)()` runs
    // the script in module-isolated scope but with globalThis access -- which
    // is exactly what BotGuard needs to attach its `vm` object under
    // challenge.globalName.
    new Function(interpJS)();

    // 4. Spin up the BotGuard client, take its snapshot.
    const bgClient = await BG.BotGuardClient.create({
        program: challenge.program,
        globalName: challenge.globalName,
        globalObj: globalThis,
    });
    const webPoSignalOutput = [];
    const botguardResponse = await bgClient.snapshot({ webPoSignalOutput });

    // 5. POST the snapshot to GenerateIT to receive an integrity token.
    const itResp = await fetch(buildURL("GenerateIT"), {
        method: "POST",
        headers: getHeaders(),
        body: JSON.stringify([REQUEST_KEY, botguardResponse]),
    });
    if (!itResp.ok) {
        throw new Error(`GenerateIT HTTP ${itResp.status}`);
    }
    const [
        integrityToken,
        estimatedTtlSecs,
        mintRefreshThreshold,
        websafeFallbackToken,
    ] = await itResp.json();
    if (!integrityToken) {
        throw new Error(
            `GenerateIT empty integrityToken (websafeFallback=${!!websafeFallbackToken})`,
        );
    }

    // 6. Build the per-binding minter once; reuse for every binding until expiry.
    const minter = await BG.WebPoMinter.create(
        { integrityToken, estimatedTtlSecs, mintRefreshThreshold, websafeFallbackToken },
        webPoSignalOutput,
    );
    return {
        minter,
        expiresAt: Date.now() + estimatedTtlSecs * 1000,
        webPoSignalOutput,
    };
}

async function getOrCreateMinter() {
    if (cachedMinter && Date.now() < cachedMinter.expiresAt) {
        return cachedMinter;
    }
    cachedMinter = await generateMinter();
    stats.cachedMinters = 1;
    return cachedMinter;
}

async function generatePoToken(binding) {
    if (!binding || typeof binding !== "string") {
        throw new Error("missing or invalid binding");
    }
    const m = await getOrCreateMinter();
    const poToken = await m.minter.mintAsWebsafeString(binding);
    if (!poToken) {
        throw new Error("WebPoMinter returned empty poToken");
    }
    return {
        poToken,
        binding,
        expiresAt: m.expiresAt,
    };
}

// ---------------------------------------------------------------------------
// JSON-RPC dispatch. Each request runs on its own Promise so concurrent
// requests can be inflight. Responses come back in completion order, not
// request order; the Go side multiplexes via `id`.
// ---------------------------------------------------------------------------
function writeResponse(resp) {
    process.stdout.write(JSON.stringify(resp) + "\n");
}

async function dispatch(req) {
    const id = req.id;
    try {
        switch (req.method) {
            case "ping":
                return writeResponse({ id, result: "pong" });

            case "generatePoToken": {
                const params = req.params || {};
                stats.mintsTotal += 1;
                try {
                    const result = await generatePoToken(params.binding);
                    return writeResponse({ id, result });
                } catch (e) {
                    stats.mintsErrored += 1;
                    throw e;
                }
            }

            case "getMemoryStats":
                // process.memoryUsage() returns { rss, heapTotal, heapUsed,
                // external, arrayBuffers }. RSS is the resident set size — the
                // user-facing "this Node process is using N MB of RAM" number.
                // V8 heap inuse/total reveals what's pressure on the JS engine
                // (large preprocessed players, BotGuard runtime, etc.).
                return writeResponse({ id, result: process.memoryUsage() });

            case "triggerGC": {
                // Force a full V8 GC cycle. Requires Node to be launched with
                // --expose-gc (Sidecar.Config.ExposeGC). Used by Moombox to
                // give the sidecar soft-limit semantics: when RSS exceeds the
                // configured threshold, parent calls triggerGC instead of
                // letting V8's hard --max-old-space-size kick in (which would
                // OOM-abort the process). Returns memory stats before/after
                // so the parent can log the effect.
                if (typeof globalThis.gc !== "function") {
                    return writeResponse({
                        id,
                        error: "triggerGC: --expose-gc not enabled in this sidecar",
                    });
                }
                const before = process.memoryUsage();
                try {
                    globalThis.gc();
                } catch (e) {
                    return writeResponse({
                        id,
                        error: `triggerGC: ${e && e.message ? e.message : String(e)}`,
                    });
                }
                const after = process.memoryUsage();
                return writeResponse({ id, result: { before, after } });
            }

            case "invalidateCaches":
                cachedMinter = null;
                stats.cachedMinters = 0;
                stats.cachedSessions = 0;
                return writeResponse({ id, result: "ok" });

            case "invalidateIT":
                cachedMinter = null;
                stats.cachedMinters = 0;
                return writeResponse({ id, result: "ok" });

            case "getStats":
                return writeResponse({ id, result: { ...stats } });

            case "solveCipher": {
                const params = req.params || {};
                const coerceArray = (v, name) => {
                    if (v == null) return [];
                    if (Array.isArray(v)) return v;
                    throw new Error(`solveCipher: ${name} must be an array, got ${typeof v}`);
                };
                try {
                    const result = solveCipher({
                        playerID: params.playerID,
                        playerJS: params.playerJS,
                        sigChallenges: coerceArray(params.sigChallenges, "sigChallenges"),
                        nChallenges: coerceArray(params.nChallenges, "nChallenges"),
                        forceReload: !!params.forceReload,
                    });
                    return writeResponse({ id, result });
                } catch (e) {
                    // Surface the PLAYER_NOT_LOADED sentinel verbatim so the Go side
                    // can recognise it and retry with playerJS attached. Other errors
                    // (including coerceArray validation failures) pass through as
                    // generic strings.
                    const msg = e && e.code === "PLAYER_NOT_LOADED"
                        ? "player not loaded"
                        : (e && e.message ? e.message : String(e));
                    return writeResponse({ id, error: msg });
                }
            }

            case "shutdown":
                writeResponse({ id, result: "bye" });
                // Drain stdout, then exit. setImmediate ensures the line is
                // flushed before the process tears down.
                setImmediate(() => process.exit(0));
                return;

            default:
                return writeResponse({ id, error: `unknown method: ${req.method}` });
        }
    } catch (e) {
        const msg = e && e.message ? e.message : String(e);
        writeResponse({ id, error: msg });
    }
}

// ---------------------------------------------------------------------------
// Stdin loop. One JSON object per line; malformed lines log to stderr and
// are dropped (defensive against partial writes during a parent crash).
//
// Inflight tracking exists so that when stdin closes (parent gone, or test
// harness piped finite input), we still wait for in-progress requests to
// finish writing their responses before exiting. Without this, an async
// generatePoToken would be killed mid-flight and the smoke-test pipeline
// would hang waiting for a response that never came.
// ---------------------------------------------------------------------------
let inflight = 0;
let stdinClosed = false;

function maybeExit() {
    if (stdinClosed && inflight === 0) {
        setImmediate(() => process.exit(0));
    }
}

const rl = createInterface({ input: process.stdin });
rl.on("line", (line) => {
    line = line.trim();
    if (!line) return;
    let req;
    try {
        req = JSON.parse(line);
    } catch (e) {
        logErr(`invalid JSON on stdin: ${e.message}`);
        return;
    }
    if (typeof req !== "object" || req === null || typeof req.id !== "number") {
        logErr(`invalid request shape: ${line.slice(0, 80)}`);
        return;
    }
    inflight += 1;
    dispatch(req).finally(() => {
        inflight -= 1;
        maybeExit();
    });
});
rl.on("close", () => {
    stdinClosed = true;
    maybeExit();
});

// Belt-and-suspenders: signal handlers in case the parent kills us hard.
process.on("SIGINT", () => process.exit(0));
process.on("SIGTERM", () => process.exit(0));

// Signal to the parent that synchronous init is complete and the readline
// interface is wired up. The parent waits for this line before treating the
// sidecar as healthy, instead of polling with a fixed-deadline ping. The
// previous ping-based handshake started racing against jsdom's cold-start
// time after the jsdom 27→29 bump in v2.6.14 (module load + DOM construction
// can exceed 5s on Windows even with a warm filesystem cache). Notifications
// are distinguished from JSON-RPC responses by the absence of an `id` field.
process.stdout.write('{"event":"ready"}\n');
