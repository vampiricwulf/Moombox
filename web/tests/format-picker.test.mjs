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
  // Owner decision PR-R8: both selects start on Auto, whatever the badges.
  assert.equal(h.el("video-format-select").value, "", "the video select starts on Auto");
  assert.equal(h.el("audio-format-select").value, "", "the audio select starts on Auto");
});

// Owner decision PR-R8, superseding FQ-1's preselection: the picker starts
// both selects on Auto, so an untouched Advanced form pins nothing — a
// preselected video itag turned it into a pinned job, and a pinned video
// itag turns a live job's quality monitoring off. A select left on an
// earlier video's rendition is cleared when the next video's formats load.
// (The submit sends an itag only for a select that holds one.)
//
// MUTANT: the preselection back (either select holds a badge's itag); the
// clear dropped (the earlier video's 248 survives the second load).
test("the picker starts both selects on Auto", { skip }, async () => {
  const h = await harness.makeApp();
  const answer = {
    videoFormats: [video(248, 1920, 1080, WEBM, 4_000_000), video(136, 1280, 720, MP4, 2_000_000)],
    audioFormats: [audio(251, OPUS, 160_000), audio(140, AAC, 128_000)],
    bestItags: { bestWebmVideo: 248, bestMp4Video: 136, bestOpusAudio: 251, bestAacAudio: 140, bestAudio: 251 },
  };
  await h.app.populateFormatSelects(answer);
  assert.equal(h.el("video-format-select").value, "");
  assert.equal(h.el("audio-format-select").value, "");

  h.el("video-format-select").value = "248"; // the operator picks one…
  await h.app.populateFormatSelects(answer); // …then another video's formats load
  assert.equal(h.el("video-format-select").value, "", "an earlier video's rendition survived the next load");
});

// The owner's third picker follow-up: while a stream is live or upcoming
// every adaptive rendition it lists is segmented — the live path fetches
// exactly those — so " [segmented]" on every row said nothing. The mark is
// left off for a live or upcoming streamStatus and kept for a VOD, a
// finished stream (post_live) and a status the answer did not state. The
// damaged mark and the badges are the same for every status, and both
// selects start on Auto (owner decision PR-R8).
//
// MUTANT: the onAir test dropped, or reading "live" alone or "upcoming"
// alone (that status's rows say segmented); "post_live" or "vod" read as on
// air (their rows lose the mark).
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
    assert.equal(h.el("video-format-select").value, "", "Auto, whatever the badges");
    assert.equal(h.el("audio-format-select").value, "");
  });
}

// Owner decision FE-10: the audio rendition automatic selection takes is
// badged [Best] whatever its codec — a 5.1 E-AC-3 track the shared rank puts
// above stereo Opus and AAC had no badge at all, and was labelled AAC (its
// container). A track that is also its codec's pick carries both badges.
// The select itself starts on Auto (owner decision PR-R8); [Best] shows
// what Auto takes.
//
// MUTANT: the [Best] badge dropped (the E-AC-3 row has none); the E-AC-3
// label dropped (the row reads AAC); [Best] keyed on bestOpusAudio (the
// Opus row carries it in the first case).
test("the automatic audio pick is badged [Best]", { skip }, async () => {
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
    assert.equal(h.el("audio-format-select").value, "", "the select stays on Auto");
  }
});

// Owner decision PE-1: the video list badges [Best] the rendition automatic
// selection takes, of either container (bestItags.bestVideo, the server's
// cap-aware pick), beside the container badges, as the audio list's [Best]
// does. The select itself starts on Auto (owner decision PR-R8).
//
// MUTANT: the [Best] badge dropped (no video row carries it); [Best] keyed on
// bestWebmVideo (the WEBM row carries it in the second case).
test("the automatic video pick is badged [Best]", { skip }, async () => {
  for (const [bestVideo, want] of [
    [248, ["1920x1080 WEBM 4000kbps [Best] [Best WEBM]", "1920x1080 MP4 3000kbps [Best MP4]", "1280x720 MP4 2000kbps"]],
    [136, ["1920x1080 WEBM 4000kbps [Best WEBM]", "1920x1080 MP4 3000kbps [Best MP4]", "1280x720 MP4 2000kbps [Best]"]],
    [null, ["1920x1080 WEBM 4000kbps [Best WEBM]", "1920x1080 MP4 3000kbps [Best MP4]", "1280x720 MP4 2000kbps"]],
  ]) {
    const h = await harness.makeApp();
    await h.app.populateFormatSelects({
      streamStatus: "vod",
      videoFormats: [video(248, 1920, 1080, WEBM, 4_000_000), video(137, 1920, 1080, MP4, 3_000_000), video(136, 1280, 720, MP4, 2_000_000)],
      audioFormats: [audio(251, OPUS, 160_000)],
      bestItags: { bestWebmVideo: 248, bestMp4Video: 137, bestOpusAudio: 251, bestAacAudio: null, bestAudio: 251, bestVideo },
    });
    assert.deepEqual(options(h, "video-format-select").map(([, text]) => text), want);
    assert.equal(h.el("video-format-select").value, "", "the select stays on Auto");
  }
});

// A rendition picked for one video must never be sent with another. The
// selects used to keep the earlier video's choice while the new URL waited
// out its 500 ms debounce, while its formats were being fetched, and after
// that fetch failed; Add then pinned the new video to the old itag, with the
// picker hidden. A pinned video itag also turns a live job's quality
// monitoring off. The listed formats are now tied to the video they were
// listed for (_formatsListedFor), and Add sends an itag only for that video.
const VIDEO_A = "aaaaaaaaaaa";
const VIDEO_B = "bbbbbbbbbbb";
const LISTED = {
  videoFormats: [video(248, 1920, 1080, WEBM, 4_000_000)],
  audioFormats: [audio(251, OPUS, 160_000)],
  bestItags: {},
};

/** An app with Advanced open on VIDEO_A, its formats listed and one of each picked. */
async function pickedForA(formats) {
  const h = await harness.makeApp({
    routes: {
      "GET /api/formats/:id": ({ params }) => formats(params.id),
      "POST /api/jobs": () => ({ id: "j1" }),
    },
  });
  h.el("advanced-options-toggle").checked = true;
  h.el("video-url-input").value = `https://www.youtube.com/watch?v=${VIDEO_A}`;
  await h.app.fetchFormatsForAdvanced();
  h.el("video-format-select").value = "248";
  h.el("audio-format-select").value = "251";
  return h;
}

/** The body of the one POST /api/jobs the submit made. */
async function submitted(h) {
  await h.app.addVideo();
  const posts = h.http.matching("/api/jobs", "POST");
  assert.equal(posts.length, 1, "Add must post the job");
  return posts[0].body;
}

// MUTANT: the _formatsListedFor test dropped from addVideo — A's choice was
// only ever listed for A, and with it no itag at all is sent.
test("Add sends the picked renditions with the video they were listed for", { skip }, async () => {
  const h = await pickedForA(() => LISTED);
  assert.deepEqual(await submitted(h), { videoId: VIDEO_A, selectedVideoItag: 248, selectedAudioItag: 251 });
});

// The URL field's value changed and Add ran before anything else did (Enter
// inside the debounce): the selects still hold A's choice, and only the
// check in addVideo stands between it and B.
//
// MUTANT: addVideo sending whatever the selects hold — B is pinned to 248/251.
test("Add sends no rendition picked for an earlier URL's video", { skip }, async () => {
  const h = await pickedForA(() => LISTED);
  h.el("video-url-input").value = `https://www.youtube.com/watch?v=${VIDEO_B}`;
  assert.deepEqual(await submitted(h), { videoId: VIDEO_B });
});

// The URL changes to another video and that video's formats cannot be loaded
// (the shared limiter's 429 does this too). The earlier list goes as soon as
// the URL names another video, before the debounce, and stays gone.
//
// MUTANT: the clear dropped from the URL's sl-input handler (A's list is
// still shown during the debounce), or from fetchFormatsForAdvanced's start
// and its catch (the hidden selects keep 248/251).
test("changing the URL to another video drops the earlier video's formats at once", { skip }, async () => {
  const h = await pickedForA((id) => (id === VIDEO_A ? LISTED : harness.response({ status: 429, body: { error: "rate limited" } })));
  assert.equal(h.el("format-selection").style.display, "block", "A's formats are shown");

  const input = h.el("video-url-input");
  input.value = `https://www.youtube.com/watch?v=${VIDEO_B}`;
  input.dispatchEvent(new h.window.Event("sl-input"));
  assert.equal(h.el("format-selection").style.display, "none", "A's formats were still shown for B during the debounce");
  assert.deepEqual(options(h, "video-format-select"), []);
  assert.equal(h.el("video-format-select").value, "");
  assert.equal(h.el("audio-format-select").value, "");

  h.advance(500); // the debounced fetch for B, which fails
  await h.flush();
  await h.flush();
  assert.equal(h.http.matching(`/api/formats/${VIDEO_B}`).length, 1, "B's formats must be asked for");
  assert.equal(h.el("format-selection").style.display, "none");
  assert.deepEqual(await submitted(h), { videoId: VIDEO_B });
});

// Back to the first video inside the debounce: its formats were dropped when
// the URL left it, so they are fetched again rather than treated as loaded.
//
// MUTANT: the sl-input clear leaving _lastFormatVideoId on A — the fetch is
// skipped as "same video" and the picker stays hidden for good.
test("returning to the first URL lists its formats again", { skip }, async () => {
  const h = await pickedForA(() => LISTED);
  const input = h.el("video-url-input");
  for (const id of [VIDEO_B, VIDEO_A]) {
    input.value = `https://www.youtube.com/watch?v=${id}`;
    input.dispatchEvent(new h.window.Event("sl-input"));
  }
  h.advance(500);
  await h.flush();
  await h.flush();
  assert.equal(h.http.matching(`/api/formats/${VIDEO_A}`).length, 2);
  assert.equal(h.el("format-selection").style.display, "block");
  assert.deepEqual(options(h, "video-format-select").map(([value]) => value), ["248"]);
  assert.equal(h.el("video-format-select").value, "", "the earlier pick is not restored");
});
