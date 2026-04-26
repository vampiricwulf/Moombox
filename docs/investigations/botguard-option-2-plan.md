# BotGuard Option 2 — Hand-rolled minimal DOM

**Status**: Day 1 in progress on the test branch.

**Goal**: replace test.49's flat-object hand-stubs with proper JS classes
implementing real DOM semantics — class hierarchy, real event dispatch
with capture/target/bubble phases, real DOM tree, real CSSStyleDeclaration
— enough fidelity that BotGuard's "decline" branch stops triggering and
`webPoSignalOutput[0]` gets populated with the `getMinter` callback.

**Out of scope**: layout (no rendering, no actual computed positions),
network APIs (fetch / XHR continue rejecting), file APIs, WebGL / Canvas
2D, Web Audio, Service Workers.

## Architecture

Embed the real-DOM JS as a string (`internal/goja/js/dom-real.js`,
loaded via `go:embed`). `RegisterDOMShim` runs the existing Go-side
setup (CSPRNG bridges, `__moomboxUserAgent`) plus the new bundled JS
which installs a properly-classed Window/Document/Element tree. Test
fixtures already in tree (`TestPrototypeChainContract`,
`TestFingerprintShape`, `TestBotGuardLiveFingerprint`) lock the
behaviour down.

Existing test.49 hand-stubs stay during the transition — they're
overwritten when the real-DOM JS reassigns `globalThis.document`,
`globalThis.navigator`, etc. Once the real-DOM is mature enough to pass
the BotGuard fingerprint (validated via `MOOMBOX_LIVE_BG_TEST=1`), the
hand-stub block can be deleted.

## Build order (Day-by-Day plan)

### Day 1 — foundation: EventTarget + Event
- Real `EventTarget` with `addEventListener`/`removeEventListener`/
  `dispatchEvent`. Listener registry with capture/bubble flags, once,
  passive, signal options (subset).
- Real `Event` with `target` / `currentTarget` / `eventPhase` /
  `bubbles` / `cancelable` / `defaultPrevented` properties; `prevent
  Default` / `stopPropagation` / `stopImmediatePropagation`.
- Capture → target → bubble propagation algorithm (WHATWG step-by-step).
- `MessageEvent` / `ErrorEvent` / `CustomEvent` extending `Event`.
- Test: `dispatchEvent(new Event('foo', {bubbles: true}))` on a child
  fires capture listeners on ancestors, target listener, then bubble
  listeners on ancestors. Listener removal works mid-dispatch.

### Day 2 — Node tree + Element/HTMLElement
- `Node` with `parentNode` / `childNodes` (live HTMLCollection) /
  `firstChild` / `lastChild` / `nextSibling` / `previousSibling`,
  `appendChild` / `removeChild` / `insertBefore` / `replaceChild`,
  `nodeType` / `nodeName` / `nodeValue` / `textContent`.
- `Element` extending `Node`: `tagName` / `id` / `className` /
  `attributes` (NamedNodeMap), `getAttribute` / `setAttribute` /
  `removeAttribute` / `hasAttribute`, `classList` (DOMTokenList), `inner
  HTML` / `outerHTML` (string-only, no real parsing — concatenation).
- `HTMLElement` extending `Element`: `style` (CSSStyleDeclaration —
  see Day 3), `dataset`, `hidden`, `tabIndex`, click() / focus() /
  blur() (no-op but reachable).
- Specific subclasses: `HTMLDivElement`, `HTMLBodyElement`,
  `HTMLHeadElement`, `HTMLHtmlElement`, `HTMLAnchorElement`,
  `HTMLImageElement`, `HTMLScriptElement`, `HTMLLinkElement`. Each is
  a no-op subclass (just for `instanceof` to work) except for the
  small handful with semantic getters (`HTMLAnchorElement.href` etc.).
- Test: `document.createElement('div')` returns instance of
  `HTMLDivElement` AND `HTMLElement` AND `Element` AND `Node` AND
  `EventTarget`. `appendChild` updates both sides of the parent/child
  relationship. Live `childNodes` reflects mutations.

### Day 3 — CSSStyleDeclaration + getComputedStyle
- `CSSStyleDeclaration` with the ~70 most common camelCase + dashed
  CSS property accessors (color, backgroundColor, display, position,
  width, height, etc.).
- `setProperty` / `getPropertyValue` / `removeProperty`.
- Inline-style backing store: assigning to `element.style.color`
  updates `element.attributes.style` to `"color: red;"`.
- `getComputedStyle(element)` returns a CSSStyleDeclaration mirror of
  the element's inline styles. Real layout values aren't computable,
  so width/height/top/left/right/bottom default to '0px', display
  defaults to the element's spec default ('block' for div, 'inline'
  for span, etc.).
- Test: setting `el.style.display = 'none'` then reading
  `el.style.display` returns 'none'. `getComputedStyle(el).display`
  matches. Setting via `el.setAttribute('style', 'color: red')`
  populates the live style object.

### Day 4 — Document / HTMLDocument + Window
- `Document` extending `Node` with `createElement` (returns real
  `HTMLElement` subclasses by tag), `createTextNode`, `createComment`,
  `createDocumentFragment`, `createEvent` (legacy).
- `HTMLDocument` extending `Document` with `body`, `head`,
  `documentElement`, `title`, `cookie` (string store), `readyState`,
  `visibilityState`, `hidden`, `URL`, `documentURI`, `domain`,
  `referrer`.
- `Document.querySelector` / `querySelectorAll` — minimal selector
  parser handling `#id`, `.class`, `tag`, descendant combinators.
- `Document.getElementById` / `getElementsByTagName` /
  `getElementsByClassName` (live HTMLCollection variants).
- `Window` extending `EventTarget`: re-binds the existing global
  proxies from test.49 (`location`, `navigator`, `screen`, `history`,
  `performance`, `localStorage`, `sessionStorage`, `crypto`) onto
  itself with proper getters; provides `setTimeout` / `setInterval` /
  `requestAnimationFrame` (already wired by RegisterTimers).
- Build a minimal initial document tree:
  `<html><head><title></title></head><body></body></html>`.
- Test: `document.body.tagName === 'BODY'`. `document.querySelector
  ('body')` returns the body element. `document.createElement('div').
  parentNode === null` until appended.

### Day 5 — DOMTokenList, MutationObserver-as-stub, polish
- `DOMTokenList` for `classList`: `add` / `remove` / `toggle` /
  `contains` / `replace`, length, item(), iterable.
- `MutationObserver` already stubbed; verify `observe` / `disconnect`
  / `takeRecords` accept the right arg shapes.
- `XMLHttpRequest` enhanced: real `open` / `send` / `setRequestHeader`
  / `readyState` transitions (still rejects all network), event
  emission via `onreadystatechange` and `addEventListener('load',
  ...)`.
- `URL` polyfill — implementation of `URL.parse` based on WHATWG URL
  spec (existing host/path/query parsing logic from
  `cipher/resolve_url.go` could inform the JS version).

### Day 6 — BotGuard regression + perf check
- `MOOMBOX_LIVE_BG_TEST=1 go test -v -run TestBotGuardLiveFingerprint
  ./internal/bgutils/...` — does `webPoSignalOutput.length` flip from
  0 to 1?
- If yes: PO tokens generate, integrity token populates, downloads
  upgrade from websafe-fallback to real path. Profile snapshot
  generation cost vs. test.49 baseline.
- If no: dump the post-snapshot console messages, look at what the
  decline path now hits. Iterate.
- VM construction cost: target ≤ 200 ms (test.49 baseline ≈ 50 ms).
  If the new shim pushes construction over budget, cache one shim-
  initialised runtime per cipher request type.

## Testing strategy

Existing tests stay as regression guards:
- `TestPrototypeChainContract` (test.49) — the 27 instanceof checks.
- `TestFingerprintShape` (test.49) — diagnostic dump.
- `TestBotGuardLiveFingerprint` (test.49) — full real flow against
  Google.
- `TestExtractRealPlayerJS*` (cipher) — fixture-based sig + n
  decryption against real player.js samples.

New tests per day:
- Day 1: `TestEventTargetCaptureBubble`, `TestEventTargetOnce`,
  `TestEventTargetSignal`, `TestEventPropagationOrder`.
- Day 2: `TestNodeTreeMutation`, `TestElementClassList`,
  `TestElementInstanceofChain`, `TestHTMLElementSubclasses`.
- Day 3: `TestStyleInlineRoundTrip`, `TestGetComputedStyleDefaults`.
- Day 4: `TestDocumentCreateElement`, `TestDocumentQuerySelector`,
  `TestWindowEventDispatch`.
- Day 5: `TestClassListAPI`, `TestURLParser`.

Each day's tests must keep passing after that day's commit; later
days don't regress earlier days' tests.

## Cipher-cache compatibility

The cipher's preprocessed-code disk cache (Phase 6 Q4) caches the
output of `preprocessPlayerWithBranch` keyed by the player URL hash.
Cached entries reference globalThis.document / navigator / etc. AT
RUNTIME — those names still resolve under the new real-DOM (we still
install document, navigator at the same names with the same property
shape). Existing cache entries should continue to work without a
forced flush.

If a property shape change DOES break cached code (e.g. element.style
changes from plain object to CSSStyleDeclaration), bake a shim-version
string into the cache filename so any future shim change auto-
invalidates without manual cleanup. Pattern: `<key>.v<N>.preprocessed.js`.

## Risk register

- **BotGuard might check something we haven't anticipated** — likely.
  After Day 6 we'll have a much better browser fidelity surface but
  no guarantee it crosses BotGuard's threshold. If it doesn't, we
  iterate by reading the post-snapshot console messages and the
  decline-response prefix.
- **Goja-vs-V8 quirks** — class field syntax, private methods, regex
  unicode flag differences may surface during the build.
  Mitigation: keep the JS to ES2020 baseline that goja's documented
  to support fully.
- **Cipher cache regression** — if real-DOM Document changes
  surfaces the cached player.js execution depends on. Mitigation:
  shim-version stamp in cache key + the existing `TestExtractReal
  PlayerJS*` corpus catches breakage at compile time.
