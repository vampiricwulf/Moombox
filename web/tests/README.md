# Frontend JS tests

Uses Node.js's built-in test runner (`node:test`). Most suites are pure — no
dependencies, no DOM — and just import from `../public/modules/`. One suite
(`player.test.mjs`) drives `player.js` inside a jsdom document; jsdom is the
only dev dependency, and it is **optional**.

## Running

From the repo root:

```bash
# Run every test file (the DOM suite skips if jsdom is not installed)
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

## The DOM suite (jsdom)

`player.test.mjs` is the only suite that needs a DOM. Install it **inside
`web/tests/`** — never at the repo root:

```bash
cd web/tests
npm ci          # uses the committed package-lock.json
cd ../..
node --test web/tests/*.test.mjs
```

`web/tests/node_modules/` is gitignored; `package.json` and
`package-lock.json` are committed so `npm ci` is reproducible.

### How the skip works

`player.test.mjs` probes `await import("jsdom")` at the top of the file. If
that throws, every test in the file is registered with `{ skip: "..." }`, so a
checkout without `npm ci` reports them as **skipped**, never failed:

```
ℹ pass 67
ℹ fail 0
ℹ skipped 9
```

The helper (`helpers/player-dom.mjs`) is imported only after the probe
succeeds, so a genuine fault in the harness is a failure rather than a silent
skip.

| Suite | Needs jsdom |
|-------|-------------|
| `chat-timeline.test.mjs`, `filter-engine.test.mjs`, `filter-parser.test.mjs`, `nico-lanes.test.mjs`, `utils.test.mjs` | no |
| `player.test.mjs` | yes |

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
  geom: { overlay: { w: 1280, h: 408 }, rowH: 24, msgW: 200 },
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

## Scope

Pure modules under `web/public/modules/` — parsers, formatters, timeline math,
the lane allocator — are covered by the plain suites. `player.js` is covered by
the jsdom suite above. The other UI-heavy modules (`settings.js`, `setup.js`,
`trimmer.js`, `app.js`) have no harness yet; `helpers/player-dom.mjs` is the
pattern to extend if one is wanted.

## Adding a test

1. Create `web/tests/<module-name>.test.mjs`.
2. Import from `../public/modules/<module-name>.js` (keep the `.js`).
3. Use `import { test } from "node:test"` and `import assert from "node:assert/strict"`.
4. For a DOM test, copy the jsdom probe at the top of `player.test.mjs` so the
   file still skips cleanly without jsdom.
5. Verify with `node --test web/tests/<module-name>.test.mjs`.
