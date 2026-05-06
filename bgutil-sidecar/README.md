# Moombox BotGuard sidecar (build inputs)

Tiny stdin/stdout JSON-RPC server that wraps [`bgutils-js`](https://github.com/LuanRT/BgUtils)
in a JSDOM. `build.mjs` packages this folder + production `node_modules/` into
a single gzipped tarball at `internal/bgutils/embed/sidecar.tar.gz`, where
Moombox `go:embed`'s it into `moombox.exe`. The Go-side `internal/bgutils/sidecar`
package (Phase 3) extracts this tarball alongside a bundled `node.exe` (from
`tools/fetch-node`, Phase 2) on first launch and pipes JSON-RPC requests to it.

See `docs/investigations/botguard-sidecar-design.md` for the full architecture.

## Local build

```bash
npm ci --omit=dev   # production deps only (jsdom + bgutils-js + transitives)
node build.mjs      # creates dist/sidecar.tar.gz and copies it to ../internal/bgutils/embed/
```

## Vendored sources

This sidecar embeds two upstream libraries:

- **bgutils-js** (npm dep, MIT, [LuanRT/BgUtils](https://github.com/LuanRT/BgUtils)) — BotGuard PO token implementation. Pinned in `package.json`.
- **ejs** (vendored source under `vendor/ejs/`, Unlicense, [yt-dlp/ejs](https://github.com/yt-dlp/ejs)) — YouTube cipher (sig + n) solver. Pinned via the SHA in `vendor/ejs/VERSION`.

Both upstreams are also referenced under `references/` (gitignored) for change tracking. When either ships a fix, the sidecar's vendored copy needs to be updated AND the corresponding goja fallback in `internal/cipher/` (ejs) or `internal/bgutils/` (bgutils-js) needs to be considered. See `.claude/skills/moombox-upstream-porting/SKILL.md` for the procedure.

The `[bgutils]` config section's `use_sidecar = true` (default) makes this sidecar primary; setting it to `false` falls back to the in-process goja implementations.

## Smoke test (without Moombox)

```bash
echo '{"id":1,"method":"ping"}' | node src/server.js
# expect: {"id":1,"result":"pong"}
```

For an end-to-end real-BotGuard mint (hits Google's WAA endpoint):

```bash
echo '{"id":1,"method":"generatePoToken","params":{"binding":"someBindingValue"}}' | node src/server.js
# expect: {"id":1,"result":{"poToken":"...","binding":"someBindingValue","expiresAt":<unixMs>}}
```

## Licenses

- `bgutils-js` — MIT (LuanRT). Source of the BotGuard JS implementation.
- `jsdom` — MIT.

This sidecar is MIT-clean. We deliberately do not depend on
`bgutil-ytdlp-pot-provider` (GPL-3.0-only) — that package's `SessionManager`
is a thin wrapper around `bgutils-js`, and embedding it would force Moombox
to GPL-3.0. We re-implement the wrapper inline in `src/server.js` (~250 lines).
