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

// Leaves an EXPIRED minter in the cache, so a non-fresh caller arriving after
// this finds nothing to reuse — the state the next two tests start from.
async function expireCachedMinter() {
    await getOrCreateMinter(null, "key-expire", true, async () => ({
        minter: { mintAsWebsafeString: async () => "old" },
        expiresAt: Date.now() - 1,
        webPoSignalOutput: [],
        globalName: "g",
        minterSource: "att_get",
    }));
}
const ticks = async (n) => {
    for (let i = 0; i < n; i++) await new Promise((r) => setImmediate(r));
};

// A non-fresh caller that found the cache empty queued its own generation
// behind a different key's and, when its turn came, paid for a second
// BotGuard pass instead of taking the minter that generation had just cached.
//
// Mutant: the cache re-check after `await runAfter` removed — two factory calls.
test("a non-fresh caller queued behind another generation reuses what it cached", async () => {
    await expireCachedMinter();
    const { factory, calls, releaseAll } = makeFactory();
    const gvs = getOrCreateMinter({ program: "X" }, "key-X", true, factory);
    await ticks(2);
    const player = getOrCreateMinter(null, null, false, factory);
    await ticks(2);
    releaseAll();
    await ticks(4);
    releaseAll();
    const [a, b] = await Promise.all([gvs, player]);
    assert.equal(calls.length, 1, "the queued caller ran its own BotGuard pass");
    assert.equal(b.m, a.m);
    assert.equal(b.fresh, false, "a reused minter is not reported fresh");
});

// …and a caller that needs a fresh minter never joins a generation that may
// hand back the cached one.
//
// Mutant: the mayReuse guard removed — the fresh caller joins the player's
// generation and gets the GVS pass's minter, reported fresh.
test("a fresh caller does not join a generation that may reuse the cache", async () => {
    await expireCachedMinter();
    const { factory, calls, releaseAll } = makeFactory();
    const gvs = getOrCreateMinter({ program: "X" }, "key-X", true, factory);
    await ticks(2);
    const player = getOrCreateMinter(null, null, false, factory);
    await ticks(2);
    const fresh = getOrCreateMinter(null, null, true, factory);
    await ticks(2);
    for (let i = 0; i < 4; i++) {
        releaseAll();
        await ticks(4);
    }
    const [a, , c] = await Promise.all([gvs, player, fresh]);
    assert.equal(calls.length, 2);
    assert.equal(c.fresh, true);
    assert.notEqual(c.m, a.m, "the fresh caller got the earlier generation's minter");
});
