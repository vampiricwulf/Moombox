// Build pipeline for the Moombox BotGuard sidecar.
//
// Why no esbuild bundle?
//
// We tried bundling src/server.js + bgutils-js + jsdom into a single CJS
// file. The bundle compiled, but JSDOM reads several static assets at
// runtime via `fs.readFileSync(__dirname, "../browser/default-stylesheet.css")`
// (and similar). After bundling, `__dirname` points at dist/, so those
// reads explode because the assets aren't there. Working around that
// requires non-trivial esbuild plugin work (intercept fs.readFileSync,
// inline the asset bytes, rewrite the call) -- way more clever than this
// project needs for its first ship.
//
// Instead: tar up the whole sidecar (package.json + src/ + production
// node_modules/) into a single gzipped tarball. The Go side extracts the
// tarball next to node.exe and invokes `node src/server.js`. Larger blob
// (~6 MB gzipped vs. ~1 MB for a hypothetical single-file bundle) but
// dead-simple and guaranteed to match what `npm install` produces.
//
// What this script does:
//   1. Verify production deps are installed (node_modules/ must exist;
//      run `npm ci --omit=dev` first if not).
//   2. Run system `tar` to create dist/sidecar.tar.gz containing
//      package.json, package-lock.json, src/, and node_modules/.
//      Excludes README, .gitignore, build.mjs, and other dev artifacts.
//   3. Copy dist/sidecar.tar.gz to internal/bgutils/embed/ for go:embed.
//   4. Print sizes.
//
// Run via:  node build.mjs

import { spawnSync } from "node:child_process";
import { statSync, readdirSync, mkdirSync, copyFileSync, rmSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);

const distDir = resolve(__dirname, "dist");
const distTar = resolve(distDir, "sidecar.tar.gz");
const embedDir = resolve(__dirname, "..", "internal", "bgutils", "embed");
const embedTar = resolve(embedDir, "sidecar.tar.gz");
const stale = resolve(embedDir, "server.cjs.gz");

// ---------------------------------------------------------------------------
// 1. Sanity checks.
// ---------------------------------------------------------------------------
const nmDir = resolve(__dirname, "node_modules");
try {
    statSync(nmDir);
} catch {
    console.error(
        "ERROR: node_modules/ missing. Run `npm ci --omit=dev` first.",
    );
    process.exit(1);
}

const productionEntries = ["package.json", "package-lock.json", "src", "node_modules", "vendor/ejs.bundle.js"];
for (const entry of productionEntries) {
    if (entry === "vendor/ejs.bundle.js") continue; // freshly written by esbuild step above
    try {
        statSync(resolve(__dirname, entry));
    } catch {
        console.error(`ERROR: required path missing: ${entry}`);
        process.exit(1);
    }
}

mkdirSync(distDir, { recursive: true });
mkdirSync(embedDir, { recursive: true });

// ---------------------------------------------------------------------------
// 1b. Bundle vendored ejs into a single ESM file using esbuild.
//
// ejs is a TypeScript project under vendor/ejs/. The sidecar imports it
// as a single bundle to avoid shipping the .ts source + transpiling at
// runtime. esbuild handles TypeScript natively — no separate tsc pass.
//
// Output: vendor/ejs.bundle.js. Gitignored; regenerated on every build.
// Included in the production tarball (see step 2 below).
//
// We invoke esbuild as a CLI subprocess rather than using the JS API so
// that esbuild.exe is a child process that exits immediately after
// bundling. This is necessary on Windows: the JS API loads esbuild.exe
// in-process via a native binding, and Windows holds a file lock on it
// for the life of the Node.js process, which would prevent the npm prune
// step (1c) from deleting the @esbuild/win32-x64 package.
// ---------------------------------------------------------------------------
const ejsEntry = resolve(__dirname, "vendor", "ejs", "src", "yt", "solver", "main.ts");
const ejsBundle = resolve(__dirname, "vendor", "ejs.bundle.js");
const esbuildResult = spawnSync(
    "npx",
    [
        "--no-install",
        "esbuild",
        ejsEntry,
        "--bundle",
        "--format=esm",
        "--platform=neutral",
        "--target=node20",
        `--outfile=${ejsBundle}`,
        "--minify",
        "--legal-comments=none",
    ],
    { cwd: __dirname, stdio: "inherit", shell: true },
);
if (esbuildResult.status !== 0) {
    console.error(`esbuild exited with status ${esbuildResult.status}`);
    process.exit(esbuildResult.status ?? 1);
}
const ejsSize = statSync(ejsBundle).size;
console.log(`ejs bundle    ${(ejsSize / 1024).toFixed(1)} KB  vendor/ejs.bundle.js`);

// ---------------------------------------------------------------------------
// 1c. Prune devDeps from node_modules so the tarball ships only runtime
// dependencies. esbuild (used in step 1b) is a devDep weighing ~12 MB —
// without this prune the tarball roughly doubles in size for no runtime
// benefit. We do this AFTER bundling so the bundle has access to
// esbuild, and step 3b below restores devDeps so further development
// isn't blocked.
// ---------------------------------------------------------------------------
const pruneResult = spawnSync(
    "npm",
    ["prune", "--omit=dev", "--no-audit", "--no-fund"],
    { cwd: __dirname, stdio: "inherit", shell: true },
);
if (pruneResult.status !== 0) {
    console.error(`npm prune --omit=dev exited with status ${pruneResult.status}`);
    process.exit(pruneResult.status ?? 1);
}

// ---------------------------------------------------------------------------
// 2. Tar.
// ---------------------------------------------------------------------------
// Use cwd=__dirname + relative entries so paths inside the archive are
// relative to the sidecar root (no `bgutil-sidecar/` prefix).
// Also pass --force-local so GNU tar on Git Bash doesn't misread the
// `D:` drive prefix as a `host:path` remote-server reference.
const tarOut = `dist/sidecar.tar.gz`;
const result = spawnSync(
    "tar",
    ["--force-local", "-czf", tarOut, ...productionEntries],
    { cwd: __dirname, stdio: "inherit" },
);
if (result.status !== 0) {
    console.error(`tar exited with status ${result.status}`);
    process.exit(result.status ?? 1);
}

// ---------------------------------------------------------------------------
// 3. Copy + clean stale.
// ---------------------------------------------------------------------------
copyFileSync(distTar, embedTar);
try { rmSync(stale); } catch { /* not present, fine */ }

// ---------------------------------------------------------------------------
// 3b. Restore devDeps so subsequent development (running `node build.mjs`
// again, npm scripts, etc.) isn't blocked by missing tools. Idempotent:
// if devDeps are already present, npm install is a near-no-op.
// ---------------------------------------------------------------------------
const restoreResult = spawnSync(
    "npm",
    ["install", "--no-audit", "--no-fund", "--ignore-scripts"],
    { cwd: __dirname, stdio: "inherit", shell: true },
);
if (restoreResult.status !== 0) {
    console.error(`npm install (devDep restore) exited with status ${restoreResult.status}`);
    process.exit(restoreResult.status ?? 1);
}

// ---------------------------------------------------------------------------
// 4. Report.
// ---------------------------------------------------------------------------
const tarSize = statSync(distTar).size;
const fmt = (n) => `${(n / 1024 / 1024).toFixed(2)} MB`;
const nmSize = sumDir(nmDir);
console.log(`node_modules/  ${fmt(nmSize)}  (production)`);
console.log(`tarball       ${fmt(tarSize)}  gzipped`);
console.log(`wrote         ${distTar}`);
console.log(`wrote         ${embedTar}`);

function sumDir(p) {
    let total = 0;
    for (const ent of readdirSync(p, { withFileTypes: true })) {
        const full = resolve(p, ent.name);
        if (ent.isDirectory()) total += sumDir(full);
        else if (ent.isFile()) total += statSync(full).size;
    }
    return total;
}
