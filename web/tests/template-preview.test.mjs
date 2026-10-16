// The Settings page's output-template example formatted ${start_date} as
// 2026-10-04 (UTC, too) and ${start_time} as 20-00-00 and appended .mkv,
// while config.ResolveTemplate writes 20261004 and 2000 in local time and the
// muxer writes .mp4 — a name no recording ever got. The TUI twin calls the
// resolver itself (internal/tui/template_preview_test.go).
//
// Mutant: restore the toISOString date, "20-00-00" or ".mkv".
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { renderTemplatePreview } from "../public/modules/settings.js";

// The page-level test needs jsdom; without it (no `npm ci`) it skips, and the
// pure tests still run.
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

test("the template example uses the resolver's formats and .mp4", () => {
  const got = renderTemplatePreview("${channel}/${start_date} ${title} [${id}] ${start_time}");
  assert.match(got, /^Example: Miko Ch\/\d{8} Singing Stream \[dQw4w9WgXcQ\] 2000\.mp4$/);
  const now = new Date();
  const pad = (n) => String(n).padStart(2, "0");
  assert.ok(got.includes(`${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}`), "the date is not today's local date");
  assert.equal(renderTemplatePreview(""), "");
});

// The example's date is the SERVER's today, not the browser's: recordings are
// named in the server's zone (config.ResolveTemplate, time.Local), and a
// remote viewer's browser, or a container's TZ, can be a day away from it.
// /api/status's utcOffsetMinutes gives the server's zone; the browser's
// date is only the fallback before it is known.
//
// Mutant: ignore utcOffsetMinutes (the browser's date — the real clock's —
// comes back); add the offset with its sign flipped (the wrong side of
// midnight).
test("the template example's date is the server's when its offset is known", (t) => {
  const realNow = Date.now;
  t.after(() => { Date.now = realNow; });
  const tmpl = "${start_date}";

  Date.now = () => Date.UTC(2026, 9, 17, 3, 30); // 2026-10-17T03:30Z
  assert.equal(renderTemplatePreview(tmpl, -420), "Example: 20261016.mp4", "a UTC-7 server is still on Oct 16");
  assert.equal(renderTemplatePreview(tmpl, 0), "Example: 20261017.mp4");

  Date.now = () => Date.UTC(2026, 9, 16, 23, 30); // 2026-10-16T23:30Z
  assert.equal(renderTemplatePreview(tmpl, 540), "Example: 20261017.mp4", "a UTC+9 server is already on Oct 17");
});

// The Settings page wires it up: /api/status's utcOffsetMinutes reaches the
// example, and re-draws it when the status lands after the config has drawn
// it with the browser's date (both load at boot, config first). The frozen
// clock is 2026-09-05T12:00Z, which is still Sep 4 at UTC-13.
//
// Mutants: loadStatus not keeping utcOffsetMinutes, or not re-drawing the
// example (it keeps the browser's date); _wireTemplatePreview not passing
// the offset (the same).
test("the Settings example shows the server's date once /api/status gives its zone", { skip }, async () => {
  const h = await harness.makeApp({
    initialState: {
      config: { downloader: { output_template: "${start_date} ${title}" } },
      status: { utcOffsetMinutes: -780 },
    },
  });
  assert.equal(h.el("cfg-template-preview").textContent, "Example: 20260904 Singing Stream.mp4");
});
