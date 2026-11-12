/**
 * Moombox Dashboard Application
 */
import { SetupController } from "./modules/setup.js";
import { CookieSetupSession } from "./modules/cookie-setup-session.js";
import { ImportController } from "./modules/imports.js";
import { PlayerController, focusPlayerSurface } from "./modules/player.js";
import { SettingsController } from "./modules/settings.js";
import { TrimController } from "./modules/trimmer.js";
import { StatsController } from "./modules/stats.js";
import { FilesController } from "./modules/files.js";
import { LogPanelController } from "./modules/log-panel.js";
import { UpdateController } from "./modules/update-indicator.js";
import { FilterBarController } from "./modules/filter-bar.js";
import { isClipJob } from "./modules/filter-engine.js";
import { JobDetailsController, stampFactsStatus } from "./modules/job-details.js";
import { formatTimestamp, formatBytes, formatDurationSeconds, formatRelativeTime, isTypingInInput, cookieIndicatorState, cookieIndicatorText, cookieCheckPending, cookieRecheckToast, cookieRefreshPreflightToast, cookieRefreshMechanismLabel, parkedCookiePlatforms, serverErrorMessage, reloginPromptTarget, canResumeJob, NAME_COLLATOR, streamUrl, isImportPlaceholderId, reinitializeDeletesText, sumReinitializePreviews, pendingAutoAction, pendingAutoActionText, isWaitLine, isAwaitingProcessing, queuedHold, queuedHoldText, CANCEL_STATUSES, REINIT_STATUSES, DELETE_STATUSES, WATCHED_STATUSES, isBackfillRetained, toastDuration } from "./modules/utils.js";
import { applyLogoutVisibility, bindLogout } from "./modules/logout.js";

// TASK_STATUS_PRIORITY orders the Tasks list's status groups (_sortJobs).
const TASK_STATUS_PRIORITY = {
  "Error": 0, "COOKIES?": 1, "Downloading": 2, "Muxing": 3,
  "Live": 4, "Upcoming": 5, "Queued": 6, "Cancelled": 7, "Finished": 8,
};

// The batch bar's action buttons, disabled while a per-job batch runs, and
// the word its progress line opens with for each of those actions.
const BATCH_ACTION_BUTTON_IDS = ["batch-cancel", "batch-resume", "batch-reinit", "batch-delete", "batch-watched", "batch-unwatched"];
const BATCH_PROGRESS_VERBS = { cancel: "Cancelling", resume: "Resuming", reinitialize: "Reinitializing", delete: "Deleting" };

/**
 * The dashboard WebSocket's states. _setConnState is the only writer of the
 * status bar's #connection-status. CONNECTING: a socket was created and has
 * not opened. OPEN: onopen ran. BACKOFF: it closed and a reconnect is
 * queued. UNAUTHORIZED: it closed without opening and the session probe
 * answered 401 — the fetch wrapper is reloading to the login page. No socket
 * is opened while it holds, but the probe is re-sent every
 * SESSION_RECHECK_MS, and the first answer that is not a 401 reconnects: the
 * reload can be cancelled (the unsaved-settings leave prompt) and is never
 * re-tried, and the operator can log in again in another tab.
 */
const SESSION_RECHECK_MS = 30000;
// How many upgrades in a row must be refused, each with the server answering
// over HTTP, before a page that has never connected says so in the Tasks
// panel (owner ruling R-B3). One or two is what a restart behind a healthy
// proxy looks like; three is about four seconds of backoff.
const WS_REFUSED_NOTICE_AFTER = 3;
// How long a socket may go without a pong before it is taken for dead, and
// the longer limit that holds until its first frame has arrived. That first
// frame is the initial_state snapshot, megabytes on a busy instance, and the
// server gives its write up to two minutes on a slow link
// (snapshotWriteTimeout, internal/web/websocket.go). A pong cannot overtake
// it, so at 45 s a phone on a weak signal closed the socket that was still
// delivering its job list, and started the download again.
const WS_PONG_TIMEOUT_MS = 45000;
const WS_FIRST_FRAME_TIMEOUT_MS = 150000;
// How many Archived cards one draw adds. The whole archived list is held in
// memory (the filter, Select All and the details lookup read all of it); only
// the cards are bounded, because each one is a dozen Shoelace elements and a
// 24/7 recorder's archive only grows (owner ruling R-B1).
const ARCHIVED_PAGE = 200;

export const ConnState = Object.freeze({
  CONNECTING: "connecting",
  OPEN: "open",
  BACKOFF: "backoff",
  UNAUTHORIZED: "unauthorized",
});

/** How many toasts that stay until closed may be up at once; the oldest makes way. */
const MAX_STAYING_TOASTS = 4;

export class MoomboxApp {
  constructor() {
    this.ws = null;
    this.jobs = [];
    this.archivedJobs = [];
    this.config = null;
    // Per-channel feed-history backfill progress, keyed by channel ID.
    // Seeded from `initial_state` (payload.backfill) and updated by
    // `backfill_status` events; holds only active states ("scanning",
    // "error") — "done"/"idle" delete the entry. Rendered as a badge on the
    // Settings channel cards.
    this.backfillStatus = {};
    // Trims the server is running, keyed by trim id (worker.TrimTask:
    // id, jobId, startTime, endTime, progress). Seeded from `initial_state`
    // (payload.runningTrims) and kept by `trim_status` frames; a trim leaves
    // it when its outcome arrives. Drawn as a progress bar in its job's Trims
    // section.
    this.runningTrims = {};
    this.selectedJobId = null;
    this._selectedTaskJobs = new Set();
    this._selectedArchivedJobs = new Set();
    this._lastCheckedJobIds = { jobs: null, archived: null };
    this.reconnectAttempts = 0;
    /** One of ConnState; written only by _setConnState. */
    this._connState = ConnState.CONNECTING;
    // What the Tasks panel's "live updates cannot connect" notice is derived
    // from: whether any socket of this page has opened, and how many upgrades
    // in a row were refused while HTTP answered. _syncWsNotice is the only
    // writer of the notice itself.
    this._wsEverOpened = false;
    // Whether the current socket has delivered a frame yet (the heartbeat
    // waits longer for the first one, WS_FIRST_FRAME_TIMEOUT_MS).
    this._wsGotFrame = false;
    this._wsRefusedReachable = 0;
    this.autoCookieReloginRequired = null;
    // Tri-state; see loadStatus. null = the server has no opinion.
    this.sidecarHealthy = null;
    // The backlog scheduler's holds (owner ruling R-D3): /api/status, the
    // initial_state snapshot and each admission_holds frame. null until read.
    this.admissionHolds = null;
    // Every monitor's health list by platform (owner ruling R-H1):
    // /api/status, the initial_state snapshot and each channel_monitors
    // frame. The Settings channel list derives its "Checks failing" flag
    // from it (failingChannel in utils.js). null until read.
    this.channelMonitors = null;
    // One-shot timer for the earliest future hold end in admissionHolds: a
    // row's own hold runs out with no frame to say so (setAdmissionHolds).
    this._holdExpiryTimer = null;
    // The server's UTC offset in minutes, from /api/setup/status at boot and
    // /api/status after; null until read.
    this.serverUtcOffsetMinutes = null;
    this.nextFeedCheck = 0;
    this.nextDecapiCheck = 0;
    this.nextTwitchCheck = 0;
    // Threshold (days) for moving Finished jobs from Tasks into Archived.
    // -1 = never archive, 0 = archive immediately, >0 = archive after N days.
    // Provided by the server in `initial_state` and refreshed via `config_update`.
    this.hideFinishedAgeDays = -1;
    this._countdownInterval = null;
    this._archiveSweepInterval = null;
    this.focusedJobIndex = -1;
    // The Archived panel's own keyboard focus, an index into its drawn rows.
    this.focusedArchivedIndex = -1;
    // The Archived fetch. fetchArchivedJobs is the only writer of these five.
    this._archivedGen = 0;            // the newest fetch; an older answer is dropped
    this._archivedAnsweredGen = 0;    // the newest fetch whose answer (or failure) was taken
    this._archivedInFlight = 0;       // fetches not yet answered, of any generation
    this._archivedLoaded = false;     // an answer has been applied
    this._archivedFetchFailed = false; // the newest answered fetch failed
    // Ids that left the archive while a fetch was out. The answer was built
    // before they left, so it is filtered against them. _removeArchivedJob
    // adds to it; fetchArchivedJobs clears it.
    this._archivedGoneInFlight = new Set();
    // Progress frames were merged while the tab was hidden and the list has
    // not been drawn since. Set by the job_progress case's hidden branch,
    // cleared by _repaintAfterHidden.
    this._repaintOnVisible = false;
    // How many Archived cards are drawn at most; Show more raises it.
    this._archivedRenderLimit = ARCHIVED_PAGE;
    // Active tab-panel name, maintained by the sl-tab-show handler. Used to
    // defer Archived-panel DOM work while that panel isn't visible (tracked
    // as state rather than queried from the DOM so the guard can't race
    // Shoelace's own active-attribute bookkeeping during tab transitions).
    this._activePanel = "tasks";
    // True when archivedJobs changed while the Archived panel was hidden —
    // the panel re-renders on activation instead of on every change.
    this._archivedRenderDirty = false;
    // A redraw of the Archived cards is waiting for the next animation frame
    // (_scheduleArchivedRender). Cleared by renderArchivedJobs itself, so a
    // direct redraw in between makes the waiting frame a no-op.
    this._archivedFramePending = false;
    // The per-job batch action that is running, as { verb, done, total }, or
    // null. Written by batchAction only; the batch bar draws it.
    this._batchProgress = null;
    // Which platforms the status bar was last told have a job parked in
    // COOKIES?. null means "never computed", which is distinct from
    // {youtube:false,twitch:false} — see _syncParkedBadge, where the first
    // sync must run even when nothing is parked.
    this._parkedPlatforms = null;
    // Theme: respect explicit user choice in localStorage; otherwise follow
    // the OS light/dark preference and keep tracking it across the session.
    const storedTheme = localStorage.getItem("moombox-theme");
    this._themeFollowsOS = !storedTheme;
    this.theme = storedTheme || (window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark");

    // Module controllers
    // The cookie-setup dialog the wizard and Settings share (K33): one
    // session, bound once, owned by whichever started the setup.
    this.cookieSetup = new CookieSetupSession(this);
    this.setup = new SetupController(this);
    this.imports = new ImportController(this);
    this.player = new PlayerController(this);
    this.settings = new SettingsController(this);
    this.trimmer = new TrimController(this);
    this.stats = new StatsController(this);
    this.files = new FilesController(this);
    this.logPanel = new LogPanelController(this);
    this.updates = new UpdateController(this);
    this.filterBar = new FilterBarController(this);
    this.details = new JobDetailsController(this);

    this.init();
  }

  /** Return the selection set for the currently active panel. */
  _activeSelectionSet() {
    // _activePanel is maintained by the sl-tab-show handler — same source of
    // truth as the archived-render deferral, and cheaper than a DOM query.
    return this._activePanel === "archived" ? this._selectedArchivedJobs : this._selectedTaskJobs;
  }

  /** Return the selection set for a given container key ("jobs" or "archived"). */
  _selectionSetFor(containerKey) {
    return containerKey === "archived" ? this._selectedArchivedJobs : this._selectedTaskJobs;
  }

  async init() {
    // Check if this is a first-run (no config)
    const status = await this.checkSetupStatus();
    // The server's zone, for the template examples (renderTemplatePreview).
    // A first run shows the wizard and never reaches loadStatus, which keeps
    // it for Settings, so the wizard's example would only ever have the
    // browser's date: take it from here, before the wizard draws.
    if (Number.isFinite(status.utcOffsetMinutes)) {
      this.serverUtcOffsetMinutes = status.utcOffsetMinutes;
    }

    if (status.isFirstRun) {
      this.setup.show();
    } else {
      // Check FFmpeg before initializing
      if (status.ffmpegValid === false) {
        this.setup.showFFmpegOverlay();
      } else {
        this.initializeApp();
      }
    }
  }

  initializeApp() {
    if (this._initialized) return;
    this._initialized = true;

    this.setTheme(this.theme, { fromOS: this._themeFollowsOS });
    this.setupEventListeners();
    this.files.bind();
    this.logPanel.bind();
    this.updates.bind();
    this.filterBar.bind();
    this.details.bind();
    this.setupKeyboardShortcuts();
    this.settings.setupListeners();
    this.connectWebSocket();
    this.loadConfig();
    this.loadStatus();
    this.checkSecurityBanner();
    // 1 Hz UI tick. Skipped entirely while the tab is hidden — the
    // visibilitychange handler at the bottom of this file re-fires both
    // updates on return so stale strings never show. Keeping it silent in
    // the background matters for a dashboard left open 24/7. The other
    // always-on DOM work is the job_progress frame's card and dialog update,
    // which a hidden tab skips the same way (_repaintAfterHidden).
    this._countdownInterval = setInterval(() => {
      if (document.hidden) return;
      this.updateCheckCountdown();
      this.refreshRelativeTimestamps();
    }, 1000);
    // Cache the header-countdown breakpoint instead of reading
    // window.innerWidth on every tick (a layout read on the 1 Hz hot path).
    this._narrowHeader = window.innerWidth <= 992;
    window.addEventListener("resize", () => {
      const narrow = window.innerWidth <= 992;
      if (narrow !== this._narrowHeader) {
        this._narrowHeader = narrow;
        this.updateCheckCountdown();
      }
    }, { passive: true });
    // Deep link from a notification embed: {public_url}/#job=<id>. The SPA has
    // no job route — details live in a modal — so the hash is the whole
    // mechanism and needs no server change. Handled on hashchange here and
    // once per initial_state below (the jobs list is what it searches, and on
    // a cold load that list arrives with the socket, not with the document).
    window.addEventListener("hashchange", () => this._consumeJobHash());
    // Idle-tick sweep: re-evaluate archive boundary in case no job_update
    // arrives but enough time has passed for a Finished job to cross the
    // hide_finished_age_days threshold.
    this._archiveSweepInterval = setInterval(() => this._evaluateArchiveBoundary(), 60_000);

    // Track OS theme changes mid-session — only applied while the user
    // has not yet made an explicit choice via the theme toggle.
    const darkMQ = window.matchMedia("(prefers-color-scheme: dark)");
    const onOSThemeChange = (e) => {
      if (this._themeFollowsOS) {
        this.setTheme(e.matches ? "dark" : "light", { fromOS: true });
      }
    };
    if (typeof darkMQ.addEventListener === "function") {
      darkMQ.addEventListener("change", onOSThemeChange);
    } else if (typeof darkMQ.addListener === "function") {
      darkMQ.addListener(onOSThemeChange); // Safari < 14
    }
  }

  async checkSetupStatus() {
    try {
      const response = await fetch("/api/setup/status");
      if (response.ok) {
        return await response.json();
      }
    } catch (e) {
      console.error("Failed to check setup status:", e);
    }
    return { isFirstRun: false };
  }

  setupEventListeners() {
    // Add video button
    const addVideoBtnClick = () => {
      document.getElementById("add-dialog").show();
      document.getElementById("video-url-input").value = "";
      this.resetAdvancedOptions();
      setTimeout(() => document.getElementById("video-url-input").focus(), 100);
    };
    document.getElementById("add-video-btn").addEventListener("click", addVideoBtnClick);

    // Empty state CTA delegates to the same add-video handler
    document.getElementById("empty-state-add-btn")?.addEventListener("click", addVideoBtnClick);

    // Add video submit
    document
      .getElementById("add-submit-btn")
      .addEventListener("click", () => this.addVideo());

    // Enter key in input
    document
      .getElementById("video-url-input")
      .addEventListener("keydown", (e) => {
        if (e.key === "Enter") this.addVideo();
      });

    // Advanced options toggle
    document
      .getElementById("advanced-options-toggle")
      .addEventListener("sl-change", (e) => {
        const panel = document.getElementById("advanced-options-panel");
        const checked = e.target.checked;
        panel.style.display = checked ? "block" : "none";
        if (checked) {
          this.fetchFormatsForAdvanced();
        }
      });

    // Debounced format fetch when URL changes while advanced is checked.
    // Store the timer on `this` so resetAdvancedOptions() and the add-dialog
    // close hook can cancel a pending fetch — otherwise a stale timer can
    // fire after submit/close and re-populate a closed dialog.
    document
      .getElementById("video-url-input")
      .addEventListener("sl-input", (e) => {
        // The listed formats belong to one video. Once the URL names another
        // they go at once, not after the debounce: the picker must not show,
        // and the submit must not send, an earlier video's rendition.
        // The same goes for a list still being fetched: disowned here
        // (_lastFormatVideoId), its answer is dropped when it arrives.
        const listedOrAsked = this._formatsListedFor ?? this._lastFormatVideoId;
        if (listedOrAsked && this.extractVideoId(e.target.value || "") !== listedOrAsked) {
          this.clearFormatChoice();
          this._lastFormatVideoId = null;
          document.getElementById("format-skeleton").style.display = "none";
        }
        if (document.getElementById("advanced-options-toggle").checked) {
          clearTimeout(this._formatFetchTimeout);
          this._formatFetchTimeout = setTimeout(() => {
            this._formatFetchTimeout = null;
            this.fetchFormatsForAdvanced();
          }, 500);
        }
      });

    // Cancel any pending format fetch when the add dialog closes so a
    // stale debounce timer can't fire against a hidden dialog.
    document.getElementById("add-dialog")?.addEventListener("sl-after-hide", () => {
      if (this._formatFetchTimeout) {
        clearTimeout(this._formatFetchTimeout);
        this._formatFetchTimeout = null;
      }
    });

    // Trim dialog cleanup on close
    document.getElementById("trim-dialog").addEventListener("sl-after-hide", () => {
      this.trimmer.destroy();
    });

    // Tab activation handlers
    const tabGroup = document.querySelector("sl-tab-group");
    if (tabGroup) {
      tabGroup.addEventListener("sl-tab-show", (e) => {
        this._activePanel = e.detail.name;
        if (e.detail.name === "archived") {
          // Render any changes deferred while the panel was hidden for
          // instant feedback, then refresh from the server. Each visit
          // starts from the newest page again, so the cards drawn by Show
          // more do not pile up across visits. A limit that shrank is drawn
          // now: the fetch below can take seconds, and until it answers
          // Arrow and Enter would walk 200 rows of a list showing more.
          const shrank = this._archivedRenderLimit !== ARCHIVED_PAGE;
          this._archivedRenderLimit = ARCHIVED_PAGE;
          if (this._archivedRenderDirty || shrank) this.renderArchivedJobs();
          this.fetchArchivedJobs();
        } else if (e.detail.name === "player") {
          if (!this.player.playerInitialized) {
            this.player.initPlayer();
          }
          // Skip if openInPlayer is handling the load (prevents race condition)
          if (!this._playerOpeningFromDetails) {
            this.player.loadPlayerJobList();
          }
          this.player.attachKeyboardControls?.();
          // Move focus off the sl-tab: sl-tab-group's own keydown handling
          // treats a focused tab's arrow keys as tab navigation, which would
          // otherwise steal ArrowLeft/Right from the player's seek shortcuts
          // on the very keypress after clicking the Player tab. rAF: the tab
          // itself only finishes taking focus after this synchronous handler.
          requestAnimationFrame(() => document.getElementById("player-video-wrapper")?.focus({ preventScroll: true }));
        } else if (e.detail.name === "imports") {
          if (!this.imports.importInitialized) {
            this.imports.initImports();
          }
        } else if (e.detail.name === "files") {
          this.files.fetchOrphanedFiles();
          this.files.fetchOrphanedHistory();
        } else if (e.detail.name === "stats") {
          this.stats.activate();
        } else if (e.detail.name === "logs") {
          this.logPanel.onShow();
        }
        // Detach the player's document-level keydown listener when leaving
        // the player tab — it used to run for every keystroke app-wide even
        // if the panel early-returned inside the handler.
        if (e.detail.name !== "player") {
          this.player.detachKeyboardControls?.();
        }
        // Deactivate stats auto-refresh when leaving the tab
        if (e.detail.name !== "stats") {
          this.stats.deactivate();
        }
        // Refresh batch action bar for the newly active panel
        this.updateBatchActionBar();
        // The 1 Hz refresh is scoped to the active panel — bring the newly
        // shown panel's relative timestamps current immediately rather than
        // waiting for the next tick.
        this.refreshRelativeTimestamps();
      });
    }

    // Intercept tab switches when settings has unsaved changes. The guard
    // itself is _canLeaveSettings / _confirmLeaveSettings, so that the two
    // programmatic switches — the 1–8 shortcuts (showTab) and Play from the
    // details dialog (openInPlayer) — owe the same answer a click does; this
    // listener is only the click's way in.
    document.querySelectorAll("sl-tab[slot='nav']").forEach(tab => {
      tab.addEventListener("click", (e) => {
        if (this._canLeaveSettings(tab.panel)) return;
        e.preventDefault();
        e.stopImmediatePropagation();
        this._confirmLeaveSettings().then(confirmed => {
          if (confirmed) tab.click();
        });
      }, true); // capture phase — intercept before Shoelace switches the tab
    });

    // Refresh cookies button. Plain click runs the in-process Go refresh;
    // shift+click runs the auto-cookie pass — headless browser when
    // cookies.auto_enabled permits it, an immediate import from the browser
    // profile when it does not.
    //
    // DELIBERATELY NOT GATED ON auto_enabled, and do not put the gate back.
    // It used to read `e.shiftKey && this.config?.cookies?.auto_enabled`, so on
    // a disabled install shift+click silently fell through to the plain
    // recheck: the gesture existed, did something else, and said nothing about
    // it. This is the Web twin of the TUI's R F chord (cmd/moombox/
    // tui_wiring.go), which was hidden by the same flag one surface over, and
    // it is the gesture an operator who has hand-updated their browser profile
    // uses to have it read. The server decides which mechanism runs — see
    // AutoCookieService.BrowserLaunchAllowed — and reports which it was.
    const refreshBtn = document.getElementById("btn-refresh-cookies");
    if (refreshBtn) {
      refreshBtn.addEventListener("click", (e) => {
        if (e.shiftKey) {
          this.autoCookieRefresh();
        } else {
          this.recheckCookies();
        }
      });
    }

    // Status-bar logout — shown by checkSecurityBanner() when auth is on and
    // this session is authenticated (modules/logout.js owns both rules).
    bindLogout(document.getElementById("btn-logout"), {
      fetchFn: (url, opts) => fetch(url, opts),
      reload: () => window.location.reload(),
    });

    // Status warnings — delegated click and keydown (text on desktop).
    //
    // One `activate` for both input paths, not two copies: a keyboard user
    // whose Enter does something other than the click is worse off than one
    // whose Enter does nothing, and a copy is how that drift starts.
    const warningsEl = document.getElementById("status-warnings");
    if (warningsEl) {
      const activate = (target) => {
        const warning = target?.closest?.(".status-warning");
        // Not every warning is a button. The sidecar alert carries no action
        // (updateStatusBar leaves its data-action unset), so it is neither
        // focusable nor activatable — and saying so here is what lets the
        // keydown leave Space to the browser instead of swallowing the page
        // scroll for a span that does nothing.
        if (!warning?.dataset.action) return false;
        this.answerReloginPrompt(warning.dataset.action);
        return true;
      };
      warningsEl.addEventListener("click", (e) => { activate(e.target); });
      // Delegated, like the click: the spans are rebuilt on every status
      // update, so per-span listeners would have to be re-bound each time.
      warningsEl.addEventListener("keydown", (e) => {
        if (e.key !== "Enter" && e.key !== " ") return;
        if (activate(e.target)) e.preventDefault();
      });
    }

    // Status warnings — collapsed icon (mobile): click or Enter/Space
    const warningsIconEl = document.getElementById("status-warnings-icon");
    if (warningsIconEl) {
      // Mirrors `activate` above, for the same reason: the icon stands in for
      // whichever warning is first, and that warning may be the action-less
      // sidecar alert. When it is, the icon is not a button (updateStatusBar
      // strips its role/tabindex) and this must not claim the key either.
      const trigger = () => {
        if (!warningsIconEl.dataset.action) return false;
        this.answerReloginPrompt(warningsIconEl.dataset.action);
        return true;
      };
      warningsIconEl.addEventListener("click", () => { trigger(); });
      warningsIconEl.addEventListener("keydown", (e) => {
        if (e.key !== "Enter" && e.key !== " ") return;
        if (trigger()) e.preventDefault();
      });
    }

    // Theme toggle
    const themeToggle = document.getElementById("theme-toggle");
    if (themeToggle) {
      themeToggle.addEventListener("click", () => {
        this.setTheme(this.theme === "dark" ? "light" : "dark");
      });
    }

    // Force-check monitors: the header countdown is the click target.
    const checkCountdown = document.getElementById("check-countdown");
    if (checkCountdown) {
      const trigger = () => this.checkMonitorsNow();
      checkCountdown.addEventListener("click", trigger);
      checkCountdown.addEventListener("keydown", (e) => {
        if (e.key === "Enter" || e.key === " ") { e.preventDefault(); trigger(); }
      });
    }

    // Thumbnail error handling via event delegation (error events don't bubble, use capture).
    // The `thumb-avatar` class only makes sense when the fallback URL is a
    // square channel avatar (Twitch's pre-stream state). For YouTube, and
    // for the new test.45+ flow where the primary src is the local file
    // and the fallback is the YouTube/Twitch CDN thumbnail (16:9), adding
    // thumb-avatar gives a square 1:1 box -- the "letterboxed in a square
    // frame" symptom owner reported. Only apply thumb-avatar when the
    // fallback explicitly opts in via data-fallback-avatar.
    const handleThumbError = (e) => {
      const img = e.target;
      if (img.tagName !== "IMG" || !img.closest(".thumb")) return;
      // Remember the exact src that just failed so updateJobCard doesn't
      // re-assign it on the next progress tick (endless 404/swap flicker
      // when a job's local thumbnailFile is missing).
      img.dataset.localFailed = img.getAttribute("src") || "";
      const fallback = img.dataset.fallback;
      if (fallback) {
        // Remove fallback so we don't loop if it also fails
        delete img.dataset.fallback;
        img.src = fallback;
        if (img.dataset.fallbackAvatar === "true") {
          img.classList.add("thumb-avatar");
        }
      } else {
        img.style.display = "none";
      }
    };
    for (const id of ["jobs-container", "archived-container"]) {
      const el = document.getElementById(id);
      if (el) el.addEventListener("error", handleThumbError, true);
    }

    // Event delegation for job items (prevents memory leaks from per-item listeners)
    // Shared click handler for job containers (active + archived)
    const setupJobContainer = (container, containerKey, jobSource) => {
      if (!container) return;
      // A row that takes DOM focus (Tab, a click, Arrow) is the highlighted
      // row: the highlight and the focus are one thing, so Enter and Space
      // act on the row a screen reader just announced.
      container.addEventListener("focusin", (e) => {
        const card = e.target;
        if (!(card instanceof Element) || !card.classList.contains("video-item")) return;
        if (card.hasAttribute("data-focused")) return;
        this._highlightRow(container, card);
      });
      container.addEventListener("click", (e) => {
        // Batch selection checkbox
        const checkbox = e.target.closest(".job-checkbox");
        if (checkbox) {
          e.stopPropagation();
          const jobId = checkbox.dataset.jobId;
          const selectionSet = this._selectionSetFor(containerKey);
          // Shift+Click: select range between last checked and current (within same container)
          const lastId = this._lastCheckedJobIds[containerKey];
          if (e.shiftKey && lastId && checkbox.checked) {
            const allCards = [...container.querySelectorAll(".video-item")];
            const lastIdx = allCards.findIndex(c => c.dataset.jobId === lastId);
            const curIdx = allCards.findIndex(c => c.dataset.jobId === jobId);
            if (lastIdx !== -1 && curIdx !== -1) {
              const [start, end] = lastIdx < curIdx ? [lastIdx, curIdx] : [curIdx, lastIdx];
              for (let i = start; i <= end; i++) {
                const id = allCards[i].dataset.jobId;
                selectionSet.add(id);
                allCards[i].classList.add("selected");
                const cb = allCards[i].querySelector(".job-checkbox");
                if (cb) cb.checked = true;
              }
            }
          } else if (checkbox.checked) {
            selectionSet.add(jobId);
          } else {
            selectionSet.delete(jobId);
          }
          this._lastCheckedJobIds[containerKey] = jobId;
          checkbox.closest(".video-item")?.classList.toggle("selected", checkbox.checked);
          this.updateBatchActionBar();
          return;
        }
        // Expandable error text
        const errorText = e.target.closest(".job-error-text");
        if (errorText) {
          e.stopPropagation();
          const isExpanded = errorText.classList.toggle("expanded");
          errorText.textContent = isExpanded ? errorText.dataset.full : errorText.dataset.short;
          return;
        }
        // Quick action buttons
        const quickBtn = e.target.closest("[data-quick-action]");
        if (quickBtn) {
          e.stopPropagation();
          const action = quickBtn.dataset.quickAction;
          const jobId = quickBtn.dataset.jobId;
          this.quickAction(action, jobId);
          return;
        }
        // Don't open the details dialog when the tap landed on the overflow
        // trigger or its menu — the dropdown handles its own interaction.
        if (e.target.closest(".video-item-overflow")) return;
        const videoItem = e.target.closest(".video-item");
        if (videoItem) {
          const jobId = videoItem.dataset.jobId;
          const job = jobSource().find((j) => j.id === jobId);
          if (job) this.details.showJobDetails(job);
        }
      });
      // sl-menu-item selections from the mobile overflow dropdown dispatch
      // the same quickAction as the inline icon buttons. The menu items
      // carry `value="cancel|reinitialize|delete"` (rather than the
      // `data-quick-action` attribute the click handler matches) so a
      // menu-item tap doesn't trigger BOTH this listener and the click
      // delegate above.
      container.addEventListener("sl-select", (e) => {
        const item = e.detail?.item;
        const action = item?.value;
        const jobId = item?.dataset?.jobId;
        if (action && jobId) this.quickAction(action, jobId);
      });
    };

    setupJobContainer(document.getElementById("jobs-container"), "jobs", () => this.jobs);
    setupJobContainer(document.getElementById("archived-container"), "archived", () => this.archivedJobs);

    // Batch action bar buttons
    document.getElementById("batch-cancel")?.addEventListener("click", () => this.batchAction("cancel"));
    document.getElementById("batch-resume")?.addEventListener("click", () => this.batchAction("resume"));
    document.getElementById("batch-reinit")?.addEventListener("click", () => this.batchAction("reinitialize"));
    document.getElementById("batch-delete")?.addEventListener("click", () => this.batchAction("delete"));
    document.getElementById("batch-watched")?.addEventListener("click", () => this.batchAction("watched"));
    document.getElementById("batch-unwatched")?.addEventListener("click", () => this.batchAction("unwatched"));
    document.getElementById("batch-select-all")?.addEventListener("click", () => {
      const panel = this._activePanel;
      const selectionSet = this._activeSelectionSet();
      let visibleJobs;
      if (panel === "archived") {
        visibleJobs = this.filterBar.getFilteredArchivedJobs();
      } else {
        visibleJobs = this.filterBar.getFilteredJobs();
      }
      visibleJobs.forEach(j => selectionSet.add(j.id));
      // Only update checkboxes in the active panel's container
      const containerId = panel === "archived" ? "archived-container" : "jobs-container";
      const container = document.getElementById(containerId);
      if (container) {
        container.querySelectorAll(".job-checkbox").forEach(cb => { cb.checked = true; });
        container.querySelectorAll(".video-item").forEach(el => el.classList.add("selected"));
      }
      this.updateBatchActionBar();
    });
    document.getElementById("batch-clear")?.addEventListener("click", () => this._clearSelectionUI());
    document.getElementById("archived-retry")?.addEventListener("click", () => this.fetchArchivedJobs());
    document.getElementById("archived-show-more")?.addEventListener("click", (e) => {
      const before = this._archivedRenderLimit;
      this._archivedRenderLimit += ARCHIVED_PAGE;
      this.renderArchivedJobs();
      // The last page hides the button: keyboard focus would fall to the
      // page, so it goes to the first row the click drew.
      if (e.currentTarget.closest("#archived-more")?.style.display === "none") {
        document.getElementById("archived-container")?.querySelectorAll(".video-item")[before]?.focus();
      }
    });
  }

  async loadConfig() {
    try {
      const response = await fetch("/api/config");
      if (response.ok) {
        this.config = await response.json();
        if (this.settings.isDirty()) {
          // User has unsaved form changes — only update lists and status bar
          // without overwriting form fields (channel/notification operations
          // trigger loadConfig but shouldn't discard unsaved edits)
          this.settings.renderChannelsList();
          this.settings.renderNotificationsList();
        } else {
          this.settings.populateConfigForm();
          this.settings.renderChannelsList();
          this.settings.renderNotificationsList();
        }
        this.updateStatusBar();
      }
    } catch (e) {
      console.error("Failed to load config:", e);
    }
  }

  /**
   * GET /api/status, given up after 15 s: fetch has no timeout of its own,
   * and a read that never settles (a stalled proxy, a laptop back from sleep)
   * would hold onStatusChanged's busy latch for the life of the tab.
   * Resolves the parsed body, or null for a non-ok answer; rejects when the
   * server cannot be reached or the deadline passes.
   */
  async _fetchStatus() {
    const abort = new AbortController();
    const deadline = setTimeout(() => abort.abort(), 15000);
    try {
      const response = await fetch("/api/status", { signal: abort.signal });
      return response.ok ? await response.json() : null;
    } finally {
      clearTimeout(deadline);
    }
  }

  /**
   * The cookie and re-login fields have several writers: loadStatus, the
   * status_changed read (_loadAuthStatus), the first-check poll
   * (_pollCookieCheck) and the answers of Recheck and Refresh. Each takes the
   * next number here; a read applies its answer only while its number is
   * still the newest, so an older read never draws over a newer answer.
   */
  _nextAuthGen() {
    return (this._authGen = (this._authGen || 0) + 1);
  }

  /**
   * The sidecar's health and the active platforms come only from a whole
   * status answer, so only the two status reads (loadStatus and
   * _loadAuthStatus) take this number. The cookie writers above do not: a
   * poll or a Recheck that answers during a status read says nothing about
   * the sidecar, and must not make that read's sidecar half be dropped (no
   * second status_changed frame would follow to correct it).
   */
  _nextHealthGen() {
    return (this._healthGen = (this._healthGen || 0) + 1);
  }

  /**
   * The auth half of a status answer: what a status_changed frame is about.
   * `gens` are the numbers the read took when it was sent; each group of
   * fields is applied only while its number is still the newest.
   */
  _applyAuthStatus(status, gens) {
    if (gens.auth === this._authGen) {
      this.autoCookieReloginRequired = status.autoCookieReloginRequired || null;
      this.cookieStatus = status.cookieStatus;
      this.twitchAuthStatus = status.twitchAuthStatus;
    }
    if (gens.health !== this._healthGen) return;
    this.activePlatforms = status.activePlatforms || {};
    // Tri-state on purpose. `null` means the server published nothing —
    // `[bgutils] use_sidecar = false` — and must read as "no opinion", not
    // as a fault; only an explicit `healthy: false` raises the warning.
    this.sidecarHealthy = status.botguardSidecar ? status.botguardSidecar.healthy === true : null;
  }

  async loadStatus() {
    // Each read takes the next number and its answer is applied only while
    // that number is still the newest: of two reads that overlap, the one
    // asked for first can answer last, with the older status.
    const gen = (this._statusGen = (this._statusGen || 0) + 1);
    const gens = { auth: this._nextAuthGen(), health: this._nextHealthGen() };
    try {
      const status = await this._fetchStatus();
      if (status) {
        if (gen !== this._statusGen) return;
        // Each group of fields is skipped when a newer writer of it answered meanwhile.
        this._applyAuthStatus(status, gens);
        // The server's zone, which recordings are named in: the Settings
        // template example renders its date there (renderTemplatePreview).
        // The config may have drawn it already, with the browser's.
        if (Number.isFinite(status.utcOffsetMinutes) && status.utcOffsetMinutes !== this.serverUtcOffsetMinutes) {
          this.serverUtcOffsetMinutes = status.utcOffsetMinutes;
          this.settings?._wireTemplatePreview?.();
        }
        if (status.version) {
          if (this._version && status.version !== this._version) {
            // The server restarted on a DIFFERENT version — an update
            // landed. The SPA in this tab is still the old build talking to
            // a new backend; reload to pick up matching assets. Respect
            // unsaved settings edits: defer (retry on the next status load)
            // and tell the user instead of discarding their changes.
            if (this.settings?.isDirty()) {
              // Said once per version: this branch runs again on every
              // status load until the settings are saved.
              if (this._updateToastShownFor !== status.version) {
                this._updateToastShownFor = status.version;
                this.showToast(`Moombox was updated to v${status.version} — reload the page after saving your settings`, "primary");
              }
            } else {
              window.location.reload();
              return;
            }
          } else {
            this._version = status.version;
          }
        }
        if (status.updateAvailable) {
          this.updates.available = status.updateAvailable;
        } else {
          // Server no longer reports a pending update (it was skipped,
          // applied or withdrawn while this page was not listening) — clear
          // the stale badge, and close an update dialog still offering it.
          this.updates.nonePending();
        }
        if (status.uptime != null) {
          this._uptimeSeconds = status.uptime;
          this._uptimeCapturedAt = Date.now();
        }
        if (status.admissionHolds) this.setAdmissionHolds(status.admissionHolds);
        if (status.channelMonitors) this.setChannelMonitors(status.channelMonitors);
        if (status.disk) this.stats.updateDiskIndicator(status.disk);
        this.updates.updateVersionIndicator();
        this.updateStatusBar();
      }
    } catch (e) {
      console.error("Failed to load status:", e);
    }
  }

  /**
   * The server's status_changed frame: cookie authentication, a re-login flag
   * or the sidecar's health changed. The frame carries nothing; the status is
   * read again, so the badges are only ever drawn from an answer this tab
   * asked for. One read is in flight at a time, and frames that arrive during
   * it ask for one more after it, however many they were. Only the fields
   * the frame is about are taken from the answer (_loadAuthStatus): the rest
   * of the status has its own frames, which this read could be older than.
   */
  async onStatusChanged() {
    if (this._statusNudgeBusy) {
      this._statusNudgeAgain = true;
      return;
    }
    this._statusNudgeBusy = true;
    try {
      do {
        this._statusNudgeAgain = false;
        await this._loadAuthStatus();
      } while (this._statusNudgeAgain);
    } finally {
      this._statusNudgeBusy = false;
    }
  }

  /** Read the status and apply its auth half only; a group of fields a newer writer answered first is left. */
  async _loadAuthStatus() {
    const gens = { auth: this._nextAuthGen(), health: this._nextHealthGen() };
    try {
      const status = await this._fetchStatus();
      if (!status) return;
      this._applyAuthStatus(status, gens);
      this.updateStatusBar();
    } catch (e) {
      // Unreachable or too slow: the reconnect's status load, or the next
      // frame, reads again.
    }
  }

  async recheckCookies() {
    const btn = document.getElementById("btn-refresh-cookies");
    if (btn) btn.classList.add("checking");

    try {
      const response = await fetch("/api/cookies/recheck", { method: "POST" });
      if (response.ok) {
        const data = await response.json();
        this._nextAuthGen(); // a status read still in flight is older than this answer
        this.autoCookieReloginRequired = data.autoCookieReloginRequired || null;
        this.cookieStatus = data.cookieStatus;
        this.twitchAuthStatus = data.twitchAuthStatus;
        if (data.activePlatforms) this.activePlatforms = data.activePlatforms;
        this.updateStatusBar();
        // The server's first check is still running: this recheck was turned
        // away by it and brought back an unchecked status, which has no
        // verdict to word. The badges read "checking" and the poll
        // (_pollCookieCheck) delivers the answer.
        if (cookieCheckPending(data.cookieStatus, data.twitchAuthStatus)) {
          this.showToast("Cookies: still checking…", "primary");
          return;
        }
        // Worded from the verdicts, in cookieRecheckToast, where it can be
        // executed. The two-arm toast this replaces was keyed on
        // `data.success` — false for a check that never reached the site —
        // so the one gesture that asks "what are my credentials doing" was
        // the last surface in this arc that could not say.
        const recheck = cookieRecheckToast(
          this.activePlatforms,
          data.cookieStatus,
          data.twitchAuthStatus,
          data.success,
        );
        this.showToast(recheck.message, recheck.variant);
      } else {
        this.showToast("Failed to recheck cookies: " + await serverErrorMessage(response), "danger");
      }
    } catch (e) {
      this.showToast("Failed to recheck cookies: " + e.message, "danger");
    } finally {
      if (btn) btn.classList.remove("checking");
    }
  }

  async autoCookieRefresh() {
    const btn = document.getElementById("btn-refresh-cookies");
    if (btn) btn.classList.add("checking");
    this.showToast(cookieRefreshPreflightToast(this.config?.cookies?.acquisition), "primary");

    try {
      const response = await fetch("/api/cookies/auto-refresh", { method: "POST" });
      const data = await response.json().catch(() => ({ error: response.statusText }));
      // The SUBJECT of every result toast below, from what the pass actually
      // did rather than from what the mode asked for. `data.mechanism` is
      // undefined against an older binary and empty when the pass declined
      // before choosing; both fall back to the configured mode, which is the
      // same answer the pre-flight toast above already gave. See
      // cookieRefreshMechanismLabel in ./modules/utils.js.
      // Computed once rather than per arm below: the 404/424 rung-3 branch
      // does not use it (it names its own affordance instead), and one call
      // here is cheaper than one at each of the arms that can drift.
      const mechanismLabel = cookieRefreshMechanismLabel(
        data.mechanism,
        this.config?.cookies?.acquisition,
      );
      if (response.ok && !data.error) {
        this._nextAuthGen(); // a status read still in flight is older than this answer
        if (data.cookieStatus) this.cookieStatus = data.cookieStatus;
        if (data.twitchAuthStatus) this.twitchAuthStatus = data.twitchAuthStatus;
        this.autoCookieReloginRequired = data.autoCookieReloginRequired || null;
        if (data.activePlatforms) this.activePlatforms = data.activePlatforms;
        this.updateStatusBar();
        // Five branches where there were three. Three independent facts come
        // back and each can disagree with the others: `ran` (did this pass do
        // any work), `verdict` (what it concluded, if anything) and `renewed`
        // (did THIS pass produce the credentials it verified).
        //
        // A browser that never ran still leaves a working cookies.txt behind
        // (the background session refresh keeps it alive), so success alone
        // would toast "successful" while the Last refresh line below refuses
        // to advance — and on Linux, where a launch can never be confirmed to
        // have acted, that is every time. That is the `renewed` arm.
        //
        // The `ran` arm is the mirror image, and the one that was live: the
        // single refresh slot is held by the 30-minute periodic tick and by
        // interactive setup, so clicking this button during either returns a
        // pass that looked at nothing — and it toasted "auth verification
        // failed" in the same payload whose cookieStatus above reported the
        // session authenticated.
        //
        // Both un-concluded arms stop short of asserting failure. We cannot
        // tell a browser that did nothing from one that finished after we
        // looked, and a verification that never happened says nothing at all
        // about the credentials; asserting failure would swap one wrong claim
        // for another. Only verdict === "failed" earns the word.
        let refreshMsg, refreshVariant;
        if (!data.success && data.ran === false) {
          // The cause list is one exhaustive set rendered in three places (see
          // cookies.RefreshDeclinedCauses). This copy cannot import it, so
          // TestAppJSMatchesTheDeclinedCauses pins the two against each other —
          // keep the phrase below byte-identical to the constant.
          refreshMsg = `${mechanismLabel} declined to run — a setup or another refresh is already in flight, or no platform has cookies worth refreshing. Nothing was learned about these cookies.`;
          refreshVariant = "neutral";
        } else if (!data.success && data.verdict === "failed") {
          refreshMsg = `${mechanismLabel} completed — auth verification failed`;
          refreshVariant = "danger";
        } else if (!data.success) {
          refreshMsg = `${mechanismLabel} ran but could not establish whether these cookies work — nothing has been concluded about them`;
          refreshVariant = "warning";
        } else if (data.renewed === false) {
          // NOT re-subjected — the Go twin's comment explains why: an import
          // always renews, so this arm is unreachable for one.
          refreshMsg =
            "Cookies still work — but this pass could not confirm the browser refreshed them, so the last-refresh time is unchanged";
          refreshVariant = "warning";
        } else {
          refreshMsg = `${mechanismLabel} successful`;
          refreshVariant = "success";
        }
        this.showToast(refreshMsg, refreshVariant);
      } else if (response.status === 404 || response.status === 424) {
        // THE BOTTOM RUNG, and the Web twin of the TUI's R F fallback.
        //
        // Shift+click asks for the strongest refresh available: the headless
        // browser when cookies.auto_enabled permits one and a browser is there,
        // otherwise an immediate import from the browser profile. These two
        // statuses are the server saying there is no profile to work from at
        // all — 404 for ErrProfileNotFound, 424 for ErrNoBrowserFound, both
        // raised from the same missing-directory check in
        // RefreshCookiesDetailed. The operator still has the in-process Go
        // refresh, which is what a plain click runs, so fall through to it
        // rather than dead-ending on "failed".
        //
        // Branched on the STATUS, not the message: those sentences are the
        // route's prose and matching them would break the moment either is
        // reworded. No other failure on this endpoint uses either code.
        //
        // Same event as the TUI's, and deliberately NOT the same sentence.
        // Each surface names ITS OWN affordance for the in-process refresh:
        // the TUI says "running R C instead..." because the TUI has chords,
        // and this says "a normal cookie refresh" because a plain click on
        // this button IS the dashboard's R C. Parallel structure, and every
        // reader is pointed at something they can actually do. Do not
        // "unify" these into one string, and do not describe the mechanism
        // instead of the affordance. Both are pinned exactly, and their
        // divergence asserted, by TestRungThreeSentencesDivergeByDesign.
        this.showToast(
          "No browser profile found, running a normal cookie refresh instead...",
          "primary",
        );
        if (btn) btn.classList.remove("checking");
        await this.recheckCookies();
        return;
      } else if (data.cause === "profile-in-use") {
        // A SKIP, not a failure: a browser the profile's SingletonLock names
        // may still be running on it, so the pass declined and launched
        // nothing. The sentence as it stands — it names the host to close the
        // browser on and the lock to delete — in the colour the Last cookie
        // error line draws it in, as the TUI's R F does. Keyed on the cause,
        // not the 409: a locked cookie DB answers the same status and is
        // still a failure.
        this.showToast(data.error, "warning");
      } else {
        this.showToast(data.error || `${mechanismLabel} failed`, "danger");
      }
    } catch (e) {
      this.showToast(
        `${cookieRefreshMechanismLabel("", this.config?.cookies?.acquisition)} failed: ${e.message}`,
        "danger",
      );
    } finally {
      if (btn) btn.classList.remove("checking");
    }
  }

  /**
   * Re-render the status bar when the set of platforms holding a parked job
   * changes. Returns whether it did.
   *
   * THE MISSING EDGE. updateStatusBar had four call sites — the config load,
   * the status load, the manual recheck and the manual browser refresh — and
   * not one of them is a job event, so a job the worker parked for a
   * membership or cookie reason showed its remedy in the list while the
   * aggregate badge above it stayed whatever it last was. The TUI has
   * reflected parked jobs in its equivalent since the per-platform split; the
   * Web was knowingly divergent, and this is the edge that closes it.
   *
   * GATED ON A CHANGE, not called per event. Job events arrive at roughly
   * 60 Hz per active download and updateStatusBar writes DOM; the standing
   * rule is to make updates cheaper rather than rarer, so the scan runs every
   * time (it is a status compare over jobs already in memory, and it stops at
   * the second platform) and only the DOM write is conditioned. `null` on the
   * first call is deliberately not equal to "nothing parked": the badge has to
   * be reconciled once against the initial job list, whatever it holds.
   */
  _syncParkedBadge() {
    const parked = parkedCookiePlatforms(this.jobs);
    const prev = this._parkedPlatforms;
    if (prev && prev.youtube === parked.youtube && prev.twitch === parked.twitch) {
      return false;
    }
    this._parkedPlatforms = parked;
    this.updateStatusBar();
    return true;
  }

  updateStatusBar() {
    const ytActive = this.activePlatforms?.youtube === true;
    const twActive = this.activePlatforms?.twitch === true;

    // 1. Warnings — text on desktop, collapsed icon on mobile
    //
    // DELIBERATELY NOT GATED ON cookies.auto_enabled, and the gate that used to
    // be here is the bug. The flag means "the session Moombox holds has been
    // rejected; a human has to sign in again" — which is true, and actionable,
    // whether or not a browser will do the signing in. A manual-cookie install
    // has to do theirs by hand, which makes them the audience LEAST able to
    // discover this any other way and the one the gate hid it from. The flag is
    // reachable for them too: POST /api/cookies/auto-refresh is not gated on
    // AutoEnabled, so an operator-triggered refresh can raise it.
    //
    // The same ruling was already made for the auth-loss NOTIFICATION, for the
    // same reason, and the TUI (cmd/moombox/tui_wiring.go) has never gated it.
    // Re-adding a gate on either surface puts the two UIs back out of step.
    const warningsEl = document.getElementById("status-warnings");
    const warningsIcon = document.getElementById("status-warnings-icon");
    const warningItems = [];
    // Held back while the first cookie check of the server process has not
    // answered (cookieCheckPending): the badges read "checking" then, and a
    // re-login prompt beside them would be a conclusion nothing has drawn yet.
    const checkPending = cookieCheckPending(this.cookieStatus, this.twitchAuthStatus);
    if (!checkPending && ytActive && this.autoCookieReloginRequired?.youtube)
      warningItems.push({ action: "yt-relogin", label: "YT: Re-login" });
    if (!checkPending && twActive && this.autoCookieReloginRequired?.twitch)
      warningItems.push({ action: "tw-relogin", label: "TW: Re-login" });
    // The BotGuard sidecar is an ALERT, not a status: while it is down,
    // signature-ciphered formats cannot be resolved at all (sig has no
    // fallback) and a PO token mint fails at once (no goja fallback). It carries
    // no action — the supervisor is already retrying and there is nothing for
    // the operator to click.
    if (this.sidecarHealthy === false)
      warningItems.push({
        label: "PO tokens: sidecar down",
        title: "The BotGuard sidecar is not running — signature-ciphered formats and PO tokens are unavailable. Moombox is retrying.",
      });

    if (warningsEl) {
      warningsEl.textContent = "";
      for (const w of warningItems) {
        const span = document.createElement("span");
        span.className = "status-warning";
        if (w.action) {
          span.dataset.action = w.action;
          // A clickable non-control: without these a keyboard user cannot
          // reach it and a screen reader announces it as text (#check-countdown
          // is the pattern this copies). Only the ACTIONABLE warnings get
          // them — the sidecar alert below is a statement, and a Tab stop
          // that does nothing is the failure this whole change removes.
          span.setAttribute("role", "button");
          span.setAttribute("tabindex", "0");
        }
        span.title = w.title || "Click to re-login";
        span.textContent = w.label;
        warningsEl.appendChild(span);
      }
    }
    if (warningsIcon) {
      const hasWarnings = warningItems.length > 0;
      warningsIcon.classList.toggle("active", hasWarnings);
      if (hasWarnings) {
        warningsIcon.textContent = "";
        const icon = document.createElement("sl-icon");
        icon.name = "exclamation-triangle";
        warningsIcon.appendChild(icon);
        warningsIcon.title = warningItems.map(w => w.label).join(", ");
        // The icon is glyph-only, so its accessible name is the title — and
        // aria-label WINS over title for a screen reader, which is why it is
        // kept in step here rather than left at the markup's static
        // "Warnings": a stale label would hide the very warnings it stands for.
        warningsIcon.setAttribute("aria-label", warningsIcon.title);
        if (warningItems[0].action) {
          warningsIcon.dataset.action = warningItems[0].action;
          warningsIcon.setAttribute("role", "button");
          warningsIcon.setAttribute("tabindex", "0");
        } else {
          // The same rule the spans follow (C5): a warning with no action is a
          // statement, so the icon standing in for it is not a button and not
          // a Tab stop. Before this it was neither — creating a dead Tab stop
          // here would have been a regression of the very failure being fixed.
          delete warningsIcon.dataset.action;
          warningsIcon.removeAttribute("role");
          warningsIcon.removeAttribute("tabindex");
        }
      } else {
        // Clear stale children, title, and dataset from previous warnings
        warningsIcon.textContent = "";
        warningsIcon.title = "";
        warningsIcon.setAttribute("aria-label", "Warnings");
        delete warningsIcon.dataset.action;
      }
    }

    // 2. + 3. Platform indicators (hidden if the platform is not active).
    //
    // Both branch chains now live in cookieIndicatorState, in utils.js, for
    // two reasons. They had already drifted — YouTube had a no-cookies arm and
    // Twitch did not, because the server sent no `found` for Twitch — and this
    // decision is the one the whole three-state change exists to fix, so it
    // has to be testable by EXECUTION rather than by matching source text.
    // The relogin flag stays computed here rather than inside the helper: it
    // arrives on a different key of the status payload than the per-platform
    // check result the badge reads. It is NOT gated on auto_enabled — see the
    // warnings block above for why.
    //
    // `parked` is computed here for the mirror-image reason: it comes off the
    // JOB LIST, not off the status payload at all. Read fresh rather than from
    // _parkedPlatforms, so that the four pre-existing triggers above — which
    // never went through _syncParkedBadge — render the same badge this one
    // does; a memoised value would let a config load paint a stale escalation.
    const parked = parkedCookiePlatforms(this.jobs);
    for (const [platform, id, active, status] of [
      ["youtube", "yt-indicator", ytActive, this.cookieStatus],
      ["twitch", "tw-indicator", twActive, this.twitchAuthStatus],
    ]) {
      const el = document.getElementById(id);
      if (!el) continue;
      el.style.display = active ? "" : "none";
      if (!active) continue;
      const relogin = !!this.autoCookieReloginRequired?.[platform];
      const { className, title } = cookieIndicatorState(platform, status, relogin, parked[platform]);
      el.className = className;
      el.title = title;
      // The sentence is also the pill's accessible name: a title alone is out
      // of reach on a phone. The pill's own text says when the first check is
      // pending (cookieIndicatorText).
      el.setAttribute("aria-label", title);
      el.textContent = cookieIndicatorText(platform, status);
    }

    // The first cookie check has not answered: ask again shortly, until it has.
    this._pollCookieCheck(checkPending);

    // 4. Hide refresh button if neither platform is active
    const refreshBtn = document.getElementById("btn-refresh-cookies");
    if (refreshBtn) {
      refreshBtn.style.display = (ytActive || twActive) ? "" : "none";
    }
  }

  /**
   * While the server's first cookie auth check is still running, the status
   * payload says `checked: false` and the badges read "checking". Nothing
   * pushes the answer, so ask /api/status again every 2 s until it arrives.
   * One timer at most; it is not re-armed once the status is checked.
   */
  _pollCookieCheck(pending) {
    if (!pending) {
      clearTimeout(this._cookieCheckTimer);
      this._cookieCheckTimer = null;
      return;
    }
    if (this._cookieCheckTimer) return;
    this._cookieCheckTimer = setTimeout(async () => {
      this._cookieCheckTimer = null;
      // Only the cookie keys are taken: loadStatus also acts on the version
      // and the zone, which this poll has no business repeating.
      const authGen = this._nextAuthGen();
      try {
        const status = await this._fetchStatus();
        if (status && authGen === this._authGen) {
          this.autoCookieReloginRequired = status.autoCookieReloginRequired || null;
          this.cookieStatus = status.cookieStatus;
          this.twitchAuthStatus = status.twitchAuthStatus;
        }
      } catch (e) {
        // The server is restarting or unreachable; the next round asks again.
      }
      // Redraws, and arms the next round while the check is still pending.
      this.updateStatusBar();
    }, 2000);
  }

  // ===== Version / Update Indicator =====

  /** Delegates to UpdateController — settings.js calls this through `app.`. */
  updateVersionIndicator(...args) {
    return this.updates.updateVersionIndicator(...args);
  }

  /** Delegates to UpdateController — settings.js reads this and the Task 5 pin assigns it. */
  get _updateAvailable() {
    return this.updates.available;
  }

  set _updateAvailable(v) {
    this.updates.available = v;
  }

  // ===== WebSocket Management =====

  connectWebSocket() {
    // Cancel any queued reconnect so a manual/early connect can't stack a
    // second attempt behind this one (the timer used to be untracked).
    if (this._reconnectTimer) {
      clearTimeout(this._reconnectTimer);
      this._reconnectTimer = null;
    }
    // Only clear the ping interval — countdown/timestamp timer runs
    // independently of WebSocket and should keep ticking during reconnection.
    if (this._pingInterval) {
      clearInterval(this._pingInterval);
      this._pingInterval = null;
    }

    // Close any existing connection to prevent zombie sockets
    if (this.ws) {
      this.ws.onclose = null; // Prevent triggering another reconnect
      this.ws.close();
    }

    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    const wsUrl = `${protocol}//${window.location.host}`;

    this.logPanel.newConnection();
    const ws = new WebSocket(wsUrl);
    this.ws = ws;
    // Whether this socket ever opened: a close before it did is how a
    // refused upgrade (an expired session's 401) looks to the page.
    let opened = false;
    this._setConnState(ConnState.CONNECTING, { attempt: this.reconnectAttempts });

    this.ws.onopen = () => {
      console.log("WebSocket connected");
      opened = true;
      this._wsEverOpened = true;
      this._wsRefusedReachable = 0;
      this._syncWsNotice();
      const isReconnect = this.reconnectAttempts > 0;
      this.reconnectAttempts = 0;
      this._lastPong = Date.now();
      this._lastPingSent = Date.now();
      this._wsGotFrame = false;
      this._setConnState(ConnState.OPEN);
      // Refresh status (cookie + Twitch auth) on reconnect
      this.loadStatus();
      // Reload config on reconnect so the form reflects any server-side changes
      // (e.g., config saved via TUI, or restart-required settings that were applied).
      // Skip if user has unsaved changes to avoid silently overwriting their edits.
      if (isReconnect && !this.settings.isDirty()) this.loadConfig();
      // Start client-side heartbeat to detect half-open connections.
      // Track pings sent AND pongs received so a transient send() throw
      // (InvalidStateError during a tab/network blip) doesn't cause a false
      // positive close — we only declare the socket dead when a ping has
      // been sent AND the corresponding pong hasn't come back in time.
      if (this._pingInterval) clearInterval(this._pingInterval);
      this._pingInterval = setInterval(() => {
        if (this.ws?.readyState !== WebSocket.OPEN) return;
        const now = Date.now();
        // Dead-socket check: a ping went out and no pong has come back for
        // 45s (longer while the first frame is still arriving, see
        // WS_FIRST_FRAME_TIMEOUT_MS). Measured against _lastPong —
        // _lastPingSent refreshes every tick (send() buffers without
        // throwing on dead sockets), so a staleness check against it would
        // never fire.
        const pongTimeout = this._wsGotFrame ? WS_PONG_TIMEOUT_MS : WS_FIRST_FRAME_TIMEOUT_MS;
        if (this._lastPingSent > this._lastPong && now - this._lastPong > pongTimeout) {
          console.log("WebSocket heartbeat timeout — reconnecting");
          this.ws.close();
          return;
        }
        try {
          this.ws.send(JSON.stringify({ type: "ping" }));
          this._lastPingSent = now;
        } catch {
          /* connection closed between check and send; next tick will observe readyState */
        }
      }, 15000);
    };

    this.ws.onmessage = (event) => {
      if (!this._wsGotFrame) {
        // The first frame proves the socket alive, so the pong clock starts
        // again from it. A snapshot that took over 45 s to arrive would
        // otherwise switch the heartbeat to its 45 s limit with _lastPong
        // still dating from onopen, and a tick landing before the pongs
        // queued behind the snapshot closed the socket it had just read.
        this._wsGotFrame = true;
        this._lastPong = Date.now();
      }
      try {
        const message = JSON.parse(event.data);
        this.handleMessage(message);
      } catch (e) {
        console.error("Failed to parse WebSocket message:", e);
      }
    };

    this.ws.onclose = () => {
      console.log("WebSocket disconnected");
      if (this._pingInterval) { clearInterval(this._pingInterval); this._pingInterval = null; }
      if (opened) {
        this.scheduleReconnect();
        return;
      }
      // Closed before it opened: one probe tells an expired session from a
      // server that is down or restarting, and both from a server that
      // answers HTTP and refuses only the upgrade.
      this._probeSession().then((answer) => {
        // A newer socket (a manual connect) owns the connection now.
        if (this.ws !== ws) return;
        // Refusals count only in a row: an answer that is not "reachable"
        // starts again, so a restart is never added to a proxy fault.
        this._wsRefusedReachable = answer === "reachable" ? this._wsRefusedReachable + 1 : 0;
        this._syncWsNotice();
        if (answer === "unauthorized") this._holdUnauthorized(ws);
        else this.scheduleReconnect();
      });
    };

    this.ws.onerror = (error) => {
      console.error("WebSocket error:", error);
    };
  }

  scheduleReconnect() {
    this.reconnectAttempts++;
    const delay = Math.min(
      1000 * Math.pow(1.5, this.reconnectAttempts - 1),
      30000,
    );
    console.log(
      `Reconnecting in ${Math.round(delay)}ms (attempt ${this.reconnectAttempts})`,
    );
    // Tracked so connectWebSocket can cancel a queued attempt instead of
    // relying solely on socket-neutralization to avoid doubled connects.
    if (this._reconnectTimer) clearTimeout(this._reconnectTimer);
    this._reconnectTimer = setTimeout(() => {
      this._reconnectTimer = null;
      this.connectWebSocket();
    }, delay);
    this._setConnState(ConnState.BACKOFF, { attempt: this.reconnectAttempts, delay });
  }

  /**
   * UNAUTHORIZED: show "Session expired" and re-probe every
   * SESSION_RECHECK_MS on the reconnect timer (so a manual connect cancels
   * it). A 401 holds the state; anything else reconnects, and a network error
   * there lands in the plain backoff through the new socket's own probe. A
   * probe that answers after a newer socket exists changes nothing.
   * @param {WebSocket} ws the socket whose refusal began the hold
   */
  _holdUnauthorized(ws) {
    this._setConnState(ConnState.UNAUTHORIZED);
    if (this._reconnectTimer) clearTimeout(this._reconnectTimer);
    this._reconnectTimer = setTimeout(() => {
      this._reconnectTimer = null;
      this._probeSession().then((answer) => {
        if (this.ws !== ws) return;
        if (answer === "unauthorized") this._holdUnauthorized(ws);
        else this.connectWebSocket();
      });
    }, SESSION_RECHECK_MS);
  }

  /**
   * The session probe sent when a socket closes without having opened: the
   * browser hides the upgrade's status, so a cheap GET /api/status — behind
   * the same AuthMiddleware as the upgrade, so a local peer is waived exactly
   * as it is — says whether the session is gone. It goes through the wrapped
   * fetch, whose 401 handling reloads to the login page; there is no second
   * 401 path here. Resolves "unauthorized" for a 401, "reachable" for a
   * success, and "unreachable" for anything else: a server that cannot be
   * reached, or a proxy answering for one that is down (a 502). Only
   * "unauthorized" stops the retries; the other two are a plain backoff,
   * never a reload, and differ only in what _syncWsNotice makes of them.
   * @returns {Promise<"unauthorized"|"reachable"|"unreachable">}
   */
  async _probeSession() {
    try {
      const response = await fetch("/api/status");
      if (response.status === 401) return "unauthorized";
      return response.ok ? "reachable" : "unreachable";
    } catch {
      return "unreachable";
    }
  }

  /**
   * The only writer of the Tasks panel's #jobs-ws-notice, which stands in
   * for the loading skeleton when live updates cannot connect (owner ruling
   * R-B3). The job list arrives only over the WebSocket, so a page whose
   * HTTP works and whose upgrade is refused — usually a reverse proxy that
   * does not forward WebSocket upgrades — showed the skeleton for ever under
   * a small "Reconnecting (n)" label.
   *
   * Derived, never stored: shown while no socket of this page has opened
   * and WS_REFUSED_NOTICE_AFTER upgrades in a row were refused with the
   * server answering over HTTP. The first open clears it and puts the
   * skeleton back for the moment before initial_state draws the list. A
   * page that has connected before keeps its list and the status bar's
   * label instead. The retries go on underneath; no job list is loaded
   * over HTTP, which would read as live.
   */
  _syncWsNotice() {
    const show = this._wsNoticeShows();
    const notice = document.getElementById("jobs-ws-notice");
    if (notice) notice.hidden = !show;
    document.getElementById("jobs-skeleton")?.classList.toggle("ws-refused", show);
  }

  /** Whether the "live updates cannot connect" notice is due (see _syncWsNotice). */
  _wsNoticeShows() {
    return !this._wsEverOpened && this._wsRefusedReachable >= WS_REFUSED_NOTICE_AFTER;
  }

  /**
   * The only writer of #connection-status. Keeps the class names the CSS
   * styles (connected / disconnected) and adds the state's own name.
   * @param {string} state one of ConnState
   * @param {{attempt?: number, delay?: number}} [info]
   */
  _setConnState(state, { attempt = 0, delay = 0 } = {}) {
    this._connState = state;
    const statusEl = document.getElementById("connection-status");
    if (!statusEl) return;
    // The label sits in the fixed-height status bar beside the active
    // indicator and the check countdown, so it stays short enough for a
    // phone; the countdown and attempt detail go in the title.
    let cls, iconName, text, detail;
    switch (state) {
      case ConnState.OPEN:
        cls = "connected"; iconName = "wifi"; text = "Connected";
        break;
      case ConnState.BACKOFF:
        cls = "disconnected backoff"; iconName = "wifi-off";
        text = `Reconnecting (${attempt})`;
        detail = `Reconnecting in ${Math.max(1, Math.round(delay / 1000))}s (attempt ${attempt})`;
        break;
      case ConnState.UNAUTHORIZED:
        cls = "disconnected unauthorized"; iconName = "lock"; text = "Session expired";
        break;
      default:
        cls = "disconnected connecting"; iconName = "wifi-off";
        if (attempt > 0) {
          text = `Reconnecting (${attempt})`;
          detail = `Reconnecting… (attempt ${attempt})`;
        } else {
          text = "Connecting...";
        }
    }
    statusEl.className = cls;
    statusEl.title = detail ?? text;
    statusEl.textContent = "";
    const icon = document.createElement("sl-icon");
    icon.name = iconName;
    const label = document.createElement("span");
    label.className = "status-label";
    label.textContent = ` ${text}`;
    statusEl.appendChild(icon);
    statusEl.appendChild(label);
  }

  /**
   * Rebuild the Player tab's video picker when that tab is open. It used to
   * refresh only on tab activation, Open in Player and a ZIP import, so with
   * the tab open a recording that finished was not offered and a deleted one
   * stayed listed. A hidden Player tab rebuilds on activation as before.
   */
  _refreshPlayerPicker() {
    if (this._activePanel === "player" && this.player?.playerInitialized) {
      this.player.loadPlayerJobList();
    }
  }

  handleMessage(message) {
    const p = message.payload;
    switch (message.type) {
      case "initial_state": {
        if (!p) break;
        // Carry the staging fields only GET /api/jobs/:id adds (hasStaging,
        // hasSegments, asides, keptChatSidecar, unmuxedParts, muxGaps, unusedRootRecording) across the replace, as
        // jobs_update and job_update do: the snapshot rows are raw, and an
        // open details dialog refreshed from one below lost its Resume and
        // Mux buttons. Not only on reconnect — the hub sends this snapshot
        // in place of a frame a lagging tab dropped. Archived rows too: a
        // deep-linked dialog may hold the enriched copy there.
        this.details._preserveStagingFields([...this.jobs, ...this.archivedJobs], p.jobs || []);
        this.jobs = p.jobs || [];
        this.logPanel.setSnapshot(p.logs || [], p.logSeq);
        // Through setAdmissionHolds, not a bare assignment: the snapshot may
        // close or open the disk gate a disconnect missed, and the gauge and
        // indicator read it (stats.onAdmissionHolds).
        if (p.admissionHolds) this.setAdmissionHolds(p.admissionHolds);
        if (p.channelMonitors) this.setChannelMonitors(p.channelMonitors);
        this.nextFeedCheck = p.nextFeedCheck || 0;
        this.nextDecapiCheck = p.nextDecapiCheck || 0;
        this.nextTwitchCheck = p.nextTwitchCheck || 0;
        if (typeof p.hideFinishedAgeDays === "number") {
          this.hideFinishedAgeDays = p.hideFinishedAgeDays;
        }
        // Backfill snapshot: a scan pages for minutes, so connecting
        // mid-flight is the common case — rebuild the map from the server's
        // per-channel entries (reconnects drop states that ended while the
        // socket was down).
        this.backfillStatus = {};
        for (const b of p.backfill || []) {
          if (b && b.channel && isBackfillRetained(b.state)) {
            this.backfillStatus[b.channel] = b;
          }
        }
        this.settings.refreshBackfillBadges();
        // Running trims: a trim outlives the page that started it, so a
        // reload mid-trim learns of it here. Rebuilt, not merged — one that
        // ended while the socket was down is dropped — and before the open
        // details are refreshed below, which redraws their Trims section.
        this.runningTrims = {};
        for (const t of p.runningTrims || []) {
          if (t?.id && t.jobId) this.runningTrims[t.id] = t;
        }
        if (this.selectedJobId) {
          // An archived row's open details are not refreshed below.
          const shown = this.jobs.find((j) => j.id === this.selectedJobId)
            || this.archivedJobs.find((j) => j.id === this.selectedJobId);
          if (shown) this.details._syncTrims(shown);
        }
        // On a reconnect the archived list may carry rows the fresh active
        // list now owns again — prune them so neither panel double-counts.
        const archivedPruned = this._pruneArchivedAgainstActive();
        const archivedMoved = this._evaluateArchiveBoundary({ silent: true });
        this.renderJobs();
        if (archivedMoved || archivedPruned) this.renderArchivedJobs();
        // The snapshot carries active rows only, so it cannot say which
        // archived rows were deleted (or which jobs aged into the archive)
        // while frames were missed. A hidden Archived panel refetches when
        // shown; an open one refetches now, or its ghosts stay until the
        // operator leaves the tab and comes back.
        if (this._activePanel === "archived") this.fetchArchivedJobs();
        this._syncParkedBadge();
        this.logPanel.renderLogs();
        this.updateCheckCountdown();
        if (p.connectivity !== undefined) {
          this.handleConnectivityChange({ online: p.connectivity });
        }
        // Refresh details dialog if open — data may have changed during disconnect
        if (this.selectedJobId) {
          const job = this.jobs.find((j) => j.id === this.selectedJobId);
          if (job) {
            this.details.updateJobDetails(job);
          } else if (!this.archivedJobs.some(j => j.id === this.selectedJobId)) {
            this._verifyJobExists(this.selectedJobId);
          }
        }
        // Safe to re-run on every reconnect: _consumeJobHash clears the hash
        // before it looks anything up, so the second call finds nothing.
        this._consumeJobHash();
        break;
      }

      case "jobs_update": {
        this.details._preserveStagingFields(this.jobs, p || []);
        this.jobs = p || [];
        // Drop any archived rows the server just promoted back to active
        // (threshold increase) before re-evaluating, so a job is never shown
        // in both panels while the async fetchArchivedJobs() catches up.
        const archivedPruned = this._pruneArchivedAgainstActive();
        const archivedMoved = this._evaluateArchiveBoundary({ silent: true });
        this.renderJobs();
        if (archivedMoved || archivedPruned) this.renderArchivedJobs();
        this._syncParkedBadge();
        // Update details dialog if open (but don't reload logs)
        if (this.selectedJobId) {
          const job = this.jobs.find((j) => j.id === this.selectedJobId);
          if (job) {
            this.details.updateJobDetails(job);
          } else if (!this.archivedJobs.some(j => j.id === this.selectedJobId)) {
            // Job not in active or cached archived lists — may have been archived or deleted.
            // Verify via API to avoid closing the dialog when a job simply transitions to archived.
            this._verifyJobExists(this.selectedJobId);
          }
        }
        break;
      }

      case "job_update": {
        // Single job update - update in place; a status change re-places the
        // one card (_moveJobCard), with renderJobs as the fallback
        const updatedJob = p;
        if (!updatedJob?.id) break;
        const jobIndex = this.jobs.findIndex((j) => j.id === updatedJob.id);
        if (jobIndex !== -1) {
          const oldJob = this.jobs[jobIndex];
          const oldStatus = oldJob.status;
          // Preserve computed staging fields from prior enriched state — WS
          // delivers raw DB objects without them. Through the SAME helper the
          // jobs_update case uses, so the set of carried-forward fields is one
          // list in one place: this case once kept its own two-field copy, and
          // when Arc A added `asides`/`keptChatSidecar` to the helper the
          // details dialog's Set-aside Recordings section vanished here on the
          // status change that first makes Recover offerable.
          this.details._preserveStagingFields([oldJob], [updatedJob]);
          this.jobs[jobIndex] = updatedJob;
          // A status change moves the row to another group: its card is taken
          // out and drawn again at its new place. The whole list is rebuilt
          // only when that cannot be proven equal to a rebuild.
          if (oldStatus !== updatedJob.status) {
            if (!this._moveJobCard(updatedJob)) this.renderJobs();
            // The picker lists Finished recordings: one that just finished
            // muxing (or left Finished) changes what it should offer.
            if (oldStatus === "Finished" || updatedJob.status === "Finished") this._refreshPlayerPicker();
            // A status change (esp. → Finished) can cross the archive
            // threshold immediately (hide_finished_age_days = 0). Progress
            // ticks can't — jobs age by TIME, which the 60s sweep interval
            // already covers — so archive evaluation stays off the per-tick
            // path instead of running an O(n) scan on every progress update.
            this._evaluateArchiveBoundary();
          } else if (this._sortKey(oldJob) !== this._sortKey(updatedJob)) {
            // Same status but a new place in it — a renamed title, or a
            // completed row's newer updatedAt. Patched in place, the card
            // kept its old position while Arrow navigation and Enter index
            // the re-sorted list, so the highlight landed on the wrong row.
            if (!this._moveJobCard(updatedJob)) this.renderJobs();
          } else if (this._cardShape(oldJob) !== this._cardShape(updatedJob)) {
            // Same status and place, but a part of the card updateJobCard
            // does not patch has changed (the Resume icon, the missing-tail
            // mark): this one card is drawn again where it is. It used to
            // wait for the next rebuild of the whole list, which another
            // row's status change no longer causes.
            if (!this._moveJobCard(updatedJob)) this.renderJobs();
          } else {
            // No active-indicator refresh in either same-status branch: the
            // indicator counts rows by status, and no status moved.
            this.updateJobCard(updatedJob);
          }
        } else {
          // Job not in the active array — either brand new, or one that had
          // aged into archivedJobs and just had a field change that bumped its
          // updated_at back inside the active window (UpdateJobFields touches
          // updated_at on every change). Push it active, then drop any stale
          // archived copy so it isn't shown in both panels. If it's still aged,
          // the _evaluateArchiveBoundary() below re-archives it.
          //
          // The archived copy is the one that holds the enriched fields the
          // details dialog fetched for it, and the dialog refresh below would
          // otherwise rebuild on a row without them.
          const archivedCopy = this.archivedJobs.find((j) => j.id === updatedJob.id);
          if (archivedCopy) this.details._preserveStagingFields([archivedCopy], [updatedJob]);
          this.jobs.push(updatedJob);
          if (this._pruneArchivedAgainstActive()) this.renderArchivedJobs();
          // One discovered job costs one card, not a rebuild of every card in
          // the list — backfill adds them one broadcast at a time. The helper
          // declines (and we fall back) whenever the fast path is not exactly
          // equivalent to the full render.
          if (!this._insertJobCard(updatedJob)) this.renderJobs();
          this._evaluateArchiveBoundary();
        }
        // Outside both branches on purpose: a park arrives either as a status
        // change on a job already held or as a row this tab has not seen, and
        // the badge owes the same answer to both. The change gate inside is
        // what keeps this off the DOM on a progress tick.
        this._syncParkedBadge();
        // Likewise the open dialog: it can be showing a job this tab holds
        // only in archivedJobs (a deep link, or an archived row the operator
        // opened), and the update that reaches the upsert branch is exactly
        // the one it is waiting on — a Mark Watched on an archived row lands
        // here once its updated_at moves back inside the active window.
        if (this.selectedJobId === updatedJob.id) {
          this.details.updateJobDetails(updatedJob);
        }
        break;
      }

      case "job_progress": {
        // The ~60 Hz frame (sweep 2 row #40 / O-O): only the columns a
        // progress tick writes. MERGE it onto the row already held — the frame
        // carries twelve fields and the row has fifty, and job_update is now
        // the only frame that restates the rest (title, channel, thumbnail,
        // description, output paths, gaps) as well as the client-computed
        // hasStaging / hasSegments flags.
        const patch = p;
        if (!patch?.id) break;
        const idx = this.jobs.findIndex((j) => j.id === patch.id);
        // Unknown id: drop it, never upsert. Pushing a twelve-field frame in as
        // a new job would put an untitled, thumbnail-less row in the list; the
        // job_update that announces a job carries the whole row, and a resync
        // restates it. It is also what stops a tick that raced a delete from
        // resurrecting the row the way job_update's upsert branch would.
        if (idx === -1) break;

        const oldStatus = this.jobs[idx].status;
        // Trust the wire: every key the frame carries is merged as-is, including
        // one outside the twelve (an additive wire change reaches the row
        // without a client release). The key set is pinned on the SERVER, by
        // newJobProgressFrame and its frame-size test, and job_update already
        // takes a whole row from the same authenticated socket — a client-side
        // allow-list would buy nothing and would break additive evolution.
        const merged = { ...this.jobs[idx], ...patch };
        // Replaced, not mutated in place, exactly as job_update does it:
        // nothing holds a reference to an element of this.jobs — every reader
        // looks the row up by id at the moment it needs it.
        this.jobs[idx] = merged;

        // The server does not send a status-changing progress frame
        // (isProgressOnlyChange excludes "status"); `status` rides the frame so
        // this can be CHECKED rather than assumed. Not dead code: the read-back
        // is under the write lock but the broadcast is not (UpdateJobFields
        // unlocks before notifying), so a tick that raced a transition can
        // carry the new status ahead of its own job_update — which would then
        // see no change and skip the re-sort. A transition re-sorts the list
        // and can cross the archive threshold, so it takes the same path
        // job_update gives it.
        if (merged.status !== oldStatus) {
          if (!this._moveJobCard(merged)) this.renderJobs();
          this._evaluateArchiveBoundary();
          this._syncParkedBadge();
          if (this.selectedJobId === merged.id) this.details.updateJobDetails(merged);
        } else if (document.hidden) {
          // Nobody can see the card or the dialog, and WebSocket frames are
          // not throttled in a background tab: the row above is merged on
          // every frame as always, and the drawing waits for the tab to show
          // again (_repaintAfterHidden). Only this branch: a status change
          // re-sorts the list and can cross the archive boundary, so it is
          // handled when it arrives.
          this._repaintOnVisible = true;
        } else {
          // Status unchanged, so the status bar's active count (a count of
          // rows by status) cannot have moved: no O(n) recount per tick.
          this.updateJobCard(merged);
          // Update details dialog if this job is selected
          if (this.selectedJobId === merged.id) this.details.updateJobDetails(merged);
        }
        // Nothing else on the fast path: a park is a status change, so
        // _syncParkedBadge() belongs to the branch above, and jobs age by TIME,
        // which the 60s sweep interval already covers — archive evaluation
        // stays off the per-tick path for exactly the reason the job_update
        // handler states.
        break;
      }

      case "config_update":
        if (!p) break;
        if (typeof p.hideFinishedAgeDays === "number") {
          const prev = this.hideFinishedAgeDays;
          this.hideFinishedAgeDays = p.hideFinishedAgeDays;
          // Decrease (or stayed): sweep active → archived for newly-aged jobs.
          // Increase: previously-archived jobs may now belong in active. The
          // server's accompanying jobs_update already widened this.jobs to
          // include them, so just refresh archivedJobs so the Archived panel
          // drops the rows that moved back into active.
          this._evaluateArchiveBoundary();
          if (p.hideFinishedAgeDays > prev) {
            this.fetchArchivedJobs();
          }
        }
        break;

      case "job_deleted": {
        // Remove the deleted job from local state without waiting for a
        // full-list rebroadcast. Backend sends this from the OnJobDeleted
        // subscriber as a discrete event, so the upsert-only job_update
        // handler no longer leaves a stale "Cancelled" row visible after delete.
        const deletedId = p?.id;
        if (!deletedId) break;
        // A deleted recording must leave the Player's picker (and stop
        // playing from a source that now 404s); loadPlayerJobList clears a
        // selection that vanished.
        this._refreshPlayerPicker();
        const deletedIdx = this.jobs.findIndex(j => j.id === deletedId);
        if (deletedIdx !== -1) {
          this.jobs.splice(deletedIdx, 1);
          // One card out, not a rebuild of every card: a batch delete of K
          // rows sends K of these frames.
          if (!this._removeJobCard(deletedId)) this.renderJobs();
          // Deleting the last parked job is the one gesture that CLEARS this
          // escalation. Without it the badge stays red over an empty list.
          this._syncParkedBadge();
        }
        // A Finished job may have aged into the archived list (client-side
        // sweep or a prior fetch); drop it there too, or a server-side delete
        // leaves a stale, still-clickable row in the Archived panel.
        this._removeArchivedJob(deletedId);
        // Close the details dialog if the deleted job is currently selected.
        if (this.selectedJobId === deletedId) {
          const dlg = document.getElementById("details-dialog");
          if (dlg?.open) dlg.hide();
          this.selectedJobId = null;
        }
        break;
      }

      case "log":
        if (p) this.logPanel.addLog(p, message.seq);
        break;

      case "check_timers":
        if (!p) break;
        this.nextFeedCheck = p.nextFeedCheck || 0;
        this.nextDecapiCheck = p.nextDecapiCheck || 0;
        this.nextTwitchCheck = p.nextTwitchCheck || 0;
        this.updateCheckCountdown();
        break;

      case "disk_status":
        this.stats.updateDiskIndicator(p);
        break;

      case "admission_holds":
        this.setAdmissionHolds(p);
        break;

      case "status_changed":
        this.onStatusChanged();
        break;

      case "channel_monitors":
        this.setChannelMonitors(p);
        break;

      case "backfill_status":
        this.handleBackfillStatus(p);
        break;

      case "trim_status":
        this.handleTrimStatus(p);
        break;

      case "update_available":
        this.updates.available = p;
        this.updates.updateVersionIndicator();
        break;

      case "update_cleared":
        // The pending release was skipped or pulled: the badge drops, and an
        // update dialog offering it closes. Only the release this page is
        // showing: a clear racing a newly-found one names the older tag.
        this.updates.withdrawn(p?.tagName);
        break;

      case "connectivity":
        this.handleConnectivityChange(p);
        break;

      case "pong":
        this._lastPong = Date.now();
        break;
    }
  }

  /**
   * setChannelMonitors stores every monitor's health list (owner ruling
   * R-H1) and redraws the Settings channel cards' "Checks failing" flags,
   * the only thing that reads it.
   */
  setChannelMonitors(monitors) {
    this.channelMonitors = monitors || null;
    this.settings.refreshChannelHealthBadges();
  }

  /**
   * setAdmissionHolds stores the backlog scheduler's hold snapshot and
   * redraws what reads it: each Queued row's line (formatProgress), an open
   * job dialog's progress row when that job is Queued, and the disk gauge and
   * indicator ("holding").
   */
  setAdmissionHolds(holds) {
    this.admissionHolds = holds || null;
    this._redrawQueuedHolds();
    this.stats.onAdmissionHolds();
  }

  /**
   * _redrawQueuedHolds rewrites each Queued row's line and an open Queued
   * job's dialog from admissionHolds at the current time, then arms a
   * one-shot timer for the earliest hold end still ahead. A row's own hold
   * (its retry backoff, a rate-limit requeue) or the YouTube rate limit runs
   * out with no admission_holds frame while the slots stay full; the timer
   * is what turns "Retry 1 of 3 at 14:05" into "Waiting in queue..." then,
   * as the TUI's per-second render does. Re-armed on every holds change.
   */
  _redrawQueuedHolds() {
    // One pass over the cards, not one document query per Queued job: a
    // backlog holds thousands of Queued rows.
    const queued = new Map();
    for (const job of this.jobs) {
      if (job.status === "Queued") queued.set(job.id, job);
    }
    const cards = queued.size ? document.querySelectorAll("#jobs-container .video-item") : [];
    for (const card of cards) {
      const job = queued.get(card.dataset.jobId);
      if (!job) continue;
      const cell = card.querySelector(".job-progress-text");
      if (!cell) continue;
      const html = this.formatProgressHtml(job);
      if (cell.dataset.progress !== html) {
        cell.innerHTML = html;
        cell.dataset.progress = html;
      }
      cell.title = this.formatProgress(job);
    }
    // An open dialog on a Queued job reads the holds too (its progress row),
    // as the TUI's details pane does; its own job_update may be far off.
    const open = this.selectedJobId ? this.jobs.find((j) => j.id === this.selectedJobId) : null;
    if (open?.status === "Queued") this.details.updateJobDetails(open);
    this._armHoldExpiry();
  }

  _armHoldExpiry() {
    clearTimeout(this._holdExpiryTimer);
    this._holdExpiryTimer = null;
    const holds = this.admissionHolds;
    if (!holds) return;
    const now = Date.now();
    let next = Infinity;
    const consider = (t) => {
      const ms = t ? Date.parse(t) : NaN;
      if (ms > now && ms < next) next = ms;
    };
    consider(holds.rateLimitedUntil);
    for (const h of Object.values(holds.jobs || {})) consider(h?.until);
    if (next === Infinity) return;
    // +1 ms: a hold holds while now < until, so the redraw lands just past
    // it. Capped at setTimeout's 2^31-1 ms limit; an early fire re-arms.
    const delay = Math.min(next - now + 1, 2 ** 31 - 1);
    this._holdExpiryTimer = setTimeout(() => {
      this._holdExpiryTimer = null;
      this._redrawQueuedHolds();
    }, delay);
  }

  handleConnectivityChange(payload) {
    const banner = document.getElementById("connectivity-banner");
    if (!banner) return;
    if (payload.online) {
      banner.classList.remove("show");
    } else {
      banner.classList.add("show");
    }
  }

  /**
   * Store the latest /api/auth/status payload and sync the status-bar logout
   * icon to it. Called from checkSecurityBanner() at boot and from
   * settings.js loadSecurityStatus() after a password is set or removed —
   * removing the password on an external install flips authRequired off,
   * and the icon must follow without a reload.
   */
  applyAuthStatus(status) {
    this.authStatus = status;
    applyLogoutVisibility(document.getElementById("btn-logout"), status);
  }

  async checkSecurityBanner() {
    // Passwordless external access — mirrors the server's startup warning.
    // Every interactive surface refuses to SET this combination, so it can
    // only come from a hand-edited config file; warn persistently, don't
    // block (update-path compatibility).
    // One of the two reads of /api/auth/status the logout icon keys off (the
    // other is settings.js loadSecurityStatus); both go through applyAuthStatus.
    try {
      const resp = await fetch("/api/auth/status");
      if (!resp.ok) return;
      const status = await resp.json();
      this.applyAuthStatus(status);
      document
        .getElementById("security-banner")
        ?.classList.toggle("show", !!status.passwordlessExternal);
    } catch {
      // Offline/restarting — the connectivity banner tells that story.
    }
  }

  /**
   * Track a per-channel backfill_status event: "scanning"/"error" store the
   * state, "done"/"idle" clear it. The Settings channel card badge (the only
   * renderer) is patched in place when that card is on screen.
   */
  handleBackfillStatus(p) {
    if (!p || !p.channel) return;
    if (isBackfillRetained(p.state)) {
      this.backfillStatus[p.channel] = p;
    } else {
      delete this.backfillStatus[p.channel];
    }
    this.settings.updateChannelBackfillBadge(p.channel);
  }

  /**
   * A trim_status frame: a trim the server runs (worker.TrimEvent). It is
   * keyed to the job and trim it names, never to whatever dialog is open —
   * a "running" frame moves that trim's bar in its job's Trims section; an
   * outcome takes the trim out of runningTrims and —
   * the dialog that asked may be long gone, or the trim may have been asked
   * for by the other UI. "finished" carries the stored record: it is merged
   * into the job this page holds (the OnTrimsChanged job_update normally
   * lands first; this makes the order not matter) and the job's open Trims
   * section is redrawn. Each outcome is toasted on every page, naming the
   * job: after a reload no page can tell which trims it asked for.
   */
  handleTrimStatus(p) {
    if (!p?.id || !p.jobId) return;
    if (p.state === "running") {
      // A trim starting, then its progress (at most four frames a second).
      this.runningTrims[p.id] = p;
      this.details.updateRunningTrim(p);
      return;
    }
    if (p.state !== "finished" && p.state !== "failed") return;
    delete this.runningTrims[p.id];
    const job = this.jobs.find((j) => j.id === p.jobId) || this.archivedJobs.find((j) => j.id === p.jobId);
    const range = `${formatTimestamp(p.startTime)} – ${formatTimestamp(p.endTime)}`;
    const of = job?.title ? ` of "${job.title}"` : "";
    if (p.state === "finished") {
      if (job && p.trim?.id) {
        const trims = Array.isArray(job.trims) ? job.trims : [];
        if (!trims.some((t) => t.id === p.trim.id)) job.trims = [...trims, p.trim];
      }
      this.showToast(`Trim ${range}${of} created`, "success");
    } else {
      this.showToast(`Trim ${range}${of} failed: ${p.error || "Failed to create trim"}`, "danger");
    }
    if (job) this.details._syncTrims(job);
  }

  /**
   * Verify whether a job still exists on the server (e.g. it was archived, not deleted).
   * Used when a job disappears from the active jobs list while the details dialog is open.
   * If the job exists, cache it in archivedJobs and update the dialog.
   * If the job is truly gone (404), close the dialog.
   */
  async _verifyJobExists(jobId) {
    // Prevent duplicate concurrent fetches for the same job
    if (this._verifyingJobId === jobId) return;
    this._verifyingJobId = jobId;
    try {
      const resp = await fetch(`/api/jobs/${jobId}`);
      if (this.selectedJobId !== jobId) return; // Selection changed during fetch
      if (resp.ok) {
        const job = stampFactsStatus(await resp.json());
        // Cache in archivedJobs so subsequent jobs_update messages skip the API check
        const existingIdx = this.archivedJobs.findIndex(j => j.id === job.id);
        if (existingIdx !== -1) {
          this.archivedJobs[existingIdx] = job;
        } else {
          this.archivedJobs.push(job);
        }
        this.details.updateJobDetails(job);
      } else if (resp.status === 404) {
        // Job truly deleted — close dialog
        const dlg = document.getElementById("details-dialog");
        if (dlg?.open) dlg.hide();
        this.selectedJobId = null;
      }
      // Other errors (500, 401, etc.) — leave dialog open with last known data
    } catch {
      // Network error — leave dialog open with last known data
    } finally {
      if (this._verifyingJobId === jobId) this._verifyingJobId = null;
    }
  }

  /**
   * Open the job named by a `#job=<id>` hash, then clear the hash.
   * Called after initial_state (the jobs list is the thing it searches) and on
   * hashchange. Idempotent: the hash is cleared before the lookup, so a
   * reconnect's second initial_state is a no-op.
   *
   * The clear is `history.replaceState(null, "", location.pathname + location.search)`
   * — jsdom implements it, and unlike assigning location.hash it leaves no
   * history entry for the operator to Back into and re-trigger.
   */
  async _consumeJobHash() {
    const hash = window.location.hash;
    if (!hash.startsWith("#job=")) return;
    // Split on the first "=" by hand and decode with decodeURIComponent —
    // never URLSearchParams. JobDeepLink (internal/notifications/mentions.go)
    // builds this fragment with url.PathEscape, which leaves "&", "=" and "+"
    // unescaped: URLSearchParams would truncate an id at a literal "&" and
    // turn a literal "+" into a space, neither of which PathEscape produces.
    const raw = hash.slice("#job=".length);
    let jobId = raw;
    try {
      jobId = decodeURIComponent(raw);
    } catch {
      // Malformed percent-escape — fall back to the raw fragment rather than
      // dropping the deep link entirely.
    }
    // Clear before the lookup: an idempotent no-op on the next call, so a
    // reconnect's second initial_state (or a stray hashchange) never re-opens
    // a dialog the operator has since closed.
    window.history.replaceState(null, "", window.location.pathname + window.location.search);

    // Claim the generation token for EVERY deep link, before the in-memory
    // lookup: a newer link that resolves from memory (a live job, which is
    // what most embeds link to) must also cancel an earlier link's slow
    // fallback fetch, or that fetch reopens its own dialog over the newer one.
    this._consumingJobId = jobId;
    const job = this.jobs.find((j) => j.id === jobId) || this.archivedJobs.find((j) => j.id === jobId);
    if (job) {
      this.details.showJobDetails(job);
      return;
    }
    // Not in memory — the commonest deep link (a Finished job past the
    // archive boundary) lands here, since archivedJobs is fetched lazily and
    // is empty on a cold load. Mirrors _verifyJobExists's response handling.
    //
    // The generation token (claimed above) is that method's `_verifyingJobId`
    // guard in the form this path needs: hashchange can fire again (a second
    // embed's link, or Back) while this fetch is in flight, and without it a
    // slow answer for the EARLIER id would open its dialog over the newer
    // selection. The token is the id, so a re-entrant call for the same id is
    // harmless.
    let status = 0;
    try {
      const resp = await fetch(`/api/jobs/${encodeURIComponent(jobId)}`);
      if (this._consumingJobId !== jobId) return; // a newer deep link took over
      status = resp.status;
      if (resp.ok) {
        const fetched = stampFactsStatus(await resp.json());
        if (this._consumingJobId !== jobId) return; // superseded while the body was read
        const existingIdx = this.archivedJobs.findIndex((j) => j.id === fetched.id);
        if (existingIdx !== -1) this.archivedJobs[existingIdx] = fetched;
        else this.archivedJobs.push(fetched);
        this.details.showJobDetails(fetched);
        return;
      }
    } catch {
      if (this._consumingJobId !== jobId) return; // superseded — the toast is not ours to show
      // Network error — status stays 0 and the toast says so below.
    } finally {
      if (this._consumingJobId === jobId) this._consumingJobId = null;
    }
    // 404 is the only status that means the job is gone. A 500 or a dead
    // socket means we could not find out, and telling an operator their
    // archive is missing when Moombox merely could not answer sends them
    // looking for a file that is still there.
    this.showToast(status === 404 ? "Job not found" : "Could not load job", "danger");
  }

  formatCountdown(epochMs) {
    if (epochMs === -1) return "…"; // a check is in progress
    if (!epochMs || epochMs < 0) return "--";
    const remaining = Math.max(0, Math.floor((epochMs - Date.now()) / 1000));
    if (remaining <= 0) return "now";
    const minutes = Math.floor(remaining / 60);
    const seconds = remaining % 60;
    if (minutes > 0) return `${minutes}m ${seconds}s`;
    return `${seconds}s`;
  }

  /**
   * The containers that can hold [data-timestamp] elements for the current
   * view: the ACTIVE panel's job/file list plus the details dialog (its own
   * subtree, independent of panels). Scoping the 1 Hz refresh to these
   * instead of document-wide querySelectorAll matters because selector
   * matching visits EVERY element \u2014 with a large VOD's chat sidebar loaded,
   * a document-wide scan walked hundreds of thousands of chat nodes per
   * second that can never match. Hidden panels refresh on activation
   * (sl-tab-show) and at worst show a \u22641s-stale relative time.
   */
  _timestampScopes() {
    const scopes = [];
    const panelContainer = {
      tasks: "jobs-container",
      archived: "archived-container",
      files: "files-table",
    }[this._activePanel];
    if (panelContainer) {
      const el = document.getElementById(panelContainer);
      if (el) scopes.push(el);
    }
    const details = document.getElementById("job-details-content");
    if (details) scopes.push(details);
    return scopes;
  }

  refreshRelativeTimestamps() {
    for (const scope of this._timestampScopes()) {
      scope.querySelectorAll("[data-timestamp]").forEach((el) => {
        const ts = el.dataset.timestamp;
        if (!ts) return;
        const relative = this.formatRelativeTime(ts);
        const prefix = el.dataset.timestampPrefix || "";
        const text = prefix + relative;
        // Diff before writing: past the 1-minute mark the string only changes
        // once a minute (then hour, then day), so the unconditional write was
        // re-assigning identical text (and dirtying layout) on ~59 of every 60
        // ticks per element \u2014 forever, scaled by card count.
        if (el.textContent !== text) {
          el.textContent = text;
          // Update title only for prefixed elements (e.g. job progress);
          // details panel spans keep their fixed full-date title set at render time
          if (prefix) el.title = text;
        }
      });
      // Update "Starts In" countdown (details panel only, but the scoped
      // query is cheap enough to run uniformly)
      scope.querySelectorAll("[data-timestamp-countdown]").forEach((el) => {
        const ts = el.dataset.timestampCountdown;
        if (!ts) return;
        const diff = Math.floor((new Date(ts).getTime() - Date.now()) / 1000);
        const text = diff > 0 ? this.formatDurationSeconds(diff) : "Now";
        if (el.textContent !== text) el.textContent = text;
      });
    }
  }

  updateCheckCountdown() {
    const el = document.getElementById("check-countdown");
    if (!el) return;
    const feed = this.formatCountdown(this.nextFeedCheck);
    const decapi = this.formatCountdown(this.nextDecapiCheck);
    const twitch = this.formatCountdown(this.nextTwitchCheck);
    // _narrowHeader is maintained by a resize listener (initializeApp) so the
    // 1 Hz tick never reads window.innerWidth \u2014 a forced-layout risk right
    // after the timestamp writes above.
    const text = this._narrowHeader
      ? `F ${feed} \u00b7 D ${decapi} \u00b7 T ${twitch}`
      : `Feed ${feed} \u00b7 DECAPI ${decapi} \u00b7 Twitch ${twitch}`;
    if (el.textContent !== text) el.textContent = text;
  }

  async checkMonitorsNow() {
    try {
      const resp = await fetch("/api/monitors/check-now", { method: "POST" });
      if (!resp.ok) {
        this.showToast("Force check failed: " + await serverErrorMessage(resp), "danger");
        return;
      }
      const data = await resp.json().catch(() => ({}));
      if (data.debounced) {
        this.showToast(`Just checked — try again in ${Math.ceil((data.retryAfterMs || 0) / 1000)}s`, "primary");
      } else if (data.success) {
        this.showToast("Checking all monitors now…", "success");
      } else {
        this.showToast("Force check failed", "danger");
      }
    } catch {
      this.showToast("Force check failed: could not reach server", "danger");
    }
  }

  // ===== Job Rendering =====

  renderJobs() {
    // While the "live updates cannot connect" notice stands in for the
    // skeleton no frame has ever arrived, so there is no list to draw. A
    // render asked for by something else (typing in the filter) would put
    // "No jobs yet" beside the notice, which the page does not know.
    if (this._wsNoticeShows()) return;

    // Remove loading skeletons on first render (any message path)
    document.getElementById("jobs-skeleton")?.remove();

    // Update active indicator in status bar
    this.stats.updateActiveIndicator(this.jobs);

    const container = document.getElementById("jobs-container");
    const emptyState = document.getElementById("empty-state");
    const filterCount = document.getElementById("tasks-filter-count");

    if (this.jobs.length === 0) {
      container.innerHTML = "";
      emptyState.style.display = "flex";
      const justSetup = sessionStorage.getItem("justCompletedSetup");
      if (justSetup) {
        sessionStorage.removeItem("justCompletedSetup");
        const icon = emptyState.querySelector("sl-icon");
        if (icon) icon.name = "check-circle";
        const msg = emptyState.querySelector("p");
        if (msg) msg.textContent = "Setup complete!";
        const subtext = emptyState.querySelector(".empty-state-subtext");
        if (subtext) subtext.textContent = "Add channels to auto-monitor streams, or add a video URL to start archiving now.";
        const cta = emptyState.querySelector(".empty-state-cta");
        if (cta) cta.style.display = "none";
        // Remove any leftover setup CTA wrapper from a previous render
        emptyState.querySelector("#empty-state-setup-cta")?.remove();
        // Inject two-button CTA
        const setupCta = document.createElement("div");
        setupCta.id = "empty-state-setup-cta";
        setupCta.style.cssText = "display:flex;gap:0.5rem;flex-wrap:wrap;justify-content:center;";
        const addChannelsBtn = document.createElement("sl-button");
        addChannelsBtn.setAttribute("variant", "default");
        addChannelsBtn.setAttribute("size", "small");
        addChannelsBtn.textContent = "Add Channels";
        addChannelsBtn.addEventListener("click", () => {
          document.querySelector('sl-tab[panel="settings"]')?.click();
          // The settings nav is an sl-menu — nothing carries data-section.
          setTimeout(() => document.querySelector('#settings-nav sl-menu-item[value="channels"]')?.click(), 100);
        });
        const addVideoBtn = document.createElement("sl-button");
        addVideoBtn.setAttribute("variant", "primary");
        addVideoBtn.setAttribute("size", "small");
        addVideoBtn.textContent = "Add Video";
        addVideoBtn.addEventListener("click", () => {
          document.getElementById("add-dialog").show();
          document.getElementById("video-url-input").value = "";
          this.resetAdvancedOptions();
          setTimeout(() => document.getElementById("video-url-input").focus(), 100);
        });
        setupCta.appendChild(addChannelsBtn);
        setupCta.appendChild(addVideoBtn);
        if (cta) cta.insertAdjacentElement("afterend", setupCta);
        else emptyState.appendChild(setupCta);
      } else {
        // Reset empty state to default content (may have been changed by a filter or setup state)
        emptyState.querySelector("#empty-state-setup-cta")?.remove();
        const icon = emptyState.querySelector("sl-icon");
        if (icon) icon.name = "inbox";
        const msg = emptyState.querySelector("p");
        if (msg) msg.textContent = "No jobs yet";
        const subtext = emptyState.querySelector(".empty-state-subtext");
        if (subtext) subtext.textContent = "Add a YouTube or Twitch URL to start archiving";
        const cta = emptyState.querySelector(".empty-state-cta");
        if (cta) cta.style.display = "";
      }
      if (filterCount) { filterCount.style.display = "none"; }
      // No jobs left — every selection is stale; clear them and sync the
      // batch bar so it doesn't stick at "N selected" with zero jobs.
      this._selectedTaskJobs.clear();
      this.updateBatchActionBar();
      return;
    }

    // Remove stale selected IDs (jobs that no longer exist) before any early
    // return below, so a filter that hides every row still syncs the bar.
    const taskJobIds = new Set(this.jobs.map(j => j.id));
    this._selectedTaskJobs.forEach(id => {
      if (!taskJobIds.has(id)) this._selectedTaskJobs.delete(id);
    });

    const filtered = this.filterBar.getFilteredJobs();
    const isFiltered = this.filterBar.tokens("jobs").length > 0;
    // And the ones the filter hides: a batch action acts on what is on
    // screen. Kept, a job ticked before the filter changed stayed in the
    // "N selected" count and in the Delete or Cancel it was never shown in.
    const visibleIds = new Set(filtered.map(j => j.id));
    this._selectedTaskJobs.forEach(id => {
      if (!visibleIds.has(id)) this._selectedTaskJobs.delete(id);
    });

    // Update filter count
    if (filterCount) {
      if (isFiltered) {
        filterCount.textContent = `${filtered.length} of ${this.jobs.length}`;
        filterCount.style.display = "";
      } else {
        filterCount.style.display = "none";
      }
    }

    if (filtered.length === 0 && isFiltered) {
      container.innerHTML = "";
      emptyState.style.display = "flex";
      const icon = emptyState.querySelector("sl-icon");
      if (icon) icon.name = "search";
      const msg = emptyState.querySelector("p");
      if (msg) msg.textContent = "No matching jobs";
      const subtext = emptyState.querySelector(".empty-state-subtext");
      if (subtext) subtext.textContent = "Search matches titles, channel names and video IDs";
      const cta = emptyState.querySelector(".empty-state-cta");
      if (cta) cta.style.display = "none";
      this.updateBatchActionBar();
      return;
    }

    emptyState.style.display = "none";

    const sortedJobs = this._sortJobs(filtered);

    // Preserve focused job across re-renders
    const focusedCard = container.querySelector(".video-item[data-focused]");
    const focusedJobId = focusedCard?.dataset.jobId;
    const hadDomFocus = !!focusedCard && document.activeElement === focusedCard;

    container.innerHTML = sortedJobs
      .map((job) => this.renderJobItem(job))
      .join("");

    // Restore focused index if the job still exists, otherwise reset
    if (focusedJobId) {
      const newIndex = sortedJobs.findIndex(j => j.id === focusedJobId);
      this.focusedJobIndex = newIndex >= 0 ? newIndex : -1;
      if (newIndex >= 0) {
        const card = container.querySelector(`.video-item[data-job-id="${CSS.escape(focusedJobId)}"]`);
        if (card) card.setAttribute("data-focused", "");
      }
    } else {
      this.focusedJobIndex = -1;
    }
    this._syncRowTabStop(container, hadDomFocus);

    // Re-apply selection state and sync the batch bar
    this._selectedTaskJobs.forEach(id => {
      const card = container.querySelector(`[data-job-id="${CSS.escape(id)}"]`);
      if (card) {
        card.classList.add("selected");
        const cb = card.querySelector(".job-checkbox");
        if (cb) cb.checked = true;
      }
    });
    this.updateBatchActionBar();
  }

  /**
   * The tab is visible again: draw what the progress frames that arrived
   * while it was hidden changed, once. The rows were merged as the frames
   * came, so this is one render of the list from this.jobs and one update of
   * an open details dialog. A no-op when no frame was skipped.
   */
  _repaintAfterHidden() {
    if (!this._repaintOnVisible) return;
    this._repaintOnVisible = false;
    this.renderJobs();
    if (this.selectedJobId) {
      const job = this.jobs.find((j) => j.id === this.selectedJobId);
      if (job) this.details.updateJobDetails(job);
    }
  }

  /**
   * Splice ONE new row into the already-rendered list instead of rebuilding
   * every card (and every Shoelace shadow root) in it.
   *
   * Feed, DECAPI and backfill discovery each add one job per broadcast, so a
   * backfill page that finds K jobs used to run K consecutive full renders.
   *
   * Returns false — and the caller must fall back to renderJobs() — whenever
   * the fast path cannot be trusted (_detachJobCard keeps the same contract
   * for taking a card out):
   *   - the container is absent,
   *   - a filter is active (the match set and the "N of M" count both have to
   *     be recomputed),
   *   - nothing is rendered yet (first job, skeleton, or an empty state),
   *   - the job is not in this.jobs,
   *   - the rendered cards are not this.jobs minus exactly this job, in order
   *     — which is also what catches a card that is somehow already on screen.
   * @param {object} job
   * @returns {boolean}
   */
  _insertJobCard(job) {
    const container = document.getElementById("jobs-container");
    if (!container) return false;
    if (this.filterBar.tokens("jobs").length > 0) return false;

    const cards = [...container.querySelectorAll(".video-item")];
    if (cards.length === 0) return false;

    const sorted = this._sortJobs(this.jobs);
    if (cards.length !== sorted.length - 1) return false;
    const index = sorted.findIndex((j) => j.id === job.id);
    if (index < 0) return false;
    // The counts agreeing is not the same as the lists agreeing: walk the
    // rendered ids against the sort MINUS the newcomer, so the splice is only
    // taken when the result is provably what renderJobs() would have drawn.
    for (let i = 0, k = 0; i < sorted.length; i++) {
      if (i === index) continue;
      if (cards[k++].dataset.jobId !== sorted[i].id) return false;
    }

    const html = this.renderJobItem(job);
    if (index >= cards.length) {
      container.insertAdjacentHTML("beforeend", html);
    } else {
      // renderJobs() JOINS the chunks before parsing, so each card's trailing
      // indentation and the next card's leading indentation land in ONE text
      // node between them. insertAdjacentHTML cannot split that node, so the
      // chunk's leading run is moved to its tail instead — which is what makes
      // the spliced list byte-identical to a rebuild (pinned by the
      // differential test) rather than merely equivalent, on a list no card has
      // ticked yet: `data-progress` below is per-card update state, not
      // rendered content, so a ticked card carries an attribute a fresh render
      // does not until its own first tick.
      const lead = html.slice(0, html.length - html.trimStart().length);
      const trail = html.slice(html.trimEnd().length);
      cards[index].insertAdjacentHTML("beforebegin", html.trim() + trail + lead);
    }

    // Everything renderJobs() would have refreshed for an ADD. Selection and
    // focus markers on the existing cards survive untouched because those
    // cards were not rebuilt; the new one is neither selected nor focused. The
    // focused INDEX is positional, so a card spliced at or above it shifts it.
    if (this.focusedJobIndex >= index) this.focusedJobIndex += 1;
    this._syncRowTabStop(container);
    this.stats.updateActiveIndicator(this.jobs);
    return true;
  }

  /**
   * Take ONE card out of the rendered list instead of rebuilding every card.
   * The twin of _insertJobCard, with the same contract: it returns null, and
   * the caller must fall back to renderJobs(), unless the result is provably
   * what a rebuild would draw. It declines when:
   *   - the container is absent,
   *   - a filter is active (the match set and the "N of M" count change),
   *   - no other job is left (the empty state is renderJobs()'s to draw),
   *   - the rendered cards are not exactly this card plus the sorted
   *     this.jobs without this job, in order.
   * this.jobs may still hold the job (a move) or not (a delete).
   * @param {string} jobId
   * @returns {{wasFocused: boolean, hadDomFocus: boolean}|null} wasFocused:
   *   the card was the highlighted row; hadDomFocus: it also held DOM focus
   */
  _detachJobCard(jobId) {
    const container = document.getElementById("jobs-container");
    if (!container) return null;
    if (this.filterBar.tokens("jobs").length > 0) return null;

    const rest = this._sortJobs(this.jobs).filter((j) => j.id !== jobId);
    if (rest.length === 0) return null;
    const cards = [...container.querySelectorAll(".video-item")];
    if (cards.length !== rest.length + 1) return null;
    let at = -1;
    for (let i = 0, k = 0; i < cards.length; i++) {
      const id = cards[i].dataset.jobId;
      if (at < 0 && id === jobId) { at = i; continue; }
      if (k >= rest.length || id !== rest[k++].id) return null;
    }
    if (at < 0) return null;

    // The whitespace goes with the card, so the list stays byte-identical to
    // a rebuild: between two cards sits one card's trailing indentation and
    // the next one's leading indentation. The run AFTER the card is dropped,
    // or the run BEFORE it when it is the last card, which leaves the list's
    // own leading and trailing runs in place. A run can be more than one
    // text node (an appended card's lead is a node of its own).
    const card = cards[at];
    const side = at === cards.length - 1 ? "previousSibling" : "nextSibling";
    for (let n = card[side]; n && n.nodeType === Node.TEXT_NODE;) {
      const next = n[side];
      n.remove();
      n = next;
    }
    const wasFocused = card.hasAttribute("data-focused");
    const hadDomFocus = document.activeElement === card;
    card.remove();
    this._syncFocusedIndex(container);
    this._syncRowTabStop(container);
    return { wasFocused, hadDomFocus };
  }

  /** focusedJobIndex from the card that carries data-focused, as renderJobs() derives it. */
  _syncFocusedIndex(container) {
    const cards = container.querySelectorAll(".video-item");
    this.focusedJobIndex = -1;
    for (let i = 0; i < cards.length; i++) {
      if (cards[i].hasAttribute("data-focused")) { this.focusedJobIndex = i; break; }
    }
  }

  /**
   * The roving tab stop of a job list: exactly one row is in the Tab order
   * (tabindex 0), the highlighted row, or the first row when none is
   * highlighted; every other row is focusable by script only (tabindex -1,
   * as renderJobItem draws it). Called by everything that draws, inserts,
   * removes or highlights a row, so it is a function of the cards alone and
   * a single-card path leaves what a rebuild leaves.
   * @param {Element} container - #jobs-container or #archived-container
   * @param {boolean} [refocus] - the highlighted row held DOM focus before
   *   its card was drawn again: give it back
   */
  _syncRowTabStop(container, refocus = false) {
    if (!container) return;
    const stop = container.querySelector(".video-item[data-focused]") || container.querySelector(".video-item");
    for (const el of container.querySelectorAll('.video-item[tabindex="0"]')) {
      if (el !== stop) el.setAttribute("tabindex", "-1");
    }
    if (!stop) return;
    if (stop.getAttribute("tabindex") !== "0") stop.setAttribute("tabindex", "0");
    if (refocus && stop.hasAttribute("data-focused")) stop.focus({ preventScroll: true });
  }

  /**
   * Make `card` its list's highlighted row: the one Arrow moves from, Enter
   * and Space open, and Tab lands on.
   */
  _highlightRow(container, card) {
    container.querySelectorAll(".video-item[data-focused]").forEach((el) => el.removeAttribute("data-focused"));
    card.setAttribute("data-focused", "");
    if (container.id === "jobs-container") this._syncFocusedIndex(container);
    this._syncRowTabStop(container);
  }

  /**
   * Remove a deleted job's card (the job is already out of this.jobs) and
   * refresh what renderJobs() would have beside it: its selection, the batch
   * bar, the focused index and the active count. False: the caller must
   * fall back to renderJobs().
   * @param {string} jobId
   * @returns {boolean}
   */
  _removeJobCard(jobId) {
    if (!this._detachJobCard(jobId)) return false;
    this._selectedTaskJobs.delete(jobId);
    this.updateBatchActionBar();
    this.stats.updateActiveIndicator(this.jobs);
    return true;
  }

  /**
   * Draw ONE job's card again at the place the sort now gives it: after a
   * status change, a new sort key, or a change to a part of the card
   * updateJobCard does not patch. The card is taken out and put back through
   * _insertJobCard, so both halves check the list against this.jobs; focus
   * and selection follow the job as they do across a rebuild. False: the
   * caller must fall back to renderJobs(), which also repairs a list this
   * left without the card.
   * @param {object} job - the row as this.jobs now holds it
   * @returns {boolean}
   */
  _moveJobCard(job) {
    const detached = this._detachJobCard(job.id);
    if (!detached) return false;
    if (!this._insertJobCard(job)) return false;
    if (detached.wasFocused) {
      const container = document.getElementById("jobs-container");
      container.querySelector(`.video-item[data-job-id="${CSS.escape(job.id)}"]`)?.setAttribute("data-focused", "");
      this._syncFocusedIndex(container);
      this._syncRowTabStop(container, detached.hadDomFocus);
    }
    // The batch bar's buttons depend on the selected rows' statuses.
    this.updateBatchActionBar();
    return true;
  }

  /**
   * What of a card depends on more than the status and the fields
   * updateJobCard patches: whether the row offers Resume, and the
   * missing-tail mark. A same-status update that changes this draws the
   * card again (_moveJobCard).
   */
  _cardShape(job) {
    const thumb = this._thumbSources(job);
    return (canResumeJob(job, { requireKnownStaging: true }) ? "R" : "-")
      + (this.incompleteIndicatorHtml(job) ? "T" : "-")
      + (thumb.thumbnailUrl || thumb.fallbackThumb ? "I" : "-");
  }

  /**
   * Draw an active job's card again when its shape is no longer oldShape.
   * For a writer that changed the row in place, outside the job_update path.
   */
  _reshapeJobCard(job, oldShape) {
    if (this._cardShape(job) === oldShape || !this.jobs.includes(job)) return;
    if (!this._moveJobCard(job)) this.renderJobs();
  }

  /**
   * Move Finished jobs in `this.jobs` that have aged past hideFinishedAgeDays
   * into `this.archivedJobs`. Mirrors the server's filterJobsByAge logic and
   * the TUI's isJobArchived check so the active panel reclassifies as time
   * passes without waiting for the user to refresh.
   *
   * Threshold semantics (match server filterJobsByAge in
   * internal/web/routes/jobs.go and cmd/moombox/helpers.go):
   *   < 0 : never archive
   *   = 0 : archive every Finished job whose updatedAt is strictly in the past
   *   > 0 : archive Finished older than N days
   *
   * Options:
   *   silent: skip the post-move re-render (caller will render).
   */
  _evaluateArchiveBoundary({ silent = false } = {}) {
    const ageDays = this.hideFinishedAgeDays;
    if (ageDays < 0 || !this.jobs.length) return false;
    const cutoffMs = ageDays * 86400 * 1000;
    const nowMs = Date.now();
    const moved = [];
    for (let i = this.jobs.length - 1; i >= 0; i--) {
      const j = this.jobs[i];
      if (j.status !== "Finished" || !j.updatedAt) continue;
      const t = Date.parse(j.updatedAt);
      if (Number.isNaN(t)) continue;
      // Strict age > threshold, matching the server's `age > hideAge`. When
      // ageDays === 0 cutoffMs is 0, so this archives every Finished job with
      // a past updatedAt — without the always-true `|| ageDays === 0` clause
      // that previously also swept a job aged exactly 0ms (server keeps it).
      if (nowMs - t > cutoffMs) {
        this.jobs.splice(i, 1);
        const existingIdx = this.archivedJobs.findIndex(a => a.id === j.id);
        if (existingIdx !== -1) {
          this.archivedJobs[existingIdx] = j;
        } else {
          this.archivedJobs.push(j);
        }
        moved.push(j.id);
      }
    }
    if (moved.length && !silent) {
      // One card out per moved row, and the whole list only when a removal
      // cannot be proven equal to a rebuild. With hide_finished_age_days = 0
      // every job that finishes lands here.
      for (const id of moved) {
        if (this._removeJobCard(id)) continue;
        this.renderJobs();
        break;
      }
      this.renderArchivedJobs();
    }
    return moved.length > 0;
  }

  /**
   * Drop archived entries that now also appear in `this.jobs` — the server
   * promoted them back to active (e.g. the user raised hideFinishedAgeDays and
   * the widened jobs_update re-included them). Without this, a job briefly
   * shows in BOTH the Tasks and Archived panels until the async
   * fetchArchivedJobs() replaces the archived list. Returns true if it changed.
   */
  _pruneArchivedAgainstActive() {
    if (!this.archivedJobs.length) return false;
    const activeIds = new Set(this.jobs.map(j => j.id));
    const before = this.archivedJobs.length;
    this.archivedJobs = this.archivedJobs.filter(j => !activeIds.has(j.id));
    return this.archivedJobs.length !== before;
  }

  /**
   * Take a row out of the archived list: deleted, or back in Tasks after a
   * Resume or Reinitialize. The id is remembered while a fetch is out, so an
   * answer built before the row left cannot bring it back.
   * @returns {boolean} whether the list held it
   */
  _removeArchivedJob(id) {
    if (this._archivedInFlight > 0) this._archivedGoneInFlight.add(id);
    const idx = this.archivedJobs.findIndex(j => j.id === id);
    if (idx === -1) return false;
    this.archivedJobs.splice(idx, 1);
    this._scheduleArchivedRender();
    return true;
  }

  /**
   * Redraw the Archived cards once in the next animation frame, however many
   * rows left the list before it: a batch delete of K rows sends K
   * job_deleted frames, and a redraw each rebuilt up to 200 cards K times.
   * Until the frame a card whose row is gone can still be on screen; a click
   * or key on it looks its row up in archivedJobs and finds nothing to act
   * on. Redraws at once where no frame would come: a hidden tab (frames are
   * paused there), or a panel that is not shown (renderArchivedJobs only
   * marks it dirty).
   */
  _scheduleArchivedRender() {
    if (this._activePanel !== "archived" || document.hidden || typeof requestAnimationFrame !== "function") {
      this.renderArchivedJobs();
      return;
    }
    if (this._archivedFramePending) return;
    this._archivedFramePending = true;
    requestAnimationFrame(() => {
      if (this._archivedFramePending) this.renderArchivedJobs();
    });
  }

  /**
   * Replace the archived list with the server's. A failure keeps the list
   * and the selection already held and shows a Retry notice: an empty list
   * here would read as "No archived jobs", which a failed request does not
   * mean. Only the newest fetch's answer is applied (house rule 6).
   */
  async fetchArchivedJobs() {
    const gen = ++this._archivedGen;
    if (this._archivedInFlight === 0) this._archivedGoneInFlight.clear();
    this._archivedInFlight++;
    // Nothing to show yet: draw the loading state. Otherwise only the
    // notices change: Retry shows that it is busy.
    if (!this._archivedLoaded && this.archivedJobs.length === 0) this.renderArchivedJobs();
    else this._syncArchivedNotices();
    let rows = null;
    try {
      const response = await fetch("/api/jobs/archived");
      if (response.ok) {
        const body = await response.json();
        if (Array.isArray(body)) rows = body;
      }
    } catch (e) {
      console.error("Failed to fetch archived jobs:", e);
    } finally {
      this._archivedInFlight--;
    }
    if (gen !== this._archivedGen) return;
    this._archivedAnsweredGen = gen;
    if (rows) {
      // The answer was built when the request was made. A row that has since
      // been deleted, or that a job_update has since moved back to Tasks, is
      // not put back: it would show as a ghost, or in both panels.
      const activeIds = new Set(this.jobs.map(j => j.id));
      this.archivedJobs = rows.filter(j => !this._archivedGoneInFlight.has(j.id) && !activeIds.has(j.id));
      this._archivedLoaded = true;
      this._archivedFetchFailed = false;
    } else {
      this._archivedFetchFailed = true;
    }
    this.renderArchivedJobs();
  }

  // Archived rows newest first, by updatedAt. Keys are read once per row.
  _sortArchived(jobs) {
    return jobs
      .map((job) => ({ job, time: new Date(job.updatedAt).getTime() }))
      .sort((a, b) => b.time - a.time)
      .map((r) => r.job);
  }

  // The Archived rows that are drawn, in drawn order: what Arrow and Enter walk.
  _archivedDrawnJobs() {
    return this._sortArchived(this.filterBar.getFilteredArchivedJobs()).slice(0, this._archivedRenderLimit);
  }

  /**
   * The only writer of the Archived fetch notices: the loading line and the
   * failure line with its Retry button. Both are derived from the fetch
   * fields and the held list. Loading shows only before the first answer,
   * and only while the NEWEST fetch is unanswered: an older fetch still out
   * says nothing about the newest one, whose failure must show. A failure
   * never hides the held list, and with nothing held it takes the empty
   * state's place rather than sitting beside "No archived jobs". Retry
   * shows busy while a newer fetch than the failed one is out.
   * @returns {{isLoading: boolean, failed: boolean}}
   */
  _syncArchivedNotices() {
    const nothingHeld = this.archivedJobs.length === 0;
    const pending = this._archivedAnsweredGen !== this._archivedGen;
    const isLoading = nothingHeld && !this._archivedLoaded && pending;
    const failed = this._archivedFetchFailed && !isLoading;
    const loadingEl = document.getElementById("archived-loading");
    if (loadingEl) loadingEl.style.display = isLoading ? "" : "none";
    const errorEl = document.getElementById("archived-fetch-error");
    if (errorEl) {
      errorEl.style.display = failed ? "" : "none";
      // Written only when it changes: the line sits in an alert region, and
      // a rewrite is announced again on every redraw.
      const text = document.getElementById("archived-fetch-error-text");
      const message = nothingHeld
        ? "Could not load the archived jobs."
        : "Could not refresh the archived jobs. The list below is from the last successful load.";
      if (text && text.textContent !== message) text.textContent = message;
      const retry = document.getElementById("archived-retry");
      if (retry) retry.loading = failed && pending;
    }
    return { isLoading, failed };
  }

  renderArchivedJobs() {
    this._archivedFramePending = false;
    // Defer DOM work while the Archived panel isn't visible. On a 24/7
    // recorder the archive grows continuously (60s sweep + job updates), and
    // this method rebuilds every drawn archived card via innerHTML — destroying
    // and recreating Shoelace shadow DOM for a hidden panel is pure waste. The
    // sl-tab-show handler re-renders on activation when this flag is set.
    if (this._activePanel !== "archived") {
      this._archivedRenderDirty = true;
      return;
    }
    this._archivedRenderDirty = false;

    const container = document.getElementById("archived-container");
    const emptyState = document.getElementById("archived-empty-state");
    const table = document.getElementById("archived-table");
    const filterCount = document.getElementById("archived-filter-count");
    const more = document.getElementById("archived-more");

    const nothingHeld = this.archivedJobs.length === 0;
    const { isLoading, failed } = this._syncArchivedNotices();

    if (nothingHeld) {
      container.innerHTML = "";
      this.focusedArchivedIndex = -1;
      table.style.display = "none";
      if (more) more.style.display = "none";
      emptyState.style.display = (isLoading || failed) ? "none" : "flex";
      const icon = emptyState.querySelector("sl-icon");
      if (icon) icon.name = "archive";
      const msg = emptyState.querySelector("p");
      if (msg) msg.textContent = "No archived jobs";
      const subtext = emptyState.querySelector(".empty-state-subtext");
      if (subtext) subtext.textContent = "Finished jobs older than the configured age will appear here automatically";
      if (filterCount) filterCount.style.display = "none";
      // No archived jobs left — clear stale selections and sync the batch bar
      this._selectedArchivedJobs.clear();
      this.updateBatchActionBar();
      return;
    }

    // Remove stale archived selected IDs before any early return below.
    const archivedJobIds = new Set(this.archivedJobs.map(j => j.id));
    this._selectedArchivedJobs.forEach(id => {
      if (!archivedJobIds.has(id)) this._selectedArchivedJobs.delete(id);
    });

    const filtered = this.filterBar.getFilteredArchivedJobs();
    const isFiltered = this.filterBar.tokens("archived").length > 0;
    // And the ones the filter hides — see renderJobs. A row that matches but
    // is not drawn yet (below Show more) keeps its tick: Select All means
    // every matching row, and the batch bar counts the ones not shown.
    const visibleArchivedIds = new Set(filtered.map(j => j.id));
    this._selectedArchivedJobs.forEach(id => {
      if (!visibleArchivedIds.has(id)) this._selectedArchivedJobs.delete(id);
    });

    // Update filter count
    if (filterCount) {
      if (isFiltered) {
        filterCount.textContent = `${filtered.length} of ${this.archivedJobs.length}`;
        filterCount.style.display = "";
      } else {
        filterCount.style.display = "none";
      }
    }

    if (filtered.length === 0 && isFiltered) {
      container.innerHTML = "";
      this.focusedArchivedIndex = -1;
      table.style.display = "none";
      if (more) more.style.display = "none";
      emptyState.style.display = "flex";
      const icon = emptyState.querySelector("sl-icon");
      if (icon) icon.name = "search";
      const msg = emptyState.querySelector("p");
      if (msg) msg.textContent = "No matching archived jobs";
      const subtext = emptyState.querySelector(".empty-state-subtext");
      if (subtext) subtext.textContent = "Search matches titles, channel names and video IDs";
      if (filterCount) filterCount.style.display = "";
      this.updateBatchActionBar();
      return;
    }

    table.style.display = "";
    emptyState.style.display = "none";

    // Newest first, and only the first _archivedRenderLimit cards.
    const sorted = this._sortArchived(filtered);
    const drawn = sorted.slice(0, this._archivedRenderLimit);

    // Preserve the focused row across the rebuild, as renderJobs does.
    const focusedCard = container.querySelector(".video-item[data-focused]");
    const focusedJobId = focusedCard?.dataset.jobId;
    const hadDomFocus = !!focusedCard && document.activeElement === focusedCard;

    container.innerHTML = drawn
      .map((job) => this.renderJobItem(job))
      .join("");

    this.focusedArchivedIndex = focusedJobId ? drawn.findIndex(j => j.id === focusedJobId) : -1;

    // Re-apply the focus and selection marks to the drawn cards.
    const cards = container.querySelectorAll(".video-item");
    if (this.focusedArchivedIndex >= 0) cards[this.focusedArchivedIndex]?.setAttribute("data-focused", "");
    this._syncRowTabStop(container, hadDomFocus);
    cards.forEach((card) => {
      if (!this._selectedArchivedJobs.has(card.dataset.jobId)) return;
      card.classList.add("selected");
      const cb = card.querySelector(".job-checkbox");
      if (cb) cb.checked = true;
    });

    if (more) {
      const rest = sorted.length - drawn.length;
      more.style.display = rest > 0 ? "" : "none";
      const count = document.getElementById("archived-more-count");
      if (count) count.textContent = `Showing ${drawn.length} of ${sorted.length}`;
    }
    this.updateBatchActionBar();
  }

  /** Update local job resume position and refresh UI (called by player on silent saves). */
  _updateJobResumePosition(jobId, position) {
    const job = this.jobs.find(j => j.id === jobId) || this.archivedJobs.find(j => j.id === jobId);
    if (job) {
      job.resumePosition = position;
      // Update thumbnail indicator
      this.updateJobCard(job);
      // Update details pill if this job's details are open
      if (this.selectedJobId === jobId) {
        const pill = document.querySelector("#job-details-content .watch-pill");
        const newPill = this.watchPillHtml(job);
        if (pill && newPill) {
          pill.outerHTML = newPill;
        } else if (!pill && newPill) {
          // Insert pill after status badge inside the same value span
          const badge = document.querySelector("#job-details-content .status");
          badge?.insertAdjacentHTML("afterend", " " + newPill);
        }
      }
    }
  }

  /**
   * The small tags ahead of a job row's title: TW for a Twitch job, and CLIP
   * for a Twitch clip job (owner decision FE-28) — told by its ID
   * (isClipJob), the platform being "twitch" for every Twitch job. The TUI's
   * Tasks panel marks the same rows (internal/tui clipTag).
   */
  titleBadgesHtml(job) {
    let html = job.platform === "twitch"
      ? '<sl-tag size="small" variant="primary" style="margin-right:4px;font-size:0.7em">TW</sl-tag>'
      : '';
    if (isClipJob(job)) {
      html += '<sl-tag class="clip-tag" size="small" variant="neutral" style="margin-right:4px;font-size:0.7em">CLIP</sl-tag>';
    }
    return html;
  }

  /** Returns eyeball overlay HTML for a job thumbnail, or empty string. */
  watchIndicatorHtml(job) {
    if (job.status !== "Finished") return "";
    if (job.watched) {
      return `<span class="watch-indicator watched"><svg viewBox="0 0 24 24" fill="currentColor" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3" fill="rgba(0,0,0,0.4)"/></svg></span>`;
    }
    if (job.resumePosition != null) {
      return `<span class="watch-indicator in-progress"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg></span>`;
    }
    return "";
  }

  /** Returns a warning overlay for a Finished job missing tail segments (resumable), or empty string. */
  incompleteIndicatorHtml(job) {
    // incompleteTail can stay stale (true) in the DB through an active
    // resume/retry download until it recomputes at completion — gate on
    // Finished so the "missing tail" warning doesn't show on a job that's
    // already downloading again.
    if (job.status !== "Finished" || !job.incompleteTail) return "";
    return `<span class="incomplete-indicator" title="Recording is missing its tail — Resume to fetch it"><svg viewBox="0 0 16 16" fill="currentColor"><path d="M8.982 1.566a1.13 1.13 0 0 0-1.96 0L.165 13.233c-.457.778.091 1.767.98 1.767h13.713c.889 0 1.438-.99.98-1.767zM8 5c.535 0 .954.462.9.995l-.35 3.507a.552.552 0 0 1-1.1 0L7.1 5.995A.905.905 0 0 1 8 5m.002 6a1 1 0 1 1 0 2 1 1 0 0 1 0-2"/></svg></span>`;
  }

  /**
   * Where a card's thumbnail comes from: thumbnailUrl is what the <img>
   * loads, fallbackThumb what its error handler swaps in. A row with neither
   * draws no <img> at all, and updateJobCard cannot add one, so _cardShape
   * reads this too.
   */
  _thumbSources(job) {
    const isTwitch = job.platform === "twitch";
    const rawVideoId = job.videoId || job.id;
    // An import's placeholder id names no video, so it has no remote
    // thumbnail: the i.ytimg.com one built here, or stored on the row by an
    // import before the server stopped writing one, is a 404.
    const placeholder = isImportPlaceholderId(rawVideoId);
    const ytThumb = placeholder ? "" : `https://i.ytimg.com/vi/${encodeURIComponent(rawVideoId)}/mqdefault.jpg`;
    const twitchAvatarFallback = isTwitch && job.channelAvatarUrl ? job.channelAvatarUrl : "";
    // Prefer the locally-stored thumbnail (downloaded during mux). The
    // remote URL still feeds the data-fallback so the img onerror
    // handler can swap in i.ytimg.com / Twitch CDN if the local file is
    // missing for any reason (mux stage hadn't finished yet, file got
    // hand-deleted, etc.).
    const localThumb = job.thumbnailFile ? `/api/jobs/${encodeURIComponent(job.id)}/thumbnail` : "";
    const remoteThumb = (placeholder ? "" : job.thumbnailUrl) || (isTwitch ? twitchAvatarFallback : ytThumb);
    const thumbnailUrl = localThumb || remoteThumb;
    const fallbackThumb = localThumb ? remoteThumb : (isTwitch ? twitchAvatarFallback : ytThumb);
    return { localThumb, twitchAvatarFallback, thumbnailUrl, fallbackThumb };
  }

  renderJobItem(job) {
    const statusClass = job.status.toLowerCase().replace("?", "");
    const isTwitch = job.platform === "twitch";
    const { localThumb, twitchAvatarFallback, thumbnailUrl, fallbackThumb } = this._thumbSources(job);
    // The Twitch channel-avatar fallback is a square image; render it in
    // a 1:1 box (.thumb-avatar). All other paths -- local file, YouTube
    // CDN, Twitch live-preview -- are 16:9 and should fill the standard
    // 16:9 thumb box.
    const isAvatarThumb = isTwitch && !localThumb && (!job.thumbnailUrl || thumbnailUrl === twitchAvatarFallback);
    // Only mark the fallback as an avatar (triggering thumb-avatar on
    // load failure) when the FALLBACK itself is a Twitch channel avatar,
    // not when it's a CDN-served 16:9 thumbnail. Owner report: local
    // thumbnails were rendering as 1:1 boxes whenever the local route
    // was slow/transient-fail and the img onerror swapped to remote --
    // the handler used to add thumb-avatar unconditionally.
    const fallbackIsAvatar = isTwitch && fallbackThumb === twitchAvatarFallback;
    const progress = this.formatProgress(job);
    const progressHtml = this.formatProgressHtml(job);
    const percent = job.percent || 0;
    const platformBadge = this.titleBadgesHtml(job);

    // Quick action button based on status
    const canCancel = CANCEL_STATUSES.has(job.status);
    const canReinit = REINIT_STATUSES.has(job.status);
    const canDelete = DELETE_STATUSES.has(job.status);

    const escId = this.escapeHtml(job.id);
    let actionsHtml = "";
    const overflowItems = [];
    // Note: the overflow menu items use `value="..."` rather than
    // `data-quick-action` because both the row's click handler (which
    // matches `[data-quick-action]`) AND the sl-select handler would
    // otherwise fire on a menu-item tap and run the action twice.
    if (canCancel) {
      actionsHtml += `<sl-icon-button name="x-circle" label="Cancel" data-quick-action="cancel" data-job-id="${escId}"></sl-icon-button>`;
      overflowItems.push(`<sl-menu-item value="cancel" data-job-id="${escId}"><sl-icon slot="prefix" name="x-circle"></sl-icon>Cancel</sl-menu-item>`);
    }
    // The row's restart icon is Resume where the job can continue its
    // recording, and Reinitialize — which deletes it, and confirms first —
    // only where it cannot (owner decision PR-M1): an arrow read as "retry"
    // used to start a recoverable job over. Both stay in the overflow menu.
    // Resume only where there is staging to resume (owner decision PE-5): a
    // list row Resume can be offered on carries the server's hasStaging
    // (worker.ListJob, on every jobs frame and GET /api/jobs; ruling R-B2
    // narrowed it to those rows), so a resumable row with nothing
    // staged offers Reinitialize, where it offered a Resume the server
    // answered with "use Reinitialize". A row without the flag is not
    // offered Resume. Only the rows that offered Reinitialize change: a
    // Finished row with an incomplete tail keeps its Resume in the details
    // dialog, as before.
    const canResumeRow = canReinit && canResumeJob(job, { requireKnownStaging: true });
    if (canResumeRow) {
      actionsHtml += `<sl-icon-button name="play-fill" label="Resume" data-quick-action="resume" data-job-id="${escId}"></sl-icon-button>`;
      overflowItems.push(`<sl-menu-item value="resume" data-job-id="${escId}"><sl-icon slot="prefix" name="play-fill"></sl-icon>Resume</sl-menu-item>`);
    }
    if (canReinit) {
      if (!canResumeRow) {
        actionsHtml += `<sl-icon-button name="arrow-clockwise" label="Reinitialize" data-quick-action="reinitialize" data-job-id="${escId}"></sl-icon-button>`;
      }
      overflowItems.push(`<sl-menu-item value="reinitialize" data-job-id="${escId}"><sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>Reinitialize</sl-menu-item>`);
    }
    if (canDelete) {
      actionsHtml += `<sl-icon-button name="trash" label="Delete" data-quick-action="delete" data-job-id="${escId}"></sl-icon-button>`;
      overflowItems.push(`<sl-menu-item value="delete" data-job-id="${escId}"><sl-icon slot="prefix" name="trash"></sl-icon>Delete</sl-menu-item>`);
    }
    // Compact-row overflow menu. Only the desktop inline icons render on
    // wide viewports; CSS swaps them for this single trigger on phones.
    const overflowHtml = overflowItems.length
      ? `<sl-dropdown class="video-item-overflow" role="cell"><sl-icon-button slot="trigger" name="three-dots-vertical" label="More actions"></sl-icon-button><sl-menu>${overflowItems.join("")}</sl-menu></sl-dropdown>`
      : "";

    const isSelected = this._selectedTaskJobs.has(job.id) || this._selectedArchivedJobs.has(job.id);
    // role="row"/"cell": the containers are role="table" with a
    // columnheader row, and a table's rows must be rows — without them the
    // checkbox, status and buttons sat directly inside the rowgroup, which
    // assistive technology cannot read as a table.
    // tabindex="-1": a row takes focus (Arrow moves it there, Enter and
    // Space open its details); _syncRowTabStop puts one row of the list in
    // the Tab order.
    return `
      <div class="video-item${isSelected ? " selected" : ""}" role="row" tabindex="-1" data-job-id="${escId}" data-status="${this.escapeHtml(statusClass)}">
        <div class="thumb" role="cell">
          <input type="checkbox" class="job-checkbox" data-job-id="${escId}" ${isSelected ? "checked" : ""} aria-label="Select ${this.escapeHtml(job.title)}">
          ${(thumbnailUrl || fallbackThumb) ? `<img src="${this.escapeHtml(thumbnailUrl || fallbackThumb)}" alt="" loading="lazy" referrerpolicy="no-referrer"
               class="${isAvatarThumb ? "thumb-avatar" : ""}"
               ${fallbackThumb ? `data-fallback="${this.escapeHtml(fallbackThumb)}"` : ""}
               ${fallbackIsAvatar ? `data-fallback-avatar="true"` : ""}>` : ""}
          ${this.watchIndicatorHtml(job)}
          ${this.incompleteIndicatorHtml(job)}
        </div>
        <div class="stream-info" role="cell">
          <div class="stream-title" title="${this.escapeHtml(job.title)}">${platformBadge}${this.escapeHtml(job.title)}</div>
          <div class="stream-author">${this.escapeHtml(job.channelName)}</div>
        </div>
        <div class="job-status-cell" role="cell">
          <sl-badge class="status ${this.escapeHtml(statusClass)}" variant="primary">${this.escapeHtml(this.displayStatus(job.status))}</sl-badge>
        </div>
        <div class="job-progress" role="cell">
          <div class="job-progress-text" ${this.upcomingShowsLastCheck(job) ? `data-timestamp="${this.escapeHtml(job.lastRecheckAt)}" data-timestamp-prefix="Last check: "` : ""} title="${this.escapeHtml(this.formatProgressTooltip(job) || progress)}">${progressHtml}</div>
          ${percent > 0 ? `<sl-progress-bar class="job-progress-bar" value="${this.escapeHtml(percent)}"></sl-progress-bar>` : ""}
        </div>
        <div class="job-quick-actions" role="cell">${actionsHtml}</div>
        ${overflowHtml}
      </div>
    `;
  }

  // Update a single job card in the list without full re-render
  updateJobCard(job) {
    const card = document.querySelector(`.video-item[data-job-id="${CSS.escape(job.id)}"]`);
    if (!card) return;

    // Update thumbnail (can change for Twitch: avatar → live thumbnail,
    // or null → local file once mux finishes and writes thumbnail_file).
    // Prefer the local thumbnail route when the job has a thumbnailFile;
    // fall back to the remote URL.
    const thumbImg = card.querySelector(".thumb img");
    if (thumbImg) {
      // When a thumbnailFile first appears server-side (fetched after job start,
      // or produced during muxing), clear a stale local-URL failure so the
      // freshly-written file gets one fresh attempt instead of staying pinned
      // to the fallback. The transition fires once, so a still-404ing file
      // re-blocks without reopening the original flicker loop.
      const hasThumbFile = job.thumbnailFile ? "1" : "0";
      if (hasThumbFile === "1" && thumbImg.dataset.hadThumbFile !== "1") {
        delete thumbImg.dataset.localFailed;
      }
      thumbImg.dataset.hadThumbFile = hasThumbFile;

      const desiredSrc = job.thumbnailFile
        ? `/api/jobs/${encodeURIComponent(job.id)}/thumbnail`
        : (job.thumbnailUrl || "");
      // Skip re-assigning a src that already errored (handleThumbError
      // swapped to the fallback) — re-setting it would just 404 again.
      if (desiredSrc && !thumbImg.src.endsWith(desiredSrc) && thumbImg.dataset.localFailed !== desiredSrc) {
        thumbImg.src = desiredSrc;
        thumbImg.classList.remove("thumb-avatar");
        // Reset the data-fallback so an error on the new src can fall
        // through to the remote URL, then static thumbnail.
        if (job.thumbnailFile && job.thumbnailUrl) {
          thumbImg.dataset.fallback = job.thumbnailUrl;
        }
      }
    }

    // Update watch indicator
    const existingIndicator = card.querySelector(".thumb .watch-indicator");
    const newIndicatorHtml = this.watchIndicatorHtml(job);
    if (newIndicatorHtml) {
      if (existingIndicator) {
        existingIndicator.outerHTML = newIndicatorHtml;
      } else {
        card.querySelector(".thumb")?.insertAdjacentHTML("beforeend", newIndicatorHtml);
      }
    } else if (existingIndicator) {
      existingIndicator.remove();
    }

    // Update title only when changed (avoids recreating sl-tag shadow DOM for TW badge)
    const titleEl = card.querySelector(".stream-title");
    if (titleEl && titleEl.title !== job.title) {
      titleEl.innerHTML = this.titleBadgesHtml(job) + this.escapeHtml(job.title);
      titleEl.title = job.title;
    }
    const authorEl = card.querySelector(".stream-author");
    if (authorEl && authorEl.textContent !== job.channelName) {
      authorEl.textContent = job.channelName;
    }

    // Status badge and data-status are NOT updated here — updateJobCard is
    // only called when status hasn't changed, so these are already correct.
    // A status change draws the card again (_moveJobCard, or renderJobs).

    // Update progress text
    const progressText = card.querySelector(".job-progress-text");
    if (progressText) {
      // ~60 Hz per active job, and this is the highest-frequency DOM write in
      // the app: the cell's text node was destroyed and recreated on every
      // tick even when the string had not moved (measured 60/60). Stash the
      // rendered markup on the element and write only when it differs — the
      // same diff updateJobDetails uses. The title is diffed separately
      // because the tooltip can move while the visible string does not.
      const progressHtml = this.formatProgressHtml(job);
      if (progressText.dataset.progress !== progressHtml) {
        progressText.innerHTML = progressHtml;
        progressText.dataset.progress = progressHtml;
      }
      const tooltip = this.formatProgressTooltip(job) || this.formatProgress(job);
      if (progressText.title !== tooltip) {
        progressText.title = tooltip;
      }
      if (this.upcomingShowsLastCheck(job)) {
        progressText.dataset.timestamp = job.lastRecheckAt;
        progressText.dataset.timestampPrefix = "Last check: ";
      } else {
        delete progressText.dataset.timestamp;
        delete progressText.dataset.timestampPrefix;
      }
    }

    // Update progress bar
    const progressContainer = card.querySelector(".job-progress");
    const progressBar = card.querySelector(".job-progress-bar");
    const percent = job.percent || 0;

    if (percent > 0) {
      if (progressBar) {
        progressBar.value = percent;
      } else if (progressContainer) {
        const newBar = document.createElement("sl-progress-bar");
        newBar.className = "job-progress-bar";
        newBar.value = percent;
        progressContainer.appendChild(newBar);
      }
    } else if (progressBar) {
      progressBar.remove();
    }

    // Quick actions are NOT rebuilt here — updateJobCard is only called when
    // status and _cardShape haven't changed (a change to either draws the
    // card again: _moveJobCard, or the full renderJobs), so the available
    // actions are identical. Skipping avoids destroying and
    // recreating sl-icon-button shadow DOM elements on every progress update.
  }

  /**
   * upcomingShowsLastCheck: an Upcoming row's progress cell is the
   * "Last check: <relative>" the timestamp refresher rewrites only when no
   * wait line is up and the row is a broadcast being waited for (owner
   * ruling R-D1). The card template and updateJobCard stamp data-timestamp
   * on exactly these rows, so the refresher never writes over a wait line.
   */
  upcomingShowsLastCheck(job) {
    return job.status === "Upcoming" && !!job.lastRecheckAt && !isWaitLine(job.progress) && !isAwaitingProcessing(job);
  }

  formatProgress(job) {
    if (job.status === "Upcoming") {
      // A wait the worker is in — a download slot, a rate limit or captcha,
      // the network — names itself in job.progress (owner ruling R-D1), and
      // used to be hidden behind "Last check". Twitch's "Waiting for
      // stream..." carries no wait marker and keeps the last-check time.
      if (isWaitLine(job.progress)) return job.progress;
      if (isAwaitingProcessing(job)) return "Awaiting processing";
      if (job.lastRecheckAt) {
        return `Last check: ${this.formatRelativeTime(job.lastRecheckAt)}`;
      }
      return "Waiting for stream...";
    }

    if (job.status === "Queued") {
      // Why the scheduler is not admitting it — offline, the disk gate, the
      // YouTube rate limit, its retry backoff — or "Waiting in queue..."
      // (owner ruling R-D3; the TUI's details pane says the same).
      return queuedHoldText(queuedHold(this.admissionHolds, job));
    }

    if (job.status === "Live" || job.status === "Downloading") {
      if (job.progress) return job.progress;
      if (job.lastVideoSeq !== undefined) {
        return `V: ${job.lastVideoSeq || 0} / A: ${job.lastAudioSeq || 0}`;
      }
    }

    if (job.status === "Muxing") {
      return job.progress || "Muxing...";
    }

    if (job.status === "Finished") {
      return job.filename ? "Complete" : "Finished";
    }

    if (job.status === "Error") {
      return job.error || "Error";
    }

    if (job.status === "COOKIES?") {
      // A membership park is a DIFFERENT remedy from a dead-cookie park:
      // refreshing the current account cannot add a channel membership to it,
      // and these jobs deliberately never resume on their own until a
      // different account is supplied. Saying "needs cookie refresh" sent an
      // operator round a loop that could never have helped.
      if (job.parkReason === "membership") {
        return "Not a member — needs member account";
      }
      return "Needs cookie refresh";
    }

    return job.progress || "-";
  }

  // Returns progress as safe HTML — identical to formatProgress for most statuses,
  // but wraps error text in an expandable span for the Error status.
  formatProgressHtml(job) {
    if (job.status === "Error") {
      const errorText = job.error || "Error";
      const truncated = errorText.length > 50 ? errorText.substring(0, 50) + "…" : errorText;
      // An Error row the Twitch monitor will mux or retry by itself says so
      // ahead of its error (owner ruling R-D2; the TUI's "⟳ mux" tag).
      const action = pendingAutoAction(job);
      const auto = action
        ? `<span class="job-auto-action" title="${this.escapeHtml(pendingAutoActionText(action))}">${action === "auto_mux" ? "⟳ Auto-mux pending" : "⟳ Auto-retry pending"}</span> `
        : "";
      return `${auto}<span class="job-error-text" title="${this.escapeHtml(errorText)}" data-full="${this.escapeHtml(errorText)}" data-short="${this.escapeHtml(truncated)}">${this.escapeHtml(truncated)}</span>`;
    }
    return this.escapeHtml(this.formatProgress(job));
  }

  // ===== Job Actions =====

  async addVideo() {
    const input = document.getElementById("video-url-input");
    const submitBtn = document.getElementById("add-submit-btn");
    if (submitBtn.disabled) return; // Prevent double-submit (Enter key bypasses disabled button)
    const value = input.value.trim();

    if (!value) return;

    // Try unified media target extraction
    const target = this.extractMediaTarget(value);
    if (!target) {
      this.showToast("Invalid YouTube/Twitch URL or ID", "warning");
      return;
    }

    // Show loading state and prevent double-submit
    submitBtn.loading = true;
    submitBtn.disabled = true;

    try {
      let body;

      if (target.platform === "twitch") {
        // A clip list is refused client-side until its job exists: the message
        // is utils.TwitchTarget.Unsupported's, which the CLI and the TUI show.
        // A clip is a job of its own (owner decision D26), posted by its slug
        // with twitchType "clip".
        if (target.type === "clips") {
          this.showToast("Twitch clip lists are not supported yet. Use a channel, VOD, clip, video list or collection URL.", "warning");
          return;
        }
        if (target.type === "videos" || target.type === "collection") {
          // A channel's video list or a collection (owner decision D18): the
          // server reads the listing — the URL as pasted, so its ?filter=
          // and ?sort= reach it — and queues one VOD job per video.
          await this.addTwitchListing(value);
          return;
        }
        // Twitch job
        body = {
          platform: "twitch",
          videoId: target.id,
          twitchType: target.type,
        };
      } else {
        // YouTube job
        body = { videoId: target.videoId };
        const advancedToggle = document.getElementById("advanced-options-toggle");

        if (advancedToggle.checked) {
          const videoSelect = document.getElementById("video-format-select");
          const audioSelect = document.getElementById("audio-format-select");
          const startInput = document.getElementById("start-time-input");
          const endInput = document.getElementById("end-time-input");

          // A rendition is sent only for the video it was listed for: the
          // selects can still hold an earlier URL's choice while the new
          // URL's formats are being fetched, or after that fetch failed.
          if (this._formatsListedFor === target.videoId) {
            if (videoSelect.value) {
              body.selectedVideoItag = parseInt(videoSelect.value, 10);
            }
            if (audioSelect.value) {
              body.selectedAudioItag = parseInt(audioSelect.value, 10);
            }
          }

          const startTime = this.parseTimeInput(startInput.value);
          if (startTime !== null && startTime > 0) {
            body.startTime = startTime;
          }
          const endTime = this.parseTimeInput(endInput.value);
          if (endTime !== null && endTime > 0) {
            body.endTime = endTime;
          }

          // Validate time range if both are provided
          if (body.startTime != null && body.endTime != null && body.endTime <= body.startTime) {
            this.showToast("End time must be after start time", "warning");
            submitBtn.loading = false;
            submitBtn.disabled = false;
            return;
          }
        }
      }

      const response = await fetch("/api/jobs", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });

      if (response.ok) {
        document.getElementById("add-dialog").hide();
        this.showToast("Video added successfully", "success");
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.showToast(data.error || "Failed to add video", "danger");
      }
    } catch (e) {
      this.showToast("Failed to add video: " + e.message, "danger");
    } finally {
      submitBtn.loading = false;
      submitBtn.disabled = false;
    }
  }

  /**
   * Queue a Twitch video list or collection (POST /api/jobs, twitchType
   * "listing"): the server answers with how many VOD jobs it queued and
   * skipped. The dialog closes once anything was listed.
   */
  async addTwitchListing(url) {
    const response = await fetch("/api/jobs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ platform: "twitch", twitchType: "listing", url }),
    });
    const data = await response.json().catch(() => ({ error: response.statusText }));
    if (!response.ok) {
      this.showToast(data.error || "Failed to add the Twitch listing", "danger");
      return;
    }
    document.getElementById("add-dialog").hide();
    const skipped = data.skipped ? `, ${data.skipped} already queued` : "";
    // The VODs the server could not queue (owner decision PR-M9): the toast
    // was a green "Queued N" while they were dropped, each named only in the
    // log (QueueTwitchVideos). It warns now, and says how many.
    const failed = data.failed ? `, ${data.failed} could not be queued` : "";
    this.showToast(`Queued ${data.added} Twitch VOD${data.added === 1 ? "" : "s"}${skipped}${failed}`, data.failed ? "warning" : "success");
  }

  /**
   * Parse time input string to seconds.
   * Supports HH:MM:SS, MM:SS, or raw seconds.
   * Returns null for invalid/empty input.
   */
  parseTimeInput(input) {
    const trimmed = (input || "").trim();
    if (!trimmed) return null;

    if (trimmed.includes(":")) {
      const parts = trimmed.split(":");
      if (parts.length < 2 || parts.length > 3) return null;
      const nums = parts.map(Number);
      if (nums.some(n => isNaN(n) || n < 0)) return null;
      if (parts.length === 3) return nums[0] * 3600 + nums[1] * 60 + nums[2];
      return nums[0] * 60 + nums[1];
    }

    const seconds = Number(trimmed);
    return isNaN(seconds) || seconds < 0 ? null : seconds;
  }

  /**
   * Reset advanced options panel to default state
   */
  resetAdvancedOptions() {
    // Cancel any pending debounced format fetch; otherwise it could fire
    // after reset and re-disable the advanced section against a stale
    // _lastFormatVideoId.
    if (this._formatFetchTimeout) {
      clearTimeout(this._formatFetchTimeout);
      this._formatFetchTimeout = null;
    }

    const toggle = document.getElementById("advanced-options-toggle");
    toggle.checked = false;
    document.getElementById("advanced-options-panel").style.display = "none";
    document.getElementById("format-selection").style.display = "none";
    document.getElementById("format-skeleton").style.display = "none";

    this.clearFormatChoice();

    document.getElementById("start-time-input").value = "";
    document.getElementById("end-time-input").value = "";

    // Reset cached video ID
    this._lastFormatVideoId = null;
  }

  /**
   * Forget the listed formats and whatever was picked from them: both selects
   * back on Auto with no renditions, the section hidden, and no video named
   * as the one they were listed for (_formatsListedFor, which addVideo checks
   * before it sends an itag). Run whenever the URL stops being that video's.
   */
  clearFormatChoice() {
    this._formatsListedFor = null;
    document.getElementById("format-selection").style.display = "none";
    for (const id of ["video-format-select", "audio-format-select"]) {
      const select = document.getElementById(id);
      select.value = "";
      // Remove dynamic options (keep the "None" option)
      select.querySelectorAll("sl-option:not([value='-1'])").forEach((el) => el.remove());
    }
  }

  /**
   * Fetch available formats for the current video URL
   */
  async fetchFormatsForAdvanced() {
    const input = document.getElementById("video-url-input");
    const value = input.value.trim();
    if (!value) return;

    const videoId = this.extractVideoId(value);
    if (!videoId) {
      // Non-YouTube URL (e.g. Twitch) — hide stale format options
      document.getElementById("format-skeleton").style.display = "none";
      this.clearFormatChoice();
      this._lastFormatVideoId = null;
      return;
    }

    // Don't re-fetch if same video
    if (this._lastFormatVideoId === videoId) return;
    this._lastFormatVideoId = videoId;
    // Another video: the earlier one's renditions and choice go now, not
    // when (and if) this one's list arrives.
    this.clearFormatChoice();

    const formatSkeleton = document.getElementById("format-skeleton");
    const formatSection = document.getElementById("format-selection");
    formatSkeleton.style.display = "block";
    formatSection.style.display = "none";

    try {
      const response = await fetch(`/api/formats/${videoId}`);

      // Discard a stale response, failed or not, if the user changed the URL
      // during the fetch: the newer fetch owns the skeleton and the toast.
      if (this._lastFormatVideoId !== videoId) return;

      if (!response.ok) {
        throw new Error(await serverErrorMessage(response));
      }

      const data = await response.json();
      if (this._lastFormatVideoId !== videoId) return;
      await this.populateFormatSelects(data);
      // The URL may have changed while the selects were filled: the list is
      // then nobody's, and whoever disowned it has hidden the section.
      if (this._lastFormatVideoId !== videoId) {
        if (!this._formatsListedFor) this.clearFormatChoice();
        return;
      }
      this._formatsListedFor = videoId;

      formatSkeleton.style.display = "none";
      formatSection.style.display = "block";
    } catch (e) {
      // A newer fetch owns the selects and the skeleton now (populate can
      // throw after the URL changed again).
      if (this._lastFormatVideoId !== videoId) return;
      // Clear cache so the user can retry (toggling advanced off/on)
      this._lastFormatVideoId = null;
      formatSkeleton.style.display = "none";
      // Keep format section hidden on error — empty selects are confusing
      this.clearFormatChoice();
      console.error("Failed to fetch formats:", e);
      this.showToast("Could not load format options: " + e.message, "warning");
    }
  }

  /**
   * Populate video and audio format select dropdowns
   */
  async populateFormatSelects(data) {
    const videoSelect = document.getElementById("video-format-select");
    const audioSelect = document.getElementById("audio-format-select");

    // Clear existing dynamic options
    videoSelect.querySelectorAll("sl-option:not([value='-1'])").forEach((el) => el.remove());
    audioSelect.querySelectorAll("sl-option:not([value='-1'])").forEach((el) => el.remove());

    const bestItags = data.bestItags || {};
    // The server's marks (cmd/moombox/adapters.go, pickerFormat), noted after
    // the Best badges as yt-dlp notes a damaged rendition "DAMAGED" in its
    // format list: damaged — encoded short of half the video's length, so its
    // track would stop partway; segmented — served only as fragments, which a
    // whole-file VOD download cannot fetch. Either can still be picked by hand.
    // Segmented is noted only for a VOD or a finished stream (the owner's
    // decision): while the stream is live or upcoming every adaptive
    // rendition it lists is segmented, and the live path fetches exactly
    // those, so the mark on every row said nothing. The server's badges
    // still pass a segmented rendition over. The TUI's Add Video reads
    // the same streamStatus the same way (notesSegmented,
    // internal/tui/add_video.go).
    const onAir = data.streamStatus === "live" || data.streamStatus === "upcoming";
    const marks = (fmt) => (fmt.damaged ? " [damaged]" : "") + (fmt.segmented && !onAir ? " [segmented]" : "");

    // Add video formats
    for (const fmt of data.videoFormats || []) {
      const opt = document.createElement("sl-option");
      opt.value = String(fmt.itag);

      const codec = (fmt.mimeType || "").split(";")[0]?.split("/")[1] || "";
      const container = codec.includes("webm") || (fmt.mimeType || "").includes("webm") ? "WEBM" : "MP4";
      const res = fmt.height ? `${fmt.width || "?"}x${fmt.height}` : "?";
      const fps = fmt.fps ? `@${fmt.fps}fps` : "";
      const bitrate = fmt.bitrate ? ` ${(fmt.bitrate / 1000).toFixed(0)}kbps` : "";

      // [Best] is the rendition automatic selection takes, of either
      // container and under the resolution cap (bestItags.bestVideo, owner
      // decision PE-1), as the audio list's [Best] is; the container badges
      // stay beside it.
      let badges = "";
      if (bestItags.bestVideo != null && fmt.itag === bestItags.bestVideo) badges += " [Best]";
      if (fmt.itag === bestItags.bestWebmVideo) badges += " [Best WEBM]";
      if (fmt.itag === bestItags.bestMp4Video) badges += " [Best MP4]";
      badges += marks(fmt);

      opt.textContent = `${res}${fps} ${container}${bitrate}${badges}`;
      videoSelect.appendChild(opt);
    }

    // Add audio formats
    for (const fmt of data.audioFormats || []) {
      const opt = document.createElement("sl-option");
      opt.value = String(fmt.itag);

      // The 5.1 codecs the shared rank can put first are named for what they
      // are: an E-AC-3 or AC-3 track is audio/mp4, and the container test
      // below called it AAC. The TUI names them the same (audioCodecLabel).
      const mime = fmt.mimeType || "";
      const lower = mime.toLowerCase();
      let codec = "";
      if (lower.includes("ec-3") || lower.includes("eac3")) codec = "EC-3";
      else if (lower.includes("ac-3")) codec = "AC-3";
      else if (mime.includes("opus") || mime.includes("webm")) codec = "OPUS";
      else if (mime.includes("mp4a") || mime.includes("mp4")) codec = "AAC";
      else codec = mime.split(";")[0]?.split("/")[1] || "?";

      const bitrate = fmt.bitrate ? `${(fmt.bitrate / 1000).toFixed(0)}kbps` : "?";
      const sampleRate = fmt.audioSampleRate ? ` ${fmt.audioSampleRate}Hz` : "";

      // [Best] is the rendition automatic selection takes, of any codec
      // (bestItags.bestAudio, owner decision FE-10): a 5.1 E-AC-3 track the
      // shared rank puts above both codec picks carried no badge at all. The
      // codec badges stay beside it.
      let badges = "";
      if (bestItags.bestAudio != null && fmt.itag === bestItags.bestAudio) badges += " [Best]";
      if (fmt.itag === bestItags.bestOpusAudio) badges += " [Best OPUS]";
      if (fmt.itag === bestItags.bestAacAudio) badges += " [Best AAC]";
      badges += marks(fmt);

      opt.textContent = `${bitrate}${sampleRate} ${codec}${badges}`;
      audioSelect.appendChild(opt);
    }

    // Both selects start on Auto (owner decision PR-R8, superseding FQ-1's
    // preselection): nothing is pinned unless the operator picks a
    // rendition. The [Best] badges above show what Auto takes. A preselected
    // video itag turned an untouched Advanced form into a pinned job — and a
    // pinned video itag turns a live job's quality monitoring off — for a
    // rendition the selector itself might not have picked. A select left on
    // a previous video's rendition is cleared too: its itag is not this
    // video's choice.
    videoSelect.value = "";
    audioSelect.value = "";

    // Update end time placeholder with video duration
    if (data.lengthSeconds && data.lengthSeconds > 0) {
      const h = Math.floor(data.lengthSeconds / 3600);
      const m = Math.floor((data.lengthSeconds % 3600) / 60);
      const s = data.lengthSeconds % 60;
      const durationStr = h > 0
        ? `${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`
        : `${m}:${String(s).padStart(2, "0")}`;
      document.getElementById("end-time-input").placeholder = durationStr;
    }

    // Show duration display
    const durationEl = document.getElementById("video-duration-display");
    if (durationEl) {
      if (data.lengthSeconds && data.lengthSeconds > 0) {
        durationEl.textContent = `Duration: ${this.formatDurationSeconds(data.lengthSeconds)}`;
        durationEl.style.display = "block";
      } else {
        durationEl.style.display = "none";
      }
    }
  }

  openJobUrl() {
    if (!this.selectedJobId) return;
    const job = this.jobs.find((j) => j.id === this.selectedJobId)
      || this.archivedJobs.find((j) => j.id === this.selectedJobId);
    if (!job) return;
    // streamUrl, the details' Stream URL row's and the TUI's rule: a Twitch
    // live row with no url has no page, where this opened
    // twitch.tv/<channelName> — for an imported capture "Import", someone
    // else's channel.
    const url = streamUrl(job);
    if (!url) return;
    try {
      const parsed = new URL(url);
      if (parsed.protocol === "https:" || parsed.protocol === "http:") {
        window.open(url, "_blank", "noopener");
      }
    } catch {
      // Invalid URL — ignore
    }
  }

  async openJobFolder() {
    if (!this.selectedJobId) return;
    try {
      const response = await fetch(`/api/jobs/${this.selectedJobId}/open-folder`, {
        method: "POST",
      });
      // The answer was thrown away, so every refusal was a button that did
      // nothing and said nothing — including the Linux case, where the host
      // has no file manager the route can spawn and says exactly that (WEB-6).
      if (!response.ok) {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.showToast(data?.error || "Failed to open folder", "danger");
      }
    } catch (e) {
      console.error("Failed to open folder:", e);
      this.showToast("Failed to open folder: " + e.message, "danger");
    }
  }

  /**
   * Open the selected job in the Player tab. Returns a promise that resolves
   * once the job is loaded and the keyboard has been handed over, so a caller
   * can await the player being ready; it never rejects (a failed load is
   * swallowed below, exactly as the fire-and-forget click path needs).
   * @returns {Promise<void>}
   */
  openInPlayer() {
    if (!this.selectedJobId) return Promise.resolve();
    // Play is a switch to the Player tab like any other, so it owes the
    // unsaved-settings guard the answer a click on that tab would get — the
    // details dialog can sit over a dirty Settings page through a #job= deep
    // link. Asked BEFORE the dialog is closed, so "stay" leaves the operator
    // exactly where they were; re-entered on "leave", by which point the
    // guard has reset the dirty state and lets the switch below through.
    if (!this._canLeaveSettings("player")) {
      return this._confirmLeaveSettings().then(confirmed => (confirmed ? this.openInPlayer() : undefined));
    }
    const jobId = this.selectedJobId;

    // Close the details dialog
    document.getElementById("details-dialog").hide();

    // Flag prevents the sl-tab-show handler from calling loadPlayerJobList
    // (which would race with our call and potentially overwrite the selection).
    // Cleared after our own loadPlayerJobList finishes, not synchronously,
    // because Shoelace may fire sl-tab-show asynchronously after show().
    this._playerOpeningFromDetails = true;

    // Switch to the Player tab
    const tabGroup = document.querySelector("sl-tab-group");
    tabGroup.show("player");

    // Initialize player if needed, then select the job
    if (!this.player.playerInitialized) {
      this.player.initPlayer();
    }

    return this.player.loadPlayerJobList().then(() => {
      const select = document.getElementById("player-job-select");
      select.value = jobId;
      // Returned so the .catch below covers the selection as well, and so the
      // caller can await the whole hand-off. The focus move is the same one
      // the picker's own sl-change handler makes, in a `finally` for the same
      // reason: a load that throws must not leave focus on the select.
      return this.player.onPlayerJobSelect(jobId).finally(focusPlayerSurface);
    }).catch(() => {}).finally(() => {
      this._playerOpeningFromDetails = false;
    });
  }

  async cancelJob(jobId) {
    const id = jobId || this.selectedJobId;
    if (!id || this._jobActionsInFlight.has(id)) return;
    if (!await this.showConfirm("Are you sure you want to cancel this job?", { okLabel: "Cancel Job", okVariant: "danger" })) return;
    // Re-read after the confirm (see batchAction): a row this tab holds that
    // left the cancellable states meanwhile is not cancelled.
    const nowCancel = this._currentJob(id);
    if (nowCancel && !CANCEL_STATUSES.has(nowCancel.status)) {
      this.showToast(`Not cancelled — the job is ${nowCancel.status} now`, "warning");
      return;
    }

    this._jobActionsInFlight.add(id);
    try {
      const response = await fetch(`/api/jobs/${id}/cancel`, {
        method: "POST",
      });
      if (response.ok) {
        this.showToast("Job cancelled", "success");
        // Update archived job status if present (no WebSocket update for archived jobs)
        const archivedJob = this.archivedJobs.find(j => j.id === id);
        if (archivedJob) {
          archivedJob.status = "Cancelled";
          this.renderArchivedJobs();
          // Update the open details dialog so buttons/status reflect the new state
          if (this.selectedJobId === id) {
            this.details.updateJobDetails(archivedJob);
          }
        }
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.showToast(data.error || "Failed to cancel job", "danger");
      }
    } catch (e) {
      this.showToast("Failed to cancel job: " + e.message, "danger");
    } finally {
      this._jobActionsInFlight.delete(id);
    }
  }

  async resumeJob(jobId) {
    const id = jobId || this.selectedJobId;
    if (!id || this._jobActionsInFlight.has(id)) return;

    this._jobActionsInFlight.add(id);
    try {
      const response = await fetch(`/api/jobs/${id}/resume`, {
        method: "POST",
      });
      if (response.ok) {
        this.showToast("Job resumed", "success");
        if (this.selectedJobId === id) {
          const dlg = document.getElementById("details-dialog");
          if (dlg?.open) dlg.hide();
          this.selectedJobId = null;
        }
        this._removeArchivedJob(id);
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.showToast(data.error || "Failed to resume job", "danger");
      }
    } catch (e) {
      this.showToast("Failed to resume job: " + e.message, "danger");
    } finally {
      this._jobActionsInFlight.delete(id);
    }
  }

  /**
   * reinitializeDeletes asks the server what reinitializing ids would delete
   * (GET /api/jobs/{id}/reinitialize/preview, owner decision PR-M1) and
   * answers the confirm's clause for their totals as text — null when a
   * preview could not be read, and the confirm then says so rather than
   * nothing — and refused, the ids whose preview says the server would not
   * reinitialize them now (allowed false: the POST's own status gate, which
   * a row the WebSocket has not caught up with may still look past).
   */
  async reinitializeDeletes(ids) {
    try {
      const previews = await Promise.all(ids.map(async (id) => {
        const res = await fetch(`/api/jobs/${encodeURIComponent(id)}/reinitialize/preview`);
        if (!res.ok) throw new Error(res.statusText);
        return res.json();
      }));
      return {
        text: reinitializeDeletesText(sumReinitializePreviews(previews)),
        refused: ids.filter((_, i) => previews[i]?.allowed === false),
      };
    } catch {
      return { text: null, refused: [] };
    }
  }

  async reinitializeJob(jobId) {
    const id = jobId || this.selectedJobId;
    if (!id || this._jobActionsInFlight.has(id)) return;

    // Reinitialize starts over and deletes what the job recorded: the
    // set-asides, the recorded parts, the staging and an imported chapter
    // list. The row icon and the details button both ask first, naming
    // those from the server's preflight (owner decision PR-M1).
    const { text: deletes, refused } = await this.reinitializeDeletes([id]);
    if (refused.length > 0) {
      // The server's gate already says no: no confirm for a reset the POST
      // would refuse.
      const status = this._currentJob(id)?.status;
      this.showToast(`Not reinitialized — ${status ? `the job is ${status} now` : "the job can no longer be reinitialized"}`, "warning");
      return;
    }
    const title = this._currentJob(id)?.title;
    const what = deletes
      ? `This ${deletes}.`
      : "What it would delete could not be read: it deletes the job's set-aside recordings, recorded parts, staged data and imported chapters.";
    const msg = `Reinitialize ${title ? `"${title}"` : "this job"} and download it again from the start? ${what} This cannot be undone.`;
    if (!await this.showConfirm(msg, { title: "Reinitialize", okLabel: "Reinitialize", okVariant: "danger" })) return;
    // Re-read after the confirm, as deleteJob does: a job a monitor retried
    // while the dialog was open is not reset under its run.
    const now = this._currentJob(id);
    if (now && !REINIT_STATUSES.has(now.status)) {
      this.showToast(`Not reinitialized — the job is ${now.status} now`, "warning");
      return;
    }
    if (this._jobActionsInFlight.has(id)) return;

    this._jobActionsInFlight.add(id);
    try {
      const response = await fetch(`/api/jobs/${id}/reinitialize`, {
        method: "POST",
      });
      if (response.ok) {
        this.showToast("Job reinitialized", "success");
        if (this.selectedJobId === id) {
          const dlg = document.getElementById("details-dialog");
          if (dlg?.open) dlg.hide();
          this.selectedJobId = null;
        }
        this._removeArchivedJob(id);
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.showToast(data.error || "Failed to reinitialize job", "danger");
      }
    } catch (e) {
      this.showToast("Failed to reinitialize job: " + e.message, "danger");
    } finally {
      this._jobActionsInFlight.delete(id);
    }
  }

  async muxJob(jobId) {
    const id = jobId || this.selectedJobId;
    if (!id || this._jobActionsInFlight.has(id)) return;

    this._jobActionsInFlight.add(id);
    try {
      const response = await fetch(`/api/jobs/${id}/mux`, {
        method: "POST",
      });
      if (response.ok) {
        this.showToast("Muxing started", "success");
        // Do NOT close dialog — job stays visible during mux
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.showToast(data.error || "Failed to mux job", "danger");
      }
    } catch (e) {
      this.showToast("Failed to mux job: " + e.message, "danger");
    } finally {
      this._jobActionsInFlight.delete(id);
    }
  }

  /**
   * Start set-aside recovery for a job. Returns true when the server accepted
   * it. Its own verb, not a Mux: /mux means "mux the recording" and refuses a
   * staging directory that holds nothing but preserved footage.
   */
  async recoverAsides(jobId) {
    const id = jobId || this.selectedJobId;
    if (!id || this._jobActionsInFlight.has(id)) return false;

    this._jobActionsInFlight.add(id);
    try {
      const response = await fetch(`/api/jobs/${id}/recover-asides`, {
        method: "POST",
      });
      if (response.ok) {
        this.showToast("Recovery started", "success");
        return true;
      }
      const data = await response.json().catch(() => ({ error: response.statusText }));
      this.showToast(data.error || "Failed to recover set-aside recordings", "danger");
      return false;
    } catch (e) {
      this.showToast("Failed to recover set-aside recordings: " + e.message, "danger");
      return false;
    } finally {
      this._jobActionsInFlight.delete(id);
    }
  }

  async deleteJob(jobId) {
    const id = jobId || this.selectedJobId;
    if (!id || this._jobActionsInFlight.has(id)) return;
    if (!await this.showConfirm("Are you sure you want to delete this job?", { okLabel: "Delete", okVariant: "danger" })) return;
    // Re-read after the confirm (see batchAction): the DELETE route cancels a
    // running download before removing it, and a job retried while the dialog
    // was open must not be pulled out from under its worker.
    const nowDelete = this._currentJob(id);
    if (nowDelete && !DELETE_STATUSES.has(nowDelete.status)) {
      this.showToast(`Not deleted — the job is ${nowDelete.status} now`, "warning");
      return;
    }

    this._jobActionsInFlight.add(id);
    try {
      const response = await fetch(`/api/jobs/${id}`, {
        method: "DELETE",
      });
      if (response.ok) {
        if (this.selectedJobId === id) {
          const dlg = document.getElementById("details-dialog");
          if (dlg?.open) dlg.hide();
          this.selectedJobId = null;
        }
        this.showToast("Job deleted", "success");
        // Optimistically remove from active jobs so the card disappears
        // immediately (instead of waiting for the next WebSocket update)
        const activeIdx = this.jobs.findIndex(j => j.id === id);
        if (activeIdx !== -1) {
          this.jobs.splice(activeIdx, 1);
          if (!this._removeJobCard(id)) this.renderJobs();
        }
        // Remove from archived jobs if present (no WebSocket update for archived jobs)
        this._removeArchivedJob(id);
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.showToast(data.error || "Failed to delete job", "danger");
      }
    } catch (e) {
      this.showToast("Failed to delete job: " + e.message, "danger");
    } finally {
      this._jobActionsInFlight.delete(id);
    }
  }

  // ===== Trim Management =====

  async createTrim(jobId, startTime, endTime) {
    try {
      const response = await fetch(`/api/jobs/${jobId}/trims`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ startTime, endTime }),
      });

      if (!response.ok) {
        const error = await response.json().catch(() => ({ error: response.statusText }));
        throw new Error(error.error || 'Failed to create trim');
      }

      // 202: the trim runs on the server, detached from this request — a
      // reload or a closed tab no longer stops it. Its result arrives as
      // trim_status frames (handleTrimStatus).
      const { trim } = await response.json();
      this.showToast('Trim started — it keeps running if you leave this page', 'primary');
      return trim;
    } catch (error) {
      this.showToast(error.message, 'danger');
      throw error;
    }
  }

  async deleteTrim(jobId, trimId) {
    if (!await this.showConfirm("Delete this trim? This cannot be undone.", { okLabel: "Delete", okVariant: "danger" })) {
      return;
    }

    try {
      const response = await fetch(`/api/jobs/${jobId}/trims/${trimId}`, {
        method: 'DELETE',
      });

      if (!response.ok) {
        throw new Error('Failed to delete trim: ' + await serverErrorMessage(response));
      }

      this.showToast('Trim deleted', 'success');
      await this.details._refreshJobDetails(jobId);
    } catch (error) {
      this.showToast(error.message, 'danger');
    }
  }

  openTrimDialog(job) {
    this.trimmer.open(job);
  }

  formatTimestamp(seconds) {
    return formatTimestamp(seconds);
  }

  formatBytes(bytes) {
    return formatBytes(bytes);
  }

  // --- Log panel (LogPanelController owns this: modules/log-panel.js) ---

  // Kept as a delegating shim: addLog is called directly from
  // web/tests/app.test.mjs, and other modules reach it through app.addLog(...).
  addLog(...args) {
    return this.logPanel.addLog(...args);
  }

  // ===== Tab switching =====

  /**
   * The unsaved-settings guard behind every tab switch. True when a switch to
   * `panel` may go ahead right now: Settings itself is always reachable, and
   * every other panel is while nothing on the Settings page is dirty.
   */
  _canLeaveSettings(panel) {
    return panel === "settings" || !this.settings?.isDirty();
  }

  /**
   * Ask whether to discard the unsaved settings. Resolves true when the
   * operator chose to leave — the dirty state is reset and the saved config
   * reloaded here, so whichever caller then switches starts clean — and false
   * when they stayed. Never rejects (showConfirm does not).
   * @returns {Promise<boolean>}
   */
  _confirmLeaveSettings() {
    return this.showConfirm("You have unsaved settings changes. Discard and leave?", {
      title: "Unsaved Changes",
      okLabel: "Leave Without Saving",
      okVariant: "warning"
    }).then(confirmed => {
      if (!confirmed) return false;
      this.settings._setClean();
      this.loadConfig();
      return true;
    });
  }

  /**
   * Switch to a tab the way a click on its sl-tab does — through the
   * unsaved-settings guard. Synchronous when the guard lets it through, so a
   * caller's ordering around the switch is what it always was; otherwise the
   * switch waits on the confirm, and is dropped when the operator stays.
   */
  showTab(panel) {
    const tabGroup = document.querySelector("sl-tab-group");
    if (!tabGroup) return;
    if (this._canLeaveSettings(panel)) {
      tabGroup.show(panel);
      return;
    }
    this._confirmLeaveSettings().then(confirmed => {
      if (confirmed) tabGroup.show(panel);
    });
  }

  // ===== Keyboard Shortcuts =====

  setupKeyboardShortcuts() {
    document.addEventListener("keydown", (e) => {
      // Skip when typing in input fields (composedPath handles Shoelace shadow DOM)
      if (isTypingInInput(e)) return;

      // Ctrl/Cmd/Alt combinations are the browser's: Ctrl+1..8 switched the
      // dashboard's tab (and not the browser's), Ctrl+F focused the filter
      // instead of opening Find, Ctrl+A opened the Add dialog.
      if (e.ctrlKey || e.metaKey || e.altKey) return;

      // A control that already handled this key consumed it. The status bar's
      // role="button" elements (#check-countdown, the re-login warnings, the
      // warnings icon, the version indicator) preventDefault() on Enter/Space
      // and deliberately do NOT stopPropagation() — the warning spans' handler
      // is delegated on #status-warnings, so the event has to keep bubbling.
      // Without this line Enter on one of them ALSO opens the focused job's
      // details, which the same control's click does not do.
      if (e.defaultPrevented) return;

      // If a dialog is open, block all shortcuts and let Shoelace
      // handle Escape natively (respects sl-request-close prevention)
      if (document.querySelector("sl-dialog[open]")) {
        return;
      }

      const panels = ["tasks", "archived", "player", "imports", "files", "stats", "logs", "settings"];
      // The tracked panel, not the DOM's [active] attribute, which lags
      // Shoelace's tab switch (see _activePanel).
      const activePanel = this._activePanel;
      const isPlayerActive = activePanel === "player";

      // The two panels with a job list. Each keeps its own focused row.
      const isJobListActive = activePanel === "tasks" || activePanel === "archived";

      switch (e.key) {
        case "Escape":
          // An error toast is on top of everything else on the page.
          if (this.closeNewestToast()) return;
          if (this._activeSelectionSet().size > 0) {
            this._clearSelectionUI();
            return;
          }
          break;
        case "a":
          if (!isPlayerActive) {
            document.getElementById("add-video-btn").click();
            e.preventDefault();
          }
          break;
        case "?":
          document.getElementById("keyboard-help-dialog").show();
          e.preventDefault();
          break;
        case "1": case "2": case "3": case "4": case "5": case "6": case "7": case "8":
          // Through showTab, never tabGroup.show() directly: a dirty
          // Settings page is a question a click on the tab asks, and the
          // shortcut used to skip it.
          this.showTab(panels[parseInt(e.key) - 1]);
          e.preventDefault();
          break;
        case "ArrowUp":
        case "ArrowDown":
          if (isJobListActive) {
            this.navigateJobList(e.key === "ArrowUp" ? -1 : 1);
            e.preventDefault();
          }
          break;
        case "Enter":
          // Enter on a focused control belongs to that control: a Shoelace
          // button does not preventDefault it, so Enter on Add Video, a
          // card's Delete icon or the theme toggle ALSO opened the
          // keyboard-focused job's details (over the control's own dialog).
          if (isJobListActive && !this._enterFromControl(e)) {
            const nav = this._jobListNav();
            const job = nav.index >= 0 ? nav.rows[nav.index] : null;
            if (job) this.details.showJobDetails(job);
          }
          break;
        case " ": {
          // Space on a focused row opens it, as Enter does. Only on the row
          // itself: Space on a checkbox or a button inside it is theirs.
          const row = e.target instanceof Element && e.target.classList.contains("video-item") ? e.target : null;
          if (isJobListActive && row) {
            const job = this._jobListNav().rows.find((j) => j.id === row.dataset.jobId);
            if (job) this.details.showJobDetails(job);
            e.preventDefault();
          }
          break;
        }
        case "f": {
          // The player tab binds its own "f" (fullscreen); the tasks/archived
          // filter focus would be a no-op there anyway (the filter input lives
          // in a hidden sl-tab-panel), but skip it explicitly like "a" above
          // so only one handler ever reacts to the keypress.
          if (!isPlayerActive) {
            const panel = activePanel;
            const filterId = panel === "archived" ? "archived-filter" : "tasks-filter";
            const filterInput = document.querySelector(`#${filterId} .unified-filter-input`);
            if (filterInput) { filterInput.focus(); e.preventDefault(); }
          }
          break;
        }
      }
    });
  }

  /**
   * The active panel's job list as the keyboard walks it: the rows in drawn
   * order, the container that holds their cards, and the focused index.
   * Tasks keeps its index up to date on every card it inserts or removes.
   * Archived re-reads its index from the highlighted card, because a few
   * writers change that list without drawing it.
   */
  _jobListNav() {
    if (this._activePanel === "archived") {
      const container = document.getElementById("archived-container");
      const rows = this._archivedDrawnJobs();
      const focusedId = container?.querySelector(".video-item[data-focused]")?.dataset.jobId;
      this.focusedArchivedIndex = focusedId ? rows.findIndex(j => j.id === focusedId) : -1;
      return { rows, container, key: "focusedArchivedIndex", index: this.focusedArchivedIndex };
    }
    return {
      rows: this._sortJobs(this.filterBar.getFilteredJobs()),
      container: document.getElementById("jobs-container"),
      key: "focusedJobIndex",
      index: this.focusedJobIndex,
    };
  }

  navigateJobList(direction) {
    const { rows, container, key } = this._jobListNav();
    if (rows.length === 0 || !container) return;

    // Remove previous focus (this panel's only: the other keeps its own)
    container.querySelectorAll(".video-item[data-focused]").forEach((el) => el.removeAttribute("data-focused"));

    this[key] += direction;
    if (this[key] < 0) this[key] = rows.length - 1;
    if (this[key] >= rows.length) this[key] = 0;

    const job = rows[this[key]];
    if (job) {
      const card = container.querySelector(`.video-item[data-job-id="${CSS.escape(job.id)}"]`);
      if (card) {
        card.setAttribute("data-focused", "");
        this._syncRowTabStop(container);
        // Real focus, so a screen reader announces the row Arrow moved to
        // and Tab goes on from it.
        card.focus({ preventScroll: true });
        card.scrollIntoView({ block: "nearest" });
      }
    }
  }

  // ===== Search/Filter =====

  // _sortKey is what _sortJobs orders a row by within its status: updatedAt
  // for the completed statuses, the title for everything else.
  _sortKey(job) {
    return (TASK_STATUS_PRIORITY[job.status] ?? 99) >= 7 ? job.updatedAt : job.title;
  }

  // The comparator reads keys worked out once per row. It used to build two
  // Dates or one collator per comparison, and this sort runs on every render,
  // card insert, Arrow key and Enter: 3,000 rows cost about 60 ms a sort, and
  // about 3 ms this way. Same keys and same comparisons, so the same order.
  _sortJobs(jobs) {
    const STATUS_PRIORITY = TASK_STATUS_PRIORITY;
    const rows = jobs.map((job) => {
      const pri = STATUS_PRIORITY[job.status] ?? 99;
      // Completed statuses (Cancelled/Finished) sort newest-first; everything
      // else — including the Queued resting state — sorts by title.
      return pri >= 7
        ? { job, pri, time: new Date(job.updatedAt).getTime(), title: "" }
        : { job, pri, time: 0, title: job.title || "" };
    });
    rows.sort((a, b) => {
      if (a.pri !== b.pri) return a.pri - b.pri;
      if (a.pri >= 7) return b.time - a.time;
      return NAME_COLLATOR.compare(a.title, b.title);
    });
    return rows.map((r) => r.job);
  }

  // ===== Quick Actions =====

  /** Set of job IDs currently being acted on — prevents concurrent operations */
  _jobActionsInFlight = new Set();

  quickAction(action, jobId) {
    if (this._jobActionsInFlight.has(jobId)) return; // prevent double-click race
    switch (action) {
      case "cancel": this.cancelJob(jobId); break;
      case "reinitialize": this.reinitializeJob(jobId); break;
      case "resume": this.resumeJob(jobId); break;
      case "mux": this.muxJob(jobId); break;
      case "delete": this.deleteJob(jobId); break;
    }
  }

  // ===== Theme =====

  setTheme(theme, opts = {}) {
    this.theme = theme;
    const html = document.documentElement;
    const darkSheet = document.getElementById("sl-theme-dark");
    const lightSheet = document.getElementById("sl-theme-light");
    const toggleBtn = document.getElementById("theme-toggle");

    if (theme === "light") {
      html.className = "sl-theme-light";
      if (darkSheet) darkSheet.media = "not all";
      if (lightSheet) lightSheet.media = "";
      if (toggleBtn) toggleBtn.name = "sun";
    } else {
      html.className = "sl-theme-dark";
      if (darkSheet) darkSheet.media = "";
      if (lightSheet) lightSheet.media = "not all";
      if (toggleBtn) toggleBtn.name = "moon";
    }

    // Persist only when the user explicitly picked a theme. OS-driven
    // changes stay ephemeral so the session continues to follow the OS.
    if (opts.fromOS) {
      this._themeFollowsOS = true;
    } else {
      this._themeFollowsOS = false;
      localStorage.setItem("moombox-theme", theme);
    }
    // Update theme-color meta tag
    const metaTheme = document.querySelector('meta[name="theme-color"]');
    if (metaTheme) metaTheme.content = theme === "light" ? "#ffffff" : "#1C1B22";
  }

  // ===== Status Display =====

  displayStatus(status) {
    if (status === "COOKIES?") return "Auth Required";
    return status;
  }

  formatProgressTooltip(job) {
    // Show full error in tooltip (card truncates to 50 chars)
    if (job.status === "Error" && job.error) return job.error;
    // COOKIES? carries the actionable remedy in its error text too — for a
    // membership park that text names the fix ("switch the browser to the
    // account that holds the membership"), and the flat list label has no
    // room for it.
    if (job.status === "COOKIES?" && job.error) return job.error;

    const p = job.progress || "";
    const chatCount = (n) => { const c = parseInt(n); return `${c.toLocaleString()} message${c === 1 ? "" : "s"}`; };
    // DASH: (V: 789/1000 A: 123/456 C: 50) — video first since v2.7.8;
    // pre-flip "(A: ... V: ...)" strings on old rows fall through to the
    // lastVideoSeq fallback below, which renders the same information.
    const dashMatch = p.match(/\(V:\s*(\S+)\s+A:\s*(\S+)(?:\s+C:\s*(\d+))?\)/);
    if (dashMatch) {
      let tip = `Video: ${dashMatch[1]} segments, Audio: ${dashMatch[2]} segments`;
      if (dashMatch[3]) tip += `, Chat: ${chatCount(dashMatch[3])}`;
      return tip;
    }
    // VOD: V:95.3% C: 456
    const vodMatch = p.match(/^V:([\d.]+%)(?:\s+C:\s*(\d+))?$/);
    if (vodMatch) {
      let tip = `Video: ${vodMatch[1]} downloaded`;
      if (vodMatch[2]) tip += `, Chat: ${chatCount(vodMatch[2])}`;
      return tip;
    }
    // HLS: Seq: 123 C: 456
    const hlsMatch = p.match(/^Seq:\s*(\d+)(?:\s+C:\s*(\d+))?$/);
    if (hlsMatch) {
      let tip = `Segments: ${parseInt(hlsMatch[1]).toLocaleString()}`;
      if (hlsMatch[2]) tip += `, Chat: ${chatCount(hlsMatch[2])}`;
      return tip;
    }
    // Frontend fallback: V: 123 / A: 456
    if (job.lastVideoSeq !== undefined) {
      return `Video: ${job.lastVideoSeq || 0} segments, Audio: ${job.lastAudioSeq || 0} segments`;
    }
    return "";
  }

  // ===== Utilities =====

  /**
   * Extract a media target from input. Returns:
   * - { platform: "youtube", videoId } for YouTube
   * - { platform: "twitch", type: "channel"|"vod", id } for Twitch
   * - null if unrecognized
   */
  extractMediaTarget(input) {
    const trimmed = input.trim();
    if (!trimmed) return null;

    // Check for Twitch URLs first
    if (trimmed.includes("twitch.tv")) {
      return this.extractTwitchTarget(trimmed);
    }

    // A "v"-prefixed Twitch VOD ID before YouTube, as the TUI's
    // extractMediaID takes it: "v" and 10 digits is 11 characters and would
    // otherwise pass for a YouTube video ID (owner decision FE-6).
    if (/^v\d{1,12}$/.test(trimmed)) return this.extractTwitchTarget(trimmed);

    // Try YouTube
    const videoId = this.extractVideoId(trimmed);
    if (videoId) return { platform: "youtube", videoId };

    // Try Twitch raw username/VOD
    const twitchTarget = this.extractTwitchTarget(trimmed);
    if (twitchTarget) return twitchTarget;

    return null;
  }

  /**
   * Read a YouTube video ID from a bare 11-character ID or a YouTube URL.
   * Mirrors utils.ExtractVideoID (internal/utils/youtube.go), the one parser
   * the CLI, the TUI and the add route use; both are tested against
   * internal/utils/testdata/youtube_urls.json. The scheme is optional, the
   * host is read in any case, and the forms are a v= parameter anywhere in
   * the query (or the fragment), youtu.be/ID, and /live/, /shorts/, /embed/
   * and /v/ID on youtube.com, youtube-nocookie.com, youtubekids.com and
   * their subdomains. When the input as a whole is not such a URL, the first
   * YouTube link inside it is read the same way ("<https://youtu.be/ID>",
   * "see https://youtu.be/ID"). The time taken grows with the length of the
   * input and no faster: a pasted megabyte must not hold the page.
   *
   * Like the Go parser this takes the link apart by hand and unescapes
   * nothing: the browser's URL and Go's net/url disagree on a stray "%", a
   * ";" in the query, dot segments and escapes, and the two parsers must
   * give one answer. Keep the two step for step alike.
   */
  extractVideoId(input) {
    const trimmed = input.trim();

    // Direct video ID (11 chars): the whole input only, never a word in a
    // sentence.
    if (/^[a-zA-Z0-9_-]{11}$/.test(trimmed)) {
      return trimmed;
    }

    const httpScheme = /^[Hh][Tt][Tt][Pp][Ss]?:\/\//;

    // Exactly 11 ID characters, then nothing or a character no ID holds.
    const leadingId = (s) => {
      const m = /^[a-zA-Z0-9_-]+/.exec(s);
      return m && m[0].length === 11 ? m[0] : null;
    };
    // The ID the first v= pair of a raw query string starts with. Pairs are
    // split on "&" only and the value is read as written, so "v=ID;t=3" and
    // "v=ID%" still name ID.
    const idParam = (query) => {
      for (const pair of query.split("&")) {
        if (pair.startsWith("v=")) return leadingId(pair.slice(2));
      }
      return null;
    };
    const cut = (s, sep) => {
      const i = s.indexOf(sep);
      return i < 0 ? [s, ""] : [s.slice(0, i), s.slice(i + 1)];
    };

    // Whether host, in any case, is one of YouTube's: "short" for youtu.be,
    // whose path is the video ID, "long" for the others, null for none. The
    // longest domain, with the dot before it, is all of a long host that can
    // matter.
    const youtubeHost = (host) => {
      host = host.slice(-".youtube-nocookie.com".length).toLowerCase();
      const isOrUnder = (domain) => host === domain || host.endsWith("." + domain);
      if (isOrUnder("youtu.be")) return "short";
      return isOrUnder("youtube.com") || isOrUnder("youtube-nocookie.com") || isOrUnder("youtubekids.com") ? "long" : null;
    };
    // The ID in the path, query and fragment that follow a YouTube host.
    const afterHost = (rest, kind) => {
      let fragment, path, query;
      [rest, fragment] = cut(rest, "#");
      [path, query] = cut(rest, "?");
      let id = idParam(query);
      if (id) return id;
      // The old "#!v=ID" and "#v=ID" forms.
      id = idParam(fragment.startsWith("!") ? fragment.slice(1) : fragment);
      if (id) return id;
      if (kind === "short") return leadingId(path.startsWith("/") ? path.slice(1) : path);
      for (const prefix of ["/live/", "/shorts/", "/embed/", "/v/"]) {
        if (path.startsWith(prefix)) return leadingId(path.slice(prefix.length));
      }
      return null;
    };

    // The ID of s when all of s is one YouTube link.
    const fromLink = (s) => {
      if (httpScheme.test(s)) s = s.slice(s.indexOf("://") + 3);
      else if (/^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//.test(s)) return null;
      else if (s.startsWith("//")) s = s.slice(2);

      // host[:port], after any user info, up to the path, query or fragment.
      const end = s.search(/[/?#]/);
      let authority = end < 0 ? s : s.slice(0, end);
      let rest = end < 0 ? "" : s.slice(end);
      authority = authority.slice(authority.lastIndexOf("@") + 1);
      let host = authority;
      const colon = authority.lastIndexOf(":");
      if (colon >= 0) {
        host = authority.slice(0, colon);
        if (!/^[0-9]*$/.test(authority.slice(colon + 1))) return null;
      }
      if (!/^[a-zA-Z0-9.-]*$/.test(host)) return null;
      const kind = youtubeHost(host);
      return kind ? afterHost(rest, kind) : null;
    };

    const whole = fromLink(trimmed);
    if (whole) return whole;

    // The links inside other text, in one pass, as the Go parser reads
    // them. Each is a run of host characters and the slash after it, with
    // an http(s) scheme, "//" or nothing before it, and it ends at the next
    // whitespace. A link written without a scheme counts only at the start
    // or after a character that cannot continue a host, a path or user
    // info, so "example.com/youtube.com/watch?v=ID" stays refused; text
    // that is not ASCII continues no host.
    const continuesLink = (i) => {
      if (i === 0) return false;
      if (trimmed[i - 1] === ":" && trimmed.startsWith("//", i)) return true; // another scheme's "://"
      return /[a-zA-Z0-9_.\/\\@-]/.test(trimmed[i - 1]);
    };
    const isHostCode = (c) =>
      (c >= 0x61 && c <= 0x7a) || (c >= 0x41 && c <= 0x5a) || (c >= 0x30 && c <= 0x39) || c === 0x2e || c === 0x2d;
    const whitespace = /[ \t\n\v\f\r]/g;
    // YouTube links that hold no ID are each read to the end of their run
    // of text without whitespace, so only this many are read in one run
    // (embeddedLinkTries in the Go parser): a megabyte of them would
    // otherwise cost its length squared.
    const tryLimit = 32;
    let runEnd = 0; // the end of the run of text without whitespace being read
    let tries = 0; // the YouTube links in it that held no ID
    for (let off = 0; off < trimmed.length; ) {
      let host = off;
      let slash = -1;
      for (let i = off; i < trimmed.length; i++) {
        const c = trimmed.charCodeAt(i);
        if (isHostCode(c)) continue;
        if (c === 0x2f && i > host) {
          slash = i;
          break;
        }
        host = i + 1;
      }
      if (slash < 0) break;
      // The link starts at its scheme, when one stands right before the
      // host and after the last link's slash.
      let start = host;
      let hasScheme = false;
      const before = trimmed.slice(Math.max(off, host - "https://".length), host);
      const scheme = /[Hh][Tt][Tt][Pp][Ss]?:\/\/$/.exec(before);
      if (scheme) {
        start = host - (before.length - scheme.index);
        hasScheme = true;
      } else if (before.endsWith("//")) {
        start = host - 2;
      }
      if (slash >= runEnd) {
        whitespace.lastIndex = slash;
        const ws = whitespace.exec(trimmed);
        runEnd = ws ? ws.index : trimmed.length;
        tries = 0;
      }
      // Look again after this link's host next time: a link inside this
      // one's path or query ("?u=https://youtu.be/ID") is still found.
      off = slash + 1;

      if (!hasScheme && continuesLink(start)) continue;
      const kind = youtubeHost(trimmed.slice(host, slash));
      if (!kind) continue;
      const id = afterHost(trimmed.slice(slash, runEnd), kind);
      if (id) return id;
      if (++tries >= tryLimit) off = runEnd;
    }
    return null;
  }

  /**
   * Parse a Twitch URL or identifier. Its URL forms mirror
   * utils.ExtractTwitchTarget (internal/utils/twitch.go) row for row: both —
   * and the TUI, which calls the Go one — are tested against
   * internal/utils/testdata/twitch_urls.json, URL rows and bare identifiers
   * alike. A bare identifier reads as Go reads it (owner decision FE-6): "v"
   * and 1-12 digits, or 7-12 bare digits, is a VOD, and any other valid login
   * is a channel — a bare "videos" or "clip" too, since the reserved words
   * name site pages only as a URL's first path segment.
   * The forms are yt-dlp's (yt_dlp/extractor/twitch.py b77c9304e): hosts www.,
   * go., m. and bare; /videos/<id>, /<ch>/v/<id>, /<ch>/video/<id>,
   * /<ch>/schedule?vodID=<id>; player.twitch.tv ?video=v<id> / ?channel=;
   * clips with or without a channel; and the listings — /<ch>/videos,
   * /<ch>/profile, /<ch>/clips, /collections/<id> — which parse to their own
   * types so none is read as a channel's live-wait job: a video list or a
   * collection queues its VODs (addTwitchListing), a clip list is refused.
   * Returns { platform: "twitch", type, id } or null; type is channel, vod,
   * clip, videos, clips or collection.
   */
  extractTwitchTarget(input) {
    const trimmed = input.trim();
    if (!trimmed) return null;

    const reserved = new Set([
      "directory", "downloads", "jobs", "settings", "videos", "search", "p", "store", "tag", "tags",
      "turbo", "drops", "wallet", "prime", "subs", "subscriptions", "friends", "inventory", "payments",
      "collections", "clip",
    ]);
    const siteHosts = new Set(["twitch.tv", "www.twitch.tv", "go.twitch.tv", "m.twitch.tv"]);
    const loginRe = /^[a-zA-Z][a-zA-Z0-9_]{0,24}$/;
    const vodIdRe = /^\d{1,12}$/;
    const slugRe = /^[a-zA-Z0-9_-]+$/;
    const target = (type, id) => ({ platform: "twitch", type, id });
    const vod = (id) => (vodIdRe.test(id || "") ? target("vod", id) : null);
    const clip = (slug) => (slugRe.test(slug || "") ? target("clip", slug) : null);

    let url = null;
    try {
      let urlStr = trimmed;
      const lower = urlStr.toLowerCase();
      const bareHost = ["twitch.tv", "www.twitch.tv", "go.twitch.tv", "m.twitch.tv", "player.twitch.tv", "clips.twitch.tv"]
        .some((h) => lower.startsWith(h + "/"));
      if (bareHost) urlStr = "https://" + urlStr;
      if (/^https?:\/\//i.test(urlStr)) url = new URL(urlStr);
    } catch {}

    if (url) {
      const host = url.hostname.toLowerCase();
      const parts = url.pathname.split("/").filter(Boolean);
      const q = url.searchParams;
      if (host === "player.twitch.tv") {
        const v = q.get("video");
        if (v) return vod(v.replace(/^v/, ""));
        const login = (q.get("channel") || "").toLowerCase();
        return loginRe.test(login) ? target("channel", login) : null;
      }
      if (host === "clips.twitch.tv") {
        if (parts.length >= 2 && parts[parts.length - 1] === "edit") parts.pop();
        const slug = parts.length === 1 && parts[0] === "embed" ? q.get("clip") : parts[parts.length - 1];
        return clip(slug);
      }
      if (siteHosts.has(host)) {
        if (parts.length === 0) return null;
        if (parts[0] === "videos") return parts.length >= 2 ? vod(parts[1]) : null;
        if (parts[0] === "clip") return parts.length >= 2 ? clip(parts[1]) : null;
        if (parts[0] === "collections") {
          return parts.length >= 2 && slugRe.test(parts[1]) ? target("collection", parts[1]) : null;
        }
        const login = parts[0].toLowerCase();
        if (reserved.has(login)) return null;
        if (parts.length >= 2) {
          if (parts[1] === "v" || parts[1] === "video") return parts.length >= 3 ? vod(parts[2]) : null;
          if (parts[1] === "clip") return parts.length >= 3 ? clip(parts[2]) : null;
          if (parts[1] === "schedule" && q.get("vodID")) return vod(q.get("vodID"));
        }
        if (!loginRe.test(login)) return null;
        if (parts.length >= 2) {
          if (parts[1] === "videos" || parts[1] === "profile") {
            return target(parts[1] === "videos" && q.get("filter") === "clips" ? "clips" : "videos", login);
          }
          if (parts[1] === "clips") return target("clips", login);
        }
        return target("channel", login);
      }
    }

    // Raw VOD ID with v prefix: 1-12 digits, as Go's twitchVODPrefixRe
    if (/^v\d{1,12}$/.test(trimmed)) {
      return { platform: "twitch", type: "vod", id: trimmed.slice(1) };
    }
    // Raw numeric VOD ID: 7-12 digits, to tell it from other numbers
    if (/^\d{7,12}$/.test(trimmed)) {
      return { platform: "twitch", type: "vod", id: trimmed };
    }
    // Raw login, a reserved path word included (Go's bare-login rule)
    if (loginRe.test(trimmed)) {
      return { platform: "twitch", type: "channel", id: trimmed.toLowerCase() };
    }
    return null;
  }

  formatRelativeTime(isoDate) {
    return formatRelativeTime(isoDate);
  }

  formatDurationSeconds(totalSeconds) {
    return formatDurationSeconds(totalSeconds);
  }

  /** Returns the compact watch status pill HTML for job details. */
  watchPillHtml(job) {
    if (job.status !== "Finished") return "";
    const resumeStr = job.resumePosition != null ? formatTimestamp(job.resumePosition) : "";
    if (job.watched && resumeStr) {
      return `<span class="watch-pill watched"><svg viewBox="0 0 24 24" fill="currentColor" stroke="currentColor" stroke-width="2"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3" fill="rgba(255,255,255,0.6)"/></svg>Watched · Paused at ${this.escapeHtml(resumeStr)}</span>`;
    }
    if (job.watched) {
      return `<span class="watch-pill watched"><svg viewBox="0 0 24 24" fill="currentColor" stroke="currentColor" stroke-width="2"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3" fill="rgba(255,255,255,0.6)"/></svg>Watched</span>`;
    }
    if (resumeStr) {
      return `<span class="watch-pill in-progress"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg>Paused at ${this.escapeHtml(resumeStr)}</span>`;
    }
    return "";
  }

  setInputValue(id, value) {
    const el = document.getElementById(id);
    if (!el) return;
    // Default to empty string for undefined/null so fields are cleared
    // when a config value is removed (e.g. omitempty field reset server-side)
    el.value = value ?? "";
  }

  getInputValue(id) {
    const el = document.getElementById(id);
    if (!el) return "";
    const val = el.value;
    // Handle both string and number values (Shoelace number inputs return numbers)
    if (typeof val === "string") {
      return val.trim();
    }
    return val !== undefined && val !== null ? String(val) : "";
  }

  getInputNumber(id) {
    const el = document.getElementById(id);
    if (!el) return undefined;
    const val = el.value;
    // Shoelace number inputs may return number directly
    if (typeof val === "number") {
      return isNaN(val) ? undefined : val;
    }
    const str = typeof val === "string" ? val.trim() : "";
    if (!str) return undefined;
    // Use Number() instead of parseInt() to reject partial numbers like "123abc"
    // Don't truncate — FlexDuration fields accept fractional values (e.g. 2.5 minutes).
    // Integer fields are truncated server-side by Go's int() conversion.
    const num = Number(str);
    return isNaN(num) ? undefined : num;
  }

  escapeHtml(text) {
    if (text == null) return "";
    return String(text)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#039;");
  }

  /**
   * Copy text to the clipboard. navigator.clipboard only exists on secure
   * origins (HTTPS or localhost) — plain-HTTP LAN access is a first-class
   * deployment mode, so fall back to a temporary textarea + execCommand there.
   */
  copyTextToClipboard(text) {
    if (navigator.clipboard) {
      navigator.clipboard.writeText(text).catch(() => {
        this.showToast("Failed to copy to clipboard", "warning");
      });
      return;
    }
    try {
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      const ok = document.execCommand("copy");
      ta.remove();
      if (!ok) throw new Error("execCommand failed");
    } catch {
      this.showToast("Clipboard requires HTTPS or localhost", "warning");
    }
  }

  /**
   * Show a themed confirmation dialog. Returns a Promise<boolean> that never
   * rejects: true for OK, false for Cancel or any other close.
   *
   * One dialog, one owner at a time (K34): a call made while a confirm is
   * open — or still closing — waits its turn and is shown once that one has
   * been answered and fully hidden (sl-after-hide). It used to replace the
   * open confirm's text and buttons under the operator's cursor and resolve
   * the first caller false, silently: a Delete confirm opened during the
   * Reinitialize preview fetch, or the restart prompt landing over a
   * leave-settings confirm after a save. Callers re-read their state after
   * the answer, so one shown late still acts on the job as it stands then.
   * @param {string} message — text to display
   * @param {object} [opts] — { okLabel, okVariant, title }
   */
  showConfirm(message, opts = {}) {
    return new Promise((resolve) => {
      this._confirmQueue ??= [];
      this._confirmQueue.push({ message, opts, resolve, answered: false });
      if (this._confirmQueue.length === 1) this._drawConfirm();
    });
  }

  // Draws the head of the confirm queue. Its buttons and the dialog's hide
  // are bound once (_bindConfirmDialog) and act on whichever entry is the
  // head, so nothing is cloned and no listener stacks.
  _drawConfirm() {
    const head = this._confirmQueue?.[0];
    if (!head) return;
    const dlg = document.getElementById("confirm-dialog");
    const msg = document.getElementById("confirm-dialog-message");
    const okBtn = document.getElementById("confirm-dialog-ok");
    if (!dlg || !msg || !okBtn) {
      // No dialog to ask in: declined, as a close would be.
      this._confirmQueue.shift();
      head.resolve(false);
      this._drawConfirm();
      return;
    }
    this._bindConfirmDialog(dlg);
    dlg.label = head.opts.title || "Confirm";
    msg.textContent = head.message;
    okBtn.textContent = head.opts.okLabel || "OK";
    okBtn.variant = head.opts.okVariant || "primary";
    dlg.show();
  }

  // The answer for the head, once: OK or Cancel resolve it at once (the
  // caller goes on while the dialog animates out), any other close resolves
  // it false on sl-after-hide. The next entry is drawn only on
  // sl-after-hide — showing during the hide animation would cancel it and
  // fire a spurious sl-after-hide.
  _answerConfirm(result) {
    const head = this._confirmQueue?.[0];
    if (!head || head.answered) return;
    head.answered = true;
    head.resolve(result);
  }

  _bindConfirmDialog(dlg) {
    if (this._confirmBound === dlg) return;
    this._confirmBound = dlg;
    document.getElementById("confirm-dialog-ok")?.addEventListener("click", () => {
      if (!this._confirmQueue?.[0] || this._confirmQueue[0].answered) return;
      this._answerConfirm(true);
      dlg.hide();
    });
    document.getElementById("confirm-dialog-cancel")?.addEventListener("click", () => {
      if (!this._confirmQueue?.[0] || this._confirmQueue[0].answered) return;
      this._answerConfirm(false);
      dlg.hide();
    });
    dlg.addEventListener("sl-after-hide", (e) => {
      // Shoelace's own children bubble their sl-after-hide up through the
      // dialog; only the dialog's own ends a confirm.
      if (e.target !== dlg) return;
      this._answerConfirm(false);
      this._confirmQueue?.shift();
      this._drawConfirm();
    });
  }


  /**
   * Show a toast. Every toast on the dashboard comes through here. How long
   * it stays is toastDuration's answer (utils.js, owner ruling R2-D3): an
   * error until it is closed, anything else for a time that grows with its
   * length. The countdown bar runs for that same time, and an error has none.
   *
   * Toasts that stay until closed are kept track of (_stayingToasts): one
   * already up with the same text is flashed, not stacked a second time (a
   * failing button pressed five times); at most MAX_STAYING_TOASTS are up,
   * the oldest making way; and Escape closes the newest (closeNewestToast).
   */
  showToast(message, variant = "primary") {
    const duration = toastDuration(message, variant);
    const timed = Number.isFinite(duration);
    if (!timed) {
      // Shoelace takes a closed toast out of the document.
      this._stayingToasts = (this._stayingToasts || []).filter((t) => t.alert.isConnected);
      const same = this._stayingToasts.find((t) => t.message === message && t.variant === variant);
      if (same) {
        // Restart the flash: the class has to leave and come back.
        same.alert.classList.remove("toast-again");
        void same.alert.offsetWidth;
        same.alert.classList.add("toast-again");
        return;
      }
    }
    const alert = document.createElement("sl-alert");
    alert.variant = variant;
    alert.closable = true;
    alert.duration = duration;
    alert.className = "toast-alert";
    if (timed) alert.style.setProperty("--toast-duration", `${duration}ms`);

    const icon = document.createElement("sl-icon");
    icon.slot = "icon";
    icon.name = this.getIconForVariant(variant);
    alert.appendChild(icon);

    alert.appendChild(document.createTextNode(message));

    if (timed) {
      const countdown = document.createElement("div");
      countdown.className = "toast-countdown";
      alert.appendChild(countdown);
    }

    // Shoelace's `.toast()` already detaches the alert on dismiss
    // (see Shoelace docs for sl-alert.toast: "when dismissed, it will
    // be removed from the DOM completely"). Adding our own
    // `sl-after-hide → alert.remove()` listener races with that
    // internal cleanup and causes a "Node.removeChild: not a child"
    // exception inside chunk.XCZS77LP.js. Removed in test.41.
    document.body.append(alert);
    alert.toast();

    if (!timed) {
      const entry = { alert, message, variant };
      this._stayingToasts.push(entry);
      alert.addEventListener("sl-after-hide", () => {
        this._stayingToasts = this._stayingToasts.filter((t) => t !== entry);
      }, { once: true });
      while (this._stayingToasts.length > MAX_STAYING_TOASTS) this._stayingToasts.shift().alert.hide();
    }
  }

  /**
   * Close the newest toast that stays until closed. Its X button is the only
   * other way, and that is at the end of the tab order. False when none is up.
   */
  closeNewestToast() {
    const open = (this._stayingToasts || []).filter((t) => t.alert.isConnected);
    const newest = open.pop();
    this._stayingToasts = open;
    if (!newest) return false;
    newest.alert.hide();
    return true;
  }

  getIconForVariant(variant) {
    switch (variant) {
      case "success":
        return "check2-circle";
      case "warning":
        return "exclamation-triangle";
      case "danger":
        return "exclamation-octagon";
      default:
        return "info-circle";
    }
  }

  // --- Files tab (FilesController owns this: modules/files.js) ---

  // Kept as delegating shims: web/tests/app.test.mjs pins these two by
  // calling them directly on the app, the same way the real DOM click
  // handlers used to before the Files tab moved into FilesController.
  renderOrphanedFiles(files) {
    return this.files.renderOrphanedFiles(files);
  }

  renderOrphanedHistory(entries) {
    return this.files.renderOrphanedHistory(entries);
  }

  /** Return selected jobs for the currently active panel. */
  /**
   * Empty the active panel's selection and undo its marks in the DOM — the
   * ticked boxes and the .selected highlight — then refresh the batch bar.
   * One home for what the Clear button, Escape and a finished batch action
   * all do.
   */
  _clearSelectionUI() {
    this._activeSelectionSet().clear();
    const containerId = this._activePanel === "archived" ? "archived-container" : "jobs-container";
    const container = document.getElementById(containerId);
    if (container) {
      container.querySelectorAll(".job-checkbox").forEach(cb => { cb.checked = false; });
      container.querySelectorAll(".video-item.selected").forEach(el => el.classList.remove("selected"));
    }
    this.updateBatchActionBar();
  }

  _getSelectedJobs() {
    const panel = this._activePanel;
    if (panel === "archived") {
      return this.archivedJobs.filter(j => this._selectedArchivedJobs.has(j.id));
    }
    return this.jobs.filter(j => this._selectedTaskJobs.has(j.id));
  }

  updateBatchActionBar() {
    const bar = document.getElementById("batch-action-bar");
    if (!bar) return;

    // While a batch runs its buttons are off: its targets are already being
    // acted on, and a second click used to be dropped without a word.
    const busy = this._batchProgress;
    for (const id of BATCH_ACTION_BUTTON_IDS) {
      const btn = document.getElementById(id);
      if (btn && btn.disabled !== !!busy) btn.disabled = !!busy;
    }
    // The bar is a live region and the progress line is rewritten once per
    // answered request: busy for the length of the batch, so a screen reader
    // is not handed "Deleting 1 of 2,000…" two thousand times. The closing
    // toast says how it ended.
    if (busy) bar.setAttribute("aria-busy", "true");
    else bar.removeAttribute("aria-busy");

    const count = this._activeSelectionSet().size;
    // The bar stays up for a running batch even once its rows have left the
    // selection (a deleted row leaves it): it is where the progress shows.
    if (count === 0 && !busy) {
      if (bar.style.display === "none") return; // already hidden
      bar.classList.remove("visible");
      document.body.classList.remove("batch-bar-active");
      bar.addEventListener("transitionend", () => {
        if (!bar.classList.contains("visible")) {
          bar.style.display = "none";
        }
      }, { once: true });
      return;
    }
    bar.style.display = "";
    bar.classList.add("visible");
    document.body.classList.add("batch-bar-active");
    // Archived draws only its newest cards, and Select All ticks every
    // matching row: say how many of the selected ones are not on screen, so
    // a batch Delete is never larger than the bar admits.
    let notShown = 0;
    if (this._activePanel === "archived") {
      const drawnSelected = document.getElementById("archived-container")
        ?.querySelectorAll(".video-item.selected").length ?? count;
      notShown = Math.max(0, count - drawnSelected);
    }
    if (busy) {
      this._drawBatchProgress();
    } else {
      document.getElementById("batch-count").textContent = notShown > 0
        ? `${count} selected (${notShown} not shown yet)`
        : `${count} selected`;
    }

    // Update Select All label with count when filters are active, and on
    // Archived whenever it would select rows that are not drawn.
    const selectAllBtn = document.getElementById("batch-select-all");
    if (selectAllBtn) {
      const panel = this._activePanel;
      const isFiltered = panel === "archived"
        ? this.filterBar.tokens("archived").length > 0
        : this.filterBar.tokens("jobs").length > 0;
      const archivedTotal = panel === "archived" ? this.filterBar.getFilteredArchivedJobs().length : 0;
      if (isFiltered || archivedTotal > this._archivedRenderLimit) {
        const totalVisible = panel === "archived"
          ? archivedTotal
          : this.filterBar.getFilteredJobs().length;
        selectAllBtn.textContent = `Select All (${totalVisible})`;
      } else {
        selectAllBtn.textContent = "Select All";
      }
    }

    const selectedJobs = this._getSelectedJobs();
    const canCancel = selectedJobs.some(j => CANCEL_STATUSES.has(j.status));
    const canResume = selectedJobs.some(j => canResumeJob(j));
    const canReinit = selectedJobs.some(j => REINIT_STATUSES.has(j.status));
    const canDelete = selectedJobs.some(j => DELETE_STATUSES.has(j.status));

    document.getElementById("batch-cancel").style.display = canCancel ? "" : "none";
    document.getElementById("batch-resume").style.display = canResume ? "" : "none";
    document.getElementById("batch-reinit").style.display = canReinit ? "" : "none";
    document.getElementById("batch-delete").style.display = canDelete ? "" : "none";
    const canWatch = selectedJobs.some(j => WATCHED_STATUSES.has(j.status) && !j.watched);
    const canUnwatch = selectedJobs.some(j => WATCHED_STATUSES.has(j.status) && (j.watched || j.resumePosition != null));
    document.getElementById("batch-watched").style.display = canWatch ? "" : "none";
    document.getElementById("batch-unwatched").style.display = canUnwatch ? "" : "none";

    // Hide divider between standard and watch buttons when either side is empty
    const divider = bar.querySelector("sl-divider");
    if (divider) {
      const hasStandard = canCancel || canResume || canReinit || canDelete;
      const hasWatch = canWatch || canUnwatch;
      divider.style.display = (hasStandard && hasWatch) ? "" : "none";
    }
  }

  /**
   * Put keyboard focus back after a batch started from a batch button
   * (`from`, null when it was not): on that button when the bar still shows
   * it, otherwise on the list's row. Nothing moves when the operator has
   * put focus somewhere else meanwhile, or changed tab.
   */
  _returnBatchFocus(from) {
    if (!from || from.panel !== this._activePanel) return;
    const active = document.activeElement;
    if (active && active !== document.body && active !== from.button) return;
    const bar = document.getElementById("batch-action-bar");
    if (bar?.classList.contains("visible") && from.button.style.display !== "none" && !from.button.disabled) {
      from.button.focus();
      return;
    }
    const container = document.getElementById(from.panel === "archived" ? "archived-container" : "jobs-container");
    const row = container?.querySelector('.video-item[tabindex="0"]') ?? container?.querySelector(".video-item");
    row?.focus({ preventScroll: true });
  }

  /** Write the running batch's progress ("Deleting 120 of 2,000…") in the
   *  batch bar. Only the text: it runs once per answered request. */
  _drawBatchProgress() {
    const p = this._batchProgress;
    const el = document.getElementById("batch-count");
    if (!p || !el) return;
    el.textContent = `${p.verb} ${p.done.toLocaleString()} of ${p.total.toLocaleString()}…`;
  }

  // _enterFromControl reports whether a keydown came from a control that
  // handles Enter itself — a button of any kind, a link, a form field.
  _enterFromControl(e) {
    const path = typeof e.composedPath === "function" ? e.composedPath() : [e.target];
    return path.some((el) => el instanceof Element && el.matches(
      "button, a[href], input, select, textarea, sl-button, sl-icon-button, sl-checkbox, sl-switch, sl-select, sl-input, sl-dropdown, sl-menu-item, [role=button], [role=menuitem]"));
  }

  // _currentJob is the row this tab holds for id now — the active list, then
  // the archived one — for actions that must re-read a status after a confirm
  // the operator may have left open.
  _currentJob(id) {
    return this.jobs.find(j => j.id === id) || this.archivedJobs.find(j => j.id === id);
  }

  async batchAction(action) {
    // The batch button pressed, read before the confirm dialog takes focus.
    // The batch disables it or hides the bar, which drops keyboard focus to
    // the page; _returnBatchFocus puts it back when the batch ends.
    const pressed = document.activeElement;
    const focusFrom = pressed && BATCH_ACTION_BUTTON_IDS.includes(pressed.id) ? { button: pressed, panel: this._activePanel } : null;
    // One batch at a time: the bar's buttons are disabled while one runs.
    if (this._batchProgress) return;
    const selectedJobs = this._getSelectedJobs();
    // Strip jobs already being acted on (single-click cancel/resume/etc.
    // immediately followed by a batch op on the same id would otherwise race).
    const isPerJob = action === "cancel" || action === "resume" || action === "reinitialize" || action === "delete";
    const eligibleJobs = isPerJob
      ? selectedJobs.filter(j => !this._jobActionsInFlight.has(j.id))
      : selectedJobs;
    let targets, confirmMsg, apiCall, qualifies;

    switch (action) {
      case "cancel":
        qualifies = j => CANCEL_STATUSES.has(j.status);
        targets = eligibleJobs.filter(qualifies);
        confirmMsg = `Cancel ${targets.length} job${targets.length !== 1 ? "s" : ""}?`;
        apiCall = (id) => fetch(`/api/jobs/${id}/cancel`, { method: "POST" });
        break;
      case "resume":
        qualifies = j => canResumeJob(j);
        targets = eligibleJobs.filter(qualifies);
        confirmMsg = `Resume ${targets.length} job${targets.length !== 1 ? "s" : ""}?`;
        apiCall = (id) => fetch(`/api/jobs/${id}/resume`, { method: "POST" });
        break;
      case "reinitialize":
        qualifies = j => REINIT_STATUSES.has(j.status);
        targets = eligibleJobs.filter(qualifies);
        confirmMsg = `Reinitialize ${targets.length} job${targets.length !== 1 ? "s" : ""}?`;
        if (targets.length > 0) {
          // Name the totals the reset deletes across them (owner decision
          // PR-M1), as the single confirm does.
          const { text: deletes } = await this.reinitializeDeletes(targets.map((j) => j.id));
          confirmMsg += deletes
            ? ` This ${deletes}. This cannot be undone.`
            : " What it would delete could not be read: it deletes each job's set-aside recordings, recorded parts, staged data and imported chapters. This cannot be undone.";
        }
        apiCall = (id) => fetch(`/api/jobs/${id}/reinitialize`, { method: "POST" });
        break;
      case "delete":
        qualifies = j => DELETE_STATUSES.has(j.status);
        targets = eligibleJobs.filter(qualifies);
        confirmMsg = `Delete ${targets.length} job${targets.length !== 1 ? "s" : ""}?`;
        apiCall = (id) => fetch(`/api/jobs/${id}`, { method: "DELETE" });
        break;
      case "watched":
        qualifies = j => WATCHED_STATUSES.has(j.status) && !j.watched;
        targets = selectedJobs.filter(qualifies);
        confirmMsg = `Mark ${targets.length} job${targets.length !== 1 ? "s" : ""} as watched?`;
        apiCall = () => fetch("/api/jobs/batch/watched", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ jobIds: targets.map(j => j.id) }),
        });
        break;
      case "unwatched":
        qualifies = j => WATCHED_STATUSES.has(j.status) && (j.watched || j.resumePosition != null);
        targets = selectedJobs.filter(qualifies);
        confirmMsg = `Mark ${targets.length} job${targets.length !== 1 ? "s" : ""} as unwatched?`;
        apiCall = () => fetch("/api/jobs/batch/watched", {
          method: "DELETE",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ jobIds: targets.map(j => j.id) }),
        });
        break;
    }

    if (!targets || targets.length === 0) return;

    const confirmed = await this.showConfirm(confirmMsg, {
      title: "Batch " + action.charAt(0).toUpperCase() + action.slice(1),
      okLabel: action.charAt(0).toUpperCase() + action.slice(1),
      okVariant: action === "delete" || action === "reinitialize" ? "danger" : action === "cancel" ? "warning" : "primary"
    });
    if (!confirmed) return;

    // The dialog waits on the operator, and rows move meanwhile — an Error
    // job a monitor retried is Downloading by the time OK is clicked, and the
    // DELETE route cancels a running download before removing it. Act only on
    // what still qualifies now, as the TUI re-validates at its confirm.
    const offered = targets.length;
    targets = targets.map(j => this._currentJob(j.id)).filter(j => j && qualifies(j));
    if (targets.length < offered) {
      const moved = offered - targets.length;
      this.showToast(`${moved} job${moved !== 1 ? "s" : ""} changed state while you confirmed and ${moved !== 1 ? "were" : "was"} skipped`, "warning");
    }
    if (targets.length === 0) return;

    let succeeded, failed;
    if (action === "watched" || action === "unwatched") {
      // Single batch API call
      try {
        const res = await apiCall();
        succeeded = res.ok ? targets.length : 0;
        failed = res.ok ? 0 : targets.length;
        // The route answers {success}, not rows, and the jobs_update that
        // follows it (BatchSetWatched still fires OnJobsChange) restates the
        // active list only — an archived row is never restated, so the
        // Archived panel kept its eyes and its checked boxes until the next
        // fetchArchivedJobs. Write what the server wrote (watched, and the
        // resume position it clears either way); the redraw below shows it.
        if (res.ok) {
          for (const j of targets) {
            j.watched = action === "watched";
            j.resumePosition = null;
          }
        }
      } catch {
        succeeded = 0;
        failed = targets.length;
      }
    } else {
      // Per-job API calls. Track each target in _jobActionsInFlight so a
      // concurrent single-click action is gated while the batch runs.
      for (const j of targets) this._jobActionsInFlight.add(j.id);
      // Say how far it is: a large batch takes a while, and the bar showed
      // nothing until the closing toast. Counted as each request is answered,
      // whatever the answer.
      const progress = { verb: BATCH_PROGRESS_VERBS[action], done: 0, total: targets.length };
      this._batchProgress = progress;
      this.updateBatchActionBar();
      const counted = (request) => request.finally(() => {
        progress.done++;
        this._drawBatchProgress();
      });
      try {
        const results = await Promise.allSettled(targets.map(j => counted(apiCall(j.id))));
        succeeded = results.filter(r => r.status === "fulfilled" && r.value?.ok).length;
        failed = targets.length - succeeded;
      } finally {
        // Before the bar is drawn again below (_clearSelectionUI), which
        // turns the buttons back on.
        this._batchProgress = null;
        for (const j of targets) this._jobActionsInFlight.delete(j.id);
      }
    }

    // The checkboxes too, not just the set: a target the server refused (a
    // 400 from /resume or /cancel) provokes no job_update, and a selected row
    // that was not a target of this verb at all is redrawn only if another
    // row's status change re-renders the list — so both kept a ticked box
    // and the .selected highlight with nothing behind them.
    this._clearSelectionUI();
    this._returnBatchFocus(focusFrom);
    // The per-job actions are redrawn by the job_update each one provokes;
    // the watched pair is redrawn here, from the rows patched above, on the
    // panel the selection was made on.
    if (succeeded > 0 && (action === "watched" || action === "unwatched")) {
      if (this._activePanel === "archived") this.renderArchivedJobs();
      else this.renderJobs();
    }

    if (failed === 0) {
      const verbs = { delete: "Deleted", cancel: "Cancelled", resume: "Resumed", reinitialize: "Reinitialized", watched: "Marked watched", unwatched: "Marked unwatched" };
      const verb = verbs[action] || action;
      this.showToast(`${verb}: ${succeeded} job${succeeded !== 1 ? "s" : ""}`, "success");
    } else {
      this.showToast(`${succeeded} of ${targets.length} succeeded. ${failed} failed.`, "warning");
    }
  }

  // Both re-login warnings land here. The remedy differs by WHO IS ASKING and
  // then by the install: the interactive browser login opens a window on the
  // host and its endpoints are loopback-gated, while the import works from
  // wherever the dashboard is being read. reloginPromptTarget decides; a status
  // that cannot be read falls back to the import panel, which holds both
  // controls.
  //
  // The auto-status fetch is on the CLICK rather than kept fresh in the
  // background: this is a rare, deliberate gesture, and one round trip on it
  // costs nothing while a cached answer could be a browser installed or removed
  // ago.
  async answerReloginPrompt(action) {
    if (action !== "yt-relogin" && action !== "tw-relogin") return;
    const platform = action === "tw-relogin" ? "twitch" : "youtube";
    let status = null;
    try {
      const response = await fetch("/api/cookies/auto-status");
      if (response.ok) status = await response.json();
    } catch {
      // Leave status null — reloginPromptTarget answers "import", which is the
      // safe direction.
    }
    if (reloginPromptTarget(status, window.location.hostname) === "wizard") {
      this.settings.startAutoCookieSetup(platform);
      return;
    }
    this.settings.openCookieImport();
  }
}

// Intercept fetch to detect 401 (session expired) and redirect to login
const _originalFetch = window.fetch;
let _reloading = false;
window.fetch = async function (...args) {
  const response = await _originalFetch.apply(this, args);
  if (response.status === 401 && !_reloading) {
    // Don't redirect for auth endpoints themselves. Extract the pathname
    // in a way that handles string URLs, Request objects, and URL
    // instances — previously fetch(new Request(...)) left url === "" and
    // triggered a reload loop because the empty string passed the
    // !url.includes("/api/auth/") guard.
    let pathname = "";
    try {
      const input = args[0];
      const rawUrl = typeof input === "string"
        ? input
        : (input instanceof URL ? input.href : input?.url || "");
      if (rawUrl) pathname = new URL(rawUrl, window.location.origin).pathname;
    } catch { /* malformed url — fall through to reload */ }
    if (!pathname.startsWith("/api/auth/")) {
      // Session expired — reload to get login page
      _reloading = true;
      window.location.reload();
    }
  }
  return response;
};

// Initialize app when DOM is ready
document.addEventListener("DOMContentLoaded", () => {
  window.app = new MoomboxApp();
});

// Tab-hide animation pause — Audit reports/frontend-html-css.md CRIT-7,
// PERF-1, PERF-2. When the tab is hidden, the browser should suspend any
// CSS animation / requestAnimationFrame loop that's currently driving the
// in-progress UI. Toggling a `data-paused` attribute on <body> lets CSS
// rules (e.g. `[data-paused="true"] .progress-bar { animation-play-state:
// paused; }`) suspend animations declaratively without touching JS state.
document.addEventListener("visibilitychange", () => {
  document.body.setAttribute("data-paused", document.hidden ? "true" : "false");
  // The 1 Hz UI tick early-returns while hidden; refresh immediately on
  // return so relative timestamps / countdowns never show stale strings.
  if (!document.hidden && window.app?._initialized) {
    // First: the cards the skipped progress frames changed, so the
    // timestamp refresh below runs over the cards that stay.
    window.app._repaintAfterHidden();
    window.app.stats.onTabVisible();
    window.app.updateCheckCountdown();
    window.app.refreshRelativeTimestamps();
  }
});
