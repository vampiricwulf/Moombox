// The two reorder ceilings (downloader.reorder_buffer_mb /
// reorder_budget_mb) are integers whose 0 means "unbounded", which makes the
// usual numeric-field shortcuts wrong in two specific ways: 0 must be SENT
// (not dropped as falsy), and an EMPTY field must be omitted (so the server
// keeps what it stored) rather than sent as 0, which would silently switch a
// bounded install to unbounded.
//
// Like app.test.mjs this suite needs jsdom, and `node --test
// web/tests/*.test.mjs` must stay green without it — so the import is probed
// first and every test is skipped (not failed) when jsdom is absent. Only an
// absent module is a skip; any other import failure must fail loudly.
import { test, after } from "node:test";
import assert from "node:assert/strict";

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

const CONFIG = {
  downloader: {
    output_template: "${channel}/${start_date} ${title} [${id}]",
    max_video_resolution: 2160,
    num_parallel_downloads: 10,
    segment_workers: 12,
    maximum_timeout: 600,
    reorder_buffer_mb: 512,
    reorder_budget_mb: 0,
  },
};

async function openSettings() {
  const h = await harness.makeApp({
    initialState: { config: structuredClone(CONFIG) },
    routes: { "PUT /api/config": () => ({ success: true }) },
  });
  h.app.config = structuredClone(CONFIG);
  h.app.settings.populateConfigForm();
  return h;
}

// MUTANT: drop either setInputValue line from populateConfigForm — the field
// renders empty, and the operator's next save then fails validation or, worse,
// omits the key and leaves them wondering why the box was blank.
test("both reorder fields are populated from the config", { skip }, async () => {
  const h = await openSettings();
  assert.equal(String(h.el("cfg-reorder-buffer-mb").value), "512");
  assert.equal(
    String(h.el("cfg-reorder-budget-mb").value), "0",
    "an explicit 0 (unbounded) must render as 0, not as an empty field",
  );
});

// MUTANT: read the fields with `|| undefined` (or any falsy guard) — 0 is
// dropped from the payload and an operator who sets "unbounded" gets whatever
// the server already had.
test("the save payload carries both keys, zero included", { skip }, async () => {
  const h = await openSettings();
  h.el("cfg-reorder-buffer-mb").value = "0";
  h.el("cfg-reorder-budget-mb").value = "2048";

  await h.app.settings.saveConfig();
  await h.flush();

  const put = h.http.matching("/api/config", "PUT").at(-1);
  assert.ok(put, "saveConfig issued no PUT /api/config");
  assert.equal(put.body.downloader.reorder_buffer_mb, 0);
  assert.equal(put.body.downloader.reorder_budget_mb, 2048);
});

// MUTANT: coerce an empty field to 0 — the key is sent, the server stores 0,
// and a bounded install silently becomes unbounded because someone cleared a
// box they meant to leave alone.
test("an empty reorder field is omitted so the server keeps its value", { skip }, async () => {
  const h = await openSettings();
  h.el("cfg-reorder-buffer-mb").value = "";

  await h.app.settings.saveConfig();
  await h.flush();

  const put = h.http.matching("/api/config", "PUT").at(-1);
  assert.ok(put, "saveConfig issued no PUT /api/config");
  assert.ok(
    !("reorder_buffer_mb" in put.body.downloader),
    `a cleared field must be omitted, not sent; got ${JSON.stringify(put.body.downloader)}`,
  );
});

// MUTANT: drop "0 = unbounded" from either help-text — the only place the
// dashboard explains what 0 does disappears, and the TUI twin's hint (pinned
// by internal/tui) then says something the dashboard does not.
test("both fields explain the unbounded value", { skip }, async () => {
  const h = await openSettings();
  for (const id of ["cfg-reorder-buffer-mb", "cfg-reorder-budget-mb"]) {
    const help = h.el(id).getAttribute("help-text") || "";
    assert.ok(help.includes("0 = unbounded"), `${id} help-text = ${JSON.stringify(help)}`);
    assert.ok(help.includes("arm64"), `${id} help-text must name the arm64 default: ${JSON.stringify(help)}`);
  }
});
