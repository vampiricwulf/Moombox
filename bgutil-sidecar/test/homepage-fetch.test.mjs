// The homepage fetch's request shape: the consent and language cookies.
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

const { fetchHomepageChallenge } = await import("../src/server.js");

// An EU, EEA or UK address is redirected from the homepage to
// consent.youtube.com unless the request carries SOCS, and the manual
// redirect then falls back to /att/get, the challenge YouTube rejects tokens
// from under the binding experiment. yt-dlp's jar carries SOCS=CAI for every
// session that is not signed in (_initialize_consent, _base.py:675-681) and
// PREF hl=en&tz=UTC (_initialize_pref); this fetch never has a session.
//
// Mutants: the cookie header dropped (the request carries none); SOCS or the
// PREF left out of HOMEPAGE_COOKIE (the exact value differs).
test("the homepage fetch carries SOCS=CAI and an English PREF", async () => {
    const seen = [];
    const realFetch = globalThis.fetch;
    globalThis.fetch = async (url, init) => {
        seen.push({ url: String(url), init });
        return new Response("unavailable", { status: 503 });
    };
    try {
        const pair = await fetchHomepageChallenge(Date.now() + 10_000);
        assert.equal(pair, null, "a 503 homepage yields no pair");
    } finally {
        globalThis.fetch = realFetch;
    }

    assert.equal(seen.length, 1, "one homepage request");
    assert.equal(seen[0].url, "https://www.youtube.com/");
    assert.equal(seen[0].init.redirect, "manual", "a redirect is still not followed");
    assert.equal(seen[0].init.headers.cookie, "PREF=hl=en&tz=UTC; SOCS=CAI");
});
