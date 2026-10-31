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

import { test, afterEach } from "node:test";
import assert from "node:assert/strict";

import ejsMain from "../vendor/ejs.bundle.js";
import {
    solveCipher,
    _cacheStats,
    _resetCacheForTest,
    _setEjsForTest,
    _ejsForTest,
} from "../src/cipher.js";

afterEach(() => {
    _setEjsForTest(null);
    _resetCacheForTest();
});

// stubEjs answers every challenge with answer(type, challenge), counting the
// solving calls (a preprocess, the "player" call loadPlayer makes, is not one).
function stubEjs(answer) {
    const stub = {
        solves: 0,
        answer,
        main(input) {
            if (input.type === "player") {
                return { type: "result", responses: [], preprocessed_player: "PREPROCESSED" };
            }
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
