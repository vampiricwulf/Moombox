// The dashboard's YouTube URL parser (app.js extractVideoId) against the one
// YouTube URL table the Go parser (internal/utils ExtractVideoID), the TUI
// and the add route are tested against:
// internal/utils/testdata/youtube_urls.json. The dashboard used to need
// "youtube.com/watch?v=" literally, so a link with v not first
// (watch?app=desktop&v=, watch?list=...&v=) was "Invalid" here while the TUI
// and the CLI took it.
//
// Like app.test.mjs this suite needs jsdom (app.js is the dashboard
// controller), so the import is probed first and every test is skipped (not
// failed) when jsdom is absent. Only an absent module is a skip.
//
// MUTANT: the regexes this replaced (watch\?v= with v first, on any host
// that contains "youtube.com") fail the app=desktop, list=, watch/ and
// -nocookie rows and take the notyoutube.com and example.com/youtube.com/
// rows; dropping the "https://" prefix for an input without a scheme fails
// every scheme-less row.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = `jsdom not installed — run \`npm ci\` in web/tests (${e.code})`;
}
const harness = jsdomMissing ? null : await import("./helpers/app-dom.mjs");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

const HERE = path.dirname(fileURLToPath(import.meta.url));
const TABLE = JSON.parse(
  fs.readFileSync(path.join(HERE, "..", "..", "internal", "utils", "testdata", "youtube_urls.json"), "utf8"),
).rows;

test("extractVideoId follows the shared YouTube URL table", { skip }, async () => {
  assert.ok(TABLE.length > 0, "the shared table is empty");
  const h = await harness.makeApp();
  for (const row of TABLE) {
    assert.equal(h.app.extractVideoId(row.input), row.id || null, row.input);
  }
});
