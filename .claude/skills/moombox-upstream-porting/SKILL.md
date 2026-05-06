---
name: moombox-upstream-porting
description: Use when analyzing upstream changes from yt-dlp, ejs, or BgUtils references, or when YouTube or Twitch extraction breaks — covers change analysis, correlation, and Go adaptation
---

# Upstream Porting

Moombox tracks upstream repos in `references/` (gitignored) for YouTube/Twitch extraction logic. When extraction breaks or upstream updates, follow this workflow.

## Upstream Sources

| Repo | Tracks | Maps To |
|------|--------|---------|
| **yt-dlp** | YouTube extraction, format selection, auth, HLS/DASH, Twitch extractor | `internal/youtube/`, `internal/twitch/`, `internal/engine/` |
| **ejs** | YouTube signature cipher, n-challenge solving | `bgutil-sidecar/vendor/ejs/` (primary) + `internal/cipher/` (fallback) |
| **BgUtils** | BotGuard protocol, PO token generation | `bgutil-sidecar/` via `bgutils-js` npm dep (primary) + `internal/bgutils/` (fallback) |

Supporting repos: `bgutil-ytdlp-pot-provider` (PO token plugin), `moonarchive` (segment strategies), `chatterino7` (Twitch chat/emotes), `moombox` (original Python version).

## Vendored Sources (sidecar)

Moombox vendors two upstream JS libraries into the BotGuard sidecar:

| Source | Vendored At | How | Pin Mechanism |
|---|---|---|---|
| **yt-dlp/ejs** | `bgutil-sidecar/vendor/ejs/` | Source files copied in (Unlicense, public-domain) | `bgutil-sidecar/vendor/ejs/VERSION` (commit SHA) |
| **bgutils-js** | `bgutil-sidecar/node_modules/bgutils-js` | npm dep | `bgutil-sidecar/package.json` (exact version) |

The sidecar's V8 path is the **primary** cipher (ejs) and BotGuard (bgutils-js) implementation in v2.6.16+. The Go-side `internal/cipher/` and `internal/bgutils/` packages are the **fallback** when the sidecar is disabled or down — they reimplement the same algorithms in goja.

**Dual-update obligation:** When upstream ships a fix:
- **ejs fix** → re-vendor `bgutil-sidecar/vendor/ejs/` to the new commit, bump `VERSION`, run `node build.mjs` to regenerate the bundle. Consider porting the equivalent change to `internal/cipher/` if the goja fallback path needs it (n-decryption mostly; sig is sidecar-only).
- **bgutils-js fix** → bump pin in `bgutil-sidecar/package.json`, run `npm install`, run `node build.mjs`. Consider porting to `internal/bgutils/botguard.go` and friends.
- **yt-dlp fix** → port to `internal/youtube/` or `internal/twitch/`. No sidecar mirror.

## File Mappings

### yt-dlp → Moombox
| yt-dlp Path | Moombox File | What It Covers |
|-------------|-------------|----------------|
| `extractor/youtube.py` | `youtube/player_api.go` | Innertube API, format parsing |
| `extractor/youtube.py` | `youtube/format_selector.go` | Format selection algorithm |
| `extractor/youtube.py` | `youtube/watch_page.go` | Watch page scraping, visitor data |
| `extractor/youtube.py` | `youtube/auth.go` | Client selection (WEB/ANDROID/IOS/TV_EMBEDDED/MWEB/WEB_CREATOR) |
| `extractor/twitch.py` | `twitch/api.go` | GQL API, stream/VOD info |
| `extractor/twitch.py` | `twitch/hls.go` | HLS playlist parsing |
| `extractor/twitch.py` | `twitch/auth.go` | OAuth token handling |
| `downloader/` | `engine/downloader.go` | Segment download strategies |
| `downloader/` | `engine/manifest.go` | DASH manifest parsing |

### ejs → Moombox
| ejs Component | Sidecar File | Fallback (Goja) | What It Covers |
|---------------|-------------|----------------|----------------|
| Cipher algorithm | `bgutil-sidecar/vendor/ejs/` (bundled via esbuild) | `cipher/extractor.go` | Regex extraction of solver candidates from player.js |
| N-challenge | `bgutil-sidecar/vendor/ejs/` (bundled via esbuild) | `cipher/solver.go` | Compiles full player.js in Goja VM, 3-slot LRU cache |
| Transform functions | `bgutil-sidecar/vendor/ejs/` (bundled via esbuild) | `cipher/decrypt.go` | Decrypts signature and n-parameter |

The sidecar runs ejs in V8 (Node.js). The `internal/cipher/` goja implementation is the fallback path when `use_sidecar = false` or the sidecar process is down. Vendored source pinned to commit SHA in `bgutil-sidecar/vendor/ejs/VERSION`.

### BgUtils → Moombox
| BgUtils Component | Sidecar | Fallback (Goja) | What It Covers |
|--------------------|---------|----------------|----------------|
| Challenge handling | `bgutils-js` npm dep in `bgutil-sidecar/` | `bgutils/challenge.go` | Challenge descrambling |
| BotGuard runtime | `bgutils-js` npm dep in `bgutil-sidecar/` | `bgutils/botguard.go` | Goja VM execution |
| Token minting | `bgutils-js` npm dep in `bgutil-sidecar/` | `bgutils/webpo_minter.go` | PO token generation |

The sidecar uses `bgutils-js` (the consumable npm package from LuanRT/BgUtils) running in Node.js. The `internal/bgutils/` goja implementation is the fallback. npm pin in `bgutil-sidecar/package.json`.

## Analysis Workflow

### Step 1: Pull Upstream
```bash
bash references/update-all.sh        # Summary of new commits per repo
bash references/update-all.sh --diff  # File-level diffs (limited to 500 lines per repo)
```

### Step 2: Triage yt-dlp Changes
Focus on: `yt_dlp/extractor/youtube.py`, `yt_dlp/extractor/twitch.py`, `yt_dlp/networking/`, `yt_dlp/utils/`, `yt_dlp/downloader/`

**Priority:** Auth/cookie changes > format selection > extraction logic > download strategies

### Step 3: Triage ejs Changes
Focus on cipher algorithm changes, n-challenge solver updates, new transform functions.

**How cipher works:** Regex patterns extract solver function candidates from player.js → full player.js is compiled into a Goja VM → solver functions are called to decrypt signatures and n-parameters. The "fallback" is alternative regex patterns for different player.js variants (TV/ES6), not a separate solving strategy.

### Step 4: Triage BgUtils Changes
Focus on BotGuard script/protocol, PO token generation, challenge format, integrity token requirements.

### Step 5: Correlate Changes
Changes often cascade:
- **YouTube new challenge** → ejs updates cipher → yt-dlp updates extraction → port both
- **Auth flow changes** → yt-dlp updates cookies/auth → BgUtils may update PO token → port both
- **New format types** → yt-dlp adds extractor logic → may need new engine support

### Step 6: Prioritize
1. **Breaking** — extraction fails, auth broken, cipher outdated (port immediately)
2. **Security/auth** — cookie handling, token generation (port soon)
3. **Format/quality** — new resolutions, codecs (port when convenient)
4. **Optimization** — download strategies, retry logic (port if beneficial)

## Porting Considerations

- **Python → Go**: Translate dict comprehensions, regex groups, dynamic typing to idiomatic Go with proper error handling
- **Python regex**: Go `regexp` doesn't support lookahead/lookbehind — rewrite patterns
- **Goja VM constraints**: No WebAssembly, no native crypto — shimmed via `internal/goja/` (minimal DOM, TextEncoder, timers)
- **VM memory**: BotGuard and cipher VMs hold multi-MB runtimes. Auto-evict when idle. Cipher capped at 3 cached VMs.
- **No CGo**: All solutions must be pure Go

## Common Mistakes

- Porting yt-dlp cipher changes without checking ejs — ejs is often more current for cipher/n-challenge
- Ignoring BgUtils when yt-dlp auth changes — PO token flow may have changed too
- Translating Python regex directly — Go `regexp` doesn't support lookahead/lookbehind
- Not testing with both authenticated and unauthenticated flows after porting auth changes
- Treating cipher as "regex fallback" — it's regex extraction + full player.js compilation, not two separate strategies
- Updating only the sidecar's vendored copy without considering the goja fallback path (or vice versa)
- Forgetting to bump `bgutil-sidecar/vendor/ejs/VERSION` after re-vendoring ejs source files

## Vendored-Source Update Procedures

### Updating vendored ejs

```bash
# 1. Update upstream cache and inspect changes:
bash references/update-all.sh           # see what's new in references/ejs/
cd references/ejs && git log --oneline   # check current upstream HEAD

# 2. Re-vendor the source files matching `bgutil-sidecar/vendor/ejs/`:
cd D:/Git/Moombox
cp references/ejs/src/yt/solver/solvers.ts bgutil-sidecar/vendor/ejs/src/yt/solver/
cp references/ejs/src/yt/solver/nsig.ts    bgutil-sidecar/vendor/ejs/src/yt/solver/
cp references/ejs/src/yt/solver/setup.ts   bgutil-sidecar/vendor/ejs/src/yt/solver/
cp references/ejs/src/yt/solver/main.ts    bgutil-sidecar/vendor/ejs/src/yt/solver/
cp references/ejs/src/types.ts             bgutil-sidecar/vendor/ejs/src/
cp references/ejs/src/utils.ts             bgutil-sidecar/vendor/ejs/src/
cp references/ejs/LICENSE                  bgutil-sidecar/vendor/ejs/

# 3. Update the SHA pin:
(cd references/ejs && git rev-parse HEAD) > bgutil-sidecar/vendor/ejs/VERSION

# 4. Lockstep meriyah/astring versions if upstream changed them:
diff <(cd references/ejs && cat package.json) <(cat bgutil-sidecar/package.json)
# If meriyah or astring versions differ, update bgutil-sidecar/package.json
# to match exactly, then `cd bgutil-sidecar && npm install`.

# 5. Rebuild the sidecar:
cd bgutil-sidecar && node build.mjs

# 6. Run the cipher test suite:
cd D:/Git/Moombox && go test -count=1 ./internal/cipher/...
MOOMBOX_LIVE_CIPHER_TEST=1 go test -count=1 -timeout 180s -run "TestSidecarSolver" ./internal/cipher/...

# 7. If the goja fallback path (internal/cipher/extractor*.go) needs a
#    parallel change, port it. The sidecar parity test
#    (TestSidecarSolverGojaParity) catches output divergence on any
#    fixture goja can statically handle.
```

### Updating bgutils-js

```bash
# 1. Check upstream:
bash references/update-all.sh   # references/BgUtils gets pulled

# 2. Bump the npm pin:
# Edit bgutil-sidecar/package.json — change the bgutils-js version.
# Match upstream as closely as possible; pin exactly if upstream is
# pre-1.0 (current pin: bgutils-js: ^3.2.0).

cd bgutil-sidecar && npm install
cd .. && cd bgutil-sidecar && node build.mjs

# 3. Run BotGuard test suite:
cd D:/Git/Moombox && go test -count=1 ./internal/bgutils/...
MOOMBOX_LIVE_BG_TEST=1 go test -count=1 -timeout 180s -run "TestBotGuardLive\|TestSidecarLive" ./internal/bgutils/...

# 4. Port equivalent changes to internal/bgutils/{botguard,webpo_client,
#    webpo_minter}.go if the BotGuard protocol changed. The goja
#    fallback path needs to track bgutils-js's protocol changes
#    independently of the sidecar's version pin.
```
