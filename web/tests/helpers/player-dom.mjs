/**
 * jsdom harness for web/public/modules/player.js.
 *
 * The player is the only frontend module with enough behaviour (a selection
 * race, a chunked build, an overlay engine, a settle timer, region promotion)
 * that a DOM is cheaper than reasoning. This file gives it one, and NOTHING
 * else: every stub below exists because player.js (or a module it imports)
 * actually calls it — each carries the one-line reason.
 *
 * Design rules:
 * - The markup is LIFTED from web/public/index.html at load time, so a change
 *   to the player panel is picked up here instead of drifting out of sync.
 * - Nothing in web/public/ is modified or monkey-patched. The seams are the
 *   browser APIs jsdom does not implement, plus layout (jsdom has none).
 * - Time is manual: `advance(ms)` drives setTimeout/setInterval, `flushRaf()`
 *   drives requestAnimationFrame. No test may depend on wall-clock time.
 * - `import("jsdom")` at the top means this module must not be imported when
 *   jsdom is absent; player.test.mjs probes for jsdom first and skips.
 */
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { JSDOM } from "jsdom";
import { PlayerController } from "../../public/modules/player.js";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const INDEX_HTML = path.join(HERE, "..", "..", "public", "index.html");
const PANEL_OPEN = '<sl-tab-panel name="player">';
const PANEL_CLOSE = "</sl-tab-panel>";

/** Real timer/frame globals, captured before any test replaces them. */
const REAL = {
  setTimeout: globalThis.setTimeout,
  clearTimeout: globalThis.clearTimeout,
  setInterval: globalThis.setInterval,
  clearInterval: globalThis.clearInterval,
  requestAnimationFrame: globalThis.requestAnimationFrame,
  cancelAnimationFrame: globalThis.cancelAnimationFrame,
};

/** Windows handed out by makePlayer, so the suite can close them at the end. */
const openWindows = [];

/**
 * The player panel markup, verbatim from index.html, with `active` added to the
 * tab panel (the keyboard handler refuses to run without it — the real app gets
 * the attribute from Shoelace's tab group, which is not loaded here).
 */
export function playerPanelMarkup() {
  const html = fs.readFileSync(INDEX_HTML, "utf8");
  const start = html.indexOf(PANEL_OPEN);
  const end = start < 0 ? -1 : html.indexOf(PANEL_CLOSE, start);
  if (start < 0 || end < 0) {
    throw new Error(`player panel markup not found in ${INDEX_HTML} (looked for ${PANEL_OPEN})`);
  }
  return html
    .slice(start, end + PANEL_CLOSE.length)
    .replace(PANEL_OPEN, '<sl-tab-panel name="player" active>');
}

// ── Fake HTTP ───────────────────────────────────────────────────────────────

/** Tag a handler result as a full response instead of a plain JSON body. */
export function response({ status = 200, body = null, bodyPromise } = {}) {
  return { __response: true, status, body, bodyPromise };
}

function toResponse(spec) {
  const r = spec && spec.__response ? spec : response({ status: 200, body: spec });
  return {
    ok: r.status >= 200 && r.status < 300,
    status: r.status,
    json: () => (r.bodyPromise ? r.bodyPromise : Promise.resolve(r.body)),
    text: () => Promise.resolve(JSON.stringify(r.body ?? null)),
  };
}

function matchRoute(routes, method, pathname) {
  const segs = pathname.split("/").filter(Boolean);
  // Literal routes win over parameterised ones regardless of insertion order,
  // so a test can register "GET /api/jobs/A" after the default "/api/jobs/:id".
  for (const wantLiteral of [true, false]) {
    for (const [key, handler] of routes) {
      const sp = key.indexOf(" ");
      const m = key.slice(0, sp);
      const pat = key.slice(sp + 1);
      if (m !== method) continue;
      const p = pat.split("/").filter(Boolean);
      if (p.length !== segs.length) continue;
      const isLiteral = !p.some((s) => s.startsWith(":"));
      if (isLiteral !== wantLiteral) continue;
      const params = {};
      let ok = true;
      for (let i = 0; i < p.length; i++) {
        if (p[i].startsWith(":")) params[p[i].slice(1)] = segs[i];
        else if (p[i] !== segs[i]) { ok = false; break; }
      }
      if (ok) return { handler, params };
    }
  }
  return null;
}

function makeHttp() {
  const routes = new Map();
  const calls = [];
  const http = {
    routes,
    calls,
    /** Register/replace a route: on("GET /api/jobs/:id", ({params}) => body). */
    on(key, handler) { routes.set(key, typeof handler === "function" ? handler : () => handler); return http; },
    /** Calls whose url contains `needle` (and optionally match `method`). */
    matching(needle, method) {
      return calls.filter((c) => c.url.includes(needle) && (!method || c.method === method));
    },
    /**
     * Install a route whose HEADERS resolve at once but whose BODY resolves only
     * when the returned handle is resolved — the shape the selection race needs.
     */
    deferBody(key) {
      let resolve;
      const bodyPromise = new Promise((r) => { resolve = r; });
      http.on(key, () => response({ status: 200, bodyPromise }));
      return { resolve, promise: bodyPromise };
    },
    fetch(input, init = {}) {
      const url = String(input);
      const method = (init.method || "GET").toUpperCase();
      const pathname = url.startsWith("http") ? new URL(url).pathname : url.split("?")[0];
      let body;
      if (typeof init.body === "string") { try { body = JSON.parse(init.body); } catch { body = init.body; } }
      calls.push({ method, url: pathname, body, headers: init.headers });
      const hit = matchRoute(routes, method, pathname);
      if (!hit) return Promise.resolve(toResponse(response({ status: 404 })));
      return Promise.resolve(hit.handler({ params: hit.params, method, url: pathname, body }))
        .then(toResponse);
    },
  };
  return http;
}

// ── Manual clock ────────────────────────────────────────────────────────────

function makeClock() {
  let now = 0;
  let seq = 0;
  const timers = new Map();
  const api = {
    get now() { return now; },
    setTimeout(fn, delay = 0, ...args) {
      const id = ++seq;
      timers.set(id, { time: now + Math.max(0, Number(delay) || 0), fn, args, every: 0, id });
      return id;
    },
    setInterval(fn, delay = 0, ...args) {
      const id = ++seq;
      const every = Math.max(1, Number(delay) || 1);
      timers.set(id, { time: now + every, fn, args, every, id });
      return id;
    },
    clear(id) { if (id != null) timers.delete(id); },
    /**
     * Move the clock forward `ms`, firing every timer that comes due — including
     * ones scheduled by the callbacks themselves (a setTimeout(0) chain, which
     * is how buildSidebarChat appends its chunks). advance(0) therefore drains
     * the whole immediate chain.
     */
    advance(ms = 0) {
      const target = now + Math.max(0, Number(ms) || 0);
      for (let guard = 0; ; guard++) {
        if (guard > 100000) throw new Error("clock.advance: runaway timer chain");
        let next = null;
        for (const t of timers.values()) {
          if (t.time <= target && (next === null || t.time < next.time || (t.time === next.time && t.id < next.id))) next = t;
        }
        if (!next) break;
        now = next.time;
        if (next.every) next.time = now + next.every;
        else timers.delete(next.id);
        next.fn(...next.args);
      }
      now = target;
    },
  };
  return api;
}

// ── Element stubs ───────────────────────────────────────────────────────────

/**
 * Shoelace stand-ins. We test our logic, not Shoelace's: each is a plain
 * HTMLElement that exposes only the properties player.js reads or writes, with
 * attribute fallbacks so markup like `<sl-checkbox checked>` behaves.
 */
function defineShoelaceStubs(window) {
  const { HTMLElement } = window;

  class SlSelect extends HTMLElement {
    // `value`/`open` — read by the sl-change handler and by isTypingInInput.
    get value() { return this._value ?? this.getAttribute("value") ?? ""; }
    set value(v) { this._value = v == null ? "" : String(v); }
    get open() { return this._open ?? this.hasAttribute("open"); }
    set open(v) { this._open = !!v; }
    // `updateComplete` — awaited by loadPlayerJobList before restoring value.
    get updateComplete() { return this._updateComplete ?? Promise.resolve(); }
    set updateComplete(p) { this._updateComplete = p; }
  }
  class SlOption extends HTMLElement {
    get value() { return this._value ?? this.getAttribute("value") ?? ""; }
    set value(v) { this._value = v == null ? "" : String(v); }
  }
  class SlCheckbox extends HTMLElement {
    // `checked` — the overlay/sidebar toggles.
    get checked() { return this._checked ?? this.hasAttribute("checked"); }
    set checked(v) { this._checked = !!v; }
  }
  class SlInput extends HTMLElement {
    // `value` — the chat-search box.
    get value() { return this._value ?? this.getAttribute("value") ?? ""; }
    set value(v) { this._value = v == null ? "" : String(v); }
  }
  // No behaviour of their own; they exist so the markup parses into real
  // elements with the right tagName (the Space guard tests tagName).
  class Passive extends HTMLElement {}

  const defs = {
    "sl-select": SlSelect, "sl-option": SlOption, "sl-checkbox": SlCheckbox, "sl-input": SlInput,
    "sl-button": class extends Passive {}, "sl-icon-button": class extends Passive {},
    "sl-icon": class extends Passive {}, "sl-tab-panel": class extends Passive {},
    "sl-dialog": class extends Passive {}, "sl-switch": class extends Passive {},
  };
  for (const [tag, cls] of Object.entries(defs)) window.customElements.define(tag, cls);
}

/** Own-property accessor with a per-element backing store and a shared default. */
function defineBacked(proto, name, initial) {
  const store = new WeakMap();
  Object.defineProperty(proto, name, {
    configurable: true,
    get() { return store.has(this) ? store.get(this) : initial; },
    set(v) { store.set(this, v); },
  });
}

// ── The harness ─────────────────────────────────────────────────────────────

const DEFAULT_GEOM = {
  /** #player-nico-overlay's measured box. {w:0,h:0} models a hidden panel. */
  overlay: { w: 1280, h: 720 },
  /** #player-video's measured box; null = same as `overlay`. */
  video: null,
  /** videoWidth/videoHeight. 0 = metadata not in yet, so no letterbox math. */
  intrinsic: { w: 0, h: 0 },
  /** Height of one `.nico-message` — what the geometry probe measures. */
  rowH: 24,
  /** Width of one `.nico-message`, i.e. what the lane allocator is given. */
  msgW: 200,
  /** Height of one `.chat-msg` sidebar row; also drives its offsetTop. */
  chatRowH: 20,
  /** #player-sidebar-messages' clientHeight/width. */
  sidebarH: 400,
  sidebarW: 320,
};

const ZERO = { w: 0, h: 0, left: 0, top: 0 };

/**
 * Build a player against a fresh jsdom.
 *
 * @param {object} [opts]
 * @param {Array}  [opts.jobs]        GET /api/jobs
 * @param {Array}  [opts.archived]    GET /api/jobs/archived
 * @param {object} [opts.job]         GET /api/jobs/:id fallback (and registered under its own id)
 * @param {object} [opts.jobsById]    GET /api/jobs/:id per id
 * @param {object} [opts.watchState]  GET /api/jobs/:id/watch-state fallback
 * @param {object} [opts.watchStateById]
 * @param {object} [opts.chat]        GET /api/jobs/:id/chat fallback
 * @param {object} [opts.chatById]
 * @param {object} [opts.segmentChatById]  GET /api/jobs/:id/segments/:i/chat, keyed "id/i"
 * @param {object} [opts.geom]        layout overrides (see DEFAULT_GEOM)
 * @param {object} [opts.storage]     localStorage seed, applied BEFORE initPlayer
 * @param {boolean}[opts.reducedMotion] make the reduce-motion media query match
 */
export function makePlayer(opts = {}) {
  const geom = { ...DEFAULT_GEOM, ...(opts.geom || {}) };
  const dom = new JSDOM(`<!doctype html><html><body></body></html>`, {
    url: "http://localhost/",
    pretendToBeVisual: true, // document.hidden must be false; spawnNicoMessages bails otherwise
  });
  const { window } = dom;
  const { document } = window;
  openWindows.push(window);

  defineShoelaceStubs(window);
  document.body.innerHTML = playerPanelMarkup();

  // Layout. jsdom has none, so every box below is 0 — the overlay engine, the
  // geometry probe and the sidebar scroll all read boxes. One measure() keyed on
  // id/class is the single seam; `geom` is the knob tests turn.
  const isHidden = (el) => {
    for (let n = el; n && n.nodeType === 1; n = n.parentElement) {
      if (n.style && n.style.display === "none") return true;
    }
    return false;
  };
  const measure = (el) => {
    if (!el || el.nodeType !== 1 || isHidden(el)) return ZERO;
    if (el.id === "player-nico-overlay") return { ...geom.overlay, left: 0, top: 0 };
    if (el.id === "player-video") return { ...(geom.video || geom.overlay), left: 0, top: 0 };
    if (el.id === "player-sidebar-messages") return { w: geom.sidebarW, h: geom.sidebarH, left: 0, top: 0 };
    const cl = el.classList;
    if (cl && cl.contains("nico-message")) return { w: geom.msgW, h: geom.msgH ?? geom.rowH, left: 0, top: 0 };
    if (cl && cl.contains("chat-msg")) {
      const i = el.parentNode ? Array.prototype.indexOf.call(el.parentNode.children, el) : 0;
      return { w: geom.sidebarW, h: geom.chatRowH, left: 0, top: i * geom.chatRowH };
    }
    return ZERO;
  };
  for (const [name, key] of [["offsetWidth", "w"], ["clientWidth", "w"], ["offsetHeight", "h"],
                             ["clientHeight", "h"], ["offsetLeft", "left"], ["offsetTop", "top"]]) {
    Object.defineProperty(window.HTMLElement.prototype, name, {
      configurable: true,
      get() { return measure(this)[key]; },
    });
  }

  // Web Animations — jsdom has none. The overlay assigns currentTime/playbackRate
  // and installs onfinish AFTER creation, so all three must be plain properties.
  const anims = [];
  Object.defineProperty(window.Element.prototype, "animate", {
    configurable: true, writable: true,
    value(keyframes, options) {
      const anim = {
        el: this, keyframes, options, state: "running",
        currentTime: 0, playbackRate: 1, onfinish: null,
        pause() { this.state = "paused"; },
        play() { this.state = "running"; },
        cancel() { this.state = "cancelled"; },
        /** Test-only: run the finish callback the real engine would fire. */
        finish() { this.state = "finished"; if (this.onfinish) this.onfinish(); },
      };
      anims.push(anim);
      return anim;
    },
  });

  // Media playback — jsdom's play/pause/load throw "not implemented", and every
  // state property below is either read-only or absent there.
  const mediaCalls = [];
  const mp = window.HTMLMediaElement.prototype;
  defineBacked(mp, "currentTime", 0);
  defineBacked(mp, "duration", NaN);   // NaN = metadata not in, like a real unloaded element
  // jsdom loads no media, so its readyState is permanently HAVE_NOTHING (0) —
  // which spawnNicoMessages reads as "no frame to sync to" and returns on.
  // Default to HAVE_ENOUGH_DATA so a tick means what the tests mean by it; a
  // test that wants the unloaded case sets `h.video.readyState = 0`.
  defineBacked(mp, "readyState", 4);
  defineBacked(mp, "paused", true);
  defineBacked(mp, "ended", false);
  defineBacked(mp, "playbackRate", 1);
  defineBacked(mp, "volume", 1);
  defineBacked(mp, "muted", false);
  Object.defineProperty(mp, "play", {
    configurable: true, writable: true,
    value() { mediaCalls.push("play"); this.paused = false; return Promise.resolve(); },
  });
  Object.defineProperty(mp, "pause", {
    configurable: true, writable: true,
    value() { mediaCalls.push("pause"); this.paused = true; },
  });
  Object.defineProperty(mp, "load", {
    configurable: true, writable: true, value() { mediaCalls.push("load"); },
  });
  for (const [name, key] of [["videoWidth", "w"], ["videoHeight", "h"]]) {
    Object.defineProperty(window.HTMLVideoElement.prototype, name, {
      configurable: true, get() { return geom.intrinsic[key]; },
    });
  }

  // Fullscreen — jsdom implements neither side of it.
  const fullscreenCalls = [];
  let fullscreenElement = null;
  Object.defineProperty(window.Element.prototype, "requestFullscreen", {
    configurable: true, writable: true,
    value() { fullscreenCalls.push(this); fullscreenElement = this; return Promise.resolve(); },
  });
  Object.defineProperty(document, "fullscreenElement", { configurable: true, get: () => fullscreenElement });
  Object.defineProperty(document, "exitFullscreen", {
    configurable: true, writable: true,
    value() { fullscreenCalls.push(null); fullscreenElement = null; return Promise.resolve(); },
  });

  // prefers-reduced-motion — read once in initPlayer to pick the overlay default.
  const media = { "(prefers-reduced-motion: reduce)": !!opts.reducedMotion };
  window.matchMedia = (q) => ({
    media: q, matches: !!media[q],
    addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {},
    onchange: null, dispatchEvent: () => false,
  });

  // ResizeObserver — jsdom has none; the player only ever constructs+observes.
  // Nothing drives it here: the geometry path it calls is exercised directly.
  window.ResizeObserver = class {
    constructor(cb) { this.cb = cb; }
    observe() {} unobserve() {} disconnect() {}
  };

  // sendBeacon — the beforeunload resume save. jsdom's Navigator has none.
  const beacons = [];
  Object.defineProperty(window.Navigator.prototype, "sendBeacon", {
    configurable: true, writable: true,
    value(url, data) { beacons.push({ url: String(url), data }); return true; },
  });

  const http = makeHttp();
  const clock = makeClock();

  // Manual frames. syncSidebarToTime clears its programmatic-scroll flag in a
  // rAF, and the resume dialog focuses in one; both must be test-driven.
  let rafSeq = 0;
  const rafQueue = new Map();
  const flushRaf = () => {
    const due = [...rafQueue.entries()];
    rafQueue.clear();
    for (const [, cb] of due) cb(clock.now);
  };

  // Default routes. A test replaces or adds to them with h.http.on(...).
  const jobs = opts.jobs || [];
  const archived = opts.archived || [];
  const jobsById = { ...(opts.jobsById || {}) };
  for (const j of [...jobs, ...archived]) if (j && j.id && !(j.id in jobsById)) jobsById[j.id] = j;
  if (opts.job && opts.job.id) jobsById[opts.job.id] ??= opts.job;
  http
    .on("GET /api/jobs", () => jobs)
    .on("GET /api/jobs/archived", () => archived)
    .on("GET /api/jobs/:id", ({ params }) => jobsById[params.id] ?? opts.job ?? response({ status: 404 }))
    .on("GET /api/jobs/:id/watch-state", ({ params }) =>
      (opts.watchStateById || {})[params.id] ?? opts.watchState ?? {})
    .on("GET /api/jobs/:id/chat", ({ params }) =>
      (opts.chatById || {})[params.id] ?? opts.chat ?? response({ status: 404 }))
    .on("GET /api/jobs/:id/segments/:i/chat", ({ params }) =>
      (opts.segmentChatById || {})[`${params.id}/${params.i}`] ?? response({ status: 404 }))
    .on("PUT /api/jobs/:id/chat-offset", () => ({}))
    .on("DELETE /api/jobs/:id/chat-offset", () => ({}))
    .on("PUT /api/jobs/:id/resume-position", () => ({}))
    .on("POST /api/jobs/:id/watched", () => ({}));

  installGlobals(window, { http, clock, rafQueue, nextRafId: () => ++rafSeq });

  for (const [k, v] of Object.entries(opts.storage || {})) window.localStorage.setItem(k, String(v));

  const app = {
    toasts: [],
    resumeUpdates: [],
    showToast(message, variant) { this.toasts.push({ message, variant }); },
    _updateJobResumePosition(jobId, position) { this.resumeUpdates.push({ jobId, position }); },
  };

  const player = new PlayerController(app);
  player.initPlayer();

  const video = document.getElementById("player-video");
  const el = (id) => document.getElementById(id);

  return {
    player, app, window, document, geom, http, clock, video,
    fetchLog: http.calls,
    anims, mediaCalls, beacons, fullscreenCalls,
    el,
    overlay: () => el("player-nico-overlay"),
    sidebar: () => el("player-sidebar-messages"),
    select: () => el("player-job-select"),
    /** Advance the manual clock; fires setTimeout/setInterval callbacks. */
    advance: (ms) => clock.advance(ms),
    flushRaf,
    /** Let every pending microtask/promise settle (real setImmediate, not the fake clock). */
    flush: () => new Promise((r) => REAL.setTimeout(r, 0)),
    /** Set the element's own currentTime (ms) and fire `timeupdate`. */
    tick(ms) {
      video.currentTime = ms / 1000;
      video.dispatchEvent(new window.Event("timeupdate"));
    },
    /**
     * A seek: set currentTime (ms) and fire `seeked` then `timeupdate`. Moving
     * BACKWARDS is only ever a seek in a real player — a bare timeupdate never
     * rewinds — so tests that go back in time must use this.
     */
    seek(ms) {
      video.currentTime = ms / 1000;
      video.dispatchEvent(new window.Event("seeked"));
      video.dispatchEvent(new window.Event("timeupdate"));
    },
    /** Dispatch a keydown, on `target` (default: document) with a composed path. */
    key(k, { target = document, ...init } = {}) {
      target.dispatchEvent(new window.KeyboardEvent("keydown", {
        key: k, bubbles: true, cancelable: true, composed: true, ...init,
      }));
    },
    /** Select a job and let its async chain settle. */
    async selectJob(id) {
      await player.onPlayerJobSelect(id);
      await new Promise((r) => REAL.setTimeout(r, 0));
    },
    close() { window.close(); },
  };
}

/**
 * Point the module-scope globals player.js reads at `window`. player.js and its
 * imports resolve `document`, `fetch`, `setTimeout`, `HTMLElement`, ... off the
 * Node global object, so a fresh jsdom has to be published there each time.
 */
function installGlobals(window, { http, clock, rafQueue, nextRafId }) {
  const set = (name, value) =>
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });

  // Exactly the globals player.js (and utils.js / segments.js) reach for.
  for (const name of [
    "window",           // "ResizeObserver" in window, window.matchMedia, beforeunload
    "document",
    "navigator",        // sendBeacon
    "localStorage",     // the two toggle preferences
    "HTMLElement",      // `target instanceof HTMLElement` in the Space guard + isTypingInInput
    "HTMLMediaElement", // HAVE_CURRENT_DATA, the readyState guard in spawnNicoMessages
    "Event",            // new Event("sl-change") from the C / S shortcuts
    "Blob",             // the beforeunload resume beacon
    "AbortController",  // the resume dialog and the cross-segment seek
    "ResizeObserver",   // the wrapper observer (stubbed above)
  ]) {
    const v = name === "window" ? window : window[name];
    if (v !== undefined) set(name, v);
  }
  set("fetch", (input, init) => http.fetch(input, init));
  set("setTimeout", (fn, delay, ...a) => clock.setTimeout(fn, delay, ...a));
  set("clearTimeout", (id) => clock.clear(id));
  set("setInterval", (fn, delay, ...a) => clock.setInterval(fn, delay, ...a));
  set("clearInterval", (id) => clock.clear(id));
  set("requestAnimationFrame", (cb) => { const id = nextRafId(); rafQueue.set(id, cb); return id; });
  set("cancelAnimationFrame", (id) => { rafQueue.delete(id); });
  // window.* aliases so code reached through `window` sees the same fakes.
  window.fetch = globalThis.fetch;
  window.setTimeout = globalThis.setTimeout;
  window.clearTimeout = globalThis.clearTimeout;
  window.setInterval = globalThis.setInterval;
  window.clearInterval = globalThis.clearInterval;
  window.requestAnimationFrame = globalThis.requestAnimationFrame;
  window.cancelAnimationFrame = globalThis.cancelAnimationFrame;
}

/** Restore the timer globals and close every jsdom window the suite opened. */
export function teardownAll() {
  for (const [name, fn] of Object.entries(REAL)) {
    if (fn !== undefined) Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: fn });
  }
  for (const w of openWindows.splice(0)) {
    try { w.close(); } catch { /* already closed */ }
  }
}
