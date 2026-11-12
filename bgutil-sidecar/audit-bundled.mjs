// Audit the libraries esbuild bundles into vendor/ejs.bundle.js.
//
//   node audit-bundled.mjs        (in bgutil-sidecar/; the weekly scan,
//                                  .github/workflows/vuln-scan.yml, runs it)
//
// meriyah and astring are devDependencies: nothing installs them at runtime,
// because build.mjs compiles them into the ejs bundle the tarball ships. They
// parse and re-print YouTube's player JavaScript on every cipher solve, so
// they are runtime code all the same, and `npm audit --omit=dev` (the scan's
// audit of "what ships inside the binary") never looks at them. A plain
// `npm audit` would, but it also reports esbuild, which only runs at build
// time. This reads the full report and keeps the entries for these names.
//
// Moderate and above fail, as in the scan's other npm audits; lower ones are
// printed and pass. Exit 1 on a finding, 2 when npm gave no report to read.

import { spawnSync } from "node:child_process";
import { dirname } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

// Every package vendor/ejs/src imports by name.
// test/audit-bundled.test.mjs fails when this list and those imports differ,
// and when one of them gains a dependency: npm lists an advisory under the
// vulnerable package's own name, which would then have to be in this list.
export const bundledLibraries = ["astring", "meriyah"];

const failing = new Set(["moderate", "high", "critical"]);

// bundledFindings picks the entries for `names` out of an `npm audit --json`
// report (auditReportVersion 2: `vulnerabilities` is an object keyed by
// package name). It throws when the report has no such object: npm prints
// `{"error": ...}` instead when it could not reach the registry, and that
// must not read as "no findings".
export function bundledFindings(report, names = bundledLibraries) {
    const all = report?.vulnerabilities;
    if (all === null || typeof all !== "object" || Array.isArray(all)) {
        const why = report?.error?.summary || report?.error?.code || "no vulnerabilities object";
        throw new Error(`npm audit returned no report (${why})`);
    }
    const findings = [];
    for (const name of names) {
        const entry = all[name];
        if (!entry) continue;
        const severity = String(entry.severity ?? "unknown");
        // `via` holds advisory objects, or the names of the packages the
        // advisory reaches this one through.
        const via = (entry.via ?? []).map((v) => (typeof v === "string" ? v : v.title || v.url || v.name));
        // A severity this does not know fails: better a look than a miss.
        const fails = failing.has(severity) || !["info", "low"].includes(severity);
        findings.push({ name, severity, range: entry.range ?? "", via, fails });
    }
    return findings;
}

function main() {
    const here = dirname(fileURLToPath(import.meta.url));
    // npm exits non-zero when it finds anything at all, so the exit code says
    // nothing here; the report does.
    // One command string through the shell: npm is npm.cmd on Windows.
    const run = spawnSync("npm audit --json", {
        cwd: here,
        encoding: "utf8",
        shell: true,
        maxBuffer: 64 << 20,
    });
    let findings;
    try {
        findings = bundledFindings(JSON.parse(run.stdout));
    } catch (err) {
        console.error(`audit-bundled: ${err.message}`);
        if (run.error) console.error(String(run.error));
        if (run.stderr) console.error(run.stderr.trim());
        process.exit(2);
    }
    if (findings.length === 0) {
        console.log(`audit-bundled: no advisory on ${bundledLibraries.join(", ")}`);
        return;
    }
    for (const f of findings) {
        console.log(`${f.fails ? "FAIL" : "note"}  ${f.name} ${f.range}  ${f.severity}: ${f.via.join("; ")}`);
    }
    if (findings.some((f) => f.fails)) {
        console.error("audit-bundled: a library bundled into vendor/ejs.bundle.js has an advisory; it runs on every cipher solve.");
        process.exit(1);
    }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
    main();
}
