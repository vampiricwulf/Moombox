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
import { BotGuardClient } from "bgutils-js/botguard";
import { WebPoMinter } from "bgutils-js/webpo";
import { USER_AGENT, buildURL, getHeaders } from "bgutils-js/utils";
import { createInterface } from "node:readline";
import { solveCipher } from "./cipher.js";
import { extractYtcfg, extractHomepageChallenge } from "./homepage.js";

// ---------------------------------------------------------------------------
// One-time DOM bootstrap. Must run before any module that inspects globalThis
// for a window/document. bgutils-js itself reads globalThis at create() time
// (via its `globalObject` arg), but JSDOM's own internals (mutationobserver,
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
const CLIENT_VERSION = "2.20260708.00.00";
const ATT_GET_URL =
    "https://www.youtube.com/youtubei/v1/att/get?prettyPrint=false";
const HOMEPAGE_URL = "https://www.youtube.com/";
// Per-fetch ceiling for the BotGuard network round-trips. Node's fetch (undici)
// has no overall request timeout — a hung YouTube endpoint would otherwise wedge
// minterPromise for ~300s (undici's headers timeout), and since every mint awaits
// that one promise, all mints cascade into the parent's 90s RPC timeout while the
// sidecar still reports healthy. 30s is well above the happy path (a few seconds)
// and well under the parent's 90s budget, so a genuine hang aborts fast and the
// next mint retries from scratch instead of piggybacking a doomed attempt.
const FETCH_TIMEOUT_MS = 30_000;
// Whole-generation ceiling. The three fetches above each carry
// FETCH_TIMEOUT_MS, and 3 × 30s meets or exceeds the parent's 90s RPC
// timeout (sidecar.Config.RequestTimeout) all by itself — so a slow-but-not
// hung YouTube could burn the parent's entire budget and still be running,
// holding serializeChain, when the parent gives up. Worse, a Go-side RPC
// timeout does NOT cancel this generation, so the next mint would queue
// behind a request already known dead. Every fetch's abort signal is derived
// from whatever remains of this budget instead of a flat 30s. 75s leaves
// headroom under 90s for the BotGuard pass itself, which is CPU work with no
// fetch of its own.
const GENERATION_BUDGET_MS = 75_000;
// The homepage fetch is an OPTIMIZATION — every failure path falls back — so
// it gets a tight ceiling rather than a share of the full budget. It must
// never be the reason a mint misses its deadline: the mid-download 403
// credential refresh calls this path with freshMinter (guaranteeing a full
// regeneration) under a budget of min(45s, MaxTimeout/3), which is as low as
// 10s at the configured floor. Measured homepage RTT is ~340ms, so 8s is
// ~20x headroom on the happy path and still cheap to abandon.
const HOMEPAGE_FETCH_TIMEOUT_MS = 8_000;

// Ceiling on the homepage body we will buffer. The real page measured
// ~875 KB decompressed on 2026-08-24, so 4 MB is ~4x headroom. Every body
// read on the Go side is capped (10 MB player, 50 MB watch page, 20 MB
// BotGuard, 1 MB challenge); this keeps the sidecar to the same rule so a
// misbehaving or hostile edge response cannot balloon the heap of a process
// that runs for weeks.
const HOMEPAGE_MAX_BYTES = 4 << 20;

// readCapped buffers a response body, aborting past `limit` bytes rather
// than growing without bound.
async function readCapped(resp, limit, label) {
    if (!resp.body) return "";
    const decoder = new TextDecoder();
    let out = "";
    let total = 0;
    for await (const chunk of resp.body) {
        total += chunk.length;
        if (total > limit) {
            throw new Error(`${label} response exceeded ${limit} bytes`);
        }
        out += decoder.decode(chunk, { stream: true });
    }
    return out + decoder.decode();
}

// budgetSignal returns an AbortSignal bounded by BOTH the per-fetch cap and
// the remaining generation budget, or throws when the budget is already
// spent so the caller fails fast instead of starting a doomed request.
function budgetSignal(deadline, capMs) {
    const remaining = deadline - Date.now();
    if (remaining <= 0) {
        throw new Error("generation budget exhausted");
    }
    return AbortSignal.timeout(Math.min(capMs, remaining));
}

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
// Interpreter-URL origin gate.
//
// generateMinter executes the interpreter body it fetches (`new Function(js)()`),
// so the URL's host is a code-execution boundary, not a hint. Only Google-owned
// hosts may serve it. Suffix matches are anchored to a leading dot so
// "evilgoogle.com" and "google.com.evil.tld" cannot pass.
// ---------------------------------------------------------------------------
// Keep this list in sync with allowedInterpreterDomains in
// internal/youtube/watch_page.go — both gates enforce the same rule, one
// before the challenge leaves Go and one before this process executes code.
// Only domains serving GOOGLE-AUTHORED code. Domains whose bytes are
// user-uploaded (googleusercontent.com, ggpht.com, googlevideo.com,
// i.ytimg.com) are excluded on purpose: the fetched body is executed, and an
// image/JS polyglot uploaded to such a host would pass a naive "is it Google?"
// check. ytimg.com is admitted by exact static host only.
// EXACT hosts only. Keep in sync with allowedInterpreterHosts in
// internal/youtube/watch_page.go, which documents why suffix matching and
// regional patterns were both removed: `.googleapis.com` re-admitted
// storage.googleapis.com (anyone's uploaded bucket objects) and
// `^google\.[a-z]{2,3}…$` matched registrable third-party domains such as
// google.com.se. Both were proven to reach code execution.
const ALLOWED_INTERPRETER_HOSTS = [
    "www.google.com",
    "google.com",
    "www.gstatic.com",
    "ssl.gstatic.com",
    "gstatic.com",
    "s.ytimg.com",
    "www.youtube.com",
    "youtube.com",
];

function isGoogleOwnedHost(hostname) {
    const host = hostname.toLowerCase().replace(/\.$/, "");
    return host !== "" && ALLOWED_INTERPRETER_HOSTS.includes(host);
}

function assertGoogleHost(rawUrl) {
    let parsed;
    try {
        parsed = new URL(rawUrl);
    } catch {
        throw new Error("interpreter URL is not a valid URL");
    }
    if (parsed.protocol !== "https:") {
        throw new Error(`interpreter URL protocol not allowed: ${parsed.protocol}`);
    }
    if (parsed.username || parsed.password) {
        throw new Error("interpreter URL carries userinfo");
    }
    if (!isGoogleOwnedHost(parsed.hostname)) {
        throw new Error(`interpreter URL host not allowed: ${parsed.hostname}`);
    }
    // An allowlisted host is NOT the same as Google-authored bytes:
    // www.google.com serves JSONP endpoints that reflect an attacker-supplied
    // callback at HTTP 200 (e.g. /complete/search?client=firefox&jsonp=...),
    // and step 3 executes whatever this fetch returns. The genuine interpreter
    // is a static TrustedResourceUrl — a bare .js path, no query, no fragment
    // (observed //www.google.com/js/th/<hash>.js) — and reflection requires a
    // query to reflect, so demanding that shape removes the whole class.
    if (parsed.search !== "" || parsed.hash !== "") {
        throw new Error("interpreter URL carries a query or fragment");
    }
    // Validate the still-encoded path against an unreserved alphabet rather
    // than just checking the .js suffix. Go decodes %3F into its path while
    // this parser keeps it encoded, so a crafted
    // /complete/search%3Fjsonp=<payload>.js reads differently on each side;
    // excluding '%' removes the disagreement instead of relying on Google
    // returning 404 for it. Keep in sync with staticScriptPathRe in
    // internal/youtube/watch_page.go.
    if (!/^\/[A-Za-z0-9._~/-]+\.[Jj][Ss]$/.test(parsed.pathname)) {
        throw new Error(`interpreter URL is not a static script: ${parsed.pathname}`);
    }
    return parsed.href;
}

// Fetch the interpreter with redirects DISABLED. undici follows redirects by
// default and only the pre-redirect URL passed the host gate, so an
// allowlisted host answering 302 → attacker.tld handed us a body we then
// executed (proven against live local servers, 2026-08-15). Refusing 3xx
// outright is safer than re-gating response.url: YouTube serves the
// interpreter directly, so a redirect here is already anomalous.
async function fetchInterpreterScript(interpUrl, deadline) {
    const resp = await fetch(interpUrl, {
        headers: { "User-Agent": USER_AGENT },
        redirect: "manual",
        signal: budgetSignal(deadline, FETCH_TIMEOUT_MS),
    });
    if (resp.status >= 300 && resp.status < 400) {
        throw new Error(
            `interpreter fetch redirected (${resp.status} → ${resp.headers.get("location") ?? "?"}); refusing to follow`,
        );
    }
    if (!resp.ok) {
        throw new Error(`interpreter fetch HTTP ${resp.status}`);
    }
    return resp.text();
}

// ---------------------------------------------------------------------------
// Homepage-pair challenge sourcing (upstream bgutil-ytdlp-pot-provider
// 495a47f parity). Fetch the YouTube homepage, extract a self-consistent
// (ytcfg, ytAtN challenge) pair from that single page, and inject
// globalThis.yt = { config_: ytcfg } so the BotGuard snapshot sees the
// session's EVENT_ID — YouTube rejects WebPO tokens minted from /att/get
// challenges when the session is enrolled in the binding experiment
// (symptom: player requests pass, googlevideo 403s the download).
//
// Returns a canonicalized challenge or null; every null path logs its
// reason and degrades to the caller's next source, so behavior is never
// worse than the pre-homepage flow.
//
// PAIRING IS ALL-OR-NOTHING. Upstream installs the ytcfg as soon as it
// parses, before the challenge is validated, and shrugs that an unpaired
// EVENT_ID is "no worse than absent". It is worse: a homepage EVENT_ID
// married to an /att/get challenge is a deliberately INCOHERENT pair, which
// is the exact condition this whole module exists to eliminate, and once
// installed it persisted across later failed fetches (a stale EVENT_ID from
// hours ago). So the injection happens only on the success path, together
// with the challenge it belongs to, and any failure clears whatever a
// previous run left behind.
// ---------------------------------------------------------------------------

// clearYtConfig removes an injected yt.config_ so a failed or fallback mint
// never runs against a previous run's EVENT_ID.
function clearYtConfig() {
    delete globalThis.yt;
    if (globalThis.window) delete globalThis.window.yt;
}

async function fetchHomepageChallenge(deadline) {
    // Drop any previous run's injection FIRST. Every early return below then
    // means "no ytcfg is installed", never "the last successful load's one is
    // still installed" — the two are indistinguishable to the snapshot but
    // very different to YouTube.
    clearYtConfig();

    let pageHtml;
    try {
        const resp = await fetch(HOMEPAGE_URL, {
            // No redirects: a challenge is only trusted off the constant URL
            // above. A consent-wall or region redirect lands here and falls
            // back — consistent with the other fetches in this file.
            redirect: "manual",
            headers: {
                accept: "*/*",
                "accept-language": "en-US,en;q=0.7",
                "user-agent": USER_AGENT,
            },
            signal: budgetSignal(deadline, HOMEPAGE_FETCH_TIMEOUT_MS),
        });
        if (resp.status >= 300 && resp.status < 400) {
            logWarn(
                `homepage-challenge: redirected (${resp.status} → ${resp.headers.get("location") ?? "?"}); falling back`,
            );
            return null;
        }
        if (!resp.ok) {
            logWarn(`homepage-challenge: HTTP ${resp.status}; falling back`);
            return null;
        }
        pageHtml = await readCapped(resp, HOMEPAGE_MAX_BYTES, "homepage");
    } catch (e) {
        logWarn(`homepage-challenge: fetch failed (${e?.message ?? e}); falling back`);
        return null;
    }

    // Extract and validate the challenge BEFORE installing anything. The
    // ytcfg is only useful as the challenge's pair-mate, so a challenge we
    // end up rejecting must leave no ytcfg behind.
    const { challenge, reason } = extractHomepageChallenge(pageHtml);
    if (!challenge) {
        logWarn(`homepage-challenge: ${reason}; falling back`);
        return null;
    }
    // Gate the interpreter origin HERE, not just in generateMinter's fetch
    // path: a disallowed host must degrade to the next challenge source
    // instead of failing the whole mint.
    try {
        assertGoogleHost(
            `https:${challenge.interpreterUrl.privateDoNotAccessOrElseTrustedResourceUrlWrappedValue}`,
        );
    } catch (e) {
        logWarn(`homepage-challenge: interpreter refused (${e?.message ?? e}); falling back`);
        return null;
    }

    const ytcfg = extractYtcfg(pageHtml);
    if (!ytcfg) {
        // The challenge alone, without its page's EVENT_ID, is not the
        // coherent pair this path exists to produce. Fall back rather than
        // mint from half of it.
        logWarn("homepage-challenge: no parseable ytcfg on page (EVENT_ID missing); falling back");
        return null;
    }
    // BotGuard reads yt.config_.EVENT_ID off its globalObject (globalThis).
    // Returned to the caller so it can re-assert the binding immediately
    // before the snapshot — the interpreter runs `new Function(js)()` with
    // full globalThis access in between, and a clobbered `yt` would fail
    // silently as a wrong EVENT_ID rather than an exception.
    const ytObj = { config_: ytcfg };
    globalThis.yt = ytObj;
    if (globalThis.window) globalThis.window.yt = ytObj;
    return { challenge, ytObj };
}

// ---------------------------------------------------------------------------
// Core: full BotGuard fetch + interpreter + minter mint flow. Mirrors the
// upstream session_manager.generateTokenMinter pattern, but stripped of
// proxy / axios / Innertube fallbacks (Moombox always supplies a binding).
// ---------------------------------------------------------------------------
// challenge: a parsed bgChallenge object from the caller's watch page, or
// null. Challenge preference order mirrors upstream 495a47f: the homepage
// (ytcfg, ytAtN) pair beats BOTH the RPC-supplied watch-page challenge
// (which cannot be paired with its originating page's ytcfg here) and the
// /att/get fallback (whose tokens the experiment rejects outright).
// deadline/ytObj are internal recursion state — callers pass neither.
// `attempted` guards the homepage fetch so the inline-JS refusal recursion
// below cannot pay for it twice (a second round trip AND a second
// globalThis.yt overwrite) inside one RPC.
async function generateMinter(challenge, deadline, homepageAttempted = false) {
    deadline ??= Date.now() + GENERATION_BUDGET_MS;
    let minterSource = "challenge";
    let ytObj = null;
    if (!homepageAttempted) {
        const pair = await fetchHomepageChallenge(deadline);
        if (pair) {
            challenge = pair.challenge;
            ytObj = pair.ytObj;
            minterSource = "homepage";
        }
        homepageAttempted = true;
    }
    if (!challenge) {
        minterSource = "att_get";
        // 1. Fetch the BotGuard challenge from YouTube's /att/get endpoint.
        // Header keys below are deliberately lowercase to match getHeaders()'s
        // own casing (content-type, user-agent) and OVERWRITE those entries
        // rather than duplicate them: native fetch's Headers treats
        // case-varied keys as the SAME header and joins their values with
        // ", " (verified against Node's undici), so a differently-cased
        // "Content-Type" here would silently corrupt the request into
        // "application/json+protobuf, application/json". getHeaders() also
        // contributes x-goog-api-key / x-user-agent, matching upstream
        // session_manager.ts's /att/get call.
        const attResp = await fetch(ATT_GET_URL, {
            method: "POST",
            // No redirects: this response is treated as TRUSTED (its inline
            // interpreterJavascript is executed without an origin check), so
            // it must come from the constant URL above and nowhere a redirect
            // could point.
            redirect: "manual",
            headers: {
                ...getHeaders(),
                "content-type": "application/json",
                "user-agent": USER_AGENT,
            },
            body: JSON.stringify({
                context: { client: { clientName: "WEB", clientVersion: CLIENT_VERSION } },
                engagementType: "ENGAGEMENT_TYPE_UNBOUND",
            }),
            signal: budgetSignal(deadline, FETCH_TIMEOUT_MS),
        });
        if (!attResp.ok) {
            throw new Error(`att/get HTTP ${attResp.status}`);
        }
        const attBody = await attResp.json();
        challenge = attBody && attBody.bgChallenge;
        if (!challenge) {
            throw new Error("att/get response missing bgChallenge");
        }
    }

    // 2. Resolve the interpreter JS. bgutils-js types interpreterJavascript
    // (inline) and interpreterUrl independently optional, and YouTube's own
    // player checks the inline form FIRST. Inline execution skips a fetch
    // entirely, but is only trusted when this challenge came from OUR OWN
    // /att/get fetch above (a real, verified YouTube API response) --
    // `minterSource === "att_get"` is exactly that condition. A challenge
    // supplied over the RPC (page-scraped, attacker-adjacent) carrying
    // inline JS has no origin we can check, so it is refused outright and we
    // regenerate from the fallback chain instead -- the Go side already
    // refuses to forward such challenges; this is defense in depth, not the
    // primary control. (generatePoToken's shape check independently requires
    // interpreterUrl on RPC-supplied challenges, so in practice this branch
    // only fires if that gate is ever bypassed or weakened. A homepage
    // challenge can never land here either: extractHomepageChallenge rebuilds
    // it from exactly program/globalName/interpreterUrl, so inline JS cannot
    // ride along — which is also why the recursion below cannot loop.)
    const trusted = minterSource === "att_get";
    const inline = challenge.interpreterJavascript;
    const inlineJS =
        inline && typeof inline.privateDoNotAccessOrElseSafeScriptWrappedValue === "string"
            ? inline.privateDoNotAccessOrElseSafeScriptWrappedValue
            : null;

    let interpJS;
    if (inlineJS !== null && !trusted) {
        logWarn(
            "RPC-supplied challenge carried inline interpreterJavascript; refusing and regenerating from homepage//att/get",
        );
        return generateMinter(null, deadline, homepageAttempted);
    } else if (inlineJS !== null) {
        interpJS = inlineJS;
    } else {
        if (!challenge.interpreterUrl) {
            throw new Error("challenge has neither interpreterJavascript nor interpreterUrl");
        }
        // Fetch the BotGuard interpreter JS by URL given in the challenge.
        // The URL is HARD-GATED to Google-owned hosts: step 3 executes
        // whatever this fetch returns, and since v2.7.8 a challenge can
        // originate from a watch page (window.ytAtN), whose HTML embeds
        // attacker-authored video metadata. Without this gate a crafted
        // description could point the interpreter at any host and get its
        // JavaScript executed in this process.
        const interpUrl = `https:${challenge.interpreterUrl.privateDoNotAccessOrElseTrustedResourceUrlWrappedValue}`;
        // Gate the host, then fetch WITHOUT following redirects — an
        // allowlisted host answering 302 → attacker would otherwise deliver
        // the body we execute. See fetchInterpreterScript.
        interpJS = await fetchInterpreterScript(assertGoogleHost(interpUrl), deadline);
    }

    // 3. Install the interpreter into globalThis. `new Function(...)()` runs
    // the script in module-isolated scope but with globalThis access -- which
    // is exactly what BotGuard needs to attach its `vm` object under
    // challenge.globalName.
    new Function(interpJS)();

    // 4. Spin up the BotGuard client, take its snapshot.
    // Re-assert the ytcfg binding: `new Function(interpJS)()` above just ran
    // Google-authored code with full globalThis access, and if it defined or
    // reset `yt` the snapshot would silently read the wrong EVENT_ID.
    if (ytObj) {
        globalThis.yt = ytObj;
        if (globalThis.window) globalThis.window.yt = ytObj;
    }

    const bgClient = await BotGuardClient.create({
        program: challenge.program,
        globalName: challenge.globalName,
        globalObject: globalThis,
    });
    const webPoSignalOutput = [];
    const botguardResponse = await bgClient.snapshot({ webPoSignalOutput });

    // 5. POST the snapshot to GenerateIT to receive an integrity token.
    const itResp = await fetch(buildURL("GenerateIT"), {
        method: "POST",
        // Fixed Google endpoint; refuse redirects so a hijacked hop can never
        // observe the BotGuard snapshot we post or answer for the integrity
        // token. Consistent with the other two fetches in this file.
        redirect: "manual",
        headers: getHeaders(),
        body: JSON.stringify([REQUEST_KEY, botguardResponse]),
        signal: budgetSignal(deadline, FETCH_TIMEOUT_MS),
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
    const minter = await WebPoMinter.create(
        { integrityToken, estimatedTtlSecs, mintRefreshThreshold, websafeFallbackToken },
        webPoSignalOutput,
    );
    return {
        minter,
        expiresAt: Date.now() + estimatedTtlSecs * 1000,
        webPoSignalOutput,
        // Tracked so getOrCreateMinter can free the stale interpreter VM when
        // YouTube rotates globalName between regenerations (see below).
        globalName: challenge.globalName,
        minterSource,
        // Whether a homepage ytcfg (EVENT_ID) was paired into this minter.
        // minterSource alone no longer says: a non-homepage source can still
        // have had one injected in principle, and the whole point of the
        // provenance line is diagnosing a 403 months from now.
        ytcfgPaired: ytObj !== null,
    };
}

// In-flight dedup + serialization for minter regeneration.
//
// At most one generateMinter() call ever actually executes at a time: it's a
// full multi-second BotGuard pass that installs its interpreter onto the
// shared globalThis via `new Function(interpJS)()`, and two interleaved runs
// would capture each other's VM mid-flight and cross-contaminate the
// resulting minter. But callers that share the SAME challenge legitimately
// share one execution and one honest provenance report; callers with
// DIFFERENT challenges must NOT share one — a joiner reporting
// "minterSource: challenge, minterFresh: true" for a minter actually built
// from a DIFFERENT job's watch-page challenge would assert session coherence
// it doesn't have, and that log line is specifically what's relied on to
// diagnose premiere 403s months from now.
//
// `minterInflight` maps EVERY in-flight challenge key to its generation
// promise; a caller with a matching key joins it. It is a map rather than a
// single slot because a queued different-challenge call would otherwise evict
// the running key, so a later caller sharing that challenge would miss the
// generation it should have joined and pay for a redundant BotGuard pass.
// `serializeChain` is the ordering backbone — every generation (matched or
// not) chains onto it, so a caller with a non-matching key waits for whatever
// is currently running before starting its own: one generateMinter()
// execution at a time, no exceptions. (Named distinctly from the unrelated
// `inflight` request counter in the stdin loop below, which tracks
// in-progress RPC dispatches.)
const minterInflight = new Map(); // challengeKey -> Promise
let serializeChain = Promise.resolve();

// /att/get callers (challenge === null) legitimately share one minter —
// there's no per-session challenge to keep coherent on that path; it's
// already documented above as session-incoherent by design. Give them one
// stable key so they still dedup/join against each other. Not valid JSON
// (a raw challenge string always starts with "{"), so it can never collide
// with a real challenge.
const ATT_GET_KEY = "att_get:no-challenge";
function normalizeChallengeKey(rawKey) {
    return rawKey || ATT_GET_KEY;
}

// freshMinter forces a regeneration even when the cache is valid (the
// fresh-per-GVS-mint policy). challengeKey is the RAW challenge JSON string
// generatePoToken parsed `challenge` from (or null for the /att/get path) —
// used only for equality against the in-flight key, never re-derived from
// `challenge` itself, so it exactly reflects what the caller sent.
// minterFactory defaults to the real generateMinter; overridable so tests can
// exercise the keying/serialization logic without real network/BotGuard work.
export async function getOrCreateMinter(challenge, challengeKey, freshMinter, minterFactory = generateMinter) {
    if (!freshMinter && cachedMinter && Date.now() < cachedMinter.expiresAt) {
        return { m: cachedMinter, fresh: false };
    }

    const key = normalizeChallengeKey(challengeKey);

    const running = minterInflight.get(key);
    if (running) {
        // Same challenge as a generation already in flight: the minter it
        // resolves to really was built from OUR challenge, so reporting
        // fresh+matching provenance is honest.
        //
        // Tracking EVERY in-flight key (not just the most recent) matters:
        // with a single slot, a different-challenge call queued in between
        // evicted the first key, so a third caller sharing the first
        // challenge missed the running generation and paid for a redundant
        // BotGuard pass.
        const m = await running;
        return { m, fresh: true };
    }

    // Different challenge (or nothing in flight yet). Queue behind whatever
    // is currently running so generateMinter() never overlaps with itself,
    // then run our own — independent of, and never shared with, whatever was
    // queued ahead of it.
    const runAfter = serializeChain;
    const generation = (async () => {
        await runAfter;
        const prev = cachedMinter;
        const m = await minterFactory(challenge);
        cachedMinter = m;
        // Free the previous interpreter's VM when YouTube rotates globalName.
        // generateMinter runs `new Function(interpJS)()`, which attaches the VM
        // under globalThis[globalName]. A stable name is overwritten in place (the
        // old VM becomes GC-eligible), but a ROTATED name leaves globalThis[oldName]
        // pinned forever — one leaked VM per rotation over a days-long run. The live
        // minter keeps its own VM reachable via the mintCallback closure (not via
        // globalThis), so deleting the stale property is safe even for an in-flight
        // mint that captured the old minter.
        if (prev && prev.globalName && prev.globalName !== m.globalName) {
            try {
                delete globalThis[prev.globalName];
            } catch (e) {
                logWarn(`could not free stale interpreter global: ${e?.message ?? e}`);
            }
        }
        stats.cachedMinters = 1;
        return m;
    })();

    // Chain future generations behind this one regardless of outcome; a
    // rejected generation must not wedge the serialization queue.
    serializeChain = generation.catch(() => {});
    minterInflight.set(key, generation);
    // .catch before .finally: a bare `generation.finally(...)` creates a
    // DERIVED promise with no rejection handler, so every failed generation
    // also emitted a process-level unhandledRejection — surfacing as a
    // duplicate Warn line alongside the real RPC error, in exactly the
    // diagnostic channel this design cares about. The caller below still
    // awaits `generation` itself and sees the rejection.
    generation.catch(() => {}).finally(() => {
        if (minterInflight.get(key) === generation) {
            minterInflight.delete(key);
        }
    });

    const m = await generation;
    return { m, fresh: true };
}

async function generatePoToken(binding, challengeJSON, freshMinter) {
    if (!binding || typeof binding !== "string") {
        throw new Error("missing or invalid binding");
    }
    let challenge = null;
    // The raw JSON string `challenge` was parsed from, used only as the
    // in-flight/join key (see getOrCreateMinter) — never re-derived from
    // `challenge` itself so it exactly reflects what this caller sent. Stays
    // null unless a challenge actually passed the shape check below.
    let challengeKey = null;
    if (challengeJSON && typeof challengeJSON === "string") {
        try {
            const parsed = JSON.parse(challengeJSON);
            // Defensive shape check: a challenge missing its program or
            // interpreter URL would crash generateMinter mid-flight; treat
            // it as absent so the /att/get fallback runs instead. Deliberately
            // requires interpreterUrl, not the inline interpreterJavascript
            // form — inline JS arriving over the RPC has no verifiable
            // origin, and generateMinter refuses it independently too (see
            // its "trusted" check) as defense in depth.
            if (parsed && parsed.program && parsed.interpreterUrl) {
                challenge = parsed;
                challengeKey = challengeJSON;
            } else {
                logWarn("challenge missing program/interpreterUrl; using homepage//att/get fallback");
            }
        } catch (e) {
            logWarn(`invalid challenge JSON ignored: ${e?.message ?? e}`);
        }
    }
    const { m, fresh } = await getOrCreateMinter(challenge, challengeKey, !!freshMinter);
    const poToken = await m.minter.mintAsWebsafeString(binding);
    if (!poToken) {
        throw new Error("WebPoMinter returned empty poToken");
    }
    return {
        poToken,
        binding,
        expiresAt: m.expiresAt,
        minterSource: m.minterSource,
        minterFresh: fresh,
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
                    const result = await generatePoToken(
                        params.binding,
                        params.challenge,
                        params.freshMinter,
                    );
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

// Log-and-survive for stray async errors. Since Node 15 an unhandled rejection
// aborts the process by default; a single bad mint/solve would then kill this
// long-lived sidecar and force the parent into a multi-second cold restart (or
// a permanent goja fallback). No current path floats a rejection, but this keeps
// the 24/7 process alive. A genuinely wedged sidecar still surfaces via the
// parent's RPC timeout → markUnhealthy → fallback, so surviving never traps us.
process.on("unhandledRejection", (reason) => {
    logErr(`unhandledRejection: ${reason && reason.message ? reason.message : String(reason)}`);
});
process.on("uncaughtException", (err) => {
    logErr(`uncaughtException: ${err && err.message ? err.message : String(err)}`);
});

// Signal to the parent that synchronous init is complete and the readline
// interface is wired up. The parent waits for this line before treating the
// sidecar as healthy, instead of polling with a fixed-deadline ping. The
// previous ping-based handshake started racing against jsdom's cold-start
// time after the jsdom 27→29 bump in v2.6.14 (module load + DOM construction
// can exceed 5s on Windows even with a warm filesystem cache). Notifications
// are distinguished from JSON-RPC responses by the absence of an `id` field.
process.stdout.write('{"event":"ready"}\n');
