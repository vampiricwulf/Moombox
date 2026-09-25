# Frontend JS tests

Uses Node.js's built-in test runner (`node:test`). Most suites are pure — no
dependencies, no DOM — and just import from `../public/modules/`. Fourteen
suites drive their module inside a jsdom document:
`a11y-controls.test.mjs`, `app.test.mjs`, `app-resync.test.mjs`,
`archive-boundary.test.mjs`, `boot-and-login.test.mjs`, `job-progress.test.mjs`,
`log-panel.test.mjs`, `open-folder.test.mjs`, `player.test.mjs`,
`release-notes-toast.test.mjs`, `render-diff.test.mjs`, `settings-reorder-budget.test.mjs`,
`sidecar-warning.test.mjs` and `update-check-debounce.test.mjs`.
jsdom is the only dev dependency, and it is **optional**.

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
modules.

## The DOM suites (jsdom)

The fourteen suites listed above are the ones that need a DOM. Install jsdom
**inside `web/tests/`** — never at the repo root:

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
throws, every test in the file is registered with `{ skip: "..." }`, so a
checkout without `npm ci` reports them as **skipped**, never failed — the 123
DOM tests (player 37, render-diff 16, app 13, a11y-controls 12, boot-and-login 12,
job-progress 8, log-panel 4, open-folder 4, settings-reorder-budget 4,
update-check-debounce 4, archive-boundary 3, sidecar-warning 3, app-resync 2,
release-notes-toast 1),
leaving 123 tests that need no DOM — the eight pure suites plus the
stylesheet-text test in `a11y-controls.test.mjs`, which reads `moombox.css`
and asserts on its text:

```
ℹ tests 246
ℹ pass 123
ℹ fail 0
ℹ skipped 123
```

With jsdom installed the same command reports `tests 246` / `pass 246` /
`skipped 0`.

The helper (`helpers/player-dom.mjs`, `helpers/app-dom.mjs`) is imported only
after the probe succeeds, so a genuine fault in the harness is a failure rather
than a silent skip.

| Suite | Needs jsdom |
|-------|-------------|
| `chat-timeline.test.mjs`, `filter-engine.test.mjs`, `filter-parser.test.mjs`, `logout.test.mjs`, `nico-geometry.test.mjs`, `nico-lanes.test.mjs`, `nico-scheduler.test.mjs`, `utils.test.mjs` | no |
| `a11y-controls.test.mjs`, `app.test.mjs`, `app-resync.test.mjs`, `archive-boundary.test.mjs`, `boot-and-login.test.mjs`, `job-progress.test.mjs`, `log-panel.test.mjs`, `open-folder.test.mjs`, `player.test.mjs`, `release-notes-toast.test.mjs`, `render-diff.test.mjs`, `settings-reorder-budget.test.mjs`, `sidecar-warning.test.mjs`, `update-check-debounce.test.mjs` | yes |

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
(a live socket would replay `initial_state` into the renderers under test),
`sl-alert.toast()` / `sl-dialog.show()` and friends, `navigator.clipboard` and
`scrollIntoView` — and, unlike the player harness, a **frozen wall clock**
(`NOW`) plus an en-US/UTC pin on `toLocaleString`, so relative timestamps
render the same string on every machine.

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
the lane allocator, the overlay scheduler — are covered by the plain suites.
`player.js` and `app.js` — and the controllers `app.js` composes, reached
through the app harness — are covered by the jsdom suites above, as are the two
standalone page scripts `boot-theme.js` and `login.js` (`boot-and-login.test.mjs`,
which evals them against the real `index.html` and `login.html`). `settings.js`,
`setup.js` and `trimmer.js` have no harness of their own; `release-notes-toast.test.mjs`
and `settings-reorder-budget.test.mjs` reach `settings.js` handlers only because
`MoomboxApp` constructs `SettingsController` and wires its listeners. The two
helpers are the pattern to extend if a real one is wanted.

## Adding a test

1. Create `web/tests/<module-name>.test.mjs`.
2. Import from `../public/modules/<module-name>.js` (keep the `.js`).
3. Use `import { test } from "node:test"` and `import assert from "node:assert/strict"`.
4. For a DOM test, copy the jsdom probe at the top of `player.test.mjs` so the
   file still skips cleanly without jsdom.
5. Verify with `node --test web/tests/<module-name>.test.mjs`.
