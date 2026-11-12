// audit-bundled.mjs reads the libraries bundled into vendor/ejs.bundle.js
// out of an `npm audit --json` report. These tests feed it reports; nothing
// here runs npm or touches the network.
//
// Run with:  cd bgutil-sidecar && npm test

import { test } from "node:test";
import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { bundledFindings, bundledLibraries } from "../audit-bundled.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

// The shape npm 7 and later print (auditReportVersion 2).
function report(vulnerabilities) {
    return { auditReportVersion: 2, vulnerabilities, metadata: {} };
}

const meriyahHigh = {
    name: "meriyah",
    severity: "high",
    isDirect: true,
    via: [{ source: 1, name: "meriyah", title: "ReDoS in the tokenizer", url: "https://example.invalid/a", severity: "high", range: "<6.2.0" }],
    effects: [],
    range: "<6.2.0",
    nodes: ["node_modules/meriyah"],
    fixAvailable: true,
};

test("a clean report has no findings", () => {
    assert.deepEqual(bundledFindings(report({})), []);
});

test("an advisory on a bundled library is a failing finding that names it", () => {
    const got = bundledFindings(report({ meriyah: meriyahHigh }));
    assert.equal(got.length, 1);
    assert.equal(got[0].name, "meriyah");
    assert.equal(got[0].fails, true);
    assert.deepEqual(got[0].via, ["ReDoS in the tokenizer"]);
});

test("a build-only package's advisory is not a finding", () => {
    const esbuild = { ...meriyahHigh, name: "esbuild", nodes: ["node_modules/esbuild"] };
    assert.deepEqual(bundledFindings(report({ esbuild })), []);
});

test("moderate and above fail; low and info are reported and pass", () => {
    const cases = [["info", false], ["low", false], ["moderate", true], ["high", true], ["critical", true], ["something-new", true]];
    for (const [severity, fails] of cases) {
        const got = bundledFindings(report({ astring: { ...meriyahHigh, name: "astring", severity } }));
        assert.equal(got.length, 1, severity);
        assert.equal(got[0].fails, fails, severity);
    }
});

test("an advisory reached through another package is named by that package", () => {
    const got = bundledFindings(report({ astring: { ...meriyahHigh, name: "astring", via: ["some-dep"] } }));
    assert.deepEqual(got[0].via, ["some-dep"]);
});

test("a report npm could not produce is an error, not a clean result", () => {
    assert.throws(() => bundledFindings({ error: { code: "ENOTFOUND", summary: "request failed" } }), /request failed/);
    assert.throws(() => bundledFindings(null), /no report/);
    assert.throws(() => bundledFindings({ vulnerabilities: [] }), /no report/);
});

test("the audited names are exactly the packages vendor/ejs/src imports", () => {
    const imported = new Set();
    const bare = /\b(?:from|import)\s*\(?\s*["']([^"'./][^"']*)["']/g;
    const walk = (dir) => {
        for (const e of readdirSync(dir, { withFileTypes: true })) {
            const p = join(dir, e.name);
            if (e.isDirectory()) {
                walk(p);
                continue;
            }
            if (!/\.(ts|js|mjs)$/.test(e.name)) continue;
            for (const m of readFileSync(p, "utf8").matchAll(bare)) {
                if (m[1].startsWith("node:")) continue;
                const parts = m[1].split("/");
                imported.add(m[1].startsWith("@") ? parts.slice(0, 2).join("/") : parts[0]);
            }
        }
    };
    walk(join(root, "vendor", "ejs", "src"));
    assert.deepEqual([...imported].sort(), [...bundledLibraries].sort());
});

// npm keys an advisory by the vulnerable package. A dependency of a bundled
// library is bundled with it, but its advisory would sit under its own name,
// which bundledFindings never reads.
test("no bundled library has a dependency the audit would not read", () => {
    const lock = JSON.parse(readFileSync(join(root, "package-lock.json"), "utf8"));
    for (const name of bundledLibraries) {
        const entry = lock.packages?.[`node_modules/${name}`];
        assert.ok(entry, `${name} is not in package-lock.json`);
        for (const kind of ["dependencies", "optionalDependencies", "peerDependencies"]) {
            assert.deepEqual(
                Object.keys(entry[kind] ?? {}),
                [],
                `${name} gained ${kind}: a bundled library gained a dependency: add it to bundledLibraries`,
            );
        }
    }
});
