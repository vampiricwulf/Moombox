/**
 * jsdom harness for web/public/app.js.
 *
 * app.js is the dashboard controller: one 5k-line class that renders every
 * panel. Arc I splits it into controllers, and a split is only safe if the
 * RENDERED OUTPUT is provably unchanged — so this harness exists to construct
 * a real MoomboxApp against the real index.html markup and let app.test.mjs
 * snapshot what it draws.
 *
 * It is the sibling of helpers/player-dom.mjs and follows its rules verbatim:
 * - The markup is LIFTED from web/public/index.html at load time, so a panel
 *   change is picked up here instead of drifting out of sync.
 * - Nothing in web/public/ is modified or monkey-patched. The seams are the
 *   browser APIs jsdom does not implement, plus layout (jsdom has none).
 *   EVERY stub below carries the one-line reason it exists.
 * - Time is manual: `advance(ms)` drives setTimeout/setInterval, `flushRaf()`
 *   drives requestAnimationFrame, and the wall clock is FROZEN (see NOW) so
 *   relative timestamps render the same string on every machine.
 * - `import("jsdom")` at the top means this module must not be imported when
 *   jsdom is absent; app.test.mjs probes for jsdom first and skips.
 *
 * The one thing this harness does that player-dom.mjs does not: app.js is a
 * TOP-LEVEL module with side effects (it wraps window.fetch and registers
 * DOMContentLoaded / visibilitychange listeners at import time). So it is
 * imported DYNAMICALLY, after the jsdom globals are published — and because
 * the document is already "complete" by then, DOMContentLoaded never fires
 * again and the harness constructs `new MoomboxApp()` itself.
 *
 * Those side effects run ONCE PER PROCESS, not once per test: the ES module
 * cache keeps the first evaluation, so app.js's `window.fetch = …` wrapper and
 * its two document listeners were installed against the FIRST makeApp's jsdom
 * window and are never re-installed for later ones. What makes later tests
 * work anyway is that the globals published below are accessors onto the
 * CURRENT window, so the one evaluated wrapper reaches whichever window is
 * live. Nothing here re-intercepts per test.
 */
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { JSDOM } from "jsdom";
// The fake-HTTP response tag is identical for both harnesses; the rest of
// player-dom.mjs is player-shaped, so its route matcher and clock are
// re-expressed below in the same style rather than exported across.
import { response } from "./player-dom.mjs";

export { response };

const HERE = path.dirname(fileURLToPath(import.meta.url));
const INDEX_HTML = path.join(HERE, "..", "..", "public", "index.html");

/**
 * The frozen wall clock. formatRelativeTime, the check countdown and the
 * uptime readout all derive strings from Date.now(); pinning it is what makes
 * a rendering snapshot reproducible on another machine, another day.
 * 2026-09-05T12:00:00Z.
 */
export const NOW = Date.UTC(2026, 8, 5, 12, 0, 0);

/** ISO timestamp `secs` seconds before the frozen NOW — for fixtures. */
export function agoISO(secs) {
  return new REAL.Date(NOW - secs * 1000).toISOString();
}

/** Real globals, captured before any test replaces them. */
const REAL = {
  setTimeout: globalThis.setTimeout,
  clearTimeout: globalThis.clearTimeout,
  setInterval: globalThis.setInterval,
  clearInterval: globalThis.clearInterval,
  requestAnimationFrame: globalThis.requestAnimationFrame,
  cancelAnimationFrame: globalThis.cancelAnimationFrame,
  Date: globalThis.Date,
};
const REAL_TO_LOCALE = {
  date: Date.prototype.toLocaleString,
  number: Number.prototype.toLocaleString,
};

/** Windows handed out by makeApp, so the suite can close them at the end. */
const openWindows = [];

/**
 * The whole <body> of index.html, minus its <script> tags: app.js reaches into
 * every panel (tasks, archived, files, history, logs, settings, dialogs), so
 * there is no useful fragment to slice — and jsdom must not try to run the
 * page's own module script, which is what we are importing by hand.
 */
export function indexBodyMarkup() {
  const html = fs.readFileSync(INDEX_HTML, "utf8");
  const start = html.indexOf("<body>");
  const end = html.indexOf("</body>");
  if (start < 0 || end < 0) throw new Error(`<body> not found in ${INDEX_HTML}`);
  return html.slice(start + "<body>".length, end).replace(/<script\b[\s\S]*?<\/script>/g, "");
}

// ── Fake HTTP ───────────────────────────────────────────────────────────────

function matchRoute(routes, method, pathname) {
  const segs = pathname.split("/").filter(Boolean);
  // Literal routes win over parameterised ones regardless of insertion order,
  // so a test can register "GET /api/jobs/A" after a default "/api/jobs/:id".
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

function toResponse(spec) {
  const r = spec && spec.__response ? spec : response({ status: 200, body: spec });
  return {
    ok: r.status >= 200 && r.status < 300,
    status: r.status,
    statusText: r.status === 200 ? "OK" : String(r.status),
    json: () => (r.bodyPromise ? r.bodyPromise : Promise.resolve(r.body)),
    text: () => Promise.resolve(JSON.stringify(r.body ?? null)),
  };
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
  return {
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
     * Move the clock forward `ms`, firing every timer that comes due —
     * including ones scheduled by the callbacks themselves, so advance(0)
     * drains a whole setTimeout(0) chain.
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
}

// ── Element stubs ───────────────────────────────────────────────────────────

/**
 * Shoelace stand-ins. We test our rendering, not Shoelace's: each is a plain
 * HTMLElement exposing only what app.js reads, writes or calls on it, with
 * attribute fallbacks so markup like `<sl-button disabled>` behaves. Anything
 * app.js only ever assigns (tag.size, alert.variant, ...) needs no stub — an
 * expando on an unknown element is enough.
 */
function defineShoelaceStubs(window) {
  const { HTMLElement } = window;

  // Value-bearing controls — settings.populateConfigForm and the filter bar
  // read `.value` / `.checked` off these during init.
  class SlValue extends HTMLElement {
    get value() { return this._value ?? this.getAttribute("value") ?? ""; }
    set value(v) { this._value = v == null ? "" : String(v); }
  }
  class SlChecked extends SlValue {
    get checked() { return this._checked ?? this.hasAttribute("checked"); }
    set checked(v) { this._checked = !!v; }
  }
  // `disabled` is read back by the Files/History tabs after a render.
  class SlDisableable extends HTMLElement {
    get disabled() { return this._disabled ?? this.hasAttribute("disabled"); }
    set disabled(v) { this._disabled = !!v; }
  }
  // show()/hide() are Shoelace's imperative open/close; app.js calls them on
  // dialogs, dropdowns and tooltips. Recorded so a test can assert on them.
  class SlOverlay extends HTMLElement {
    show() { (this._calls ??= []).push("show"); this._open = true; }
    hide() { (this._calls ??= []).push("hide"); this._open = false; }
  }
  // sl-alert.toast() appends the alert to Shoelace's toast stack; showToast
  // calls it on every toast, so it must exist or every toast throws.
  class SlAlert extends SlOverlay {
    toast() { (this._calls ??= []).push("toast"); return Promise.resolve(); }
  }
  // sl-tab-group.show(name) selects a panel; the keyboard shortcuts use it.
  class SlTabGroup extends HTMLElement {
    show(name) { (this._shown ??= []).push(name); }
  }
  class Passive extends HTMLElement {}

  // customElements.define refuses a constructor it has already seen, so every
  // tag gets its own fresh subclass of the behaviour it needs.
  const defs = {
    "sl-input": SlValue, "sl-select": SlValue, "sl-option": SlValue, "sl-textarea": SlValue,
    "sl-radio-group": SlValue, "sl-radio": SlValue, "sl-range": SlValue, "sl-color-picker": SlValue,
    "sl-menu-item": SlValue,
    "sl-checkbox": SlChecked, "sl-switch": SlChecked, "sl-radio-button": SlChecked,
    "sl-button": SlDisableable, "sl-icon-button": SlDisableable,
    "sl-dialog": SlOverlay, "sl-drawer": SlOverlay, "sl-dropdown": SlOverlay,
    "sl-tooltip": SlOverlay, "sl-details": SlOverlay, "sl-alert": SlAlert,
    "sl-tab-group": SlTabGroup,
    "sl-tag": Passive, "sl-badge": Passive, "sl-icon": Passive, "sl-spinner": Passive,
    "sl-progress-bar": Passive, "sl-progress-ring": Passive, "sl-divider": Passive,
    "sl-menu": Passive, "sl-tab": Passive, "sl-tab-panel": Passive, "sl-card": Passive,
    "sl-format-bytes": Passive, "sl-relative-time": Passive, "sl-button-group": Passive,
    "sl-menu-label": Passive, "sl-avatar": Passive, "sl-animation": Passive,
    "sl-visually-hidden": Passive, "sl-copy-button": Passive, "sl-tree": Passive,
    "sl-qr-code": Passive, "sl-skeleton": Passive, "sl-split-panel": Passive,
  };
  for (const [tag, base] of Object.entries(defs)) window.customElements.define(tag, class extends base {});
}

// ── The harness ─────────────────────────────────────────────────────────────

/**
 * Build a MoomboxApp against a fresh jsdom.
 *
 * @param {object}   [opts]
 * @param {object}   [opts.routes]        extra/overriding fake routes, keyed "METHOD /path"
 * @param {object}   [opts.initialState]  bodies for the boot fetches:
 *                                        { setup, config, status, auth, cookieAutoStatus }
 * @param {object}   [opts.storage]       localStorage seed, applied BEFORE the app is constructed
 * @param {boolean}  [opts.lightTheme]    make the prefers-color-scheme: light query match
 */
export async function makeApp({ routes = {}, initialState = {}, storage = {}, lightTheme = false } = {}) {
  // One app is live at a time (one per test), so close the previous window
  // here instead of holding every jsdom instance of the run open until
  // teardownAll. Each makeApp republishes its own globals.
  closeOpenWindows();

  const dom = new JSDOM("<!doctype html><html><body></body></html>", {
    url: "http://localhost/",
    pretendToBeVisual: true, // document.hidden must be false; the 1 Hz tick early-returns otherwise
  });
  const { window } = dom;
  const { document } = window;
  openWindows.push(window);

  defineShoelaceStubs(window);
  document.body.innerHTML = indexBodyMarkup();

  // prefers-color-scheme — read in the constructor to pick the theme, and
  // again in initializeApp to track OS changes. jsdom has no matchMedia.
  const media = { "(prefers-color-scheme: light)": !!lightTheme, "(prefers-color-scheme: dark)": !lightTheme };
  window.matchMedia = (q) => ({
    media: q, matches: !!media[q],
    addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {},
    onchange: null, dispatchEvent: () => false,
  });

  // ResizeObserver — jsdom has none; the player observes its video wrapper
  // with one and only ever constructs and observes.
  window.ResizeObserver = class { constructor(cb) { this.cb = cb; } observe() {} unobserve() {} disconnect() {} };

  // scrollIntoView — jsdom implements no scrolling at all, and the job list
  // scrolls the focused row into view on keyboard navigation.
  window.HTMLElement.prototype.scrollIntoView = function () {};

  // navigator.clipboard — absent in jsdom; copyTextToClipboard prefers it.
  const clipboard = [];
  Object.defineProperty(window.navigator, "clipboard", {
    configurable: true,
    value: { writeText: (t) => { clipboard.push(String(t)); return Promise.resolve(); } },
  });

  // The WebSocket the dashboard opens on init. It must never connect: a live
  // socket would replay `initial_state` into the very renderers under test.
  // readyState 3 = CLOSED, and no handler is ever fired.
  const sockets = [];
  class FakeWebSocket {
    static CONNECTING = 0; static OPEN = 1; static CLOSING = 2; static CLOSED = 3;
    constructor(url) {
      this.url = String(url);
      this.readyState = 3;
      this.sent = [];
      this.onopen = this.onmessage = this.onclose = this.onerror = null;
      sockets.push(this);
    }
    send(data) { this.sent.push(data); }
    close() { this.readyState = 3; }
    addEventListener() {} removeEventListener() {}
  }
  window.WebSocket = FakeWebSocket;

  const http = makeHttp();
  const clock = makeClock();

  // Manual frames. addLog/renderLogs clear their programmatic-scroll flag in a
  // rAF, so it has to be test-driven.
  let rafSeq = 0;
  const rafQueue = new Map();
  const flushRaf = () => {
    const due = [...rafQueue.entries()];
    rafQueue.clear();
    for (const [, cb] of due) cb(clock.now);
  };

  // The three fetches init() makes, in order — plus the two the boot chain
  // fires straight after (checkSecurityBanner, and the auto-cookie probe the
  // status bar makes), answered so the app settles in its healthy state
  // instead of down an error branch. A test overrides any of them via `routes`.
  http
    .on("GET /api/setup/status", () => initialState.setup ?? { isFirstRun: false, ffmpegValid: true })
    .on("GET /api/config", () => initialState.config ?? {})
    .on("GET /api/status", () => initialState.status ?? {})
    .on("GET /api/auth/status", () => initialState.auth ?? { authRequired: false, passwordlessExternal: false })
    .on("GET /api/cookies/auto-status", () => initialState.cookieAutoStatus ?? {});
  for (const [key, handler] of Object.entries(routes)) http.on(key, handler);

  installGlobals(window, { http, clock, rafQueue, nextRafId: () => ++rafSeq });
  freezeTime();

  for (const [k, v] of Object.entries(storage)) window.localStorage.setItem(k, String(v));

  // Dynamic, and only now: app.js wraps window.fetch and registers its
  // document listeners the moment it is evaluated, so every global it reaches
  // for has to already be the jsdom one. That evaluation happens exactly once
  // per process: the FIRST makeApp of a run gets app.js's own wrapper and
  // listeners on its window; the second and later makeApp re-use the cached
  // module, whose wrapper and listeners stay bound to that first window and
  // are NOT re-installed here. The globals are accessors onto the CURRENT
  // window (see installGlobals) so the cached module code itself keeps
  // reaching the live document.
  const { MoomboxApp } = await import("../../public/app.js");
  const app = new MoomboxApp();

  // init() is async (checkSetupStatus → initializeApp → loadConfig/loadStatus);
  // a few microtask turns settle the whole chain.
  const flush = () => new Promise((r) => REAL.setTimeout(r, 0));
  for (let i = 0; i < 4; i++) await flush();

  const el = (id) => document.getElementById(id);
  return {
    app, window, document, http, clock, sockets, clipboard,
    fetchLog: http.calls,
    el,
    /** Advance the manual clock; fires setTimeout/setInterval callbacks. */
    advance: (ms) => clock.advance(ms),
    flushRaf,
    /** Let every pending microtask/promise settle (real timers, not the fake clock). */
    flush,
    /** Toasts currently attached to <body>. */
    toasts: () => [...document.body.querySelectorAll("sl-alert.toast-alert")],
    close() { window.close(); },
  };
}

/**
 * Point the module-scope globals app.js reads at `window`. app.js and its
 * imports resolve `document`, `fetch`, `WebSocket`, `localStorage`, ... off
 * the Node global object, so a fresh jsdom has to be published there each
 * time — and, because the app module is only evaluated once per process,
 * through accessors that always name the CURRENT window.
 */
function installGlobals(window, { http, clock, rafQueue, nextRafId }) {
  const set = (name, value) =>
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });

  // Exactly the globals app.js (and the modules it imports) reach for.
  for (const name of [
    "window",              // window.matchMedia, window.innerWidth, window.location, window.open
    "document",
    "navigator",           // clipboard, userAgent
    "localStorage",        // theme + panel preferences
    "sessionStorage",        // the post-setup "justCompletedSetup" flag; Node 24 has no global of its own (Node 26 does, which hid this)
    "HTMLElement",         // `target instanceof HTMLElement` in isTypingInInput
    "HTMLMediaElement",    // the player module's readyState guard
    "Element",
    "Node",                // Node.TEXT_NODE in the log search highlighter
    "Event", "CustomEvent", "KeyboardEvent", "MouseEvent",
    "Blob",                // the player's resume beacon
    "FormData",            // the cookie-file import
    "AbortController",     // details fetches and the resume dialog
    "CSS",                 // CSS.escape in every job-card query
    "WebSocket",           // stubbed above
    "ResizeObserver",      // the player's video-wrapper observer
  ]) {
    const v = name === "window" ? window : window[name];
    if (v !== undefined) set(name, v);
  }

  // fetch is an ACCESSOR onto window.fetch, not a copy: app.js replaces
  // window.fetch with its 401-interceptor at module scope, and in a browser
  // (where window IS the global) that also replaces the bare `fetch` binding.
  // Here that happens ONCE, on the first makeApp's window: app.js's module
  // scope never re-runs, and the assignment below re-seats window.fetch on
  // every new window with the plain fake, so from the second makeApp onward
  // the interceptor is NOT on the path — it stays stranded on window #1. Only
  // the first test of a process exercises the interceptor; the rest exercise
  // app.js's fetch calls against the fake directly.
  window.fetch = (input, init) => http.fetch(input, init);
  Object.defineProperty(globalThis, "fetch", {
    configurable: true,
    get() { return window.fetch; },
    set(v) { window.fetch = v; },
  });

  set("setTimeout", (fn, delay, ...a) => clock.setTimeout(fn, delay, ...a));
  set("clearTimeout", (id) => clock.clear(id));
  set("setInterval", (fn, delay, ...a) => clock.setInterval(fn, delay, ...a));
  set("clearInterval", (id) => clock.clear(id));
  set("requestAnimationFrame", (cb) => { const id = nextRafId(); rafQueue.set(id, cb); return id; });
  set("cancelAnimationFrame", (id) => { rafQueue.delete(id); });
  // window.* aliases so code reached through `window` sees the same fakes.
  window.setTimeout = globalThis.setTimeout;
  window.clearTimeout = globalThis.clearTimeout;
  window.setInterval = globalThis.setInterval;
  window.clearInterval = globalThis.clearInterval;
  window.requestAnimationFrame = globalThis.requestAnimationFrame;
  window.cancelAnimationFrame = globalThis.cancelAnimationFrame;
}

/**
 * Freeze the wall clock and pin the host locale.
 *
 * `new Date()` / `Date.now()` feed formatRelativeTime, the check countdown and
 * the uptime readout; `toLocaleString()` (on both Date and Number) formats the
 * Files/History tables and the progress tooltips using the HOST's locale and
 * time zone. Neither is a rendering choice app.js makes, and both would make a
 * snapshot fail on a machine in another zone — so they are pinned here rather
 * than papered over in the assertions.
 */
function freezeTime() {
  const RealDate = REAL.Date;
  class FrozenDate extends RealDate {
    constructor(...args) { if (args.length === 0) super(NOW); else super(...args); }
    static now() { return NOW; }
  }
  Object.defineProperty(globalThis, "Date", { configurable: true, writable: true, value: FrozenDate });

  RealDate.prototype.toLocaleString = function (locale, opts) {
    return REAL_TO_LOCALE.date.call(this, locale ?? "en-US", { timeZone: "UTC", ...(opts || {}) });
  };
  Number.prototype.toLocaleString = function (locale, opts) {
    return REAL_TO_LOCALE.number.call(this, locale ?? "en-US", opts);
  };
}

function closeOpenWindows() {
  for (const w of openWindows.splice(0)) {
    try { w.close(); } catch { /* already closed */ }
  }
}

/** Restore the timer/clock/locale globals and close every jsdom window the suite opened. */
export function teardownAll() {
  for (const [name, fn] of Object.entries(REAL)) {
    if (fn !== undefined) Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: fn });
  }
  REAL.Date.prototype.toLocaleString = REAL_TO_LOCALE.date;
  Number.prototype.toLocaleString = REAL_TO_LOCALE.number;
  closeOpenWindows();
}
