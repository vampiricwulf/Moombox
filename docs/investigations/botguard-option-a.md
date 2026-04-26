# BotGuard Option A — Day 1 Findings

**Status as of test.49**: investigation complete; both candidate JS DOM
libraries blocked from a clean goja embed. Build scaffold + smoke tests
preserved in-repo for the next iteration.

## What was tried

### happy-dom (preferred candidate)

`references/happy-dom-build/` has the full build scaffold: `package.json`
with `npm install happy-dom esbuild linkedom`, an `entry.js` that
re-exports `Window`, and shim modules under `shims/` mapping Node
built-ins to either goja-provided globals or harmless stubs.

esbuild surfaces the following unresolved imports inside happy-dom that
can't be silently aliased:

```
node_modules/happy-dom/lib/index.js                       url
node_modules/happy-dom/lib/window/BrowserWindow.js        util  (TextEncoder/Decoder)
node_modules/happy-dom/lib/browser/utilities/...Script... vm    (Script eval)
node_modules/happy-dom/lib/file/Blob.js                   stream/web (sub-path alias)
node_modules/happy-dom/lib/url/URL.js                     url
node_modules/happy-dom/lib/fetch/Fetch.js                 fs, path, stream, buffer
node_modules/happy-dom/lib/fetch/cache/.../FileSystem.js  fs, path, crypto
```

The non-fetch deps (url, util, vm) sit on the Window/Document/Navigator
hot path that BotGuard would actually exercise. `vm.Script` is
particularly hostile — happy-dom uses it to evaluate scripts inside
sandboxed contexts; we can't reasonably stub Script semantics inside
goja without recursive embedding.

The fetch/file deps are not on the BotGuard-fingerprint hot path so
those CAN be stubbed (the current `shims/` directory has the stubs);
but `vm`, `util`, and `stream/web` all transitively pull in code that
runs at Window construction.

### linkedom (fallback candidate)

`references/happy-dom-build/dist/linkedom.bundle.js` (456 KB) builds
clean and loads inside our goja runtime via `TestLinkedomBundleSmoke`.
The smoke test reports:

```json
{
  "documentToString": "[object Object]",          // expected: [object HTMLDocument]
  "navigatorUA": "Mozilla/5.0 (X11; Linux x86_64) ...",
  "locationHref": "https://www.youtube.com/",
  "windowSelfEq": false,                          // expected: true
  "documentInstanceofDocument": true,             // ✅
  "navigatorInstanceofNavigator": false,          // expected: true
  "elementToString": "[object Object]"            // expected: [object HTMLBodyElement]
}
```

linkedom is a "DOM parser quality" library, not "browser fidelity".
Symbol.toStringTag is not set on most prototypes; `window === self` is
broken; instanceof chains are partial. Would not improve our BotGuard
fingerprint surface beyond what test.49's hand-stubs already cover —
might actually be a regression because it overrides our properly-tagged
hand-stubs.

## What this means for BotGuard

The `webPoSignalOutput length=0 has0=false` decline path observed
across test.41 → test.49 isn't gated on any of the surface-level
fingerprint checks the live integration test (`TestBotGuardLive
Fingerprint`) confirms our hand-stubs now pass:

- toString tags ✅
- constructor.name ✅
- instanceof Class chains ✅
- native function .toString() ✅
- crypto.getRandomValues for Uint32Array ✅
- userAgent as a getter ✅
- Event / WeakRef / FinalizationRegistry constructors ✅

BotGuard is checking something deeper — probably one of:

1. **Timing-based detection**: interpreted-goja vs JIT-compiled-V8
   timing differences in tight loops or crypto operations.
2. **Specific behavioural quirks**: e.g. `getComputedStyle` returning
   real CSS values, real `dispatchEvent` event flow, Canvas/WebGL
   fingerprinting.
3. **Source-level stack inspection**: `Error().stack` strings differ
   between goja and V8 in detectable ways.

None of these are addressable by stubs at the API surface; they need
either a real DOM library (Options A/B blocked above) or a real V8.

## Practical recommendation

**Keep BotGuard degraded**; downloads continue working through the
websafe-fallback rejection path. The investigation infrastructure
stays in tree:

- `internal/goja/fingerprint_diag_test.go` — `TestFingerprintShape`
  diagnostic dump + `TestPrototypeChainContract` (27 instanceof /
  constructor.name guards).
- `internal/goja/linkedom_smoke_test.go` — gated on the bundle
  existing under `references/happy-dom-build/dist/`.
- `internal/bgutils/botguard_live_test.go` — `TestBotGuardLive
  Fingerprint`, gated on `MOOMBOX_LIVE_BG_TEST=1`. Runs the full
  challenge → snapshot → GenerateIT flow against Google.
- `references/happy-dom-build/` — npm + esbuild scaffold, gitignored
  except for `package.json`, `entry.js`, `shims/*.js`. Re-runnable
  with `npm install && npm run build` if a future maintainer wants to
  iterate on the bundling problem.

## Next-iteration paths if BotGuard becomes a hard requirement

1. **Node subprocess for BotGuard only**: spawn a long-lived Node
   process at Moombox startup; IPC over stdin/stdout for challenge,
   snapshot, mint operations; the Node process uses real JSDOM. Cost
   is the Node runtime dependency (~50 MB) — directly conflicts with
   Moombox's "single binary, no CGo" philosophy from CLAUDE.md.
2. **Hand-rolled minimal DOM in JS**: 1500–2500 LoC of JS embedded as
   a single shim, defining `Window` / `Document` / `HTMLElement` /
   `Event` / `CSSStyleDeclaration` / `Storage` etc. with proper
   prototype chain, real toString tags, real event dispatch. Multi-week
   effort. Risk: BotGuard probably checks something we wouldn't
   anticipate, so we'd be iterating blind against a moving target.
3. **Wait**: bgutil-ytdlp-pot-provider (the upstream JSDOM-based
   reference) ships a Node server with HTTP API. Future Moombox could
   talk to a user-installed instance of that server rather than
   trying to embed BotGuard ourselves. Same dependency cost as
   Option 1 but pushes the work to the user.
