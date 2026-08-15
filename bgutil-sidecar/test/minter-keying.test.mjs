// Minter keying / dedup tests for the BotGuard sidecar.
//
// Run with:  cd bgutil-sidecar && npm test
// (equivalently: node --test --test-force-exit test/)
//
// --test-force-exit is required: importing server.js starts its stdin loop,
// and the resumed stdin handle below keeps the event loop alive after the
// assertions finish.
//
// These exercise getOrCreateMinter's concurrency contract WITHOUT any network,
// V8 BotGuard run, or subprocess: the function takes an injectable
// minterFactory, so a fake factory that records its calls is enough to pin the
// behaviour that two separate reviews found bugs in.
//
// Importing server.js starts its stdin readline loop; under `node --test` stdin
// is not a TTY and closes immediately, which would exit the process mid-test.
// Keeping a stdin resume handle open for the duration prevents that.

import { test } from "node:test";
import assert from "node:assert/strict";

process.stdin.resume();

const { getOrCreateMinter } = await import("../src/server.js");

// A fake minter whose generation we control, so we can hold generations open
// and interleave callers deterministically.
function makeFactory() {
    const calls = [];
    let resolveNext = [];
    const factory = async (challenge) => {
        calls.push(challenge);
        return new Promise((resolve) => {
            resolveNext.push(() =>
                resolve({
                    minter: { mintAsWebsafeString: async () => "tok" },
                    expiresAt: Date.now() + 3_600_000,
                    webPoSignalOutput: [],
                    globalName: "g",
                    minterSource: challenge ? "challenge" : "att_get",
                }),
            );
        });
    };
    return {
        factory,
        calls,
        releaseAll: () => {
            const pending = resolveNext;
            resolveNext = [];
            pending.forEach((fn) => fn());
        },
    };
}

test("same-challenge callers share one generation", async () => {
    const { factory, calls, releaseAll } = makeFactory();
    const a = getOrCreateMinter({ program: "X" }, "key-X", true, factory);
    const b = getOrCreateMinter({ program: "X" }, "key-X", true, factory);
    await new Promise((r) => setImmediate(r));
    releaseAll();
    const [ra, rb] = await Promise.all([a, b]);

    assert.equal(calls.length, 1, "one BotGuard pass for two same-challenge callers");
    assert.equal(ra.m, rb.m, "both callers get the same minter");
    assert.ok(ra.fresh && rb.fresh);
});

test("different-challenge callers never share a minter", async () => {
    const { factory, calls, releaseAll } = makeFactory();
    const a = getOrCreateMinter({ program: "X" }, "key-X", true, factory);
    const b = getOrCreateMinter({ program: "Y" }, "key-Y", true, factory);
    await new Promise((r) => setImmediate(r));
    releaseAll();
    await new Promise((r) => setImmediate(r));
    releaseAll();
    const [ra, rb] = await Promise.all([a, b]);

    assert.equal(calls.length, 2, "each distinct challenge gets its own pass");
    assert.notEqual(ra.m, rb.m, "a caller must never inherit another session's minter");
});

// The regression the single-slot tracker had: a different-challenge call
// queued between two same-challenge calls evicted the first key, so the third
// caller missed the generation it should have joined.
test("an interleaved different challenge does not break same-challenge joining", async () => {
    const { factory, calls, releaseAll } = makeFactory();
    const a = getOrCreateMinter({ program: "X" }, "key-X", true, factory);
    const b = getOrCreateMinter({ program: "Y" }, "key-Y", true, factory);
    const c = getOrCreateMinter({ program: "X" }, "key-X", true, factory);
    await new Promise((r) => setImmediate(r));
    releaseAll();
    await new Promise((r) => setImmediate(r));
    releaseAll();
    const [ra, , rc] = await Promise.all([a, b, c]);

    assert.equal(calls.length, 2, "X and Y only — C must join A rather than start a third pass");
    assert.equal(ra.m, rc.m, "C joined A's generation");
});
