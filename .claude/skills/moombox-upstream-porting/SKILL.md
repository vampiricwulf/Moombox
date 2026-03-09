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
| **ejs** | YouTube signature cipher, n-challenge solving | `internal/cipher/` |
| **BgUtils** | BotGuard protocol, PO token generation | `internal/bgutils/` |

Supporting repos: `bgutil-ytdlp-pot-provider` (PO token plugin), `moonarchive` (segment strategies), `chatterino7` (Twitch chat/emotes), `moombox` (original Python version).

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
| ejs Component | Moombox File | What It Covers |
|---------------|-------------|----------------|
| Cipher algorithm | `cipher/extractor.go` | Regex extraction of solver candidates from player.js |
| N-challenge | `cipher/solver.go` | Compiles full player.js in Goja VM, 3-slot LRU cache |
| Transform functions | `cipher/decrypt.go` | Decrypts signature and n-parameter |

### BgUtils → Moombox
| BgUtils Component | Moombox File | What It Covers |
|--------------------|-------------|----------------|
| Challenge handling | `bgutils/challenge.go` | Challenge descrambling |
| BotGuard runtime | `bgutils/botguard.go` | Goja VM execution |
| Token minting | `bgutils/webpo_minter.go` | PO token generation |

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
