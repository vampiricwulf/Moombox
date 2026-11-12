// The theme bootstrap and the login form moved out of inline <script> blocks so
// script-src could drop 'unsafe-inline' (sweep 2 row #91). Both are now files,
// and both are exercised here against the REAL pages — a page whose script the
// CSP now refuses would look identical in Go and be dead in a browser.
//
// jsdom is constructed with runScripts: "outside-only" throughout. Without it
// window.eval is Node's eval running in NODE's global scope, where `document`
// does not exist; with it the script runs in the window's own scope, which is
// what a browser does with a <script src>. The pages' own <script> tags are
// never fetched (no `resources: "usable"`), so each test evaluates exactly the
// file it is about.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

let jsdomMissing = null;
let JSDOM = null;
let VirtualConsole = null;
try {
  ({ JSDOM, VirtualConsole } = await import("jsdom"));
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = `jsdom not installed — run \`npm ci\` in web/tests (${e.code})`;
}
const skip = jsdomMissing || false;

const HERE = path.dirname(fileURLToPath(import.meta.url));
const PUBLIC = path.join(HERE, "..", "public");
const read = (f) => fs.readFileSync(path.join(PUBLIC, f), "utf8");

const open = [];
after(() => { for (const w of open.splice(0)) { try { w.close(); } catch { /* closed */ } } });

/** A jsdom over one real page, with a stubbed matchMedia and a quiet console. */
function page(file, { light = false, url = null } = {}) {
  const virtualConsole = new VirtualConsole();
  const jsdomErrors = [];
  virtualConsole.on("jsdomError", (e) => jsdomErrors.push(e.message));
  const dom = new JSDOM(read(file), {
    url: url ?? "http://localhost/" + (file === "index.html" ? "" : file),
    runScripts: "outside-only",
    virtualConsole,
  });
  open.push(dom.window);
  // prefers-color-scheme — jsdom's own matchMedia answers `matches: false` to
  // every query, so both arms of the bootstrap would look identical.
  dom.window.matchMedia = (q) => ({
    media: q,
    matches: q.includes("light") ? light : !light,
    addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {},
    onchange: null, dispatchEvent: () => false,
  });
  dom.jsdomErrors = jsdomErrors;
  return dom;
}

/** Build a page, seed localStorage, and run boot-theme.js over it. */
function boot(file, { light = false, stored = null } = {}) {
  const dom = page(file, { light });
  if (stored) dom.window.localStorage.setItem("moombox-theme", stored);
  dom.window.eval(read("boot-theme.js"));
  return dom;
}

/**
 * Resolves when jsdom has finished parsing and fired DOMContentLoaded. It
 * waits for `load`, the event after it: readyState "interactive" does not say
 * whether DOMContentLoaded has fired yet (jsdom 31 returns from the
 * constructor "interactive" with the event still queued), "complete" does.
 */
function domContentLoaded(window) {
  return new Promise((resolve) => {
    if (window.document.readyState === "complete") {
      resolve();
      return;
    }
    window.addEventListener("load", () => resolve(), { once: true });
  });
}

// ── boot-theme.js ───────────────────────────────────────────────────────────

// MUTANT: leave the stylesheet swap behind in the page (or run boot-theme.js
// before the <link> tags) — the light theme's CSS never activates and the
// dashboard flashes dark for every light-theme user.
for (const file of ["index.html", "login.html"]) {
  test(`${file}: boot-theme.js applies a stored light theme and swaps the stylesheet`, { skip }, () => {
    const { window } = boot(file, { stored: "light" });
    const doc = window.document;
    assert.equal(doc.documentElement.className, "sl-theme-light",
      "the root class drives every Shoelace token; without it the page renders dark");
    assert.equal(doc.getElementById("sl-theme-light").media, "",
      "the light stylesheet must be activated");
    assert.equal(doc.getElementById("sl-theme-dark").media, "not all",
      "the dark stylesheet must be deactivated, or both cascade");
    assert.equal(doc.querySelector('meta[name="theme-color"]').content, "#ffffff",
      "the browser chrome colour must follow the theme");
  });

  // MUTANT: hard-code "dark" instead of consulting matchMedia — a light-themed
  // desktop with no stored choice boots dark, which is the flash this file exists
  // to remove.
  test(`${file}: boot-theme.js follows prefers-color-scheme with nothing stored`, { skip }, () => {
    const { window } = boot(file, { light: true });
    assert.equal(window.document.documentElement.className, "sl-theme-light",
      "with no stored preference the OS setting decides");
    assert.equal(window.document.getElementById("sl-theme-light").media, "",
      "the swap is driven by the RESOLVED theme, not by what is in localStorage");
  });

  // MUTANT: let the stored value lose to the OS query — a user who chose dark
  // on a light-themed desktop gets the light stylesheet on every load.
  test(`${file}: boot-theme.js lets a stored dark theme beat a light desktop`, { skip }, () => {
    const { window } = boot(file, { light: true, stored: "dark" });
    const doc = window.document;
    assert.equal(doc.documentElement.className, "sl-theme-dark");
    assert.equal(doc.getElementById("sl-theme-light").media, "not all",
      "the markup's default must be left alone for dark");
    assert.equal(doc.querySelector('meta[name="theme-color"]').content, "#1C1B22");
  });

  // MUTANT: drop the try/catch — a private window (or blocked site data) throws
  // before the rest of the page's scripts run, so the login form never wires up.
  test(`${file}: boot-theme.js survives a localStorage that throws`, { skip }, () => {
    const dom = page(file);
    Object.defineProperty(dom.window, "localStorage", {
      configurable: true,
      get() { throw new Error("blocked site data"); },
    });
    dom.window.matchMedia = () => { throw new Error("no matchMedia"); };
    assert.doesNotThrow(() => dom.window.eval(read("boot-theme.js")),
      "a private window or blocked site data must leave the default dark theme intact, not throw " +
      "before the rest of the page runs");
    assert.equal(dom.window.document.documentElement.className, "sl-theme-dark");
    assert.equal(dom.window.document.getElementById("sl-theme-light").media, "not all");
  });
}

// ── login.js ────────────────────────────────────────────────────────────────

/** Record every fetch and answer it with `respond()`. */
function stubFetch(window, respond) {
  const calls = [];
  window.fetch = (url, init) => {
    calls.push({ url: String(url), init });
    return Promise.resolve(respond());
  };
  return calls;
}

const refused = () => ({
  ok: false,
  status: 401,
  statusText: "Unauthorized",
  json: () => Promise.resolve({ error: "Invalid password" }),
});

/** Two microtask turns: doLogin awaits fetch, then response.json(). */
const settle = async () => {
  await new Promise((r) => setTimeout(r, 0));
  await new Promise((r) => setTimeout(r, 0));
};

// MUTANT: keep the bare `document.addEventListener("DOMContentLoaded", …)` —
// a script evaluated AFTER parsing (a cached load, a deferred one, or jsdom
// here) never initialises and the Login button does nothing.
test("login.js wires the form when it is evaluated after DOMContentLoaded", { skip }, async () => {
  const dom = page("login.html");
  const { window } = dom;
  await domContentLoaded(window);
  assert.notEqual(window.document.readyState, "loading",
    "premise: this test is about a script that arrives after the parser has finished");

  const calls = stubFetch(window, refused);
  window.eval(read("login.js"));

  window.document.getElementById("login-password").value = "hunter2";
  window.document.getElementById("login-submit").dispatchEvent(new window.Event("click"));
  await settle();

  assert.equal(calls.length, 1, "the Login button must POST");
  assert.equal(calls[0].url, "/api/auth/login");
  assert.equal(calls[0].init.method, "POST");
  assert.equal(JSON.parse(calls[0].init.body).password, "hunter2");
  assert.equal(window.document.getElementById("login-error-text").textContent, "Invalid password",
    "a refused login must show the server's message");
  assert.equal(window.document.getElementById("login-error").style.display, "block",
    'the alert must be shown with "block" — clearing the inline style falls back to the ' +
    "stylesheet's display:none");
  assert.equal(window.document.getElementById("login-password").value, "",
    "the password field must be cleared after a refusal");
  assert.equal(window.document.getElementById("login-submit").disabled, false,
    "the button must be usable again for a second attempt");
});

// MUTANT: drop the readyState guard and call initLogin() immediately — a
// script that reaches the DOM before the form exists finds nothing, returns,
// and never registers a listener.
test("login.js defers its wiring when it is evaluated while the document is still parsing", { skip }, async () => {
  const virtualConsole = new VirtualConsole();
  const dom = new JSDOM("<!doctype html><html><head></head><body></body></html>", {
    url: "http://localhost/login.html",
    runScripts: "outside-only",
    virtualConsole,
  });
  open.push(dom.window);
  const { window } = dom;
  // document.open() puts the document back in the parser's hands: readyState
  // is "loading" until close(), which is what a script reached before the end
  // of the markup sees in a browser. (A script-free jsdom document is already
  // "interactive" when the constructor returns, as of jsdom 31; jsdom 30 left
  // it "loading" there, which this test used to rely on.)
  window.document.open();
  assert.equal(window.document.readyState, "loading",
    "premise: an opened document is still parsing and has not fired DOMContentLoaded");

  const calls = stubFetch(window, refused);
  window.eval(read("login.js"));

  // The form appears only now — as it does in a browser when the script is
  // reached before the markup it drives. The real page's markup, lifted.
  const html = read("login.html");
  const start = html.indexOf("<main");
  const end = html.indexOf("</main>") + "</main>".length;
  assert.ok(start > 0 && end > start, "login.html must still have a <main> to lift");
  window.document.write("<!doctype html><html><head></head><body>" + html.slice(start, end) + "</body></html>");
  assert.equal(window.document.readyState, "loading", "premise: the parser is still open after the write");
  // Registered after login.js's own listener, so it runs after initLogin.
  const fired = new Promise((r) => window.document.addEventListener("DOMContentLoaded", r, { once: true }));
  window.document.close();
  await fired;

  window.document.getElementById("login-password").value = "hunter2";
  window.document.getElementById("login-submit").dispatchEvent(new window.Event("click"));
  await settle();

  assert.equal(calls.length, 1,
    "the wiring must happen at DOMContentLoaded when the form was not there at evaluation time");
});

// MUTANT: submit on any keydown instead of Enter, or drop the keydown listener
// entirely — the password field's Enter key stops logging in.
test("login.js logs in on Enter in the password field", { skip }, async () => {
  const dom = page("login.html");
  const { window } = dom;
  await domContentLoaded(window);
  const calls = stubFetch(window, refused);
  window.eval(read("login.js"));

  const input = window.document.getElementById("login-password");
  input.value = "hunter2";
  input.dispatchEvent(new window.KeyboardEvent("keydown", { key: "a" }));
  await settle();
  assert.equal(calls.length, 0, "an ordinary keystroke must not submit");

  input.dispatchEvent(new window.KeyboardEvent("keydown", { key: "Enter" }));
  await settle();
  assert.equal(calls.length, 1, "Enter must submit");
});

// MUTANT: clear the loading state on success too — the button flickers back to
// its idle look for the frame before the browser navigates. Or drop the
// redirect, and a correct password leaves the visitor on the login page.
test("login.js redirects to the dashboard on success and holds the button", { skip }, async () => {
  const dom = page("login.html");
  const { window } = dom;
  await domContentLoaded(window);
  const calls = stubFetch(window, () => ({
    ok: true,
    status: 200,
    statusText: "OK",
    json: () => Promise.resolve({}),
  }));
  window.eval(read("login.js"));

  window.document.getElementById("login-password").value = "hunter2";
  window.document.getElementById("login-submit").dispatchEvent(new window.Event("click"));
  await settle();

  assert.equal(calls.length, 1);
  // jsdom implements no navigation; it reports the attempt on the virtual
  // console instead, which is the only observable the redirect leaves here.
  assert.ok(dom.jsdomErrors.some((m) => /navigation/i.test(m)),
    "a successful login must navigate to the dashboard");
  assert.equal(window.document.getElementById("login-submit").disabled, true,
    "the button stays disabled/loading until the navigation completes, or it flickers");
  assert.equal(window.document.getElementById("login-error").style.display, "none",
    "no error may be shown for a successful login");
});

// A notification's deep link is {public_url}/#job=<id>. A browser with no
// session is answered the login page at that same URL (AuthMiddleware serves
// it in place), so the hash is still in the address bar when the password is
// accepted, and the dashboard opens the job from it once it loads.
//
// jsdom implements no navigation: a reload is reported on the virtual
// console, and an href that differs from the current URL by its fragment
// alone is applied as a hash change.
//
// MUTANT: `window.location.href = "/"` — the hash is gone and the visitor
// lands on the plain job list. `window.location.href = "/" + hash` alone —
// the same URL again, which a browser does not load: the visitor stays on
// the login form (no navigation is reported here).
test("login.js keeps a #job= deep link through a successful login", { skip }, async () => {
  const dom = page("login.html", { url: "http://localhost/#job=yt_aB3-xY_9zqw" });
  const { window } = dom;
  await domContentLoaded(window);
  stubFetch(window, () => ({ ok: true, status: 200, statusText: "OK", json: () => Promise.resolve({}) }));
  window.eval(read("login.js"));

  window.document.getElementById("login-password").value = "hunter2";
  window.document.getElementById("login-submit").dispatchEvent(new window.Event("click"));
  await settle();

  assert.equal(window.location.href, "http://localhost/#job=yt_aB3-xY_9zqw",
    "the deep link's hash must still be in the address bar after the login");
  assert.ok(dom.jsdomErrors.some((m) => /navigation/i.test(m)),
    "the page must be loaded again, or the login form stays up");
});
