# Arc T — Twitch (sweep-2 fix chain) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the eight verified Twitch defects of sweep 2 (emote 404s, the IRC progress-under-lock write, IRC parse allocations + the useless `membership` CAP, IRCv3 tag unescaping, the resume-sidecar timestamp unit, a 429 with a long `Retry-After`, the VOD-chat orphan staging directory), put a 5 s floor on the IRC resume sidecar, and opt in to Twitch enhanced broadcasts (usher `platform`/`supported_codecs`, `CODECS` parsing, codec-aware source selection) — with the Twitch sections of `docs/spec/platform-services.md` made true again.

**Architecture:** Every change lives inside `internal/twitch/` plus the Twitch sections of one spec doc. No package boundary moves, no exported signature changes: `SelectBestVariant`, `ParseHLSMasterPlaylist`, `BuildUsherLiveURL`, `BuildUsherVodURL`, `ChatDownloader` and `VodChatDownloader` keep the shapes `internal/worker` already calls. New behaviour is additive (two new fields on `TwitchHLSVariant`, one new field on `chatDelays`, one sentinel error, four unexported helpers), and the enhanced-broadcast selection is written so that a playlist with no `CODECS` attribute — every playlist Twitch serves today without the new usher parameters — selects byte-identically to the code being replaced.

**Tech Stack:** Go 1.27 (no CGo), `coder/websocket` (IRC), `net/http` + the package-level `twitchHTTPClient` seam (every HTTP test swaps it), standard `testing` (no assertion library), Markdown spec docs gated by `internal/docs`' citation test.

**Spec:** `docs/superpowers/specs/2026-09-17-sweep2-fix-chain-design.md` — §0 (owner decisions O-S, "Twitch usher", "IRC sidecar"), §2 (global constraints), §3 "Arc T", §4 (order), §5 (rulings). Report rows: `reports/sweep-2026-09-15b.md` #28, #29, #61, #62, #63, #94, #95, #102 (TWITCH-10) plus the two owner rows. Area report: `reports/sweep-2026-09-15b/twitch.md` (TWITCH-4…11, owner choices O2/O4/O5). Verifier: `reports/sweep-2026-09-15b/_verify-engine-twitch.md` (TWITCH-4/5 rows). History that must not be re-litigated: `.superpowers/sdd/2026-09-15-sweep-1-twitch-chat/progress.md` (emote offsets are CODE POINTS on the wire, UTF-16 in the file — settled on 120/120 real ranges) and `.superpowers/sdd/2026-09-15-followup-a-twitch-chat/final-review.md` (the `CloseNow`-before-sentinel ruling; the RECONNECT-before-001 guard).

## Global Constraints

Copied from the spec's §2. Every task's requirements implicitly include this section.

- `go 1.27`, no CGo, pure-Go dependencies; Windows x64 + Linux x64 + Linux arm64 must build (`GOOS=linux GOARCH=amd64 go vet ./...` is part of every merge candidate).
- LF line endings in every file the chain touches. Every commit's LAST TWO LINES are exactly `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq` — the project's rule, which takes precedence over any attribution reminder in an implementer's own context whatever model name it shows.
- The anonymous logger interface (`Debug/Info/Warn/Error(msg string, args ...any)`) stays anonymous per struct. Every goroutine has an inline `defer func() { if r := recover(); … }()`.
- `internal/tui` never imports `internal/web/routes` or `internal/bgutils`.
- Helpers are defined in the task that first uses them (staticcheck U1000 is a hard gate).
- TDD per task: the failing test is written and run red before the change; every new assertion names the mutant that fails it (the reviewer verifies at least one by execution).
- Every JS-touching task gates `go test ./internal/web/routes/` AND `node --test web/tests/*.test.mjs`. Every task that renames, moves or deletes a Go symbol, or edits `docs/spec/*.md`/`SPEC.md`/`CLAUDE.md`, gates `go test ./internal/docs/` (the citation test requires the DECLARING file; after O-Z it also scans `SPEC.md` and `CLAUDE.md`).
- Protected behaviour stays: ~60 Hz progress pipeline and DB write cadence (make updates cheaper, never rarer); the DB layer untouched for perf; `monitors.probe_cooldown` default 0; connectivity-monitor probe design; the BotGuard interpreter gate; `/retry` vs `/resume` gates never shared; the Web cookie import stays unbounded (the timeout); RotateCookies rejected; DPAPI two-pass never; never kill processes by image name; never `rm -rf` under %TEMP%; no Shoelace bundling; extraction mirrors yt-dlp (android_vr retained; VISIONOS split-adaptive; homepage minting); update-path compatibility (old launcher + new child analysed before any launcher/updater/exit-code/swap-artifact change; success paths byte-identical); the setup wizard stays loopback-gated.
- Implementers commit with the pathspec ON the commit (`git add <files> && git commit -m … -- <same files>`); no stash/checkout/rebase/reset/amend; reviewers never edit (they reproduce in `git archive` scratch exports, copying the four gitignored embed blobs when a package needs them — youtube, chat, twitch, worker, engine, monitor, web all pull `internal/bgutils` transitively); scratch test files never named `*_linux_test.go`/`*_windows_test.go`; `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp` on every go command; ONE controller-run `go test -count=1 ./...` at a time.
- User-facing strings: match the twin UI's exact output before mirroring it (read the twin's code).

### Arc-specific constraints

- **Branch `sweep2-t-twitch`, worktree `.worktrees/sweep2-t-twitch`, cut from `main`.** Recipe: `git worktree add -b sweep2-t-twitch .worktrees/sweep2-t-twitch main`, then copy `internal/bgutils/embed/{node-windows-amd64.gz,node-linux-amd64.gz,node-linux-arm64.gz,sidecar.tar.gz}` and `internal/cipher/testdata/*.js` in, then `cd web/tests && npm ci --no-audit --no-fund`.
- **Arc T merges AFTER Arc E** (both touch `internal/twitch/api.go`). Arc E owns exactly one hunk there — the `GetStreamInfo` `(nil, nil)` collapse at `api.go:469-476`. This arc must not touch that function. Merge `main` into the branch before the merge candidate.
- **File set:** `internal/twitch/**` except that `GetStreamInfo` hunk, plus the Twitch sections of `docs/spec/platform-services.md`. Nothing in `internal/engine/**`, `internal/worker/**`, `cmd/**` or `web/**` is edited by this arc.
- **Gates per task:** `go test -count=1 ./internal/twitch/` always; `go test -count=1 ./internal/worker/` on any task that changes variant selection or a downloader's exported behaviour (Tasks 6, 7); `go test -count=1 ./internal/docs/` on any task that edits `docs/spec/platform-services.md` (Tasks 1, 2, 3, 4, 7, 9). No `./...` from an implementer — the controller runs the one full suite.
- **No JS is touched by this arc**, so the node suite is a merge-candidate gate only, not a per-task gate.
- Settled facts that must not be re-derived: Twitch IRC emote/gif offsets index Unicode **code points** on the wire and are emitted as **UTF-16** code units for `player.js`; `TwitchChatMessage.Raw` is KEPT (owner choice O5); the RECONNECT sentinel does `conn.CloseNow()` before returning and is never charged against the reconnect budget.

## File Structure

| File | Responsibility after this arc | Tasks |
|---|---|---|
| `internal/twitch/emotes.go` | Third-party emote fetch + LRU/TTL cache. Gains `errEmoteProviderNotFound`; a 404 becomes an ANSWER with zero emotes; 5xx/transport/parse stay non-answers. | 1 |
| `internal/twitch/chat.go` | IRC downloader state machine. `addMessage` reports progress with `cd.mu` released; new `lastResumeSave` field + `saveResumeStateThrottled`; `ircResumeSaveFloor` constant; `ChatResumeState.Timestamp` written in ms (unchanged) and documented as ms. | 2, 4 |
| `internal/twitch/chat_recording.go` | Flush / part-roll / emote memo. Flush uses the throttled sidecar save; `RollFile` resets the floor so a new part's first sidecar is never skipped. | 2 |
| `internal/twitch/delays.go` | The one timing seam tests drive at ms scale. Gains `resumeSaveFloor`. | 2 |
| `internal/twitch/chat_irc.go` | Wire parsing + session I/O. `parseIRCTags` returns nil for a tagless line; `parseEmoteTags` builds its index table only for non-BMP text; `ircCapRequest` drops `twitch.tv/membership`; new `unescapeIRCTag` applied to `system-msg`, `display-name`, `msg-param-recipient-display-name`. | 3, 4 |
| `internal/twitch/vod_chat.go` | VOD comment paging. Gains a session cancel fired by `Stop()`, a shared interrupted-exit path that always flushes + saves, and a guard that never recreates a removed staging directory. `Timestamp` written in ms. | 4, 6 |
| `internal/twitch/api.go` | GQL + usher URLs. `gqlRequest` stops retrying a 429 whose `Retry-After` exceeds the cap; `BuildUsherLiveURL`/`BuildUsherVodURL` send `platform=web` + `supported_codecs=av1,h265,h264`. **`GetStreamInfo` is Arc E's — do not touch it.** | 5, 7 |
| `internal/twitch/hls.go` | Master-playlist parsing + variant selection. Parses `CODECS`, derives a normalized video-codec family, and prefers the enhanced source when one is offered. | 7 |
| `internal/twitch/types.go` | Wire/struct shapes. `TwitchHLSVariant` gains `Codecs` + `VideoCodec`; `ChatResumeState.Timestamp` documented as epoch **milliseconds** on both paths. | 4, 7 |
| `internal/twitch/emotes_test.go` | + two tests: an all-404 channel is cached with zero emotes; 5xx is still a non-answer. | 1 |
| `internal/twitch/chat_progress_and_sidecar_test.go` (new) | Progress callback runs lock-free; the 5 s sidecar floor and its reset at a part roll. | 2 |
| `internal/twitch/chat_irc_parse_test.go` (new) | Nil tag map, BMP fast path, CAP without membership, IRCv3 unescaping. | 3, 4 |
| `internal/twitch/chat_resume_timestamp_test.go` (new) | Both sidecar writers stamp epoch ms. | 4 |
| `internal/twitch/api_retry_after_test.go` (new) | A 429 with `Retry-After` past the cap returns once instead of retrying. | 5 |
| `internal/twitch/vod_chat_stop_test.go` (new) | `Stop()` aborts the in-flight page; a removed output directory is never recreated. | 6 |
| `internal/twitch/hls_test.go` | + `CODECS` parsing and enhanced-source selection, including the no-`CODECS` byte-identity pin. | 7 |
| `internal/twitch/api_usher_test.go` (new) | Both usher builders carry the two new parameters and keep every old one. | 7 |
| `internal/twitch/hls_live_test.go` (new) | `MOOMBOX_LIVE_TWITCH_TEST=1` capability gate against a real live channel's usher playlist. | 8 |
| `docs/spec/platform-services.md` | Twitch sections made true: emote answer rule (:653), CAP line (:520), IRC sidecar floor + resume examples (:590, :598, :639), enhanced broadcasts (URL Construction / Master Playlist Parsing / Variant Selection), emote cache expiry (:1203), file/line count (:1226). | 1, 2, 3, 4, 7, 9 |

---

### Task 1: Emote providers — a 404 is an answer with zero emotes

Report row #28 / TWITCH-4. Verified by curl on real ids: `141981764` (twitchdev) answers 404 on BTTV **and** FFZ **and** 7TV; `12826` answers 404/200/404. Today every one of those 404s is an error, so `answered` is false, nothing is cached, and `Resolve` re-fires three requests and four Warn lines on every part roll and every stream end for the life of the daemon.

**Files:**
- Modify: `internal/twitch/emotes.go` (`fetchJSON` ~:455-464; `fetchBTTV` ~:248-266; `fetchFFZ` ~:299-318; `fetch7TV` ~:357-376)
- Modify: `internal/twitch/emotes_test.go` (append two tests)
- Modify: `docs/spec/platform-services.md:653`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `var errEmoteProviderNotFound = errors.New("emote provider has no record of this channel")` in `internal/twitch/emotes.go` — package-private, used only by the three fetchers in this task.

- [ ] **Step 1: Write the failing tests**

Append to `internal/twitch/emotes_test.go`. `installEmoteFetchStub`, `testLogger` and `renderingLogger` already exist in this package (`emotes_test.go`, `api_gql_log_hygiene_test.go`).

```go
// TestEmoteResolverTreatsA404AsAnAnswerWithNoEmotes is TWITCH-4 (report row
// #28). A channel registered with none of the three providers answers 404 on
// all three — measured with curl on 2026-09-15: id 141981764 (twitchdev, a
// real channel) answers BTTV 404 / FFZ 404 / 7TV 404. That is the honest state
// of many channels, not a failure, so it must be CACHED: before this fix every
// part roll and stream end of every job on such a channel re-fired three
// requests and wrote four Warn lines, forever.
//
// Mutants each assertion kills:
//   - Resolve returning nil        -> fetchJSON keeps returning a plain error for 404.
//   - the request count going to 6 -> a 404 still reads as "did not answer", so nothing is cached.
//   - a "fetch failed" Warn        -> the 404 arm was added after the Warn instead of before it.
func TestEmoteResolverTreatsA404AsAnAnswerWithNoEmotes(t *testing.T) {
	log := &renderingLogger{}
	er := NewEmoteResolver(log)
	calls := installEmoteFetchStub(t, http.StatusNotFound, `not found`)

	got := er.Resolve(context.Background(), "141981764", "twitchdev")
	if got == nil {
		t.Fatal("Resolve returned nil although all three providers ANSWERED with 404 — " +
			"a channel registered with none of them is a real, cacheable answer")
	}
	if len(got.BTTV) != 0 || len(got.FFZ) != 0 || len(got.SevenTV) != 0 {
		t.Errorf("Resolve = %+v, want all three sets empty", got)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("the first Resolve made %d requests, want 3", n)
	}

	if again := er.Resolve(context.Background(), "141981764", "twitchdev"); again == nil {
		t.Error("the second Resolve returned nil — the zero-emote answer must have been cached")
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("the second Resolve brought the total to %d requests, want 3 — an all-404 "+
			"channel must be cached exactly once", n)
	}
	if n := log.countLinesContaining("every third-party emote provider failed"); n != 0 {
		t.Errorf("%d 'every provider failed' Warn line(s) for an all-404 channel, want 0", n)
	}
	if n := log.countLinesContaining("fetch failed"); n != 0 {
		t.Errorf("%d '<provider> fetch failed' Warn line(s) for a 404, want 0 — a 404 is an "+
			"answer, and the Warn flood it caused is half of what this row is about", n)
	}
}

// TestEmoteResolverKeepsA5xxANonAnswer is the other half of TWITCH-4: ONLY 404
// became an answer. A provider outage must still leave the set uncached so the
// next Resolve retries (the 2026-09-15 curl run saw 7TV answer 500 for a
// nonexistent id, so the two shapes really do arrive together).
//
// Mutant: widening the new arm to every non-200 — Resolve then returns a
// non-nil empty set and the second call makes no requests at all.
func TestEmoteResolverKeepsA5xxANonAnswer(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	calls := installEmoteFetchStub(t, http.StatusInternalServerError, `upstream down`)

	if got := er.Resolve(context.Background(), "chan-5xx", "chan-5xx"); got != nil {
		t.Fatalf("Resolve = %+v, want nil — three 5xx answers are three failures", got)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("the first Resolve made %d requests, want 3", n)
	}
	er.Resolve(context.Background(), "chan-5xx", "chan-5xx")
	if n := calls.Load(); n != 6 {
		t.Errorf("the second Resolve brought the total to %d requests, want 6 — a total "+
			"failure must not be cached", n)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestEmoteResolverTreatsA404|TestEmoteResolverKeepsA5xx' -v ./internal/twitch/
```
Expected: `TestEmoteResolverTreatsA404AsAnAnswerWithNoEmotes` FAILS at "Resolve returned nil although all three providers ANSWERED with 404". `TestEmoteResolverKeepsA5xxANonAnswer` already PASSES (it is the guard, and it must stay green through Step 3).

- [ ] **Step 3: Implement**

In `internal/twitch/emotes.go`, add `"errors"` to the import block, and the sentinel just above `fetchJSON`:

```go
// errEmoteProviderNotFound marks a 404 from a third-party emote provider.
// It is an ANSWER, not a failure: BTTV, FFZ and 7TV all answer 404 for a
// channel that never registered with them, which is the honest state of many
// channels (measured 2026-09-15: id 141981764 answers 404 on all three).
// Reading it as a failure meant nothing was cached, so Resolve re-fired three
// requests and four Warn lines on every part roll and stream end of every job
// on that channel, for the life of the daemon (TWITCH-4).
var errEmoteProviderNotFound = errors.New("emote provider has no record of this channel")
```

In `fetchJSON`, replace the single non-200 arm with:

```go
	if resp.StatusCode == http.StatusNotFound {
		// Drain a bounded prefix so the connection is reusable, then report
		// the sentinel — the caller turns it into an empty, cacheable answer.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, errEmoteProviderNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
```

In `fetchBTTV`, replace the error arm:

```go
	data, err := fetchJSON(ctx, url)
	if err != nil {
		if errors.Is(err, errEmoteProviderNotFound) {
			// A real answer: this channel has no BTTV emotes. Cacheable, and
			// deliberately not a Warn — see errEmoteProviderNotFound.
			er.logger.Debug("bttv has no record of this channel", "channelID", channelID)
			return nil, true
		}
		// Warn rather than Debug — a persistently-down emote provider was
		// invisible at the default log level, so missing emotes looked like
		// a Moombox bug. Audit-finding twitch.md #36.
		er.logger.Warn("bttv fetch failed", "err", err, "channelID", channelID)
		return nil, false
	}
```

The same shape in `fetchFFZ` (`"ffz has no record of this channel"`, keep the `// Audit-finding twitch.md #36 — see fetchBTTV.` comment above the Warn) and in `fetch7TV` (`"7tv has no record of this channel"`). The parse-failure arms below them are untouched: a 200 whose body will not unmarshal is still a non-answer.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/
```
Expected: `ok github.com/vampiricwulf/Moombox/internal/twitch`. All pre-existing emote tests (`TestEmoteResolverDoesNotCacheATotalFailure` drives a transport failure, `TestEmoteResolverRefetchesAfterTheTTL` drives status 0) stay green — neither uses 404.

- [ ] **Step 5: Correct the spec sentence**

`docs/spec/platform-services.md:653`, in `#### Fetch Strategy`. Replace the sentence beginning "Each returns its emotes AND whether it ANSWERED" so the whole paragraph reads:

```markdown
All three providers are fetched in parallel using a `sync.WaitGroup`. Each has an 8-second timeout (`emoteTimeout`). Each returns its emotes AND whether it ANSWERED. Two shapes are answers: a 200 listing no emotes, and a **404** — BTTV, FFZ and 7TV all answer 404 for a channel that never registered with them, and a channel registered with none of the three answers 404 on all three (`errEmoteProviderNotFound`, `internal/twitch/emotes.go`). Only a provider that could not be reached, that answered 5xx, or whose body could not be parsed is a failure. Reading the 404 as a failure meant nothing was cached for such a channel, so `Resolve` re-fired three requests and four Warn lines on every part roll and stream end of every job on it. Failures are logged at warn level and are non-fatal; a 404 logs at debug level.
```

- [ ] **Step 6: Gates**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/ ./internal/docs/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/twitch/
gofmt -l ./internal/twitch
```
Expected: two `ok` lines, clean vet, empty `gofmt -l`.

- [ ] **Step 7: Commit**

```bash
git add internal/twitch/emotes.go internal/twitch/emotes_test.go docs/spec/platform-services.md
git commit -m "$(cat <<'EOF'
fix(twitch): a 404 from an emote provider is an answer with zero emotes

TWITCH-4 (sweep-2 row #28). BTTV/FFZ/7TV all answer 404 for a channel that
never registered with them, and a channel registered with none of the three
answers 404 on all three (curl, 2026-09-15: id 141981764). fetchJSON read that
as an error, so `answered` was false, nothing was cached, and Resolve re-fired
three requests and four Warn lines on every part roll and stream end for the
life of the daemon. A 404 now returns (nil, true) behind the
errEmoteProviderNotFound sentinel; 5xx, transport and parse failures stay
non-answers. platform-services.md:653's "no third-party emotes is a real
answer" is now true of the 404 shape it described.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)" -- internal/twitch/emotes.go internal/twitch/emotes_test.go docs/spec/platform-services.md
```

---

### Task 2: IRC read loop — progress outside the chat lock, and a 5 s floor on the resume sidecar

Report row #29 / TWITCH-5 plus the owner's "IRC sidecar" ruling. Two changes in the same three files, both about what the read loop pays per message. `addMessage` holds `cd.mu` across `callOnProgress`, which reaches `ProgressTracker.maybeUpdate` → `db.UpdateJobFields` under FULL-sync SQLite up to ~60×/s — so the flusher tick, `RollFile` and `MessageCount()` all queue behind an fsync. The YouTube twin releases first (`internal/chat/downloader.go` mutates under the lock, then calls the callback outside it). Separately, the sidecar is fsync+renamed after EVERY flush (≤ 1/s while chat is pending, ~39 KB), beside the chat.json append fsync; the owner ruled a 5 s floor, the VOD path's shape, with the deferred final save on stop unchanged.

**Files:**
- Modify: `internal/twitch/chat.go` (constants block ~:23-45; `ChatDownloader` struct ~:311-332; `saveResumeState` ~:663-688; the exit defer's save at ~:1268 stays unthrottled; `addMessage` ~:1466-1504)
- Modify: `internal/twitch/chat_recording.go` (`flushLocked` ~:66-67; `RollFile`'s `cd.mu` section ~:181-194)
- Modify: `internal/twitch/delays.go`
- Modify: `internal/twitch/chat_keepalive_test.go` (`TestDefaultChatDelaysMatchConstants`)
- Create: `internal/twitch/chat_progress_and_sidecar_test.go`
- Modify: `docs/spec/platform-services.md` (the `#### Reconnection` state-preservation bullet at ~:590 and the `#### Resume State` paragraph under it)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces, all in `internal/twitch`:
  - `const ircResumeSaveFloor = 5 * time.Second` (`chat.go`)
  - field `resumeSaveFloor time.Duration` on `chatDelays` (`delays.go`), set by `defaultChatDelays()`
  - field `lastResumeSave time.Time` on `ChatDownloader`, guarded by `cd.mu` (`chat.go`)
  - `func (cd *ChatDownloader) saveResumeStateThrottled() bool` (`chat.go`) — returns whether it wrote

- [ ] **Step 1: Write the failing tests**

Create `internal/twitch/chat_progress_and_sidecar_test.go`:

```go
package twitch

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// newSidecarTestChatDownloader is a downloader wired to a temp part file, with
// no network and no credentials — everything these tests drive is in-process.
func newSidecarTestChatDownloader(t *testing.T) *ChatDownloader {
	t.Helper()
	return NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin:   "testchan",
		ChannelDisplay: "TestChan",
		StreamID:       "stream-1",
		OutputPath:     filepath.Join(t.TempDir(), "chat.json"),
	}, &testLogger{})
}

// TestIRCProgressCallbackRunsWithTheChatLockReleased is TWITCH-5 (report row
// #29). addMessage's progress callback reaches ProgressTracker -> UpdateJobFields
// under the database's FULL sync, up to ~60x/s on a busy channel; holding cd.mu
// across it queues the flusher tick, RollFile and MessageCount() behind an
// fsync. The YouTube twin (internal/chat/downloader.go) releases first.
//
// TryLock rather than a nested Lock on purpose: sync.Mutex is not reentrant, so
// a nested Lock under the old code would DEADLOCK the test rather than fail it,
// and a hung test is not a red.
//
// Mutant: restoring `defer cd.mu.Unlock()` in addMessage — TryLock then fails
// and sawLockFree stays false.
func TestIRCProgressCallbackRunsWithTheChatLockReleased(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	var sawLockFree atomic.Bool
	var gotCount atomic.Int64
	cd.SetOnProgress(func(count int) {
		gotCount.Store(int64(count))
		if cd.mu.TryLock() {
			sawLockFree.Store(true)
			cd.mu.Unlock()
		}
	})

	cd.addMessage(&TwitchChatMessage{ID: "m1", TimestampMs: 1})

	if got := gotCount.Load(); got != 1 {
		t.Fatalf("the progress callback saw count %d, want 1 — without the callback firing "+
			"this test is vacuous", got)
	}
	if !sawLockFree.Load() {
		t.Error("the progress callback ran while cd.mu was still held: every progress DB " +
			"write blocks the flusher, RollFile and MessageCount()")
	}
}

// TestIRCResumeSidecarObeysTheFloor pins the owner's "IRC sidecar" ruling: a
// 5 s floor on the resume-sidecar save, the VOD path's shape. Today a ~39 KB
// marshal + fsync + rename follows EVERY flush — up to once a second while
// chat is pending, beside the chat.json append fsync.
//
// Driven through the throttle's return value so nothing depends on wall-clock
// timing: a floor of an hour can never elapse inside a test, and a floor of 0
// disables the throttle entirely.
//
// Mutants: flushLocked calling saveResumeState directly (the second call
// returns true); a floor that also swallows the FIRST save (the first call
// returns false).
func TestIRCResumeSidecarObeysTheFloor(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	cd.delays.resumeSaveFloor = time.Hour

	if !cd.saveResumeStateThrottled() {
		t.Fatal("the first sidecar save was skipped — nothing has been written yet, so there " +
			"is no floor to be inside of")
	}
	if cd.saveResumeStateThrottled() {
		t.Error("a second sidecar save inside the floor wrote anyway — the floor is the whole ruling")
	}

	cd.delays.resumeSaveFloor = 0
	if !cd.saveResumeStateThrottled() {
		t.Error("a zero floor must disable the throttle (the shape tests use to drive the old cadence)")
	}
}

// TestIRCFlushDoesNotRewriteTheSidecarInsideTheFloor is the wiring half: the
// PERIODIC flush must go through the throttle. The sidecar is deleted after the
// first flush; if the second flush writes one, the throttle is not wired in.
//
// Mutant: flushLocked keeping its direct cd.saveResumeState() call.
func TestIRCFlushDoesNotRewriteTheSidecarInsideTheFloor(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	cd.delays.resumeSaveFloor = time.Hour
	sidecar := chatResumePath(cd.outputPath)

	cd.addMessage(&TwitchChatMessage{ID: "m1", TimestampMs: 1})
	cd.flush()
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("the first flush wrote no sidecar (%v) — this test cannot say anything without one", err)
	}
	if err := os.Remove(sidecar); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}

	cd.addMessage(&TwitchChatMessage{ID: "m2", TimestampMs: 2})
	cd.flush()
	if _, err := os.Stat(sidecar); err == nil {
		t.Error("the second flush rewrote the sidecar inside the floor — every flush still " +
			"pays a marshal, an fsync and a rename")
	}
}

// TestPartRollClearsTheSidecarFloor guards the one place a floor could cost
// data: RollFile redirects recording to a NEW part whose sidecar does not exist
// yet. If the floor carried over from the closed part, the new part's first
// flush would leave it with no sidecar at all, and a crash inside the window
// would resume it from nothing.
//
// Mutant: dropping the `cd.lastResumeSave = time.Time{}` line from RollFile.
func TestPartRollClearsTheSidecarFloor(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	cd.delays.resumeSaveFloor = time.Hour

	cd.addMessage(&TwitchChatMessage{ID: "m1", TimestampMs: 1})
	cd.flush()

	next := filepath.Join(t.TempDir(), "chat-part2.json")
	cd.RollFile(next, time.Now().UTC().Format(time.RFC3339))

	cd.addMessage(&TwitchChatMessage{ID: "m2", TimestampMs: 2})
	cd.flush()

	if _, err := os.Stat(chatResumePath(next)); err != nil {
		t.Errorf("the new part has no resume sidecar after its first flush (%v) — the floor "+
			"carried across the part boundary", err)
	}
}
```

Also update `TestDefaultChatDelaysMatchConstants` in `internal/twitch/chat_keepalive_test.go` so the new field is pinned: add `resumeSaveFloor: ircResumeSaveFloor,` to the `want` literal and `"resumeSaveFloor": {want.resumeSaveFloor, 5 * time.Second},` to the map it iterates. (That test already fails a zero-valued field, which is the mutant for "added to the struct but not to `defaultChatDelays`".)

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestIRCProgress|TestIRCResumeSidecar|TestIRCFlushDoesNot|TestPartRollClears|TestDefaultChatDelays' ./internal/twitch/
```
Expected: a BUILD failure — `cd.delays.resumeSaveFloor`, `cd.saveResumeStateThrottled` and `ircResumeSaveFloor` are undefined. That is the red for the three sidecar tests. To see the progress test's own red independently, comment out the three sidecar tests, re-run, and confirm `TestIRCProgressCallbackRunsWithTheChatLockReleased` fails at "the progress callback ran while cd.mu was still held"; then restore them.

- [ ] **Step 3: Implement the lock release**

In `internal/twitch/chat.go`, rewrite `addMessage` — the body is unchanged except for the lock discipline and the snapshot:

```go
func (cd *ChatDownloader) addMessage(msg *TwitchChatMessage) {
	cd.mu.Lock()

	if !cd.dedup.Add(msg.ID) {
		cd.mu.Unlock()
		return
	}
	// Prune at 2× threshold to amortize the Keep cost across inserts.
	if cd.dedup.Len() > chatDedupMax*2 {
		cd.dedup.Keep(chatDedupMax)
	}

	// OffsetMs is computed HERE, under the same lock RollFile holds to swap
	// the output file and rebase recordingStartMs — guaranteeing a message's
	// offset base always matches the part file it gets flushed into.
	// Computing it at parse time (outside the lock) let a message parsed
	// just before a roll land in the NEW part with an OLD-base offset,
	// replaying hours out of position.
	baseMs := cd.recordingStartMs.Load()
	if baseMs == 0 {
		baseMs = cd.streamStartMs
	}
	if baseMs > 0 {
		// Signed on purpose: a message that arrived before this part's
		// recording base was sent BEFORE the video starts. The player renders
		// negative offsets as pre-show chat ("-1:30"); clamping to 0 used to
		// pile them onto 0:00 (review 2026-09-03, N-F2).
		msg.OffsetMs = msg.TimestampMs - baseMs
	}

	cd.messages = append(cd.messages, *msg)
	cd.totalCount++
	cd.fileCount++
	if msg.TimestampMs > cd.lastTimestampMs {
		cd.lastTimestampMs = msg.TimestampMs
	}
	total := cd.totalCount
	cd.mu.Unlock()

	// OUTSIDE the lock. This callback is ProgressTracker.SetChatCount, which
	// reaches db.UpdateJobFields under the database's FULL sync up to ~60×/s
	// on a busy channel; reporting under cd.mu queued the flusher tick,
	// RollFile and every MessageCount() behind an fsync (TWITCH-5). The
	// YouTube twin has always released first — internal/chat/downloader.go.
	cd.callOnProgress(total)
}
```

- [ ] **Step 4: Implement the sidecar floor**

`internal/twitch/chat.go`, in the constants block beside `ircKeepaliveCheck`:

```go
	// ircResumeSaveFloor is the minimum gap between two writes of the IRC
	// chat resume sidecar. Owner ruling (sweep-2 "IRC sidecar"), and the VOD
	// path's shape (vodChatFlushInterval).
	//
	// Every flush used to be followed by a ~39 KB marshal, fsync and rename —
	// up to once a second while chat is pending, beside the chat.json append
	// fsync. The sidecar only ever carries the dedup window a reconnect replay
	// can overlap, so a save that is at most five seconds behind the file
	// costs a header undercount that self-heals on the next flush; Twitch IRC
	// has no replay, so nothing else reads it. The DEFERRED final save on stop
	// (Start's exit path) is deliberately NOT throttled.
	ircResumeSaveFloor = 5 * time.Second
```

`internal/twitch/delays.go` — widen the struct doc's first sentence and add the field:

```go
// chatDelays is every timing knob the IRC session sleeps on or measures
// against, in one place so tests can drive the same loop at millisecond scale.
```
```go
type chatDelays struct {
	keepaliveIdle     time.Duration // ircKeepaliveIdle — silence before we speak first
	keepalivePongWait time.Duration // ircKeepalivePongWait — how long an answer may take
	keepaliveCheck    time.Duration // ircKeepaliveCheck — how often the two above are evaluated
	resumeSaveFloor   time.Duration // ircResumeSaveFloor — minimum gap between resume-sidecar writes
}

// defaultChatDelays returns production timing.
func defaultChatDelays() chatDelays {
	return chatDelays{
		keepaliveIdle:     ircKeepaliveIdle,
		keepalivePongWait: ircKeepalivePongWait,
		keepaliveCheck:    ircKeepaliveCheck,
		resumeSaveFloor:   ircResumeSaveFloor,
	}
}
```

`internal/twitch/chat.go`, one field on `ChatDownloader` beside the other `cd.mu`-guarded counters:

```go
	// lastResumeSave is when saveResumeStateThrottled last WROTE. Guarded by
	// cd.mu; the zero value means "never", which always writes. A time.Time
	// in a struct field keeps its monotonic reading, so the comparison below
	// is immune to a wall-clock step (ruling R7b, sweep 1).
	lastResumeSave time.Time
```

And the throttle, directly under `saveResumeState`:

```go
// saveResumeStateThrottled writes the resume sidecar unless one was written
// less than delays.resumeSaveFloor ago. Returns whether it wrote.
//
// This is the periodic path (flushLocked). The exit path in Start calls
// saveResumeState directly and is never throttled: that save is the one a
// restart actually reads.
func (cd *ChatDownloader) saveResumeStateThrottled() bool {
	cd.mu.Lock()
	floor := cd.delays.resumeSaveFloor
	last := cd.lastResumeSave
	if floor > 0 && !last.IsZero() && time.Since(last) < floor {
		cd.mu.Unlock()
		return false
	}
	cd.lastResumeSave = time.Now()
	cd.mu.Unlock()

	cd.saveResumeState()
	return true
}
```

`internal/twitch/chat_recording.go`, in `flushLocked`, replace the trailing save:

```go
	// Save resume state after a flush, no more often than
	// delays.resumeSaveFloor (owner ruling; see ircResumeSaveFloor).
	cd.saveResumeStateThrottled()
```

And in `RollFile`, inside the existing `cd.mu` critical section, immediately after `cd.flushedToDisk = false`:

```go
		// The new part has no sidecar yet, so the floor must not carry across
		// the boundary: its first flush has to write one.
		cd.lastResumeSave = time.Time{}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=3 -race -run 'TestIRC|TestChat|TestPartRoll' ./internal/twitch/
```
Expected: `ok` for both; no race reports. (`-race` matters here: `addMessage` now publishes `total` across the unlock.)

- [ ] **Step 6: Correct the spec**

`docs/spec/platform-services.md`, `#### Reconnection` — replace the state-preservation bullet:

```markdown
- **State preservation**: `flush()` is called before each reconnect. Resume state is written to `{outputPath}.resume.json`, at most once per `ircResumeSaveFloor` (`internal/twitch/chat.go`, 5 s) — `saveResumeStateThrottled` (same file) is what the periodic flush calls, so a busy channel no longer pays a ~39 KB marshal, fsync and rename once a second beside the chat.json append fsync. The floor is cleared at every part boundary (`RollFile`, `internal/twitch/chat_recording.go`) so a new part's first flush always writes its own sidecar, and the DEFERRED save on stop is never throttled — that one is what a restart reads. The cost of the floor is that after a crash the on-disk dedup window can be up to five seconds older than the chat file; Twitch IRC has no replay, so the practical consequence is a header undercount that self-heals on the next flush.
```

In `#### Message Processing`, append one sentence to the paragraph that describes the read loop (or add it as a new final paragraph of that subsection):

```markdown
The read loop reports progress with the chat mutex RELEASED: `addMessage` (`internal/twitch/chat.go`) snapshots the running total inside `cd.mu` and calls the progress callback outside it. That callback reaches the job row through `ProgressTracker` under the database's FULL sync, up to ~60 times a second on a busy channel, and holding the mutex across it queued the flush ticker, `RollFile` and every `MessageCount()` behind an fsync. The YouTube twin (`internal/chat/downloader.go`) has always released first.
```

- [ ] **Step 7: Gates**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/ ./internal/docs/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/twitch/
gofmt -l ./internal/twitch
staticcheck ./internal/twitch/
```
Expected: two `ok`, clean vet, empty `gofmt -l`, clean staticcheck (U1000 would fire if `saveResumeStateThrottled` were unused).

- [ ] **Step 8: Commit**

```bash
git add internal/twitch/chat.go internal/twitch/chat_recording.go internal/twitch/delays.go internal/twitch/chat_keepalive_test.go internal/twitch/chat_progress_and_sidecar_test.go docs/spec/platform-services.md
git commit -m "$(cat <<'EOF'
perf(twitch): report IRC chat progress outside the lock; floor the resume sidecar at 5s

TWITCH-5 (sweep-2 row #29): addMessage held cd.mu across callOnProgress, which
reaches db.UpdateJobFields under FULL-sync SQLite up to ~60x/s, so the flusher
tick, RollFile and MessageCount() all queued behind an fsync. It now snapshots
the total under the lock and reports outside it, matching the YouTube twin.

Owner ruling "IRC sidecar": the resume sidecar is written at most once per
ircResumeSaveFloor (5 s) on the periodic path — the VOD path's shape. RollFile
clears the floor so a new part's first flush always writes its own sidecar, and
the deferred save on stop stays unthrottled.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)" -- internal/twitch/chat.go internal/twitch/chat_recording.go internal/twitch/delays.go internal/twitch/chat_keepalive_test.go internal/twitch/chat_progress_and_sidecar_test.go docs/spec/platform-services.md
```

---

### Task 3: IRC parse path — nil tag map, BMP-only fast path, CAP without `membership`

Report row #61 / TWITCH-6, with owner decision **O-S** ("`twitch.tv/membership` is dropped from the IRC CAP request; `platform-services.md:520` updated"). Measured on the sweep benchmark: a tagless line (JOIN/PART/PONG/CAP/ROOMSTATE) pays 1,240 B / 4 allocs for an empty tag map it never reads; `parseEmoteTags` builds a `[]rune` plus a `[]int` index table (1,320 B / 8 allocs) that is the identity mapping whenever no rune needs a surrogate pair. And the session subscribes to `twitch.tv/membership` — JOIN/PART bursts on channels under 1,000 chatters — although `parseLine` drops JOIN and PART outright (chatterino requests it only to render a user list).

**Files:**
- Modify: `internal/twitch/chat_irc.go` (CAP write ~:240; `parseEmoteTags` ~:862-912; `parseIRCTags` ~:793-807)
- Create: `internal/twitch/chat_irc_parse_test.go`
- Modify: `docs/spec/platform-services.md:520`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces, all in `internal/twitch/chat_irc.go`:
  - `const ircCapRequest = "CAP REQ :twitch.tv/tags twitch.tv/commands"`
  - `func messageHasNonBMP(runes []rune) bool`
  - `func buildCPToUnit(runes []rune) []int`
  - `parseIRCTags(s string) map[string]string` now returns `nil` (not an empty map) for `s == ""`.

- [ ] **Step 1: Write the failing tests**

Create `internal/twitch/chat_irc_parse_test.go`:

```go
package twitch

import (
	"strings"
	"testing"
)

// TestParseIRCTagsReturnsNilForATaglessLine is TWITCH-6 (report row #61).
// Every inbound line without an "@tags" prefix — JOIN, PART, PONG, CAP,
// ROOMSTATE — allocated a 16-slot map (1,240 B / 4 allocs, measured) that no
// caller ever reads. Nil-map reads are legal in Go, and every consumer in
// parseLine/parsePrivmsg/parseUsernotice only ever reads.
//
// Mutant: restoring `tags := make(map[string]string, 16)` before the empty
// check — got is then non-nil and the first assertion fails.
func TestParseIRCTagsReturnsNilForATaglessLine(t *testing.T) {
	got := parseIRCTags("")
	if got != nil {
		t.Errorf("parseIRCTags(\"\") = %v (non-nil), want nil — a tagless line must allocate nothing", got)
	}
	// The nil map has to be SAFE for every read shape the parsers use.
	if v := got["id"]; v != "" {
		t.Errorf("reading a missing key from the nil map returned %q, want \"\"", v)
	}
	if _, ok := got["display-name"]; ok {
		t.Error("the nil map reported a key present")
	}
	if n := len(got); n != 0 {
		t.Errorf("len(nil map) = %d, want 0", n)
	}
}

// TestParseEmoteTagsSkipsTheIndexTableForBMPText is the second half of
// TWITCH-6. The code-point -> UTF-16 index table is the IDENTITY mapping when
// no rune needs a surrogate pair, which is almost every chat message; building
// it cost 1,320 B / 8 allocs per emote message.
//
// The assertion is RELATIVE — BMP strictly cheaper than non-BMP for the same
// tag — so it says nothing about absolute allocator behaviour and cannot rot
// against a Go release.
//
// Mutant: building cpToUnit unconditionally — the two counts then match and
// `bmp < nonBMP` is false.
func TestParseEmoteTagsSkipsTheIndexTableForBMPText(t *testing.T) {
	bmp := testing.AllocsPerRun(100, func() {
		parseEmoteTags("25:0-4", "Kappa hello world")
	})
	nonBMP := testing.AllocsPerRun(100, func() {
		parseEmoteTags("25:0-4", "Kappa hello \U0001F918world")
	})
	if !(bmp < nonBMP) {
		t.Errorf("parseEmoteTags allocated %v for BMP-only text and %v for text with a "+
			"surrogate pair; the BMP case must be strictly cheaper (it needs no index table)",
			bmp, nonBMP)
	}
}

// TestParseEmoteTagsBMPFastPathKeepsTheOffsets proves the fast path is not a
// behaviour change: for BMP-only text the wire code-point offsets and the
// emitted UTF-16 offsets coincide, which is exactly why the table can be
// skipped.
//
// Mutant: a fast path that forgets to slice Name out of the runes, or that
// zeroes Start/End when cpToUnit is nil.
func TestParseEmoteTagsBMPFastPathKeepsTheOffsets(t *testing.T) {
	refs := parseEmoteTags("25:6-10", "hello Kappa world")
	if len(refs) != 1 {
		t.Fatalf("parseEmoteTags returned %d refs, want 1", len(refs))
	}
	if refs[0].Name != "Kappa" || refs[0].Start != 6 || refs[0].End != 10 {
		t.Errorf("ref = %+v, want Name \"Kappa\" Start 6 End 10", refs[0])
	}
}

// TestIRCCapRequestDropsMembership is owner decision O-S. Twitch's
// twitch.tv/membership capability delivers JOIN and PART bursts — every one of
// them dropped by parseLine's default arm — so the session was paying for
// traffic nothing consumes. chatterino requests it only because it renders a
// user list (TwitchIrcServer.cpp); Moombox does not.
//
// Mutant: putting twitch.tv/membership back into ircCapRequest.
func TestIRCCapRequestDropsMembership(t *testing.T) {
	if strings.Contains(ircCapRequest, "twitch.tv/membership") {
		t.Error("the CAP request still asks for twitch.tv/membership — nothing consumes JOIN/PART")
	}
	if !strings.Contains(ircCapRequest, "twitch.tv/tags") {
		t.Error("the CAP request no longer asks for twitch.tv/tags — every emote, badge and " +
			"id Moombox archives rides on that capability")
	}
	if !strings.Contains(ircCapRequest, "twitch.tv/commands") {
		t.Error("the CAP request no longer asks for twitch.tv/commands — USERNOTICE, RECONNECT " +
			"and NOTICE all ride on it")
	}
	if ircCapRequest != "CAP REQ :twitch.tv/tags twitch.tv/commands" {
		t.Errorf("ircCapRequest = %q, want exactly %q", ircCapRequest,
			"CAP REQ :twitch.tv/tags twitch.tv/commands")
	}
}

// TestIRCSessionSendsTheCapRequestVerbatim proves the constant is what actually
// goes on the wire, not a value the session ignores. startIRCReplier records
// the four handshake messages of each connection.
//
// Mutant: leaving the literal in runIRCSession while the constant changes.
func TestIRCSessionSendsTheCapRequestVerbatim(t *testing.T) {
	rep := startIRCReplier(t, []string{welcomeLine})
	cd := newDowngradeTestChatDownloader(t,
		staticCredentials("token-one", "archiveraccount"), &testLogger{}, func(string) {})

	runLiveIRCSession(t, cd)

	lines := rep.nextSession(t)
	var cap string
	for _, l := range lines {
		if strings.HasPrefix(l, "CAP REQ") {
			cap = l
			break
		}
	}
	if cap == "" {
		t.Fatalf("no CAP REQ in the recorded handshake %q", lines)
	}
	if cap != ircCapRequest {
		t.Errorf("the session wrote %q, want ircCapRequest (%q)", cap, ircCapRequest)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestParseIRCTagsReturnsNil|TestParseEmoteTagsSkips|TestParseEmoteTagsBMPFastPath|TestIRCCapRequest|TestIRCSessionSendsTheCap' ./internal/twitch/
```
Expected: a BUILD failure on `ircCapRequest` (undefined). Comment out the two CAP tests and re-run to see the other three: `TestParseIRCTagsReturnsNilForATaglessLine` fails at "(non-nil), want nil", `TestParseEmoteTagsSkipsTheIndexTableForBMPText` fails with two equal alloc counts, and `TestParseEmoteTagsBMPFastPathKeepsTheOffsets` PASSES (it is the no-regression guard). Restore the CAP tests.

- [ ] **Step 3: Implement**

`internal/twitch/chat_irc.go` — the CAP constant, placed with the other session constants near the top of the file:

```go
// ircCapRequest is the capability line every session sends.
//
// twitch.tv/tags carries the emote ranges, badges, message ids and timestamps
// Moombox archives; twitch.tv/commands carries USERNOTICE, NOTICE and
// RECONNECT. twitch.tv/membership is deliberately ABSENT (owner decision O-S):
// it delivers JOIN and PART for channels under 1,000 chatters, parseLine drops
// both, and chatterino only asks for it because it renders a user list
// (references/chatterino7 TwitchIrcServer.cpp).
const ircCapRequest = "CAP REQ :twitch.tv/tags twitch.tv/commands"
```

Replace the CAP write:

```go
	// Request capabilities
	if err := conn.Write(sessionCtx, websocket.MessageText, []byte(ircCapRequest)); err != nil {
		return fmt.Errorf("IRC CAP REQ failed: %w", err)
	}
```

`parseIRCTags` — the empty check moves above the allocation:

```go
// parseIRCTags parses IRC tags from a string like "key=value;key2=value2".
//
// Returns nil for a tagless line. Every consumer only ever READS the map, and
// reads of a nil map are legal, so the 16-slot allocation a JOIN/PART/PONG/
// CAP/ROOMSTATE used to pay for (1,240 B / 4 allocs, measured) buys nothing.
func parseIRCTags(s string) map[string]string {
	if s == "" {
		return nil
	}
	tags := make(map[string]string, 16)
	for pair := range strings.SplitSeq(s, ";") {
		key, value, ok := strings.Cut(pair, "=")
		if ok {
			tags[key] = value
		} else {
			tags[pair] = ""
		}
	}
	return tags
}
```

`parseEmoteTags` — replace the unconditional table build. The doc comment above it (the code-point/UTF-16 explanation) stays exactly as it is; append one paragraph to it:

```go
// The index table is built ONLY when the message actually contains a rune
// outside the BMP. For text that does not — which is almost every message —
// the code-point space and the UTF-16 space are the same space, so the wire
// Start/End are already the values to emit and the table would be the identity
// mapping (1,320 B / 8 allocs per emote message, measured).
```

Body, replacing the `cpToUnit := make(...)` block and the write-back:

```go
	runes := []rune(message)
	// nil when the message is BMP-only: see the paragraph above.
	var cpToUnit []int
	if messageHasNonBMP(runes) {
		cpToUnit = buildCPToUnit(runes)
	}

	var refs []TwitchEmoteRef
	for group := range strings.SplitSeq(emotesStr, "/") {
		emoteID, positions, ok := strings.Cut(group, ":")
		if !ok {
			continue
		}

		for pos := range strings.SplitSeq(positions, ",") {
			startStr, endStr, ok := strings.Cut(pos, "-")
			if !ok {
				continue
			}
			start, err1 := strconv.Atoi(startStr)
			end, err2 := strconv.Atoi(endStr)
			if err1 != nil || err2 != nil {
				continue
			}

			ref := TwitchEmoteRef{ID: emoteID, Start: start, End: end}
			if start >= 0 && start <= end && end < len(runes) {
				ref.Name = string(runes[start : end+1])
				if cpToUnit != nil {
					ref.Start = cpToUnit[start]
					ref.End = cpToUnit[end+1] - 1
				}
			}
			refs = append(refs, ref)
		}
	}

	return refs
}

// messageHasNonBMP reports whether any rune needs a UTF-16 surrogate pair.
func messageHasNonBMP(runes []rune) bool {
	for _, r := range runes {
		if r >= 0x10000 {
			return true
		}
	}
	return false
}

// buildCPToUnit maps code-point index -> UTF-16 index. cpToUnit[i] is the
// UTF-16 index at which code point i begins; the extra entry at len(runes)
// holds the message's total UTF-16 length, which is what makes End computable
// as cpToUnit[end+1]-1 with no special case for a range ending on the last
// code point.
func buildCPToUnit(runes []rune) []int {
	cpToUnit := make([]int, len(runes)+1)
	units := 0
	for i, r := range runes {
		cpToUnit[i] = units
		if r >= 0x10000 {
			units += 2 // surrogate pair
		} else {
			units++
		}
	}
	cpToUnit[len(runes)] = units
	return cpToUnit
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/
```
Expected: `ok`. In particular `TestParseEmoteTagsNonBMP`, `TestParseEmoteTagsOutOfBounds`, `TestParseEmoteTagsInvertedRange` and `TestParseIRCTags` (whose empty-string case compares `len(got)`, so nil is fine) all stay green.

- [ ] **Step 5: Correct the spec**

`docs/spec/platform-services.md:520` — replace the line:

```markdown
Then `ircCapRequest` (`internal/twitch/chat_irc.go`) — `CAP REQ :twitch.tv/tags twitch.tv/commands` — and `JOIN #{channel_login}` (lowercased). `twitch.tv/tags` carries the emote ranges, badges, message ids and timestamps the archive is made of; `twitch.tv/commands` carries USERNOTICE, NOTICE and RECONNECT. `twitch.tv/membership` is deliberately NOT requested (owner decision O-S): it delivers JOIN/PART bursts for channels under 1,000 chatters, `parseLine` drops both, and chatterino asks for it only because it renders a user list (`references/chatterino7` TwitchIrcServer.cpp).
```

- [ ] **Step 6: Gates**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/ ./internal/docs/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/twitch/
gofmt -l ./internal/twitch
staticcheck ./internal/twitch/
```

- [ ] **Step 7: Commit**

```bash
git add internal/twitch/chat_irc.go internal/twitch/chat_irc_parse_test.go docs/spec/platform-services.md
git commit -m "$(cat <<'EOF'
perf(twitch): nil tag map for tagless IRC lines, BMP fast path, CAP without membership

TWITCH-6 (sweep-2 row #61) plus owner decision O-S. parseIRCTags returned a
16-slot map (1,240 B / 4 allocs, measured) for every JOIN/PART/PONG/CAP/
ROOMSTATE although every consumer only reads it; it now returns nil.
parseEmoteTags built a code-point -> UTF-16 index table (1,320 B / 8 allocs)
that is the identity mapping for BMP-only text; it is now built only when the
message actually carries a surrogate pair, and the emitted offsets are
unchanged either way. The CAP request drops twitch.tv/membership: it delivers
JOIN/PART bursts that parseLine discards.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)" -- internal/twitch/chat_irc.go internal/twitch/chat_irc_parse_test.go docs/spec/platform-services.md
```

---

### Task 4: IRCv3 tag unescaping, and one unit for the resume sidecar's timestamp

Report rows #94 (TWITCH-7) and #95 (TWITCH-8). IRCv3 message-tag values are escaped on the wire; today only `\s` is decoded, and only in `system-msg`. `\:` (semicolon), `\\`, `\r` and `\n` are left literal, and `display-name` / `msg-param-recipient-display-name` are never decoded at all (chatterino runs `parseTagString` over all three — `references/chatterino7/src/util/IrcHelpers.hpp:13-56`). Separately, the shared `ChatResumeState.Timestamp` is written in **milliseconds** by the IRC path and **seconds** by the VOD path; nothing reads it, so the two sidecars silently disagree about a field that exists only for humans.

**Planner ruling (spec left this open):** milliseconds for both, matching the already-dominant IRC path and `LastTimestampMs` beside it. The field is kept rather than deleted — it is the only human-readable "when was this written" in the file.

**Planner ruling (escape alphabet):** the IRCv3 message-tags spec is the authority. A backslash followed by anything outside `:srn\` yields that character with the backslash dropped, and a **lone trailing backslash is dropped**. chatterino's loop stops one character short and therefore keeps a trailing backslash; that is a quirk of its in-place `QString::replace` walk, not a rule, and Twitch never emits one.

**Files:**
- Modify: `internal/twitch/chat_irc.go` (`parseUsernotice` ~:738, ~:751, ~:779; `parsePrivmsg` ~:682)
- Modify: `internal/twitch/vod_chat.go` (`saveResumeState` ~:524)
- Modify: `internal/twitch/types.go` (`ChatResumeState` doc ~:157-172)
- Modify: `internal/twitch/chat_irc_parse_test.go` (append)
- Create: `internal/twitch/chat_resume_timestamp_test.go`
- Modify: `docs/spec/platform-services.md` (:598 and :639 JSON examples, and the sentence under each)

**Interfaces:**
- Consumes: `internal/twitch/chat_irc_parse_test.go` from Task 3 (this task appends to it); `newSidecarTestChatDownloader(t *testing.T) *ChatDownloader` from Task 2's `internal/twitch/chat_progress_and_sidecar_test.go`.
- Produces: `func unescapeIRCTag(v string) string` in `internal/twitch/chat_irc.go`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/twitch/chat_irc_parse_test.go`:

```go
// TestUnescapeIRCTag is TWITCH-7 (report row #94). IRCv3 message-tag values
// escape five things on the wire; Moombox decoded one of them, in one tag.
// chatterino applies its equivalent (parseTagString,
// references/chatterino7/src/util/IrcHelpers.hpp) to system-msg, display-name
// and msg-param-recipient-display-name alike.
//
// The last two rows are the spec's own fallbacks and are where implementations
// differ: an unknown escape yields the character with the backslash dropped,
// and a lone TRAILING backslash is dropped. (chatterino keeps the trailing one
// because its in-place walk stops a character short — a quirk, not a rule.)
//
// Mutants: keeping strings.ReplaceAll(v, `\s`, " ") (every row but the first
// and the last fails); decoding `\:` to ':' rather than ';'; emitting the
// backslash for an unknown escape.
func TestUnescapeIRCTag(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"space", `User\ssubscribed\sat\sTier\s1`, "User subscribed at Tier 1"},
		{"semicolon", `a\:b`, "a;b"},
		{"backslash", `a\\b`, `a\b`},
		{"cr and lf", "a\\rb\\nc", "a\rb\nc"},
		{"unknown escape drops the backslash", `a\qb`, "aqb"},
		{"lone trailing backslash is dropped", `trailing\`, "trailing"},
		{"nothing to do", "plain text", "plain text"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unescapeIRCTag(tc.in); got != tc.want {
				t.Errorf("unescapeIRCTag(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestUsernoticeUnescapesEveryTextTag pins the three call sites. A resub
// message quoting a semicolon is the reachable case: Twitch escapes it as \:
// because ';' is the tag separator.
//
// Mutants: leaving any one of the three tags un-decoded — each assertion names
// its own tag.
func TestUsernoticeUnescapesEveryTextTag(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	line := `@id=un-1;msg-id=subgift;system-msg=Ann\sgifted\sa\ssub\sto\sBo\:\snice!;` +
		`display-name=Ann\sB;msg-param-recipient-display-name=Bo\sC;tmi-sent-ts=1700000000000 ` +
		`:tmi.twitch.tv USERNOTICE #testchan :thanks`
	msg := cd.parseLine(line)
	if msg == nil {
		t.Fatal("parseLine returned nil for a well-formed USERNOTICE")
	}
	if msg.SystemMsg != "Ann gifted a sub to Bo; nice!" {
		t.Errorf("SystemMsg = %q, want %q (system-msg)", msg.SystemMsg, "Ann gifted a sub to Bo; nice!")
	}
	if msg.AuthorName != "Ann B" {
		t.Errorf("AuthorName = %q, want %q (display-name)", msg.AuthorName, "Ann B")
	}
	if msg.GiftRecipient != "Bo C" {
		t.Errorf("GiftRecipient = %q, want %q (msg-param-recipient-display-name)",
			msg.GiftRecipient, "Bo C")
	}
}

// TestPrivmsgUnescapesTheDisplayName is the fourth call site: parsePrivmsg's
// author-name fallback chain reads the same tag.
//
// Mutant: leaving parsePrivmsg's `tags["display-name"]` bare.
func TestPrivmsgUnescapesTheDisplayName(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	line := `@id=pm-1;display-name=Ann\sB;tmi-sent-ts=1700000000000 ` +
		`:ann!ann@ann.tmi.twitch.tv PRIVMSG #testchan :hello`
	msg := cd.parseLine(line)
	if msg == nil {
		t.Fatal("parseLine returned nil for a well-formed PRIVMSG")
	}
	if msg.AuthorName != "Ann B" {
		t.Errorf("AuthorName = %q, want %q", msg.AuthorName, "Ann B")
	}
}
```

Create `internal/twitch/chat_resume_timestamp_test.go`:

```go
package twitch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// resumeTimestampFloor is 2020-01-01T00:00:00Z in epoch MILLISECONDS. Any
// seconds-valued stamp taken this decade is three orders of magnitude below
// it, so this one comparison separates the two units without freezing a clock.
const resumeTimestampFloor int64 = 1577836800000

// TestBothChatResumeWritersStampMilliseconds is TWITCH-8 (report row #95). The
// shared ChatResumeState.Timestamp was written in ms by the IRC path and in
// seconds by the VOD path; nothing reads it, so the two sidecars silently
// disagreed about the unit of a field that exists only for a human reading the
// file. Milliseconds wins: it matches the dominant writer and LastTimestampMs
// beside it.
//
// Mutant: restoring time.Now().Unix() in vod_chat.go's saveResumeState — the
// VOD subtest's value drops below the floor by a factor of 1000.
func TestBothChatResumeWritersStampMilliseconds(t *testing.T) {
	t.Run("irc", func(t *testing.T) {
		cd := newSidecarTestChatDownloader(t)
		cd.saveResumeState()
		got := readResumeTimestamp(t, chatResumePath(cd.outputPath))
		if got < resumeTimestampFloor {
			t.Errorf("the IRC sidecar's timestamp is %d, which is seconds, not milliseconds", got)
		}
	})

	t.Run("vod", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "chat.json")
		vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
			VodID:      "v1",
			OutputPath: out,
		}, &testLogger{})
		vcd.saveResumeState(0)
		got := readResumeTimestamp(t, out+".resume.json")
		if got < resumeTimestampFloor {
			t.Errorf("the VOD sidecar's timestamp is %d, which is seconds, not milliseconds", got)
		}
	})
}

// readResumeTimestamp reads one sidecar's `timestamp` field.
func readResumeTimestamp(t *testing.T, path string) int64 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sidecar %s: %v", path, err)
	}
	var state ChatResumeState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("unmarshal sidecar %s: %v", path, err)
	}
	if state.Timestamp == 0 {
		t.Fatalf("sidecar %s carries no timestamp", path)
	}
	return state.Timestamp
}

// TestResumeTimestampFloorIsBelowNow keeps the floor above honest: if the
// constant ever drifted past the current clock, the test above would pass for
// the wrong reason.
func TestResumeTimestampFloorIsBelowNow(t *testing.T) {
	if now := time.Now().UnixMilli(); resumeTimestampFloor >= now {
		t.Fatalf("resumeTimestampFloor (%d) is not in the past (now %d)", resumeTimestampFloor, now)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestUnescapeIRCTag|TestUsernoticeUnescapes|TestPrivmsgUnescapes|TestBothChatResumeWriters|TestResumeTimestampFloor' -v ./internal/twitch/
```
Expected: build failure on `unescapeIRCTag` (undefined). Comment the three unescape tests out and re-run to see `TestBothChatResumeWritersStampMilliseconds/vod` fail with a ten-digit value; restore them.

- [ ] **Step 3: Implement the unescaper**

`internal/twitch/chat_irc.go`, above `parseBadges`:

```go
// unescapeIRCTag decodes the IRCv3 message-tags escape alphabet:
//
//	\:  ->  ;      (the tag separator, so this one is unavoidable on the wire)
//	\s  ->  space
//	\\  ->  \
//	\r  ->  CR
//	\n  ->  LF
//
// A backslash before anything else yields that character with the backslash
// dropped, and a lone TRAILING backslash is dropped — both are the spec's own
// fallbacks. (chatterino's parseTagString keeps a trailing backslash because
// its in-place walk stops one character short; that is a quirk of the walk,
// not a rule, and Twitch does not emit one.)
//
// Applied to system-msg, display-name and msg-param-recipient-display-name —
// the same three chatterino decodes. Before this, only \s was decoded and only
// in system-msg, so a system message quoting a semicolon archived as "Bo\:"
// (TWITCH-7).
func unescapeIRCTag(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' {
			b.WriteByte(v[i])
			continue
		}
		if i+1 >= len(v) {
			break // lone trailing backslash: dropped
		}
		i++
		switch v[i] {
		case ':':
			b.WriteByte(';')
		case 's':
			b.WriteByte(' ')
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteByte(v[i])
		}
	}
	return b.String()
}
```

Four call sites:

`parseUsernotice`:
```go
	// System message (IRCv3 tag escapes decoded: \s \: \\ \r \n)
	systemMsg := unescapeIRCTag(tags["system-msg"])
```
```go
	// Author name fallback chain
	authorName := unescapeIRCTag(tags["display-name"])
```
```go
	if v := unescapeIRCTag(tags["msg-param-recipient-display-name"]); v != "" {
		msg.GiftRecipient = v
	}
```

`parsePrivmsg`:
```go
	authorName := unescapeIRCTag(tags["display-name"])
```

(`login`, `user-id`, `color`, `badges`, `emotes`, `msg-param-sub-plan`, `msg-param-color` and `msg-param-viewerCount` are byte-restricted or numeric on the wire and carry no escapes; leave them bare.)

- [ ] **Step 4: Implement the one timestamp unit**

`internal/twitch/vod_chat.go`, in `saveResumeState`:

```go
		Timestamp:         time.Now().UnixMilli(),
```

`internal/twitch/types.go`, extend the `ChatResumeState` doc comment with a second paragraph:

```go
// Timestamp is epoch MILLISECONDS on BOTH paths. It exists only so a human
// reading a sidecar can see when it was written — nothing loads it — and until
// sweep 2 the IRC writer used milliseconds while the VOD writer used seconds,
// so two files in the same staging tree disagreed about the unit by a factor
// of a thousand (TWITCH-8).
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/
```
Expected: `ok`. Existing tests that assert on `system-msg` (`TestParseLineUsernotice`) keep passing — `\s` still decodes to a space.

- [ ] **Step 6: Correct the spec**

`docs/spec/platform-services.md`, IRC `#### Resume State` (~:592-602): change `"timestamp": 1709000000,` to `"timestamp": 1709000000000,` and append after the fenced block:

```markdown
`timestamp` is epoch MILLISECONDS on both chat paths (`ChatResumeState`, `internal/twitch/types.go`). Nothing loads it — it is there so a human reading a sidecar can see when it was written — and until sweep 2 the IRC writer used milliseconds while the VOD writer used seconds, so two files in the same staging tree disagreed about the unit by a factor of a thousand.
```

VOD `#### Resume State` (~:633-644): change `"timestamp": 1709000000,` to `"timestamp": 1709000000000,` (the paragraph below it already covers the shared cap; the unit sentence above is the single statement of the rule).

Also append to `#### Message Processing`:

```markdown
IRCv3 tag values are decoded through `unescapeIRCTag` (`internal/twitch/chat_irc.go`) — `\:` to `;`, `\s` to a space, `\\`, `\r` and `\n`, with an unknown escape yielding its character and a lone trailing backslash dropped. It is applied to `system-msg`, `display-name` and `msg-param-recipient-display-name`, the same three chatterino decodes (`references/chatterino7` IrcHelpers.hpp). The reachable case is a system message that quotes a semicolon: `;` separates tags on the wire, so Twitch must escape it.
```

- [ ] **Step 7: Gates**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/ ./internal/docs/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/twitch/
gofmt -l ./internal/twitch
staticcheck ./internal/twitch/
```

- [ ] **Step 8: Commit**

```bash
git add internal/twitch/chat_irc.go internal/twitch/vod_chat.go internal/twitch/types.go internal/twitch/chat_irc_parse_test.go internal/twitch/chat_resume_timestamp_test.go docs/spec/platform-services.md
git commit -m "$(cat <<'EOF'
fix(twitch): decode IRCv3 tag escapes; one unit for the chat resume timestamp

TWITCH-7 (sweep-2 row #94): only \s was decoded, and only in system-msg, so a
system message quoting a semicolon archived as "Bo\:" and display names were
never decoded at all. unescapeIRCTag implements the five-escape alphabet with
the spec's fallbacks (unknown escape drops the backslash; a lone trailing
backslash is dropped) and is applied to system-msg, display-name and
msg-param-recipient-display-name — the three chatterino decodes.

TWITCH-8 (row #95): ChatResumeState.Timestamp was milliseconds on the IRC path
and seconds on the VOD path, with no reader to notice. Milliseconds on both.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)" -- internal/twitch/chat_irc.go internal/twitch/vod_chat.go internal/twitch/types.go internal/twitch/chat_irc_parse_test.go internal/twitch/chat_resume_timestamp_test.go docs/spec/platform-services.md
```

---

### Task 5: A GQL 429 whose `Retry-After` exceeds the cap stops retrying

Report row #62 / TWITCH-9. `gqlRequest` honours `Retry-After` only when it is `<= gqlMaxRetryDelay` (30 s). A 429 asking for two minutes therefore falls through to the 1 s / 2 s / 4 s schedule — three quick retries into a throttle Twitch explicitly asked us to respect — and the monitor cycle repeats the same three 15 s later.

**Planner ruling (the row offered two shapes):** **return the 429 immediately**, without further retries. Sleeping the 30 s cap inside the call would park a monitor batch goroutine for the whole window and then still stack the caller's own cadence on top; returning lets the caller's cycle BE the backoff, which is the behaviour the row names as sufficient. The error string is byte-identical to the existing 429 error so `worker.classifyProbeErr`'s positional read is untouched.

**Arc boundary:** this edits `gqlRequest` only. `GetStreamInfo` (~:452-513) is Arc E's hunk — do not touch it.

**Files:**
- Modify: `internal/twitch/api.go` (`gqlRequest` doc ~:186-197 and its 429 branch ~:242-260)
- Create: `internal/twitch/api_retry_after_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: no new symbols; `gqlRequest` keeps its signature `(ctx context.Context, opName string, body any, authToken string) (json.RawMessage, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/twitch/api_retry_after_test.go`:

```go
package twitch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// errUnexpectedStubHost marks a request the 429 stub was never meant to see —
// a misrouted request must fail the test, not pass it silently.
var errUnexpectedStubHost = errors.New("stub received an unexpected request host")

// install429Stub answers every GQL request with 429 and the given Retry-After
// header, and returns the request counter.
func install429Stub(t *testing.T, retryAfter string) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(req.URL.String(), constants.TwitchURLs.GQL) {
			return nil, errUnexpectedStubHost
		}
		calls.Add(1)
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		if retryAfter != "" {
			h.Set("Retry-After", retryAfter)
		}
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     h,
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    req,
		}, nil
	})}
	return &calls
}

// TestGQL429WithALongRetryAfterReturnsWithoutRetrying is TWITCH-9 (report row
// #62). A 429 whose Retry-After exceeds gqlMaxRetryDelay was retried on the
// 1s/2s/4s schedule instead — three quick requests into a throttle Twitch had
// just asked us to respect, and the monitor cycle repeated them 15 s later.
//
// The planner's ruling of the row's two options: return at once and let the
// caller's cycle cadence be the backoff, rather than parking a monitor batch
// goroutine for the 30 s cap and THEN letting the caller's cadence stack on it.
//
// Mutants: the pre-fix `ra > 0 && ra <= gqlMaxRetryDelay` condition alone (the
// call makes gqlMaxRetries+1 requests); a fix that returns the "exhausted"
// wrapper instead of the 429 error (the message assertion fails, and
// worker.classifyProbeErr reads the status positionally out of that string).
func TestGQL429WithALongRetryAfterReturnsWithoutRetrying(t *testing.T) {
	prevDelay := gqlBaseRetryDelay
	gqlBaseRetryDelay = time.Millisecond
	t.Cleanup(func() { gqlBaseRetryDelay = prevDelay })

	calls := install429Stub(t, "120") // 2 minutes: four times the 30 s cap
	log := &renderingLogger{}
	a := NewAPI(log)

	start := time.Now()
	_, err := a.gqlRequest(context.Background(), "TestOp", map[string]any{"q": 1}, "")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a 429 must not succeed")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the stub answered %d times, want exactly 1 — a Retry-After past the cap must "+
			"end the attempt, not start a 1s/2s/4s schedule", n)
	}
	if elapsed > 5*time.Second {
		t.Errorf("gqlRequest took %v — it must return rather than sleep out the window", elapsed)
	}
	if !strings.Contains(err.Error(), "gql rate limited (429) (TestOp): ") {
		t.Errorf("err = %q, want the ordinary 429 error — worker.classifyProbeErr reads the "+
			"status positionally out of it", err)
	}
	if n := log.countLinesContaining("twitch gql retry"); n != 0 {
		t.Errorf("%d retry Debug line(s), want 0", n)
	}
}

// TestGQL429WithAShortRetryAfterStillRetries guards the other side: a
// Retry-After inside the cap is still honoured and still retried, which is the
// behaviour audit-finding twitch.md #11 put there.
//
// Mutant: a fix that returns on EVERY 429 carrying a Retry-After — the call
// then makes 1 request instead of gqlMaxRetries+1.
func TestGQL429WithAShortRetryAfterStillRetries(t *testing.T) {
	prevDelay := gqlBaseRetryDelay
	gqlBaseRetryDelay = time.Millisecond
	t.Cleanup(func() { gqlBaseRetryDelay = prevDelay })

	calls := install429Stub(t, "1") // 1 s: inside gqlMaxRetryDelay
	a := NewAPI(&testLogger{})

	if _, err := a.gqlRequest(context.Background(), "TestOp", map[string]any{"q": 1}, ""); err == nil {
		t.Fatal("a 429 must not succeed")
	}
	if n := calls.Load(); n != gqlMaxRetries+1 {
		t.Errorf("the stub answered %d times, want %d — a Retry-After inside the cap is a "+
			"retry hint, not a stop", n, gqlMaxRetries+1)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestGQL429With' -v ./internal/twitch/
```
Expected: `TestGQL429WithALongRetryAfterReturnsWithoutRetrying` FAILS at `the stub answered 4 times, want exactly 1`. `TestGQL429WithAShortRetryAfterStillRetries` PASSES (the guard).

- [ ] **Step 3: Implement**

`internal/twitch/api.go` — replace the 429 branch inside `gqlRequest`:

```go
		// 429: respect Retry-After. Inside gqlMaxRetryDelay it becomes THIS
		// retry's delay; beyond it, retrying is the wrong answer altogether —
		// three requests on the 1s/2s/4s schedule walk straight into a
		// throttle Twitch just asked us to sit out, and the caller's own cycle
		// (the monitor's 15 s, a quality probe's 30 s) is already a better
		// backoff than anything this loop can offer (TWITCH-9).
		if statusCode == http.StatusTooManyRequests {
			ra := parseRetryAfter(hdrRetryAfter)
			lastErr = fmt.Errorf("gql rate limited (429) (%s): %s", opLabel(opName), gqlBodySize(respData))
			lastStatus = statusCode
			if ra > gqlMaxRetryDelay {
				if a.logger != nil {
					a.logger.Warn("twitch gql 429 asked for longer than we retry; deferring to the caller's cadence",
						"op", opLabel(opName), "retry_after", ra.String(), "cap", gqlMaxRetryDelay.String())
				}
				return nil, lastErr
			}
			if ra > 0 {
				if a.logger != nil {
					a.logger.Debug("twitch gql 429 honoring Retry-After", "op", opLabel(opName), "wait", ra.String())
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(ra):
				}
				// Retry-After already served as this retry's delay — don't
				// stack the exponential backoff on top of it.
				skipBackoff = true
			}
			continue
		}
```

And update the function's doc paragraph:

```go
// Transient failures (5xx, 429, transport errors) are retried with
// exponential backoff (1s → 2s → 4s) up to gqlMaxRetries. A 429 honors
// `Retry-After` when it is within gqlMaxRetryDelay (that wait replaces the
// retry's own backoff); a `Retry-After` LONGER than the cap returns the 429 at
// once, so the caller's cycle cadence is the backoff rather than three quick
// retries into a throttle Twitch asked us to respect. Auth failures (401/403)
// and other 4xx responses are not retried — they need a different recovery
// path (re-login, fix caller).
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/
```
Expected: `ok`. `TestGQLRetriedArmsNeverLogOrReturnBody`'s 429 arm drives a 429 with **no** `Retry-After` header, so it still sees `gqlMaxRetries` retries and stays green.

- [ ] **Step 5: Gates**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/twitch/
gofmt -l ./internal/twitch
staticcheck ./internal/twitch/
```
(No docs gate: this task edits no `docs/spec/*.md`.)

- [ ] **Step 6: Commit**

```bash
git add internal/twitch/api.go internal/twitch/api_retry_after_test.go
git commit -m "$(cat <<'EOF'
fix(twitch): a 429 asking for longer than we retry returns instead of retrying

TWITCH-9 (sweep-2 row #62). Retry-After was honoured only when it fit inside
gqlMaxRetryDelay; a 429 asking for two minutes fell through to the 1s/2s/4s
schedule — three quick requests into a throttle Twitch had just asked us to sit
out, repeated by the next monitor cycle 15 s later. gqlRequest now returns the
429 immediately in that case so the caller's own cadence is the backoff, and
logs one Warn naming the wait. A Retry-After inside the cap still sleeps and
still retries. The error string is unchanged: worker.classifyProbeErr reads the
status positionally out of it.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)" -- internal/twitch/api.go internal/twitch/api_retry_after_test.go
```

---

### Task 6: VOD chat — `Stop()` aborts the in-flight page, and a removed staging directory is never recreated

Report row #63 / TWITCH-11. `Stop()` only flips `running`, so a goroutine sitting in a GQL page fetch is bounded by the 30 s client timeout × 4 attempts plus ~7 s of backoff — far past the orchestrator's 2 s grace. The job finalizes, `processJob` does `os.RemoveAll(stagingDir)`, and the goroutine's exit path then calls `flush()`, whose `os.MkdirAll` **recreates** `<staging>/<jobID>/` and drops a `chat.json` plus a resume sidecar into a tree Moombox never cleans.

A second, smaller bug rides with the cancel: the fetch-error branch's `case <-ctx.Done(): return ctx.Err()` returns **without flushing or saving**, so once `Stop()` starts cancelling the context, a cancel landing in that branch would lose the pending batch and the sidecar that today's `!running` exit preserves. Both exits now go through one helper.

**Files:**
- Modify: `internal/twitch/vod_chat.go` (struct ~:39-71; `Start` ~:139-360; `flush` ~:409-465; `saveResumeState` ~:512-534; `Stop` ~:557-559)
- Create: `internal/twitch/vod_chat_stop_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces, all in `internal/twitch/vod_chat.go`:
  - `func (vcd *VodChatDownloader) finishInterrupted(contentOffset float64)`
  - `func (vcd *VodChatDownloader) outputDirGone() bool`
  - fields `sessionCancelMu sync.Mutex`, `sessionCancel context.CancelFunc`, `wroteFile atomic.Bool`
  - `Stop()` keeps its signature `func (vcd *VodChatDownloader) Stop()`

- [ ] **Step 1: Write the failing tests**

Create `internal/twitch/vod_chat_stop_test.go`:

```go
package twitch

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// installBlockingGQLStub answers every GQL request by parking until the
// request's own context is cancelled, then reporting that cancellation — the
// exact shape of a page fetch in flight when the job finalizes.
func installBlockingGQLStub(t *testing.T) chan struct{} {
	t.Helper()
	entered := make(chan struct{}, 1)
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	return entered
}

// TestVodChatStopAbortsTheInFlightPage is TWITCH-11 (report row #63). Stop()
// only flipped `running`, so a goroutine mid-GQL was bounded by the 30 s client
// timeout times four attempts plus ~7 s of backoff — minutes past the
// orchestrator's 2 s grace, during which the job finalized and removed staging.
//
// Mutant: dropping the sessionCancel call from Stop() — Start never returns and
// this test fails on its own 5 s deadline instead of hanging the suite.
func TestVodChatStopAbortsTheInFlightPage(t *testing.T) {
	entered := installBlockingGQLStub(t)
	out := filepath.Join(t.TempDir(), "chat.json")
	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: out,
	}, &testLogger{})

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- nil
			}
		}()
		done <- vcd.Start(context.Background())
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the downloader never reached a page fetch; the stub was not exercised")
	}

	vcd.Stop()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return within 5 s of Stop() — the in-flight page was not " +
			"cancelled, so it outlives the orchestrator's 2 s grace and the staging removal")
	}
}

// TestVodChatFlushNeverRecreatesARemovedOutputDirectory is the orphan half of
// TWITCH-11: the goroutine's exit flush called os.MkdirAll unconditionally, so
// a job that had already removed <staging>/<jobID>/ got it back, holding a
// chat.json and a resume sidecar nothing ever cleans up.
//
// Mutant: removing the outputDirGone() guard from flush() — the directory
// exists again after the second flush.
func TestVodChatFlushNeverRecreatesARemovedOutputDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "job-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	out := filepath.Join(dir, "chat.json")
	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: out,
	}, &testLogger{})

	vcd.messages = []TwitchChatMessage{{ID: "c1", Message: "first"}}
	vcd.totalCount.Store(1)
	vcd.flush()
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("the first flush wrote no file (%v) — this test says nothing without one", err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove staging: %v", err)
	}

	vcd.messages = []TwitchChatMessage{{ID: "c2", Message: "second"}}
	vcd.totalCount.Store(2)
	vcd.flush()
	vcd.saveResumeState(0)

	if _, err := os.Stat(dir); err == nil {
		t.Error("the exit flush recreated the removed staging directory — an orphan tree " +
			"holding a chat.json and a resume sidecar that nothing ever cleans")
	}
}

// TestVodChatFirstFlushStillCreatesItsDirectory guards the other edge: the
// guard must not stop a genuine first write. Before any file exists the
// directory is the worker's own staging dir and MkdirAll is the ordinary path.
//
// Mutant: an unconditional dir-exists guard (no wroteFile gate) — the first
// flush then writes nothing.
func TestVodChatFirstFlushStillCreatesItsDirectory(t *testing.T) {
	out := filepath.Join(t.TempDir(), "not-created-yet", "chat.json")
	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: out,
	}, &testLogger{})

	vcd.messages = []TwitchChatMessage{{ID: "c1", Message: "first"}}
	vcd.totalCount.Store(1)
	vcd.flush()

	if _, err := os.Stat(out); err != nil {
		t.Errorf("the first flush wrote nothing (%v) — the orphan guard must only fire for a "+
			"directory that existed and was removed", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestVodChat(Stop|Flush|First)' -v ./internal/twitch/
```
Expected: `TestVodChatStopAbortsTheInFlightPage` FAILS at "Start did not return within 5 s of Stop()"; `TestVodChatFlushNeverRecreatesARemovedOutputDirectory` FAILS at "the exit flush recreated the removed staging directory"; `TestVodChatFirstFlushStillCreatesItsDirectory` PASSES (the guard).

Note on the red run: with the pre-fix `Stop()`, nothing cancels the request context, so the `Start` goroutine stays parked in the stub for the rest of the test binary. That is the defect the test names, it costs one leaked goroutine in a run that is already failing, and it goes away with Step 3 — do not "fix" it by giving the test a cancellable parent context, which would hide exactly the mutant being killed.

- [ ] **Step 3: Implement the session cancel**

`internal/twitch/vod_chat.go` — add to the struct, below `onProgress`:

```go
	// sessionCancel aborts the page fetch in flight. Stop() fires it so a
	// goroutine parked in a GQL round trip unwinds inside the orchestrator's
	// 2 s grace instead of outliving the job's staging removal (TWITCH-11).
	// The IRC path's interruptSession is the same shape.
	sessionCancelMu sync.Mutex
	sessionCancel   context.CancelFunc

	// wroteFile records that this downloader has successfully written its
	// chat file at least once — the precondition for reading a missing output
	// directory as "the job removed staging under us" rather than "the first
	// write has not happened yet". See outputDirGone.
	wroteFile atomic.Bool
```

In `Start`, immediately after the `defer` block that recovers, before the first log line:

```go
	// Derive a cancellable context so Stop() can abort a page fetch in flight.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	vcd.sessionCancelMu.Lock()
	vcd.sessionCancel = cancel
	vcd.sessionCancelMu.Unlock()
```

Rewrite `Stop`:

```go
// Stop cancels the VOD chat download: the loop stops paging AND the page fetch
// in flight is aborted, so the goroutine unwinds inside the orchestrator's
// grace window rather than minutes later, after staging has been removed
// (TWITCH-11). running is cleared FIRST so the exit path takes the
// "stopped before completion" branch, which preserves the resume sidecar.
func (vcd *VodChatDownloader) Stop() {
	vcd.running.Store(false)
	vcd.sessionCancelMu.Lock()
	cancel := vcd.sessionCancel
	vcd.sessionCancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}
```

- [ ] **Step 4: Implement the one interrupted-exit path and the orphan guard**

Add below `pagingStalled`:

```go
// finishInterrupted is the exit every path that ends BEFORE the VOD's last
// page shares: flush what is buffered and keep the resume sidecar, so a
// relaunch inside the same job (or a restart of a still-Downloading job)
// continues from this offset. It never deletes the sidecar and never enriches:
// an enriched file must not receive further appends.
//
// Reaching it through a context cancellation is new. Before Stop() cancelled
// the session, the fetch-error branch's cancel arm returned ctx.Err() with no
// flush and no save at all — which, once Stop() starts cancelling, would have
// thrown away exactly the batch the pre-cancel code preserved.
func (vcd *VodChatDownloader) finishInterrupted(contentOffset float64) {
	vcd.flush()
	vcd.saveResumeState(contentOffset)
	vcd.logger.Info("VOD chat download stopped before completion; resume state preserved",
		"vodID", vcd.vodID, "offset", contentOffset, "messages", vcd.totalCount.Load())
}

// outputDirGone reports whether the directory that held the chat file has been
// removed under this downloader — the finalize race of TWITCH-11: the job
// removed its staging tree while this goroutine was mid-GQL. Only meaningful
// once a file has been written; before that the directory is the worker's
// freshly-created staging dir and creating it is the ordinary first-write path.
func (vcd *VodChatDownloader) outputDirGone() bool {
	if vcd.outputPath == "" || !vcd.wroteFile.Load() {
		return false
	}
	_, err := os.Stat(filepath.Dir(vcd.outputPath))
	return err != nil && os.IsNotExist(err)
}
```

Rewire the three exits in `Start`:

```go
		select {
		case <-ctx.Done():
			vcd.finishInterrupted(contentOffset)
			return nil
		default:
		}
```
```go
			vcd.logger.Warn("vod chat fetch error", "err", err, "consecutive", consecutiveErrors)
			select {
			case <-ctx.Done():
				vcd.finishInterrupted(contentOffset)
				return ctx.Err()
			case <-time.After(2 * time.Duration(consecutiveErrors) * time.Second):
			}
			continue
```
and the post-loop branch (the comment above it stays; only the three statements collapse):
```go
	if !vcd.running.Load() {
		vcd.finishInterrupted(contentOffset)
		return nil
	}
```

In `flush`, insert the guard between the empty check and the `MkdirAll`, and record the write at the end:

```go
func (vcd *VodChatDownloader) flush() {
	if len(vcd.messages) == 0 || vcd.outputPath == "" {
		return
	}
	if vcd.outputDirGone() {
		// The job finalized and removed staging while this goroutine was still
		// paging. MkdirAll here would REBUILD <staging>/<jobID>/ around a
		// chat.json and a resume sidecar nothing ever cleans (TWITCH-11).
		vcd.logger.Warn("[TwitchVodChat] output directory is gone; dropping the exit flush",
			"path", vcd.outputPath, "pending", len(vcd.messages))
		return
	}

	dir := filepath.Dir(vcd.outputPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		vcd.logger.Error("create vod chat output dir", "err", err)
		return
	}
	…
	// Clear messages from memory after successful write to prevent unbounded growth
	vcd.messages = vcd.messages[:0]
	vcd.wroteFile.Store(true)
}
```

And in `saveResumeState`, widen the existing early return:

```go
	if vcd.outputPath == "" || vcd.outputDirGone() {
		return
	}
```

(`sync`, `sync/atomic`, `context`, `os` and `path/filepath` are already imported.)

- [ ] **Step 5: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=3 -race -run 'TestVodChat' ./internal/twitch/
```
Expected: `ok` for both, no race reports. The existing `vod_chat_paging_test.go` suite (`TestVodChat*`, eight tests, including the R4 stall cases) stays green — none of them calls `Stop()` mid-fetch or removes the output directory.

- [ ] **Step 6: Worker gate**

`internal/worker` owns the 2 s grace (`orchestrator_chat.go`) and the finalize `RemoveAll` (`worker.go`); neither is edited, but this task changes when `Start` returns.

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/twitch/
gofmt -l ./internal/twitch
staticcheck ./internal/twitch/
```
Expected: `ok` for the worker suite (~16 s), clean vet/gofmt/staticcheck.

- [ ] **Step 7: Commit**

```bash
git add internal/twitch/vod_chat.go internal/twitch/vod_chat_stop_test.go
git commit -m "$(cat <<'EOF'
fix(twitch): Stop() cancels the in-flight VOD comment page; no orphan staging dir

TWITCH-11 (sweep-2 row #63). Stop() only flipped `running`, so a goroutine
parked in a GQL page fetch was bounded by the 30 s client timeout times four
attempts plus ~7 s of backoff — far past the orchestrator's 2 s grace. The job
finalized, processJob removed staging, and the goroutine's exit flush then
RECREATED <staging>/<jobID>/ with a chat.json and a resume sidecar nothing ever
cleans. Stop() now fires a session cancel (the IRC interruptSession shape) so
the fetch aborts inside the grace, and flush/saveResumeState refuse to write
once the directory they wrote to has been removed.

The three interrupted exits collapse into finishInterrupted, which always
flushes and saves: the fetch-error branch's cancel arm used to return ctx.Err()
with neither, which the new cancel would otherwise have started reaching.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)" -- internal/twitch/vod_chat.go internal/twitch/vod_chat_stop_test.go
```

---

### Task 7: Enhanced broadcasts — usher parameters, `CODECS` parsing, codec-aware source selection

Owner decision **"Twitch usher"**: *OPT IN to enhanced broadcasts: send `platform=web` + `supported_codecs=av1,h265,h264` like yt-dlp; parse `CODECS` in the playlist parser; codec-aware variant selection preferring the enhanced source; the mux container handling yt-dlp uses for AV1-in-TS. A new-feature task, not a fix.* Upstream: `references/yt-dlp/yt_dlp/extractor/twitch.py:189-203` (`_extract_twitch_m3u8_formats`). Without those two parameters Twitch never offers the HEVC/AV1 1440p/4K source and the capture takes the H.264 transcode.

**The AV1-in-TS container handling — analysed, no engine change needed.** yt-dlp's workaround is `fmt.setdefault('downloader_options', {}).update({'ffmpeg_args_out': ['-f', 'mp4']})` for any format whose `vcodec` starts with `av01`, because *its* downloader is ffmpeg and would otherwise keep the mpegts container end to end. Moombox never downloads through ffmpeg: `internal/engine` writes the raw HLS segments (and the `#EXT-X-MAP` init segment, already supported) into `<staging>/video_stream`, and the mux is `ffmpeg -y -i <video_stream> -c copy -movflags faststart <out>.mp4` (`Muxer.buildArgs`, `internal/engine/muxer.go`; the Twitch call site is `muxAndFinalize` → `MuxCopy`). The output container is **already** MP4 on every Twitch path — exactly the state yt-dlp's flag forces. So there is nothing to change and nothing to hand the controller as a cross-arc dependency. What remains is a field gate, not a code change: the first real enhanced capture proves the local ffmpeg demuxes the enhanced source. Record that as a residual in the arc ledger; do not edit `internal/engine/**`.

**Files:**
- Modify: `internal/twitch/api.go` (`BuildUsherLiveURL` ~:873-892, `BuildUsherVodURL` ~:894-913 — NOT `GetStreamInfo`)
- Modify: `internal/twitch/hls.go` (regex block ~:16-21; `ParseHLSMasterPlaylist` attribute loop ~:35-48; `SelectBestVariant`'s source step ~:166-171)
- Modify: `internal/twitch/types.go` (`TwitchHLSVariant` ~:27-37)
- Modify: `internal/twitch/hls_test.go` (append)
- Create: `internal/twitch/api_usher_test.go`
- Modify: `docs/spec/platform-services.md` (`#### URL Construction` ~:449-459, `#### Master Playlist Parsing` ~:461-474, `#### Variant Selection Algorithm` ~:476-489)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces, all in `internal/twitch`:
  - `TwitchHLSVariant.Codecs string` (`json:"codecs,omitempty"`) — the raw `CODECS` attribute
  - `TwitchHLSVariant.VideoCodec string` (`json:"videoCodec,omitempty"`) — `"av01"`, `"hevc"`, `"avc1"` or `""`
  - `func videoCodecFamily(codecs string) string` (`hls.go`)
  - `func codecRank(family string) int` (`hls.go`)
  - `func pixelArea(v *TwitchHLSVariant) int` (`hls.go`)
  - `func selectSourceVariant(variants []TwitchHLSVariant) *TwitchHLSVariant` (`hls.go`)
  - `SelectBestVariant`, `ParseHLSMasterPlaylist`, `BuildUsherLiveURL`, `BuildUsherVodURL` keep their existing signatures — `internal/worker` calls all four.

- [ ] **Step 1: Write the failing tests**

Append to `internal/twitch/hls_test.go`:

```go
// enhancedMasterPlaylist is the shape Twitch serves once the usher request
// carries platform=web and supported_codecs=av1,h265,h264: the source group
// gains an HEVC rendition above the H.264 one, and the transcodes stay H.264.
const enhancedMasterPlaylist = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=16000000,RESOLUTION=2560x1440,CODECS="hvc1.2.4.L150.90,mp4a.40.2",FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/chunked-hevc.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=8000000,RESOLUTION=1920x1080,CODECS="avc1.64002A,mp4a.40.2",FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/chunked-h264.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,CODECS="avc1.4D401F,mp4a.40.2",FRAME-RATE=30.000,VIDEO="720p30"
https://example.com/720p30.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=160000,CODECS="mp4a.40.2",VIDEO="audio_only"
https://example.com/audio_only.m3u8`

// TestParseHLSMasterPlaylistReadsCodecs pins the new attribute. The parser
// keeps the raw CODECS list AND a normalized video family, because the raw list
// is an ordered comma string whose video entry is not always first.
//
// Mutants: not parsing CODECS at all (Codecs is empty); deriving the family
// from the FIRST entry unconditionally (the audio-only variant then reports a
// video codec, and an "mp4a,hvc1" ordering would report none).
func TestParseHLSMasterPlaylistReadsCodecs(t *testing.T) {
	variants := ParseHLSMasterPlaylist(enhancedMasterPlaylist)
	if len(variants) != 4 {
		t.Fatalf("parsed %d variants, want 4", len(variants))
	}
	for i, want := range []struct{ codecs, family string }{
		{"hvc1.2.4.L150.90,mp4a.40.2", "hevc"},
		{"avc1.64002A,mp4a.40.2", "avc1"},
		{"avc1.4D401F,mp4a.40.2", "avc1"},
		{"mp4a.40.2", ""},
	} {
		if variants[i].Codecs != want.codecs {
			t.Errorf("variant[%d].Codecs = %q, want %q", i, variants[i].Codecs, want.codecs)
		}
		if variants[i].VideoCodec != want.family {
			t.Errorf("variant[%d].VideoCodec = %q, want %q", i, variants[i].VideoCodec, want.family)
		}
	}
}

// TestVideoCodecFamily covers the families Twitch can offer plus the shapes
// that must NOT be read as video.
//
// Mutant: matching "av1"/"h265" (the usher REQUEST spelling) instead of the
// RFC 6381 codec ids "av01"/"hev1"/"hvc1" that appear in a playlist.
func TestVideoCodecFamily(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"av01.0.08M.10,mp4a.40.2", "av01"},
		{"hvc1.2.4.L150.90", "hevc"},
		{"hev1.2.4.L150.90", "hevc"},
		{"avc1.64002A", "avc1"},
		{"avc3.64002A", "avc1"},
		{"mp4a.40.2", ""},
		{"", ""},
		{"  AV01.0.08M.10 , mp4a.40.2 ", "av01"},
	} {
		if got := videoCodecFamily(tc.in); got != tc.want {
			t.Errorf("videoCodecFamily(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSelectBestVariantPrefersTheEnhancedSource is the owner's "codec-aware
// variant selection preferring the enhanced source". Both source renditions
// are VIDEO="chunked"; the old rule took the FIRST one in playlist order, which
// is fine today (there is only ever one) but would be a coin flip once Twitch
// offers two.
//
// Mutants: keeping the first-IsSource loop (the 1080p H.264 source is
// returned); ranking by bandwidth instead of codec (also the H.264 source on a
// playlist where the transcode outbids the AV1 source).
func TestSelectBestVariantPrefersTheEnhancedSource(t *testing.T) {
	variants := ParseHLSMasterPlaylist(enhancedMasterPlaylist)
	got := SelectBestVariant(variants, "best", 0)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil")
	}
	if got.VideoCodec != "hevc" || got.Height != 1440 {
		t.Errorf("selected %s %dx%d (%s), want the 2560x1440 hevc source",
			got.Name, got.Width, got.Height, got.VideoCodec)
	}
}

// TestSelectBestVariantIsUnchangedWithoutCodecs is the byte-identity pin: a
// playlist with no CODECS attribute — every playlist Twitch serves without the
// new usher parameters — must select exactly what it selected before this task.
//
// Mutant: ranking an absent codec family above avc1, or letting pixel area
// reorder equal-rank sources — either one changes the answer here.
func TestSelectBestVariantIsUnchangedWithoutCodecs(t *testing.T) {
	const legacy = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1920x1080,FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/chunked.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=9000000,RESOLUTION=1280x720,FRAME-RATE=30.000,VIDEO="chunked"
https://example.com/chunked-second.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,FRAME-RATE=30.000,VIDEO="720p30"
https://example.com/720p30.m3u8`
	got := SelectBestVariant(ParseHLSMasterPlaylist(legacy), "best", 0)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil")
	}
	if got.URL != "https://example.com/chunked.m3u8" {
		t.Errorf("selected %q, want the FIRST source variant in playlist order — with no "+
			"CODECS attribute nothing may reorder the candidates", got.URL)
	}
}

// TestSelectBestVariantHonoursAnExplicitHeightOverTheEnhancedSource: an
// operator who asked for 1080p60 gets 1080p60, enhanced source or not. The
// codec preference is the SOURCE step's tie-break, not an override of the
// quality preference.
//
// Mutant: applying the codec rank inside selectVariantByHeight — the 1440p
// HEVC source is returned for a 1080p60 request.
func TestSelectBestVariantHonoursAnExplicitHeightOverTheEnhancedSource(t *testing.T) {
	got := SelectBestVariant(ParseHLSMasterPlaylist(enhancedMasterPlaylist), "1080p60", 0)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil")
	}
	if got.Height != 1080 {
		t.Errorf("selected %dx%d for quality pref 1080p60, want a 1080-high variant",
			got.Width, got.Height)
	}
}
```

Create `internal/twitch/api_usher_test.go`:

```go
package twitch

import (
	"net/url"
	"strings"
	"testing"
)

// usherParams parses a built usher URL back into its query values.
func usherParams(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse usher URL: %v", err)
	}
	return u.Query()
}

// TestUsherURLsOptInToEnhancedBroadcasts is the owner's "Twitch usher"
// decision. yt-dlp sends platform=web and supported_codecs=av1,h265,h264
// (references/yt-dlp/yt_dlp/extractor/twitch.py, _extract_twitch_m3u8_formats);
// without them Twitch never offers the HEVC/AV1 source and the capture takes
// the H.264 transcode.
//
// Every pre-existing parameter is asserted too: usher is unforgiving, and
// dropping fast_bread or type while adding these two would be a silent
// regression in latency or rerun coverage that no other test would catch.
//
// Mutants: adding platform but not supported_codecs (or the reverse); sending
// the spelling yt-dlp does not ("h265" vs "hevc", "av1" vs "av01" — the usher
// REQUEST uses the short names, unlike the playlist's RFC 6381 ids).
func TestUsherURLsOptInToEnhancedBroadcasts(t *testing.T) {
	token := &TwitchAccessToken{Value: "token-value", Signature: "sig-value"}

	for _, tc := range []struct {
		name string
		url  string
		host string
	}{
		{"live", BuildUsherLiveURL("TestChan", token), "/api/channel/hls/testchan.m3u8"},
		{"vod", BuildUsherVodURL("123456789", token), "/vod/123456789.m3u8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.url, tc.host) {
				t.Fatalf("URL path is not %s", tc.host)
			}
			q := usherParams(t, tc.url)
			if got := q.Get("platform"); got != "web" {
				t.Errorf("platform = %q, want %q", got, "web")
			}
			if got := q.Get("supported_codecs"); got != "av1,h265,h264" {
				t.Errorf("supported_codecs = %q, want %q", got, "av1,h265,h264")
			}
			for k, want := range map[string]string{
				"allow_source":               "true",
				"allow_audio_only":           "true",
				"allow_spectre":              "true",
				"player":                     "twitchweb",
				"playlist_include_framerate": "true",
				"type":                       "any",
				"sig":                        "sig-value",
				"token":                      "token-value",
			} {
				if got := q.Get(k); got != want {
					t.Errorf("%s = %q, want %q (a pre-existing parameter must survive)", k, got, want)
				}
			}
			if q.Get("p") == "" {
				t.Error("p is empty — the cache-buster must survive")
			}
		})
	}

	if q := usherParams(t, BuildUsherLiveURL("TestChan", token)); q.Get("fast_bread") != "true" {
		t.Error("fast_bread = \"\" on the LIVE URL, want \"true\" — low-latency mode is live-only " +
			"and must survive")
	}
	if q := usherParams(t, BuildUsherVodURL("123456789", token)); q.Get("fast_bread") != "" {
		t.Error("fast_bread is set on the VOD URL — it never was, and a VOD has no low-latency edge")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestParseHLSMasterPlaylistReadsCodecs|TestVideoCodecFamily|TestSelectBestVariant|TestUsherURLs' -v ./internal/twitch/
```
Expected: build failure (`videoCodecFamily` undefined, `Codecs`/`VideoCodec` not fields). Comment out the two tests that reference `videoCodecFamily` and the codec fields, re-run, and confirm `TestUsherURLsOptInToEnhancedBroadcasts` fails at `platform = "", want "web"` and `TestSelectBestVariantIsUnchangedWithoutCodecs` PASSES (it is the byte-identity guard and must stay green through Step 4). Restore.

- [ ] **Step 3: Implement the usher parameters**

`internal/twitch/api.go` — add two entries to each builder's `url.Values` (`url.Values.Encode` sorts by key, so placement in the literal is cosmetic; keep it alphabetical to match the existing block):

```go
// BuildUsherLiveURL constructs the Usher HLS master playlist URL for a live channel.
//
// platform=web and supported_codecs=av1,h265,h264 are the ENHANCED-BROADCAST
// opt-in, byte-for-byte what yt-dlp sends (references/yt-dlp
// yt_dlp/extractor/twitch.py, _extract_twitch_m3u8_formats). Without them
// Twitch never offers the HEVC/AV1 1440p/4K source of a channel that
// multi-encodes, and the capture takes the H.264 transcode. The short names
// here are the request spelling; the playlist answers in RFC 6381 codec ids
// (av01…/hev1…/hvc1…) — see videoCodecFamily in hls.go.
func BuildUsherLiveURL(channelLogin string, token *TwitchAccessToken) string {
	params := url.Values{
		"allow_source":               {"true"},
		"allow_audio_only":           {"true"},
		"allow_spectre":              {"true"},
		"fast_bread":                 {"true"},
		"p":                          {strconv.Itoa(rand.IntN(10_000_000))},
		"platform":                   {"web"},
		"player":                     {"twitchweb"},
		"playlist_include_framerate": {"true"},
		"sig":                        {token.Signature},
		"supported_codecs":           {"av1,h265,h264"},
		"token":                      {token.Value},
		"type":                       {"any"},
	}
	…
```

and the same two entries in `BuildUsherVodURL` (which has no `fast_bread`), with a one-line comment pointing at the live builder's paragraph:

```go
// BuildUsherVodURL constructs the Usher HLS master playlist URL for a VOD.
// platform / supported_codecs: see BuildUsherLiveURL — yt-dlp sends the same
// pair for both paths, and enhanced-broadcast VODs exist.
```

- [ ] **Step 4: Implement `CODECS` parsing and codec-aware source selection**

`internal/twitch/types.go`:

```go
// TwitchHLSVariant represents an HLS quality variant from the master playlist.
type TwitchHLSVariant struct {
	URL        string  `json:"url"`
	Name       string  `json:"name"`
	Bandwidth  int     `json:"bandwidth"`
	Width      int     `json:"width,omitempty"`
	Height     int     `json:"height,omitempty"`
	FPS        float64 `json:"fps,omitempty"`
	VideoGroup string  `json:"videoGroup,omitempty"`
	// Codecs is the raw CODECS attribute (RFC 6381 ids, comma-separated) and
	// VideoCodec the normalized family derived from it: "av01", "hevc",
	// "avc1", or "" when the playlist carries no CODECS or the variant has no
	// video track. Populated only once the usher request opts in to enhanced
	// broadcasts — see BuildUsherLiveURL.
	Codecs     string `json:"codecs,omitempty"`
	VideoCodec string `json:"videoCodec,omitempty"`
	IsSource   bool   `json:"isSource"`
}
```

`internal/twitch/hls.go` — the regex, the parse hook, and the four helpers:

```go
var (
	hlsBandwidthRe  = regexp.MustCompile(`BANDWIDTH=(\d+)`)
	hlsResolutionRe = regexp.MustCompile(`RESOLUTION=(\d+)x(\d+)`)
	hlsFrameRateRe  = regexp.MustCompile(`FRAME-RATE=([\d.]+)`)
	hlsVideoGroupRe = regexp.MustCompile(`VIDEO="([^"]+)"`)
	hlsCodecsRe     = regexp.MustCompile(`CODECS="([^"]*)"`)
)
```

In `ParseHLSMasterPlaylist`, directly after the `hlsVideoGroupRe` block:

```go
		if m := hlsCodecsRe.FindStringSubmatch(line); m != nil {
			variant.Codecs = m[1]
			variant.VideoCodec = videoCodecFamily(m[1])
		}
```

Above `SelectBestVariant`:

```go
// videoCodecFamily normalizes an HLS CODECS attribute to the video family it
// carries: "av01", "hevc", "avc1", or "" when the list holds no recognised
// video codec (an audio-only rendition, or a playlist that sends no CODECS at
// all — every playlist Twitch serves without the enhanced-broadcast opt-in).
//
// The list is scanned in order rather than read at index 0: the video entry is
// not always first, and an audio-only rendition's single mp4a entry must not be
// mistaken for one. Matching is on the RFC 6381 ids a PLAYLIST uses
// (av01…/hev1…/hvc1…/avc1…/avc3…), not the short names the usher REQUEST sends.
func videoCodecFamily(codecs string) string {
	for entry := range strings.SplitSeq(codecs, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		switch {
		case strings.HasPrefix(entry, "av01"):
			return "av01"
		case strings.HasPrefix(entry, "hev1"), strings.HasPrefix(entry, "hvc1"):
			return "hevc"
		case strings.HasPrefix(entry, "avc1"), strings.HasPrefix(entry, "avc3"):
			return "avc1"
		}
	}
	return ""
}

// codecRank orders the video families an enhanced broadcast can offer.
//
// An ABSENT family ranks 0, below avc1's 1, and that is what makes the whole
// feature byte-compatible: on a playlist with no CODECS attribute every
// candidate ranks 0, every comparison in selectSourceVariant ties, and the
// incumbent — the first source in playlist order — wins exactly as before.
func codecRank(family string) int {
	switch family {
	case "av01":
		return 3
	case "hevc":
		return 2
	case "avc1":
		return 1
	default:
		return 0
	}
}

// pixelArea is the variant's frame area, 0 when the playlist gave no RESOLUTION.
func pixelArea(v *TwitchHLSVariant) int { return v.Width * v.Height }

// selectSourceVariant returns the best SOURCE variant, or nil when the playlist
// carries none.
//
// The incumbent is the FIRST source in playlist order — the rule that shipped
// before enhanced broadcasts, and the only rule that ever applied, because a
// pre-enhanced Twitch playlist holds exactly one VIDEO="chunked" rendition. It
// is displaced only by a strictly BETTER video family, or by a larger frame at
// the same family once that family is already better than H.264. Nothing about
// bandwidth or resolution alone can reorder a playlist that carries no CODECS.
func selectSourceVariant(variants []TwitchHLSVariant) *TwitchHLSVariant {
	best := -1
	for i := range variants {
		if !variants[i].IsSource {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		cur, cand := codecRank(variants[best].VideoCodec), codecRank(variants[i].VideoCodec)
		switch {
		case cand > cur:
			best = i
		case cand == cur && cand > codecRank("avc1") && pixelArea(&variants[i]) > pixelArea(&variants[best]):
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	return &variants[best]
}
```

And the source step in `SelectBestVariant`:

```go
	// Prefer source quality (codec-aware — see selectSourceVariant).
	if src := selectSourceVariant(filtered); src != nil {
		return src
	}
```

`selectVariantByHeight`, `selectNextLowerVariant`, the audio-only arm, the resolution cap and the bandwidth fallback are deliberately unchanged: an operator who asked for `1080p60` gets 1080p60, and `max_resolution` still filters an enhanced 1440p source out for a 1080-capped job.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/ ./internal/worker/
```
Expected: both `ok`. Every pre-existing `hls_test.go` case (`TestParseHLSMasterPlaylistBasic`, `TestSelectBestVariantSource`, `TestSelectBestVariantMaxResolution`, `TestSelectBestVariantFallbackToLower`, …) stays green unchanged — verified during planning against these exact helpers.

- [ ] **Step 6: Document the feature**

`docs/spec/platform-services.md`, `#### URL Construction` — add two bullets to the shared parameter list, in the alphabetical position they encode to, and a paragraph under it:

```markdown
- `platform=web` -- Enhanced-broadcast opt-in (see below).
- `supported_codecs=av1,h265,h264` -- Enhanced-broadcast opt-in (see below).
```
```markdown
`platform` and `supported_codecs` are byte-for-byte what yt-dlp sends (`references/yt-dlp/yt_dlp/extractor/twitch.py`, `_extract_twitch_m3u8_formats`), on the live and the VOD URL alike. They are the opt-in to Twitch **enhanced broadcasts**: a channel that multi-encodes then offers an HEVC or AV1 source alongside the H.264 one, typically at a higher resolution, and without them Usher never lists it and the capture takes the H.264 transcode. The short names here are the REQUEST spelling; the playlist answers in RFC 6381 codec ids (`av01…`, `hev1…`/`hvc1…`, `avc1…`). No container work follows from an AV1 source: yt-dlp forces `-f mp4` on its own ffmpeg downloader because that downloader would otherwise keep the mpegts container end to end, whereas Moombox writes raw segments (`internal/engine`) and always muxes to MP4 with `-c copy` (`Muxer.buildArgs`, `internal/engine/muxer.go`) — the state that flag exists to force.
```

`#### Master Playlist Parsing` — add a table row and a sentence:

```markdown
| `CODECS` | `CODECS="([^"]*)"` | `Codecs`, and `VideoCodec` via `videoCodecFamily` |
```
```markdown
`videoCodecFamily` (`internal/twitch/hls.go`) normalizes the raw list to `"av01"`, `"hevc"`, `"avc1"` or `""`. It scans the list in order rather than reading its first entry: the video codec is not always first, and an audio-only rendition's single `mp4a` entry must not be read as one. A playlist served without the enhanced-broadcast opt-in carries no `CODECS` attribute at all, so both fields stay empty.
```

`#### Variant Selection Algorithm` — replace step 5:

```markdown
5. **Source preference** (codec-aware): if no quality preference matched, take the best `IsSource` variant via `selectSourceVariant` (`internal/twitch/hls.go`). The incumbent is the FIRST source in playlist order — the rule that applied before enhanced broadcasts, when a Twitch playlist held exactly one `VIDEO="chunked"` rendition. It is displaced only by a strictly better video family (`codecRank`: AV1 > HEVC > H.264 > absent) or, once that family already beats H.264, by a larger frame at the same family. An ABSENT family ranks below H.264, so on a playlist with no `CODECS` attribute every candidate ties and the first source wins exactly as before — the selection is byte-identical for every playlist Twitch served before the opt-in.
```

and append one sentence after the numbered list:

```markdown
The codec preference is the SOURCE step's tie-break, not an override: an operator who asked for `1080p60` still gets a 1080-high variant, and `max_resolution` still filters an enhanced 1440p source out of a 1080-capped job.
```

- [ ] **Step 7: Gates**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/ ./internal/worker/ ./internal/docs/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/twitch/
gofmt -l ./internal/twitch
staticcheck ./internal/twitch/
```

- [ ] **Step 8: Commit**

```bash
git add internal/twitch/api.go internal/twitch/hls.go internal/twitch/types.go internal/twitch/hls_test.go internal/twitch/api_usher_test.go docs/spec/platform-services.md
git commit -m "$(cat <<'EOF'
feat(twitch): opt in to enhanced broadcasts (usher codecs, CODECS parsing, codec-aware source)

Owner decision "Twitch usher". Both usher builders now send platform=web and
supported_codecs=av1,h265,h264, byte-for-byte what yt-dlp sends
(_extract_twitch_m3u8_formats); without them Usher never lists the HEVC/AV1
source of a channel that multi-encodes and the capture takes the H.264
transcode. The master-playlist parser reads CODECS into a raw field plus a
normalized family (videoCodecFamily), and the source step prefers a strictly
better family — or a larger frame at the same better-than-H.264 family — over
the first source in playlist order.

Byte-identical on every playlist Twitch served before the opt-in: an ABSENT
codec family ranks below H.264, so with no CODECS attribute every candidate
ties and the incumbent wins. The quality preference and the resolution cap are
untouched.

No container work follows: yt-dlp's `-f mp4` workaround for AV1-in-TS exists
because its downloader IS ffmpeg and would keep mpegts end to end; Moombox
writes raw segments and already muxes to MP4 with -c copy.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)" -- internal/twitch/api.go internal/twitch/hls.go internal/twitch/types.go internal/twitch/hls_test.go internal/twitch/api_usher_test.go docs/spec/platform-services.md
```

---

### Task 8: The live Twitch playlist gate (`MOOMBOX_LIVE_TWITCH_TEST=1`)

A fixture cannot see a dead or degraded extraction path: a broken usher request answers with a well-formed error body, and a parameter Twitch stopped accepting answers with a perfectly parseable playlist that is simply missing the enhanced renditions. This gate asserts **capabilities against real Usher**, never mechanisms: that the two new parameters reach the wire, that the master playlist still parses into usable variants, and that when a variant carries `CODECS` the parser derives a family from it. It never asserts that an enhanced rendition EXISTS — most channels do not multi-encode — and it never prints the URL, the token or the signature.

**Files:**
- Create: `internal/twitch/hls_live_test.go`

**Interfaces:**
- Consumes: `videoCodecFamily`, `TwitchHLSVariant.Codecs`/`.VideoCodec`, `BuildUsherLiveURL`, `FetchHLSMasterPlaylist`, `SelectBestVariant` (Task 7).
- Produces: no production symbols.

- [ ] **Step 1: Write the test**

This task has no red/green cycle in the ordinary sense — the gate is skipped by default, which IS its default state. The verification in Step 2 is that it skips cleanly with no env, and in Step 3 that it fails loudly when the assertions are mutated.

Create `internal/twitch/hls_live_test.go`:

```go
package twitch

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveTwitchMasterPlaylistParses is the arc's live gate for the
// enhanced-broadcast opt-in.
//
// WHY A LIVE GATE. A fixture cannot see this path die. If Usher ever refuses
// platform= or supported_codecs=, or renames a codec, the answer is not an
// error — it is a perfectly well-formed master playlist that simply no longer
// lists the enhanced renditions, and every fixture in hls_test.go keeps
// passing. The failure mode is silent quality loss on exactly the channels the
// feature was added for.
//
// WHAT IT ASSERTS — capabilities, never mechanisms:
//   - the two opt-in parameters reach the built URL;
//   - Usher answers a parseable master playlist with at least one usable variant;
//   - at least one variant carries a resolution (so the selector has something to rank);
//   - every variant that carries CODECS yields a recognised video family, or is
//     the audio-only rendition;
//   - SelectBestVariant picks something.
//
// It does NOT assert that an enhanced rendition exists: most channels do not
// multi-encode, and a gate that demanded one would fail for a reason that says
// nothing about Moombox.
//
// WHAT IT NEVER PRINTS. Not the URL, not the token, not the signature. The
// usher URL is a signed entitlement; the whole question here is answered by
// counts, families and booleans. The codec families ARE logged, because they
// are the measurement the operator running this gate wants.
//
// Enable with:
//
//	MOOMBOX_LIVE_TWITCH_TEST=1
//	MOOMBOX_LIVE_TWITCH_CHANNEL=<login of a channel that is LIVE right now>
//
// The channel is a required input rather than a hardcoded default, matching
// TestLivePlaybackTokenShape in playback_token_live_test.go: a default would
// rot, and an offline channel yields no stream playback token at all. Always
// run with -count=1 — a cached PASS on a live probe is not a measurement.
func TestLiveTwitchMasterPlaylistParses(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_TWITCH_TEST") != "1" {
		t.Skip("set MOOMBOX_LIVE_TWITCH_TEST=1 to run the live Twitch master-playlist gate")
	}
	channel := os.Getenv("MOOMBOX_LIVE_TWITCH_CHANNEL")
	if channel == "" {
		t.Skip("set MOOMBOX_LIVE_TWITCH_CHANNEL=<login of a channel that is live right now> " +
			"to run the live Twitch master-playlist gate")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	api := NewAPI(&testLogger{})

	info, err := api.GetStreamInfo(ctx, channel, "")
	if err != nil {
		t.Fatalf("GetStreamInfo(%s) failed: %v", channel, err)
	}
	if info == nil || !info.IsLive {
		t.Skipf("%s is not live right now; nothing to measure", channel)
	}

	token, err := api.GetStreamAccessToken(ctx, channel, "")
	if err != nil {
		t.Fatalf("GetStreamAccessToken(%s) failed: %v", channel, err)
	}

	usherURL := BuildUsherLiveURL(channel, token)
	// Assert on the URL WITHOUT printing it: it carries the signed token.
	if !strings.Contains(usherURL, "platform=web") {
		t.Error("the built usher URL carries no platform=web — the enhanced-broadcast opt-in " +
			"never reaches Usher (URL not printed: it carries a signed entitlement)")
	}
	if !strings.Contains(usherURL, "supported_codecs=av1") {
		t.Error("the built usher URL carries no supported_codecs — the enhanced-broadcast " +
			"opt-in never reaches Usher")
	}

	variants, err := FetchHLSMasterPlaylist(ctx, usherURL)
	if err != nil {
		t.Fatalf("FetchHLSMasterPlaylist failed: %v", err)
	}
	if len(variants) == 0 {
		t.Fatal("Usher answered a master playlist with no variants — the capability this gate " +
			"exists for (a usable playlist) is gone")
	}

	sawResolution := false
	sawCodecs := false
	families := map[string]int{}
	for _, v := range variants {
		if v.URL == "" {
			t.Errorf("variant %q carries no URL", v.Name)
		}
		if v.Height > 0 && v.Width > 0 {
			sawResolution = true
		}
		if v.Codecs == "" {
			continue
		}
		sawCodecs = true
		families[v.VideoCodec]++
		audioOnly := strings.Contains(strings.ToLower(v.Name), "audio_only")
		switch v.VideoCodec {
		case "av01", "hevc", "avc1":
		case "":
			if !audioOnly {
				t.Errorf("variant %q carries CODECS but no recognised video family — "+
					"videoCodecFamily no longer understands what Twitch sends", v.Name)
			}
		default:
			t.Errorf("variant %q reported video family %q, which is not one this parser emits",
				v.Name, v.VideoCodec)
		}
	}
	if !sawResolution {
		t.Error("no variant carried a RESOLUTION — the selector has nothing to rank")
	}
	if !sawCodecs {
		t.Log("NOTE: no variant carried a CODECS attribute. That is not a failure (Usher may " +
			"omit it), but the codec-aware selection is dormant for this channel.")
	}
	t.Logf("variants=%d codec families=%v enhanced=%v",
		len(variants), families, families["av01"]+families["hevc"] > 0)

	if best := SelectBestVariant(variants, "best", 0); best == nil {
		t.Error("SelectBestVariant returned nil for a real live playlist")
	} else {
		t.Logf("selected: %s %dx%d fps=%.0f codec=%q source=%v",
			best.Name, best.Width, best.Height, best.FPS, best.VideoCodec, best.IsSource)
	}
}
```

- [ ] **Step 2: Verify it skips cleanly by default**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestLiveTwitchMasterPlaylistParses -v ./internal/twitch/
```
Expected: `--- SKIP: TestLiveTwitchMasterPlaylistParses` with `set MOOMBOX_LIVE_TWITCH_TEST=1 to run the live Twitch master-playlist gate`. No network traffic.

- [ ] **Step 3: Verify it measures something (needs a live channel)**

```bash
MOOMBOX_LIVE_TWITCH_TEST=1 MOOMBOX_LIVE_TWITCH_CHANNEL=<a channel that is live right now> \
  GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestLiveTwitchMasterPlaylistParses -v ./internal/twitch/
```
Expected: PASS with a `variants=N codec families=map[...]` line and a `selected: …` line. If the named channel has gone offline between the two calls, expect `SKIP … is not live right now`. Confirm the gate is not vacuous by temporarily changing `platform=web` to `platform=zzz` in the `strings.Contains` assertion and re-running: it must FAIL at "the built usher URL carries no platform=web". Revert.

If no channel is live at execution time, record that in the arc ledger as an unrun gate and run it at the merge candidate instead — do not weaken the test to make it runnable.

- [ ] **Step 4: Gates**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/twitch/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/twitch/
gofmt -l ./internal/twitch
staticcheck ./internal/twitch/
```
Expected: `ok` (the live test skipped), clean everything.

- [ ] **Step 5: Commit**

```bash
git add internal/twitch/hls_live_test.go
git commit -m "$(cat <<'EOF'
test(twitch): MOOMBOX_LIVE_TWITCH_TEST gate for the enhanced-broadcast playlist

A fixture cannot see this path die: if Usher stops accepting platform= or
supported_codecs=, the answer is a perfectly parseable master playlist that
simply no longer lists the enhanced renditions, and every hls_test.go fixture
keeps passing. The gate asserts capabilities against real Usher — the opt-in
parameters reach the URL, the playlist parses into usable variants, at least one
carries a resolution, every variant with CODECS yields a recognised family, and
SelectBestVariant picks something — never that an enhanced rendition exists.
Skips cleanly without MOOMBOX_LIVE_TWITCH_TEST=1, without a named channel, and
when that channel is not live. Never prints the usher URL, the token or the
signature.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)" -- internal/twitch/hls_live_test.go
```

---

### Task 9: TWITCH-10 residual doc drift, and delete this plan

Report row #102's TWITCH-10 names three drifted lines. Two of them are still open: `docs/spec/platform-services.md:1203`'s caching table says the Twitch emote cache is "Unbounded (no expiry)" although the same file's `#### Caching` section and `emoteCacheTTL` both say 24 hours, and `:1226` still says `internal/twitch/` is "9 files, ~3,200 lines" when it is 14 files and was 5,977 lines before this arc. (The third, the CAP sentence at `:520`, shipped with Task 3.) Then the plan deletes itself — git history is the archive.

**Files:**
- Modify: `docs/spec/platform-services.md:1203`, `:1226`
- Delete: `docs/superpowers/plans/2026-09-17-sweep2-t-twitch.md`

**Interfaces:**
- Consumes: every earlier task's source edits (the line count is measured after them).
- Produces: nothing.

- [ ] **Step 1: Measure the current counts**

```bash
ls internal/twitch/*.go | grep -v '_test\.go' | wc -l
cat $(ls internal/twitch/*.go | grep -v '_test\.go') | wc -l
```
Expected: `14` files (this arc adds no source file) and a line count somewhat above the pre-arc `5977`. Use the two numbers this prints — rounded to the nearest 25 for the line figure, matching the `~` convention the surrounding rows use.

- [ ] **Step 2: Fix the caching table row**

`docs/spec/platform-services.md:1203` — replace:

```markdown
| Twitch Emotes | Memory LRU | Unbounded (no expiry) | 200 channels | Oldest by insertion order | lowercased channelLogin |
```
with:
```markdown
| Twitch Emotes | Memory LRU + TTL | 24 h (`emoteCacheTTL`, `internal/twitch/emotes.go`) | 200 channels | Oldest by insertion order | lowercased channelLogin |
```

(The `#### Caching` section above already describes the stale-on-refetch-failure rule; only the table row was stale.)

- [ ] **Step 3: Fix the package line**

`docs/spec/platform-services.md:1226` — replace:

```markdown
- `internal/twitch/` -- Service, API, Auth, HLS, Chat, VodChat, Emotes, Types (9 files, ~3,200 lines).
```
with the measured shape (substituting the numbers from Step 1; `14` and a line figure just above 6,000 at the time of writing):

```markdown
- `internal/twitch/` -- Service, API, Auth, HLS, Chat (IRC + recording + file), VodChat, Emotes, PlaybackToken, LivenessProbe, Delays, Types (14 files, ~6,050 lines).
```

- [ ] **Step 4: Gate the docs**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/
```
Expected: `ok` — `emoteCacheTTL` is declared in the cited `internal/twitch/emotes.go`, which is what the citation test checks.

- [ ] **Step 5: Delete the plan**

```bash
git rm docs/superpowers/plans/2026-09-17-sweep2-t-twitch.md
```

- [ ] **Step 6: Commit**

```bash
git add docs/spec/platform-services.md
git commit -m "$(cat <<'EOF'
docs(twitch): TWITCH-10 drift — emote cache expiry, package file count; delete the arc plan

Sweep-2 row #102 (TWITCH-10). The caching table still called the Twitch emote
cache unbounded although the same file's Caching section and emoteCacheTTL both
say 24 hours, and the package line still said 9 files / ~3,200 lines for what is
14 files and just over 6,000. (The third drifted line, the join/part CAP
sentence, shipped with the CAP change itself.)

The arc plan deletes itself: git history is the archive.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq
EOF
)" -- docs/spec/platform-services.md docs/superpowers/plans/2026-09-17-sweep2-t-twitch.md
```

---

## Merge-candidate gates (controller, after Task 9)

Merge `main` into `sweep2-t-twitch` FIRST — Arc E merges before this arc and owns the `GetStreamInfo` hunk in the same file this arc edits.

```bash
gofmt -l ./cmd ./internal ./tools ./web
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
staticcheck ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build -o /dev/null ./cmd/moombox
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build -o /dev/null ./cmd/moombox
GOOS=linux GOARCH=arm64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build -o /dev/null ./cmd/moombox
cd web/tests && node --test *.test.mjs
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./...
MOOMBOX_LIVE_TWITCH_TEST=1 MOOMBOX_LIVE_TWITCH_CHANNEL=<live channel> \
  GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestLiveTwitchMasterPlaylistParses -v ./internal/twitch/
```

Residual to carry into the arc ledger: **the first real enhanced-broadcast capture is a field gate.** The selection and the mux are analysed and require no engine change (Task 7), but no capture of an HEVC/AV1 Twitch source has been observed end to end through `ffmpeg -c copy`.
