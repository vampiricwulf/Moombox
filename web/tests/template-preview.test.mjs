// The Settings page's output-template example formatted ${start_date} as
// 2026-10-04 (UTC, too) and ${start_time} as 20-00-00 and appended .mkv,
// while config.ResolveTemplate writes 20261004 and 2000 in local time and the
// muxer writes .mp4 — a name no recording ever got. The TUI twin calls the
// resolver itself (internal/tui/template_preview_test.go).
//
// Mutant: restore the toISOString date, "20-00-00" or ".mkv".
import { test } from "node:test";
import assert from "node:assert/strict";
import { renderTemplatePreview } from "../public/modules/settings.js";

test("the template example uses the resolver's formats and .mp4", () => {
  const got = renderTemplatePreview("${channel}/${start_date} ${title} [${id}] ${start_time}");
  assert.match(got, /^Example: Miko Ch\/\d{8} Singing Stream \[dQw4w9WgXcQ\] 2000\.mp4$/);
  const now = new Date();
  const pad = (n) => String(n).padStart(2, "0");
  assert.ok(got.includes(`${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}`), "the date is not today's local date");
  assert.equal(renderTemplatePreview(""), "");
});
