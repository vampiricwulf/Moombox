// SegmentPlayer (web/public/modules/segments.js): seeking across the parts of
// a multi-part archive. No DOM needed: a fake media element is an EventTarget
// that pauses on a new src, as the HTML media load algorithm does.
import { test } from "node:test";
import assert from "node:assert/strict";
import { SegmentPlayer } from "../public/modules/segments.js";

function fakeVideo() {
  const v = new EventTarget();
  let src = "";
  v.paused = false;
  v.currentTime = 0;
  v.plays = 0;
  Object.defineProperty(v, "src", {
    get: () => src,
    // Loading a new source pauses the element and resets its position.
    set: (s) => { src = s; v.paused = true; v.currentTime = 0; },
  });
  v.play = () => { v.paused = false; v.plays++; return Promise.resolve(); };
  return v;
}

function twoParts(video) {
  const seg = new SegmentPlayer();
  seg.init("j1", [
    { segmentIndex: 0, durationSeconds: 60 },
    { segmentIndex: 1, durationSeconds: 60 },
  ]);
  seg.loadSegment(0, video);
  video.paused = false; // part 0 is playing
  return seg;
}

// A held arrow key: the second press lands before part 1 has loaded. It used
// to read the paused element, so playback stayed paused, and to count from
// about 0 instead of the first press's target.
//
// Mutant: compute wasPlaying from video.paused alone; set currentTime directly
// for a same-part target while a load is pending; drop the getGlobalTime
// pending branch.
test("a relative seek made while a cross-part seek loads keeps playing from the first target", () => {
  const video = fakeVideo();
  const seg = twoParts(video);
  video.currentTime = 57;

  seg.seekToGlobalTime(seg.getGlobalTime(video) + 5, video); // 62: part 1 at 2 s
  assert.ok(video.src.endsWith("/segments/1/video"));
  assert.equal(seg.getGlobalTime(video), 62, "the pending target, not the unloaded element");

  seg.seekToGlobalTime(seg.getGlobalTime(video) + 5, video); // 67: part 1 at 7 s
  video.dispatchEvent(new Event("loadeddata"));

  assert.equal(video.currentTime, 7, "landed where the last press meant");
  assert.equal(video.plays, 1, "playback resumed");
  assert.equal(seg.getGlobalTime(video), 67);
});

test("a second cross-part seek while paused does not start playback", () => {
  const video = fakeVideo();
  const seg = twoParts(video);
  video.paused = true;
  video.currentTime = 30;

  seg.seekToGlobalTime(70, video);
  seg.seekToGlobalTime(10, video); // back to part 0 before part 1 loaded
  assert.ok(video.src.endsWith("/segments/0/video"));
  video.dispatchEvent(new Event("loadeddata"));

  assert.equal(video.currentTime, 10);
  assert.equal(video.plays, 0);
});

// Mutant: reset leaves the pending record (getGlobalTime keeps answering 90).
test("reset drops a pending seek", () => {
  const video = fakeVideo();
  const seg = twoParts(video);
  seg.seekToGlobalTime(90, video);
  seg.reset();
  video.dispatchEvent(new Event("loadeddata"));
  assert.equal(video.plays, 0, "the aborted seek did not resume");

  seg.init("j1", [
    { segmentIndex: 0, durationSeconds: 60 },
    { segmentIndex: 1, durationSeconds: 60 },
  ]);
  seg.loadSegment(0, video);
  video.currentTime = 5;
  assert.equal(seg.getGlobalTime(video), 5, "the element, not the dropped target");
});

// A part that fails to load (a 404, a decode error) never fires loadeddata.
// The pending seek must not outlive it: getGlobalTime would keep answering
// the stale target, and every later seek in that part would wait on a load
// that never comes instead of setting currentTime.
//
// Mutant: drop the error listener.
test("a part that fails to load drops the pending seek", () => {
  const video = fakeVideo();
  const seg = twoParts(video);
  seg.seekToGlobalTime(90, video); // part 1 at 30 s
  video.dispatchEvent(new Event("error"));

  video.currentTime = 12;
  assert.equal(seg.getGlobalTime(video), 72, "the element again, not the stale target");
  seg.seekToGlobalTime(100, video); // within part 1
  assert.equal(video.currentTime, 40, "a seek in the failed part sets the position itself");
  video.dispatchEvent(new Event("loadeddata"));
  assert.equal(video.plays, 0, "no stale resume");
});
