// Homepage (ytcfg, ytAtN) pair extraction for session-coherent WebPO minting.
//
// YouTube binds the initial attestation challenge to the webpage session
// (yt.config_.EVENT_ID) and rejects WebPO tokens minted from /att/get
// challenges when the session is enrolled in the experiment — symptom:
// player requests pass, googlevideo 403s the download (upstream
// bgutil-ytdlp-pot-provider 495a47f, LuanRT/BgUtils#44). The fix is to mint
// from a SELF-CONSISTENT pair extracted from one youtube.com homepage fetch:
// the page's ytcfg (injected as globalThis.yt.config_ so the BotGuard
// snapshot sees EVENT_ID) and the page's own window.ytAtN challenge.
//
// This module is the pure extraction half — no network, no globals — so it
// can be tested against HTML fixtures. server.js owns the fetch, the ytcfg
// injection, and the interpreter-origin gate.
//
// Extraction hardening mirrors internal/youtube/watch_page.go rather than
// upstream's regexes:
//   - The object extent comes from string-aware brace balancing, not a
//     non-greedy regex: a `})` inside the opaque challenge program truncates
//     upstream's /{[\s\S]*?}/ match into an unparseable fragment, and a
//     `});` inside any ytcfg string value truncates /ytcfg\.set\(({.+?})\);/s
//     the same way.
//   - The outer ytAtN blob is parsed by a tolerant recursive-descent parser
//     (parseLooseJSON below), never eval'd or Function()'d — page HTML embeds
//     third-party-authored strings. Note this keeps page text from being
//     EXECUTED; it does not keep page text from producing a challenge (see
//     the trust note on parseLooseJSON).
//   - The returned challenge is REBUILT from exactly the three fields the
//     minter consumes (program, globalName, interpreterUrl), matching Go's
//     canonicalizeChallenge: no extra keys, no inline interpreterJavascript
//     riding along, no parser-differential surface. A challenge without an
//     interpreterUrl is refused outright — inline JS from scraped HTML has
//     no origin to check.

// scanBalancedObject returns the balanced `{...}` object literal starting at
// s[0], or null when s does not start with '{' or the object never closes.
// String literals (single/double quoted, with backslash escapes) are skipped
// so braces inside them cannot unbalance the scan.
export function scanBalancedObject(s) {
    if (s[0] !== "{") return null;
    let depth = 0;
    let quote = null;
    let escaped = false;
    for (let i = 0; i < s.length; i++) {
        const ch = s[i];
        if (quote !== null) {
            if (escaped) {
                escaped = false;
            } else if (ch === "\\") {
                escaped = true;
            } else if (ch === quote) {
                quote = null;
            }
            continue;
        }
        switch (ch) {
            case '"':
            case "'":
                quote = ch;
                break;
            case "{":
                depth++;
                break;
            case "}":
                depth--;
                if (depth === 0) return s.slice(0, i + 1);
                break;
        }
    }
    return null;
}

// parseLooseJSON parses a JS-object-literal-ish value (unquoted keys,
// single-quoted strings) into plain data without ever executing it. Throws
// on anything outside that grammar — a backslash outside a string, an
// identifier value, a function call. Named after bgutils-js's own helper,
// which IS available at our pin (^4.0.3 exports parseLooseJSON from
// bgutils-js/utils) — this is a deliberate replacement, not a shim for a
// missing export. The vendored one rewrites single-quoted strings with a
// global regex, which mangles any apostrophe inside a double-quoted string;
// on a page built from arbitrary video titles that is a real hazard.
//
// NOT a security boundary. An earlier version of this comment claimed the
// throw-on-anything-else behavior stopped attacker text smuggled through
// page metadata; that was WRONG and the test asserting it only proved the
// double-quoted case. JSON string escaping touches none of ' { } : or bare
// identifiers, so a payload written entirely in single quotes survives
// verbatim inside a JSON string value and parses cleanly here — verified
// 2026-08-24 against this exact function. Assume any object on the page can
// reach this parser and come out intact. What actually contains the damage
// is assertGoogleHost in server.js (the interpreter-origin gate) plus the
// canonicalization below, and those are what must never be relaxed.
//
// Residual risk, stated plainly so nobody has to rediscover it: on the live
// homepage the genuine window.ytAtN( call sits at ~byte 714k and
// ytInitialData — the only third-party-authored surface — begins at ~757k,
// so the genuine call is always the first VALID candidate and wins. That is
// YouTube's emission ordering, not something enforced here. If it ever
// flipped (a populated feed rendered before the attestation bootstrap, or
// this fetch made session-aware), a crafted video title could supply the
// challenge. The impact even then is bounded to denial of POT minting, NOT
// code execution: the interpreter origin is still gated to Google hosts, and
// an attacker program paired with a genuine Google interpreter fails inside
// BotGuardClient.create. Verified by adversarial probe 2026-08-24, which
// could not defeat the host gate across ~30 host-confusion payloads. A
// structural anchor (requiring the call to sit outside any JSON string) was
// considered and rejected: every cheap version of that test risks rejecting
// the GENUINE challenge, and falling back to /att/get is the exact
// degradation this module exists to prevent.
export function parseLooseJSON(text) {
    let i = 0;
    const fail = (msg) => {
        throw new Error(`parseLooseJSON: ${msg} at offset ${i}`);
    };
    const ws = () => {
        while (i < text.length && " \t\r\n".includes(text[i])) i++;
    };
    const ident = () => {
        const m = /^[A-Za-z_$][A-Za-z0-9_$]*/.exec(text.slice(i));
        if (!m) return null;
        i += m[0].length;
        return m[0];
    };
    function parseString(q) {
        i++; // opening quote
        let out = "";
        while (i < text.length) {
            const ch = text[i];
            if (ch === "\\") {
                const esc = text[i + 1];
                i += 2;
                switch (esc) {
                    case "n": out += "\n"; break;
                    case "t": out += "\t"; break;
                    case "r": out += "\r"; break;
                    case "b": out += "\b"; break;
                    case "f": out += "\f"; break;
                    case "u": {
                        const hex = text.slice(i, i + 4);
                        if (!/^[0-9a-fA-F]{4}$/.test(hex)) fail("bad \\u escape");
                        out += String.fromCharCode(parseInt(hex, 16));
                        i += 4;
                        break;
                    }
                    case "x": {
                        // YouTube hex-escapes the homepage's R payload:
                        // window.ytAtN({'R': '\x7b\x22responseContext\x22…'})
                        // — \x7b is '{', \x22 is '"'. Without this the payload
                        // decodes to literal "x7bx22…" and never parses as the
                        // JSON it is. Verified against the live homepage
                        // 2026-08-24.
                        const hex = text.slice(i, i + 2);
                        if (!/^[0-9a-fA-F]{2}$/.test(hex)) fail("bad \\x escape");
                        out += String.fromCharCode(parseInt(hex, 16));
                        i += 2;
                        break;
                    }
                    default:
                        // \" \' \\ \/ and any other escaped char → the char
                        if (esc === undefined) fail("dangling backslash");
                        out += esc;
                }
                continue;
            }
            i++;
            if (ch === q) return out;
            out += ch;
        }
        fail("unterminated string");
    }
    function parseObject() {
        i++; // {
        const obj = {};
        ws();
        if (text[i] === "}") {
            i++;
            return obj;
        }
        for (;;) {
            ws();
            // Trailing comma before '}' — the live homepage emits one
            // (…\x3d',}), so rejecting it fails the whole extraction.
            if (text[i] === "}") {
                i++;
                return obj;
            }
            let key;
            if (text[i] === '"' || text[i] === "'") {
                key = parseString(text[i]);
            } else {
                key = ident();
                if (key === null) fail("expected object key");
            }
            ws();
            if (text[i] !== ":") fail("expected ':' after key");
            i++;
            // defineProperty, not obj[key] = …: keys come from
            // attacker-reachable page text, and a plain assignment of
            // `__proto__` reaches the prototype SETTER rather than creating
            // an own property. This keeps the result an ordinary object
            // (so callers and tests see normal prototypes) while making
            // every key an inert own data property.
            Object.defineProperty(obj, key, {
                value: parseValue(),
                writable: true,
                enumerable: true,
                configurable: true,
            });
            ws();
            if (text[i] === ",") {
                i++;
                continue;
            }
            if (text[i] === "}") {
                i++;
                return obj;
            }
            fail("expected ',' or '}' in object");
        }
    }
    function parseArray() {
        i++; // [
        const arr = [];
        ws();
        if (text[i] === "]") {
            i++;
            return arr;
        }
        for (;;) {
            ws();
            if (text[i] === "]") {
                i++; // trailing comma before ']' — same tolerance as objects
                return arr;
            }
            arr.push(parseValue());
            ws();
            if (text[i] === ",") {
                i++;
                continue;
            }
            if (text[i] === "]") {
                i++;
                return arr;
            }
            fail("expected ',' or ']' in array");
        }
    }
    function parseValue() {
        ws();
        const ch = text[i];
        if (ch === "{") return parseObject();
        if (ch === "[") return parseArray();
        if (ch === '"' || ch === "'") return parseString(ch);
        const num = /^-?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?/.exec(text.slice(i));
        if (num) {
            i += num[0].length;
            return Number(num[0]);
        }
        const word = ident();
        if (word === "true") return true;
        if (word === "false") return false;
        if (word === "null") return null;
        if (word === "undefined") return undefined;
        if (word !== null) fail(`unexpected identifier '${word}'`);
        fail(`unexpected character '${ch ?? "<eof>"}'`);
    }
    const value = parseValue();
    ws();
    if (i !== text.length) fail("trailing content");
    return value;
}

// extractYtcfg pulls the page's primary ytcfg.set({...}) argument as a plain
// object, or null when no parseable one exists. The blob is server-rendered
// strict JSON, so JSON.parse is authoritative (matching upstream); only the
// object's EXTENT comes from balanced scanning. Call sites where the
// argument is not an object literal (ytcfg.set("KEY", v)) are skipped.
//
// EVERY candidate is tried, and a failure advances to the next one rather
// than abandoning the scan. First-match-then-hard-fail was a denial of
// service: the literal text `ytcfg.set({` inside any string on the page —
// a recommended video's title, say — made this return null, silently
// reverting the whole install to the /att/get path this module exists to
// avoid, with only a Debug-level warning to show for it.
export function extractYtcfg(html) {
    const marker = "ytcfg.set(";
    for (let from = 0; ; ) {
        const at = html.indexOf(marker, from);
        if (at === -1) return null;
        from = at + marker.length;

        let j = at + marker.length;
        while (j < html.length && " \t\r\n".includes(html[j])) j++;
        if (html[j] !== "{") continue;

        const obj = scanBalancedObject(html.slice(j));
        if (obj === null) continue;
        try {
            return JSON.parse(obj);
        } catch {
            continue;
        }
    }
}

// Failure reasons, mirrored from watch_page.go's atn* constants so a log
// line diagnosing a 403 months from now names WHY the homepage pair was
// unavailable instead of a catch-all.
export const REASONS = Object.freeze({
    ok: "ok",
    noCall: "no ytAtN call on page",
    unbalanced: "ytAtN argument never closes",
    outerParse: "outer object does not parse",
    noRKey: "no R key",
    rParse: "R payload is not JSON",
    noChallenge: "no bgChallenge in R payload",
    challengeShape: "bgChallenge is not an object",
    noProgram: "bgChallenge has no program",
    noInterpURL:
        "bgChallenge has no interpreterUrl (inline interpreterJavascript is refused from page-sourced challenges)",
});

const ytAtNOpenRe = /window\.ytAtN\(\s*\{/g;

// Identifier shape for globalName. Excludes __proto__ explicitly: it matches
// the identifier pattern but a write to globalThis["__proto__"] is not an
// ordinary property write.
const safeGlobalNameRe = /^(?!__proto__$)[A-Za-z_$][A-Za-z0-9_$]{0,63}$/;

// extractHomepageChallenge pulls the bgChallenge out of the page's
// window.ytAtN(...) blob and canonicalizes it down to the three fields the
// minter consumes. Returns { challenge, reason } — challenge is null on any
// miss, with reason naming the failure mode (the LAST candidate's reason,
// when several were tried). The interpreter URL's origin is NOT validated
// here; the caller MUST run it through assertGoogleHost before anything is
// fetched — see the trust note below for why that gate, not this parser, is
// the actual security boundary.
//
// EVERY `window.ytAtN(` candidate is tried and a failure advances to the
// next, because taking only the first match let one decoy — the literal
// text in any string on the page — mask the genuine call behind it.
export function extractHomepageChallenge(html) {
    ytAtNOpenRe.lastIndex = 0;
    let lastReason = REASONS.noCall;
    for (let loc; (loc = ytAtNOpenRe.exec(html)) !== null; ) {
        const result = parseAtNCandidate(html, loc.index + loc[0].length - 1);
        if (result.challenge) return result;
        lastReason = result.reason;
    }
    return { challenge: null, reason: lastReason };
}

// parseAtNCandidate parses one `window.ytAtN({` occurrence, given the index
// of its opening brace.
function parseAtNCandidate(html, open) {
    const blob = scanBalancedObject(html.slice(open));
    if (blob === null) return { challenge: null, reason: REASONS.unbalanced };

    let outer;
    try {
        outer = parseLooseJSON(blob);
    } catch {
        return { challenge: null, reason: REASONS.outerParse };
    }
    if (!outer || typeof outer !== "object" || !("R" in outer)) {
        return { challenge: null, reason: REASONS.noRKey };
    }

    // R arrives as a JSON string on watch pages and as an inline object on
    // some homepage renders; accept both. The string form is strict JSON.
    let r = outer.R;
    if (typeof r === "string") {
        try {
            r = JSON.parse(r);
        } catch {
            return { challenge: null, reason: REASONS.rParse };
        }
    }
    if (!r || typeof r !== "object") {
        return { challenge: null, reason: REASONS.rParse };
    }
    const bg = r.bgChallenge;
    if (bg === undefined || bg === null) {
        return { challenge: null, reason: REASONS.noChallenge };
    }
    if (typeof bg !== "object") {
        return { challenge: null, reason: REASONS.challengeShape };
    }

    // Canonicalize: rebuild from exactly the consumed fields (see header).
    const program = bg.program;
    if (typeof program !== "string" || program === "") {
        return { challenge: null, reason: REASONS.noProgram };
    }
    // globalName becomes a globalThis property key (server.js installs the
    // interpreter under it and later deletes the stale one), and this value
    // is attacker-reachable — see the trust note on parseLooseJSON. Restrict
    // it to an identifier shape so it can never be __proto__, a numeric
    // index, or anything else with meaning to a property write.
    const rawGlobalName = typeof bg.globalName === "string" ? bg.globalName : "";
    const globalName = safeGlobalNameRe.test(rawGlobalName) ? rawGlobalName : "";
    const wrapped =
        bg.interpreterUrl && typeof bg.interpreterUrl === "object"
            ? bg.interpreterUrl.privateDoNotAccessOrElseTrustedResourceUrlWrappedValue
            : undefined;
    if (typeof wrapped !== "string" || wrapped === "") {
        return { challenge: null, reason: REASONS.noInterpURL };
    }
    return {
        challenge: {
            program,
            globalName,
            interpreterUrl: {
                privateDoNotAccessOrElseTrustedResourceUrlWrappedValue: wrapped,
            },
        },
        reason: REASONS.ok,
    };
}
