import { safePlay } from "./utils.js";

/**
 * Multi-segment playback helper — shared between Player and Trimmer.
 *
 * Manages sequential segment sources, cumulative time offsets,
 * segment switching, and cross-segment seeking.
 */
export class SegmentPlayer {
  constructor() {
    this.segments = null;
    this.segOffsets = null;
    this.segIdx = 0;
    this.segTimeOffset = 0;
    this.totalDuration = 0;
    // A seek waiting for its part to load: { target, wasPlaying } (see
    // seekToGlobalTime).
    this._pendingSeek = null;
  }

  /** @returns {boolean} Whether multi-segment mode is active. */
  get active() {
    return this.segments !== null && this.segments.length > 0;
  }

  /**
   * Initialize multi-segment playback.
   * @param {string} jobId
   * @param {Array} segments — array of { segmentIndex, durationSeconds, quality, ... }
   */
  init(jobId, segments) {
    this.segments = segments;
    this.segIdx = 0;
    this.segTimeOffset = 0;

    let cumulative = 0;
    this.segOffsets = segments.map((seg) => {
      const entry = {
        ...seg,
        startOffset: cumulative,
        url: `/api/jobs/${jobId}/segments/${seg.segmentIndex}/video`,
      };
      cumulative += seg.durationSeconds || 0;
      return entry;
    });
    this.totalDuration = cumulative;
  }

  /** Reset all state. */
  reset() {
    if (this._seekAbort) { this._seekAbort.abort(); this._seekAbort = null; }
    this._pendingSeek = null;
    this.segments = null;
    this.segOffsets = null;
    this.segIdx = 0;
    this.segTimeOffset = 0;
    this.totalDuration = 0;
  }

  /**
   * Load a segment by index into the video element.
   * @param {number} idx
   * @param {HTMLVideoElement} video
   */
  loadSegment(idx, video) {
    if (!this.segOffsets || idx >= this.segOffsets.length) return;
    this.segIdx = idx;
    this.segTimeOffset = this.segOffsets[idx].startOffset;
    video.src = this.segOffsets[idx].url;
  }

  /**
   * Get the global playback time in seconds (segment offset + local time).
   * @param {HTMLVideoElement} video
   * @returns {number}
   */
  getGlobalTime(video) {
    if (!video) return 0;
    if (this.active) {
      // While a part loads the element reads about 0, not where the seek
      // meant to land; a relative seek (the arrow keys) counts from the target.
      if (this._pendingSeek) return this._pendingSeek.target;
      return this.segTimeOffset + video.currentTime;
    }
    return video.currentTime;
  }

  /**
   * Handle segment end — advance to next segment if available.
   * @param {HTMLVideoElement} video
   * @returns {boolean} true if advanced to next segment
   */
  onSegmentEnded(video) {
    if (!this.active) return false;
    if (this.segIdx + 1 < this.segments.length) {
      this.loadSegment(this.segIdx + 1, video);
      safePlay(video);
      return true;
    }
    return false;
  }

  /**
   * Seek to a global time (seconds), switching segments if needed.
   *
   * A seek into another part loads it and lands once it has loaded, resuming
   * playback if it was running. Loading pauses the element, so a second seek
   * made before that part has loaded (a held arrow key, two quick chapter
   * clicks) cannot read the play state or the position from the element:
   * both come from the pending record, and the second seek replaces the
   * first one's target on the same load instead of setting currentTime on an
   * element that has not loaded yet. A load that fails (the element's error
   * event) drops the pending record.
   * @param {number} globalSeconds
   * @param {HTMLVideoElement} video
   */
  seekToGlobalTime(globalSeconds, video) {
    if (!this.segOffsets || this.segOffsets.length === 0) return;

    // Unknown durations (ffprobe failed → durationSeconds 0) make the
    // global timeline degenerate: every seek would clamp to 0 and the
    // last-segment fallback below would jump there. Seek within the
    // current segment instead.
    if (!this.totalDuration || this.totalDuration <= 0) {
      video.currentTime = Math.max(0, globalSeconds);
      return;
    }

    // Clamp to valid range to prevent out-of-bounds seeks
    globalSeconds = Math.max(0, Math.min(this.totalDuration, globalSeconds));

    // Cancel any pending cross-segment seek listener from a previous call
    if (this._seekAbort) this._seekAbort.abort();

    for (let i = 0; i < this.segOffsets.length; i++) {
      const seg = this.segOffsets[i];
      const segEnd = seg.startOffset + (seg.durationSeconds || 0);
      if (globalSeconds < segEnd || i === this.segOffsets.length - 1) {
        if (i !== this.segIdx || this._pendingSeek) {
          const wasPlaying = !video.paused || !!this._pendingSeek?.wasPlaying;
          if (i !== this.segIdx) this.loadSegment(i, video);
          this._pendingSeek = { target: globalSeconds, wasPlaying };
          const ac = (this._seekAbort = new AbortController());
          video.addEventListener("loadeddata", () => {
            // The seek landed: take its error listener off with it, or one
            // would stay on the element for every seek that crossed a part.
            ac.abort();
            if (this._seekAbort === ac) this._seekAbort = null;
            this._pendingSeek = null;
            video.currentTime = globalSeconds - seg.startOffset;
            if (wasPlaying) safePlay(video);
          }, { once: true, signal: ac.signal });
          // A part that fails to load never fires loadeddata: the seek is
          // dropped, so a later seek in that part sets currentTime itself and
          // getGlobalTime reads the element again.
          video.addEventListener("error", () => {
            ac.abort();
            if (this._seekAbort === ac) {
              this._seekAbort = null;
              this._pendingSeek = null;
            }
          }, { once: true, signal: ac.signal });
        } else {
          video.currentTime = globalSeconds - seg.startOffset;
        }
        return;
      }
    }
  }
}
