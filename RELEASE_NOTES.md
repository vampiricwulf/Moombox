## Bug Fixes

- Fix Goja VM timer callbacks racing with JS execution — callbacks now queue to a channel and drain synchronously on the VM-owning goroutine
- Fix chat file header corruption when `messageCount` digit count crosses a boundary (999 to 1000, etc.) — `updateChatFileHeader` now reads rest data before writing the longer header
- Fix chat file fallback rewrite failing on Windows — restore `f.Close()` before `writeFullChatFile()` so `os.Rename` can overwrite the open file
- Fix YouTube `Init()` permanently failing after a transient error — replace `sync.Once` with mutex+bool that only sets on success
- Fix `n=` signature parameter replacement matching substrings — use boundary-aware replacement
- Fix format selector keeping stale codec score after FPS tiebreaker update
- Fix chat surge false positive — initialize baseline on first callback instead of zero
- Fix quality probe not applying cipher/PO token to manifest URL
- Fix `resolve_url` URL-decode mismatch — extract raw n-param from `RawQuery`
- Fix CDP event loop reading wrong response when interleaved events arrive
- Fix `PersistPlatforms`/`getActivePlatforms` data race on `cfg.Cookies.Platforms`
- Fix `VodChatDownloader.MessageCount()` data race — use `atomic.Int64`
- Fix `recordingStartMs` data race in Twitch chat — use `atomic.Int64`
- Fix `lastSegmentTime`/`consecutiveLiveChecks` races in orchestrator — use atomics
- Fix logger `closed` field race — use `atomic.Bool`
- Fix `GetJobLogs` returning direct slice reference — return copy
- Fix `quitTUI` function pointer race — protect with `sync.Mutex`
- Fix notification event filter bypass when `opts.Event` is empty
- Fix DECAPI monitor not draining response body on errors
- Fix database `Close()` double-close panic — use `sync.Once`
- Fix database path with spaces/special chars failing — URL-encode DSN
- Fix settings restart overlay never appearing in TUI

## Improvements

- Pad `messageCount` to fixed 20-char width in chat JSON headers — eliminates header size changes during in-place updates, making incremental chat writes always same-size
- Add panic recovery to BotGuard timer callbacks, cipher solver closures, and all three monitor goroutines
- Add Windows Job Object protection to setup browser processes for guaranteed cleanup
- Add constant-time comparison for CSRF internal token validation
- Add `filepath.EvalSymlinks` to path traversal guards in orphan cleanup and zip import
- Add database subscriber unsubscription — proper cleanup of `OnJobUpdate`/`OnJobsChange` callbacks on shutdown
- Add `createNoWindow` constant for detached process spawning on Windows
- Add 60-second TTL cache for browser detection to avoid repeated registry/filesystem I/O
- Add `log.Close()` flush before force exit to ensure logs are written
- Cache `runtime.ReadMemStats` with 5-second TTL on `/api/status` to avoid stop-the-world pauses
- Use `crypto/rand` instead of `math/rand` for BotGuard challenge nonces and UserAgent generation
- Use `io.LimitReader` on HTTP response bodies across bgutils, chat, monitor, and utils packages
- Move `OnProgress` callback invocation outside mutex in Twitch chat
- Use insertion-order tracking (`seenOrder` slice) for deterministic dedup pruning in all chat downloaders
- Optimize `filterJobsByAge` with two-pass approach — skip allocation when no jobs are filtered
- Sort platforms list for deterministic config output
- Add import route cleanup on shutdown

## Security

- Replace `crypto.getRandomValues` Goja stub (was `Math.random()`) with Go-native CSPRNG
- Tighten auto-cookie profile directory permissions from 0o755 to 0o700
- Add CSP WebSocket scheme (`ws:`/`wss:`) to Content-Security-Policy header
- Remove dead macOS code paths from browser detection (Windows-only)

## Internal

- Comprehensive codebase audit across all 80+ files with 3 review passes
- Remove dead code: `FetchWatchPageHtml`, `downloadDirectURL`, `LogMsg` type
- Unify `downloadedAt` header replacement to use `replaceQuotedField` helper across all chat packages
- Add `cappedBuffer` for FFmpeg stderr (caps at 1MB, keeps last 512KB)
- Add `recoveryWriter` to track HTTP header state and avoid writing 500 after partial response
- Add `shutdownOnce` to web server to prevent double shutdown
- Add `cancelErr` helper to SegmentDownloader for user-initiated cancellation
- Convert SegmentDownloader `cancelled` flag to `atomic.Bool`
- Add cipher solver insertion-order tracking for deterministic LRU eviction
- Simplify `GetAllJobs` signature (remove unused parameter)
- Add codec score caching in YouTube format selector
- Update CLAUDE.md with current architecture documentation
