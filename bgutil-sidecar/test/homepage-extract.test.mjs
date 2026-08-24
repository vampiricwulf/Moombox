// Extraction tests for the homepage (ytcfg, ytAtN) pair — pure functions,
// no network, no globals.
//
// Run with:  cd bgutil-sidecar && npm test
//
// homepage.js has no import-time side effects, so unlike minter-keying these
// need no stdin dance. Fixtures mirror internal/youtube/watch_page_test.go:
// the same failure modes must fail the same way on both sides of the RPC.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
    scanBalancedObject,
    parseLooseJSON,
    extractYtcfg,
    extractHomepageChallenge,
    REASONS,
} from "../src/homepage.js";

const GOOD_CHALLENGE = {
    program: "PROG})with-brace-trap",
    globalName: "trayride",
    interpreterUrl: {
        privateDoNotAccessOrElseTrustedResourceUrlWrappedValue:
            "//www.google.com/js/th/abc123.js",
    },
};

function pageWith(atnBlob, ytcfgJSON) {
    const cfg = ytcfgJSON ?? '{"EVENT_ID":"evt-1","INNERTUBE_CONTEXT":{}}';
    return (
        "<html><head><script>ytcfg.set(" +
        cfg +
        ");</script></head><body><script>window.ytAtN(" +
        atnBlob +
        ");</script></body></html>"
    );
}

// --- scanBalancedObject -----------------------------------------------------

test("scanBalancedObject handles braces inside strings", () => {
    const s = '{a: "close}}", b: {c: 1}}tail';
    assert.equal(scanBalancedObject(s), '{a: "close}}", b: {c: 1}}');
});

test("scanBalancedObject handles escaped quotes inside strings", () => {
    const s = '{a: "he said \\"}\\" ok", b: 2}rest';
    assert.equal(scanBalancedObject(s), '{a: "he said \\"}\\" ok", b: 2}');
});

test("scanBalancedObject returns null for unbalanced input", () => {
    assert.equal(scanBalancedObject("{a: {b: 1}"), null);
    assert.equal(scanBalancedObject("no brace"), null);
});

// --- parseLooseJSON ---------------------------------------------------------

test("parseLooseJSON parses unquoted keys and single quotes", () => {
    const v = parseLooseJSON("{R: 'x', num: -1.5e3, t: true, n: null, arr: [1, 'a']}");
    assert.deepEqual(v, { R: "x", num: -1500, t: true, n: null, arr: [1, "a"] });
});

test("parseLooseJSON rejects backslash-escaped quote smuggling", () => {
    // Text that LOOKS like an object but arrived inside a JSON string
    // elsewhere on the page carries escaped quotes; a backslash outside a
    // string is a parse error, never a challenge.
    assert.throws(() => parseLooseJSON('{R: \\"fake\\"}'));
});

test("parseLooseJSON rejects identifiers and calls as values", () => {
    assert.throws(() => parseLooseJSON("{R: alert(1)}"));
    assert.throws(() => parseLooseJSON("{R: window}"));
});

test("parseLooseJSON decodes escapes inside strings", () => {
    const v = parseLooseJSON('{s: "a\u0041\n\\"q\\""}');
    assert.deepEqual(v, { s: 'aA\n"q"' });
});

// --- extractYtcfg -----------------------------------------------------------

test("extractYtcfg returns the parsed primary config", () => {
    const html = pageWith("{R: {}}");
    assert.deepEqual(extractYtcfg(html), {
        EVENT_ID: "evt-1",
        INNERTUBE_CONTEXT: {},
    });
});

test("extractYtcfg survives a close-paren trap inside a string value", () => {
    // Upstream's non-greedy regex form truncates on this.
    const html = pageWith("{R: {}}", '{"EVENT_ID":"evt-2","TRAP":"x});y"}');
    assert.deepEqual(extractYtcfg(html), { EVENT_ID: "evt-2", TRAP: "x});y" });
});

test("extractYtcfg skips two-arg ytcfg.set calls", () => {
    const html =
        '<script>ytcfg.set("KEY", 1);</script><script>ytcfg.set({"EVENT_ID":"evt-3"});</script>';
    assert.deepEqual(extractYtcfg(html), { EVENT_ID: "evt-3" });
});

test("extractYtcfg returns null when absent or unparseable", () => {
    assert.equal(extractYtcfg("<html>nothing</html>"), null);
    assert.equal(extractYtcfg("<script>ytcfg.set({broken: );</script>"), null);
});

// --- extractHomepageChallenge -----------------------------------------------

test("challenge extracted from R as inline object", () => {
    const blob = "{R: " + JSON.stringify({ bgChallenge: GOOD_CHALLENGE }) + ", other: 1}";
    const { challenge, reason } = extractHomepageChallenge(pageWith(blob));
    assert.equal(reason, REASONS.ok);
    assert.deepEqual(challenge, GOOD_CHALLENGE);
});

test("challenge extracted from R as JSON string (watch-page form)", () => {
    const rString = JSON.stringify(JSON.stringify({ bgChallenge: GOOD_CHALLENGE }));
    const blob = "{R: " + rString + "}";
    const { challenge, reason } = extractHomepageChallenge(pageWith(blob));
    assert.equal(reason, REASONS.ok);
    assert.deepEqual(challenge, GOOD_CHALLENGE);
});

test("brace trap inside the program payload does not truncate", () => {
    // GOOD_CHALLENGE.program contains "})" — the balanced scan must ride
    // over it where a non-greedy regex match would stop short.
    const blob = "{R: " + JSON.stringify({ bgChallenge: GOOD_CHALLENGE }) + "}";
    const { challenge } = extractHomepageChallenge(pageWith(blob));
    assert.equal(challenge.program, GOOD_CHALLENGE.program);
});

test("canonicalization strips extra fields including inline interpreter JS", () => {
    const dirty = {
        ...GOOD_CHALLENGE,
        interpreterJavascript: {
            privateDoNotAccessOrElseSafeScriptWrappedValue: "alert(1)",
        },
        extraneous: "x",
    };
    const blob = "{R: " + JSON.stringify({ bgChallenge: dirty }) + "}";
    const { challenge, reason } = extractHomepageChallenge(pageWith(blob));
    assert.equal(reason, REASONS.ok);
    assert.deepEqual(challenge, GOOD_CHALLENGE); // only the 3 consumed fields
    assert.equal("interpreterJavascript" in challenge, false);
});

test("inline-only challenge (no interpreterUrl) is refused", () => {
    const inlineOnly = {
        program: "P",
        globalName: "g",
        interpreterJavascript: {
            privateDoNotAccessOrElseSafeScriptWrappedValue: "alert(1)",
        },
    };
    const blob = "{R: " + JSON.stringify({ bgChallenge: inlineOnly }) + "}";
    const { challenge, reason } = extractHomepageChallenge(pageWith(blob));
    assert.equal(challenge, null);
    assert.equal(reason, REASONS.noInterpURL);
});

test("SINGLE-QUOTED attacker payload DOES parse — parser is not the boundary", () => {
    // Regression for a false security claim: the header comment and the
    // double-quoted test below once implied parseLooseJSON stopped attacker
    // text. It does not. JSON escaping touches none of ' { } : or bare
    // identifiers, so this survives verbatim inside a JSON string value.
    // Pinning the TRUE behaviour keeps anyone from re-deriving the wrong
    // guarantee — containment is assertGoogleHost + canonicalization.
    const attack =
        "window.ytAtN({R:{bgChallenge:{program:'ATTACKER',globalName:'pwn'," +
        "interpreterUrl:{privateDoNotAccessOrElseTrustedResourceUrlWrappedValue:" +
        "'//www.gstatic.com/attacker/path.js'}}}})";
    const page =
        '<script>var ytInitialData = {"t":' + JSON.stringify(attack) + "};</script>";
    const { challenge } = extractHomepageChallenge(page);
    assert.notEqual(challenge, null, "parser does NOT stop this — do not assert otherwise");
    assert.equal(challenge.program, "ATTACKER");
    // What must hold: canonicalization still strips everything else, so the
    // only attacker levers are the three fields the host gate then judges.
    assert.deepEqual(Object.keys(challenge).sort(), [
        "globalName",
        "interpreterUrl",
        "program",
    ]);
});

test("a decoy ytAtN does not mask the genuine call behind it", () => {
    const real =
        "<script>window.ytAtN({'R': " +
        JSON.stringify(JSON.stringify({ bgChallenge: GOOD_CHALLENGE })) +
        "});</script>";
    for (const decoy of [
        '<script>var d="window.ytAtN({Q:1})";</script>',
        '<script>var d="window.ytAtN({";</script>',
        "<script>window.ytAtN({R: window});</script>",
    ]) {
        const { challenge, reason } = extractHomepageChallenge(decoy + real);
        assert.equal(reason, REASONS.ok, decoy);
        assert.deepEqual(challenge, GOOD_CHALLENGE, decoy);
    }
});

test("a decoy ytcfg.set does not mask the genuine config behind it", () => {
    const real = '<script>ytcfg.set({"EVENT_ID":"real"});</script>';
    for (const decoy of [
        '<script>var d={"t":"ytcfg.set({not:json}) haha"};</script>',
        '<script>var d="ytcfg.set({";</script>',
        '<script>ytcfg.set("KEY", 1);</script>',
    ]) {
        assert.deepEqual(extractYtcfg(decoy + real), { EVENT_ID: "real" }, decoy);
    }
});

test("globalName is restricted to an identifier shape", () => {
    const mk = (name) => {
        const c = { ...GOOD_CHALLENGE, globalName: name };
        return extractHomepageChallenge(
            "<script>window.ytAtN({R: " + JSON.stringify({ bgChallenge: c }) + "});</script>",
        ).challenge;
    };
    assert.equal(mk("trayride").globalName, "trayride");
    // Rejected shapes fall back to "" rather than becoming a globalThis key.
    for (const bad of ["__proto__", "a-b", "0abc", "with space", "x".repeat(70)]) {
        assert.equal(mk(bad).globalName, "", bad);
    }
});

test("a __proto__ key cannot pollute Object.prototype", () => {
    const v = parseLooseJSON('{"__proto__": {"polluted": true}}');
    assert.equal({}.polluted, undefined, "Object.prototype was polluted");
    // __proto__ became an inert own property, not a prototype swap.
    assert.equal(Object.getPrototypeOf(v), Object.prototype);
    assert.deepEqual(Object.getOwnPropertyNames(v), ["__proto__"]);
});

test("hostile ytAtN-lookalike smuggled in a JSON string fails to parse", () => {
    // A video title rendered into a JSON string can contain the literal text
    // window.ytAtN({R: ...}) — but its quotes arrive backslash-escaped, and a
    // backslash outside a string is a parseLooseJSON error. Mirrors the Go
    // hostile-metadata test. Built by escaping a real payload into a JSON
    // string exactly the way a page renderer would.
    const payload =
        "window.ytAtN({R: " +
        JSON.stringify(JSON.stringify({ bgChallenge: {
            program: "P",
            interpreterUrl: {
                privateDoNotAccessOrElseTrustedResourceUrlWrappedValue: "//evil.tld/x.js",
            },
        } })) +
        "})";
    const hostile = '<script>var ytInitialData = {"title":' + JSON.stringify(payload) + "};</script>";
    const { challenge } = extractHomepageChallenge(hostile);
    assert.equal(challenge, null);
});

test("failure-mode reasons are distinct", () => {
    const cases = [
        ["<html>no call</html>", REASONS.noCall],
        ["<script>window.ytAtN({R: );</script>", REASONS.unbalanced],
        // An identifier value (not true/false/null/undefined) is never valid
        // data — parseLooseJSON throws rather than evaluating it.
        ["<script>window.ytAtN({R: window});</script>", REASONS.outerParse],
        ["<script>window.ytAtN({Q: 1});</script>", REASONS.noRKey],
        ['<script>window.ytAtN({R: "not json"});</script>', REASONS.rParse],
        ['<script>window.ytAtN({R: "{\\"noChallenge\\":1}"});</script>', REASONS.noChallenge],
        ['<script>window.ytAtN({R: {bgChallenge: "str"}});</script>', REASONS.challengeShape],
        ['<script>window.ytAtN({R: {bgChallenge: {globalName: "g"}}});</script>', REASONS.noProgram],
    ];
    for (const [html, want] of cases) {
        const { challenge, reason } = extractHomepageChallenge(html);
        assert.equal(challenge, null, html);
        assert.equal(reason, want, html);
    }
});

// --- live-shape regressions -------------------------------------------------
// Both fixtures below are the exact shapes the real homepage emits; each one
// broke extraction when first run against live YouTube (2026-08-24).

test("parseLooseJSON decodes \\xNN hex escapes", () => {
    // window.ytAtN({'R': '\x7b\x22a\x22:1\x7d'}) — the live payload form.
    const v = parseLooseJSON("{'R': '\x7b\x22a\x22:1\x7d'}");
    assert.deepEqual(v, { R: '{"a":1}' });
    assert.deepEqual(JSON.parse(v.R), { a: 1 });
});

test("parseLooseJSON rejects a malformed \\x escape", () => {
    assert.throws(() => parseLooseJSON("{'R': '\\xZZ'}"));
});

test("parseLooseJSON tolerates trailing commas", () => {
    assert.deepEqual(parseLooseJSON("{a: 1,}"), { a: 1 });
    assert.deepEqual(parseLooseJSON("{a: [1, 2,],}"), { a: [1, 2] });
});

test("live homepage shape: hex-escaped R with trailing comma yields a challenge", () => {
    const inner = JSON.stringify({ bgChallenge: GOOD_CHALLENGE });
    // Hex-escape every char the live page escapes: { } [ ] " 
    const hexed = inner.replace(/[{}[\]"]/g, (c) =>
        "\\x" + c.charCodeAt(0).toString(16).padStart(2, "0"),
    );
    const html = "<script>window.ytAtN({'R': '" + hexed + "',});</script>";
    const { challenge, reason } = extractHomepageChallenge(html);
    assert.equal(reason, REASONS.ok);
    assert.deepEqual(challenge, GOOD_CHALLENGE);
});
