/**
 * Pure geometry for the niconico overlay: where the picture actually is inside
 * the <video> box, and how many text rows fit. No DOM here — player.js reads
 * the element sizes and writes the styles; this file only does the arithmetic,
 * so it can be pinned without jsdom.
 */

/**
 * Centred-fit stage. The <video> paints its content centred inside its box at
 * the largest scale that fits, so a portrait video in a landscape box gets
 * pillarbox bars (and vice versa). Before `loadedmetadata` the intrinsic size
 * is unknown (0) and the element box is the best available stage.
 * @returns {{left:number, top:number, w:number, h:number}|null} null when the
 *   result would be zero-sized — the caller keeps the last good geometry.
 */
export function letterboxStage({ boxW, boxH, offsetLeft, offsetTop, videoW, videoH }) {
  let w = boxW, h = boxH, left = offsetLeft, top = offsetTop;
  if (videoW > 0 && videoH > 0 && boxW > 0 && boxH > 0) {
    const scale = Math.min(boxW / videoW, boxH / videoH);
    w = Math.round(videoW * scale);
    h = Math.round(videoH * scale);
    left += Math.round((boxW - w) / 2);
    top += Math.round((boxH - h) / 2);
  }
  if (w <= 0 || h <= 0) return null;
  return { left, top, w, h };
}

/** Text rows that fit a stage of height `h` at one line box of `rowH`. */
export function rowsFor(h, rowH) {
  return Math.max(1, Math.floor(h / rowH));
}

/** True when `geo` already describes a stage of exactly this size and row count. */
export function sameStage(geo, w, h, rows) {
  return !!geo && geo.width === w && geo.height === h && geo.rows === rows;
}

/** The geometry record player.js installs; `version` counts installs. */
export function nextGeometry(prev, w, h, rows) {
  return { width: w, height: h, laneHeight: h / rows, rows, version: (prev?.version || 0) + 1 };
}

// WALL-CLOCK milliseconds (not media time): how long the stage box must hold
// still before a changed geometry is committed. A window drag or an animated
// fullscreen transition is a continuous stream of REAL changes, and committing
// each one would clear the stage every frame.
export const NICO_GEO_SETTLE_MS = 120;
