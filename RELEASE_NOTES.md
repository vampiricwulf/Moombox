### Bug Fixes

- **Resume truncation fallback** — if file truncation fails during download resume, fall back to a fresh start instead of appending after corrupted data
- **Gap reporting off-by-one** — HLS VOD and DASH parallel gap detection now correctly uses inclusive end ranges, matching the convention from HLS live gap reporting
- **Chat empty-ID dedup** — messages with missing IDs are no longer silently dropped by the deduplication check; empty IDs are excluded from the dedup set entirely
- **Chat resume state capped** — resume files now cap stored IDs to the dedup limit (5,000) to prevent unbounded growth between cull cycles
- **WebSocket pong error handling** — pong write failures now properly return instead of silently ignoring the error, consistent with broadcast write handling
- **Cookie parsing indentation** — fixed misleading indentation in cookie expires parsing (cosmetic, not a logic bug)

### Improvements

- **HLS frame rate parsing** — support fractional FRAME-RATE format (e.g., `30000/1001`) per HLS spec, in addition to decimal format
- **Panic recovery** — all 44 goroutines now have panic recovery per project conventions
- **HTML accessibility** — added `lang="en"` to HTML elements in index.html and login.html

### Internal

- **Go modernization** — comprehensive codebase audit replacing legacy patterns with modern Go idioms: `min`/`max` builtins (~90 sites), `strings.Cut`/`CutPrefix` (~45 sites), `strings.SplitSeq` (12 sites), `interface{}` to `any` (60 sites), `range`-over-int (12 sites), `maps.Copy` (7 sites), `slices.SortFunc` (4 sites), `fmt.Fprintf` (4 sites), `cmp.Compare` (6 sites), tagged switches (5 sites)
- **Dead code removal** — removed unused `internal/errors` package, dead utility files (`ffprobe.go`, `ip.go`), and orphaned functions/constants (~1,100 lines)
- **Deduplication** — extracted shared chat file I/O into `twitch/chat_file.go`, consolidated path traversal guards, frontend status constants
- **Dependency migration** — migrated from deprecated `nhooyr.io/websocket` to `github.com/coder/websocket`
- **Linter compliance** — addressed golangci-lint findings: deprecated API usage, struct conversions, error wrapping, loop modernization
