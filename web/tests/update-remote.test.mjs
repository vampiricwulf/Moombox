// The update dialog and the viewer it is shown to. The server installs an
// update only for a loopback peer (updateApplyOriginAllowed,
// internal/web/routes/update.go) — never for a LAN viewer, nor for any client
// of a Docker container, which all arrive from the bridge gateway — yet the
// dialog offered every viewer Update Now, and pressing it always answered
// "Update failed: update apply restricted to local origins", with no word of
// what would work. A viewer whose page is not on a loopback name
// (viewerIsAtTheHost, the cookie setup's test) now sees Update Now disabled
// and a line saying updates install from the host's own browser or the TUI,
// or in Docker by pulling the new image; the release notes and Skip stay.
//
// Like the other dialog suites this needs jsdom, and every test skips (not
// fails) when it is absent.
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

const release = {
  version: "9.9.9",
  tagName: "v9.9.9",
  releaseNotes: "notes",
  releaseNotesHtml: "<p>the notes</p>",
  publishedAt: "2026-09-01T00:00:00Z",
};

// What updateApplyOriginAllowed answers a peer that is not loopback.
const refused = () => harness.response({ status: 403, body: { error: "update apply restricted to local origins" } });

// Opens the dialog the way an operator does: Settings' Check Now (the manual
// check the Docker seed and README advertise), then the version badge.
async function openDialog(url, apply = refused) {
  const h = await harness.makeApp({
    url,
    initialState: { status: { version: "2.8.10" } },
    routes: {
      "POST /api/update/check": () => ({ currentVersion: "2.8.10", available: true, ...release }),
      "POST /api/update/apply": apply,
      "POST /api/update/dismiss": () => ({ success: true, skipped: "v9.9.9" }),
      "GET /api/update/release-notes": () => ({ tagName: "v2.8.10", releaseNotesHtml: "<p>current</p>" }),
    },
  });
  h.el("btn-check-update-now").click();
  await h.flush(); await h.flush();
  h.el("version-indicator").click();
  await h.flush();
  assert.equal(h.el("update-dialog")._open, true, "the update dialog did not open");
  return h;
}

const shown = (el) => el.style.display !== "none";

// MUTANTS: drop showUpdateDialog's `updateBtn.disabled = remote` (Update Now
// is enabled); drop the note's display toggle (it stays hidden); drop
// applyUpdate's viewerIsAtTheHost guard (a click on the disabled button —
// which jsdom's stand-in still dispatches — POSTs apply).
for (const url of ["http://192.168.1.50:774/", "http://172.17.0.1:774/"]) {
  test(`a viewer at ${url} is told where updates install from, and keeps the notes and Skip`, { skip }, async () => {
    const h = await openDialog(url);
    const btn = h.el("update-now-btn");
    const note = h.el("update-remote-note");
    assert.equal(btn.disabled, true, "Update Now is offered to a viewer the server never installs for");
    assert.ok(shown(note), "the line saying where updates install from is hidden");
    assert.match(note.textContent, /browser on the Moombox host itself/);
    assert.match(note.textContent, /terminal UI/);
    assert.match(note.textContent, /pull the new image/);
    assert.match(h.el("update-release-notes").innerHTML, /the notes/, "the release notes are not shown");
    assert.equal(h.el("update-dismiss-btn").disabled, false, "Skip is not available");

    btn.click();
    await h.flush(); await h.flush();
    assert.equal(h.http.matching("/api/update/apply", "POST").length, 0, "a disabled Update Now still asked the server");
    assert.ok(h.toasts().some((t) => /pull the new image/.test(t.textContent)), "the refusal does not say what would work");

    h.el("update-dismiss-btn").click();
    await h.flush(); await h.flush();
    assert.equal(h.http.matching("/api/update/dismiss", "POST").length, 1, "Skip did not reach the server");
  });
}

// MUTANT: invert the test (`viewerIsAtTheHost(...)` for `!viewerIsAtTheHost`)
// — the host's own browser gets the disabled button.
for (const url of ["http://localhost:774/", "http://127.0.0.1:774/", "http://[::1]:774/"]) {
  test(`the host's own browser at ${url} is offered Update Now`, { skip }, async () => {
    const h = await openDialog(url, () => ({ success: true }));
    assert.equal(h.el("update-now-btn").disabled, false, "Update Now is disabled for the host's own browser");
    assert.equal(shown(h.el("update-remote-note")), false, "the remote-viewer line shows for the host's own browser");
    h.el("update-now-btn").click();
    await h.flush(); await h.flush();
    assert.equal(h.http.matching("/api/update/apply", "POST").length, 1, "Update Now did not ask the server");
  });
}

// A page on a loopback name can still be refused: a browser on a Docker host
// at http://localhost reaches the container from the bridge gateway, which
// is not loopback to the server. The refusal names the same remedy.
//
// MUTANT: drop applyUpdate's 403 arm (the toast is the bare error).
test("a 403 from apply names where updates install from", { skip }, async () => {
  const h = await openDialog("http://localhost:774/");
  h.el("update-now-btn").click();
  await h.flush(); await h.flush();
  const toast = h.toasts().map((t) => t.textContent).find((t) => t.includes("Update failed"));
  assert.ok(toast, "no failure toast");
  assert.match(toast, /restricted to local origins/);
  assert.match(toast, /pull the new image/, "the 403 toast does not say what would work");
});

// The same dialog reused by Settings > View Release Notes installs nothing,
// so the line does not belong there, whoever is looking.
//
// MUTANT: drop the note's hide in the View Release Notes handler (a remote
// viewer who saw the update dialog first sees the line in the notes viewer).
test("the release-notes viewer does not carry the remote-viewer line", { skip }, async () => {
  const h = await openDialog("http://192.168.1.50:774/");
  assert.ok(shown(h.el("update-remote-note")));
  h.el("update-dialog").hide();
  h.el("btn-view-release-notes").click();
  await h.flush(); await h.flush();
  assert.equal(h.el("update-dialog").dataset.viewerMode, "true", "the notes viewer did not open");
  assert.equal(shown(h.el("update-remote-note")), false, "the notes viewer shows the remote-viewer line");
});
