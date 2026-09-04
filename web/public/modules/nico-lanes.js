/**
 * Lane allocator for right-to-left scrolling comments with a CONSTANT traverse
 * duration (the niconico model): every comment crosses stageWidth + ownWidth in
 * durationMs, so wider comments move faster and can overtake a narrower leader.
 * The traverse duration is the caller's to choose — this module is duration-agnostic.
 *
 * A lane is free for a follower of width wj at media time t only if
 *     t >= leader.spawnAt + durationMs * max(wi, wj) / (stageWidth + max(wi, wj)) + gapMs
 * This single bound covers both collision conditions — "the leader's tail has
 * cleared the spawn edge" (needs wi) and "the follower cannot catch the
 * leader before it exits" (needs wj) — because w/(W+w) increases with w.
 *
 * All times are MEDIA milliseconds passed in by the caller: lanes freeze while
 * the video is paused and scale with playbackRate for free. Pure; tested in
 * web/tests/nico-lanes.test.mjs.
 */
export class LaneAllocator {
  constructor(laneCount) {
    this.lanes = [];
    this.reset(laneCount);
  }

  /** Clear all occupancy; optionally change the lane count. */
  reset(laneCount = this.lanes.length) {
    this.lanes = Array.from({ length: Math.max(0, laneCount | 0) }, () => null);
  }

  get laneCount() {
    return this.lanes.length;
  }

  /** Media time at which lane `l` accepts a follower of `widthPx`; -Infinity when empty. */
  freeAt(l, widthPx, stageWidthPx, durationMs, gapMs) {
    const lead = this.lanes[l];
    if (!lead) return -Infinity;
    const w = Math.max(lead.width, widthPx);
    return lead.spawnAt + (durationMs * w) / (stageWidthPx + w) + gapMs;
  }

  /**
   * Occupy `lanesNeeded` consecutive lanes at `nowMs` and return the first
   * index, or -1 when no run of lanes is free yet.
   */
  allocate({ nowMs, widthPx, stageWidthPx, durationMs, lanesNeeded = 1, gapMs = 0 }) {
    const n = this.lanes.length;
    for (let l = 0; l + lanesNeeded <= n; l++) {
      let ok = true;
      for (let k = 0; k < lanesNeeded; k++) {
        if (nowMs < this.freeAt(l + k, widthPx, stageWidthPx, durationMs, gapMs)) {
          ok = false;
          break;
        }
      }
      if (ok) {
        for (let k = 0; k < lanesNeeded; k++) this.lanes[l + k] = { spawnAt: nowMs, width: widthPx };
        return l;
      }
    }
    return -1;
  }
}

/**
 * Index of the first message the overlay considers after a reset/seek at
 * media time `effectiveMs`: newer than `effectiveMs − latenessMs` AND within
 * the last `seedMax` messages at or before `effectiveMs`. The count cap keeps
 * a legacy archive with thousands of messages piled at one offset from
 * spending many ticks skipping them (research 2026-09-03, R10).
 * `indexAfter(messages, t)` = first index whose offsetMs > t (binary search).
 */
export function seedCursorIndex(messages, effectiveMs, latenessMs, seedMax, indexAfter) {
  const byTime = indexAfter(messages, effectiveMs - latenessMs);
  const byCount = indexAfter(messages, effectiveMs) - Math.max(1, seedMax | 0);
  return Math.max(byTime, byCount, 0);
}
