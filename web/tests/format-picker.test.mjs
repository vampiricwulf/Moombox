// The Add dialog's format picker (populateFormatSelects in web/public/app.js),
// fed by GET /api/formats — cmd/moombox/adapters.go's formatsPayload, whose Go
// test pins the server half: each row carries `damaged` and `segmented`, and
// bestItags already skips both, falling back to the first rendition of a kind
// only when every one of that kind is marked.
//
// This suite needs jsdom. `node --test web/tests/*.test.mjs` must stay green
// without it, so the import is probed first and every test skips (not fails)
// when it is absent.
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

const WEBM = 'video/webm; codecs="vp9"';
const MP4 = 'video/mp4; codecs="avc1.640028"';
const OPUS = 'audio/webm; codecs="opus"';
const AAC = 'audio/mp4; codecs="mp4a.40.2"';

const video = (itag, width, height, mimeType, bitrate, marks = {}) =>
  ({ itag, width, height, mimeType, bitrate, damaged: false, segmented: false, ...marks });
const audio = (itag, mimeType, bitrate, marks = {}) =>
  ({ itag, mimeType, bitrate, audioSampleRate: "48000", damaged: false, segmented: false, ...marks });

/** Each option of a select, as [value, text]. */
function options(h, id) {
  return [...h.el(id).querySelectorAll("sl-option:not([value='-1'])")].map((o) => [o.value, o.textContent]);
}

// MUTANT: dropping the marks from either loop — the damaged and segmented
// rows read as any other, and the operator pins one unaware.
test("the picker marks damaged and segmented renditions after their badges", { skip }, async () => {
  const h = await harness.makeApp();
  await h.app.populateFormatSelects({
    videoFormats: [
      video(313, 3840, 2160, WEBM, 20_000_000, { damaged: true }),
      video(271, 2560, 1440, WEBM, 9_000_000, { segmented: true }),
      video(248, 1920, 1080, WEBM, 4_000_000),
      video(137, 1920, 1080, MP4, 6_000_000, { damaged: true, segmented: true }),
    ],
    audioFormats: [
      audio(251, OPUS, 160_000),
      audio(140, AAC, 128_000, { damaged: true }),
    ],
    // Every MP4 and every AAC is marked, so their badges are the fallback.
    bestItags: { bestWebmVideo: 248, bestMp4Video: 137, bestOpusAudio: 251, bestAacAudio: 140 },
  });

  assert.deepEqual(options(h, "video-format-select"), [
    ["313", "3840x2160 WEBM 20000kbps [damaged]"],
    ["271", "2560x1440 WEBM 9000kbps [segmented]"],
    ["248", "1920x1080 WEBM 4000kbps [Best WEBM]"],
    ["137", "1920x1080 MP4 6000kbps [Best MP4] [damaged] [segmented]"],
  ]);
  assert.deepEqual(options(h, "audio-format-select"), [
    ["251", "160kbps 48000Hz OPUS [Best OPUS]"],
    ["140", "128kbps 48000Hz AAC [Best AAC] [damaged]"],
  ]);
  assert.equal(h.el("video-format-select").value, "248", "the intact WEBM badge is preselected");
  assert.equal(h.el("audio-format-select").value, "251");
});

// MUTANT: the preselection back to "the WEBM badge, else the MP4 one" — the
// damaged WEBM fallback is preselected over an intact MP4, and the damaged
// Opus over an intact AAC.
test("the preselection goes to an intact badge before a marked one", { skip }, async () => {
  const h = await harness.makeApp();
  await h.app.populateFormatSelects({
    videoFormats: [
      video(313, 3840, 2160, WEBM, 20_000_000, { damaged: true }), // every WEBM is damaged
      video(136, 1280, 720, MP4, 2_000_000),
    ],
    audioFormats: [
      audio(251, OPUS, 160_000, { segmented: true }), // every Opus is segmented
      audio(140, AAC, 128_000),
    ],
    bestItags: { bestWebmVideo: 313, bestMp4Video: 136, bestOpusAudio: 251, bestAacAudio: 140 },
  });
  assert.equal(h.el("video-format-select").value, "136");
  assert.equal(h.el("audio-format-select").value, "140");
});

// MUTANT: the preselection skipping a marked badge even when no intact one
// exists — a live stream, every rendition segmented, preselects nothing.
test("with every badge marked the preselection keeps the old order", { skip }, async () => {
  const h = await harness.makeApp();
  await h.app.populateFormatSelects({
    videoFormats: [
      video(303, 1920, 1080, WEBM, 6_000_000, { segmented: true }),
      video(299, 1920, 1080, MP4, 6_000_000, { segmented: true }),
    ],
    audioFormats: [audio(140, AAC, 128_000, { segmented: true })],
    bestItags: { bestWebmVideo: 303, bestMp4Video: 299, bestOpusAudio: null, bestAacAudio: 140 },
  });
  assert.equal(h.el("video-format-select").value, "303");
  assert.equal(h.el("audio-format-select").value, "140");
});

// The owner's third picker follow-up: while a stream is live or upcoming
// every adaptive rendition it lists is segmented — the live path fetches
// exactly those — so " [segmented]" on every row said nothing. The mark is
// left off for a live or upcoming streamStatus and kept for a VOD, a
// finished stream (post_live) and a status the answer did not state. The
// damaged mark, the badges and the preselection, which still passes a
// segmented badge over, are the same for every status.
//
// MUTANT: the onAir test dropped, or reading "live" alone or "upcoming"
// alone (that status's rows say segmented); "post_live" or "vod" read as on
// air (their rows lose the mark); the preselection reading the mark as shown
// rather than the server's flag (a live stream's segmented WEBM and Opus
// badges are preselected).
for (const [streamStatus, shown] of [
  ["live", false],
  ["upcoming", false],
  ["vod", true],
  ["post_live", true],
  [undefined, true],
]) {
  test(`streamStatus ${streamStatus}: the segmented mark is ${shown ? "shown" : "left off"}`, { skip }, async () => {
    const h = await harness.makeApp();
    await h.app.populateFormatSelects({
      streamStatus,
      videoFormats: [
        video(303, 1920, 1080, WEBM, 6_000_000, { segmented: true }),
        video(299, 1920, 1080, MP4, 6_000_000, { damaged: true, segmented: true }),
        video(136, 1280, 720, MP4, 2_000_000),
      ],
      audioFormats: [
        audio(251, OPUS, 160_000, { segmented: true }),
        audio(140, AAC, 128_000),
      ],
      bestItags: { bestWebmVideo: 303, bestMp4Video: 136, bestOpusAudio: 251, bestAacAudio: 140 },
    });
    const seg = shown ? " [segmented]" : "";
    assert.deepEqual(options(h, "video-format-select"), [
      ["303", `1920x1080 WEBM 6000kbps [Best WEBM]${seg}`],
      ["299", `1920x1080 MP4 6000kbps [damaged]${seg}`],
      ["136", "1280x720 MP4 2000kbps [Best MP4]"],
    ]);
    assert.deepEqual(options(h, "audio-format-select"), [
      ["251", `160kbps 48000Hz OPUS [Best OPUS]${seg}`],
      ["140", "128kbps 48000Hz AAC [Best AAC]"],
    ]);
    assert.equal(h.el("video-format-select").value, "136", "the intact MP4 badge, past the segmented WEBM one");
    assert.equal(h.el("audio-format-select").value, "140", "the intact AAC badge, past the segmented Opus one");
  });
}

// Owner decision FE-10: the audio rendition automatic selection takes is
// badged [Best] whatever its codec — a 5.1 E-AC-3 track the shared rank puts
// above stereo Opus and AAC had no badge at all, and was labelled AAC (its
// container). A track that is also its codec's pick carries both badges.
// The preselection still follows the codec badges.
//
// MUTANT: the [Best] badge dropped (the E-AC-3 row has none); the E-AC-3
// label dropped (the row reads AAC); [Best] keyed on bestOpusAudio (the
// Opus row carries it in the first case).
test("the automatic audio pick is badged [Best], beside the codec badges", { skip }, async () => {
  const EAC3 = 'audio/mp4; codecs="ec-3"';
  for (const [bestAudio, want] of [
    [328, ["384kbps 48000Hz EC-3 [Best]", "160kbps 48000Hz OPUS [Best OPUS]", "128kbps 48000Hz AAC [Best AAC]"]],
    [251, ["384kbps 48000Hz EC-3", "160kbps 48000Hz OPUS [Best] [Best OPUS]", "128kbps 48000Hz AAC [Best AAC]"]],
  ]) {
    const h = await harness.makeApp();
    await h.app.populateFormatSelects({
      streamStatus: "vod",
      videoFormats: [video(248, 1920, 1080, WEBM, 4_000_000)],
      audioFormats: [audio(328, EAC3, 384_000), audio(251, OPUS, 160_000), audio(140, AAC, 128_000)],
      bestItags: { bestWebmVideo: 248, bestMp4Video: null, bestOpusAudio: 251, bestAacAudio: 140, bestAudio },
    });
    assert.deepEqual(options(h, "audio-format-select").map(([, text]) => text), want);
    assert.equal(h.el("audio-format-select").value, "251", "the preselection still follows the codec badges");
  }
});
