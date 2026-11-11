// Every response body the sidecar reads is capped: the interpreter script,
// the /att/get challenge and the GenerateIT answer, as the homepage already
// was. The limits are the Go side's own for the same responses (20 MB, 1 MB,
// 1 MB).
//
// Run with:  cd bgutil-sidecar && npm test
//
// Importing server.js starts its stdin loop, so this file keeps stdin open as
// minter-keying.test.mjs does, and needs --test-force-exit for the same
// reason. The fetch is answered by a stand-in for globalThis.fetch: no
// network.

import { test } from "node:test";
import assert from "node:assert/strict";

process.stdin.resume();

const { fetchInterpreterScript, fetchAttChallenge, fetchIntegrityToken } = await import(
    "../src/server.js"
);

const CHUNK = 64 * 1024;

// endlessBody is a response body that never ends by itself: 64 KB chunks
// for as long as the reader keeps pulling. `pulled` counts the bytes handed
// out, so a test can tell a read that stopped at its cap from one that
// buffered until some other limit.
function endlessBody() {
    const state = { pulled: 0, cancelled: false };
    const chunk = new Uint8Array(CHUNK).fill(0x20); // spaces: valid JSON whitespace, valid JS
    const stream = new ReadableStream(
        {
            pull(controller) {
                // A hard stop far past every cap, so an uncapped reader ends
                // the test with a failure and not with an out-of-memory abort.
                if (state.pulled >= 64 << 20) {
                    controller.close();
                    return;
                }
                state.pulled += CHUNK;
                controller.enqueue(chunk);
            },
            cancel() {
                state.cancelled = true;
            },
        },
        { highWaterMark: 0 },
    );
    return { stream, state };
}

async function withFetch(respond, run) {
    const seen = [];
    const realFetch = globalThis.fetch;
    globalThis.fetch = async (url, init) => {
        seen.push({ url: String(url), init });
        return respond();
    };
    try {
        await run();
    } finally {
        globalThis.fetch = realFetch;
    }
    return seen;
}

const deadline = () => Date.now() + 10_000;

const cases = [
    {
        name: "the interpreter script",
        limit: 20 << 20,
        label: "interpreter",
        call: () => fetchInterpreterScript("https://www.google.com/js/bg/x.js", deadline()),
    },
    {
        name: "the /att/get challenge",
        limit: 1 << 20,
        label: "att/get",
        call: () => fetchAttChallenge(deadline()),
    },
    {
        name: "the GenerateIT answer",
        limit: 1 << 20,
        label: "GenerateIT",
        call: () => fetchIntegrityToken("snapshot", deadline()),
    },
];

// Mutants: the body read with resp.text() / resp.json() again (it is
// buffered to the stream's 64 MB end and the call does not reject with the
// cap's message); a cap larger than the Go side's (the message names another
// figure, and more is pulled than the cap allows).
for (const c of cases) {
    test(`${c.name} is refused past ${c.limit} bytes`, async () => {
        const { stream, state } = endlessBody();
        await withFetch(
            () => new Response(stream, { status: 200 }),
            async () => {
                await assert.rejects(c.call(), {
                    message: `${c.label} response exceeded ${c.limit} bytes`,
                });
            },
        );
        assert.ok(
            state.pulled <= c.limit + 2 * CHUNK,
            `read stopped at the cap (pulled ${state.pulled} of a ${c.limit} cap)`,
        );
        assert.ok(state.cancelled, "the rest of the body was abandoned");
    });
}

// A body at exactly the cap is still accepted: the cap is "past", not "at".
test("an interpreter script of exactly 20 MB is read whole", async () => {
    const body = new Uint8Array(20 << 20).fill(0x20);
    let got;
    await withFetch(
        () => new Response(body, { status: 200 }),
        async () => {
            got = await fetchInterpreterScript("https://www.google.com/js/bg/x.js", deadline());
        },
    );
    assert.equal(got.length, 20 << 20);
});

// The capped reads still return what the uncapped ones did.
test("in-limit bodies come back as before", async () => {
    await withFetch(
        () => new Response("var a = 1;", { status: 200 }),
        async () => {
            assert.equal(
                await fetchInterpreterScript("https://www.google.com/js/bg/x.js", deadline()),
                "var a = 1;",
            );
        },
    );

    const challenge = { program: "p", globalName: "g" };
    const attSeen = await withFetch(
        () => new Response(JSON.stringify({ bgChallenge: challenge }), { status: 200 }),
        async () => {
            assert.deepEqual(await fetchAttChallenge(deadline()), challenge);
        },
    );
    assert.equal(attSeen[0].init.redirect, "manual", "att/get still refuses redirects");
    assert.equal(attSeen[0].init.method, "POST");

    const itSeen = await withFetch(
        () => new Response(JSON.stringify(["token", 3600, 300, null]), { status: 200 }),
        async () => {
            assert.deepEqual(await fetchIntegrityToken("snapshot", deadline()), [
                "token",
                3600,
                300,
                null,
            ]);
        },
    );
    assert.equal(itSeen[0].init.redirect, "manual", "GenerateIT still refuses redirects");
    assert.match(itSeen[0].init.body, /"snapshot"\]$/, "the snapshot is posted");
});

// The status errors are worded as they were, and are raised before any body
// is read.
test("non-OK statuses keep their messages", async () => {
    const bad = () => new Response("nope", { status: 503 });
    await withFetch(bad, async () => {
        await assert.rejects(
            fetchInterpreterScript("https://www.google.com/js/bg/x.js", deadline()),
            { message: "interpreter fetch HTTP 503" },
        );
        await assert.rejects(fetchAttChallenge(deadline()), { message: "att/get HTTP 503" });
        await assert.rejects(fetchIntegrityToken("snapshot", deadline()), {
            message: "GenerateIT HTTP 503",
        });
    });
    await withFetch(
        () => new Response("{}", { status: 200 }),
        async () => {
            await assert.rejects(fetchAttChallenge(deadline()), {
                message: "att/get response missing bgChallenge",
            });
        },
    );
});
