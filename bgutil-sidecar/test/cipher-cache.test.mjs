// Cipher answer-cache tests for the BotGuard sidecar (src/cipher.js).
//
// Run with:  cd bgutil-sidecar && npm test
//
// solveCipher's caches are this sidecar's own logic; ejs is upstream's. The
// tests stand a stub in for ejs's main (_setEjsForTest) so they can hand the
// cache the answers a real player gives only when it breaks — the exception
// sentinel above all — with no player JS and no V8 solving. They need
// vendor/ejs.bundle.js on disk all the same (cipher.js imports it, and the
// first test pins it as the production solver): `node build.mjs` writes it.

import { test, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import ejsMain from "../vendor/ejs.bundle.js";
import {
    solveCipher,
    sweepIdle,
    IDLE_EVICT_MS,
    _cacheStats,
    _entryForTest,
    _resetCacheForTest,
    _setEjsForTest,
    _ejsForTest,
} from "../src/cipher.js";
import { _setStoreDirForTest, rawHash, readPrepared, writePrepared } from "../src/player-store.js";

// The prepared-player store is off unless a test points it at a temp dir, so
// MOOMBOX_SIDECAR_PLAYER_DIR in the environment never reaches these tests.
beforeEach(() => {
    _setStoreDirForTest("");
});

afterEach(() => {
    _setEjsForTest(null);
    _resetCacheForTest();
    _setStoreDirForTest(null);
});

function tmpStore() {
    const d = fs.mkdtempSync(path.join(os.tmpdir(), "cipher-cache-"));
    _setStoreDirForTest(d);
    return d;
}

// stubEjs answers every challenge with answer(type, challenge), counting the
// solving calls (a preprocess, the "player" call loadPlayer makes, is not one)
// and recording the type of every call in calls. A preprocess of JS "x"
// yields "PREP(x)"; onPreprocessed(input), when set, may throw or return a
// response of its own for a "preprocessed" call.
function stubEjs(answer) {
    const stub = {
        solves: 0,
        calls: [],
        answer,
        onPreprocessed: null,
        main(input) {
            stub.calls.push(input.type);
            if (input.type === "player") {
                return { type: "result", responses: [], preprocessed_player: `PREP(${input.player})` };
            }
            const own = stub.onPreprocessed?.(input);
            if (own) return own;
            stub.solves++;
            return {
                type: "result",
                responses: input.requests.map((r) => ({
                    type: "result",
                    data: Object.fromEntries(r.challenges.map((c) => [c, stub.answer(r.type, c)])),
                })),
            };
        },
    };
    _setEjsForTest((input) => stub.main(input));
    return stub;
}

function nCacheSize(playerID) {
    return _cacheStats().bindings.find((b) => b.playerID === playerID)?.nCacheSize ?? 0;
}

const CHALLENGE = "abcdefghijk";
const sentinel = (c) => `enhanced_except_AAAAAAAA_${c}`;

test("the production solver is ejs's main", () => {
    assert.equal(_ejsForTest(), ejsMain, "with no stub in, solveCipher must call the vendored ejs bundle");
});

// Audit G14. An n answer that ends with its own challenge is the player's
// exception sentinel; yt-dlp rejects it (jsc/_director.py:265-278). It used
// to be returned and cached in nCache, so every later solve of that challenge
// replayed it until the player was evicted. Now it throws, labelled so the Go
// side's isStaleJSError runs its fresh-JS retry, and nothing is cached: the
// next solve asks ejs again, and its good answer is the one kept.
//
// Mutants: the endsWith check dropped (no throw; the sentinel is cached and
// the second call never reaches ejs); the label changed (the message match
// fails); the check after the caching loop (the sentinel is cached).
test("the exception sentinel throws, is not cached, and is asked again", () => {
    const ejs = stubEjs((_, c) => sentinel(c));

    assert.throws(
        () => solveCipher({ playerID: "p1", playerJS: "js", nChallenges: [CHALLENGE] }),
        { message: /^ejs solve n: no solutions \(exception sentinel\)$/ },
    );
    assert.equal(nCacheSize("p1"), 0, "the sentinel must not be cached");

    ejs.answer = () => "SOLVED";
    const { nResults } = solveCipher({ playerID: "p1", nChallenges: [CHALLENGE] });
    assert.deepEqual(nResults, { [CHALLENGE]: "SOLVED" });
    assert.equal(ejs.solves, 2, "the second solve must reach ejs, not a cached sentinel");
    assert.equal(nCacheSize("p1"), 1, "a real answer is cached");

    solveCipher({ playerID: "p1", nChallenges: [CHALLENGE] });
    assert.equal(ejs.solves, 2, "a cached real answer is served without ejs");
});

// A response with one sentinel among good answers is not trusted at all, as
// yt-dlp's validator rejects the whole response: nothing in it is cached.
//
// Mutant: the check and the caching back in one loop (the good answer ahead
// of the sentinel is cached).
test("one sentinel in a batch caches none of it", () => {
    // A good answer must not end with its challenge, or it is a sentinel too.
    stubEjs((_, c) => (c === "bbbbbbbbbbb" ? sentinel(c) : c.toUpperCase()));
    assert.throws(
        () => solveCipher({ playerID: "p2", playerJS: "js", nChallenges: ["aaaaaaaaaaa", "bbbbbbbbbbb"] }),
        /exception sentinel/,
    );
    assert.equal(nCacheSize("p2"), 0, "no answer from the rejected response may be cached");
});

// The rule is n's alone, as upstream's is (validate_sig_challenge_output
// checks only the shape): a signature answer is a reordering of its input's
// characters and is not held to it.
//
// Mutant: the check applied to every type (this throws).
test("a signature answer is not held to the n rule", () => {
    stubEjs((type, c) => (type === "sig" ? `x${c}` : c.toUpperCase()));
    const { sigResults } = solveCipher({ playerID: "p3", playerJS: "js", sigChallenges: [CHALLENGE] });
    assert.deepEqual(sigResults, { [CHALLENGE]: `x${CHALLENGE}` });
});

// An answer of either type must be a string, as upstream's validators both
// require (jsc/_director.py:265-287). A null used to be cached and replayed,
// and the Go side read it as "" — for a signature, appended as the answer.
// Now it throws, unlabelled as a stale player (upstream refetches nothing),
// and nothing is cached: the next solve asks ejs again.
//
// Mutants: the string check dropped (the n row throws a bare TypeError from
// endsWith instead, the sig row caches and returns the null); the throw
// labelled "no solutions" (the message match fails, and the Go side would
// refetch the player for it).
test("a non-string answer throws and caches nothing", () => {
    for (const type of ["n", "sig"]) {
        _resetCacheForTest();
        const ejs = stubEjs(() => null);
        const req = type === "n" ? { nChallenges: [CHALLENGE] } : { sigChallenges: [CHALLENGE] };
        assert.throws(
            () => solveCipher({ playerID: "p4", playerJS: "js", ...req }),
            { message: `ejs solve ${type}: non-string result for challenge` },
            `a null ${type} answer must throw`,
        );
        ejs.answer = () => "SOLVED";
        const res = solveCipher({ playerID: "p4", ...req });
        assert.deepEqual(type === "n" ? res.nResults : res.sigResults, { [CHALLENGE]: "SOLVED" });
        assert.equal(ejs.solves, 2, `the second ${type} solve must reach ejs, not a cached null`);
    }
});

// ---- Prepared players: flat copy, disk load order, self-heal, idle eviction.

const FIXTURE = new URL("../../internal/cipher/testdata/player_cb017549.js", import.meta.url);
const BUNDLE = new URL("../vendor/ejs.bundle.js", import.meta.url);

// The ejs result is a rope of the player's pieces; held as is, every later
// solve flattens a copy of it on V8's heap. The entry must hold a flat string.
//
// A copy read back from the store is a slice of the file's text: flat already,
// but under the pinned Node (24) that text is a string on V8's heap, two bytes
// a character, and the slice holds all of it. The reload gets the same copy,
// which Node holds outside the heap, so a reloaded player adds next to nothing
// to heapUsed. (Node 26 decodes a file this size off the heap by itself, so
// there the reload half passes either way.)
//
// Mutants: the Buffer round trip dropped after a preprocess (%StringIsFlat is
// false); dropped on the disk path (heapUsed grows two bytes a character under
// Node 24).
test("an entry is stored flat, and off the heap when reloaded from disk", (t) => {
    if (!fs.existsSync(BUNDLE) || !fs.existsSync(FIXTURE)) {
        t.skip("ejs bundle or player_cb017549.js fixture absent");
        return;
    }
    const cipherURL = new URL("../src/cipher.js", import.meta.url).href;
    const storeURL = new URL("../src/player-store.js", import.meta.url).href;
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "cipher-flat-"));
    const head = [
        `import fs from "node:fs";`,
        `import { solveCipher, _entryForTest } from ${JSON.stringify(cipherURL)};`,
        `import { _setStoreDirForTest } from ${JSON.stringify(storeURL)};`,
        `_setStoreDirForTest(${JSON.stringify(dir)});`,
    ];
    const run = (lines) =>
        JSON.parse(
            execFileSync(
                process.execPath,
                ["--allow-natives-syntax", "--expose-gc", "--input-type=module", "-e", [...head, ...lines].join("\n")],
                { encoding: "utf8" },
            ),
        );
    // One process preprocesses the player and writes the store.
    const fresh = run([
        `solveCipher({ playerID: "cb017549", playerJS: fs.readFileSync(new URL(${JSON.stringify(FIXTURE.href)}), "utf8") });`,
        `process.stdout.write(JSON.stringify({ flat: %StringIsFlat(_entryForTest("cb017549").preprocessed) }));`,
    ]);
    assert.equal(fresh.flat, true, "the preprocessed copy is not flat");
    // A second one reloads it. The header match leaves the file's text in the
    // regexp engine's last-match state; another match lets go of it, as the
    // next solve would.
    const reloaded = run([
        `gc(); gc();`,
        `const before = process.memoryUsage().heapUsed;`,
        `solveCipher({ playerID: "cb017549" });`,
        `const entry = _entryForTest("cb017549");`,
        `/x/.exec("x");`,
        `gc(); gc();`,
        `const grown = process.memoryUsage().heapUsed - before;`,
        `process.stdout.write(JSON.stringify({ fromDisk: entry.fromDisk, heapPerChar: grown / entry.preprocessed.length }));`,
    ]);
    assert.equal(reloaded.fromDisk, true, "the second process did not load from the store");
    assert.ok(
        reloaded.heapPerChar < 1.0, // an uncopied reload measures about 2.0; the slack is for other platforms
        `the reloaded copy is on V8's heap (heapUsed grew ${reloaded.heapPerChar} bytes a character)`,
    );
});

// Mutant: the disk read skipped (a second "player" call).
test("a second process-life reloads from disk without preprocessing", () => {
    tmpStore();
    const stub = stubEjs((_, c) => c.toUpperCase());
    const first = solveCipher({ playerID: "p1", playerJS: "js", nChallenges: [CHALLENGE] });
    _resetCacheForTest();
    stub.calls.length = 0;
    const second = solveCipher({ playerID: "p1", nChallenges: [CHALLENGE] });
    assert.deepEqual(second, first);
    assert.ok(!stub.calls.includes("player"), `no preprocess the second time, got ${stub.calls}`);
});

// Mutant: the hash compare dropped (the old copy answers, no "player" call).
test("attached JS with a different hash replaces the disk copy", () => {
    tmpStore();
    const stub = stubEjs((_, c) => c.toUpperCase());
    solveCipher({ playerID: "p1", playerJS: "js-A", nChallenges: [CHALLENGE] });
    _resetCacheForTest();
    stub.calls.length = 0;
    solveCipher({ playerID: "p1", playerJS: "js-B", nChallenges: [CHALLENGE] });
    assert.equal(stub.calls.filter((c) => c === "player").length, 1);
    assert.equal(readPrepared("p1").rawHash, rawHash("js-B"));
    assert.equal(readPrepared("p1").source, "PREP(js-B)");
});

// The disk copy here claims the very JS that comes attached, so only
// forceReload's own removal keeps it from answering.
//
// Mutant: forceReload drops only the RAM entry (the stale copy is read back,
// matches the hash, answers, and is kept).
test("forceReload removes the disk copy", () => {
    tmpStore();
    const stub = stubEjs((_, c) => c.toUpperCase());
    solveCipher({ playerID: "p1", playerJS: "js-A", nChallenges: [CHALLENGE] }); // RAM entry
    assert.equal(writePrepared("p1", rawHash("js-B"), "STALE"), true);
    stub.calls.length = 0;
    solveCipher({ playerID: "p1", playerJS: "js-B", forceReload: true, nChallenges: [CHALLENGE] });
    assert.equal(stub.calls.filter((c) => c === "player").length, 1);
    assert.equal(readPrepared("p1").rawHash, rawHash("js-B"));
    assert.equal(readPrepared("p1").source, "PREP(js-B)");
});

// captureWarn collects what fn writes to stderr.
function captureWarn(fn) {
    const lines = [];
    const orig = process.stderr.write;
    process.stderr.write = (s) => {
        lines.push(String(s));
        return true;
    };
    try {
        return { result: fn(), lines };
    } catch (e) {
        return { error: e, lines };
    } finally {
        process.stderr.write = orig;
    }
}

const HEAL_WARN = "[bgutil-sidecar:warn] cipher: discarded a bad prepared copy of p1\n";

// A copy whose hash matches the attached JS but whose body ejs cannot run.
function writeBadCopy() {
    assert.equal(writePrepared("p1", rawHash("js"), "BAD"), true);
}
function throwOnBad(input) {
    if (input.preprocessed_player === "BAD") throw new Error("unexpected structure");
    return null;
}

// Mutant: no self-heal (the throw surfaces as "ejs solve n: ...").
test("a bad prepared copy heals with JS attached", () => {
    tmpStore();
    writeBadCopy();
    const stub = stubEjs((_, c) => c.toUpperCase());
    stub.onPreprocessed = throwOnBad;
    const { result, error, lines } = captureWarn(() =>
        solveCipher({ playerID: "p1", playerJS: "js", nChallenges: [CHALLENGE] }),
    );
    assert.equal(error, undefined);
    assert.deepEqual(result.nResults, { [CHALLENGE]: CHALLENGE.toUpperCase() });
    assert.equal(readPrepared("p1").source, "PREP(js)", "the file is rewritten from the attached JS");
    assert.deepEqual(lines, [HEAL_WARN]);
});

// Mutant: the file kept on a failed heal (it exists afterwards).
test("a bad prepared copy without JS answers PLAYER_NOT_LOADED", () => {
    const dir = tmpStore();
    writeBadCopy();
    const stub = stubEjs((_, c) => c.toUpperCase());
    stub.onPreprocessed = throwOnBad;
    const { error, lines } = captureWarn(() => solveCipher({ playerID: "p1", nChallenges: [CHALLENGE] }));
    assert.equal(error?.code, "PLAYER_NOT_LOADED");
    assert.equal(error.message, "player not loaded");
    assert.deepEqual(fs.readdirSync(dir), [], "the bad file is gone");
    assert.deepEqual(lines, [HEAL_WARN]);
});

// ejs returns, not throws, "Failed to extract sig function".
//
// Mutant: healing only on a thrown ejs error.
test("a returned response error also heals", () => {
    tmpStore();
    writeBadCopy();
    const stub = stubEjs((_, c) => `x${c}`);
    stub.onPreprocessed = (input) =>
        input.preprocessed_player === "BAD"
            ? { type: "result", responses: [{ type: "error", error: "Failed to extract sig function" }] }
            : null;
    const { result, error, lines } = captureWarn(() =>
        solveCipher({ playerID: "p1", playerJS: "js", sigChallenges: [CHALLENGE] }),
    );
    assert.equal(error, undefined);
    assert.deepEqual(result.sigResults, { [CHALLENGE]: `x${CHALLENGE}` });
    assert.equal(readPrepared("p1").source, "PREP(js)");
    assert.deepEqual(lines, [HEAL_WARN]);
});

// Once a disk copy has answered, its later failures are the player's, and the
// Go side's stale-JS path owns them.
//
// Mutant: proven never set (the sentinel is healed: a "player" call, a warn).
test("a proven entry's later failure is not healed", () => {
    const dir = tmpStore();
    const stub = stubEjs((_, c) => c.toUpperCase());
    solveCipher({ playerID: "p1", playerJS: "js", nChallenges: [CHALLENGE] });
    _resetCacheForTest();
    solveCipher({ playerID: "p1", playerJS: "js", nChallenges: [CHALLENGE] }); // from disk, proven
    stub.answer = (_, c) => sentinel(c);
    stub.calls.length = 0;
    const { error, lines } = captureWarn(() =>
        solveCipher({ playerID: "p1", playerJS: "js", nChallenges: ["bbbbbbbbbbb"] }),
    );
    assert.equal(error?.message, "ejs solve n: no solutions (exception sentinel)");
    assert.ok(!stub.calls.includes("player"));
    assert.deepEqual(lines, []);
    assert.equal(fs.readdirSync(dir).length, 1, "the file is kept");
});

// Mutant: the comparison inverted, or every entry dropped.
test("sweepIdle drops only idle entries", () => {
    tmpStore();
    const stub = stubEjs((_, c) => c.toUpperCase());
    solveCipher({ playerID: "p1", playerJS: "js1", nChallenges: [CHALLENGE] });
    solveCipher({ playerID: "p2", playerJS: "js2", nChallenges: [CHALLENGE] });
    const t0 = Date.now();
    _entryForTest("p1").lastUsed = t0;
    _entryForTest("p2").lastUsed = t0 + 14 * 60 * 1000;
    assert.equal(sweepIdle(t0 + 16 * 60 * 1000), 1);
    assert.equal(_entryForTest("p1"), undefined);
    assert.notEqual(_entryForTest("p2"), undefined);
    stub.calls.length = 0;
    const { nResults } = solveCipher({ playerID: "p1", nChallenges: [CHALLENGE] });
    assert.deepEqual(nResults, { [CHALLENGE]: CHALLENGE.toUpperCase() });
    assert.ok(!stub.calls.includes("player"));
});

// The Go side sends an evicted player's solves without its JS, one after the
// other: the first reloads the prepared copy, the second finds it in RAM.
//
// Mutants: the reloaded entry not put back in the cache (the second solve
// reads the file again, and a store that lost the file answers
// PLAYER_NOT_LOADED); the reload cached under another key (two RAM entries).
test("two solves in a row for an evicted player both answer from the one reloaded entry", () => {
    const dir = tmpStore();
    const stub = stubEjs((_, c) => c.toUpperCase());
    solveCipher({ playerID: "p1", playerJS: "js", nChallenges: [CHALLENGE] });
    assert.equal(sweepIdle(Date.now() + IDLE_EVICT_MS + 1), 1);
    stub.calls.length = 0;

    const first = solveCipher({ playerID: "p1", nChallenges: ["aaaaaaaaaaa"] });
    assert.deepEqual(first.nResults, { aaaaaaaaaaa: "AAAAAAAAAAA" });
    const reloaded = _entryForTest("p1");
    for (const name of fs.readdirSync(dir)) fs.rmSync(path.join(dir, name));
    const second = solveCipher({ playerID: "p1", nChallenges: ["bbbbbbbbbbb"] });
    assert.deepEqual(second.nResults, { bbbbbbbbbbb: "BBBBBBBBBBB" });

    assert.ok(!stub.calls.includes("player"), `no preprocess for an evicted player, got ${stub.calls}`);
    assert.equal(_entryForTest("p1"), reloaded, "the second solve did not use the reloaded entry");
    assert.equal(_cacheStats().players, 1);
});

// The idle window is exclusive: an entry used exactly IDLE_EVICT_MS ago stays.
//
// Mutant: > written as >= (the entry at the boundary is dropped).
test("an entry idle for exactly the window is kept, one millisecond longer is dropped", () => {
    stubEjs((_, c) => c.toUpperCase());
    solveCipher({ playerID: "p1", playerJS: "js", nChallenges: [CHALLENGE] });
    const t0 = _entryForTest("p1").lastUsed;
    assert.equal(sweepIdle(t0 + IDLE_EVICT_MS), 0);
    assert.notEqual(_entryForTest("p1"), undefined);
    assert.equal(sweepIdle(t0 + IDLE_EVICT_MS + 1), 1);
    assert.equal(_entryForTest("p1"), undefined);
});

// A store that cannot be written (its directory is a regular file) costs the
// disk copy, never the solve: the player answers, and goes on answering from
// RAM. What the store logs about it differs by platform and is not pinned.
//
// Mutant: a failed write thrown out of solveCipher, or the entry dropped with it.
test("a store that cannot be written does not stop solving", () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "cipher-cache-"));
    const file = path.join(dir, "regular");
    fs.writeFileSync(file, "x");
    _setStoreDirForTest(file);
    const stub = stubEjs((_, c) => c.toUpperCase());
    const first = captureWarn(() => solveCipher({ playerID: "p1", playerJS: "js", nChallenges: ["aaaaaaaaaaa"] }));
    assert.equal(first.error, undefined);
    assert.deepEqual(first.result.nResults, { aaaaaaaaaaa: "AAAAAAAAAAA" });
    stub.calls.length = 0;
    const second = captureWarn(() => solveCipher({ playerID: "p1", nChallenges: ["bbbbbbbbbbb"] }));
    assert.equal(second.error, undefined);
    assert.deepEqual(second.result.nResults, { bbbbbbbbbbb: "BBBBBBBBBBB" });
    assert.ok(!stub.calls.includes("player"), "the second solve preprocessed again instead of using RAM");
    assert.equal(fs.readFileSync(file, "utf8"), "x");
});

// A player ID that is not a plain token never reaches the store, and solves
// like any other from RAM.
//
// Mutant: solveCipher refusing a player the store refuses.
test("a player ID the store refuses still solves, from RAM only", () => {
    // The store is a directory inside the temp one, so "../x" would land in a
    // directory this test owns.
    const parent = tmpStore();
    const store = path.join(parent, "store");
    fs.mkdirSync(store);
    _setStoreDirForTest(store);
    const stub = stubEjs((_, c) => c.toUpperCase());
    const first = solveCipher({ playerID: "../x", playerJS: "js", nChallenges: ["aaaaaaaaaaa"] });
    assert.deepEqual(first.nResults, { aaaaaaaaaaa: "AAAAAAAAAAA" });
    stub.calls.length = 0;
    const second = solveCipher({ playerID: "../x", nChallenges: ["bbbbbbbbbbb"] });
    assert.deepEqual(second.nResults, { bbbbbbbbbbb: "BBBBBBBBBBB" });
    assert.ok(!stub.calls.includes("player"));
    assert.deepEqual(fs.readdirSync(store), [], "the store holds a file for an unsafe player ID");
    assert.deepEqual(fs.readdirSync(parent), ["store"], "a file was written beside the store");
});

// Mutant: an evicted player answered from anywhere but RAM, disk or attached JS.
test("with the store disabled behaviour is today's", () => {
    stubEjs((_, c) => c.toUpperCase());
    solveCipher({ playerID: "p1", playerJS: "js", nChallenges: [CHALLENGE] });
    assert.equal(sweepIdle(Date.now() + IDLE_EVICT_MS + 1), 1);
    assert.throws(() => solveCipher({ playerID: "p1", nChallenges: [CHALLENGE] }), { code: "PLAYER_NOT_LOADED" });
});
