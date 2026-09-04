/**
 * Player Controller — Video player + chat replay
 */
import { formatMsToTime, formatTimestamp, isTypingInInput, safePlay } from "./utils.js";
import { SegmentPlayer } from "./segments.js";
import {
  normalizeOffsetMs,
  computeChatBiasMs,
  mergePartChats,
  indexAfter,
  partitionChatByVideo,
} from "./chat-timeline.js";
import { LaneAllocator, seedCursorIndex } from "./nico-lanes.js";

const ANNOUNCEMENT_COLORS = new Set(["primary", "blue", "green", "orange", "purple"]);

// Niconico overlay engine tuning. All times are MEDIA milliseconds, so the
// overlay freezes with the video and scales with playbackRate for free.
const NICO_DURATION_MS = 4000;      // niconico's traverse time (owner decision D4)
const NICO_MAX_LATENESS_MS = 2000;  // a message not placed within 2 s of its time is dropped and counted
const NICO_LANE_GAP_MS = 150;       // spacing buffer between consecutive occupants of a lane
const NICO_MAX_PER_TICK = 20;       // DOM work cap for NEW messages per timeupdate tick
const NICO_SEED_MAX_FALLBACK = 30;  // seed cap when the row count is unknown

// WALL-CLOCK milliseconds (not media time): how long the stage box must hold
// still before a changed geometry is committed. A window drag or an animated
// fullscreen transition is a continuous stream of REAL changes, and committing
// each one would clear the stage every frame.
const NICO_GEO_SETTLE_MS = 120;

function announcementColorClass(color) {
  return ANNOUNCEMENT_COLORS.has(color) ? color : "primary";
}

export class PlayerController {
  constructor(app) {
    this.app = app;
    this.playerJob = null;
    this.playerChatData = null;
    this.playerChatMessages = [];
    this.playerAutoScroll = true;
    this.playerScrollLock = false;
    this.playerActiveChatIndex = 0;
    /**
     * Where the chat sits relative to the recording: waiting-room messages
     * before it, messages after it ran out. Recomputed whenever the messages
     * or the known video duration change; null until a chat is loaded.
     * @type {{preCount: number, firstLiveIndex: number, postCount: number, firstPostIndex: number}|null}
     */
    this._chatParts = null;
    this.nicoEnabled = true;
    /** @type {LaneAllocator} the row count is re-derived by _updateNicoGeometry */
    this._lanes = new LaneAllocator(15);
    /**
     * The COMMITTED overlay stage: the video's rendered rect and the row grid
     * measured from it. Undefined until _updateNicoGeometry has seen a visible
     * overlay. During a resize the overlay's own box is already ahead of this
     * (see _updateNicoGeometry). `version` is bumped on every commit; it is
     * reserved for stale-geometry checks — nothing reads it yet.
     * @type {{width: number, height: number, laneHeight: number, rows: number, version: number}|undefined}
     */
    this._nicoGeo = undefined;
    /** @type {ReturnType<typeof setTimeout>|null} pending geometry commit (NICO_GEO_SETTLE_MS) */
    this._nicoGeoSettle = null;
    /** Index of the next message to consider; -1 = not anchored yet */
    this.nicoCursor = -1;
    /**
     * Entries that found no lane yet, in offset order. Each caches the built,
     * detached element and its measurements so a retry costs no DOM work.
     * @type {Array<{msg: object, el: HTMLElement, w: number, h: number}>}
     */
    this._nicoPending = [];
    /** Effective time of the last reset; only newer messages count as drops */
    this._nicoAnchorMs = -Infinity;
    /** @type {Set<Animation>} every in-flight overlay animation */
    this._nicoAnims = new Set();
    this.nicoDropped = 0;
    this._nicoDroppedShown = 0;
    this._nicoDropPillTimer = null;
    this.playerCustomOffsetMs = 0;
    this.playerInitialized = false;
    /** @type {Map<string, string>} code → URL for 3rd-party Twitch emotes */
    this.twitchEmoteMap = new Map();

    // Multi-segment playback
    this._seg = new SegmentPlayer();
    /** Monotonic counter to detect stale responses from rapid job switching */
    this._selectionSeq = 0;

    // Watch state tracking
    this._watchSaveInterval = null;
    this._watchedTriggered = false;
    this._onPauseSave = null;
    this._onSeekedWatch = null;
    this._onBeforeUnload = null;

    // Abort controller for the resume-dialog document keydown listener,
    // so clearPlayer()/job switches don't leak a stale Escape handler.
    this._resumeDialogAbort = null;
  }

  initPlayer() {
    if (this.playerInitialized) return;
    this.playerInitialized = true;

    const jobSelect = document.getElementById("player-job-select");
    const nicoToggle = document.getElementById("player-nico-toggle");
    const sidebarToggle = document.getElementById("player-sidebar-toggle");
    const video = document.getElementById("player-video");
    const sidebarMessages = document.getElementById("player-sidebar-messages");
    const syncBtn = document.getElementById("player-sync-btn");

    // Job selection
    jobSelect.addEventListener("sl-change", () => {
      const val = jobSelect.value;
      if (val) {
        this.onPlayerJobSelect(val);
      } else {
        this.clearPlayer();
      }
    });

    // Restore saved toggle state
    const savedNico = localStorage.getItem("player-nico-toggle");
    const savedSidebar = localStorage.getItem("player-sidebar-toggle");
    if (savedNico !== null) {
      nicoToggle.checked = savedNico === "true";
      this.nicoEnabled = nicoToggle.checked;
      document.getElementById("player-nico-overlay").style.display = this.nicoEnabled ? "" : "none";
    }
    if (savedSidebar !== null) {
      sidebarToggle.checked = savedSidebar === "true";
      document.getElementById("player-sidebar").style.display = sidebarToggle.checked ? "" : "none";
    }
    const reduceMotion = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches === true;
    if (savedNico === null && reduceMotion) {
      // Flying text is exactly what this preference asks to avoid. Default the
      // overlay off; the checkbox still lets the user opt in.
      nicoToggle.checked = false;
      this.nicoEnabled = false;
      document.getElementById("player-nico-overlay").style.display = "none";
    }

    // Nico toggle
    nicoToggle.addEventListener("sl-change", () => {
      this.nicoEnabled = nicoToggle.checked;
      localStorage.setItem("player-nico-toggle", nicoToggle.checked);
      const overlay = document.getElementById("player-nico-overlay");
      overlay.style.display = this.nicoEnabled ? "" : "none";
      if (this.nicoEnabled) {
        // Measure now that the overlay is visible — geometry updates are refused
        // while it is display:none, so without this the first tick after the
        // toggle would run on stale (or missing) geometry. Immediate: the user
        // just asked for the overlay, so it must not wait out a settle window.
        this._updateNicoGeometry({ immediate: true });
      } else {
        this.clearNicoOverlay();
        // Un-anchor: no ticks run while the overlay is off, so the next enabled
        // tick must re-seed at the current time instead of grinding through
        // (and counting as dropped) every message that passed meanwhile.
        this.nicoCursor = -1;
      }
    });

    // Sidebar toggle
    sidebarToggle.addEventListener("sl-change", () => {
      localStorage.setItem("player-sidebar-toggle", sidebarToggle.checked);
      const sidebar = document.getElementById("player-sidebar");
      sidebar.style.display = sidebarToggle.checked ? "" : "none";
      if (sidebarToggle.checked && this.playerChatMessages.length > 0) {
        const currentMs = this.getGlobalTimeMs();
        this.resetSidebarToTime(currentMs);
        this.syncSidebarToTime();
      }
    });

    // Video timeupdate
    video.addEventListener("timeupdate", () => this.onPlayerTimeUpdate());

    // Video seeked — reset both systems
    video.addEventListener("seeked", () => {
      const currentMs = this.getGlobalTimeMs();
      this.resetSidebarToTime(currentMs);
      this._reanchorNicoAt(currentMs + this.playerCustomOffsetMs);
    });

    // Pause/play nico animations. The overlay clock is media time, so the
    // animations simply follow the video — the cursor is NEVER advanced here
    // (doing so skipped every message that was due while paused).
    video.addEventListener("pause", () => {
      if (video.ended) return; // end-of-media pause: let in-flight text finish
      for (const a of this._nicoAnims) a.pause();
    });

    video.addEventListener("play", () => {
      for (const a of this._nicoAnims) a.play();
    });

    video.addEventListener("ratechange", () => {
      const rate = video.playbackRate || 1;
      for (const a of this._nicoAnims) a.playbackRate = rate;
    });

    document.addEventListener("visibilitychange", () => {
      // Hidden documents never dispatch animation finish events, so spawned
      // messages would pile up until the tab is shown. Clear and re-anchor.
      if (document.hidden) {
        this.clearNicoOverlay();
      } else if (this.playerChatMessages.length) {
        this._reanchorNicoAt(this.getGlobalTimeMs() + this.playerCustomOffsetMs);
      }
    });

    // Multi-segment: auto-advance to next segment when current one ends
    video.addEventListener("ended", () => this.onSegmentEnded());

    // Never freeze the overlay at end of media — let in-flight text fly out.
    video.addEventListener("ended", () => {
      for (const a of this._nicoAnims) a.play();
    });

    // Overlay geometry — the stage is the video's RENDERED rect, so re-measure
    // whenever the intrinsic size (`loadedmetadata`, `resize`), the fullscreen
    // state or the wrapper's own box changes.
    video.addEventListener("loadedmetadata", () => this._updateNicoGeometry());
    video.addEventListener("resize", () => this._updateNicoGeometry());

    // A single-file job only learns its real length here, so the post-end
    // region (and its divider) can only be final once metadata is in. Kept
    // separate from the geometry listener above: different concern, and the
    // re-stamp is skipped when the partition did not actually move.
    video.addEventListener("loadedmetadata", () => {
      if (!this.playerChatMessages.length) return;
      const before = this._chatParts ? this._chatParts.firstPostIndex : -1;
      this._computeChatParts();
      if (this._chatParts.firstPostIndex !== before) this._applyDividers();
    });

    document.addEventListener("fullscreenchange", () => this._updateNicoGeometry());
    const wrapper = document.getElementById("player-video-wrapper");
    if (wrapper && "ResizeObserver" in window) {
      // One measurement per frame: a window drag fires a callback storm, and
      // each measurement forces a layout flush (the probe). Note that during a
      // drag every frame IS a real change, so R11's same-size skip does nothing
      // here — what keeps the stage from being cleared per frame is the settle
      // timer in _updateNicoGeometry, which commits once the box holds still.
      let pending = 0;
      new ResizeObserver(() => {
        cancelAnimationFrame(pending);
        pending = requestAnimationFrame(() => this._updateNicoGeometry());
      }).observe(wrapper);
    }

    // Surface video load errors to user (e.g. segment 404s)
    video.addEventListener("error", () => {
      if (video.error && video.src) {
        console.error("Video load error:", video.error.message);
        this.app.showToast("Video failed to load — segment may be missing", "danger");
      }
    });

    // Sidebar scroll locking
    sidebarMessages.addEventListener("mouseenter", () => {
      this.playerScrollLock = true;
    });

    sidebarMessages.addEventListener("mouseleave", () => {
      if (this.playerAutoScroll) {
        this.playerScrollLock = false;
      }
    });

    sidebarMessages.addEventListener("scroll", () => {
      if (!this._programmaticScroll) {
        this.playerAutoScroll = false;
      }
    });

    // Sync button
    syncBtn.addEventListener("click", () => {
      this.playerAutoScroll = true;
      this.playerScrollLock = false;
      this.syncSidebarToTime();
    });

    // Chat search
    const chatSearch = document.getElementById("chat-search");
    if (chatSearch) {
      let searchTimeout = null;
      chatSearch.addEventListener("sl-input", () => {
        clearTimeout(searchTimeout);
        searchTimeout = setTimeout(() => this.filterChat(chatSearch.value), 200);
      });
    }

    // Custom chat offset — live apply on input, persist on blur/Enter
    const offsetInput = document.getElementById("player-chat-offset");
    if (offsetInput) {
      // Filter to valid numeric characters (digits, decimal point, minus sign)
      offsetInput.addEventListener("input", () => {
        let v = offsetInput.value.replace(/[^0-9.\-]/g, "");
        // Allow only one minus (at start) and one decimal point
        v = v.replace(/(?!^)-/g, "").replace(/(\..*)\./g, "$1");
        offsetInput.value = v;
        const val = parseFloat(v);
        this.playerCustomOffsetMs = isNaN(val) ? 0 : val * 1000;
        // Re-sync chat to current time with new offset
        const currentMs = this.getGlobalTimeMs();
        this.resetSidebarToTime(currentMs);
        this._reanchorNicoAt(currentMs + this.playerCustomOffsetMs);
        if (this.playerAutoScroll && !this.playerScrollLock) {
          this.syncSidebarToTime();
        }
        const resetBtn = document.getElementById("player-chat-offset-reset");
        if (resetBtn) {
          resetBtn.style.display = this.playerCustomOffsetMs !== 0 ? "" : "none";
        }
      });

      const persistOffset = () => {
        if (!this.playerJob) return;
        const val = parseFloat(offsetInput.value);
        const jobId = this.playerJob.id;
        if (isNaN(val) || val === 0) {
          this.playerCustomOffsetMs = 0;
          fetch(`/api/jobs/${jobId}/chat-offset`, { method: "DELETE" }).catch(() => {});
        } else {
          fetch(`/api/jobs/${jobId}/chat-offset`, {
            method: "PUT",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ chatOffset: val }),
          }).catch(() => {});
        }
      };

      offsetInput.addEventListener("blur", persistOffset);
      offsetInput.addEventListener("keydown", (e) => {
        if (e.key === "Enter") {
          offsetInput.blur();
        }
      });

      document.getElementById("player-chat-offset-reset")?.addEventListener("click", () => {
        offsetInput.value = "";
        this.playerCustomOffsetMs = 0;
        const currentMs = this.getGlobalTimeMs();
        this.resetSidebarToTime(currentMs);
        this._reanchorNicoAt(currentMs + this.playerCustomOffsetMs);
        if (this.playerAutoScroll && !this.playerScrollLock) {
          this.syncSidebarToTime();
        }
        // Persist the cleared offset
        if (this.playerJob) {
          fetch(`/api/jobs/${this.playerJob.id}/chat-offset`, { method: "DELETE" }).catch(() => {});
        }
        const resetBtn = document.getElementById("player-chat-offset-reset");
        if (resetBtn) resetBtn.style.display = "none";
      });
    }

    // Keyboard controls for player
    this.setupKeyboardControls();
  }

  setupKeyboardControls() {
    // The handler is attached only while the player tab is active (see
    // attachKeyboardControls/detachKeyboardControls). The panel-active
    // check inside the handler is a defensive second layer in case a
    // Shoelace tab-change event is missed mid-transition.
    this._playerKeyHandler = (e) => {
      const playerPanel = document.querySelector('sl-tab-panel[name="player"]');
      if (!playerPanel || !playerPanel.hasAttribute("active")) return;

      // Block shortcuts when a dialog is open (e.g. trim dialog)
      if (document.querySelector("sl-dialog[open]")) return;

      // Skip when typing in inputs (composedPath handles Shoelace shadow DOM)
      if (isTypingInInput(e)) return;

      const video = document.getElementById("player-video");
      if (!video || !video.src) return;

      switch (e.key) {
        case " ":
          if (video.paused) safePlay(video); else video.pause();
          e.preventDefault();
          break;
        case "ArrowLeft": {
          const delta = e.shiftKey ? 30 : 5;
          if (this._seg.active) {
            const globalSec = this.getGlobalTimeMs() / 1000 - delta;
            this.seekToGlobalTime(Math.max(0, globalSec));
          } else {
            video.currentTime -= delta;
          }
          e.preventDefault();
          break;
        }
        case "ArrowRight": {
          const delta = e.shiftKey ? 30 : 5;
          if (this._seg.active) {
            const globalSec = this.getGlobalTimeMs() / 1000 + delta;
            const maxSec = this._seg.totalDuration > 0 ? this._seg.totalDuration : Infinity;
            this.seekToGlobalTime(Math.min(maxSec, globalSec));
          } else {
            video.currentTime += delta;
          }
          e.preventDefault();
          break;
        }
        case "ArrowUp":
          video.volume = Math.min(1, video.volume + 0.1);
          e.preventDefault();
          break;
        case "ArrowDown":
          video.volume = Math.max(0, video.volume - 0.1);
          e.preventDefault();
          break;
        case "f": {
          const wrapper = document.getElementById("player-video-wrapper");
          if (document.fullscreenElement) {
            document.exitFullscreen();
          } else if (wrapper) {
            wrapper.requestFullscreen();
          }
          e.preventDefault();
          break;
        }
        case "m":
          video.muted = !video.muted;
          e.preventDefault();
          break;
        case "c": {
          const nicoToggle = document.getElementById("player-nico-toggle");
          if (nicoToggle) {
            nicoToggle.checked = !nicoToggle.checked;
            nicoToggle.dispatchEvent(new Event("sl-change"));
          }
          e.preventDefault();
          break;
        }
        case "s": {
          const sidebarToggle = document.getElementById("player-sidebar-toggle");
          if (sidebarToggle) {
            sidebarToggle.checked = !sidebarToggle.checked;
            sidebarToggle.dispatchEvent(new Event("sl-change"));
          }
          e.preventDefault();
          break;
        }
      }
    };

    // Attach immediately because initPlayer() only runs once the player
    // tab becomes active for the first time.
    this.attachKeyboardControls();
  }

  /** Attach the document-level player keydown listener. Idempotent. */
  attachKeyboardControls() {
    if (!this._playerKeyHandler || this._playerKeyAttached) return;
    document.addEventListener("keydown", this._playerKeyHandler);
    this._playerKeyAttached = true;
  }

  /** Detach the document-level player keydown listener. Idempotent. */
  detachKeyboardControls() {
    if (!this._playerKeyHandler || !this._playerKeyAttached) return;
    document.removeEventListener("keydown", this._playerKeyHandler);
    this._playerKeyAttached = false;
  }

  filterChat(query) {
    const container = document.getElementById("player-sidebar-messages");
    if (!container) return;
    const children = container.children;
    const needle = query.trim().toLowerCase();

    if (!needle) {
      // Clear search — restore all messages and fix active/future state
      // (messages may have stale active/future classes from during the search)
      for (let i = 0; i < children.length; i++) {
        children[i].classList.remove("search-hidden");
      }
      this.resetSidebarToTime(this.getGlobalTimeMs());
      this.playerAutoScroll = true;
      this.playerScrollLock = false;
      this.syncSidebarToTime();
      return;
    }

    // Filter messages
    for (let i = 0; i < this.playerChatMessages.length; i++) {
      const msg = this.playerChatMessages[i];
      const authorMatch = (msg.authorName || "").toLowerCase().includes(needle);
      const textParts = msg.message || [];
      let textMatch = false;
      if (typeof textParts === "string") {
        textMatch = textParts.toLowerCase().includes(needle);
      } else if (Array.isArray(textParts)) {
        textMatch = textParts.some((p) => (p.text || "").toLowerCase().includes(needle));
      }
      const child = children[i];
      if (child) {
        if (authorMatch || textMatch) {
          child.classList.remove("search-hidden");
        } else {
          child.classList.add("search-hidden");
        }
      }
    }
    // Disable auto-scroll during search
    this.playerAutoScroll = false;
  }

  clearPlayer() {
    // Invalidate any in-flight fetches from a previous selection
    this._selectionSeq++;

    // Stop watch tracking (interval, pause handler, beforeunload)
    this._clearWatchTracking();
    this._dismissResumeDialog();

    this.playerJob = null;
    this.playerChatData = null;
    this.playerChatMessages = [];
    this.twitchEmoteMap = new Map();
    this.playerAutoScroll = true;
    this.playerScrollLock = false;
    this.playerActiveChatIndex = 0;
    this._chatParts = null;
    this._updateSidebarHeader();
    this.nicoCursor = -1;
    // A geometry commit armed by the last resize has nothing left to commit.
    clearTimeout(this._nicoGeoSettle);
    this._nicoGeoSettle = null;
    this._resetNicoDropCount();
    this.playerCustomOffsetMs = 0;
    const offsetInput = document.getElementById("player-chat-offset");
    if (offsetInput) offsetInput.value = "";
    const chatSearch = document.getElementById("chat-search");
    if (chatSearch && chatSearch.value) chatSearch.value = "";

    // Reset multi-segment state
    this._seg.reset();

    const video = document.getElementById("player-video");
    video.pause();
    video.removeAttribute("src");
    video.load();

    document.getElementById("player-viewport").style.display = "none";
    document.getElementById("player-empty-state").style.display = "";
    document.getElementById("player-sidebar-messages").innerHTML = "";
    // Remove segment indicator if present
    const segIndicator = document.getElementById("player-segment-indicator");
    if (segIndicator) segIndicator.remove();
    this.clearNicoOverlay();
  }

  /**
   * Get the global playback time in milliseconds (accounting for multi-segment offset).
   */
  getGlobalTimeMs() {
    const video = document.getElementById("player-video");
    if (!video) return 0;
    return this._seg.getGlobalTime(video) * 1000;
  }

  /**
   * Initialize multi-segment playback with sequential source switching.
   */
  initMultiSegmentPlayer(jobId, segments) {
    const video = document.getElementById("player-video");
    this._seg.init(jobId, segments);
    this._seg.loadSegment(0, video);
    this.buildSegmentIndicator();
  }

  onSegmentEnded() {
    const video = document.getElementById("player-video");
    const advanced = this._seg.onSegmentEnded(video);

    // The recording is over: its tail is "after it", not "future". Guarded on
    // its own rather than folded into the watched-detection branch below —
    // re-watching a finished video must still promote the tail.
    if (!advanced) this._markPostEnd();

    // Watched detection fallback — video played to natural end
    // Only trigger when no more segments to advance to (advanced === false or non-segmented)
    if (!advanced && this.playerJob && !this._watchedTriggered) {
      this._watchedTriggered = true;
      this._clearWatchTracking();
      fetch(`/api/jobs/${this.playerJob.id}/watched`, { method: "POST" }).catch(() => {});
    }
  }

  /**
   * Seek to a global time (seconds) across segments.
   */
  seekToGlobalTime(globalSeconds) {
    const video = document.getElementById("player-video");
    this._seg.seekToGlobalTime(globalSeconds, video);
  }

  /**
   * Build a segment indicator bar showing quality changes below the video.
   */
  buildSegmentIndicator() {
    // Remove existing indicator
    let indicator = document.getElementById("player-segment-indicator");
    if (indicator) indicator.remove();
    if (!this._seg.segOffsets || this._seg.segOffsets.length <= 1) return;
    if (this._seg.totalDuration <= 0) return;

    indicator = document.createElement("div");
    indicator.id = "player-segment-indicator";
    indicator.className = "segment-indicator";

    const colors = ["#3b82f6", "#8b5cf6", "#06b6d4", "#f59e0b", "#ef4444", "#10b981"];

    this._seg.segOffsets.forEach((seg, i) => {
      const pct = ((seg.durationSeconds || 0) / this._seg.totalDuration) * 100;
      const block = document.createElement("button");
      block.type = "button";
      block.className = "segment-indicator-block";
      block.style.width = `${pct}%`;
      block.style.background = colors[i % colors.length];
      block.title = `Segment ${i}: ${seg.quality} (${Math.round(seg.durationSeconds || 0)}s)`;
      block.setAttribute("aria-label", `Seek to segment ${i + 1}, ${seg.quality}`);
      block.textContent = seg.quality || `Seg ${i}`;
      block.addEventListener("click", () => {
        this.seekToGlobalTime(seg.startOffset);
      });
      indicator.appendChild(block);
    });

    document.getElementById("player-video-column")?.appendChild(indicator);
  }

  async loadPlayerJobList() {
    const select = document.getElementById("player-job-select");
    const currentValue = select.value;

    try {
      const [jobsRes, archivedRes] = await Promise.all([
        fetch("/api/jobs"),
        fetch("/api/jobs/archived"),
      ]);
      const jobs = jobsRes.ok ? await jobsRes.json() : [];
      const archived = archivedRes.ok ? await archivedRes.json() : [];
      if (!jobsRes.ok && !archivedRes.ok) {
        this.app.showToast("Failed to load video list", "warning");
      }

      const all = [...jobs, ...archived]
        .filter((j) => j.status === "Finished" && j.filename)
        .sort((a, b) => new Date(b.updatedAt) - new Date(a.updatedAt));

      // If a video is actively playing, only update the selection state
      // without rebuilding options (removing options triggers sl-change which
      // can interrupt playback via clearPlayer).
      const isPlaying = this.playerJob && document.getElementById("player-video")?.src;
      if (isPlaying) {
        // Ensure current job still exists; if not, clear the player
        // and fall through to rebuild options (clearPlayer is idempotent
        // so any sl-change from option removal is harmless).
        if (currentValue && !all.some((j) => j.id === currentValue)) {
          this.clearPlayer();
        } else {
          return;
        }
      }

      // Remove existing options
      select.querySelectorAll("sl-option").forEach((o) => o.remove());

      all.forEach((job) => {
        const opt = document.createElement("sl-option");
        opt.value = job.id;
        const noChat = !job.chatFilename ? " (no chat)" : "";
        opt.textContent = `${job.title} — ${job.channelName}${noChat}`;
        select.appendChild(opt);
      });

      // Wait for Shoelace to register new options before restoring selection
      if (select.updateComplete) {
        await select.updateComplete.catch(() => {});
      }

      if (currentValue && all.some((j) => j.id === currentValue)) {
        select.value = currentValue;
      }

      // Show/hide empty state
      const emptyState = document.getElementById("player-empty-state");
      if (all.length === 0) {
        emptyState.style.display = "";
        emptyState.querySelector("p").textContent = "No finished videos available.";
      } else if (!select.value) {
        emptyState.style.display = "";
        emptyState.querySelector("p").textContent = "Select a finished video to play.";
      }
    } catch (e) {
      console.error("Failed to load player job list:", e);
    }
  }

  async onPlayerJobSelect(jobId) {
    const video = document.getElementById("player-video");
    const nicoToggle = document.getElementById("player-nico-toggle");
    const sidebarToggle = document.getElementById("player-sidebar-toggle");

    // Track selection to detect stale responses from rapid switching
    const selectionId = ++this._selectionSeq;

    // Fetch job details
    try {
      const res = await fetch(`/api/jobs/${jobId}`);
      if (!res.ok || this._selectionSeq !== selectionId) return;
      const job = await res.json();
      // The body of an OLDER selection can resolve after a newer one completed —
      // re-check before anything observable (playerJob, video.src) is touched.
      if (this._selectionSeq !== selectionId) return;
      this.playerJob = job;
    } catch (e) {
      console.error("Failed to fetch job:", e);
      return;
    }

    // Reset scroll state for new video
    this.playerAutoScroll = true;
    this.playerScrollLock = false;

    // Show viewport, hide empty state
    document.getElementById("player-viewport").style.display = "";
    document.getElementById("player-empty-state").style.display = "none";

    // Reset multi-segment state
    this._seg.reset();
    const segIndicator = document.getElementById("player-segment-indicator");
    if (segIndicator) segIndicator.remove();

    // Remove resume overlay if present from previous job
    this._dismissResumeDialog();

    // Multi-segment or single-file video source
    if (this.playerJob.segments && this.playerJob.segments.length > 0) {
      this.initMultiSegmentPlayer(jobId, this.playerJob.segments);
    } else {
      video.src = `/api/jobs/${jobId}/video`;
    }

    // Fetch fresh watch state (not cached like the job endpoint)
    this._clearWatchTracking();
    this._watchedTriggered = false;
    try {
      const wsRes = await fetch(`/api/jobs/${jobId}/watch-state`);
      if (this._selectionSeq !== selectionId) return;
      if (wsRes.ok) {
        const ws = await wsRes.json();
        this.playerJob.watched = ws.watched;
        this.playerJob.resumePosition = ws.resumePosition;
        // The chat-offset input is restored from playerJob.chatOffset — copy
        // the fresh value too, or a stale cached offset gets re-applied.
        if (ws.chatOffset !== undefined) this.playerJob.chatOffset = ws.chatOffset;
      }
    } catch { /* proceed with cached values */ }

    const resumePos = this.playerJob.resumePosition;
    if (resumePos != null && resumePos > 0) {
      this._showResumeDialog(jobId, resumePos);
    } else {
      this._startWatchTracking(jobId);
    }

    // Load chat if available
    this.playerChatMessages = [];
    this.playerChatData = null;
    this.twitchEmoteMap = new Map();
    this.playerActiveChatIndex = 0;
    this.nicoCursor = -1;
    this._resetNicoDropCount();

    if (this.playerJob.chatFilename || (this.playerJob.segments || []).some((s) => s.chatFile)) {
      try {
        this.playerChatData = await this._fetchChatData(jobId, selectionId);
        if (this._selectionSeq !== selectionId) return; // Selection changed during fetch
        if (this.playerChatData) {
          // Chat-to-video timing correction (see chat-timeline.js for the
          // semantics per platform). Multi-part YouTube jobs use the same
          // rule: the video begins at the actual stream start regardless of
          // when Moombox started downloading.
          const chatBiasMs = computeChatBiasMs({
            platform: this.playerChatData.platform,
            chatStreamStartTime: this.playerChatData.streamStartTime,
            jobStreamStartTime: this.playerJob.streamStartTime,
          });
          this.playerChatMessages = (this.playerChatData.messages || [])
            .map((m) => ({ ...m, offsetMs: normalizeOffsetMs(m.offsetMs) - chatBiasMs }))
            .sort((a, b) => a.offsetMs - b.offsetMs);

          // Build 3rd-party emote lookup map for Twitch chat
          // Priority (Chatterino order): FFZ > BTTV > 7TV — add lowest first so higher overwrites
          if (this.playerChatData.emotes) {
            const { bttv, ffz, seventv } = this.playerChatData.emotes;
            for (const e of seventv || []) this.twitchEmoteMap.set(e.code, e.url);
            for (const e of bttv || []) this.twitchEmoteMap.set(e.code, e.url);
            for (const e of ffz || []) this.twitchEmoteMap.set(e.code, e.url);
          }

          // Release the raw array now that playerChatMessages holds the
          // normalized/biased copy — halves peak memory for large chat
          // files. filterChat and everything else read playerChatMessages.
          this.playerChatData.messages = null;
        }
      } catch (e) {
        console.error("Failed to load chat:", e);
        this.app.showToast("Failed to load chat replay", "warning");
      }
    }

    // Show/hide chat UI based on whether chat is available.
    // Toggles stay enabled so user preferences persist across selections;
    // just hide the actual sidebar/overlay when there's no chat data.
    const hasChat = this.playerChatMessages.length > 0;
    document.getElementById("player-sidebar").style.display =
      hasChat && sidebarToggle.checked ? "" : "none";
    document.getElementById("player-nico-overlay").style.display =
      hasChat && nicoToggle.checked ? "" : "none";

    // Clear chat search on job change
    const chatSearch = document.getElementById("chat-search");
    if (chatSearch && chatSearch.value) {
      chatSearch.value = "";
    }

    // Partition + header BEFORE the build: _buildChatMessageEl stamps the two
    // divider rows as it creates them, so this._chatParts has to exist first.
    // A single-file job whose duration is not known yet gets no post region
    // here — the `loadedmetadata` listener recomputes and re-stamps.
    this._computeChatParts();

    // Build sidebar chat
    this.buildSidebarChat();
    this.clearNicoOverlay();

    // Load saved custom chat offset (from watch-state response, already on playerJob)
    const offsetInput = document.getElementById("player-chat-offset");
    if (offsetInput) {
      offsetInput.value = "";
      this.playerCustomOffsetMs = 0;
      const savedOffset = this.playerJob.chatOffset;
      if (savedOffset && savedOffset !== 0) {
        offsetInput.value = savedOffset;
        this.playerCustomOffsetMs = savedOffset * 1000;
      }
    }

    // Same rule as the nico toggle: the overlay's display was just decided, so
    // measure it now it is visible — a job switch that reveals a previously
    // hidden overlay (the last job had no chat) resizes nothing and may have
    // already missed this video's `loadedmetadata`. Last, so the re-anchor it
    // may trigger sees the restored chat offset; immediate, because the first
    // tick of a freshly selected job must already have the right row count.
    this._updateNicoGeometry({ immediate: true });
  }

  /**
   * Load the chat for the selected job. A multi-part job whose parts carry
   * their own chat files (Twitch live: offsets are part-relative) is merged
   * onto the global timeline part by part; everything else uses the job-level
   * file. Returns null when the selection changed underneath us or nothing
   * was available.
   */
  async _fetchChatData(jobId, selectionId) {
    const segments = this.playerJob.segments || [];
    const withChat = segments.filter((s) => s.chatFile);
    if (segments.length > 1 && withChat.length > 0 && this._seg.active) {
      // Captured BEFORE the per-part fetch: a newer selection's reset()
      // nulls this._seg.segOffsets mid-flight, and reading it after the
      // await inside the closure would throw on the stale `.find()` call
      // instead of falling through to the seq check below.
      const segOffsets = this._seg.segOffsets;
      const parts = await Promise.all(withChat.map(async (s) => {
        try {
          const r = await fetch(`/api/jobs/${jobId}/segments/${s.segmentIndex}/chat`);
          if (!r.ok) return null;
          const data = await r.json();
          const off = segOffsets.find((o) => o.segmentIndex === s.segmentIndex);
          return { startOffsetSec: off ? off.startOffset : 0, data };
        } catch {
          return null;
        }
      }));
      if (this._selectionSeq !== selectionId) return null;
      const merged = mergePartChats(parts.filter(Boolean));
      if (merged.messages.length > 0) return merged;
    }
    const chatRes = await fetch(`/api/jobs/${jobId}/chat`);
    if (this._selectionSeq !== selectionId) return null;
    if (!chatRes.ok) return null;
    const data = await chatRes.json();
    return this._selectionSeq !== selectionId ? null : data;
  }

  buildSidebarChat() {
    const container = document.getElementById("player-sidebar-messages");
    container.innerHTML = "";
    this.playerActiveChatIndex = 0;

    const messages = this.playerChatMessages;
    if (messages.length === 0) return;

    // Chunked build: creating DOM for every message up-front froze the UI for
    // seconds on long VODs (50-100K+ messages × ~4 nodes each). The first
    // chunk builds synchronously — chats up to one chunk behave exactly as
    // before — and the rest appends in setTimeout(0) batches so the player
    // stays interactive while a huge chat materializes. Chunks append in
    // order, preserving the children[i] === playerChatMessages[i] alignment
    // that filterChat / resetSidebarToTime / updateSidebarActiveState rely
    // on (all three already tolerate missing tail children). A selection
    // change mid-build cancels via the _selectionSeq token.
    const CHUNK = 2500;
    const seq = this._selectionSeq;

    const buildFrom = (start) => {
      if (this._selectionSeq !== seq) return; // job changed mid-build
      const end = Math.min(start + CHUNK, messages.length);
      const frag = document.createDocumentFragment();
      for (let i = start; i < end; i++) {
        frag.appendChild(this._buildChatMessageEl(messages[i], i));
      }
      container.appendChild(frag);
      if (end < messages.length) {
        setTimeout(() => buildFrom(end), 0);
        return;
      }
      // Build complete — reconcile the divider rows (the partition may have
      // moved mid-build, e.g. `loadedmetadata` landing between two chunks)
      // and, if playback already reached the end while the list was still
      // materializing, promote the tail the late chunks built as `.future`.
      this._applyDividers();
      if (this._atRecordingEnd()) this._markPostEnd();

      // A search typed while chunks were pending only hid the children that
      // existed at the time; re-apply it over the full set.
      const search = document.getElementById("chat-search");
      if (search && search.value) {
        this.filterChat(search.value);
      }
    };

    buildFrom(0);
  }

  /**
   * Build one sidebar chat message element. `index` decides the initial
   * active/future class: playback may advance past a message while its chunk
   * is still pending, and updateSidebarActiveState only walks FORWARD — it
   * never revisits earlier indices — so a late-built element for an
   * already-passed message must materialize as active.
   */
  _buildChatMessageEl(msg, index) {
    const div = document.createElement("div");
    div.className = index < this.playerActiveChatIndex ? "chat-msg active" : "chat-msg future";
    div.dataset.offset = msg.offsetMs;

    // Region boundary: the divider is ::before pseudo-content on the first row
    // of the region, so the children[i] === playerChatMessages[i] alignment
    // that filterChat / resetSidebarToTime / updateSidebarActiveState rely on
    // survives (a real divider element would shift every index after it).
    const dividerLabel = this._dividerLabelFor(index);
    if (dividerLabel) {
      div.classList.add("divider-before");
      div.dataset.divider = dividerLabel;
    }

    if (msg.superchat) {
      div.classList.add("superchat");
    }

    if (msg.messageType === "announcement") {
      div.classList.add("announcement");
      div.classList.add(`announcement-${announcementColorClass(msg.announcementColor)}`);
    }

    // Timestamp
    const timeSpan = document.createElement("span");
    timeSpan.className = "chat-msg-time";
    timeSpan.textContent = formatMsToTime(msg.offsetMs);
    div.appendChild(timeSpan);

    // Superchat amount
    if (msg.superchat) {
      const scSpan = document.createElement("span");
      scSpan.className = "chat-msg-superchat";
      scSpan.textContent = msg.superchat.amount;
      div.appendChild(scSpan);
    }

    // Author
    const authorSpan = document.createElement("span");
    authorSpan.className = "chat-msg-author";
    if (msg.authorBadges && Array.isArray(msg.authorBadges)) {
      // Twitch badges use "type/tier" format (e.g. "subscriber/12"), so check prefix
      const hasBadge = (name) => msg.authorBadges.some((b) => b === name || b.startsWith(name + "/"));
      if (hasBadge("owner") || hasBadge("broadcaster")) authorSpan.classList.add("owner");
      else if (hasBadge("moderator")) authorSpan.classList.add("moderator");
      else if (hasBadge("member") || hasBadge("subscriber")) authorSpan.classList.add("member");
      else if (hasBadge("vip")) authorSpan.classList.add("member");
    }
    authorSpan.textContent = msg.authorName + ": ";
    div.appendChild(authorSpan);

    // Message content — use safe DOM builder instead of innerHTML
    const contentSpan = document.createElement("span");
    this.appendChatContent(contentSpan, msg.message || [], msg.emotes);
    div.appendChild(contentSpan);

    return div;
  }

  /**
   * Known length of the recording in ms: the segment sum for a multi-part job,
   * else the loaded media's own duration, else the job's metadata length.
   * 0 = not known yet (single-file job before `loadedmetadata`), which means
   * "no post-end region" until it is.
   * @returns {number}
   */
  _videoDurationMs() {
    const video = document.getElementById("player-video");
    if (this._seg.active && this._seg.totalDuration > 0) return this._seg.totalDuration * 1000;
    if (video && Number.isFinite(video.duration) && video.duration > 0) return video.duration * 1000;
    const len = this.playerJob?.lengthSeconds;
    return len > 0 ? len * 1000 : 0;
  }

  /** Re-derive the pre-show / post-end partition and refresh the header. */
  _computeChatParts() {
    this._chatParts = partitionChatByVideo(this.playerChatMessages, this._videoDurationMs());
    this._updateSidebarHeader();
  }

  /**
   * The sidebar header. Sole writer of #player-sidebar-msg-count, and every
   * count comes from the messages actually loaded — never from a chat file's
   * own header, which can disagree with what was parsed.
   */
  _updateSidebarHeader() {
    const n = this.playerChatMessages.length;
    const p = this._chatParts || { preCount: 0, postCount: 0 };
    let text = `${n} messages`;
    if (p.preCount > 0) text += ` · ${p.preCount} pre-show`;
    if (p.postCount > 0) text += ` · ${p.postCount} after end`;
    document.getElementById("player-sidebar-msg-count").textContent = text;
  }

  /**
   * Divider text for the row at `index`, or null. Both regions can start on
   * the SAME row (a chat whose entire live section falls past the recording);
   * the waiting-room label wins there — it explains that row's own position.
   * @param {number} index
   * @returns {string|null}
   */
  _dividerLabelFor(index) {
    const p = this._chatParts;
    if (!p) return null;
    if (p.preCount > 0 && index === p.firstLiveIndex) {
      return `Waiting room — ${p.preCount} messages before the stream`;
    }
    if (p.firstPostIndex >= 0 && index === p.firstPostIndex) {
      return `Recording ended — ${p.postCount} messages after it`;
    }
    return null;
  }

  /**
   * Stamp/clear the divider classes on the boundary rows. Idempotent, and safe
   * while the chunked build is still running — it only ever touches children
   * that exist, and the completion callback runs it again over the full list.
   */
  _applyDividers() {
    const children = document.getElementById("player-sidebar-messages").children;
    for (const el of children) {
      if (el.classList.contains("divider-before")) {
        el.classList.remove("divider-before");
        delete el.dataset.divider;
      }
    }
    const p = this._chatParts;
    if (!p) return;
    for (const index of [p.firstLiveIndex, p.firstPostIndex]) {
      if (index < 0 || !children[index]) continue;
      const label = this._dividerLabelFor(index);
      if (!label) continue;
      children[index].classList.add("divider-before");
      children[index].dataset.divider = label;
    }
  }

  /**
   * Has playback reached the end of the recording? The 250 ms slack covers a
   * media file that stops a hair short of its reported duration.
   * @param {number} [currentMs] global playback time, if already known
   */
  _atRecordingEnd(currentMs = this.getGlobalTimeMs()) {
    const durationMs = this._videoDurationMs();
    return durationMs > 0 && currentMs + 250 >= durationMs;
  }

  /**
   * After the recording ends, its tail is "after it", not "future": readable,
   * labelled and reachable rather than dimmed like something still to come.
   */
  _markPostEnd() {
    const p = this._chatParts;
    if (!p || p.firstPostIndex < 0) return;
    const children = document.getElementById("player-sidebar-messages").children;
    for (let i = p.firstPostIndex; i < children.length; i++) {
      children[i].classList.remove("future");
      children[i].classList.add("post");
    }
  }

  onPlayerTimeUpdate() {
    const video = document.getElementById("player-video");
    if (!video || !this.playerChatMessages.length) return;

    const currentMs = this.getGlobalTimeMs();

    // Update sidebar active state
    this.updateSidebarActiveState(currentMs);

    // Spawn nico messages (the argument IS effective — offset-adjusted — time)
    if (this.nicoEnabled) {
      this.spawnNicoMessages(currentMs + this.playerCustomOffsetMs);
    }

    // Auto-scroll sidebar
    if (this.playerAutoScroll && !this.playerScrollLock) {
      this.syncSidebarToTime();
    }

    // Promote the after-the-recording tail. `ended` covers the normal case,
    // but a media file that stops short of its duration never fires it, and a
    // seek to the last second only ever produces this one tick.
    if (this._atRecordingEnd(currentMs)) this._markPostEnd();
  }

  updateSidebarActiveState(currentMs) {
    const container = document.getElementById("player-sidebar-messages");
    const children = container.children;

    // Walk forward from current index
    while (
      this.playerActiveChatIndex < this.playerChatMessages.length &&
      this.playerChatMessages[this.playerActiveChatIndex].offsetMs <= currentMs + this.playerCustomOffsetMs
    ) {
      const child = children[this.playerActiveChatIndex];
      if (child) {
        child.classList.remove("future");
        child.classList.add("active");
      }
      this.playerActiveChatIndex++;
    }
  }

  syncSidebarToTime() {
    const container = document.getElementById("player-sidebar-messages");
    if (!container) return;

    // At the end of the recording, "current time" IS the end: scroll to the
    // "Recording ended" divider so the tail — the part that has no playback
    // position of its own — is what the sync button hands you. Checked before
    // the active-index guard so a chat that is entirely post-end still syncs.
    const video = document.getElementById("player-video");
    const p = this._chatParts;
    if (video?.ended && p && p.firstPostIndex >= 0 && container.children[p.firstPostIndex]) {
      this._programmaticScroll = true;
      container.scrollTop = Math.max(0, container.children[p.firstPostIndex].offsetTop - 8);
      requestAnimationFrame(() => {
        this._programmaticScroll = false;
      });
      return;
    }

    if (this.playerActiveChatIndex === 0) return;

    const targetChild = container.children[this.playerActiveChatIndex - 1];
    if (!targetChild) return;

    // Scroll so last active message is at ~70% from top
    const containerHeight = container.clientHeight;
    const targetOffset = targetChild.offsetTop - containerHeight * 0.7;

    this._programmaticScroll = true;
    container.scrollTop = Math.max(0, targetOffset);
    requestAnimationFrame(() => {
      this._programmaticScroll = false;
    });
  }

  resetSidebarToTime(currentMs) {
    const container = document.getElementById("player-sidebar-messages");
    const children = container.children;
    const messages = this.playerChatMessages;
    const effectiveMs = currentMs + this.playerCustomOffsetMs;

    // Binary search for the split point (first message after effectiveMs)
    let lo = 0;
    let hi = messages.length;
    while (lo < hi) {
      const mid = (lo + hi) >>> 1;
      if (messages[mid].offsetMs <= effectiveMs) {
        lo = mid + 1;
      } else {
        hi = mid;
      }
    }
    // lo = number of active messages (all with offsetMs <= effectiveMs)
    const newActiveIndex = lo;

    // Only update DOM for children that changed state
    // Previously active but now should be future (seeked backwards)
    for (let i = newActiveIndex; i < this.playerActiveChatIndex && i < children.length; i++) {
      children[i].classList.remove("active");
      children[i].classList.remove("post");
      children[i].classList.add("future");
    }
    // Previously future but now should be active (seeked forwards)
    for (let i = this.playerActiveChatIndex; i < newActiveIndex && i < children.length; i++) {
      children[i].classList.remove("future");
      children[i].classList.add("active");
    }

    // Seeking back off the end returns the post-end rows to "future". The loop
    // above only reaches the ones that were also active, and _markPostEnd
    // strips `.future`, so re-adding it here is what makes them dim again.
    const p = this._chatParts;
    if (p && p.firstPostIndex >= 0 && newActiveIndex < p.firstPostIndex) {
      for (let i = p.firstPostIndex; i < children.length; i++) {
        children[i].classList.remove("post");
        children[i].classList.add("future");
      }
    }

    this.playerActiveChatIndex = newActiveIndex;
  }

  // Niconico overlay engine

  /**
   * Size the overlay to the VIDEO'S RENDERED RECT (not the wrapper, which has
   * letterbox bars around a video whose aspect ratio differs from its box) and
   * derive the row count from a MEASURED line box, so the rows follow the
   * container-query font instead of a hard-coded constant.
   *
   * Committing a changed geometry is destructive — in-flight keyframes were
   * computed for the old stage width and pending entries cache measurements
   * taken at the old font size, so the stage has to be cleared and re-anchored.
   * Two things keep that rare:
   * - a callback that measures the SAME width, height and row count returns
   *   here and commits nothing (R11);
   * - a callback that measures a real change only ARMS the commit, which fires
   *   once the box has held still for NICO_GEO_SETTLE_MS (R23). Dragging a
   *   window edge produces a different box on every frame — all of them real
   *   changes — so without the timer the overlay would be cleared per frame and
   *   stay blank for the whole drag.
   *
   * The overlay's own box follows every call, so the stage never lags the video
   * during a drag. In that window the messages still fly on the previously
   * committed width — a small horizontal offset for ~120 ms, against a blank
   * overlay for as long as the drag lasts.
   * @param {{immediate?: boolean}} [opts] `immediate` commits without waiting —
   *   used where the overlay has just been made visible and the very next tick
   *   must already use the right row count.
   */
  _updateNicoGeometry({ immediate = false } = {}) {
    const video = document.getElementById("player-video");
    const overlay = document.getElementById("player-nico-overlay");
    if (!video || !overlay) return;
    // A hidden overlay (toggle off, player tab inactive) measures 0x0 and its
    // container query resolves against nothing, so a measurement here would be
    // garbage. Leave _nicoGeo untouched; the toggle-on handler re-measures once
    // the overlay is visible again.
    if (overlay.clientWidth === 0 || overlay.clientHeight === 0) return;

    // Letterbox math: the <video> paints its content centred inside its box at
    // the largest scale that fits, so a portrait video in a landscape box gets
    // pillarbox bars (and vice versa). Before `loadedmetadata` the intrinsic
    // size is unknown (0) and the element box is the best available stage.
    const bw = video.clientWidth, bh = video.clientHeight;
    let w = bw, h = bh, left = video.offsetLeft, top = video.offsetTop;
    const vw = video.videoWidth, vh = video.videoHeight;
    if (vw > 0 && vh > 0 && bw > 0 && bh > 0) {
      const scale = Math.min(bw / vw, bh / vh);
      w = Math.round(vw * scale);
      h = Math.round(vh * scale);
      left += Math.round((bw - w) / 2);
      top += Math.round((bh - h) / 2);
    }
    // Never write a zero-sized box: the guard above would then refuse every
    // later measurement, wedging the overlay shut. Keep the last good geometry
    // and wait for the next resize instead.
    if (w <= 0 || h <= 0) return;
    Object.assign(overlay.style, { left: `${left}px`, top: `${top}px`, width: `${w}px`, height: `${h}px` });

    // Measure one real line box AFTER the box is applied — the font is sized in
    // cqh, so the probe has to see the new overlay height.
    const probe = document.createElement("div");
    probe.className = "nico-message";
    probe.style.visibility = "hidden";
    probe.textContent = "Ag";
    overlay.appendChild(probe);
    const rowH = probe.offsetHeight || 24;
    probe.remove();

    const rows = Math.max(1, Math.floor(h / rowH));
    const geo = this._nicoGeo;
    if (geo && geo.width === w && geo.height === h && geo.rows === rows) {
      // R11: nothing to commit. Also drop a commit armed earlier in the same
      // gesture — a drag that returned to its starting size would otherwise
      // install a stage that is no longer on screen.
      clearTimeout(this._nicoGeoSettle);
      this._nicoGeoSettle = null;
      return;
    }

    clearTimeout(this._nicoGeoSettle);
    this._nicoGeoSettle = null;
    // The first-ever measurement has no stage to protect (nothing is flying and
    // spawning is blocked until _nicoGeo exists), so it commits at once, as do
    // the calls that follow making the overlay visible.
    if (immediate || !geo) {
      this._commitNicoGeometry(w, h, rows);
      return;
    }
    this._nicoGeoSettle = setTimeout(() => this._commitNicoGeometry(w, h, rows), NICO_GEO_SETTLE_MS);
  }

  /**
   * Install a settled geometry — the destructive half of _updateNicoGeometry.
   * Both guards are re-checked because this can run NICO_GEO_SETTLE_MS after the
   * measurement: the overlay may have been hidden meanwhile (toggle off, tab
   * switch), and the box may have been committed by another path.
   * @param {number} w
   * @param {number} h
   * @param {number} rows
   */
  _commitNicoGeometry(w, h, rows) {
    this._nicoGeoSettle = null;
    const overlay = document.getElementById("player-nico-overlay");
    if (!overlay || overlay.clientWidth === 0 || overlay.clientHeight === 0) return;
    const geo = this._nicoGeo;
    if (geo && geo.width === w && geo.height === h && geo.rows === rows) return;

    this._nicoGeo = { width: w, height: h, laneHeight: h / rows, rows, version: (geo?.version || 0) + 1 };
    this._lanes.reset(rows);
    // Exactly one clear on this path: _reanchorNicoAt clears before re-seeding
    // (and clearNicoOverlay empties _nicoPending, so no cached w/h measured at
    // the old font size outlives the change); _lanes.reset() inside it keeps the
    // row count just set above.
    if (this.playerChatMessages.length) {
      // Re-anchor so the messages that should be on screen come back mid-flight
      // at the new scale (rather than the overlay staying blank for a traverse).
      this._reanchorNicoAt(this.getGlobalTimeMs() + this.playerCustomOffsetMs);
    } else {
      this.clearNicoOverlay();
    }
  }

  clearNicoOverlay() {
    for (const a of this._nicoAnims) a.cancel();
    this._nicoAnims.clear();
    const overlay = document.getElementById("player-nico-overlay");
    if (overlay) overlay.replaceChildren();
    this._lanes.reset();
    // Pending entries hold DETACHED elements, so dropping the list is the discard.
    this._nicoPending = [];
  }

  /**
   * Clear the stage and re-anchor at `effectiveMs` — the ONLY way the cursor is
   * re-seeded. `_resetNicoCursor` frees every lane, so it must never run while
   * elements are still flying; pairing the two here makes that structural rather
   * than a rule each call site has to remember.
   * @param {number} effectiveMs
   */
  _reanchorNicoAt(effectiveMs) {
    this.clearNicoOverlay();
    this._resetNicoCursor(effectiveMs);
  }

  /**
   * Anchor the overlay cursor at `effectiveMs`: the next tick considers only a
   * short seed of "chat that was already flying" (newer than
   * effectiveMs − NICO_MAX_LATENESS_MS, and at most two screens' worth of rows),
   * never the whole pre-show backlog. Also drops any deferred messages and frees
   * every lane — the caller has cleared the overlay.
   */
  _resetNicoCursor(effectiveMs) {
    const rows = this._lanes.laneCount || NICO_SEED_MAX_FALLBACK / 2;
    this.nicoCursor = seedCursorIndex(
      this.playerChatMessages, effectiveMs, NICO_MAX_LATENESS_MS, 2 * rows, indexAfter,
    );
    this._nicoPending = [];
    this._nicoAnchorMs = effectiveMs;
    this._lanes.reset();
  }

  /** Zero the drop counter and hide the pill (job switch / player teardown). */
  _resetNicoDropCount() {
    this.nicoDropped = 0;
    this._nicoDroppedShown = 0;
    clearTimeout(this._nicoDropPillTimer);
    this._nicoDropPillTimer = null;
    const pill = document.getElementById("player-nico-dropped");
    if (pill) pill.hidden = true;
  }

  /**
   * Advance the overlay to `effectiveMs` (media time plus the user's chat offset).
   * @param {number} effectiveMs
   */
  spawnNicoMessages(effectiveMs) {
    const messages = this.playerChatMessages;
    if (!messages.length || document.hidden) return;
    if (this.nicoCursor < 0) this._reanchorNicoAt(effectiveMs);
    const overlay = document.getElementById("player-nico-overlay");
    const video = document.getElementById("player-video");
    if (!overlay || !video) return;
    if (overlay.clientWidth === 0 || overlay.clientHeight === 0) {
      // The player panel is hidden (another app tab is active) but `timeupdate`
      // keeps firing. Un-anchor instead of returning: leaving the cursor parked
      // would make every message in the gap arrive >NICO_MAX_LATENESS_MS late and
      // newer than the anchor, i.e. a climbing bogus drop count and a dead
      // overlay while it grinds through them. The next visible tick re-seeds.
      // The clear is required — _resetNicoCursor frees the lanes, which must not
      // happen under elements that are still flying.
      this.clearNicoOverlay();
      this.nicoCursor = -1;
      return;
    }
    const geo = this._nicoGeo;
    // Visible but not measured yet (a tick that beats `loadedmetadata`), or a
    // zero-sized video. Distinct from hidden: there is nothing to place, but the
    // cursor is still valid, so this must NOT clear or un-anchor.
    if (!geo || !geo.width || !geo.height) return;
    const stageW = geo.width;
    const laneHeight = geo.laneHeight;
    const ctx = {
      stageW,
      laneHeight,
      rate: video.playbackRate || 1,
      paused: video.paused && !video.ended,
      overlay,
    };

    // 1. Deferred entries first (oldest first) — no head-of-line blocking: an
    //    entry that still finds no lane stays pending, one that is now too late
    //    is dropped (and counted when it is newer than the anchor), and the
    //    ones behind it are still tried this tick. A retry is placed at the
    //    CURRENT time (retry mode, see _placeEntry) and reuses the element and
    //    measurements taken at first sight — no rebuild, no re-measure.
    const stillPending = [];
    for (const entry of this._nicoPending) {
      if (effectiveMs - entry.msg.offsetMs > NICO_MAX_LATENESS_MS) {
        this._countNicoDrop(entry.msg); // entry (and its detached element) is discarded
        continue;
      }
      if (!this._placeEntry(entry, effectiveMs, ctx, true)) stillPending.push(entry);
    }
    this._nicoPending = stillPending;

    // 2. New messages up to the per-tick cap; the cursor ALWAYS advances.
    //    The cap bounds DOM WORK, not the walk: skipping a too-late message
    //    builds nothing, so it must not consume a slot — otherwise a backlog
    //    would drain at only NICO_MAX_PER_TICK per tick, leaving the overlay
    //    dead for seconds. Skips are free, so any backlog clears in one tick.
    let work = 0;
    while (this.nicoCursor < messages.length && messages[this.nicoCursor].offsetMs <= effectiveMs) {
      const msg = messages[this.nicoCursor];
      if (effectiveMs - msg.offsetMs > NICO_MAX_LATENESS_MS) {
        this.nicoCursor++;
        this._countNicoDrop(msg);
        continue;
      }
      if (work++ >= NICO_MAX_PER_TICK) break; // cursor stays put — retried next tick
      this.nicoCursor++;
      const entry = this._prepareNico(msg, ctx);
      if (!entry) continue; // nothing renderable (system-only message)
      if (!this._placeEntry(entry, effectiveMs, ctx, false)) this._nicoPending.push(entry);
    }

    this._updateNicoDropPill();
  }

  /**
   * Count a message the overlay could not show. Messages at or before the last
   * anchor are seed-window skips, not drops — they are not reported.
   */
  _countNicoDrop(msg) {
    if (msg.offsetMs > this._nicoAnchorMs) this.nicoDropped++;
  }

  /**
   * Build and measure `msg` once. The element is parked off-stage at the right
   * edge and appended, because offsetWidth/offsetHeight need layout. The entry
   * is then handed to `_placeEntry`, which either flies it this tick or detaches
   * it and hands it back for the caller to retry — so a deferred message is
   * never rebuilt or re-measured.
   * @param {object} msg
   * @param {{stageW: number, overlay: HTMLElement}} ctx
   * @returns {{msg: object, el: HTMLElement, w: number, h: number}|null} null when
   *   the message has no renderable content.
   */
  _prepareNico(msg, { stageW, overlay }) {
    const el = this._buildNicoEl(msg);
    if (!el) return null;
    el.style.left = `${stageW}px`;
    el.style.top = "0";
    overlay.appendChild(el);
    return { msg, el, w: el.offsetWidth, h: el.offsetHeight };
  }

  /**
   * Try to put a prepared entry on stage. Returns true when it is flying, false
   * when every candidate lane is busy — the element is detached and the caller
   * keeps the entry for a later tick.
   *
   * Two placement modes, because the allocator's clock only ever moves forward:
   * - FIRST sight (`retry` false): allocate at the MESSAGE's own time and start
   *   the animation mid-flight by its lateness, so late and seeded messages
   *   appear where they would have been rather than bunching at the right edge.
   * - RETRY (`retry` true): the entry was already rejected once, so its own time
   *   is in the past and every lane's occupancy has only grown newer since —
   *   re-asking at `msg.offsetMs` could never succeed and deferral would be a
   *   no-op. A retry therefore allocates at the CURRENT time and spawns at the
   *   right edge; the allocator's ordinary two-edge bound at placement time is
   *   what keeps it collision-free.
   * @param {{msg: object, el: HTMLElement, w: number, h: number}} entry
   * @param {number} effectiveMs
   * @param {{stageW: number, laneHeight: number, rate: number, paused: boolean, overlay: HTMLElement}} ctx
   * @param {boolean} retry
   * @returns {boolean}
   */
  _placeEntry(entry, effectiveMs, { stageW, laneHeight, rate, paused, overlay }, retry) {
    const { msg, el, w, h } = entry;
    const lanesNeeded = Math.max(1, Math.ceil(h / laneHeight));
    const lane = this._lanes.allocate({
      nowMs: retry ? effectiveMs : msg.offsetMs,
      widthPx: w,
      stageWidthPx: stageW,
      durationMs: NICO_DURATION_MS,
      lanesNeeded,
      gapMs: NICO_LANE_GAP_MS,
    });
    if (lane === -1) {
      el.remove(); // a no-op on a repeat retry — the element is already detached
      return false;
    }
    if (!el.isConnected) {
      // Re-attach the element rejected on an earlier tick (measurements reused).
      el.style.left = `${stageW}px`;
      overlay.appendChild(el);
    }
    el.style.top = `${lane * laneHeight}px`;
    const anim = el.animate(
      [{ transform: "translateX(0)" }, { transform: `translateX(-${stageW + w}px)` }],
      { duration: NICO_DURATION_MS, fill: "forwards" },
    );
    anim.currentTime = retry
      ? 0
      : Math.min(Math.max(0, effectiveMs - msg.offsetMs), NICO_DURATION_MS - 1);
    anim.playbackRate = rate;
    if (paused) anim.pause();
    anim.onfinish = () => {
      this._nicoAnims.delete(anim);
      el.remove();
    };
    this._nicoAnims.add(anim);
    return true;
  }

  /**
   * Build an overlay element for `msg`, or null when it has no renderable content.
   * @param {object} msg
   * @returns {HTMLElement|null}
   */
  _buildNicoEl(msg) {
    const el = document.createElement("div");
    el.className = "nico-message";
    if (msg.messageType === "announcement") {
      el.classList.add("announcement", `announcement-${announcementColorClass(msg.announcementColor)}`);
    }
    this.appendChatContent(el, msg.message || [], msg.emotes);
    if (!el.hasChildNodes()) return null;
    // Nico emotes must load immediately — override the default lazy loading set
    // by _createEmoteImg (right for the sidebar's thousands of off-screen
    // messages, wrong for emotes that cross the screen in a few seconds).
    el.querySelectorAll(".chat-emoji").forEach((img) => { img.loading = "eager"; });
    return el;
  }

  /** Show "+N not shown" for a few seconds whenever the drop counter grew. */
  _updateNicoDropPill() {
    const pill = document.getElementById("player-nico-dropped");
    if (!pill) return;
    if (this.nicoDropped === this._nicoDroppedShown) return;
    this._nicoDroppedShown = this.nicoDropped;
    pill.textContent = `+${this.nicoDropped} not shown`;
    pill.hidden = false;
    clearTimeout(this._nicoDropPillTimer);
    this._nicoDropPillTimer = setTimeout(() => { pill.hidden = true; }, 3000);
  }

  /**
   * Safely append chat message content as DOM nodes (no innerHTML).
   * Handles both YouTube MessagePart[] and Twitch string messages.
   * @param {HTMLElement} container — element to append nodes into
   * @param {Array|string} parts — message parts or Twitch plain string
   * @param {Array} [twitchNativeEmotes] — native Twitch emotes from IRC tags
   */
  appendChatContent(container, parts, twitchNativeEmotes) {
    if (typeof parts === "string") {
      this._appendTwitchMessage(container, parts, twitchNativeEmotes);
      return;
    }
    if (!Array.isArray(parts)) return;

    for (const part of parts) {
      if (part.type === "emoji" && part.emojiUrl) {
        const alt = part.text || part.emojiId || "";
        const url = part.emojiUrl.replace(/=[^/]*$/, "");
        if (/^https?:\/\//i.test(url)) {
          container.appendChild(this._createEmoteImg(url, alt));
        } else {
          container.appendChild(document.createTextNode(alt));
        }
      } else {
        container.appendChild(document.createTextNode(part.text || ""));
      }
    }
  }

  /**
   * Create a safe emote <img> element.
   * @param {string} url — must be http/https
   * @param {string} alt
   * @returns {HTMLImageElement}
   */
  _createEmoteImg(url, alt) {
    const img = document.createElement("img");
    img.className = "chat-emoji";
    img.src = url;
    img.alt = alt;
    img.loading = "lazy";
    img.referrerPolicy = "no-referrer";
    // Fall back to alt text if the emote CDN returns 404 or is unreachable
    img.onerror = () => { img.replaceWith(document.createTextNode(alt || "")); };
    return img;
  }

  /**
   * Append Twitch message content as DOM nodes with native + 3rd-party emotes.
   * @param {HTMLElement} container
   * @param {string} message
   * @param {Array} [nativeEmotes]
   */
  _appendTwitchMessage(container, message, nativeEmotes) {
    if (!message) return;

    // No emote data at all — fast path
    if ((!nativeEmotes || nativeEmotes.length === 0) && this.twitchEmoteMap.size === 0) {
      container.appendChild(document.createTextNode(message));
      return;
    }

    // If no native emotes, just do word-by-word 3rd-party lookup
    if (!nativeEmotes || nativeEmotes.length === 0) {
      this._appendTwitchWords(container, message);
      return;
    }

    // Sort native emotes by start position
    const sorted = [...nativeEmotes].sort((a, b) => a.start - b.start);
    let cursor = 0;

    for (const emote of sorted) {
      // Text before this emote — check for 3rd-party emotes
      if (emote.start > cursor) {
        this._appendTwitchWords(container, message.substring(cursor, emote.start));
      }
      // Native Twitch emote
      const url = `https://static-cdn.jtvnw.net/emoticons/v2/${encodeURIComponent(emote.id)}/default/dark/2.0`;
      container.appendChild(this._createEmoteImg(url, emote.name));
      cursor = emote.end + 1;
    }

    // Remaining text after last native emote
    if (cursor < message.length) {
      this._appendTwitchWords(container, message.substring(cursor));
    }
  }

  // ── Resume dialog & watch tracking ──────────────────────────────────

  _showResumeDialog(jobId, resumeSeconds) {
    const wrapper = document.getElementById("player-video-wrapper");
    // Remove any existing overlay and tear down any prior keydown handler
    this._dismissResumeDialog();

    const formatted = formatTimestamp(resumeSeconds);
    const overlay = document.createElement("div");
    overlay.className = "resume-overlay";
    overlay.setAttribute("role", "dialog");
    overlay.setAttribute("aria-label", "Resume playback");
    overlay.innerHTML = `
      <div class="resume-overlay-content">
        <p>Resume where you left off?</p>
        <div class="resume-actions">
          <sl-button variant="primary" size="medium" id="resume-continue">
            <sl-icon slot="prefix" name="play-fill"></sl-icon> Resume from ${formatted}
          </sl-button>
          <sl-button variant="neutral" size="medium" id="resume-start">
            Start from beginning
          </sl-button>
        </div>
      </div>
    `;
    wrapper.appendChild(overlay);

    // Bind all handlers through an AbortController so they can be torn down
    // cleanly on job switch / clearPlayer, not just Escape/click inside the overlay.
    this._resumeDialogAbort = new AbortController();
    const sig = this._resumeDialogAbort.signal;

    const dismiss = () => this._dismissResumeDialog();

    document.addEventListener("keydown", (e) => {
      if (e.key === "Escape") {
        dismiss();
        // Start from beginning on Escape (same as clicking "Start from beginning")
        safePlay(document.getElementById("player-video"));
        this._startWatchTracking(jobId);
      }
    }, { signal: sig });

    overlay.querySelector("#resume-continue").addEventListener("click", () => {
      dismiss();
      const video = document.getElementById("player-video");
      if (this._seg.active) {
        this._seg.seekToGlobalTime(resumeSeconds, video);
      } else {
        video.currentTime = resumeSeconds;
      }
      safePlay(video);
      this._startWatchTracking(jobId);
    }, { signal: sig });

    overlay.querySelector("#resume-start").addEventListener("click", () => {
      dismiss();
      safePlay(document.getElementById("player-video"));
      this._startWatchTracking(jobId);
    }, { signal: sig });

    // Focus the primary action
    requestAnimationFrame(() => {
      overlay.querySelector("#resume-continue")?.focus();
    });
  }

  /**
   * Tear down the resume-dialog overlay and any associated listeners.
   * Safe to call even if no overlay is currently shown.
   */
  _dismissResumeDialog() {
    const wrapper = document.getElementById("player-video-wrapper");
    wrapper?.querySelector(".resume-overlay")?.remove();
    if (this._resumeDialogAbort) {
      this._resumeDialogAbort.abort();
      this._resumeDialogAbort = null;
    }
  }


  _startWatchTracking(jobId) {
    this._clearWatchTracking();

    const video = document.getElementById("player-video");

    // Periodic save every 10 seconds
    this._watchSaveInterval = setInterval(() => {
      if (!video || video.paused || document.hidden) return;
      const pos = this._seg.active ? this._seg.getGlobalTime(video) : video.currentTime;
      this._saveResumePosition(jobId, pos);
      this._checkWatched(jobId, video);
    }, 10000);

    // Save on pause
    this._onPauseSave = () => {
      const pos = this._seg.active ? this._seg.getGlobalTime(video) : video.currentTime;
      this._saveResumePosition(jobId, pos);
    };
    video.addEventListener("pause", this._onPauseSave);

    // Check watched on seek (handles seek-to-end)
    this._onSeekedWatch = () => this._checkWatched(jobId, video);
    video.addEventListener("seeked", this._onSeekedWatch);

    // Save on tab close
    this._onBeforeUnload = () => {
      const pos = this._seg.active ? this._seg.getGlobalTime(video) : video.currentTime;
      if (!isFinite(pos) || pos <= 0) return;
      const blob = new Blob([JSON.stringify({ position: pos })], { type: "application/json" });
      navigator.sendBeacon(`/api/jobs/${jobId}/resume-position`, blob);
    };
    window.addEventListener("beforeunload", this._onBeforeUnload);
  }

  _clearWatchTracking() {
    if (this._watchSaveInterval) {
      clearInterval(this._watchSaveInterval);
      this._watchSaveInterval = null;
    }
    const video = document.getElementById("player-video");
    if (this._onPauseSave) {
      video?.removeEventListener("pause", this._onPauseSave);
      this._onPauseSave = null;
    }
    if (this._onSeekedWatch) {
      video?.removeEventListener("seeked", this._onSeekedWatch);
      this._onSeekedWatch = null;
    }
    if (this._onBeforeUnload) {
      window.removeEventListener("beforeunload", this._onBeforeUnload);
      this._onBeforeUnload = null;
    }
  }

  _saveResumePosition(jobId, position) {
    if (!isFinite(position) || position <= 0) return;
    fetch(`/api/jobs/${jobId}/resume-position`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ position }),
    }).catch(() => {}); // Fire-and-forget

    // Update local state so the details pill reflects the current position
    // without needing a WebSocket broadcast (resume saves are silent)
    if (this.playerJob) this.playerJob.resumePosition = position;
    this.app._updateJobResumePosition(jobId, position);
  }

  _checkWatched(jobId, video) {
    if (this._watchedTriggered) return;

    let currentPos, totalDuration;
    if (this._seg.active) {
      currentPos = this._seg.getGlobalTime(video);
      totalDuration = this._seg.totalDuration;
    } else {
      currentPos = video.currentTime;
      // Prefer lengthSeconds from job metadata; fall back to video element duration
      const jobLen = this.playerJob?.lengthSeconds;
      totalDuration = (jobLen && jobLen > 0) ? jobLen : video.duration;
    }

    if (!totalDuration || !isFinite(totalDuration)) return;

    const withinThreshold =
      (totalDuration > 60 && totalDuration - currentPos <= 30) ||
      (currentPos / totalDuration >= 0.95);

    if (withinThreshold) {
      this._watchedTriggered = true;
      this._clearWatchTracking();
      fetch(`/api/jobs/${jobId}/watched`, { method: "POST" }).catch(() => {});
    }
  }

  /**
   * Append a text segment as DOM nodes with word-by-word 3rd-party emote lookup.
   * @param {HTMLElement} container
   * @param {string} text
   */
  _appendTwitchWords(container, text) {
    if (!text) return;
    if (this.twitchEmoteMap.size === 0) {
      container.appendChild(document.createTextNode(text));
      return;
    }

    // Split preserving whitespace tokens
    const tokens = text.split(/(\s+)/);
    for (const token of tokens) {
      if (/^\s+$/.test(token)) {
        container.appendChild(document.createTextNode(token));
        continue;
      }
      const url = this.twitchEmoteMap.get(token);
      if (url && /^https?:\/\//i.test(url)) {
        container.appendChild(this._createEmoteImg(url, token));
      } else {
        container.appendChild(document.createTextNode(token));
      }
    }
  }

}
