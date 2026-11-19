import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
    _setStoreDirForTest, rawHash, readPrepared, writePrepared, sweepStore,
} from "../src/player-store.js";

const EJS12 = fs.readFileSync(new URL("../vendor/ejs/VERSION", import.meta.url), "utf8").slice(0, 12);
const DAY = 24 * 60 * 60 * 1000;

function tmp() {
    const d = fs.mkdtempSync(path.join(os.tmpdir(), "player-store-"));
    _setStoreDirForTest(d);
    return d;
}
function age(file, ms) {
    const t = new Date(Date.now() - ms);
    fs.utimesSync(file, t, t);
}
afterEach(() => _setStoreDirForTest(null));

test("round trip is ===", () => {
    const dir = tmp();
    const src = ("var a=\"\u2022\";/*\u{1F600}*/\n" + "x".repeat(50)).repeat(3000);
    assert.ok(src.length > 200000);
    const h = rawHash("raw player");
    assert.equal(writePrepared("p1", h, src), true);
    assert.ok(fs.existsSync(path.join(dir, `p1.${EJS12}.js`)));
    const got = readPrepared("p1");
    assert.equal(got.source, src);
    assert.equal(got.rawHash, h);
    assert.deepEqual(fs.readdirSync(dir), [`p1.${EJS12}.js`]);
});

test("disabled store is inert", () => {
    delete process.env.MOOMBOX_SIDECAR_PLAYER_DIR;
    _setStoreDirForTest(null);
    assert.equal(writePrepared("p1", rawHash("x"), "src"), false);
    assert.equal(readPrepared("p1"), null);
    assert.equal(sweepStore(), 0);
});

test("unsafe player IDs bypass the store", () => {
    const dir = tmp();
    for (const id of ["../x", "a/b", "", "a".repeat(65)]) {
        assert.equal(writePrepared(id, rawHash("x"), "src"), false);
        assert.equal(readPrepared(id), null);
    }
    assert.deepEqual(fs.readdirSync(dir), []);
});

test("a lone surrogate is not written", () => {
    const dir = tmp();
    assert.equal(writePrepared("p1", rawHash("x"), "a\ud800b"), false);
    assert.deepEqual(fs.readdirSync(dir), []);
});

test("a damaged header is removed", () => {
    const dir = tmp();
    const f = path.join(dir, `p1.${EJS12}.js`);
    fs.writeFileSync(f, "//moombox-prepared v1 sha256=nothex\nsource");
    assert.equal(readPrepared("p1"), null);
    assert.equal(fs.existsSync(f), false);
});

// The temp file is written and the rename onto the target fails (a directory
// sits at the target's name): the write answers false, says so once, and
// leaves no temp file behind.
//
// Mutants: the temp file left after a failed rename; the failure thrown; the
// warn dropped or repeated.
test("a write that fails at the rename warns once and leaves no temp file", () => {
    const dir = tmp();
    const target = `p1.${EJS12}.js`;
    fs.mkdirSync(path.join(dir, target));
    const lines = [];
    const orig = process.stderr.write;
    process.stderr.write = (chunk) => {
        lines.push(String(chunk));
        return true;
    };
    let wrote;
    try {
        wrote = writePrepared("p1", rawHash("x"), "src");
    } finally {
        process.stderr.write = orig;
    }
    assert.equal(wrote, false);
    assert.equal(lines.length, 1, `want one warn line, got ${JSON.stringify(lines)}`);
    assert.ok(lines[0].startsWith("[bgutil-sidecar:warn] player-store: write p1: "), lines[0]);
    assert.deepEqual(fs.readdirSync(dir), [target], "a temp file was left behind");
    assert.deepEqual(fs.readdirSync(path.join(dir, target)), []);
});

test("sweep removes other ejs versions and old files, keeps fresh ones", () => {
    const dir = tmp();
    const other = path.join(dir, "a.aaaaaaaaaaaa.js");
    const old = path.join(dir, `b.${EJS12}.js`);
    const fresh = path.join(dir, `c.${EJS12}.js`);
    for (const f of [other, old, fresh]) fs.writeFileSync(f, "x");
    age(old, 8 * DAY);
    assert.equal(sweepStore(Date.now()), 2);
    assert.deepEqual(fs.readdirSync(dir), [`c.${EJS12}.js`]);
});

test("a read touches mtime", () => {
    const dir = tmp();
    assert.equal(writePrepared("p1", rawHash("x"), "src"), true);
    const f = path.join(dir, `p1.${EJS12}.js`);
    age(f, 6 * DAY);
    assert.ok(readPrepared("p1"));
    assert.equal(sweepStore(Date.now() + 2 * DAY), 0);
    assert.ok(fs.existsSync(f));
});
