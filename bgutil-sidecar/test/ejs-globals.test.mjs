// ejs / BotGuard realm-sharing tests for the BotGuard sidecar (audit G23).
//
// Run with:  cd bgutil-sidecar && npm test
//
// ejs's setupNodes, which every preprocessed player runs first, overwrites
// globalThis.location with a URL for watch?v=yt-dlp-wins and stubs
// XMLHttpRequest, document, navigator, self and window where they are absent
// (vendor/ejs/src/yt/solver/setup.ts). This sidecar runs ejs in the realm
// BotGuard snapshots, so cipher.js restores those globals after every ejs
// call, and server.js re-installs the JSDOM page before every
// BotGuardClient.create.
//
// Importing server.js runs its JSDOM bootstrap, which is the realm under test,
// and starts its stdin loop: the resumed stdin keeps the process alive, and
// npm test's --test-force-exit ends it (see minter-keying.test.mjs). The ejs
// bundle must be on disk: `node build.mjs` writes it.

import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import ejsMain from "../vendor/ejs.bundle.js";
import { solveCipher, _resetCacheForTest, _setEjsForTest } from "../src/cipher.js";

process.stdin.resume();
const { installDomGlobals } = await import("../src/server.js");

afterEach(() => {
    _setEjsForTest(null);
    _resetCacheForTest();
    installDomGlobals();
});

const NAMES = ["location", "XMLHttpRequest", "self", "document", "navigator", "window"];

function snapshot() {
    return Object.fromEntries(NAMES.map((n) => [n, Object.getOwnPropertyDescriptor(globalThis, n)]));
}

function assertSameGlobals(before, context) {
    for (const n of NAMES) {
        assert.deepEqual(
            Object.getOwnPropertyDescriptor(globalThis, n),
            before[n],
            `globalThis.${n} changed by ${context}`,
        );
    }
}

function assertJsdomLocation() {
    assert.equal(Object.prototype.toString.call(globalThis.location), "[object Location]");
    assert.equal(globalThis.location, globalThis.document.location, "globalThis.location must be the JSDOM page's own");
    assert.equal(globalThis.location.href, "https://www.youtube.com/");
}

// ejs's own setupNodes, read from the vendored source rather than copied, so
// the test follows an ejs re-pin: the template literal setup.ts hands parse().
function vendoredSetupNodes() {
    const src = readFileSync(new URL("../vendor/ejs/src/yt/solver/setup.ts", import.meta.url), "utf8");
    const m = src.match(/parse\(`([\s\S]*?)`\)/);
    assert.ok(m, "setup.ts no longer has the parse(`...`) shape this test reads");
    return m[1];
}

// The preprocessed player under test: ejs's setupNodes, then a solver that
// reads globalThis.location the way player code may, so the answer proves the
// setup ran inside the solve.
const PREPROCESSED = `${vendoredSetupNodes()}
_result.n = (x) => x.split("").reverse().join("") + (String(globalThis.location.href).includes("yt-dlp-wins") ? "-ejsloc" : "-pageloc");
_result.sig = null;`;

// A stand-in for ejs's preprocess step only (a real one needs a real player):
// the "player" call returns PREPROCESSED, and every solve goes to the real
// ejsMain, which evaluates it.
function realEjsOnPreprocessed(input) {
    if (input.type === "player") {
        return { ...ejsMain({ type: "preprocessed", preprocessed_player: PREPROCESSED, requests: [] }), preprocessed_player: PREPROCESSED };
    }
    return ejsMain(input);
}

// Mutants: runEjs calling ejs without withEjsGlobalsRestored (location is the
// yt-dlp-wins URL afterwards, self and XMLHttpRequest stay defined); the
// restore skipping the delete of a global that was absent (self and
// XMLHttpRequest stay defined).
test("a real ejs solve leaves the JSDOM page's globals as they were", () => {
    assertJsdomLocation();
    const before = snapshot();
    _setEjsForTest(realEjsOnPreprocessed);

    const { nResults } = solveCipher({ playerID: "p1", playerJS: "js", nChallenges: ["abcdefghijk"] });

    assert.equal(nResults.abcdefghijk, "kjihgfedcba-ejsloc", "the solve must have run under ejs's own location");
    assertJsdomLocation();
    assertSameGlobals(before, "an ejs solve");
});

// Mutant: the restore moved out of finally (a throwing ejs leaves the
// globals polluted).
test("the globals are restored when ejs throws, and its error still surfaces", () => {
    const before = snapshot();
    _setEjsForTest(() => {
        globalThis.location = new URL("https://www.youtube.com/watch?v=yt-dlp-wins");
        globalThis.self = globalThis;
        throw new Error("unexpected structure");
    });

    assert.throws(
        () => solveCipher({ playerID: "p2", playerJS: "js", nChallenges: ["abcdefghijk"] }),
        { message: /^ejs preprocess: unexpected structure$/ },
    );
    assertJsdomLocation();
    assertSameGlobals(before, "a throwing ejs call");
});

// Mutant: installDomGlobals not setting location.
test("installDomGlobals puts the JSDOM location back", () => {
    globalThis.location = new URL("https://www.youtube.com/watch?v=yt-dlp-wins");
    installDomGlobals();
    assertJsdomLocation();
});

// generateMinter is not runnable offline (it fetches a challenge and runs
// BotGuard), so its order is read from the source: the JSDOM page is
// re-installed after the interpreter runs and before BotGuardClient.create.
//
// Mutant: the installDomGlobals() call in generateMinter removed, or moved
// above the interpreter's new Function(interpJS)().
test("generateMinter re-installs the JSDOM page before BotGuardClient.create", () => {
    const src = readFileSync(new URL("../src/server.js", import.meta.url), "utf8");
    const fnStart = src.indexOf("async function generateMinter(");
    assert.ok(fnStart >= 0, "generateMinter not found in server.js");
    const body = src.slice(fnStart);
    const interp = body.indexOf("new Function(interpJS)();");
    const create = body.indexOf("BotGuardClient.create(");
    assert.ok(interp >= 0 && create > interp, "server.js no longer has the interpreter-then-create shape this test reads");
    const between = body.slice(interp, create);
    assert.match(between, /^\s*installDomGlobals\(\);\s*$/m, "installDomGlobals() must run between the interpreter and BotGuardClient.create");
});
