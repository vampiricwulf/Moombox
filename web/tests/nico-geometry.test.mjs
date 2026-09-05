// Tests for web/public/modules/nico-geometry.js
import { test } from "node:test";
import assert from "node:assert/strict";
import { letterboxStage, rowsFor, sameStage, nextGeometry } from "../public/modules/nico-geometry.js";

test("a 16:9 video in a square box is letterboxed top and bottom", () => {
  const s = letterboxStage({ boxW: 400, boxH: 400, offsetLeft: 10, offsetTop: 20, videoW: 1920, videoH: 1080 });
  assert.deepEqual(s, { left: 10, top: 20 + Math.round((400 - 225) / 2), w: 400, h: 225 });
});

test("a portrait video in a landscape box is pillarboxed left and right", () => {
  const s = letterboxStage({ boxW: 800, boxH: 450, offsetLeft: 0, offsetTop: 0, videoW: 1080, videoH: 1920 });
  const w = Math.round(1080 * (450 / 1920));
  assert.deepEqual(s, { left: Math.round((800 - w) / 2), top: 0, w, h: 450 });
});

test("before loadedmetadata the element box is the stage", () => {
  assert.deepEqual(letterboxStage({ boxW: 640, boxH: 360, offsetLeft: 5, offsetTop: 6, videoW: 0, videoH: 0 }),
    { left: 5, top: 6, w: 640, h: 360 });
});

test("a zero-sized result is null, never a zero box", () => {
  assert.equal(letterboxStage({ boxW: 0, boxH: 360, offsetLeft: 0, offsetTop: 0, videoW: 1920, videoH: 1080 }), null);
  assert.equal(letterboxStage({ boxW: 0, boxH: 0, offsetLeft: 0, offsetTop: 0, videoW: 0, videoH: 0 }), null);
});

test("rows floor the stage height by the row height and never drop below one", () => {
  assert.equal(rowsFor(450, 24), 18);
  assert.equal(rowsFor(10, 24), 1);
});

test("sameStage compares width, height and rows; nextGeometry bumps the version", () => {
  const g1 = nextGeometry(null, 800, 450, 18);
  assert.deepEqual(g1, { width: 800, height: 450, laneHeight: 25, rows: 18, version: 1 });
  assert.equal(sameStage(g1, 800, 450, 18), true);
  assert.equal(sameStage(g1, 800, 450, 17), false);
  assert.equal(sameStage(null, 800, 450, 18), false);
  assert.equal(nextGeometry(g1, 640, 360, 15).version, 2);
});
