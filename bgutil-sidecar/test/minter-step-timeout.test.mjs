// A BotGuard step whose VM never calls back is bounded, so it cannot wedge
// the generation queue.
//
// Run with:  cd bgutil-sidecar && npm test
//
// Importing server.js starts its stdin loop; the resumed stdin handle keeps
// the process alive under `node --test` (see minter-keying.test.mjs).

import { test } from "node:test";
import assert from "node:assert/strict";

process.stdin.resume();

const {
    getOrCreateMinter,
    invalidateIT,
    mintWith,
    snapshotAndMinter,
    _setStepTimeoutForTest,
    _generationsInFlightForTest,
} = await import("../src/server.js");

const never = () => new Promise(() => {});

// A generation whose snapshot never calls back used to stay pending for good:
// every later generation queued behind it on serializeChain, minterInflight
// never drained (so the parent's recycles, which need generationsInFlight ==
// 0, never ran), and the hung client was never shut down. A mint that never
// called back likewise held inUse above zero. Each step now times out.
//
// Mutants: the snapshot unbounded (the first generation and the one queued
// behind it never settle; the test's own timeout fires); the timed-out step
// not shutting the client down (shutdowns 0); the mint unbounded (mintWith
// never settles); inUse not released on a timed-out mint (inUse 1).
test("a hung BotGuard step does not block the next generation", { timeout: 10_000 }, async () => {
    _setStepTimeoutForTest(50);
    try {
        invalidateIT();
        let shutdowns = 0;
        const hungClient = {
            snapshot: never,
            shutdown: async () => {
                shutdowns++;
            },
        };
        const hung = getOrCreateMinter({ program: "A" }, "key-A", true, () =>
            snapshotAndMinter(hungClient, async () => {
                throw new Error("GenerateIT reached past a hung snapshot");
            }),
        );
        const good = { minter: { mintAsWebsafeString: never }, bgClient: { shutdown: async () => {} } };
        const next = getOrCreateMinter({ program: "B" }, "key-B", true, async () => ({
            ...good,
            expiresAt: Date.now() + 3_600_000,
            webPoSignalOutput: [],
            globalName: "g",
            inUse: 0,
            retired: false,
        }));

        await assert.rejects(hung, /BotGuard snapshot timed out after 50 ms/);
        const { m } = await next;
        assert.equal(m.minter, good.minter, "the queued generation did not run its own factory");
        assert.equal(_generationsInFlightForTest(), 0, "minterInflight did not drain");
        assert.equal(shutdowns, 1, "the hung client was not shut down");

        await assert.rejects(mintWith(m, "binding"), /mint timed out after 50 ms/);
        assert.equal(m.inUse, 0, "a timed-out mint still counts as using the entry");
    } finally {
        _setStepTimeoutForTest();
    }
});

// A cached minter whose mint timed out used to stay cached, so every later
// non-fresh caller waited out the same timeout until the entry expired. The
// timed-out mint now drops it, and the next caller builds a new minter.
//
// Mutant: the catch in mintWith removed (the second getOrCreateMinter returns
// the hung entry and never calls its factory).
test("a mint that timed out drops its minter from the cache", { timeout: 10_000 }, async () => {
    _setStepTimeoutForTest(50);
    try {
        invalidateIT();
        const entry = (mint) => ({
            minter: { mintAsWebsafeString: mint },
            bgClient: { shutdown: async () => {} },
            expiresAt: Date.now() + 3_600_000,
            webPoSignalOutput: [],
            globalName: "g",
            inUse: 0,
            retired: false,
        });
        const hung = entry(never);
        const { m } = await getOrCreateMinter(null, null, false, async () => hung);
        assert.equal(m, hung);

        await assert.rejects(mintWith(m, "binding"), /mint timed out after 50 ms/);

        const fresh = entry(async () => "tok");
        const { m: next } = await getOrCreateMinter(null, null, false, async () => fresh);
        assert.equal(next, fresh, "the timed-out minter was still served from the cache");
        assert.equal(await mintWith(next, "binding"), "tok");
    } finally {
        _setStepTimeoutForTest();
        invalidateIT();
    }
});
