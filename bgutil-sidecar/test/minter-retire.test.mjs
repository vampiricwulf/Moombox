// A replaced BotGuard client is shut down once no mint uses it, and an
// identical interpreter is not run twice.
//
// Run with:  cd bgutil-sidecar && npm test
//
// Importing server.js starts its stdin loop; the resumed stdin handle keeps
// the process alive under `node --test` (see minter-keying.test.mjs).

import { test } from "node:test";
import assert from "node:assert/strict";

process.stdin.resume();

const { mintWith, _retireForTest, shouldRunInterpreter } = await import("../src/server.js");

const ticks = async (n) => {
    for (let i = 0; i < n; i++) await new Promise((r) => setImmediate(r));
};

function makeEntry(mint = async () => "tok", shutdown) {
    const e = {
        calls: 0,
        minter: { mintAsWebsafeString: mint },
        inUse: 0,
        retired: false,
    };
    e.bgClient = {
        shutdown:
            shutdown ??
            (async () => {
                e.calls++;
            }),
    };
    return e;
}

test("a retired idle entry is shut down on the next macrotask, once", async () => {
    const e = makeEntry();
    _retireForTest(e);
    assert.equal(e.calls, 0, "shutdown ran synchronously");
    await ticks(2);
    assert.equal(e.calls, 1);
    _retireForTest(e);
    await ticks(2);
    assert.equal(e.calls, 1, "a client was shut down twice");
});

test("an entry mid-mint is shut down only after the mint returns", async () => {
    let release;
    const gate = new Promise((r) => (release = r));
    const e = makeEntry(async (b) => {
        await gate;
        return `tok-${b}`;
    });
    const p = mintWith(e, "b");
    _retireForTest(e);
    await ticks(3);
    assert.equal(e.calls, 0, "the client was shut down under a running mint");
    release();
    assert.equal(await p, "tok-b");
    await ticks(2);
    assert.equal(e.calls, 1);
});

test("a mint that starts in the same microtask turn as the retire still completes", async () => {
    let shutdownBeforeMint = false;
    const e = makeEntry(async () => {
        await ticks(2);
        if (e.calls > 0) shutdownBeforeMint = true;
        return "tok";
    });
    const p = Promise.resolve(e).then((x) => mintWith(x, "b"));
    _retireForTest(e);
    assert.equal(await p, "tok");
    assert.equal(shutdownBeforeMint, false, "the client was shut down under a running mint");
    await ticks(2);
    assert.equal(e.calls, 1);
});

test("a rejecting shutdown is logged, not thrown", async () => {
    const unhandled = [];
    const onUnhandled = (r) => unhandled.push(r);
    process.on("unhandledRejection", onUnhandled);
    const lines = [];
    const origWrite = process.stderr.write;
    process.stderr.write = (chunk, ...rest) => {
        lines.push(String(chunk));
        return true;
    };
    try {
        const e = makeEntry(undefined, async () => {
            throw new Error("VM operation timed out");
        });
        _retireForTest(e);
        await ticks(4);
    } finally {
        process.stderr.write = origWrite;
        process.off("unhandledRejection", onUnhandled);
    }
    assert.deepEqual(unhandled, []);
    assert.ok(
        lines.some((l) => l.includes("botguard shutdown: VM operation timed out")),
        `no shutdown warning in ${JSON.stringify(lines)}`,
    );
});

test("an entry without a client is ignored", async () => {
    const e = { minter: { mintAsWebsafeString: async () => "tok" }, inUse: 0, retired: false };
    _retireForTest(e);
    assert.equal(await mintWith(e, "b"), "tok");
    await ticks(2);
    assert.equal(e.inUse, 0);
});

test("shouldRunInterpreter runs the interpreter unless the same body already installed its global", () => {
    const g = { vmA: {} };
    const last = { body: "body-1", globalName: "vmA" };
    assert.equal(shouldRunInterpreter(null, "body-1", "vmA", g), true, "no last run");
    assert.equal(shouldRunInterpreter(last, "body-2", "vmA", g), true, "a different body");
    assert.equal(shouldRunInterpreter(last, "body-1", "vmB", { vmA: {}, vmB: {} }), true, "a different global name");
    assert.equal(shouldRunInterpreter(last, "body-1", "vmA", {}), true, "the global is gone");
    assert.equal(shouldRunInterpreter(last, "body-1", "vmA", g), false);
});
