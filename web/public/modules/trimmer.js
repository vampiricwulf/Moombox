/**
 * Trim Controller — interactive video-based trim UI
 */
import { formatTimestamp, isTypingInInput } from "./utils.js";
import { SegmentPlayer } from "./segments.js";

/**
 * Format seconds with sub-second precision: H:MM:SS.ms or M:SS.ms.
 * Omits the decimal part when it's exactly zero.
 */
function fmtPrecise(seconds) {
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = seconds % 60;
  const intS = Math.floor(s);
  const intStr = String(intS).padStart(2, "0");
  // Show up to 3 decimal places, strip trailing zeros
  let sStr;
  if (s === intS) {
    sStr = intStr;
  } else {
    const dec = (s - intS).toFixed(3).slice(1).replace(/0+$/, "");
    sStr = dec.length > 1 ? intStr + dec : intStr;
  }
  if (h > 0) return `${h}:${String(m).padStart(2, "0")}:${sStr}`;
  return `${m}:${sStr}`;
}

export class TrimController {
  constructor(app) {
    this.app = app;
    this.job = null;
    this.duration = 0;
    this.startMarker = 0;
    this.endMarker = 0;
    this._abort = null;
    this._dragging = null; // "start" | "end" | null
    this._dragCleanup = null; // cleanup fn for active drag listeners
    /** @type {Record<string, HTMLElement>} cached DOM refs, populated in open() */
    this._el = {};

    // Multi-segment playback
    this._seg = new SegmentPlayer();
  }

  /**
   * Open the trim dialog with video for the given job.
   */
  open(job) {
    this.job = job;
    this.duration = job.lengthSeconds || 0;
    this.startMarker = 0;
    this.endMarker = this.duration;

    // Reset multi-segment state
    this._seg.reset();

    // Cache all DOM refs once
    this._el = {
      dialog:       document.getElementById("trim-dialog"),
      details:      document.getElementById("details-dialog"),
      video:        document.getElementById("trim-video"),
      startInput:   document.getElementById("trim-start-input"),
      endInput:     document.getElementById("trim-end-input"),
      timeCurrent:  document.getElementById("trim-time-current"),
      timeTotal:    document.getElementById("trim-time-total"),
      track:        document.getElementById("trim-timeline-track"),
      range:        document.getElementById("trim-timeline-range"),
      playhead:     document.getElementById("trim-playhead"),
      handleStart:  document.getElementById("trim-handle-start"),
      handleEnd:    document.getElementById("trim-handle-end"),
      existingMarks:document.getElementById("trim-existing-marks"),
      rangeLabel:   document.getElementById("trim-range-label"),
      playBtn:      document.getElementById("trim-play-btn"),
      setStartBtn:  document.getElementById("trim-set-start-btn"),
      setEndBtn:    document.getElementById("trim-set-end-btn"),
      submitBtn:    document.getElementById("trim-submit-btn"),
    };

    const { dialog, details, video, startInput, endInput } = this._el;

    dialog.label = `Create Trim — ${job.title}`;

    // Multi-segment or single-file video source
    if (job.segments && job.segments.length > 0) {
      this._initMultiSegment(job.id, job.segments);
    } else {
      video.src = `/api/jobs/${job.id}/video`;
    }

    video.currentTime = 0;
    this._setPlayIcon(true);

    // Reset inputs
    startInput.value = fmtPrecise(0);
    endInput.value = this.duration > 0 ? fmtPrecise(this.duration) : "";

    // Show initial total time from job metadata (refined later by loadedmetadata for single-file)
    this._el.timeCurrent.textContent = formatTimestamp(0);
    this._el.timeTotal.textContent = this.duration > 0 ? formatTimestamp(this.duration) : "0:00";

    // Bind all events via AbortController
    if (this._abort) this._abort.abort();
    this._abort = new AbortController();
    const sig = this._abort.signal;

    // Video metadata — refine duration once the browser knows it (single-file only)
    video.addEventListener("loadedmetadata", () => {
      if (!this._seg.active && video.duration && isFinite(video.duration)) {
        this.duration = video.duration;
        this.endMarker = this.duration;
        endInput.value = fmtPrecise(this.duration);
        this._el.timeTotal.textContent = formatTimestamp(this.duration);
        this._renderExistingTrims();
      }
      this._updateTimeline();
    }, { signal: sig });

    // Playhead tracking
    video.addEventListener("timeupdate", () => {
      this._el.timeCurrent.textContent = formatTimestamp(this._getGlobalTime());
      this._updatePlayhead();
    }, { signal: sig });

    // Reset play icon when video ends or is paused externally
    video.addEventListener("pause", () => this._setPlayIcon(true), { signal: sig });
    video.addEventListener("play", () => this._setPlayIcon(false), { signal: sig });

    // Multi-segment: auto-advance on segment end
    video.addEventListener("ended", () => this._onSegmentEnded(), { signal: sig });

    // Click video to toggle play/pause
    video.addEventListener("click", () => this._togglePlay(), { signal: sig });

    // Timeline seek / handle drag
    this._el.track.addEventListener("pointerdown", (e) => this._onTrackPointerDown(e), { signal: sig });

    // Cancel drag on window blur to avoid stuck drag state
    window.addEventListener("blur", () => {
      if (this._dragCleanup) {
        this._dragCleanup();
        this._dragCleanup = null;
      }
    }, { signal: sig });

    // Control buttons
    this._el.playBtn.addEventListener("click", () => this._togglePlay(), { signal: sig });
    this._el.setStartBtn.addEventListener("click", () => this._setStartMarker(), { signal: sig });
    this._el.setEndBtn.addEventListener("click", () => this._setEndMarker(), { signal: sig });
    document.getElementById("trim-step-back-btn").addEventListener("click", () => this._frameStep(-1), { signal: sig });
    document.getElementById("trim-step-fwd-btn").addEventListener("click", () => this._frameStep(1), { signal: sig });
    document.getElementById("trim-goto-start-btn").addEventListener("click", () => {
      this._seekToGlobalTime(this.startMarker);
    }, { signal: sig });
    document.getElementById("trim-goto-end-btn").addEventListener("click", () => {
      this._seekToGlobalTime(this.endMarker);
    }, { signal: sig });

    // Keyboard shortcuts
    dialog.addEventListener("keydown", (e) => this._onKeyDown(e), { signal: sig });

    // Text input sync (inputs -> markers), clamped to opposite marker
    startInput.addEventListener("sl-change", () => {
      const val = this.app.parseTimeInput(startInput.value);
      if (val !== null && val >= 0 && val <= this.duration) {
        this.startMarker = Math.min(val, this.endMarker);
        startInput.value = fmtPrecise(this.startMarker);
        this._updateTimeline();
      }
    }, { signal: sig });

    endInput.addEventListener("sl-change", () => {
      const val = this.app.parseTimeInput(endInput.value);
      if (val !== null && val >= 0 && val <= this.duration) {
        this.endMarker = Math.max(val, this.startMarker);
        endInput.value = fmtPrecise(this.endMarker);
        this._updateTimeline();
      }
    }, { signal: sig });

    // Submit & cancel
    this._el.submitBtn.addEventListener("click", () => this._submit(), { signal: sig });
    document.getElementById("trim-cancel-btn").addEventListener("click", () => {
      dialog.hide();
    }, { signal: sig });

    // Initial render
    this._renderExistingTrims();
    this._updateTimeline();

    // Close details, open trim dialog
    details.hide();
    setTimeout(() => dialog.show(), 100);
  }

  /**
   * Clean up: pause video, remove source, abort all listeners.
   */
  destroy() {
    if (this._dragCleanup) {
      this._dragCleanup();
      this._dragCleanup = null;
    }
    const video = this._el.video;
    if (video) {
      video.pause();
      video.removeAttribute("src");
      video.load();
    }
    if (this._abort) {
      this._abort.abort();
      this._abort = null;
    }
    this._dragging = null;
    this._seg.reset();
    this._el = {};
    this.job = null;
  }

  // ===== Multi-segment playback =====

  _initMultiSegment(jobId, segments) {
    this._seg.init(jobId, segments);

    // Use total segment duration (more accurate than job.lengthSeconds which is truncated to int)
    this.duration = this._seg.totalDuration;
    this.endMarker = this.duration;

    // Load first segment
    this._seg.loadSegment(0, this._el.video);
  }

  _onSegmentEnded() {
    this._seg.onSegmentEnded(this._el.video);
  }

  /** Get current playback position in global seconds (accounting for segment offset). */
  _getGlobalTime() {
    return this._seg.getGlobalTime(this._el.video);
  }

  /** Seek to a global time (seconds), switching segments if needed. */
  _seekToGlobalTime(globalSeconds) {
    const video = this._el.video;
    if (!this._seg.active) {
      // Single-file: seek directly
      video.currentTime = Math.max(0, Math.min(this.duration, globalSeconds));
      return;
    }
    this._seg.seekToGlobalTime(globalSeconds, video);
  }

  // ===== Timeline rendering =====

  _updateTimeline() {
    if (!this.duration) return;

    const startPct = (this.startMarker / this.duration) * 100;
    const endPct = (this.endMarker / this.duration) * 100;

    // Range highlight
    this._el.range.style.left = `${startPct}%`;
    this._el.range.style.width = `${endPct - startPct}%`;

    // Handles
    this._el.handleStart.style.left = `${startPct}%`;
    this._el.handleEnd.style.left = `${endPct}%`;

    // Range label
    const dur = this.endMarker - this.startMarker;
    this._el.rangeLabel.textContent =
      `${fmtPrecise(this.startMarker)} — ${fmtPrecise(this.endMarker)} (${fmtPrecise(Math.max(0, dur))})`;

    this._updatePlayhead();
  }

  _updatePlayhead() {
    if (!this._el.video || !this.duration) return;
    const pct = Math.max(0, Math.min(100, (this._getGlobalTime() / this.duration) * 100));
    this._el.playhead.style.left = `${pct}%`;
  }

  _renderExistingTrims() {
    const container = this._el.existingMarks;
    if (!container) return;
    container.innerHTML = "";
    if (!this.job?.trims?.length || !this.duration) return;

    for (const trim of this.job.trims) {
      const mark = document.createElement("div");
      mark.className = "trim-existing-mark";
      const left = (trim.startTime / this.duration) * 100;
      const width = ((trim.endTime - trim.startTime) / this.duration) * 100;
      mark.style.left = `${left}%`;
      mark.style.width = `${Math.max(0.5, width)}%`;
      mark.title = `Existing: ${fmtPrecise(trim.startTime)} — ${fmtPrecise(trim.endTime)}`;
      container.appendChild(mark);
    }
  }

  // ===== Track pointer handling =====

  _onTrackPointerDown(e) {
    if (!this.duration) return;
    const track = this._el.track;
    const rect = track.getBoundingClientRect();
    const x = e.clientX - rect.left;

    // Check proximity to handles — prefer whichever is closer
    const startPx = (this.startMarker / this.duration) * rect.width;
    const endPx = (this.endMarker / this.duration) * rect.width;
    const distStart = Math.abs(x - startPx);
    const distEnd = Math.abs(x - endPx);
    const threshold = 12;

    if (distStart < threshold || distEnd < threshold) {
      // Pick the closer handle; ties go to end (more common intent)
      const handle = distStart < distEnd ? "start" : "end";
      this._startDrag(handle, e, track);
    } else {
      // Seek to clicked position (global time)
      const pct = x / rect.width;
      this._seekToGlobalTime(Math.max(0, Math.min(this.duration, pct * this.duration)));
    }
  }

  _startDrag(handle, e, track) {
    this._dragging = handle;
    track.setPointerCapture(e.pointerId);

    // Per-drag AbortController so pointermove/up/cancel are removed together.
    // This nests inside the outer this._abort (set in open()) — if the dialog
    // is destroyed mid-drag, the blur handler calls _dragCleanup, and
    // destroy()'s outer abort also aborts this signal defensively.
    const dragAbort = new AbortController();
    const sig = dragAbort.signal;

    const onMove = (ev) => {
      const rect = track.getBoundingClientRect();
      const x = ev.clientX - rect.left;
      const pct = Math.max(0, Math.min(1, x / rect.width));
      const time = pct * this.duration;

      if (this._dragging === "start") {
        this.startMarker = Math.max(0, Math.min(time, this.endMarker - 1));
        this._el.startInput.value = fmtPrecise(this.startMarker);
      } else {
        this.endMarker = Math.min(this.duration, Math.max(time, this.startMarker + 1));
        this._el.endInput.value = fmtPrecise(this.endMarker);
      }
      this._updateTimeline();
    };

    const onUp = () => {
      this._dragging = null;
      this._dragCleanup = null;
      dragAbort.abort();
    };

    // Store cleanup so destroy() / window blur can remove listeners mid-drag
    this._dragCleanup = onUp;

    track.addEventListener("pointermove", onMove, { signal: sig });
    track.addEventListener("pointerup", onUp, { signal: sig });
    track.addEventListener("pointercancel", onUp, { signal: sig });
  }

  // ===== Transport controls =====

  _togglePlay() {
    const video = this._el.video;
    if (video.paused) {
      video.play();
    } else {
      video.pause();
    }
  }

  /** Update the play button icon. @param {boolean} paused */
  _setPlayIcon(paused) {
    const icon = this._el.playBtn?.querySelector("sl-icon");
    if (icon) icon.name = paused ? "play-fill" : "pause-fill";
  }

  _setStartMarker() {
    this.startMarker = Math.min(this._getGlobalTime(), this.endMarker);
    this._el.startInput.value = fmtPrecise(this.startMarker);
    this._updateTimeline();
    this._flashButton(this._el.setStartBtn);
  }

  _setEndMarker() {
    this.endMarker = Math.max(this._getGlobalTime(), this.startMarker);
    this._el.endInput.value = fmtPrecise(this.endMarker);
    this._updateTimeline();
    this._flashButton(this._el.setEndBtn);
  }

  _frameStep(direction) {
    const video = this._el.video;
    if (video && !video.paused) video.pause();
    // Approximate frame duration at ~30fps
    const step = 1 / 30;
    const target = this._getGlobalTime() + step * direction;
    this._seekToGlobalTime(Math.max(0, Math.min(this.duration, target)));
  }

  /** @param {HTMLElement} btn */
  _flashButton(btn) {
    if (!btn) return;
    btn.classList.add("trim-btn-flash");
    setTimeout(() => btn?.classList.remove("trim-btn-flash"), 300);
  }

  // ===== Keyboard shortcuts =====

  _onKeyDown(e) {
    // Ignore if focus is inside a text input (composedPath handles Shoelace shadow DOM)
    if (isTypingInInput(e)) return;

    // Don't intercept Space/Enter on buttons — let them activate normally
    if ((e.key === " " || e.key === "Enter") && e.composedPath().some(el =>
      el instanceof HTMLElement && (el.tagName === "SL-BUTTON" || el.tagName === "SL-ICON-BUTTON" || el.tagName === "BUTTON")
    )) return;

    // Stop the keydown before any bubbling listener (the app-level document
    // keydown for shortcuts, the player document keydown for transport)
    // can also act on it. Anything we handle here is trim-local.
    const handle = () => { e.preventDefault(); e.stopPropagation(); };

    switch (e.key) {
      case " ":
        handle();
        this._togglePlay();
        break;
      case "i":
      case "I":
        handle();
        this._setStartMarker();
        break;
      case "o":
      case "O":
        handle();
        this._setEndMarker();
        break;
      case "ArrowLeft": {
        handle();
        const delta = e.shiftKey ? 30 : 5;
        this._seekToGlobalTime(Math.max(0, this._getGlobalTime() - delta));
        break;
      }
      case "ArrowRight": {
        handle();
        const delta = e.shiftKey ? 30 : 5;
        this._seekToGlobalTime(Math.min(this.duration, this._getGlobalTime() + delta));
        break;
      }
      case ",":
        handle();
        this._frameStep(-1);
        break;
      case ".":
        handle();
        this._frameStep(1);
        break;
    }
  }

  // ===== Submit =====

  async _submit() {
    const startTime = this.startMarker;
    const endTime = this.endMarker;

    if (endTime <= startTime) {
      this.app.showToast("End time must be after start time", "warning");
      return;
    }

    if (endTime - startTime < 1) {
      this.app.showToast("Trim must be at least 1 second long", "warning");
      return;
    }

    this._el.submitBtn.loading = true;
    this._el.submitBtn.disabled = true;
    try {
      // Restore selectedJobId so _refreshJobDetails can update the details content.
      // It was cleared when the details dialog was hidden to open the trim dialog.
      // Set it only during the async operation — if createTrim fails, clear it to
      // avoid stale selectedJobId pointing at a job whose details dialog is closed.
      this.app.selectedJobId = this.job.id;
      await this.app.createTrim(this.job.id, startTime, endTime);
      // Save ref before hide — destroy() clears _el on sl-after-hide,
      // which fires before the timeout under prefers-reduced-motion.
      const detailsDlg = this._el.details;
      this._el.dialog.hide();
      // Reopen details dialog to show updated trims
      setTimeout(() => detailsDlg?.show(), 100);
    } catch (error) {
      // Error already shown by createTrim(). Clear selectedJobId since the details
      // dialog is still closed — leaving it set would cause jobs_update WebSocket
      // messages to call updateJobDetails() on empty content.
      this.app.selectedJobId = null;
    } finally {
      if (this._el?.submitBtn) {
        this._el.submitBtn.loading = false;
        this._el.submitBtn.disabled = false;
      }
    }
  }
}
