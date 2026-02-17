/**
 * Player Controller — Video player + chat replay
 */
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
    this.nicoLastSpawnMs = -1;
    this.playerInitialized = false;
  }

  initPlayer() {
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
        const currentMs = video.currentTime * 1000;
        this.resetSidebarToTime(currentMs);
        this.syncSidebarToTime();
      }
    });

    // Video timeupdate
    video.addEventListener("timeupdate", () => this.onPlayerTimeUpdate());

    // Video seeked — reset both systems
    video.addEventListener("seeked", () => {
      const currentMs = video.currentTime * 1000;
      this.resetSidebarToTime(currentMs);
      this.clearNicoOverlay();
      this.nicoLastSpawnMs = currentMs;
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
  }

  clearPlayer() {
    this.playerJob = null;
    this.playerChatData = null;
    this.playerChatMessages = [];
    this.playerActiveChatIndex = 0;
    this.nicoLastSpawnMs = -1;

    const video = document.getElementById("player-video");
    video.removeAttribute("src");
    video.load();

    document.getElementById("player-viewport").style.display = "none";
    document.getElementById("player-empty-state").style.display = "";
    document.getElementById("player-sidebar-messages").innerHTML = "";
    this.clearNicoOverlay();
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

      // Restore selection if still valid
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

    // Set video source
    video.src = `/api/jobs/${jobId}/video`;

    // Load chat if available
    this.playerChatMessages = [];
    this.playerChatData = null;
    this.playerActiveChatIndex = 0;
    this.nicoLastSpawnMs = -1;

    if (this.playerJob.chatFilename) {
      try {
        const chatRes = await fetch(`/api/jobs/${jobId}/chat`);
        if (chatRes.ok) {
          this.playerChatData = await chatRes.json();
          this.playerChatMessages = (this.playerChatData.messages || [])
            .map((m) => ({
              ...m,
              offsetMs: m.offsetMs || 0,
            }))
            .sort((a, b) => a.offsetMs - b.offsetMs);
        }
      } catch (e) {
        console.error("Failed to load chat:", e);
      }

      // Enable chat toggles
      nicoToggle.disabled = false;
      sidebarToggle.disabled = false;
    } else {
      // No chat — disable toggles
      nicoToggle.disabled = true;
      sidebarToggle.disabled = true;
      sidebarToggle.checked = false;
      document.getElementById("player-sidebar").style.display = "none";
    }

    // Build sidebar chat
    this.buildSidebarChat();
    this.clearNicoOverlay();

    // Update sidebar header
    document.getElementById("player-sidebar-header").textContent =
      `${this.playerChatMessages.length} messages`;
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
      timeSpan.textContent = this.formatMsToTime(msg.offsetMs);
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

      // Message content
      const contentSpan = document.createElement("span");
      contentSpan.innerHTML = this.renderChatMessageParts(msg.message || []);
      div.appendChild(contentSpan);

      frag.appendChild(div);
    });

    container.appendChild(frag);
    this.playerActiveChatIndex = 0;
  }

  onPlayerTimeUpdate() {
    const video = document.getElementById("player-video");
    if (!video || !this.playerChatMessages.length) return;

    const currentMs = video.currentTime * 1000;

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
      this.playerChatMessages[this.playerActiveChatIndex].offsetMs <= currentMs
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

      if (this.playerChatMessages[i].offsetMs <= currentMs) {
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
    if (overlay) overlay.innerHTML = "";
    this.nicoLaneAvail = new Array(this.nicoLaneCount).fill(0);
  }

  spawnNicoMessages(currentMs) {
    // On first call, set cursor to -5001 so messages within 5s before stream
    // start (offsetMs >= -5000) deploy instantly. Older pre-stream messages
    // are permanently skipped from the overlay (still visible in sidebar).
    const firstSpawn = this.nicoLastSpawnMs < 0;
    if (firstSpawn) {
      this.nicoLastSpawnMs = -5001;
    }

    const messages = this.playerChatMessages;
    if (!messages.length) return;

    // Binary search for start of window (nicoLastSpawnMs, currentMs]
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

    for (let i = lo; i < messages.length && messages[i].offsetMs <= currentMs; i++) {
      if (spawned >= maxPerFrame) break;

      const msg = messages[i];
      const html = this.renderChatMessageParts(msg.message || []);
      if (!html) continue;

      // Create element off-screen to measure its actual height
      const el = document.createElement("div");
      el.className = "nico-message";
      el.innerHTML = html;
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

    this.nicoLastSpawnMs = currentMs;
  }

  renderChatMessageParts(parts) {
    // Twitch chat stores message as a plain string; YouTube uses MessagePart[]
    if (typeof parts === "string") {
      return this.app.escapeHtml(parts);
    }
    if (!Array.isArray(parts)) return "";
    return parts
      .map((part) => {
        if (part.type === "emoji" && part.emojiUrl) {
          const alt = part.text || part.emojiId || "";
          const url = part.emojiUrl.replace(/=[^/]*$/, "");
          return `<img class="chat-emoji" src="${this.app.escapeHtml(url)}" alt="${this.app.escapeHtml(alt)}" loading="lazy" referrerpolicy="no-referrer">`;
        }
        return this.app.escapeHtml(part.text || "");
      })
      .join("");
  }

  formatMsToTime(ms) {
    const negative = ms < 0;
    const absTotalSec = Math.floor(Math.abs(ms) / 1000);
    const h = Math.floor(absTotalSec / 3600);
    const m = Math.floor((absTotalSec % 3600) / 60);
    const s = absTotalSec % 60;

    const prefix = negative ? "-" : "";
    if (h > 0) {
      return `${prefix}${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
    }
    return `${prefix}${m}:${String(s).padStart(2, "0")}`;
  }
}
