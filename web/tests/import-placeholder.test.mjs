// An imported archive that carried no real YouTube id gets the placeholder
// the ZIP import mints — "imp_" + randomHex(4), internal/web/routes/
// import_routes.go — and a url, a thumbnail and (in the dialog) an embed built
// from it, all pointing at a video that does not exist. The details dialog
// drew a dead YouTube embed, a Stream URL row with a copy button for the fake
// link, and an Open URL button that opened it.
//
// isImportPlaceholderId is the one place the frontend knows that shape, so the
// first test pins it against the Go side's exact output: lowercase hex, eight
// digits — the eleven-character budget of a real YouTube id makes "imp_" + 7
// a legal id and "imp_" + 8 never one.
//
// Like app.test.mjs, this suite needs jsdom, and `node --test web/tests/*.test.mjs`
// must stay green without it — so the import is probed first and every DOM
// test is skipped (not failed) when jsdom is absent. Only an absent module is
// a skip; the helper test itself needs no DOM.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { isImportPlaceholderId, streamUrl } from "../public/modules/utils.js";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = `jsdom not installed — run \`npm ci\` in web/tests (${e.code})`;
}
const harness = jsdomMissing ? null : await import("./helpers/app-dom.mjs");
const inputs = jsdomMissing ? null : await import("./fixtures/app-render-inputs.mjs");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

const PLACEHOLDER = "imp_0a1b2c3d";

/** The row the import creates: Finished, manually added, every link derived from the placeholder. */
const imported = () => ({
  ...inputs.JOBS.Finished,
  id: PLACEHOLDER,
  videoId: PLACEHOLDER,
  url: `https://www.youtube.com/watch?v=${PLACEHOLDER}`,
  thumbnailUrl: `https://i.ytimg.com/vi/${PLACEHOLDER}/maxresdefault.jpg`,
  filename: "imports/An Import [imp_0a1b2c3d].mp4",
  manuallyAdded: true,
  watched: false,
  incompleteTail: false,
});

// MUTANT: match `^imp_` alone — a real YouTube video whose id happens to start
// with "imp_" loses its embed and its link. MUTANT: accept uppercase — the Go
// side never produces it, so the match would be claiming a shape it does not
// have. Pure, so no `skip`.
test("isImportPlaceholderId matches the import's exact shape and nothing else", () => {
  assert.equal(isImportPlaceholderId("imp_0a1b2c3d"), true);
  assert.equal(isImportPlaceholderId("imp_ffffffff"), true);
  // Legal YouTube ids: eleven characters of [A-Za-z0-9_-].
  assert.equal(isImportPlaceholderId("imp_0a1b2c3"), false, "imp_ + 7 hex digits is a legal YouTube id");
  assert.equal(isImportPlaceholderId("imp_0A1B2C3D"), false, "hex.EncodeToString is lowercase");
  assert.equal(isImportPlaceholderId("imp_0a1b2c3dz"), false, "too long");
  assert.equal(isImportPlaceholderId("dQw4w9WgXcQ"), false);
  assert.equal(isImportPlaceholderId("tw_v123456789"), false);
  assert.equal(isImportPlaceholderId(""), false);
  assert.equal(isImportPlaceholderId(undefined), false);
  // The row still has a url — streamUrl is the TUI twin and is left alone;
  // the dialog is where the fake link is withheld.
  assert.equal(streamUrl({ url: `https://www.youtube.com/watch?v=${PLACEHOLDER}`, videoId: PLACEHOLDER }),
    `https://www.youtube.com/watch?v=${PLACEHOLDER}`);
});

// MUTANT: render the embed regardless (the shipped dialog) — an iframe to
// youtube-nocookie.com/embed/imp_… sits above the details, playing nothing.
// MUTANT: keep the Stream URL row — a copy button hands the operator a link
// to a video that does not exist.
test("an imported job's dialog shows no embed and no Stream URL row", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.details.renderJobDetails(imported());
  const content = h.el("job-details-content");

  assert.equal(content.querySelector("iframe"), null, "no embed for a placeholder id");
  assert.equal(content.querySelector(".details-top .details-section + .details-section"), null,
    "the embed's own section must be gone, not left empty beside the details");
  assert.ok(content.querySelector(".details-top").classList.contains("no-embed"),
    "the info column takes the embed's width (moombox.css .details-top.no-embed)");
  const labels = [...content.querySelectorAll(".details-label")].map((l) => l.textContent);
  assert.ok(!labels.includes("Stream URL:"), "no Stream URL row for the fake link");
  assert.ok(labels.includes("Video ID:"), "the id itself is real — it names the file — and stays");
  assert.ok(!content.innerHTML.includes(`watch?v=${PLACEHOLDER}`), "the fake link must appear nowhere");
  assert.equal(h.el("details-open-url-btn").style.display, "none", "Open URL must be hidden");
  // The other footer buttons are untouched by the import check.
  assert.equal(h.el("details-play-btn").style.display, "", "Play stays — the file is real");
});

// MUTANT: hide the embed on `manuallyAdded`, or on every youtube job — a real
// id's dialog loses its embed and its link. The Finished fixture is the
// snapshot app.test.mjs pins byte for byte, so this is also the proof that the
// template change is invisible for every job that is not an import.
test("a real id keeps its embed, its Stream URL row and its Open URL button", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.details.renderJobDetails(inputs.JOBS.Finished);
  const content = h.el("job-details-content");

  assert.ok(content.querySelector("iframe.details-embed"), "the embed must still render");
  assert.ok(!content.querySelector(".details-top").classList.contains("no-embed"));
  const labels = [...content.querySelectorAll(".details-label")].map((l) => l.textContent);
  assert.ok(labels.includes("Stream URL:"));
  assert.equal(h.el("details-open-url-btn").style.display, "");

  // And the button comes back when the dialog moves from an import to a real
  // job without a rebuild in between: updateDetailsButtons owns it.
  h.app.details.renderJobDetails(imported());
  assert.equal(h.el("details-open-url-btn").style.display, "none");
  h.app.details.updateDetailsButtons(inputs.JOBS.Finished);
  assert.equal(h.el("details-open-url-btn").style.display, "");
});

/** An imported Twitch live capture: a stream id that names no page, so no url (internal/web/routes/import_routes.go). */
const importedTwitchCapture = (channelName) => ({
  ...inputs.JOBS.Finished,
  id: "tw_316543210987",
  videoId: "tw_316543210987",
  platform: "twitch",
  url: "",
  thumbnailUrl: "",
  channelName,
  filename: "imports/Late Night [tw_316543210987].mp4",
  manuallyAdded: true,
  isVod: false,
  watched: false,
  incompleteTail: false,
});

// An imported Twitch live capture has no url, and the dialog built its page
// from the channel name anyway: "Import" (no chat in the zip) embedded and
// linked twitch.tv/Import, a stranger's channel, and a display name a page
// that does not exist — the Stream URL row and its copy button, the embed and
// Open URL all of them.
//
// MUTANT: streamUrl deriving twitch.tv/<channelName> again (the Stream URL row
// and Open URL come back). MUTANT: the embed taking channelName when the url
// is empty (player.twitch.tv/?channel=import). MUTANT: openJobUrl building its
// own twitch.tv/<channelName> (it opens the stranger's channel). MUTANT: Open
// URL shown on the placeholder check alone. MUTANT: a Twitch row with no page
// falling through to the YouTube embed (youtube-nocookie.com/embed/tw_…).
test("an imported Twitch live capture's dialog derives no page from its channel name", { skip }, async () => {
  for (const channelName of ["Import", "加藤純一"]) {
    const h = await harness.makeApp();
    const job = importedTwitchCapture(channelName);
    h.app.jobs = [job];
    h.app.selectedJobId = job.id;
    h.app.details.renderJobDetails(job);
    h.app.details.updateDetailsButtons(job);
    const content = h.el("job-details-content");

    assert.equal(content.querySelector("iframe"), null, `${channelName}: no embed for a channel the row does not name`);
    const labels = [...content.querySelectorAll(".details-label")].map((l) => l.textContent);
    assert.ok(!labels.includes("Stream URL:"), `${channelName}: no Stream URL row`);
    assert.ok(!content.innerHTML.includes("twitch.tv/"), `${channelName}: no twitch.tv link anywhere`);
    assert.equal(h.el("details-open-url-btn").style.display, "none", `${channelName}: Open URL hidden`);

    const opened = [];
    h.window.open = (...args) => { opened.push(args[0]); };
    h.app.openJobUrl();
    assert.deepEqual(opened, [], `${channelName}: Open URL opened ${opened}`);
  }
});

// MUTANT: the embed's login read from nothing but the channelName, or Open URL
// hidden for every Twitch live row — a native capture carries its url, and
// keeps all three.
test("a native Twitch capture keeps its embed, its Stream URL row and Open URL", { skip }, async () => {
  const h = await harness.makeApp();
  // A display name that is not the login: the embed's login is the url's.
  const job = { ...importedTwitchCapture("サム Streamer"), url: "https://www.twitch.tv/somestreamer", manuallyAdded: false };
  h.app.jobs = [job];
  h.app.selectedJobId = job.id;
  h.app.details.renderJobDetails(job);
  h.app.details.updateDetailsButtons(job);
  const content = h.el("job-details-content");

  assert.match(content.querySelector("iframe.details-embed")?.src ?? "", /player\.twitch\.tv\/\?channel=somestreamer&/);
  const labels = [...content.querySelectorAll(".details-label")].map((l) => l.textContent);
  assert.ok(labels.includes("Stream URL:"));
  assert.equal(h.el("details-open-url-btn").style.display, "");
  const opened = [];
  h.window.open = (...args) => { opened.push(args[0]); };
  h.app.openJobUrl();
  assert.deepEqual(opened, ["https://www.twitch.tv/somestreamer"]);
});
