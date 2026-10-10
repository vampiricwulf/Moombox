// A custom FFmpeg path checked in the FFmpeg overlay is saved by the server
// (POST /api/ffmpeg/check persists it), but the Settings form was loaded
// once and never heard about it, so its next Save — of any field — sent the
// old, usually empty, ffmpeg_path back and muxing returned to the PATH lookup.
//
// Same jsdom probe as app.test.mjs: an absent jsdom skips, anything else fails.
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

// Mutant: drop the setInputValue/config update — the field stays empty.
test("a checked FFmpeg path reaches the Settings form", { skip }, async () => {
  const h = await harness.makeApp({
    routes: {
      "POST /api/ffmpeg/check": () => ({ valid: true, version: "7.1", path: "/opt/ffmpeg/bin/ffmpeg" }),
    },
  });
  h.app.setup.initializeApp = () => {}; // the overlay closing re-runs boot; not under test
  h.el("ffmpeg-custom-path").value = "/opt/ffmpeg/bin/ffmpeg";
  await h.app.setup.checkFFmpegPath("ffmpeg-custom-path", "ffmpeg-check-result", "ffmpeg-check-btn");
  await h.flush();

  assert.equal(h.app.getInputValue("cfg-ffmpeg-path"), "/opt/ffmpeg/bin/ffmpeg",
    "the form's next Save would send the old path back");
  if (h.app.config?.paths) assert.equal(h.app.config.paths.ffmpeg_path, "/opt/ffmpeg/bin/ffmpeg");
});
