// The player's notice for an archive whose audio the browser cannot play
// (owner decision PR-R7): Moombox keeps yt-dlp's audio ranking, which puts a
// 5.1 E-AC-3 track above stereo Opus and AAC, and most Chrome and Firefox
// builds cannot decode it — the archive played in silence with nothing
// saying why. The player asks GET /api/jobs/{id}/audio-info for the codec
// and the video element's canPlayType for a verdict.
//
// jsdom is probed first and the DOM tests skip without it; the pure one
// runs everywhere.
import { test, after } from "node:test";
import assert from "node:assert/strict";

import { unplayableAudioText } from "../public/modules/utils.js";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = `jsdom not installed — run \`npm ci\` in web/tests (${e.code})`;
}
const harness = jsdomMissing ? null : await import("./helpers/player-dom.mjs");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

const NO_EAC3 = (mime) => (mime.includes("ec-3") || mime.includes("ac-3") ? "" : "probably");

// Mutants: canPlay's answer ignored (the AAC row warns); the channel layout
// dropped ("E-AC-3" alone); an unknown codec judged (the "truehd" row warns).
test("unplayableAudioText names the codec and layout the browser cannot play", () => {
  assert.equal(unplayableAudioText({ codec: "eac3", channels: 6 }, NO_EAC3),
    "This archive's audio (E-AC-3, 5.1) can't play in this browser; the file is fine. Open it in VLC or mpv.");
  assert.equal(unplayableAudioText({ codec: "ac3", channels: 2 }, NO_EAC3),
    "This archive's audio (AC-3, stereo) can't play in this browser; the file is fine. Open it in VLC or mpv.");
  assert.equal(unplayableAudioText({ codec: "aac", channels: 2 }, NO_EAC3), "");
  assert.equal(unplayableAudioText({ codec: "truehd", channels: 8 }, () => ""), "");
  assert.equal(unplayableAudioText({}, () => ""), "");
});

const finished = (id) => ({
  id, status: "Finished", filename: `${id}.mp4`, title: `Title ${id}`,
  channelName: "Chan", updatedAt: "2026-09-01T00:00:00Z",
});

// Selecting a job with E-AC-3 audio shows the notice; the next job, with
// AAC, clears it, and so does one whose audio-info cannot be read.
//
// Mutants: _checkAudioPlayable not called on select (no notice); the
// previous job's notice not cleared (it stays on the AAC job).
test("the player shows the notice for an archive whose audio cannot play, and clears it", { skip }, async () => {
  const h = harness.makePlayer({ jobs: [finished("ec"), finished("aa"), finished("gone")] });
  h.video.canPlayType = NO_EAC3;
  h.http.on("GET /api/jobs/:id/audio-info", ({ params }) =>
    params.id === "ec" ? { codec: "eac3", channels: 6 }
      : params.id === "aa" ? { codec: "aac", channels: 2 }
        : { __response: true, status: 500, body: { error: "x" } });
  const notice = h.el("player-audio-notice");
  assert.ok(notice, "no #player-audio-notice in the player markup");

  await h.selectJob("ec");
  await h.flush();
  assert.equal(notice.hidden, false);
  assert.match(notice.textContent, /E-AC-3, 5\.1\) can't play in this browser/);

  await h.selectJob("aa");
  await h.flush();
  assert.equal(notice.hidden, true);
  assert.equal(notice.textContent, "");

  await h.selectJob("ec");
  await h.flush();
  assert.equal(notice.hidden, false);
  await h.selectJob("gone");
  await h.flush();
  assert.equal(notice.hidden, true, "a job whose audio-info failed kept the previous job's notice");
});
