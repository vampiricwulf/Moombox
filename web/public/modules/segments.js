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
      video.play();
      return true;
    }
    return false;
  }

  /**
   * Seek to a global time (seconds), switching segments if needed.
   * @param {number} globalSeconds
   * @param {HTMLVideoElement} video
   */
  seekToGlobalTime(globalSeconds, video) {
    if (!this.segOffsets || this.segOffsets.length === 0) return;

    for (let i = 0; i < this.segOffsets.length; i++) {
      const seg = this.segOffsets[i];
      const segEnd = seg.startOffset + (seg.durationSeconds || 0);
      if (globalSeconds < segEnd || i === this.segOffsets.length - 1) {
        if (i !== this.segIdx) {
          const wasPlaying = !video.paused;
          this.loadSegment(i, video);
          video.addEventListener("loadeddata", () => {
            video.currentTime = globalSeconds - seg.startOffset;
            if (wasPlaying) video.play();
          }, { once: true });
        } else {
          video.currentTime = globalSeconds - seg.startOffset;
        }
        return;
      }
    }
  }
}
