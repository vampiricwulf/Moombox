// The unified filter bar (web/public/modules/filter-bar.js) as an operator
// types into it: real `input` events, the 200 ms debounce driven by the
// harness clock, Enter through a real keydown.
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

const JOBS = [
  { id: "1", status: "Live", title: "Karaoke night", channelName: "Mori", platform: "youtube", videoId: "aaa" },
  { id: "2", status: "Finished", title: "Zatsudan", channelName: "Mori", platform: "youtube", videoId: "bbb" },
  { id: "3", status: "Upcoming", title: "Minecraft", channelName: "Shachi Too", platform: "twitch", videoId: "ccc" },
];

async function setup() {
  const h = await harness.makeApp();
  h.app.jobs = JOBS.map((j) => ({ ...j }));
  h.app.renderJobs();
  const container = h.el("tasks-filter");
  const input = container.querySelector(".unified-filter-input");
  input.focus();
  const chips = () => [...container.querySelector(".unified-filter-chips").children].map((c) => c.textContent);
  const tokens = () => h.app.filterBar.tasksFilterTokens.map((t) => JSON.parse(JSON.stringify(t)));
  const shown = () => h.app.filterBar.getFilteredJobs().map((j) => j.id);
  /** One character at a time, `gap` ms apart — brisk typing is under the debounce. */
  const type = (text, gap = 80) => {
    for (const ch of text) {
      input.value += ch;
      input.dispatchEvent(new h.window.Event("input", { bubbles: true }));
      h.advance(gap);
    }
  };
  /** The operator stops typing for longer than the debounce. */
  const pause = () => h.advance(300);
  const enter = () =>
    input.dispatchEvent(new h.window.KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true }));
  /** Select-all + Delete, or the box's text edited to something else. */
  const replace = (text) => {
    input.value = text;
    input.dispatchEvent(new h.window.Event("input", { bubbles: true }));
  };
  return { h, input, chips, tokens, shown, type, pause, enter, replace };
}

// A pause mid-token committed the half-typed token as a chip on the debounce
// and cleared the box: `status:` became an empty chip and the `live` typed
// after it a text term, so the Live job vanished — where the TUI's / box,
// which re-parses the whole text, showed it. A pause in `channel:mo` likewise
// chipped an exact-match "mo" that matched nothing.
//
// Mutants: syncTokens' `open` always "" (the token being typed is settled on
// the debounce too) — the first pause chips "" and the second "mo"; chippable
// without its halfTyped check on a term — Enter on a bare `status:` chips "";
// without it on an OR group's terms — Enter chips "live | "; Enter syncing
// without `commit` — the open `status:live` stays in the box, unchipped.
test("a pause mid-token leaves the token being typed in the box", { skip }, async () => {
  const f = await setup();
  f.type("status:");
  f.pause();
  assert.deepEqual(f.chips(), [], "a half-typed token is not a chip");
  assert.equal(f.input.value, "status:", "and the box keeps what was typed");
  f.type("live");
  f.pause();
  assert.deepEqual(f.shown(), ["1"], "the box reads status:live, so the Live job shows — as in the TUI");
  assert.equal(f.input.value, "status:live");
  assert.deepEqual(f.chips(), [], "the token is still open: nothing has closed it yet");
  f.enter();
  assert.deepEqual(f.chips(), ["live"], "Enter commits the token being typed");
  assert.equal(f.input.value, "");
  assert.deepEqual(f.shown(), ["1"]);

  const g = await setup();
  g.type("channel:mo");
  g.pause();
  assert.deepEqual(g.chips(), [], "a partial channel name is not committed as an exact-match chip");
  g.type("ri");
  g.pause();
  assert.deepEqual(g.shown(), ["1", "2"], "Mori's two jobs");

  // A filter key with no value yet is never a chip, even on Enter: it stays
  // in the box, applied as typed, for the operator to finish or delete.
  const k = await setup();
  k.type("status:");
  k.enter();
  assert.deepEqual(k.chips(), []);
  assert.equal(k.input.value, "status:");
  k.type("finished");
  k.enter();
  assert.deepEqual(k.chips(), ["Finished"]);
  assert.deepEqual(k.shown(), ["2"]);

  // Nor is an OR group with such a term in it.
  const o = await setup();
  o.type("status:live|status:");
  o.enter();
  assert.deepEqual(o.chips(), []);
  assert.equal(o.input.value, "status:live|status:");
  o.type("upcoming");
  o.enter();
  assert.deepEqual(o.chips(), ["live | upcoming"]);
  assert.deepEqual(o.shown(), ["1", "3"]);
});

// The debounce still chips a token once a space has closed it, and the box
// keeps the space after the free text left in it.
//
// Mutants: the `rest.push("")` dropped — the box reads "night" and the next word
// typed joins it ("nightkaraoke"); the open token re-serialized rather than kept as
// typed — the box reads `channel:'"Shachi'` mid-quote.
test("a token closed by a space chips on the debounce; the open one stays as typed", { skip }, async () => {
  const f = await setup();
  f.type("night status:live ");
  f.pause();
  assert.deepEqual(f.chips(), ["live"], "a closed structured token becomes a chip without Enter");
  assert.equal(f.input.value, "night ", "the free text stays, followed by the space that was typed");
  f.type("karaoke");
  f.pause();
  assert.deepEqual(
    f.tokens().filter((t) => t.type === "text").map((t) => t.value),
    ["night", "karaoke"],
    "the next word is its own term, not joined to the last",
  );
  assert.deepEqual(f.shown(), ["1"]);

  // An open quote keeps its token open past spaces, and the box keeps it
  // exactly as typed while the closed token before it chips.
  const g = await setup();
  g.type('status:upcoming channel:"Shachi');
  g.pause();
  assert.deepEqual(g.chips(), ["upcoming"]);
  assert.equal(g.input.value, 'channel:"Shachi');
  g.type(' Too"');
  g.enter();
  assert.deepEqual(g.chips(), ["upcoming", "Shachi Too"]);
  assert.deepEqual(g.shown(), ["3"]);
});

// A text-only OR group stays in the box, but every later sync carried it over
// as a chip because its type is "or", not "text": clearing the box left the
// list filtered by a group nothing showed, and editing it AND-ed the old group
// with the new one.
//
// Mutants: isChip back to `t.type !== "text"`, or the box's tokens no longer
// marked `typed` — the cleared box keeps the karaoke|zatsudan filter, and the
// edit keeps it as well; chippable's OR arm without its structured-term check
// — Enter chips the text group.
test("a text-only OR query lives in the box: clearing or editing the box replaces it", { skip }, async () => {
  const f = await setup();
  f.type("karaoke|zatsudan");
  f.enter();
  assert.deepEqual(f.chips(), [], "a pure-text OR group is free text, not a chip");
  assert.equal(f.input.value, "karaoke|zatsudan");
  assert.deepEqual(f.shown(), ["1", "2"]);

  f.replace("minecraft|zatsudan");
  f.pause();
  assert.deepEqual(f.shown(), ["2", "3"], "the box reads minecraft|zatsudan, and nothing else filters");

  f.replace("");
  f.pause();
  assert.deepEqual(f.tokens(), [], "an empty box with no chip leaves no filter");
  assert.deepEqual(f.shown(), ["1", "2", "3"]);

  // The same for a structured token the box still holds open: deleting the
  // text deletes the filter.
  f.type("status:li");
  f.pause();
  f.replace("");
  f.pause();
  assert.deepEqual(f.tokens(), []);
  assert.deepEqual(f.shown(), ["1", "2", "3"]);
});
