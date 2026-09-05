/**
 * The niconico overlay's scheduler: the cursor, the anchor, the pending list
 * and the drop count. Pure — no DOM here. player.js owns the elements, the
 * measurements, the Web Animations and the "+N not shown" pill and hands them
 * in as the `prepare` / `place` / `discard` hooks, so the state machine that
 * decides WHAT is shown WHEN can be pinned without jsdom
 * (web/tests/nico-scheduler.test.mjs).
 */

// Niconico overlay engine tuning. All times are MEDIA milliseconds, so the
// overlay freezes with the video and scales with playbackRate for free.
//
// A message ENTERS at the right edge NICO_LEAD_MS before its timestamp, so at
// its timestamp it is NICO_LEAD_MS / NICO_DURATION_MS = a quarter of the way
// across — niconico's own model, and what makes messages slide in from the edge
// instead of appearing mid-stage. `entryMs = msg.offsetMs − NICO_LEAD_MS` is the
// clock the rest of the engine works in: consumption, the lateness bound, the
// lane allocator and the seed all measure from it (R29).
export const NICO_DURATION_MS = 4000;      // niconico's traverse time (owner decision D4)
export const NICO_LEAD_MS = 1000;          // niconico: a comment starts moving 1 s before its timestamp (100 vpos)
export const NICO_TICK_AHEAD_MS = 300;     // consume up to one timeupdate interval early; the WAAPI delay holds the entry instant
export const NICO_MAX_LATENESS_MS = 2000;  // a message not placed within 2 s of ENTERING (1 s past its timestamp) is dropped and counted
export const NICO_LANE_GAP_MS = 150;       // spacing buffer between consecutive occupants of a lane
export const NICO_MAX_PER_TICK = 20;       // DOM work cap for NEW messages per timeupdate tick
export const NICO_SEED_MAX_FALLBACK = 30;  // seed cap when the row count is unknown

export class NicoScheduler {
  /**
   * @param {object} deps
   * @param {{laneCount: number, reset: Function, allocate: Function}} deps.lanes the LaneAllocator
   * @param {Function} deps.indexAfter first index whose offsetMs is greater than t
   * @param {Function} deps.seedCursorIndex the seed rule (nico-lanes.js)
   */
  constructor({
    lanes, indexAfter, seedCursorIndex,
    leadMs = NICO_LEAD_MS, maxLatenessMs = NICO_MAX_LATENESS_MS, tickAheadMs = NICO_TICK_AHEAD_MS,
    maxPerTick = NICO_MAX_PER_TICK, seedMaxFallback = NICO_SEED_MAX_FALLBACK,
  }) {
    this.lanes = lanes;
    this.indexAfter = indexAfter;
    this.seedCursorIndex = seedCursorIndex;
    this.leadMs = leadMs;
    this.maxLatenessMs = maxLatenessMs;
    this.tickAheadMs = tickAheadMs;
    this.maxPerTick = maxPerTick;
    this.seedMaxFallback = seedMaxFallback;
    /** Index of the next message to consider; -1 = not anchored yet */
    this.cursor = -1;
    /** Effective time of the last reset; only newer messages count as drops */
    this.anchorMs = -Infinity;
    /**
     * Entries that found no lane yet, in offset order. Each caches whatever the
     * caller's `prepare` built for it, so a retry costs no work.
     * @type {Array<{msg: object}>}
     */
    this.pending = [];
    this.dropped = 0;
  }

  /**
   * Anchor the overlay cursor at `effectiveMs`: the next tick considers only a
   * short seed of "chat that was already flying" (it ENTERED within the last
   * NICO_MAX_LATENESS_MS, and at most two screens' worth of rows), never the
   * whole pre-show backlog. The horizon is handed to `seedCursorIndex` shifted
   * by NICO_LEAD_MS — the seed works in message time while the engine works in
   * entry time, and the shift is what keeps "entered within the last 2 s" and
   * "the last 2×rows to have entered" meaning what they say. Also drops any
   * deferred messages and frees every lane — the caller has cleared the overlay.
   * @param {Array<object>} messages
   * @param {number} effectiveMs
   */
  anchor(messages, effectiveMs) {
    const rows = this.lanes.laneCount || this.seedMaxFallback / 2;
    this.cursor = this.seedCursorIndex(
      messages, effectiveMs + this.leadMs, this.maxLatenessMs, 2 * rows, this.indexAfter,
    );
    this.pending = [];
    this.anchorMs = effectiveMs;
    this.lanes.reset();
  }

  /**
   * Forget the cursor and the deferred entries: the next tick re-anchors at the
   * time it runs. The caller clears the overlay first — nothing here touches the
   * lanes, which must not be freed under elements that are still flying.
   */
  unanchor() {
    this.cursor = -1;
    this.pending = [];
  }

  /** Zero the drop counter (job switch / player teardown). */
  resetDropCount() {
    this.dropped = 0;
  }

  /**
   * Count a message the overlay could not show. A message that had already
   * ENTERED at the last anchor is a seed-window skip, not a drop — the viewer
   * landed in the middle of its flight — so the comparison is on entry time,
   * not on the timestamp a whole NICO_LEAD_MS later.
   */
  countDrop(msg) {
    if (msg.offsetMs - this.leadMs > this.anchorMs) this.dropped++;
  }

  /** The instant `msg` enters at the right edge — the engine's clock (R29). */
  entryMs(msg) {
    return msg.offsetMs - this.leadMs;
  }

  /** True when `msg` entered more than NICO_MAX_LATENESS_MS ago. */
  tooLate(msg, effectiveMs) {
    return effectiveMs - this.entryMs(msg) > this.maxLatenessMs;
  }

  /**
   * Advance to `effectiveMs`: retry what is deferred, then consume what has
   * entered.
   * @param {Array<object>} messages sorted by offsetMs
   * @param {number} effectiveMs media time plus the user's chat offset
   * @param {{prepare: (msg: object) => (object|null),
   *          place: (entry: object, effectiveMs: number, retry: boolean) => boolean,
   *          discard: (entry: object) => void}} hooks
   * @returns {{placed: number, deferred: number, skipped: number}}
   */
  tick(messages, effectiveMs, { prepare, place, discard }) {
    let placed = 0;
    let deferred = 0;
    let skipped = 0;

    // 1. Deferred entries first (oldest first) — no head-of-line blocking: an
    //    entry that still finds no lane stays pending, one that is now too late
    //    is dropped (and counted when it is newer than the anchor), and the
    //    ones behind it are still tried this tick. A retry is placed at the
    //    CURRENT time (retry mode, see _placeEntry) and reuses the element and
    //    measurements taken at first sight — no rebuild, no re-measure.
    const stillPending = [];
    for (const entry of this.pending) {
      if (this.tooLate(entry.msg, effectiveMs)) {
        this.countDrop(entry.msg);
        discard(entry); // the entry (and its detached element) is released
        skipped++;
        continue;
      }
      if (place(entry, effectiveMs, true)) placed++;
      else stillPending.push(entry);
    }
    this.pending = stillPending;

    // 2. New messages up to the per-tick cap; the cursor ALWAYS advances.
    //    The cap bounds DOM WORK, not the walk: skipping a too-late message
    //    builds nothing, so it must not consume a slot — otherwise a backlog
    //    would drain at only NICO_MAX_PER_TICK per tick, leaving the overlay
    //    dead for seconds. Skips are free, so any backlog clears in one tick.
    //    A message is taken as soon as it enters, plus NICO_TICK_AHEAD_MS: ticks
    //    arrive at ~4 Hz and an entry instant almost never lands on one, so the
    //    engine takes it up to a tick early and lets the animation's `delay`
    //    hold the exact instant (see _placeEntry). Consuming late instead would
    //    make the tick rate visible as a start already inside the stage.
    let work = 0;
    while (this.cursor < messages.length
           && this.entryMs(messages[this.cursor]) <= effectiveMs + this.tickAheadMs) {
      const msg = messages[this.cursor];
      if (this.tooLate(msg, effectiveMs)) {
        this.cursor++;
        this.countDrop(msg);
        skipped++;
        continue;
      }
      if (work++ >= this.maxPerTick) break; // cursor stays put — retried next tick
      this.cursor++;
      const entry = prepare(msg);
      if (!entry) continue; // nothing renderable (system-only message)
      if (place(entry, effectiveMs, false)) placed++;
      else {
        this.pending.push(entry);
        deferred++;
      }
    }

    return { placed, deferred, skipped };
  }
}
