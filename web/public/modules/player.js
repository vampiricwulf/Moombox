/**
 * Player Controller — Video player + chat replay
 */
import { formatMsToTime } from "./utils.js";
import { SegmentPlayer } from "./segments.js";

export class PlayerController {
  constructor(app) {
    this.app = app;
    this.playerJob = null;
    this.playerChatData = null;
    this.playerChatMessages = [];
    this.playerAutoScroll = true;
    this.playerScrollLock = false;
    this.playerActiveChatIndex = 0;
    this.nicoEnabled = true;
    this.nicoLaneCount = 15;
    this.nicoLaneAvail = [];
    this.nicoLastSpawnMs = null;
    this.playerCustomOffsetMs = 0;
    this.playerInitialized = false;
    /** @type {Map<string, string>} code → URL for 3rd-party Twitch emotes */
    this.twitchEmoteMap = new Map();

    // Multi-segment playback
    this._seg = new SegmentPlayer();
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

    // Nico toggle
    nicoToggle.addEventListener("sl-change", () => {
      this.nicoEnabled = nicoToggle.checked;
      localStorage.setItem("player-nico-toggle", nicoToggle.checked);
      const overlay = document.getElementById("player-nico-overlay");
      overlay.style.display = this.nicoEnabled ? "" : "none";
      if (!this.nicoEnabled) {
        this.clearNicoOverlay();
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
      this.clearNicoOverlay();
      this.nicoLastSpawnMs = currentMs + this.playerCustomOffsetMs;
    });

    // Pause/play nico animations
    video.addEventListener("pause", () => {
      document.querySelectorAll(".nico-message").forEach((el) => {
        if (el._nicoAnim) el._nicoAnim.pause();
      });
    });

    video.addEventListener("play", () => {
      document.querySelectorAll(".nico-message").forEach((el) => {
        if (el._nicoAnim) el._nicoAnim.play();
      });
    });

    // Multi-segment: auto-advance to next segment when current one ends
    video.addEventListener("ended", () => this.onSegmentEnded());

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
        this.clearNicoOverlay();
        this.nicoLastSpawnMs = currentMs + this.playerCustomOffsetMs;
        if (this.playerAutoScroll && !this.playerScrollLock) {
          this.syncSidebarToTime();
        }
      });

      const persistOffset = () => {
        if (!this.playerJob) return;
        const val = parseFloat(offsetInput.value);
        const videoID = this.playerJob.videoId;
        if (isNaN(val) || val === 0) {
          // Remove custom offset
          this.playerCustomOffsetMs = 0;
          fetch(`/api/player-prefs/${encodeURIComponent(videoID)}`, { method: "DELETE" }).catch(() => {});
        } else {
          fetch(`/api/player-prefs/${encodeURIComponent(videoID)}`, {
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
    }

    // Keyboard controls for player
    this.setupKeyboardControls();
  }

  setupKeyboardControls() {
    document.addEventListener("keydown", (e) => {
      // Only active when player tab is shown
      const playerPanel = document.querySelector('sl-tab-panel[name="player"]');
      if (!playerPanel || !playerPanel.hasAttribute("active")) return;

      // Skip when typing in inputs
      const tag = e.target.tagName;
      if (["INPUT", "TEXTAREA", "SL-INPUT", "SL-TEXTAREA", "SL-SELECT"].includes(tag)) return;

      const video = document.getElementById("player-video");
      if (!video || !video.src) return;

      switch (e.key) {
        case " ":
          if (video.paused) video.play(); else video.pause();
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
            this.seekToGlobalTime(Math.min(this._seg.totalDuration, globalSec));
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
    });
  }

  filterChat(query) {
    const container = document.getElementById("player-sidebar-messages");
    if (!container) return;
    const children = container.children;
    const needle = query.trim().toLowerCase();

    if (!needle) {
      // Clear search — restore all messages
      for (let i = 0; i < children.length; i++) {
        children[i].classList.remove("search-hidden");
      }
      this.playerAutoScroll = true;
      this.playerScrollLock = false;
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
    this.playerJob = null;
    this.playerChatData = null;
    this.playerChatMessages = [];
    this.twitchEmoteMap = new Map();
    this.playerActiveChatIndex = 0;
    this.nicoLastSpawnMs = null;
    this.playerCustomOffsetMs = 0;
    const offsetInput = document.getElementById("player-chat-offset");
    if (offsetInput) offsetInput.value = "";

    // Reset multi-segment state
    this._seg.reset();

    const video = document.getElementById("player-video");
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
    this._seg.onSegmentEnded(video);
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
      const block = document.createElement("div");
      block.className = "segment-indicator-block";
      block.style.width = `${pct}%`;
      block.style.background = colors[i % colors.length];
      block.title = `Segment ${i}: ${seg.quality} (${Math.round(seg.durationSeconds || 0)}s)`;
      block.textContent = seg.quality || `Seg ${i}`;
      block.addEventListener("click", () => {
        this.seekToGlobalTime(seg.startOffset);
      });
      indicator.appendChild(block);
    });

    // Insert after video element
    const videoWrapper = document.getElementById("player-video-wrapper");
    if (videoWrapper) {
      videoWrapper.parentNode.insertBefore(indicator, videoWrapper.nextSibling);
    }
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

      // Remove existing options
      select.querySelectorAll("sl-option").forEach((o) => o.remove());

      all.forEach((job) => {
        const opt = document.createElement("sl-option");
        opt.value = job.id;
        const noChat = !job.chatFilename ? " (no chat)" : "";
        opt.textContent = `${job.title} — ${job.channelName}${noChat}`;
        select.appendChild(opt);
      });

      // Restore selection if still valid — defer to let Shoelace register new options
      const restoreAndUpdateEmpty = () => {
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
      };
      if (select.updateComplete) {
        select.updateComplete.then(restoreAndUpdateEmpty);
      } else {
        requestAnimationFrame(restoreAndUpdateEmpty);
      }
    } catch (e) {
      console.error("Failed to load player job list:", e);
    }
  }

  async onPlayerJobSelect(jobId) {
    const video = document.getElementById("player-video");
    const nicoToggle = document.getElementById("player-nico-toggle");
    const sidebarToggle = document.getElementById("player-sidebar-toggle");

    // Fetch job details
    try {
      const res = await fetch(`/api/jobs/${jobId}`);
      if (!res.ok) return;
      this.playerJob = await res.json();
    } catch (e) {
      console.error("Failed to fetch job:", e);
      return;
    }

    // Show viewport, hide empty state
    document.getElementById("player-viewport").style.display = "";
    document.getElementById("player-empty-state").style.display = "none";

    // Reset multi-segment state
    this._seg.reset();
    const segIndicator = document.getElementById("player-segment-indicator");
    if (segIndicator) segIndicator.remove();

    // Multi-segment or single-file video source
    if (this.playerJob.segments && this.playerJob.segments.length > 0) {
      this.initMultiSegmentPlayer(jobId, this.playerJob.segments);
    } else {
      video.src = `/api/jobs/${jobId}/video`;
    }

    // Load chat if available
    this.playerChatMessages = [];
    this.playerChatData = null;
    this.twitchEmoteMap = new Map();
    this.playerActiveChatIndex = 0;
    this.nicoLastSpawnMs = null;

    if (this.playerJob.chatFilename) {
      try {
        const chatRes = await fetch(`/api/jobs/${jobId}/chat`);
        if (chatRes.ok) {
          this.playerChatData = await chatRes.json();
          // Compute chat-to-video timing correction. Chat offsets are relative to
          // streamStartTime in the chat file, but video playback starts from the
          // first captured content. If the stream started late (actual > scheduled),
          // chat offsets will be inflated and need correction.
          let chatBiasMs = 0;
          const chatStartMs = this.playerChatData.streamStartTime
            ? Date.parse(this.playerChatData.streamStartTime)
            : 0;
          if (chatStartMs > 0) {
            if (this.playerJob.segments && this.playerJob.segments.length > 0) {
              // Multi-segment: use first segment's capture time as video epoch
              const firstSegStart = this.playerJob.segments[0].unixStart;
              if (firstSegStart > 0) {
                chatBiasMs = firstSegStart * 1000 - chatStartMs;
              }
            } else if (this.playerJob.streamStartTime) {
              // Single-file: use job's stream start time (updated to actual on live)
              const jobStartMs = Date.parse(this.playerJob.streamStartTime);
              if (jobStartMs > 0 && jobStartMs !== chatStartMs) {
                chatBiasMs = jobStartMs - chatStartMs;
              }
            }
          }

          this.playerChatMessages = (this.playerChatData.messages || [])
            .map((m) => ({
              ...m,
              offsetMs: (m.offsetMs || 0) - chatBiasMs,
            }))
            .sort((a, b) => a.offsetMs - b.offsetMs);

          // Build 3rd-party emote lookup map for Twitch chat
          // Priority (Chatterino order): FFZ > BTTV > 7TV — add lowest first so higher overwrites
          if (this.playerChatData.emotes) {
            const { bttv, ffz, seventv } = this.playerChatData.emotes;
            for (const e of seventv || []) this.twitchEmoteMap.set(e.code, e.url);
            for (const e of bttv || []) this.twitchEmoteMap.set(e.code, e.url);
            for (const e of ffz || []) this.twitchEmoteMap.set(e.code, e.url);
          }
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

    // Build sidebar chat
    this.buildSidebarChat();
    this.clearNicoOverlay();

    // Update sidebar header
    document.getElementById("player-sidebar-msg-count").textContent =
      `${this.playerChatMessages.length} messages`;

    // Load saved custom chat offset
    const offsetInput = document.getElementById("player-chat-offset");
    if (offsetInput) {
      offsetInput.value = "";
      this.playerCustomOffsetMs = 0;
      if (this.playerJob.videoId) {
        try {
          const prefRes = await fetch(`/api/player-prefs/${encodeURIComponent(this.playerJob.videoId)}`);
          if (prefRes.ok) {
            const pref = await prefRes.json();
            if (pref.chatOffset && pref.chatOffset !== 0) {
              offsetInput.value = pref.chatOffset;
              this.playerCustomOffsetMs = pref.chatOffset * 1000;
            }
          }
        } catch (e) {
          // Ignore — no saved offset
        }
      }
    }
  }

  buildSidebarChat() {
    const container = document.getElementById("player-sidebar-messages");
    container.innerHTML = "";

    if (this.playerChatMessages.length === 0) return;

    const frag = document.createDocumentFragment();

    this.playerChatMessages.forEach((msg) => {
      const div = document.createElement("div");
      div.className = "chat-msg future";
      div.dataset.offset = msg.offsetMs;

      if (msg.superchat) {
        div.classList.add("superchat");
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

      frag.appendChild(div);
    });

    container.appendChild(frag);
    this.playerActiveChatIndex = 0;
  }

  onPlayerTimeUpdate() {
    const video = document.getElementById("player-video");
    if (!video || !this.playerChatMessages.length) return;

    const currentMs = this.getGlobalTimeMs();

    // Update sidebar active state
    this.updateSidebarActiveState(currentMs);

    // Spawn nico messages
    if (this.nicoEnabled) {
      this.spawnNicoMessages(currentMs);
    }

    // Auto-scroll sidebar
    if (this.playerAutoScroll && !this.playerScrollLock) {
      this.syncSidebarToTime();
    }
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
    if (!container || this.playerActiveChatIndex === 0) return;

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

    this.playerActiveChatIndex = 0;

    for (let i = 0; i < this.playerChatMessages.length; i++) {
      const child = children[i];
      if (!child) continue;

      if (this.playerChatMessages[i].offsetMs <= currentMs + this.playerCustomOffsetMs) {
        child.classList.remove("future");
        child.classList.add("active");
        this.playerActiveChatIndex = i + 1;
      } else {
        child.classList.remove("active");
        child.classList.add("future");
      }
    }
  }

  // Niconico overlay engine

  clearNicoOverlay() {
    const overlay = document.getElementById("player-nico-overlay");
    if (overlay) {
      // Cancel running animations to free CPU/memory before detaching elements
      overlay.querySelectorAll(".nico-message").forEach((el) => {
        if (el._nicoAnim) { el._nicoAnim.cancel(); el._nicoAnim = null; }
      });
      overlay.innerHTML = "";
    }
    this.nicoLaneAvail = new Array(this.nicoLaneCount).fill(0);
  }

  spawnNicoMessages(currentMs) {
    // On first call, set cursor to -5001 so messages within 5s before stream
    // start (offsetMs >= -5000) deploy instantly. Older pre-stream messages
    // are permanently skipped from the overlay (still visible in sidebar).
    const messages = this.playerChatMessages;
    if (!messages.length) return;

    // Use offset-adjusted time consistently for binary search, loop, and cursor
    const effectiveMs = currentMs + this.playerCustomOffsetMs;

    // First-spawn: deploy pre-stream messages within 5s of effective start.
    // Use null sentinel so negative effectiveMs doesn't re-trigger every frame.
    const firstSpawn = this.nicoLastSpawnMs === null;
    if (firstSpawn) {
      this.nicoLastSpawnMs = effectiveMs - 5001;
    }

    // Binary search for start of window (nicoLastSpawnMs, effectiveMs]
    let lo = 0;
    let hi = messages.length;
    while (lo < hi) {
      const mid = (lo + hi) >>> 1;
      if (messages[mid].offsetMs <= this.nicoLastSpawnMs) {
        lo = mid + 1;
      } else {
        hi = mid;
      }
    }

    const overlay = document.getElementById("player-nico-overlay");
    if (!overlay) return;

    const overlayWidth = overlay.clientWidth;
    const overlayHeight = overlay.clientHeight;
    if (!overlayWidth || !overlayHeight) return;

    const laneHeight = overlayHeight / this.nicoLaneCount;
    const now = performance.now();
    const duration = 8000;

    let spawned = 0;
    // No limit on first spawn so all offsetMs=0 pre-stream messages deploy at once
    const maxPerFrame = firstSpawn ? Infinity : 10;

    for (let i = lo; i < messages.length && messages[i].offsetMs <= effectiveMs; i++) {
      if (spawned >= maxPerFrame) break;

      const msg = messages[i];
      // Create element off-screen to measure its actual height
      const el = document.createElement("div");
      el.className = "nico-message";
      this.appendChatContent(el, msg.message || [], msg.emotes);
      if (!el.hasChildNodes()) continue;
      el.style.top = "0";
      el.style.left = `${overlayWidth}px`;
      overlay.appendChild(el);

      // Measure actual dimensions (emotes can make height > laneHeight)
      const msgWidth = el.offsetWidth;
      const msgHeight = el.offsetHeight;
      const lanesNeeded = Math.max(1, Math.ceil(msgHeight / laneHeight));

      // Find a run of consecutive available lanes
      let lane = -1;
      for (let l = 0; l <= this.nicoLaneCount - lanesNeeded; l++) {
        let allFree = true;
        for (let k = 0; k < lanesNeeded; k++) {
          if (this.nicoLaneAvail[l + k] > now) {
            allFree = false;
            break;
          }
        }
        if (allFree) {
          lane = l;
          break;
        }
      }
      if (lane === -1) {
        el.remove();
        continue; // All lanes busy
      }

      // Position at the chosen lane
      el.style.top = `${lane * laneHeight}px`;

      const totalTravel = overlayWidth + msgWidth;

      // Animate using Web Animations API
      const anim = el.animate(
        [
          { transform: "translateX(0)" },
          { transform: `translateX(-${totalTravel}px)` },
        ],
        { duration, fill: "forwards" },
      );
      el._nicoAnim = anim;

      // If video is paused, pause animation immediately
      const video = document.getElementById("player-video");
      if (video && video.paused) {
        anim.pause();
      }

      anim.onfinish = () => el.remove();

      // Calculate when these lanes become available (when the message clears the right edge)
      const clearTime = (msgWidth / totalTravel) * duration;
      const availAt = now + clearTime + 200; // 200ms buffer
      for (let k = 0; k < lanesNeeded; k++) {
        this.nicoLaneAvail[lane + k] = availAt;
      }

      spawned++;
    }

    this.nicoLastSpawnMs = effectiveMs;
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
