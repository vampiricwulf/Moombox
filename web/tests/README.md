# Frontend JS tests

Uses Node.js's built-in test runner (`node:test`). Every `*.test.mjs` file here
is one suite. The pure suites import from `../public/modules/` and need nothing
else; the DOM suites drive the dashboard, the player or a standalone page
inside a jsdom document. jsdom is the only dev dependency, and it is
**optional**: without it the DOM suites skip.

## Running

From the repo root:

```bash
# Run every test file (the DOM suites skip if jsdom is not installed)
node --test web/tests/*.test.mjs

# Run a single file
node --test web/tests/filter-parser.test.mjs

# With detailed output per test
node --test --test-reporter=spec web/tests/*.test.mjs

# One test by name
node --test --test-name-pattern="selection" web/tests/player.test.mjs
```

Requires Node.js 20+. The `.mjs` extension tells Node to parse files as ES
modules. CI (`.github/workflows/ci.yml`) runs `npm ci` here first, so it runs
every suite, the DOM ones included.

## The suites

Pure — no DOM, nothing to install:

| Suite | What it covers |
|-------|----------------|
| `chat-timeline.test.mjs` | `chat-timeline.js`: chat offsets, the YouTube start bias, the chat header and divider counts |
| `filter-engine.test.mjs` | `filter-engine.js`: parsed filter tokens evaluated against jobs |
| `filter-parser.test.mjs` | `filter-parser.js`: the filter language (text, `status:`/`channel:`/`platform:`, `-` negation, `a\|b`, quotes) parsed into tokens |
| `logout.test.mjs` | `logout.js`: when the logout button shows, and what its click does |
| `nico-geometry.test.mjs` | `nico-geometry.js`: the video's box inside the player (letterbox, pillarbox) for the chat overlay |
| `nico-lanes.test.mjs` | `nico-lanes.js`: lane allocation for the scrolling chat overlay |
| `nico-scheduler.test.mjs` | `nico-scheduler.js`: the overlay's cursor, anchor, pending list and drop count |
| `template-preview.test.mjs` | the Settings page's output-template example, formatted as `config.ResolveTemplate` writes a name |
| `utils.test.mjs` | `utils.js`: the formatters (`formatBytes`, timestamps, durations) and the small helpers beside them |

DOM — jsdom, through `helpers/app-dom.mjs` (the dashboard), `helpers/player-dom.mjs`
(the player) or, for `boot-and-login.test.mjs`, the real pages directly:

| Suite | What it covers |
|-------|----------------|
| `a11y-controls.test.mjs` | the status bar's and log panel's clickable controls reachable by keyboard and screen reader; two of its tests read `moombox.css`'s text and need no DOM |
| `app.test.mjs` | rendering pins: job cards, Files rows and the details dialog against `fixtures/app-job-items.json` (see [The app harness](#the-app-harness)) |
| `app-resync.test.mjs` | a mid-session `initial_state` replaces the job list and the log buffer, and keeps the details dialog |
| `archive-boundary.test.mjs` | the dashboard's archive cutoff, the JS twin of `internal/jobfilter` |
| `boot-and-login.test.mjs` | `boot-theme.js` and `login.js` evaluated against the real `index.html` and `login.html` |
| `channel-removal.test.mjs` | removing a channel asks: keep its jobs (the default) or delete the pending ones, and the toast says what it did; its prompt-text tests are pure |
| `dashboard-text.test.mjs` | small user-facing strings: a singular chat count, the Stats tab with no disk reading, a single-file recording's load error |
| `details-mux-finished.test.mjs` | the details dialog's Mux button on a Finished job that still holds an unmuxed part |
| `fetch-errors.test.mjs` | failed dashboard requests toast the server's reason |
| `ffmpeg-path-check.test.mjs` | a checked FFmpeg path reaches the Settings form |
| `files-panel.test.mjs` | the Files tab's rows: labelled delete controls, set-aside confirms, refusals |
| `filter-bar.test.mjs` | the filter bar under real typing: the debounce, chips, the caret |
| `import-placeholder.test.mjs` | an imported job's placeholder id draws no dead embed, Stream URL or Open URL; one pure helper test |
| `imports-importing.test.mjs` | once an import's body is sent the panel says "Importing…" with no Cancel, and Clear and another file wait for the response |
| `imports-upload.test.mjs` | the import panel's Clear and file choice while an upload runs, and the outcome note or refusal an import answers with |
| `job-asides.test.mjs` | set-aside recordings in the details dialog, its Recover button, and the Files tab naming them |
| `job-deeplink.test.mjs` | `#job=<id>` deep links open that job's details |
| `job-progress.test.mjs` | `job_progress` frames merge onto the row the tab holds |
| `job-selection-actions.test.mjs` | the Tasks list's selection and the batch and single actions that read it |
| `keyboard-modifiers.test.mjs` | Ctrl/Cmd/Alt combinations never reach a dashboard or trim-dialog shortcut |
| `log-panel.test.mjs` | the Logs panel: batched appends, the 500-line window, the snapshot and frame numbers (`logSeq`/`seq`) across connects and resyncs |
| `open-folder.test.mjs` | Open Folder surfaces a refusal |
| `player.test.mjs` | `player.js`: selection, the chat sidebar and offset, seeking, search, the overlay; a few pure helper tests |
| `release-notes-toast.test.mjs` | a failed release-notes fetch toasts rather than blocking the tab |
| `render-diff.test.mjs` | a repeated `job_update` makes no DOM write in the details dialog or the status bar |
| `resolution-picker.test.mjs` | the Max Resolution picker in Settings and the setup wizard; its preset-mapping tests are pure |
| `settings-active-platforms.test.mjs` | the active-platform toggles send an override only when the operator set one |
| `settings-guard.test.mjs` | the unsaved-settings guard on every way out of a dirty Settings page |
| `settings-notification-mode.test.mjs` | each notification target's delivery mode |
| `settings-notifications.test.mjs` | the notification card's auto-saving controls and the Public Dashboard URL row |
| `settings-reorder-budget.test.mjs` | the two reorder ceilings: 0 sent, an empty field omitted |
| `settings-text.test.mjs` | Settings and setup text that pointed the wrong way or dropped the server's reason |
| `settings-channels.test.mjs` | both Add Channel dialogs: a URL or bare `@handle` resolved first, a configured ID switching to editing it, the edit mark, and a stale list's `409` |
| `setup-wizard.test.mjs` | the first-run wizard's Finish: the address it redirects the tab to after the restart |
| `sidecar-warning.test.mjs` | the header warning while the BotGuard sidecar is down |
| `stats-storage.test.mjs` | the Stats tab's storage breakdown, Cancelled included |
| `trimmer.test.mjs` | the trim dialog's failure states and keyboard reach |
| `update-check-debounce.test.mjs` | a debounced update check reports the wait, not "Up to date" |
| `update-check-keep.test.mjs` | an up-to-date check keeps a release that reached the page during its round trip |
| `update-dialog.test.mjs` | the update dialog's Update Now and Skip name the release on screen, and the dialog closes, saying why, once that release is skipped or withdrawn elsewhere |
| `verify-signature.test.mjs` | Verify Signature's wording for a checked manifest and for a signature alone |
| `watched-state.test.mjs` | Mark Watched / Unwatched from the details dialog and the batch bar |

A new suite gets a row in one of these tables.

## The DOM suites (jsdom)

Install jsdom **inside `web/tests/`** — never at the repo root:

```bash
cd web/tests
npm ci          # uses the committed package-lock.json
cd ../..
node --test web/tests/*.test.mjs
```

`web/tests/node_modules/` is gitignored; `package.json` and
`package-lock.json` are committed so `npm ci` is reproducible.

### How the skip works

Each DOM suite probes `await import("jsdom")` at the top of the file. If that
throws `ERR_MODULE_NOT_FOUND`, every test that needs a DOM is registered with
`{ skip: "..." }`, so a checkout without `npm ci` reports them as **skipped**,
never failed; the pure tests inside a DOM suite (the ones the table names) still
run. Any other error from the probe fails the suite.

The harness (`helpers/player-dom.mjs`, `helpers/app-dom.mjs`) — and anything
that imports it, such as `fixtures/app-render-inputs.mjs` — is imported
**dynamically, only after the probe succeeds**: the harness imports jsdom
itself, so a static import would fail the whole file without it instead of
skipping, and a genuine fault in the harness is then a failure rather than a
silent skip.

Without jsdom the run ends with `fail 0` and the DOM tests counted under
`skipped`; with it, `skipped 0`.

## The player harness

`helpers/player-dom.mjs` exports `makePlayer(opts)`, which builds a jsdom
document from the player panel markup **read out of `web/public/index.html`**
(so the harness follows the real markup instead of a copy), registers stub
`sl-*` custom elements, and returns a live `PlayerController` plus the handles
a test needs:

```js
const h = harness.makePlayer({
  jobs: [ /* GET /api/jobs */ ],
  watchStateById: { j1: { chatOffset: 1.5 } },
  chat: { platform: "twitch", messages: [ /* ... */ ] },
  geom: { overlay: { w: 1280, h: 408 }, rowH: 24, msgW: 200 },  // msgH defaults to rowH
  storage: { "player-sidebar-toggle": "true" },
});
await h.selectJob("j1");
h.tick(60000);          // set currentTime (ms) + fire `timeupdate`
h.seek(30000);          // ...+ fire `seeked` first (going back in time IS a seek)
h.advance(120);         // manual clock: fires setTimeout/setInterval
h.flushRaf();           // manual frames
h.key("c");             // keydown on document (or `{ target }`)
```

Everything stubbed is something `player.js` actually calls and jsdom does not
provide: layout boxes (jsdom has none — `geom` is the knob), `Element.animate`,
`HTMLMediaElement.play/pause/load` and its state properties, fullscreen,
`matchMedia`, `ResizeObserver`, `navigator.sendBeacon`, `fetch` (a recording
route table — see `h.fetchLog` / `h.http.matching(...)`), and the timer/frame
globals. Nothing under `web/public/` is patched.

**Time is manual.** No test may sleep or depend on wall-clock timing: drive
`setTimeout`/`setInterval` with `h.advance(ms)` and `requestAnimationFrame`
with `h.flushRaf()`.

## The app harness

`helpers/app-dom.mjs` exports `makeApp(opts)`, the same shape one level up: it
builds a jsdom document from the **whole `<body>` of `web/public/index.html`**
(the dashboard touches every panel), publishes the globals, answers the boot
fetches, imports `app.js` **dynamically** — it has module-level side effects,
so every global has to exist first — and constructs a real `MoomboxApp`:

```js
const h = await harness.makeApp({
  initialState: { status: { version: "2.8.7" }, config: { /* GET /api/config */ } },
  routes: { "GET /api/files/orphaned": () => [] },
  storage: { "moombox-theme": "light" },
});
h.app.renderJobItem(job);   // the live controller
h.el("logs-viewer");        // getElementById
h.advance(1000);            // manual clock
h.flush();                  // let promises settle
```

Beyond the player harness's stubs it adds: a `WebSocket` that never connects
(a live socket would replay `initial_state` into the renderers under test; a
test feeds frames through `h.app.handleMessage`), `sl-alert.toast()` /
`sl-dialog.show()` and friends, `navigator.clipboard` and `scrollIntoView` —
and, unlike the player harness, a **frozen wall clock** (`NOW`) plus an
en-US/UTC pin on `toLocaleString`, so relative timestamps render the same
string on every machine.

`app.test.mjs` is a **pin**, not a behaviour suite: it snapshots what the
dashboard draws so the controller extractions can be proved to change nothing.
The snapshot lives in `fixtures/app-job-items.json` and its inputs in
`fixtures/app-render-inputs.mjs`. When a rendering change is *intended*,
regenerate it deliberately — from `web/tests/`:

```bash
node --input-type=module -e '
import fs from "node:fs";
const h = await (await import("./helpers/app-dom.mjs")).makeApp();
const i = await import("./fixtures/app-render-inputs.mjs");
const jobItems = Object.fromEntries(
  Object.entries(i.JOBS).map(([s, j]) => [s, h.app.renderJobItem(j)]));
h.app.renderOrphanedFiles(i.FILES);
h.app.renderOrphanedHistory(i.HISTORY);
h.app.details.renderJobDetails(i.JOBS.Finished);
const rows = (t, r) => [...h.el(t).querySelectorAll(r)].map((x) => x.outerHTML);
fs.writeFileSync("fixtures/app-job-items.json", JSON.stringify({
  jobItems,
  orphanedFiles: rows("files-table", ".files-row"),
  orphanedHistory: rows("history-table", ".history-row"),
  jobDetails: h.el("job-details-content").innerHTML,
}, null, 2) + "\n");
'
```

## Scope

Pure modules under `web/public/modules/` — parsers, formatters, timeline math,
the lane allocator, the overlay scheduler — are covered by the pure suites.
`player.js` has the player harness; `app.js` and every controller it composes
are reached through the app harness. That includes the Settings page, the setup
wizard and the trim dialog (`settings.js`, `setup.js`, `trimmer.js`), whose
suites drive the live controllers `MoomboxApp` constructs rather than a harness
of their own; the two standalone page scripts, `boot-theme.js` and `login.js`,
are evaluated against the real pages by `boot-and-login.test.mjs`.
`helpers/player-dom.mjs` and `helpers/app-dom.mjs` are the pattern to extend if
a module ever needs a harness of its own.

## Adding a test

1. Create `web/tests/<module-name>.test.mjs`, or add to the suite that already
   covers the behaviour.
2. Import from `../public/modules/<module-name>.js` (keep the `.js`).
3. Use `import { test } from "node:test"` and `import assert from "node:assert/strict"`.
4. For a DOM test, copy the jsdom probe and the dynamic harness import from the
   top of `log-panel.test.mjs`, and pass `{ skip }` to every test that needs
   the DOM, so the file still skips cleanly without jsdom.
5. Give a new suite its row in [The suites](#the-suites).
6. Verify with `node --test web/tests/<module-name>.test.mjs`, and once with
   `web/tests/node_modules` moved aside to see the DOM tests skip.
