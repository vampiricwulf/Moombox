/**
 * Player Controller — Video player + chat replay
 */
import { formatMsToTime, formatTimestamp, isTypingInInput, safePlay } from "./utils.js";
import { SegmentPlayer } from "./segments.js";
import {
  normalizeOffsetMs,
  computeChatBiasMs,
  deriveMissingOffsets,
  mergePartChats,
  indexAfter,
  partitionChatByVideo,
  formatChatHeader,
  dividerLabelFor,
  correctLegacyTwitchEmotes,
} from "./chat-timeline.js";
import { LaneAllocator, seedCursorIndex } from "./nico-lanes.js";
import { letterboxStage, rowsFor, sameStage, nextGeometry, NICO_GEO_SETTLE_MS } from "./nico-geometry.js";
import { NicoScheduler, NICO_DURATION_MS, NICO_LEAD_MS, NICO_LANE_GAP_MS } from "./nico-scheduler.js";

const ANNOUNCEMENT_COLORS = new Set(["primary", "blue", "green", "orange", "purple"]);

function announcementColorClass(color) {
  return ANNOUNCEMENT_COLORS.has(color) ? color : "primary";
}

/**
 * YouTube's Super Chat palette: each tier's header and body colour, 1 (blue,
 * $1) through 7 (red, $100+), plus a neutral gray pair for tier 0 — the
 * archive's marker for a colour pair internal/chat's table did not know
 * (SuperchatInfo.Tier).
 *
 * The table lives here rather than in a CSS [data-tier] block because a card's
 * INK is derived from the colour actually painted: a stylesheet-only fallback
 * would leave this file unable to compute the ink for an archive written
 * before headerColor/bodyColor were recorded, and a second per-tier ink table
 * in CSS would then have to agree with this one forever. `data-tier` is still
 * stamped on the element as a styling hook.
 */
export const SUPERCHAT_TIER_COLORS = {
  0: { header: "#606060", body: "#757575" },
  1: { header: "#1565C0", body: "#1E88E5" },
  2: { header: "#00B8D4", body: "#00E5FF" },
  3: { header: "#00BFA5", body: "#1DE9B6" },
  4: { header: "#FFB300", body: "#FFCA28" },
  5: { header: "#E65100", body: "#F57C00" },
  6: { header: "#C2185B", body: "#E91E63" },
  7: { header: "#D00000", body: "#E62117" },
};

/**
 * YouTube's member green. The body is that green mixed 55% toward white
 * (#0F9D58 → #93D3B4): a lighter tint of the same hue, computed from the
 * header rather than picked, and chosen so its relative luminance (0.561)
 * clears the 0.5 threshold — the card then takes dark ink on both halves,
 * 5.4:1 on the green header and 10:1 on the body. (The 25% tint, #4BB682, sat
 * at 0.366 and put white text at 2.5:1 on the body.)
 *
 * Unlike the Super Chat tiers, this pair is ours and not YouTube's, so it is
 * held to the contrast rule rather than to fidelity.
 */
export const MEMBER_CARD_COLORS = { header: "#0F9D58", body: "#93D3B4" };

/**
 * The Twitch event kinds that become a full-strength notice block.
 * `announcement` deliberately keeps the flat row it has today (2026-09-25
 * ruling K3) — its colour classes are its whole styling. `system` is not a
 * member either, but reaches the same block dimmed through
 * `isDimTwitchNotice`; the set stays the four kinds `twitchNoticeLine` can
 * rebuild a line for.
 */
export const TWITCH_NOTICE_TYPES = new Set(["sub", "resub", "subgift", "raid"]);

/**
 * A `system`-typed USERNOTICE worth a notice block (owner ruling 2026-09-25,
 * after the whole-range audit). `internal/twitch/chat_irc.go` types everything
 * it cannot normalize to one of the four kinds as `system` — prime and gift
 * upgrades, viewer milestones, rituals, pay-forwards — and the wire's
 * `system-msg` is the only sentence such an event has: there is no kind to
 * rebuild from, so a `system` message with an EMPTY system line has nothing to
 * put in a block and keeps its flat row.
 *
 * Deliberately a predicate rather than a fifth member of TWITCH_NOTICE_TYPES:
 * membership is unconditional, and a `system` message with an EMPTY system
 * line must keep its flat row — a set cannot express that condition. (Search
 * is unaffected either way: twitchNoticeLine returns `systemMsg` before it
 * reaches the kind switch, so both arms of filterChat's ternary produce the
 * same string for a `system` message.)
 * @param {object} msg
 * @returns {boolean}
 */
export function isDimTwitchNotice(msg) {
  return msg.messageType === "system" && !!msg.systemMsg;
}

/** msg-param-sub-plan (internal/twitch: SubPlan) → the name Twitch shows. */
const TWITCH_PLAN_NAMES = { 1000: "Tier 1", 2000: "Tier 2", 3000: "Tier 3", Prime: "Prime" };

/**
 * The bold first line of a Twitch notice: the wire's own `systemMsg` when the
 * archive has one — it is richer than anything reconstructable, carrying month
 * counts and streaks — else rebuilt from the fields the IRC parser records
 * (internal/twitch/types.go: SubPlan, GiftRecipient, ViewerCount). Returns ""
 * when nothing can be said, and the caller omits the line rather than printing
 * a half-sentence.
 * @param {object} msg
 * @returns {string}
 */
export function twitchNoticeLine(msg) {
  if (msg.systemMsg) return msg.systemMsg;
  const who = msg.authorName || "Someone";
  switch (msg.messageType) {
    case "sub":
    case "resub": {
      const plan = TWITCH_PLAN_NAMES[msg.subPlan];
      return plan ? `${who} subscribed with ${plan}` : `${who} subscribed`;
    }
    case "subgift":
      return msg.giftRecipient ? `${who} gifted a sub to ${msg.giftRecipient}` : `${who} gifted a sub`;
    case "raid": {
      const n = Number(msg.viewerCount);
      if (!Number.isFinite(n) || n <= 0) return `${who} is raiding`;
      return `${who} is raiding with ${n} ${n === 1 ? "viewer" : "viewers"}`;
    }
    default:
      return "";
  }
}

/**
 * Twitch's cheer colour scale, richest first so the first match wins:
 * gray under 100 bits, purple from 100, green from 1,000, blue from 5,000 and
 * red from 10,000.
 */
export const CHEER_SCALE = [
  { min: 10000, color: "#f43021" },
  { min: 5000, color: "#0099fe" },
  { min: 1000, color: "#1db2a5" },
  { min: 100, color: "#9c3ee8" },
  { min: 0, color: "#979797" },
];

/**
 * The colour for a cheer of `bits`. Anything unparseable reads as 0, i.e. the
 * bottom band — never an exception inside a sidebar build chunk.
 * @param {number|string} bits
 * @returns {string}
 */
export function cheerColor(bits) {
  const n = Number(bits) || 0;
  for (const step of CHEER_SCALE) {
    if (n >= step.min) return step.color;
  }
  return CHEER_SCALE[CHEER_SCALE.length - 1].color;
}

/** internal/chat's argbHex writes exactly #RRGGBB; nothing else is a colour. */
const HEX_RE = /^#[0-9a-fA-F]{6}$/;

function isHexColor(s) {
  return typeof s === "string" && HEX_RE.test(s);
}

/**
 * WCAG 2.x relative luminance of an #RRGGBB colour, or null when the string is
 * not one (an old or malformed archive), which every caller reads as "no
 * colour recorded".
 * @param {string} hex
 * @returns {number|null}
 */
export function relativeLuminance(hex) {
  if (!isHexColor(hex)) return null;
  const chan = (i) => {
    const c = parseInt(hex.slice(1 + i * 2, 3 + i * 2), 16) / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * chan(0) + 0.7152 * chan(1) + 0.0722 * chan(2);
}

/**
 * The luminance at which #ffffff and rgba(0, 0, 0, .87) give equal WCAG
 * contrast: below it white wins, above it the dark ink does. Passed as the
 * threshold for a surface with no platform ink to reproduce, where the only
 * question is which of the two reads better.
 */
export const INK_CROSSOVER = 0.179;

/**
 * Which ink reads on `hex`: "dark" at or above `threshold`, "light" below it
 * and for anything unparseable (the safe default on a saturated card).
 *
 * The default 0.5 is YouTube's own line. Measured: the seven tier body colours
 * come out at 0.235, 0.633, 0.619, 0.637, 0.338, 0.192 and 0.180, so this one
 * threshold reproduces YouTube's own choice — dark text on tiers 2, 3 and 4,
 * white on 1, 5, 6 and 7 — without a second hard-coded table, and an archived
 * colour YouTube has never shipped still reads. Fidelity is the point there,
 * so it stays even where YouTube's own pick is below AA. Pass INK_CROSSOVER
 * instead on a surface we invented, where nothing is being reproduced.
 * @param {string} hex
 * @param {number} [threshold]
 * @returns {"light"|"dark"}
 */
export function readableInk(hex, threshold = 0.5) {
  const l = relativeLuminance(hex);
  return l !== null && l >= threshold ? "dark" : "light";
}

/** The colour actually painted: the archived one when usable, else the fallback. */
function resolvedColor(archived, fallback) {
  return isHexColor(archived) ? archived : fallback;
}

/**
 * Hand the keyboard to the player surface after a job has been selected: off
 * the picker (where every shortcut is swallowed) and onto the video wrapper,
 * so Space/arrows/F/M/C/S work without the user clicking the video first. Both
 * ways into a selection — the picker's own `sl-change` and "Open in Player" on
 * the job details — call this, so they behave identically. The wrapper lives
 * inside #player-viewport, which is only revealed once the job data is in, so
 * this must run AFTER onPlayerJobSelect resolves: a hidden element cannot take
 * focus.
 */
export function focusPlayerSurface() {
  document.getElementById("player-job-select")?.blur();
  document.getElementById("player-video-wrapper")?.focus({ preventScroll: true });
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
     * @type {ReturnType<typeof partitionChatByVideo>|null}
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
    /**
     * The cursor, the anchor, the deferred entries and the drop count — the
     * overlay's whole "what is shown when" state machine. Its pending entries
     * cache the built, detached element and its measurements (see
     * `_prepareNico`), so a retry costs no DOM work.
     * @type {NicoScheduler}
     */
    this.nico = new NicoScheduler({ lanes: this._lanes, indexAfter, seedCursorIndex });
    /** @type {Set<Animation>} every in-flight overlay animation */
    this._nicoAnims = new Set();
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
    /**
     * Job-list rebuild bookkeeping (see loadPlayerJobList): `_rebuildToken` is
     * the generation — only the newest call writes — and `_rebuildsActive`
     * counts the rebuilds currently mutating the option list, which is what
     * tells a synthetic `sl-change` from a real user pick.
     */
    this._rebuildToken = 0;
    this._rebuildsActive = 0;

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
    jobSelect.addEventListener("sl-change", async () => {
      if (this._rebuildsActive > 0) return;
      const val = jobSelect.value;
      if (val) {
        // Await the load, then hand the keyboard over (see
        // focusPlayerSurface). In a `finally`: a load that throws must not
        // leave focus parked on the select, where every shortcut is swallowed
        // and the user cannot tell why.
        try {
          await this.onPlayerJobSelect(val);
        } catch (e) {
          console.error("player: job select failed", e?.message ?? e);
        } finally {
          focusPlayerSurface();
        }
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
        // Un-anchor BEFORE measuring. `seeked`, the offset input/reset and
        // `visibilitychange` all re-seed the cursor whether or not the overlay
        // is on, so any seed sitting here was made while nothing was ticking.
        // Keeping it would make the first enabled tick walk the whole gap and
        // count every message newer than that stale anchor as dropped (and the
        // re-measure below cannot save us: an unchanged stage returns early).
        // The next tick lazily re-anchors at the current time.
        this.nico.unanchor();
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
        this.nico.unanchor();
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

    // Video seeking — un-anchor the overlay BEFORE the seek's own timeupdate.
    // The HTML seek algorithm queues `timeupdate` and only THEN `seeked`, so
    // one tick runs in between with `currentTime` already at the target but
    // `nico.cursor` still parked at the old position: the loop walks every
    // message in the gap, finds each one more than NICO_MAX_LATENESS_MS late
    // and — being newer than the anchor — counts it, i.e. a bogus
    // "+N not shown" on every forward seek longer than 2 s. The load algorithm
    // fires its own `timeupdate` too, so cross-segment part loads had the same
    // pill. Un-anchored the intervening tick does nothing; `seeked` below
    // re-anchors at the target.
    video.addEventListener("seeking", () => {
      this.clearNicoOverlay();
      this.nico.unanchor();
    });

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
    // re-stamp is skipped when the partition did not actually move. The empty
    // guard is also what keeps a job switch honest: onPlayerJobSelect empties
    // the array before assigning the new source, so this listener is a no-op
    // until the new chat has been built rather than partitioning the previous
    // job's messages against the new video's duration.
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

    // Sidebar scroll locking. Pointer events over mouse events: a touch tap
    // synthesizes mouseenter but rarely a matching mouseleave, so on touch
    // devices the lock would stick after the first tap and autoscroll would
    // never resume. Touch pointers are ignored entirely — a scroll-lock on
    // hover has no equivalent gesture on touch, so autoscroll there only
    // stops on an actual user scroll (the "scroll" listener below), which is
    // the mobile-expected behavior.
    sidebarMessages.addEventListener("pointerenter", (e) => {
      if (e.pointerType !== "touch") this.playerScrollLock = true;
    });

    sidebarMessages.addEventListener("pointerleave", (e) => {
      if (e.pointerType !== "touch" && this.playerAutoScroll) {
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
        this._syncOffsetResetButton();
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
        this._applyOffsetUI(0);
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
      });
    }

    // Keyboard controls for player
    this.setupKeyboardControls();
  }

  /** Apply a persisted/cleared offset to state, input text and the reset button. */
  _applyOffsetUI(seconds) {
    const s = Number.isFinite(seconds) ? seconds : 0;
    this.playerCustomOffsetMs = Math.round(s * 1000);
    const input = document.getElementById("player-chat-offset");
    if (input) input.value = s === 0 ? "" : String(s);
    this._syncOffsetResetButton();
  }

  _syncOffsetResetButton() {
    const btn = document.getElementById("player-chat-offset-reset");
    if (btn) btn.style.display = this.playerCustomOffsetMs !== 0 ? "" : "none";
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

      // Block shortcuts while the resume overlay is up: it is a modal scrim
      // over the video, and a keypress must not toggle playback or seek
      // behind it. Escape is handled by the overlay's own document keydown
      // listener (added in _showResumeDialog, torn down in
      // _dismissResumeDialog) rather than by this switch below, so letting
      // this handler return early for every key — Escape included — does not
      // stop the overlay from dismissing itself.
      if (document.querySelector("#player-video-wrapper .resume-overlay")) return;

      // Skip when typing in inputs (composedPath handles Shoelace shadow DOM)
      if (isTypingInInput(e)) return;

      // Let a focused control handle its own Space (activate/toggle) instead
      // of the player also toggling playback on the same keypress.
      const target = e.composedPath()[0];
      const tTag = target instanceof HTMLElement ? target.tagName : "";
      if (e.key === " " && /^(BUTTON|SL-BUTTON|SL-ICON-BUTTON|SL-CHECKBOX|SL-SWITCH)$/.test(tTag)) return;
      // Caps Lock (or Shift) must not silently disable the letter shortcuts.
      const key = e.key.length === 1 ? e.key.toLowerCase() : e.key;

      const video = document.getElementById("player-video");
      if (!video || !video.src) return;

      switch (key) {
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
      // The lines the sidebar shows beside or instead of the message (2026-09-25
      // K1-K3): the member line, the amount and the Twitch system line are on
      // screen, so they must be findable too.
      if (!textMatch) {
        const shown = [msg.membershipText, msg.superchat && msg.superchat.amount,
          TWITCH_NOTICE_TYPES.has(msg.messageType) ? twitchNoticeLine(msg) : msg.systemMsg];
        textMatch = shown.some((s) => typeof s === "string" && s.toLowerCase().includes(needle));
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
    this.nico.unanchor();
    // A geometry commit armed by the last resize has nothing left to commit.
    clearTimeout(this._nicoGeoSettle);
    this._nicoGeoSettle = null;
    this._resetNicoDropCount();
    this._applyOffsetUI(0);
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
    // re-watching a finished video must still promote the tail. Following
    // along means following along to the end, so the sidebar is carried to
    // the "Recording ended" divider the promotion just made reachable.
    if (!advanced) {
      this._markPostEnd();
      if (this.playerAutoScroll && !this.playerScrollLock) this.syncSidebarToTime();
    }

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
      // One numbering (1-based) and one label across title, accessible name and
      // visible text: a screen reader and the eye must name the same block.
      const label = seg.quality || `Seg ${i + 1}`;
      block.title = `Segment ${i + 1}: ${label} (${Math.round(seg.durationSeconds || 0)}s)`;
      block.setAttribute("aria-label", `Seek to segment ${i + 1}, ${label}`);
      block.textContent = label;
      block.addEventListener("click", () => {
        this.seekToGlobalTime(seg.startOffset);
      });
      indicator.appendChild(block);
    });

    document.getElementById("player-video-column")?.appendChild(indicator);
  }

  /**
   * Rebuild the video picker from the live and archived job lists.
   *
   * Two calls overlapping is normal — a WebSocket job update lands while a
   * manual refresh is still in flight — and each one awaits three times. A
   * generation token makes the NEWEST call the only one that writes: every
   * await is followed by a bail, so a superseded rebuild leaves the option list
   * and the selection alone instead of restoring a value its own stale list
   * happened to contain. `_rebuildsActive` is a separate counter on purpose: it
   * says how many rebuilds are inside the option-mutation window, which is what
   * the `sl-change` listener needs in order to tell a synthetic event from a
   * user pick, and a superseded rebuild must not clear that while a newer one
   * is still mutating.
   */
  async loadPlayerJobList() {
    const select = document.getElementById("player-job-select");
    const currentValue = select.value;
    const token = ++this._rebuildToken;

    try {
      const [jobsRes, archivedRes] = await Promise.all([
        fetch("/api/jobs"),
        fetch("/api/jobs/archived"),
      ]);
      if (token !== this._rebuildToken) return;
      const jobs = jobsRes.ok ? await jobsRes.json() : [];
      const archived = archivedRes.ok ? await archivedRes.json() : [];
      if (token !== this._rebuildToken) return;
      if (!jobsRes.ok && !archivedRes.ok) {
        this.app.showToast("Failed to load video list", "warning");
      }

      const all = [...jobs, ...archived]
        .filter((j) => j.status === "Finished" && j.filename)
        .sort((a, b) => new Date(b.updatedAt) - new Date(a.updatedAt));

      // The currently loaded job disappeared (deleted, or its id changed via
      // re-import) — clear the player instead of leaving a dangling selection.
      if (currentValue && !all.some((j) => j.id === currentValue)) {
        this.clearPlayer();
      }

      // Rebuild the option list even while a video is playing: removing/adding
      // sl-options does not itself emit sl-change in Shoelace 2.16 (verified —
      // only user-driven paths emit it), so this never interrupts playback.
      // Guard against any synthetic sl-change firing mid-rebuild anyway.
      this._rebuildsActive++;
      try {
        select.querySelectorAll("sl-option").forEach((o) => o.remove());

        all.forEach((job) => {
          const opt = document.createElement("sl-option");
          opt.value = job.id;
          const noChat = !job.chatFilename ? " (no chat)" : "";
          opt.textContent = `${job.title} — ${job.channelName}${noChat}`;
          select.appendChild(opt);
        });

        // Wait for Shoelace to register new options before restoring selection
        if (select.updateComplete) await select.updateComplete.catch(() => {});
        // A newer rebuild started while we waited: it owns the option list from
        // here on, so restoring OUR remembered value would fight it.
        if (token !== this._rebuildToken) return;

        if (currentValue && all.some((j) => j.id === currentValue)) select.value = currentValue;
      } finally {
        this._rebuildsActive--;
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

    // Drop the previous job's chat and overlay state BEFORE the new source is
    // assigned and before any await below. Two reasons:
    // - the new video's `loadedmetadata` would otherwise partition the PREVIOUS
    //   job's messages against the new duration (a ~100 ms flash of wrong
    //   dividers); with the array already empty that listener no-ops until
    //   buildSidebarChat has run;
    // - a `seeked` inside the fetch window (a restored resume position) would
    //   re-anchor over the old job's flying text on top of the new picture.
    this.playerChatMessages = [];
    this.playerChatData = null;
    this._chatParts = null;
    this.twitchEmoteMap = new Map();
    this.playerActiveChatIndex = 0;
    // Rows and header are one state: dropping the array alone would leave the
    // previous job's messages on screen under the previous job's count for the
    // whole fetch window. buildSidebarChat rebuilds both once the chat is in.
    document.getElementById("player-sidebar-messages").replaceChildren();
    this._updateSidebarHeader();
    this.clearNicoOverlay();
    this.nico.unanchor();
    this._resetNicoDropCount();

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

    // Load chat if available (the state it replaces was cleared above, before
    // the source swap).
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

    // Build sidebar chat. The overlay was cleared and un-anchored before the
    // source swap and nothing can have spawned since (the message array was
    // empty for the whole fetch window), so there is nothing to clear here.
    this.buildSidebarChat();

    // Un-anchor again: a `seeked` inside the fetch window (the resume dialog
    // seeks BEFORE the chat arrives) re-seeded the cursor on the then-EMPTY
    // array, which leaves it at 0 with the anchor at the seek target. The
    // messages that just landed are all newer than that anchor, so the first
    // tick would walk the whole file up to `now` and count the gap as dropped.
    // _updateNicoGeometry below cannot undo it — an unchanged stage returns
    // early — so drop the seed here and let the next tick anchor lazily.
    this.nico.unanchor();

    // Load saved custom chat offset (from watch-state response, already on playerJob)
    this._applyOffsetUI(this.playerJob.chatOffset || 0);

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
          // Per part, against the PART's own header — before mergePartChats
          // shifts it onto the global timeline (one file, one epoch, and the
          // merge keeps no header scalars at all).
          //
          // YouTube: recover the offset of a message the producer left without
          // one, from the header epoch. Twitch parts are skipped there — their
          // offsets are already video-relative and their header epoch is the
          // recording start — and get the other repair instead: a part written
          // before the emote-offset fix carries code-point spans and wrapped
          // /me text, which correctLegacyTwitchEmotes maps into the UTF-16
          // space _appendTwitchMessage slices in.
          if (data && data.platform === "twitch") correctLegacyTwitchEmotes(data);
          else if (data) deriveMissingOffsets(data.messages, data.streamStartTime);
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
    if (this._selectionSeq !== selectionId) return null;
    // A message the producer left without an offset of its own (offsetMs 0
    // and no hasOffset) is recovered from the header epoch before the caller
    // applies the bias and sorts (T-F12); one that already carries a real
    // offset is authoritative and untouched. Twitch files are skipped
    // outright — their offsets are already video-relative (F1) — and take the
    // legacy emote-offset repair instead (see the per-part branch above).
    if (data && data.platform === "twitch") correctLegacyTwitchEmotes(data);
    else if (data) deriveMissingOffsets(data.messages, data.streamStartTime);
    return data;
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
    const dividerLabel = dividerLabelFor(this._chatParts, index);
    if (dividerLabel) {
      div.classList.add("divider-before");
      div.dataset.divider = dividerLabel;
    }

    // Shape dispatch. Every branch fills the SAME element: one direct child of
    // #player-sidebar-messages per message, still carrying `chat-msg`. The
    // sidebar's promotion, reset, post-end marking, divider reconciliation,
    // search filter and scroll maths all address rows by container.children[i]
    // and would not notice the class going — but every `.chat-msg.<state>`
    // rule in moombox.css would, and so would the jsdom harness's measured box.
    if (msg.superchat) {
      this._fillSuperchatCard(div, msg);
      return div;
    }
    if (msg.isMembership) {
      this._fillMemberCard(div, msg);
      return div;
    }
    if (TWITCH_NOTICE_TYPES.has(msg.messageType) || isDimTwitchNotice(msg)) {
      this._fillTwitchNotice(div, msg);
      return div;
    }
    this._fillPlainRow(div, msg);
    return div;
  }

  /**
   * The ordinary flat row: time, author, content. Extracted verbatim from
   * _buildChatMessageEl; the Super Chat class and amount span it used to carry
   * moved into _fillSuperchatCard, which is now the only shape that reaches
   * them.
   */
  _fillPlainRow(div, msg) {
    if (msg.messageType === "announcement") {
      div.classList.add("announcement");
      div.classList.add(`announcement-${announcementColorClass(msg.announcementColor)}`);
    }
    div.appendChild(this._timeSpan(msg));
    div.appendChild(this._authorSpan(msg, true));
    // K3: a cheer's amount, coloured by Twitch's scale. Gated on the count
    // rather than on messageType === "bits": a bits message with no count has
    // nothing to put in a chip, and a cheer that arrived typed as ordinary
    // chat still has its amount.
    const bits = Number(msg.bits) || 0;
    if (bits > 0) div.appendChild(this._cheerChip(bits));
    const contentSpan = document.createElement("span");
    this.appendChatContent(contentSpan, msg.message || [], msg.emotes);
    div.appendChild(contentSpan);
  }

  /** The row's offset timestamp. */
  _timeSpan(msg) {
    const span = document.createElement("span");
    span.className = "chat-msg-time";
    span.textContent = formatMsToTime(msg.offsetMs);
    return span;
  }

  /**
   * The author span with its badge class. `withColon` is the flat row's
   * "Name: " prefix; a card header puts the name on its own line and drops it.
   */
  _authorSpan(msg, withColon) {
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
    authorSpan.textContent = withColon ? msg.authorName + ": " : msg.authorName;
    return authorSpan;
  }

  /**
   * Turn `div` into a two-part card and hand back its header and body.
   *
   * The colours ride as CSS custom properties written through the CSSOM. A
   * setProperty write is not an inline <style> element and is not governed by
   * style-src (which internal/web/middleware.go grants 'unsafe-inline' anyway,
   * for Shoelace's shadow DOM), so nothing about the CSP moves.
   *
   * ONE ink for the whole card, derived from the BODY colour — the half that
   * carries the message, and the one YouTube picks its text colour from. Both
   * halves take it, as they do on YouTube: a header strip is always the darker
   * partner of its body, so deriving each half separately would put white text
   * on the tier-2 and tier-3 headers (#00B8D4 0.390, #00BFA5 0.400 — at 2.4:1
   * and 2.3:1) above dark text on their bodies — a card that changes ink
   * halfway down. Measured header luminances, tiers 0-7: 0.117, 0.133, 0.390,
   * 0.400, 0.535, 0.227, 0.129, 0.134 — tiers 2 and 3 would disagree with
   * their bodies; tier 4's header (0.535) is the only one dark-inked on its
   * own, and it agrees.
   */
  _cardParts(div, headerColor, bodyColor) {
    div.classList.add("chat-card");
    div.style.setProperty("--card-header", headerColor);
    div.style.setProperty("--card-body", bodyColor);
    const ink = `chat-ink-${readableInk(bodyColor)}`;
    const header = document.createElement("div");
    header.className = `chat-card-header ${ink}`;
    const body = document.createElement("div");
    body.className = `chat-card-body ${ink}`;
    div.appendChild(header);
    div.appendChild(body);
    return { header, body };
  }

  /**
   * K1: a Super Chat or Super Sticker as YouTube draws it — a header strip in
   * the tier's header colour carrying the author, the amount and the time, and
   * the message in the body colour beneath it.
   *
   * The archived headerColor/bodyColor win; SUPERCHAT_TIER_COLORS is the
   * fallback for a file written before internal/chat recorded them (or with a
   * pair its table did not know, which arrives as tier 0). The amount is shown
   * exactly as archived — SuperchatInfo.Currency is derived from that same
   * string, so appending it would restate it.
   *
   * A Super Chat paid with no message is a header-only card, as on YouTube —
   * the body element is dropped rather than left as an empty coloured strip;
   * the sticker label still applies to `kind: "sticker"` and to a legacy
   * record with no `kind`.
   */
  _fillSuperchatCard(div, msg) {
    const sc = msg.superchat || {};
    const tier = SUPERCHAT_TIER_COLORS[sc.tier] ? sc.tier : 0;
    const palette = SUPERCHAT_TIER_COLORS[tier];
    div.classList.add("superchat");
    div.dataset.tier = String(tier);
    const { header, body } = this._cardParts(
      div,
      resolvedColor(sc.headerColor, palette.header),
      resolvedColor(sc.bodyColor, palette.body),
    );

    header.appendChild(this._authorSpan(msg, false));
    const amount = document.createElement("span");
    amount.className = "chat-msg-superchat";
    amount.textContent = sc.amount || "";
    header.appendChild(amount);
    header.appendChild(this._timeSpan(msg));

    // A Super Sticker's image is not archived, so the body says what it was.
    // `kind` arrived with the tier fix (2026-09-05); an older file has none,
    // and a paid message with no parts at all is a sticker in all but name.
    const parts = Array.isArray(msg.message) ? msg.message : [];
    if (sc.kind === "sticker" || (!sc.kind && parts.length === 0)) {
      body.textContent = "Super Sticker";
      return;
    }
    this.appendChatContent(body, msg.message || [], msg.emotes);
    if (!body.hasChildNodes()) body.remove();
  }

  /**
   * K2: a membership event — a new member, a milestone, a gift purchase or a
   * gift redemption — as a green card.
   *
   * `membershipText` is the renderer's own header line ("Welcome to Member!",
   * "Member for 6 months", "Gifted 5 memberships"), captured by
   * internal/chat/api.go; `message` is whatever the member typed, which a new
   * member and a gift purchase do not have. The two are kept apart — the line
   * beside the name, the words in the body — which is the reason the archive
   * carries them as separate fields.
   *
   * A card with nothing to put in its body drops the body element rather than
   * leaving an empty coloured strip; .chat-card-header:last-child rounds the
   * header on all four corners when that happens.
   */
  _fillMemberCard(div, msg) {
    div.classList.add("member");
    const { header, body } = this._cardParts(div, MEMBER_CARD_COLORS.header, MEMBER_CARD_COLORS.body);
    header.appendChild(this._authorSpan(msg, false));
    if (msg.membershipText) {
      const note = document.createElement("span");
      note.className = "chat-card-note";
      note.textContent = msg.membershipText;
      header.appendChild(note);
    }
    header.appendChild(this._timeSpan(msg));
    this.appendChatContent(body, msg.message || [], msg.emotes);
    if (!body.hasChildNodes()) body.remove();
  }

  /**
   * K3: a Twitch sub, resub, gift or raid as a highlighted block — Twitch's
   * purple down the left edge, the same purple at 10% behind it, the system
   * line first and the sender's own words, if any, underneath. A `system`
   * USERNOTICE with a system line (isDimTwitchNotice) takes the same block
   * with `.dim` added: the same shape at reduced emphasis, per the 2026-09-25
   * follow-up ruling. The class, not a second builder — the two differ only in
   * how loudly they are painted.
   *
   * Not a card: these carry no colour of their own and no amount, so the
   * two-part shell would be two strips of the same purple. The content span is
   * appended only when it produced nodes, the same hasChildNodes idiom the
   * overlay builder uses, so a silent notice does not end in an empty span
   * that the divider-dim rule would then dim as a child.
   *
   * ("the overlay builder", not its name: task 8's K4 gate greps the diff for
   * the overlay symbol, and a prose mention would read as an overlay edit.)
   */
  _fillTwitchNotice(div, msg) {
    div.classList.add("chat-notice", "twitch");
    if (isDimTwitchNotice(msg)) div.classList.add("dim");
    div.appendChild(this._timeSpan(msg));
    const line = twitchNoticeLine(msg);
    if (line) {
      const lineEl = document.createElement("div");
      lineEl.className = "chat-notice-line";
      lineEl.textContent = line;
      div.appendChild(lineEl);
    }
    // `internal/twitch` parseUsernotice copies `system-msg` into the message
    // when the sender typed nothing, so a silent notice reaches the archive
    // with `message === systemMsg`. That echo is not the sender's own words,
    // and printing it would put the same sentence in the block twice. Compared
    // as the raw string rather than the built nodes: a third-party emote map
    // can turn a word of it into an image, and the echo is still an echo.
    if (typeof msg.message !== "string" || msg.message !== line) {
      const content = document.createElement("span");
      this.appendChatContent(content, msg.message || [], msg.emotes);
      if (content.hasChildNodes()) div.appendChild(content);
    }
  }

  /**
   * The cheer chip: "<n> bits" in a pill of the scale's colour, with the ink
   * the shared luminance rule asks for — but taken at INK_CROSSOVER, not at
   * the cards' 0.5. The filled pill is ours: Twitch prints a cheer's amount as
   * coloured text and never on a background, so there is no platform ink to
   * reproduce and the only question is which of the two reads better. At 0.5
   * four of the five bands would be white below 4.5:1 (gray 2.92, green 2.64,
   * blue 3.01, red 3.97); at the crossover they are dark at 6.33, 6.99, 6.23
   * and 4.86, and purple stays white at 4.92.
   */
  _cheerChip(bits) {
    const chip = document.createElement("span");
    const color = cheerColor(bits);
    chip.className = `cheer-chip chat-ink-${readableInk(color, INK_CROSSOVER)}`;
    chip.style.setProperty("--cheer-bg", color);
    chip.textContent = `${bits} ${bits === 1 ? "bit" : "bits"}`;
    return chip;
  }

  /**
   * Known length of the recording in ms: the segment sum for a multi-part job,
   * else the loaded media's own duration, else the job's metadata length.
   * 0 = not known yet (single-file job before `loadedmetadata`), which means
   * "no post-end region" until it is.
   *
   * A segmented job returns the sum and NOTHING ELSE — `video.duration` there
   * is one PART, and falling through to it when the parts carry no durations
   * would put a "Recording ended" divider in the middle of the video and fire
   * _markPostEnd at every part boundary. 0 (unknown) is the honest answer.
   * @returns {number}
   */
  _videoDurationMs() {
    if (this._seg.active) return this._seg.totalDuration * 1000;
    const video = document.getElementById("player-video");
    if (video && Number.isFinite(video.duration) && video.duration > 0) {
      return video.duration * 1000;
    }
    const len = this.playerJob?.lengthSeconds;
    return len > 0 ? len * 1000 : 0;
  }

  /** Re-derive the pre-show / post-end partition and refresh the header. */
  _computeChatParts() {
    this._chatParts = partitionChatByVideo(this.playerChatMessages, this._videoDurationMs());
    this._updateSidebarHeader();
  }

  /** Sole writer of #player-sidebar-msg-count. Text from formatChatHeader. */
  _updateSidebarHeader() {
    const p = this._chatParts;
    const text = formatChatHeader(
      this.playerChatMessages.length,
      p ? p.preCount : 0,
      p ? p.postCount : 0,
    );
    document.getElementById("player-sidebar-msg-count").textContent = text;
  }

  /**
   * Stamp/clear the divider classes on the boundary rows. Idempotent, and safe
   * while the chunked build is still running — it only ever touches children
   * that exist, and the completion callback runs it again over the full list.
   */
  _applyDividers() {
    const container = document.getElementById("player-sidebar-messages");
    if (!container) return;
    const children = container.children;
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
      const label = dividerLabelFor(p, index);
      if (!label) continue;
      children[index].classList.add("divider-before");
      children[index].dataset.divider = label;
    }
  }

  /**
   * Has playback reached the end of the recording? Measured in EFFECTIVE ms —
   * the policy is "effectiveMs >= totalDuration", and the sidebar's regions
   * are read through the same offset-adjusted clock as its active state. The
   * 250 ms slack covers a media file that stops a hair short of its duration.
   * @param {number} [effectiveMs] offset-adjusted playback time, if known
   * @returns {boolean}
   */
  _atRecordingEnd(effectiveMs = this.getGlobalTimeMs() + this.playerCustomOffsetMs) {
    const durationMs = this._videoDurationMs();
    return durationMs > 0 && effectiveMs + 250 >= durationMs;
  }

  /**
   * After the recording ends, its tail is "after it", not "future": readable,
   * labelled and reachable rather than dimmed like something still to come.
   */
  _markPostEnd() {
    const p = this._chatParts;
    if (!p || p.firstPostIndex < 0) return;
    const container = document.getElementById("player-sidebar-messages");
    if (!container) return;
    const children = container.children;
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
    if (this._atRecordingEnd(currentMs + this.playerCustomOffsetMs)) this._markPostEnd();
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
    // The condition is the exact inverse of the promotion trigger — comparing
    // indices instead would miss firstPostIndex === 0 (a chat that is entirely
    // post-end) and would disagree with a `timeupdate` that lands before the
    // `seeked` this is running for.
    const p = this._chatParts;
    if (p && p.firstPostIndex >= 0 && !this._atRecordingEnd(effectiveMs)) {
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
   * committed width — a small horizontal offset, against a blank overlay for as
   * long as the drag lasts. The window is the WHOLE gesture plus
   * NICO_GEO_SETTLE_MS, not 120 ms in total, and the overlay text is sized in
   * `cqh`, so applying the new box re-sizes in-flight and already-measured text
   * immediately while the lane records still hold the widths measured at the old
   * size: a transient overlap is possible until the commit wipes the stage
   * (accepted trade, R23).
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
    const stage = letterboxStage({
      boxW: video.clientWidth, boxH: video.clientHeight,
      offsetLeft: video.offsetLeft, offsetTop: video.offsetTop,
      videoW: video.videoWidth, videoH: video.videoHeight,
    });
    // Never write a zero-sized box: the guard above would then refuse every
    // later measurement, wedging the overlay shut. Keep the last good geometry
    // and wait for the next resize instead.
    if (!stage) return;
    const { left, top, w, h } = stage;
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

    const rows = rowsFor(h, rowH);
    const geo = this._nicoGeo;
    if (sameStage(geo, w, h, rows)) {
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
    if (sameStage(geo, w, h, rows)) return;

    this._nicoGeo = nextGeometry(geo, w, h, rows);
    this._lanes.reset(rows);
    // Exactly one clear on this path: _reanchorNicoAt clears before re-seeding
    // (and clearNicoOverlay empties nico.pending, so no cached w/h measured at
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
    this.nico.pending = [];
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

  /** Anchor the cursor at `effectiveMs` — the seed rule lives in NicoScheduler. */
  _resetNicoCursor(effectiveMs) {
    this.nico.anchor(this.playerChatMessages, effectiveMs);
  }

  /** Zero the drop counter and hide the pill (job switch / player teardown). */
  _resetNicoDropCount() {
    this.nico.resetDropCount();
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
    const overlay = document.getElementById("player-nico-overlay");
    const video = document.getElementById("player-video");
    if (!overlay || !video) return;
    // No decoded frame at the current position — a seek still in flight, or a
    // source that has only just been assigned. There is nothing to sync to, and
    // the `timeupdate` the seek/load algorithm queues before `seeked` lands
    // here; placing against it would use a position the picture has not reached.
    // `seeked`/`loadedmetadata` re-anchor once there is a frame.
    if (video.readyState < HTMLMediaElement.HAVE_CURRENT_DATA) return;
    if (overlay.clientWidth === 0 || overlay.clientHeight === 0) {
      // The player panel is hidden (another app tab is active) but `timeupdate`
      // keeps firing. Un-anchor instead of returning: leaving the cursor parked
      // would make every message in the gap arrive >NICO_MAX_LATENESS_MS late and
      // newer than the anchor, i.e. a climbing bogus drop count and a dead
      // overlay while it grinds through them. The next visible tick re-seeds.
      // The clear is required — _resetNicoCursor frees the lanes, which must not
      // happen under elements that are still flying.
      this.clearNicoOverlay();
      this.nico.unanchor();
      return;
    }
    // Lazy anchor, AFTER the hidden-panel guard: with the two the other way
    // round a hidden tick paid a seedCursorIndex + replaceChildren (~4 Hz) to
    // build a cursor the guard then threw away again.
    if (this.nico.cursor < 0) this._reanchorNicoAt(effectiveMs);
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

    this.nico.tick(messages, effectiveMs, {
      prepare: (msg) => this._prepareNico(msg, ctx),
      place: (entry, at, retry) => this._placeEntry(entry, at, ctx, retry),
      discard: () => {}, // the entry's element is already detached (see _placeEntry)
    });

    this._updateNicoDropPill();
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
   * - FIRST sight (`retry` false): allocate at the message's ENTRY time
   *   (`offsetMs − NICO_LEAD_MS`) and let the animation's `delay` hold the exact
   *   entry instant, so a message taken up to NICO_TICK_AHEAD_MS early waits
   *   off-stage instead of jumping in, and a late or seeded one starts as far
   *   into the flight as it is late. The allocator's two-edge rule is a
   *   time-invariant relation, so recording a spawn slightly in the future is
   *   safe; a later message whose entry precedes a lane's latest occupant is
   *   simply refused by `nowMs < freeAt` and deferred.
   * - RETRY (`retry` true): the entry was already rejected once and every lane's
   *   occupancy has only grown newer since, so re-asking at its own entry time
   *   could never succeed and deferral would be a no-op. A retry therefore
   *   allocates at the CURRENT time and gets no head start into the flight; the
   *   allocator's ordinary two-edge bound at placement time is what keeps it
   *   collision-free (R21). It still waits for its entry instant if that instant
   *   is somehow ahead: the lookahead means first sight can happen up to
   *   NICO_TICK_AHEAD_MS BEFORE the entry, so "a retry is past its entry" is no
   *   longer something to read off the code, and `early` is clamped at 0 here
   *   rather than assumed. (It is still true in practice — a lane's free time
   *   only grows, so a retry that succeeds at `effectiveMs` was refused at an
   *   `entryMs` below it — but the guarantee is now by construction, and the
   *   clamp costs one comparison.)
   *
   * The wait is a WAAPI `delay` rather than a negative `currentTime` because
   * `play()` rewinds a negative current time to 0 — and `fill: "both"` keeps the
   * untransformed first keyframe applied for the whole delay (the element is
   * already parked at `left = stageW`, so the fill is belt-and-braces).
   * @param {{msg: object, el: HTMLElement, w: number, h: number}} entry
   * @param {number} effectiveMs
   * @param {{stageW: number, laneHeight: number, rate: number, paused: boolean, overlay: HTMLElement}} ctx
   * @param {boolean} retry
   * @returns {boolean}
   */
  _placeEntry(entry, effectiveMs, { stageW, laneHeight, rate, paused, overlay }, retry) {
    const { msg, el, w, h } = entry;
    const entryMs = msg.offsetMs - NICO_LEAD_MS;
    // Time still to run before it enters; negative = it entered that long ago.
    // A retry never gets a head start (it spawns at the right edge), but it does
    // still wait out an entry instant that has not arrived — see above.
    const early = retry ? Math.max(0, entryMs - effectiveMs) : entryMs - effectiveMs;
    const lanesNeeded = Math.max(1, Math.ceil(h / laneHeight));
    const lane = this._lanes.allocate({
      nowMs: retry ? effectiveMs : entryMs,
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
      { duration: NICO_DURATION_MS, delay: Math.max(0, early), fill: "both" },
    );
    // Never negative, so the `play()` on resume cannot rewind it (see above).
    // No upper clamp is needed: the lateness bound above refuses anything more
    // than NICO_MAX_LATENESS_MS into the flight, and a retry starts at 0.
    anim.currentTime = Math.max(0, -early);
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
    if (this.nico.dropped === this._nicoDroppedShown) return;
    this._nicoDroppedShown = this.nico.dropped;
    pill.textContent = `+${this.nico.dropped} not shown`;
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
    overlay.setAttribute("aria-modal", "true");
    this._resumeReturnFocus = document.activeElement;
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
      if (e.key !== "Escape") return;
      if (document.querySelector("sl-dialog[open]") || isTypingInInput(e)) return;
      e.preventDefault();
      // Start from beginning on Escape (same as clicking "Start from beginning")
      dismiss();
      safePlay(document.getElementById("player-video"));
      this._startWatchTracking(jobId);
    }, { signal: sig });

    // Focus trap (U-M8): Tab and Shift+Tab cycle within the dialog's two
    // actions while it is open; focus is restored on dismiss (already wired).
    //
    // Bound on `document`, like the Escape handler above: focus can legitimately
    // sit OUTSIDE the overlay while the dialog is up — a click on the scrim (the
    // overlay div is not focusable) leaves it on <body>, and so does a Tab
    // pressed before the requestAnimationFrame focus below has run or before
    // Shoelace has upgraded <sl-button>. An overlay-bound handler never sees
    // those keystrokes and focus walks into the page behind the dialog, so the
    // third case pulls it back in.
    const focusables = () => [...overlay.querySelectorAll("sl-button, button, [tabindex]:not([tabindex='-1'])")]
      .filter((el) => !el.disabled);
    document.addEventListener("keydown", (e) => {
      if (e.key !== "Tab") return;
      const items = focusables();
      if (!items.length) return;
      const first = items[0], last = items[items.length - 1];
      if (!overlay.contains(document.activeElement)) { e.preventDefault(); first.focus(); return; }
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
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
    if (this._resumeReturnFocus?.isConnected) this._resumeReturnFocus.focus({ preventScroll: true });
    this._resumeReturnFocus = null;
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
