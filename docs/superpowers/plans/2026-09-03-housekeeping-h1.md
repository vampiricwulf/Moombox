# Housekeeping H1 Implementation Plan

> **RULED (a), controller 2026-09-03 — Step 3a IS executed.** Why: R1 exists so the signal closes AT the exit; a poll completing inside the window would re-latch it after the close and nothing on the exit path closes it again, which half-defeats R1. Cost if wrong: four lines behind a test. The original question: R1's fix closes the signal at `MarkStreamEnded()`, but `noteLivePollResult(true)` (`internal/chat/downloader.go:450-457`, called at `:535` after every successful live fetch) checks none of the exit flags — so a poll that completed just before `MarkStreamEnded()` (or `Stop()`, which has had this window all along) re-opens the signal AFTER the exit closed it, and the loop then leaves on `shouldStop()` with it latched true; nothing on the loop's exit path closes it again. The window is one `processBatch` + `maybeFlush` (disk I/O). **(a)** Fold the guard into Task 1 — Step 3a is written out below with its test and mutant: four lines of production code, no change to a live run (`Start()` sets `running` before the first poll), verified green against the whole `internal/chat` package at `383ed7d`. **(b)** Leave it: the worklist calls the whole item "mitigated by the joint-idle gate", and that mitigation covers this window too. Reviewer's recommendation: (a). Task 1 executes WITHOUT Step 3a unless ruled (a).

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the six housekeeping items that sit OUTSIDE the cookie subsystem — one chat-signal fix, one extracted-and-tested orchestrator write site, one Twitch GQL log-hygiene fix, one stale config comment, two `.gitattributes` lines, and a citation-rot test that guards the six spec docs — on `cookie-housekeeping-h1`, in a worktree, in parallel with Arc 12c.

**Architecture:** Five independent tasks against five disjoint file sets. R1 and R2 are single-package Go changes with mutation-checked tests. R3 removes response bodies from every `gqlRequest` error and from the retry log line, proven by a logger that RENDERS args (the package's existing recorders drop them, which would make a leak invisible). R4+R5 are same-shape small edits (one comment, two gitattributes lines). R6 adds a `go/parser`-backed test under `internal/docs` that resolves every path, directory, symbol and `§`-heading citation in the six spec docs and re-verifies five absence/state claims by walking non-test ASTs — then fixes the five rots its first run finds.

**Tech Stack:** Go 1.26 (stdlib only: `go/ast`, `go/parser`, `go/token`, `sync/atomic`, `net/http`, `testing`), git attributes, Markdown.

**Spec:** `docs/superpowers/specs/2026-09-03-housekeeping-h1-design.md`

## Global Constraints

- **You are already in the worktree**, on branch `cookie-housekeeping-h1` cut from `main`. Do not create branches or worktrees, do not merge, do not push.
- **`const livenessRecoveryArmed = false` stays false.** No task here touches it or anything that reads it.
- **`cmd/moombox/main.go:276-278` is a NO-TOUCH range** (the `SetExpectedPlatforms` seeding). R4 documents the rule that block depends on; it does not edit that block.
- **No cookie value, token, credential or upstream response body may reach a log line, an error string, a test assertion, or test OUTPUT.** When a test fails on a hygiene assertion it prints a fixed sentence, never the offending text.
- **Every goroutine gets an inline `defer func() { if r := recover(); r != nil { … } }()`.** No task in this plan starts a goroutine; if you find yourself writing one, stop and re-read the task.
- **The anonymous logger interface** is repeated inline in every struct that needs it (`Debug/Info/Warn/Error(msg string, args ...any)`). Never extract it to a named interface.
- **Byte-wise LF.** Every file you write or edit stays LF-only. Verify with `perl -0777 -ne 'print tr/\r//' <file>` — it must print `0`. (`.gitattributes` itself is CRLF in the Windows working tree and LF in the index; Task 4 says exactly how to check that one.)
- **Every `go build` / `go test` is prefixed `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`** — Defender quarantines test binaries written under `%TEMP%`. Create the directory once with `mkdir -p D:/Git/Moombox/.superpowers/gotmp`.
- **Run SINGLE packages only** (`go test ./internal/chat/ …`). The controller runs the full suite at the merge gate; a full `./...` from the worktree is not your job and wastes minutes.
- **Every commit ends with:**
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
  ```
- **No cookie-subsystem file** (`internal/cookies/**`) is edited by any task here. Those items are H2.

## File Structure

- **Task 1** — `internal/chat/downloader.go` (one assignment + two doc comments), `internal/chat/downloader_livestate_test.go` (fifth exit test + header enumeration).
- **Task 2** — `internal/worker/interruption.go` (new pure `noteSegmentProgress`), `orchestrator_youtube.go` (closure body → the call), `interruption_test.go` (`TestNoteSegmentProgress`).
- **Task 3** — `internal/twitch/api.go` (`gqlBodySize`, five error sites, `prev_status`, `gqlBaseRetryDelay` const → var), new `internal/twitch/api_gql_log_hygiene_test.go`, `docs/spec/security.md` (one bullet).
- **Task 4** — `internal/config/types.go` (comment only), `.gitattributes` (two lines).
- **Task 5** — new `internal/docs/{doc.go,citations_test.go,citation_allowlist.txt}`; the five rots in `docs/spec/{architecture,operations,platform-services,user-interfaces}.md`; `internal/youtube/watch_page.go` (comment only).

---

### Task 1: `MarkStreamEnded` closes the chat-open resume signal (R1)

**Files:**
- Modify: `internal/chat/downloader.go:325-336` (`MarkStreamEnded`) and `:386-402` (`LiveContinuationOpen` and its doc comment); Step 3a, if ruled in, also `:448-457` (`noteLivePollResult`)
- Test: `internal/chat/downloader_livestate_test.go` (header block at `:43-53`, new test after `TestHandleEndOfStreamClosesSignalOnDefiniteUnrecoveredEnd`)

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: no new exported surface. `ChatDownloader.MarkStreamEnded()` keeps its signature; after this task it also leaves `LiveContinuationOpen() == false`.

**Why this shape.** `setLiveContinuationOpen` takes `cd.mu` itself, and `MarkStreamEnded` already holds it — calling the setter here would deadlock on a non-reentrant `sync.Mutex`. `Stop()` has the same constraint and assigns the field directly under the held lock; do exactly that. The assignment goes BEFORE `cancelCtx()` so a loop woken by the cancel already observes the closed signal.

- [ ] **Step 1: Write the failing test**

Append to `internal/chat/downloader_livestate_test.go`, immediately after `TestHandleEndOfStreamClosesSignalOnDefiniteUnrecoveredEnd`:

```go
// TestMarkStreamEndedClosesLiveContinuationOpen is the fifth permanent-exit
// test. MarkStreamEnded is the orchestrator's own end verdict
// (orchestrator.go's `if !isVod` branch), and until it closed the signal the
// worker's joint-idle gate (buildMayResume, internal/worker/interruption.go)
// kept counting a downloader the orchestrator had already retired as live
// resume evidence -- for as long as the chat loop took to notice, which on a
// sleeping poll is the whole poll interval.
func TestMarkStreamEndedClosesLiveContinuationOpen(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json", IsLiveOrUpcoming: true})
	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()
	cd.setLiveContinuationOpen(true) // a healthy in-progress live poll

	cd.MarkStreamEnded()

	if cd.LiveContinuationOpen() {
		t.Error("MarkStreamEnded() must close the resume signal -- the orchestrator has declared the stream over, and this downloader will not poll again")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
mkdir -p D:/Git/Moombox/.superpowers/gotmp
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/chat/ -run TestMarkStreamEndedClosesLiveContinuationOpen -count=1 -v
```
Expected: FAIL — "MarkStreamEnded() must close the resume signal…". If it PASSES, stop: you are not on `main`'s `downloader.go`.

- [ ] **Step 3: Make it pass**

In `internal/chat/downloader.go`, replace the whole `MarkStreamEnded` block:

```go
// MarkStreamEnded signals that the stream has ended naturally.
// This exits the polling loop promptly, writes the final chat file,
// and clears resume state (successful completion).
// Distinct from Stop() which is for cancellation/shutdown.
//
// A PERMANENT exit, so it closes liveContinuationOpen here rather than
// leaving it to the loop: this downloader will not poll again, and the
// worker's joint-idle gate (buildMayResume, internal/worker/interruption.go)
// must stop counting it as resume evidence the moment the orchestrator
// marks the stream ended -- not whenever the loop happens to wake up. The
// field is assigned directly under the lock this function already holds,
// exactly as Stop() does: setLiveContinuationOpen takes mu itself and would
// deadlock here.
func (cd *ChatDownloader) MarkStreamEnded() {
	cd.mu.Lock()
	cd.streamEnded = true
	cd.liveContinuationOpen = false
	if cd.cancelCtx != nil {
		cd.cancelCtx() // Wake up any sleeping poll
	}
	cd.mu.Unlock()
}
```

- [ ] **Step 3a (RULED (a) — execute this step): a late poll result cannot re-open a retired downloader**

In `internal/chat/downloader.go`, replace `noteLivePollResult` (`:448-457`):

```go
// noteLivePollResult is the runChatLoop hook: a successful LIVE poll with a
// continuation opens the signal; replay polls never do. A poll whose result
// lands AFTER a permanent exit (Stop, MarkStreamEnded, or the loop's own
// cancel flag) does not re-open it: the exit closed the signal on purpose,
// the loop is about to leave on shouldStop(), and nothing on its way out
// would close the signal again.
func (cd *ChatDownloader) noteLivePollResult(hasContinuation bool) {
	if cd.opts.IsReplay || !hasContinuation {
		return
	}
	cd.mu.Lock()
	if cd.running && !cd.cancelFlag && !cd.streamEnded {
		cd.liveContinuationOpen = true
	}
	cd.mu.Unlock()
}
```

Append to `internal/chat/downloader_livestate_test.go`, after `TestMarkStreamEndedClosesLiveContinuationOpen`:

```go
// TestLatePollResultDoesNotReopenAfterMarkStreamEnded closes the window the
// fifth exit test cannot see: a fetch that completed just before
// MarkStreamEnded() reaches noteLivePollResult after it, and used to latch
// the signal open on a downloader the loop is about to abandon.
func TestLatePollResultDoesNotReopenAfterMarkStreamEnded(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json", IsLiveOrUpcoming: true})
	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()
	cd.setLiveContinuationOpen(true)

	cd.MarkStreamEnded()
	cd.noteLivePollResult(true) // the in-flight poll's result arrives late

	if cd.LiveContinuationOpen() {
		t.Error("a poll result that lands after MarkStreamEnded() must not re-open the resume signal")
	}
}
```

Mutation (do it in Step 5 alongside the other): drop the `!cd.streamEnded` conjunct → this test fails; restore. `TestLiveContinuationOpenReplayNeverOpens` (`:35-41`, the only other direct caller of `noteLivePollResult`) asserts closed and still passes; the integration tests at `:324` and `:376` drive the loop through `Start()`, which sets `running = true` before the first poll, so a normal run still opens — the whole package was green with this step applied at `383ed7d`. Under (a), Step 6's two enumerations and Step 8's commit message gain one clause each: "…and a poll result that lands after any of them does not re-open it."

- [ ] **Step 4: Run the whole package**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/chat/ -count=1
```
Expected: PASS, including `TestMarkStreamEndedSetsFlag` (unchanged) and the four earlier exit tests.

- [ ] **Step 5: Mutation check**

Delete the line `cd.liveContinuationOpen = false` you just added, re-run Step 2's command, confirm FAIL, then put it back and re-run to confirm PASS. Record both outcomes in your report.

- [ ] **Step 6: Update the two enumerations**

In `internal/chat/downloader_livestate_test.go`, the header block at `:43-53`, replace:

```go
// signal: ErrAuthRequired, the consecutive-error budget exhausting (both
// inside handleFetchError), and Stop(). This is NOT an "ended" inference —
```
with:
```go
// signal: ErrAuthRequired, the consecutive-error budget exhausting (both
// inside handleFetchError), Stop(), and MarkStreamEnded(). For the first
// three this is NOT an "ended" inference —
```

In `internal/chat/downloader.go`, `LiveContinuationOpen`'s doc comment, replace:

```go
// A TRANSIENT fetch error does not change it — only a definitive
// end-of-stream (handleEndOfStream) or a PERMANENT loop exit closes it:
// ErrAuthRequired, the consecutive-error budget exhausting (both I5 fix,
// handleFetchError), or Stop() — a downloader that has stopped
// polling for good carries no information any more, and closed is what
// "no information" means here, by design, not an inference that the
// broadcast ended.
```
with:
```go
// A TRANSIENT fetch error does not change it — only a definitive
// end-of-stream (handleEndOfStream), the orchestrator's own end verdict
// (MarkStreamEnded), or a PERMANENT loop exit closes it: ErrAuthRequired,
// the consecutive-error budget exhausting (both I5 fix, handleFetchError),
// or Stop() — a downloader that has stopped polling for good carries no
// information any more, and closed is what "no information" means here, by
// design, not an inference that the broadcast ended.
```

- [ ] **Step 7: Re-run and check line endings**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/chat/ -count=1
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/chat/
perl -0777 -ne 'print tr/\r//' internal/chat/downloader.go internal/chat/downloader_livestate_test.go
```
Expected: PASS, no vet output, and `00` (one `0` per file).

- [ ] **Step 8: Commit**

```bash
git add internal/chat/downloader.go internal/chat/downloader_livestate_test.go
git commit -m "$(cat <<'EOF'
fix(chat): MarkStreamEnded closes the chat-open resume signal

The orchestrator's own end verdict is a permanent exit like Stop(), but it
left liveContinuationOpen latched true until the polling loop noticed the
cancellation -- a whole poll interval of wrong Tier-1 resume evidence for
the worker's joint-idle gate. Assigned under the held lock (the setter takes
mu itself), before cancelCtx(), as Stop() does. Fifth exit test; both
enumerations updated.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 2: The shared segment clock's write site becomes testable (R2)

**Files:**
- Modify: `internal/worker/interruption.go` (add `noteSegmentProgress` directly below `segmentProgressResetsStallCounters`, which ends at `:62`)
- Modify: `internal/worker/orchestrator_youtube.go:166-171` (the `onSegmentProgress` closure body)
- Test: `internal/worker/interruption_test.go` (append `TestNoteSegmentProgress` after `TestSegmentProgressResetsStallCounters`)

**Interfaces:**
- Consumes: `segmentProgressResetsStallCounters(p engine.DownloadProgress, lastBytes *atomic.Int64) bool` and `atomicTimeValue` (both already in `interruption.go`).
- Produces: `func noteSegmentProgress(p engine.DownloadProgress, lastBytes *atomic.Int64, lastSegTime *atomicTimeValue, consecutiveLiveChecks *atomic.Int32) bool` — reports whether the report counted as progress; production discards the value.

**Why this shape.** `consecutiveLiveChecks` is declared `var consecutiveLiveChecks atomic.Int32` inside `runLiveStreamDownload`, and the clock is `*atomicTimeValue` — so those are the two pointer parameters. `atomicTimeValue` has `Store(time.Time)` and `StoreNow()` but takes **no injected clock**, and adding one would put an unconditional `time.Now()` in a ~60 Hz progress callback for no behavioural gain. So the extraction keeps `StoreNow()` byte-identical and the test brackets the call: `before := time.Now()` … `after := time.Now()`, asserting the stored time lands inside. That is an exact assertion, not a loose one.

- [ ] **Step 1: Write the failing test**

Append to `internal/worker/interruption_test.go`, after `TestSegmentProgressResetsStallCounters`:

```go
// TestNoteSegmentProgress covers the live loop's ONLY write to the shared
// last-new-segment clock. Before the extraction it lived inside
// runLiveStreamDownload's onSegmentProgress closure, which nothing in this
// package calls -- so deleting either half of the reset left every test
// green while the verify branch's 10-minute streamSegmentTimeout and the
// chat gate's joint-idle release silently lost their evidence.
func TestNoteSegmentProgress(t *testing.T) {
	t.Run("genuine new bytes stamp the clock and zero the counter", func(t *testing.T) {
		var lastBytes atomic.Int64
		var clock atomicTimeValue
		var checks atomic.Int32
		checks.Store(7)

		before := time.Now()
		if !noteSegmentProgress(engine.DownloadProgress{Bytes: 1024}, &lastBytes, &clock, &checks) {
			t.Fatal("a fresh stream's first Bytes>0 report is progress")
		}
		after := time.Now()

		got := clock.Load()
		if got.Before(before) || got.After(after) {
			t.Errorf("segment clock = %v, want a stamp inside [%v, %v] -- this is the loop's only write to it", got, before, after)
		}
		if checks.Load() != 0 {
			t.Errorf("consecutiveLiveChecks = %d, want 0 after genuine progress", checks.Load())
		}
	})

	t.Run("a same-cumulative-Bytes echo touches neither", func(t *testing.T) {
		var lastBytes atomic.Int64
		lastBytes.Store(5_000_000)
		var clock atomicTimeValue
		var checks atomic.Int32
		checks.Store(7)

		if noteSegmentProgress(engine.DownloadProgress{Bytes: 5_000_000}, &lastBytes, &clock, &checks) {
			t.Fatal("a byte-identical echo from a re-run cancelled downloader is not progress")
		}
		if !clock.Load().IsZero() {
			t.Error("an echo must not stamp the shared segment clock -- that is the I3 livelock")
		}
		if checks.Load() != 7 {
			t.Errorf("consecutiveLiveChecks = %d, want 7 (untouched by an echo)", checks.Load())
		}
	})
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/worker/ -run TestNoteSegmentProgress -count=1 -v
```
Expected: FAIL to COMPILE — `undefined: noteSegmentProgress`.

- [ ] **Step 3: Add the function**

In `internal/worker/interruption.go`, immediately after `segmentProgressResetsStallCounters`'s closing brace:

```go
// noteSegmentProgress is runLiveStreamDownload's onSegmentProgress body,
// extracted whole so the live loop's only write to the shared
// last-new-segment clock is reachable from a test: nothing in this package
// calls runLiveStreamDownload, so in the closure these two lines were
// unreachable to every test in the tree.
//
// On genuine new bytes for this stream (segmentProgressResetsStallCounters
// decides, and owns the echo suppression) it stamps lastSegTime -- the ONE
// clock shared by the verify branch's streamSegmentTimeout and the chat
// gate's joint-idle release, "one clock, no drift" -- and zeroes the
// still-live re-verification counter. Returns whether it counted as
// progress; production discards it, tests assert on it. StoreNow() rather
// than an injected now: this runs on every progress callback of every live
// download, and an unconditional time.Now() in the caller would buy nothing
// but a parameter.
func noteSegmentProgress(p engine.DownloadProgress, lastBytes *atomic.Int64, lastSegTime *atomicTimeValue, consecutiveLiveChecks *atomic.Int32) bool {
	if !segmentProgressResetsStallCounters(p, lastBytes) {
		return false
	}
	lastSegTime.StoreNow()
	consecutiveLiveChecks.Store(0)
	return true
}
```

- [ ] **Step 4: Run the test**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/worker/ -run TestNoteSegmentProgress -count=1 -v
```
Expected: PASS (both subtests).

- [ ] **Step 5: Point production at it**

In `internal/worker/orchestrator_youtube.go`, replace the closure body:

```go
	onSegmentProgress := func(p engine.DownloadProgress, lastBytes *atomic.Int64) {
		if segmentProgressResetsStallCounters(p, lastBytes) {
			lastSegTime.StoreNow()
			consecutiveLiveChecks.Store(0)
		}
	}
```
with:
```go
	onSegmentProgress := func(p engine.DownloadProgress, lastBytes *atomic.Int64) {
		noteSegmentProgress(p, lastBytes, lastSegTime, &consecutiveLiveChecks)
	}
```

Behaviour is byte-identical: same predicate, same two writes, same order, return value discarded. Leave the comment block above the closure exactly as it is.

- [ ] **Step 6: Run the whole package**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/worker/ -count=1
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/worker/
```
Expected: PASS, no vet output. (`internal/worker` is a large package; give it a few minutes.)

- [ ] **Step 7: Two mutation checks**

1. Delete `lastSegTime.StoreNow()` from `noteSegmentProgress`, run Step 4's command → the first subtest must FAIL ("segment clock = 0001-01-01…"). Restore.
2. Delete `consecutiveLiveChecks.Store(0)`, run Step 4's command → the first subtest must FAIL ("consecutiveLiveChecks = 7, want 0"). Restore.

Re-run Step 4 after restoring; record all four outcomes.

- [ ] **Step 8: Line endings and commit**

```bash
perl -0777 -ne 'print tr/\r//' internal/worker/interruption.go internal/worker/orchestrator_youtube.go internal/worker/interruption_test.go
git add internal/worker/interruption.go internal/worker/orchestrator_youtube.go internal/worker/interruption_test.go
git commit -m "$(cat <<'EOF'
test(worker): pin the shared segment clock's only write site

No test in the tree calls runLiveStreamDownload, so deleting the clock stamp
or the counter reset from its onSegmentProgress closure stayed green.
Extracted whole as noteSegmentProgress (byte-identical behaviour, the
package's own extracted-pure-helper pattern); both deletions now fail.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 3: No GQL response body reaches a log line or an error string (R3)

**Files:**
- Modify: `internal/twitch/api.go` — `gqlBaseRetryDelay` at `:163` (const → var), a new `gqlBodySize` helper directly above `gqlRequest` (`:180`), the retry Debug line at `:196-198`, the five `string(respData)` interpolations at `:233`, `:239`, `:252`, `:254`, `:260`
- Create: `internal/twitch/api_gql_log_hygiene_test.go`
- Modify: `docs/spec/security.md` — one bullet at the end of `## Rules and Constraints` (after the "**All goroutines must have panic recovery.**" bullet at `:18`)

**Interfaces:**
- Consumes: `installProbeStub(t *testing.T, status int, body string) *atomic.Int64` from `internal/twitch/liveness_probe_test.go:31` (swaps the package-level `twitchHTTPClient`, restores it in `t.Cleanup`, refuses any host that is not `constants.TwitchURLs.GQL`, and COUNTS calls — the count is how each test proves which arm ran). `NewAPI(logger)` at `api.go:96` takes the anonymous four-method logger; the rate limiter it lazily starts pre-fills a burst of `twitchGQLRatePerSec` = 10 tokens (`:109-117`), so four back-to-back attempts never wait on it.
- Produces: `func gqlBodySize(respData []byte) string` (unexported, `internal/twitch`) rendering `"<n>-byte body"`; `gqlBaseRetryDelay` becomes a package `var` with the same value so the test can drive the retried arms to exhaustion in milliseconds.

**The decision, and why.** All six `gqlRequest` callers (`api.go:429, :503, :708, :750, :779, :895`) do `if err != nil { return …, err }` — none parses the error text, and no `strings.Contains` on a GQL error exists anywhere in `internal/twitch`. So take the safer shape the spec offers: **drop the body from the error too**, at all five sites, not only the two retried ones — the un-retried 401/403/4xx errors are precisely the ones that travel up to callers who log them. Every message prefix is preserved verbatim (`"gql http %d ("`, `"gql auth failure (%d) ("`, `"gql rate limited (429) ("`). Two of them are load-bearing for `worker.classifyProbeErr` (`internal/worker/probe_classify.go:63-79`), which lowercases the message and looks for `http 5` / `http 4` positionally: `gql http 503 (` keeps a 5xx in the network class, `gql http 404 (` keeps a 4xx terminal, and `gql auth failure (401) (` deliberately contains NEITHER substring, which is what routes a 401 to the network (keep-waiting) default — `internal/worker/worker.go:1081` says so in as many words. Today that classification is body-dependent: a 4xx body that happens to contain `timeout`, `eof` or `tls` flips a definitive 4xx into the transient class (the network-class substrings are tested first). With the body gone it is not.

The retry line loses `prev_err` entirely and gains `prev_status`, tracked in a new `lastStatus` alongside `lastErr`. `parseGQLBody`'s 200-path errors interpolate `errCheck.Errors[0].Message` — a modelled GQL field parsed out of JSON, not an intermediary's page (a non-JSON 200 body fails `json.Unmarshal` and wraps the decoder's one-character error, never the bytes) — and are deliberately left alone.

**Why the tests drive the retries to exhaustion.** The retry Debug line is the leak's outlet, but after the fix it no longer renders `lastErr` at all — so a test that only watches the log cannot tell whether the 5xx/429 error still carries the body: that error reaches a caller only when the retries are exhausted (`gql exhausted 3 retries: %w`, `:267`). A context deadline that cuts the backoff short returns `ctx.Err()`, not `lastErr`, and proves nothing about the error either (a draft of this task did exactly that, and its stated mutant survived). `gqlBaseRetryDelay` is therefore a `var` the test sets to one millisecond (restored in `t.Cleanup`), and the retried arms run all four attempts in ~3 ms: every retry line is inspected AND the exhausted error is. The three un-retried arms return `gqlRequest`'s own error on the first attempt and need no seam.

- [ ] **Step 1: Write the failing tests**

Create `internal/twitch/api_gql_log_hygiene_test.go`:

```go
package twitch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// renderingLogger records every log line with its ARGS RENDERED as
// key=value pairs. That is the point: this package's other recorders drop
// args on purpose (chat_reauth_test.go's recordingLogger, internal/cookies'
// capturingLogger) so a captured token cannot reach test output. Here the
// hazard IS in the args, so a recorder that dropped them would make the leak
// invisible and these tests vacuous. Nothing it captures is ever printed.
//
// No t.Parallel in this file: installProbeStub swaps a package-level var,
// and the retried-arm test shrinks a package-level delay.
type renderingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *renderingLogger) record(level, msg string, args ...any) {
	var b strings.Builder
	b.WriteString(level + " " + msg)
	for i := 0; i < len(args); i += 2 {
		if i+1 < len(args) {
			fmt.Fprintf(&b, " %v=%v", args[i], args[i+1])
		} else {
			fmt.Fprintf(&b, " %v", args[i])
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, b.String())
}

func (l *renderingLogger) Debug(msg string, args ...any) { l.record("DEBUG", msg, args...) }
func (l *renderingLogger) Info(msg string, args ...any)  { l.record("INFO", msg, args...) }
func (l *renderingLogger) Warn(msg string, args ...any)  { l.record("WARN", msg, args...) }
func (l *renderingLogger) Error(msg string, args ...any) { l.record("ERROR", msg, args...) }

// countLinesContaining reports how many captured lines contain sub.
func (l *renderingLogger) countLinesContaining(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

// gqlLeakMarker stands in for what actually rides in an intermediary's error
// page: an echo of the request's Authorization header. A synthetic literal,
// so it is safe to search for -- and no assertion below prints it.
const gqlLeakMarker = "OAuth-echoed-credential-marker"

// gqlLeakBody is the stub's answer for every arm. 42 bytes.
const gqlLeakBody = `{"error":"` + gqlLeakMarker + `"}`

// TestGQLRetriedArmsNeverLogOrReturnBody drives the two RETRIED arms -- 429
// without a Retry-After header, and 5xx -- to exhaustion with the backoff
// shrunk to a millisecond, so every retry Debug line fires and the error the
// caller finally receives is gqlRequest's own. Before this fix the retry
// line logged lastErr verbatim and lastErr interpolated the raw body, so
// every 5xx or 429 wrote an upstream error page into a log Moombox fans out
// over the WebSocket to the dashboard and the TUI.
func TestGQLRetriedArmsNeverLogOrReturnBody(t *testing.T) {
	prevDelay := gqlBaseRetryDelay
	gqlBaseRetryDelay = time.Millisecond
	t.Cleanup(func() { gqlBaseRetryDelay = prevDelay })

	for _, tc := range []struct {
		status int
		prefix string
	}{
		{http.StatusTooManyRequests, "gql rate limited (429) (TestOp): "},
		{http.StatusServiceUnavailable, "gql http 503 (TestOp): "},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			calls := installProbeStub(t, tc.status, gqlLeakBody)
			log := &renderingLogger{}
			a := NewAPI(log)

			_, err := a.gqlRequest(context.Background(), "TestOp", map[string]any{"q": 1}, "")
			if err == nil {
				t.Fatalf("a %d answer must not succeed", tc.status)
			}
			if got := calls.Load(); got != gqlMaxRetries+1 {
				t.Fatalf("stub answered %d times, want %d (the first attempt plus every retry)", got, gqlMaxRetries+1)
			}
			if n := log.countLinesContaining("twitch gql retry"); n != gqlMaxRetries {
				t.Fatalf("%d retry Debug lines, want %d -- without them this test is vacuous", n, gqlMaxRetries)
			}

			// The log: no body, no rendering of the previous error at all
			// (not even its sanitised form), and the status the retry
			// followed.
			if n := log.countLinesContaining(gqlLeakMarker); n != 0 {
				t.Errorf("%d log line(s) carry the response body; the line itself is not printed here on purpose", n)
			}
			last := errors.Unwrap(err) // the exhausted wrap's %w: the final lastErr
			if last == nil {
				t.Fatal("the exhausted-retries error must wrap the last attempt's error")
			}
			if n := log.countLinesContaining(last.Error()); n != 0 {
				t.Errorf("%d retry line(s) render the previous error; the retry line reports prev_status, never prev_err", n)
			}
			if n := log.countLinesContaining(fmt.Sprintf("prev_status=%d", tc.status)); n != gqlMaxRetries {
				t.Errorf("%d retry line(s) report prev_status=%d, want every one of the %d", n, tc.status, gqlMaxRetries)
			}

			// The returned error: the SIZE instead of the bytes, prefix
			// intact -- worker.classifyProbeErr reads the status out of
			// "gql http <code> (" positionally.
			if strings.Contains(err.Error(), gqlLeakMarker) {
				t.Error("the returned error still interpolates the response body; not printed here on purpose")
			}
			want := tc.prefix + fmt.Sprintf("%d-byte body", len(gqlLeakBody))
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the returned error must contain %q -- the prefix verbatim and the body SIZE, not its bytes", want)
			}
		})
	}
}

// TestGQLUnretriedArmsCarryByteCountNotBody covers the three arms that return
// without a retry -- exactly the errors that travel up to callers who log
// them: 401/403 with a token (wraps ErrTwitchAuthExpired), 401/403 without
// one (no sentinel: anonymous GQL legitimately gets 401 on some paths, and
// looping the user through a login flow for it would be pointless), and any
// other 4xx.
func TestGQLUnretriedArmsCarryByteCountNotBody(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		token    string
		prefix   string
		sentinel bool
	}{
		{"403 with token", http.StatusForbidden, "a-token", "gql auth failure (403) (TestOp): ", true},
		{"401 without token", http.StatusUnauthorized, "", "gql auth failure (401) (TestOp): ", false},
		{"400 other 4xx", http.StatusBadRequest, "", "gql http 400 (TestOp): ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := installProbeStub(t, tc.status, gqlLeakBody)
			log := &renderingLogger{}
			a := NewAPI(log)

			_, err := a.gqlRequest(context.Background(), "TestOp", map[string]any{"q": 1}, tc.token)
			if err == nil {
				t.Fatalf("a %d answer must not succeed", tc.status)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("stub answered %d times, want exactly 1 -- this arm must not retry", got)
			}
			if errors.Is(err, ErrTwitchAuthExpired) != tc.sentinel {
				t.Errorf("errors.Is(err, ErrTwitchAuthExpired) = %v, want %v", !tc.sentinel, tc.sentinel)
			}
			if strings.Contains(err.Error(), gqlLeakMarker) {
				t.Error("the returned error still interpolates the response body; not printed here on purpose")
			}
			want := tc.prefix + fmt.Sprintf("%d-byte body", len(gqlLeakBody))
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the returned error must contain %q -- the prefix verbatim and the body SIZE, not its bytes", want)
			}
			if n := log.countLinesContaining(gqlLeakMarker); n != 0 {
				t.Errorf("%d log line(s) carry the response body; not printed here on purpose", n)
			}
		})
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/twitch/ -run 'TestGQL(RetriedArms|UnretriedArms)' -count=1 -v
```
Expected: FAIL to COMPILE — `cannot assign to gqlBaseRetryDelay` (it is still a const). Do the first half of Step 3 (the `var`) and re-run: now all five subtests FAIL — the two retried ones with "3 log line(s) carry the response body" and the missing `42-byte body`, the three un-retried ones with "the returned error still interpolates the response body" and the missing `42-byte body`. (`gqlLeakBody` is 42 bytes; `gqlBodySize` does not exist yet, but the assertion is on the rendered text, so nothing else needs to compile first.)

- [ ] **Step 3: The delay seam and the helper**

In `internal/twitch/api.go`, replace `const gqlBaseRetryDelay = 1 * time.Second` (`:163`) — keep its two comment lines above — with:

```go
// A var, not a const, for exactly one reader: api_gql_log_hygiene_test.go
// shrinks it to a millisecond so the retried arms can be driven to
// exhaustion. Production never assigns it.
var gqlBaseRetryDelay = 1 * time.Second
```

The `min(gqlBaseRetryDelay<<(attempt-1), gqlMaxRetryDelay)` at `:195` compiles unchanged — a shift on a `time.Duration` variable.

Then, directly above `func (a *API) gqlRequest`:

```go
// gqlBodySize renders a GQL response body as its SIZE, never its bytes.
//
// Every gqlRequest error used to interpolate string(respData). An
// intermediary's error page can echo the request headers -- including
// Authorization: OAuth <token> -- and those errors reach both the retry log
// line and every caller that logs a failure, which Moombox fans out over the
// WebSocket to the dashboard and the TUI. The size is what an operator
// actually needs from a body they must not see: it separates "empty 503" from
// "an HTML error page" without carrying either.
func gqlBodySize(respData []byte) string {
	return fmt.Sprintf("%d-byte body", len(respData))
}
```

- [ ] **Step 4: Replace the five interpolations**

In `gqlRequest`, each `string(respData)` becomes `gqlBodySize(respData)` — five sites (`:233` 429, `:239` 5xx, `:252` auth+sentinel, `:254` auth, `:260` other 4xx), message prefixes and every other argument untouched:

```go
			lastErr = fmt.Errorf("gql rate limited (429) (%s): %s", opLabel(opName), gqlBodySize(respData))
			lastErr = fmt.Errorf("gql http %d (%s): %s", statusCode, opLabel(opName), gqlBodySize(respData))
				return nil, fmt.Errorf("gql auth failure (%d) (%s): %s: %w", statusCode, opLabel(opName), gqlBodySize(respData), ErrTwitchAuthExpired)
			return nil, fmt.Errorf("gql auth failure (%d) (%s): %s", statusCode, opLabel(opName), gqlBodySize(respData))
			return nil, fmt.Errorf("gql http %d (%s): %s", statusCode, opLabel(opName), gqlBodySize(respData))
```
(Those five lines are not adjacent in the file — each replaces the one at the line number listed above. When you are done, `grep -c 'string(respData)' internal/twitch/api.go` prints `0`.)

- [ ] **Step 5: Track the status and rewrite the retry line**

Replace `	var lastErr error` with:

```go
	var lastErr error
	// lastStatus is what the retry line reports instead of the previous
	// error: 0 for a transport-level failure, otherwise the HTTP status that
	// caused the retry. The error itself is deliberately NOT logged -- a
	// transport error wraps whatever the proxy said, and that is the same
	// class of untrusted upstream text gqlBodySize exists to keep out.
	lastStatus := 0
```

Set it at the three transient sites — `lastStatus = 0` beside the transport-error `lastErr`, `lastStatus = statusCode` beside the 429 and 5xx `lastErr` assignments (429's `statusCode` is already `http.StatusTooManyRequests`):

```go
			lastErr = fmt.Errorf("gql request (%s): %w", opLabel(opName), doErr)
			lastStatus = 0
```
```go
			lastErr = fmt.Errorf("gql rate limited (429) (%s): %s", opLabel(opName), gqlBodySize(respData))
			lastStatus = statusCode
```
```go
			lastErr = fmt.Errorf("gql http %d (%s): %s", statusCode, opLabel(opName), gqlBodySize(respData))
			lastStatus = statusCode
```

And the Debug line itself:

```go
			if a.logger != nil {
				a.logger.Debug("twitch gql retry", "op", opLabel(opName), "attempt", attempt, "delay", delay.String(), "prev_status", lastStatus)
			}
```

`lastErr` is still returned by the exhausted-retries wrap at the end of the function — that is the only place it goes now.

- [ ] **Step 6: Run the package**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/twitch/ -count=1
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/twitch/
```
Expected: PASS (the five new subtests plus the existing ones — `api_test.go:19`'s `TestErrTwitchAuthExpiredWrapping` mirrors the auth format string, which is unchanged; only its `%s` argument is). No vet output. The package still finishes in a few seconds: the retried arms cost ~3 ms each.

- [ ] **Step 7: Seven mutation checks**

For each one: apply it, run Step 2's command, confirm the named failure, restore, re-run and confirm PASS before the next. Record all fourteen outcomes in your report.

1. `string(respData)` back at `:239` (5xx) → `TestGQLRetriedArmsNeverLogOrReturnBody/503` fails: "the returned error still interpolates the response body".
2. Back at `:233` (429) → `/429` fails the same way.
3. Back at `:252` (auth + sentinel) → `TestGQLUnretriedArmsCarryByteCountNotBody/403_with_token` fails.
4. Back at `:254` (auth, no token) → `/401_without_token` fails.
5. Back at `:260` (other 4xx) → `/400_other_4xx` fails.
6. ADD `, "prev_err", lastErr` to the Debug line after `"prev_status", lastStatus` → both retried subtests fail: "3 retry line(s) render the previous error". (Replacing `prev_status` outright leaves `lastStatus` unused and fails to COMPILE, which proves nothing — add, do not replace.)
7. Delete the `lastStatus = statusCode` under the 5xx `lastErr` → `/503` fails: "0 retry line(s) report prev_status=503, want every one of the 3".

- [ ] **Step 8: The security.md sentence**

In `docs/spec/security.md`, append one bullet to `## Rules and Constraints`, after the "**All goroutines must have panic recovery.**" bullet (`:18`) and before the `---`:

```markdown
- **Upstream response bodies never reach a log line or an error string.** `gqlRequest` (`internal/twitch/api.go`) reports a failed Twitch GQL call by status code and body SIZE — `gqlBodySize` renders `"<n>-byte body"`, never the bytes — at all five failure arms, and its retry line logs `op`/`attempt`/`delay`/`prev_status`, never the previous error. The reason is concrete: an intermediary's error page can echo the request's `Authorization: OAuth …` header, and Moombox fans log lines out over the WebSocket to the dashboard and the TUI. The message prefixes are load-bearing and unchanged: `classifyProbeErr` (`internal/worker/probe_classify.go`) reads `http 5`/`http 4` out of `gql http <code> (` positionally, and the auth arm's `gql auth failure (<code>) (` deliberately contains neither substring, which is what keeps a 401 in the network (keep-waiting) class. Held by `TestGQLRetriedArmsNeverLogOrReturnBody` and `TestGQLUnretriedArmsCarryByteCountNotBody` (`internal/twitch/api_gql_log_hygiene_test.go`).
```

(Task 5's checker will pair `gqlRequest`, `classifyProbeErr` and the two test names with the files cited beside them and resolve all four — that is the reason the file paths sit right after the symbols.)

- [ ] **Step 9: Line endings and commit**

```bash
perl -0777 -ne 'print tr/\r//' internal/twitch/api.go internal/twitch/api_gql_log_hygiene_test.go docs/spec/security.md
git add internal/twitch/api.go internal/twitch/api_gql_log_hygiene_test.go docs/spec/security.md
git commit -m "$(cat <<'EOF'
fix(twitch): a GQL response body never reaches a log line or an error

The retry Debug line logged prev_err verbatim and the errors interpolated
string(respData), so an intermediary's error page -- which can echo the
Authorization header -- was written at Debug on every retry and handed to
every caller that logs a failure. All five failure arms now report
gqlBodySize ("<n>-byte body") and the retry line reports prev_status. No
caller reads the body out of the error string (all six do
if err != nil { return err }). Message prefixes preserved for
classifyProbeErr's positional status read. gqlBaseRetryDelay is a var so
the test can drive the retried arms to exhaustion; production never
assigns it.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 4: The `Platforms` comment tells the truth, and CSS/SVG are pinned LF (R4 + R5)

**Files:**
- Modify: `internal/config/types.go:209-211` (comment only — the `Platforms []string` field line at `:212` is untouched)
- Modify: `.gitattributes`

**Interfaces:**
- Consumes: nothing. Produces: nothing. Both edits are non-code.

Two same-shape small edits batched into one task: neither has a test cycle of its own, and a reviewer would accept or reject them together.

- [ ] **Step 1: Read the truth you are about to mirror**

```bash
git show HEAD:cmd/moombox/services.go | sed -n '292,331p'
git show HEAD:docs/spec/data-and-storage.md | sed -n '529p'
```
Confirm three things before editing: `detectCookiePlatforms` prefers `meta.Platforms` (sidecar) outright; falls back to the LOOSE `HasAnyYouTubeAuthCookie`/`HasAnyTwitchAuthCookie`; and the call site runs only when `len(cfg.Cookies.Platforms) == 0 && len(cfg.Cookies.ActivePlatforms) == 0`.

- [ ] **Step 2: Replace the comment**

In `internal/config/types.go`, replace:

```go
	// Platforms is the auto-detected platform list — populated at
	// startup from cookie file inspection (HasYouTubeAuthCookies /
	// HasTwitchAuthCookies). Treat as read-only-from-config.
```
with:
```go
	// Platforms is the platform list SEEDED on first run by
	// detectCookiePlatforms (cmd/moombox/services.go), and only when BOTH
	// this list and ActivePlatforms are empty. The sidecar's recorded
	// meta.Platforms wins outright when it has one — a real verification
	// result, never unioned with a guess; only in its absence does the
	// seed fall back to the LOOSE HasAnyYouTubeAuthCookie /
	// HasAnyTwitchAuthCookie predicates, not the strict
	// HasYouTubeAuthCookies / HasTwitchAuthCookies pair, because a jar
	// holding SAPISID with LOGIN_INFO already cleared is a CONFIGURED
	// platform with broken credentials, not an unconfigured one. Nothing
	// automatic ever prunes it — the automatic writers only add — and the
	// sole removal path is an operator replacing the list through
	// PUT /api/config. Treat as read-only-from-config.
```

Do not touch `cmd/moombox/main.go:276-278`, and do not touch the `DpapiFallback` field below (Arc 12c adds `Acquisition` there).

- [ ] **Step 3: Prove it compiles and is comment-only**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./internal/config/
git diff -U0 -- internal/config/types.go
```
Expected: builds; every added/removed line in the diff starts with `//`.

- [ ] **Step 4: Record the gitattributes "before"**

```bash
git ls-files --eol -- '*.css' '*.svg'
```
Expected, verbatim (two files, no attribute):
```
i/lf    w/crlf  attr/                 	web/public/favicon.svg
i/lf    w/crlf  attr/                 	web/public/moombox.css
```

- [ ] **Step 5: Add the two rules**

In `.gitattributes`, replace:

```
# JS/TS, JSON, and config we control are LF too — same reasoning.
*.html text eol=lf
```
with:
```
# Web assets, JS/TS, JSON, and config we control are LF too — same reasoning.
*.html text eol=lf
*.css text eol=lf
*.svg text eol=lf
```

`.gitattributes` is CRLF in this Windows working tree (no rule covers itself) and LF in the index — so do not "fix" its line endings; `core.autocrlf` normalises on add. Step 7 checks the index blob, which is what ships.

- [ ] **Step 6: Renormalise (expected: nothing to stage but the attributes file)**

```bash
git add --renormalize -- '*.css' '*.svg'
git status --short
```
Expected: `git status --short` shows only `M .gitattributes` (and the config file from Step 2). Neither `moombox.css` nor `favicon.svg` appears — both index blobs are already LF, which is exactly why the rule is prevention, not repair.

- [ ] **Step 7: Record the "after" and check the index blob**

```bash
git add .gitattributes
git ls-files --eol -- '*.css' '*.svg'
git show :.gitattributes | perl -0777 -ne 'print tr/\r//'
perl -0777 -ne 'print tr/\r//' internal/config/types.go
```
Expected: the two rows now read `attr/text eol=lf` (the `i/lf` column unchanged); the staged `.gitattributes` blob prints `0` carriage returns; `types.go` prints `0`.

- [ ] **Step 8: Commit**

```bash
git add internal/config/types.go .gitattributes
git commit -m "$(cat <<'EOF'
docs(config): the Platforms comment states the real seeding rule; pin CSS/SVG to LF

The comment named the strict HasYouTubeAuthCookies / HasTwitchAuthCookies
pair that first-run detection used BEFORE the loose predicates replaced it.
detectCookiePlatforms takes the sidecar's verified list first, falls back to
HasAny*, seeds only when both lists are empty, and nothing automatic prunes
the result. Mirrors data-and-storage.md's table row. Comment-only; main.go's
SetExpectedPlatforms block untouched.

*.css and *.svg join the LF rules. git add --renormalize was a no-op on
today's index -- both blobs are already LF -- so this is prevention against
a future CRLF commit from a Windows checkout.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 5: A citation-rot test guards the six spec docs (R6)

**Files:**
- Create: `internal/docs/doc.go`, `internal/docs/citations_test.go`, `internal/docs/citation_allowlist.txt`
- Modify: `docs/spec/architecture.md` (`:201`, `:219`, `:918`), `docs/spec/platform-services.md` (`:789`, `:907`), `docs/spec/operations.md` (`:130`)
- Modify: `internal/youtube/watch_page.go:824-830` (comment only)

**Interfaces:**
- Consumes: `internal/twitch/api_gql_log_hygiene_test.go` must already exist (Task 3) — `security.md`'s new bullet cites it, and the checker resolves that citation.
- Produces: `internal/docs` — a test-only package. No exported API.

**Package placement.** `internal/docs` with a one-line `doc.go`, not `docs/spec/spec_test.go`: a directory holding only `_test.go` files is a package `go build ./...` reports as having no non-test Go files, and CI builds before it tests. A package clause costs one file and makes the directory ordinary. `internal/` rather than `docs/` because the test finds the docs by walking up to `go.mod` anyway.

**What the checker does.**
1. **Path citations** — a backticked token starting `internal/ cmd/ web/ tools/ docs/ bgutil-sidecar/ .github/` whose last segment ends in a known extension (optionally with a `:123` / `:123-456` suffix) must exist on disk. 310 today.
2. **Directory citations** — the same prefixes with no dot in any segment (trailing `/` stripped) must be a directory. 60 today.
3. **Symbol pairs** — the backtick span IMMEDIATELY BEFORE a `.go` path citation, when it is identifier-shaped (`Name`, `pkg.Name`, `Type.Method`, or any of those written as a call — `run()`, `refresh(ctx, allowFallback)` — with the call suffix stripped) and the gap between the two spans is a bare connector (an optional closing `**`, an optional `(`/`[`/`,`/`—`, then EITHER one lowercase word plus a bracket or comma — ` gate (`, ` helper, ` — OR an optional single lowercase word plus one of `see`/`in`/`at`/`from`/`via` — ` in `, ` wired at `, ` seeding at `, ` function in `), must appear in that file's CODE — as an identifier (a declaration's own name is one) or inside a string literal. 155 today. (A first draft of this task paired 134 with a narrower connector and no call shapes; the 21 it missed were all real citations — `SetExpectedPlatforms` seeding at `cmd/moombox/main.go`, `NewServer()` in `internal/web/server.go`, `refresh(ctx, allowFallback)` (`internal/cookies/refresh.go`) — and all 21 resolve.) **Appears, not "is declared", is deliberate**: the docs legitimately cite wiring and call sites — `AutoCookieService.VerifyYouTubeAuth` at `cmd/moombox/services.go` is the CALL site, `runtime.ReadMemStats` at `internal/web/routes/jobs.go` a stdlib use, `FetchVariantsFn` at `internal/worker/worker.go` another package's struct field assigned there, `finishCtx` in `cmd/moombox/tui_wiring.go` a local, `js_to_json` at `internal/utils/jsjson.go` a name inside an error string — and requiring a top-level declaration flags all five, every one a correct citation. Comments do NOT count: a symbol surviving only in a comment is exactly the rot at `platform-services.md:907`. Known weakness, accepted: a short common name (`Load` at `internal/cookies/jar.go`) resolves against ANY `Load` identifier in that file, so a rename of that one method would not be caught. `TestCitationShapes` pins both regexes with the shapes that must and must not pair, so a loosening that starts pairing prose fails by name.
4. **Absence and state claims** — five doc sentences, each located by a distinctive substring (line numbers drift; the substring not being found is itself a failure), each re-verified by walking non-test ASTs: `NewRefreshService`'s dead interval parameter; the removed `Logger` per-job buffer API; the writer set behind "Nothing automatic ever prunes it" — exactly three files, each read before it was listed: the first-run seed and the wizard's `FinishSetup` union-merge in `cmd/moombox/services.go` (`:522`, `:1001` — both only add), the legacy-section migration in `internal/config/config.go` (`:389`), and the operator's `PUT /api/config` in `internal/web/routes/config_routes.go` (`:580` — REPLACES the list, which is the sole removal path the sentence names); and `const livenessRecoveryArmed = false`, quoted verbatim by `data-and-storage.md` and `operations.md` — the day the owner arms the pilot both sentences must flip, and `pilotDisarmed` is the check that says so (it READS the constant; nothing here writes it).
5. **Heading cross-references** — every `§` reference resolves to a heading of the doc it points at. RFC section numbers (`RFC 6265 §4.1.2.3`) and `spec §10` are excluded by shape.

An `internal/docs/citation_allowlist.txt` file exists for citations the checker is wrong about; it is empty today.

- [ ] **Step 1: Create the package file and the allowlist**

`internal/docs/doc.go`:

```go
// Package docs carries no production code. It exists so the spec-citation
// test beside it lives in a package `go test ./...` reaches, and so
// `go build ./...` sees an ordinary package rather than a directory of
// nothing but test files. The checks are in citations_test.go.
package docs
```

`internal/docs/citation_allowlist.txt`:

```
# Allowlist for the spec-citation checks in citations_test.go.
#
# One entry per line; blank lines and lines starting with # are ignored.
#
#   <doc>.md|<path>            skip the existence check for that citation
#   <doc>.md|<symbol>|<path>   skip the symbol check for that pair
#
# An entry asserts the CHECKER is wrong about a citation that is right --
# prose shaped like a citation, a TOML key that looks like an identifier, a
# path that is deliberately illustrative. Doc rot does not belong here: fix
# the doc. Every entry carries its reason on the line above it.
#
# Empty today: every citation in docs/spec resolves.
```

- [ ] **Step 2: Write the checker — helpers**

Create `internal/docs/citations_test.go` with this first half:

```go
package docs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// specDocs are the six deep-dive docs that carry code citations.
// appendix-metrics.md (volatile numbers), design-philosophy.md and
// vision-and-purpose.md (prose) are out on purpose.
var specDocs = []string{
	"architecture.md",
	"data-and-storage.md",
	"operations.md",
	"platform-services.md",
	"security.md",
	"user-interfaces.md",
}

// citationPrefixes are the repo-relative roots a path citation may start
// with. A backticked token starting with anything else is prose.
var citationPrefixes = []string{"internal/", "cmd/", "web/", "tools/", "docs/", "bgutil-sidecar/", ".github/"}

var (
	// fileExtRe recognises the last segment of a FILE citation.
	fileExtRe = regexp.MustCompile(`\.(go|js|mjs|ts|json|md|html|css|toml|yml|yaml|sh|txt|sql|gz|svg|ico)$`)
	// lineRefRe strips a trailing :123 or :123-456 line reference.
	lineRefRe = regexp.MustCompile(`:\d+(-\d+)?$`)
	// identRe is the shape a symbol citation must have: Name, pkg.Name,
	// Type.Method, optionally written as a call -- Name(), Name(ctx, x).
	// The docs cite functions both ways; the call suffix is stripped by
	// symbolName before the lookup.
	identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?(\([^()]*\))?$`)
	// callSuffixRe is that optional call suffix.
	callSuffixRe = regexp.MustCompile(`\([^()]*\)$`)
	// connectorRe is the ONLY text allowed between a symbol citation and the
	// path citation it is paired with: an optional closing bold marker, an
	// optional bracket/comma/dash, then EITHER one lowercase word followed by
	// a bracket or comma ("gate (", "helper, ") OR an optional single
	// lowercase word plus one of see/in/at/from/via ("in ", "wired at ",
	// "seeding at ", "function in "). Anything longer means the path is not
	// a citation OF that symbol -- "returns them under one `RLock`, because
	// `internal/twitch/chat_irc.go` builds one handshake" pairs nothing, and
	// neither does "`liveDownloadChat` is true (`…`)".
	connectorRe = regexp.MustCompile(`^[ \t]*(?:\*\*)?[ \t]*(?:[(\[,—-][ \t]*)?(?:[a-z]+[ \t]*[(\[,][ \t]*|(?:[a-z]+ )?(?:see|in|at|from|via) )?$`)
	// docNameRe finds the doc a "§ Heading" reference points at.
	docNameRe = regexp.MustCompile("([a-z-]+\\.md)[`)\\s]*$")
	// rfcRe excludes RFC section numbers from the heading check.
	rfcRe = regexp.MustCompile(`(?i)RFC\s+[0-9A-Za-z-]+(bis)?\s*$`)
)

// symbolName strips a call suffix: "refresh(ctx, allowFallback)" -> "refresh".
func symbolName(span string) string {
	return callSuffixRe.ReplaceAllString(span, "")
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// span is one backtick-delimited run: start is the opening backtick's byte
// offset, end is one past the closing backtick.
type span struct {
	start, end int
	text       string
}

func backtickSpans(line string) []span {
	var out []span
	for i := 0; i < len(line); i++ {
		if line[i] != '`' {
			continue
		}
		j := strings.IndexByte(line[i+1:], '`')
		if j < 0 {
			break
		}
		j += i + 1
		out = append(out, span{start: i, end: j + 1, text: line[i+1 : j]})
		i = j
	}
	return out
}

// docLines reads a spec doc, LF-normalised.
func docLines(t *testing.T, root, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "docs", "spec", name))
	if err != nil {
		t.Fatalf("read docs/spec/%s: %v", name, err)
	}
	return strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
}

// forEachProseLine calls fn for every line outside a ``` fence, 1-indexed.
// Fenced blocks are skipped because they are literal listings: package
// inventories with truncated names, shell transcripts, JSON.
func forEachProseLine(lines []string, fn func(lineNo int, line string)) {
	fenced := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		fn(i+1, line)
	}
}

func hasCitationPrefix(tok string) bool {
	for _, p := range citationPrefixes {
		if strings.HasPrefix(tok, p) {
			return true
		}
	}
	return false
}

// parseAllowlist turns the allowlist file into a key set.
func parseAllowlist(s string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[line] = true
	}
	return out
}

func loadAllowlist(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile("citation_allowlist.txt")
	if err != nil {
		t.Fatalf("read citation_allowlist.txt: %v", err)
	}
	return parseAllowlist(string(b))
}

func TestCitationAllowlistParsing(t *testing.T) {
	got := parseAllowlist("# a comment\n\narchitecture.md|internal/x/y.go\n  operations.md|Foo|cmd/moombox/z.go  \n")
	if len(got) != 2 || !got["architecture.md|internal/x/y.go"] || !got["operations.md|Foo|cmd/moombox/z.go"] {
		t.Errorf("comments and blank lines must be dropped and entries trimmed; got %v", got)
	}
}

// TestCitationShapes pins the two regexes the symbol check hangs on, so a
// loosening that starts pairing prose (or a tightening that stops pairing
// real citations) fails here with the offending shape named, not silently
// in the doc walk.
func TestCitationShapes(t *testing.T) {
	for _, s := range []string{"Foo", "pkg.Foo", "Type.Method", "run()", "refresh(ctx, allowFallback)", "Type.Method(origin)"} {
		if !identRe.MatchString(s) {
			t.Errorf("%q must be identifier-shaped", s)
		}
	}
	for _, s := range []string{"PUT /api/config", "0.0.0.0", "go:embed", "//go:embed", "X-Forwarded-For", "const x = false", "f(a) (b, error)"} {
		if identRe.MatchString(s) {
			t.Errorf("%q must NOT be identifier-shaped", s)
		}
	}
	if got := symbolName("CookieJar.GenerateAuthorizationHeader(origin)"); got != "CookieJar.GenerateAuthorizationHeader" {
		t.Errorf("symbolName stripped to %q", got)
	}
	for _, g := range []string{" (", ", ", " in ", " at ", " — ", " (see ", " wired at ", " seeding at ", " function in ", " gate (", " helper, ", "** (", ""} {
		if !connectorRe.MatchString(g) {
			t.Errorf("gap %q must pair", g)
		}
	}
	for _, g := range []string{", because ", " is true (", "'s no-segment backstop (both ", " | ", ". ", " (all bound in ", " reads the jar; its consumer is "} {
		if connectorRe.MatchString(g) {
			t.Errorf("gap %q must NOT pair", g)
		}
	}
}

// fileFacts is what one cited .go file's CODE contains: every identifier
// (a declaration's own name is one, so declarations are covered) and every
// string literal. Comments are deliberately excluded -- a symbol that
// survives only in a comment is exactly the rot at platform-services.md:907.
type fileFacts struct {
	idents   map[string]bool
	literals string
}

// resolves accepts Name, pkg.Name, Type.Method and Type.Field: the dotted
// forms fall back to their last segment, which is the identifier the file
// actually spells.
func (ff *fileFacts) resolves(sym string) bool {
	if ff.idents[sym] {
		return true
	}
	last := sym
	if i := strings.LastIndex(sym, "."); i >= 0 {
		last = sym[i+1:]
	}
	if ff.idents[last] {
		return true
	}
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(last) + `\b`).MatchString(ff.literals)
}

func receiverTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	case *ast.IndexExpr:
		return receiverTypeName(t.X)
	case *ast.IndexListExpr:
		return receiverTypeName(t.X)
	}
	return ""
}

func parseGoFile(t *testing.T, abs string) *fileFacts {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, abs, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", abs, err)
	}
	ff := &fileFacts{idents: map[string]bool{}}
	var lits strings.Builder
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			ff.idents[n.Name] = true
		case *ast.BasicLit:
			if n.Kind == token.STRING {
				lits.WriteString(n.Value)
				lits.WriteByte('\n')
			}
		}
		return true
	})
	ff.literals = lits.String()
	return ff
}
```

- [ ] **Step 3: Write the checker — the three tests**

Append to `internal/docs/citations_test.go`:

```go
// TestSpecDocCitationsResolve is checks (a), (a') and (b): every path, every
// directory and every symbol the six docs cite still exists. It names each
// failure by doc:line so the fix is a one-line edit, not a hunt.
func TestSpecDocCitationsResolve(t *testing.T) {
	root := repoRoot(t)
	allow := loadAllowlist(t)
	factsCache := map[string]*fileFacts{}

	for _, doc := range specDocs {
		lines := docLines(t, root, doc)
		forEachProseLine(lines, func(lineNo int, line string) {
			spans := backtickSpans(line)
			for i, sp := range spans {
				tok := sp.text
				if !hasCitationPrefix(tok) || strings.ContainsAny(tok, " \t") {
					continue
				}
				p := lineRefRe.ReplaceAllString(tok, "")

				// (a) file citation
				if fileExtRe.MatchString(p) {
					if allow[doc+"|"+p] {
						continue
					}
					if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err != nil {
						t.Errorf("%s:%d cites `%s`, which does not exist", doc, lineNo, tok)
						continue
					}
					if !strings.HasSuffix(p, ".go") || i == 0 {
						continue
					}
					// (b) symbol paired with a .go citation
					prev := spans[i-1]
					if !identRe.MatchString(prev.text) || fileExtRe.MatchString(prev.text) {
						continue
					}
					if !connectorRe.MatchString(line[prev.end:sp.start]) {
						continue
					}
					sym := symbolName(prev.text)
					if allow[doc+"|"+sym+"|"+p] {
						continue
					}
					abs := filepath.Join(root, filepath.FromSlash(p))
					ff, ok := factsCache[abs]
					if !ok {
						ff = parseGoFile(t, abs)
						factsCache[abs] = ff
					}
					if !ff.resolves(sym) {
						t.Errorf("%s:%d cites `%s` (`%s`), but that file neither declares nor mentions it (a comment does not count)", doc, lineNo, prev.text, tok)
					}
					continue
				}

				// (a') directory citation: no dot in any segment
				dir := strings.TrimSuffix(p, "/")
				if strings.Contains(dir, ".") {
					continue // pkg.Type.Method and friends -- not a path
				}
				if allow[doc+"|"+p] {
					continue
				}
				st, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir)))
				if err != nil || !st.IsDir() {
					t.Errorf("%s:%d cites `%s`, which is not a directory", doc, lineNo, tok)
				}
			}
		})
	}
}

// TestSpecDocHeadingReferencesResolve is check (d). A reference resolves when
// some heading of the target doc -- backticks and a trailing parenthetical
// stripped, lowercased -- is a WORD-prefix of the text after the §. That
// handles both "§ Cookies" pointing at "## Cookies (internal/cookies/)" and
// "§ Refresh Service for the conditions" trailing off into prose. Known
// blind spot, accepted: a short heading is a word-prefix of a longer,
// non-existent one ("§ Cookies Import" would pass on "## Cookies").
func TestSpecDocHeadingReferencesResolve(t *testing.T) {
	root := repoRoot(t)
	headings := map[string][]string{}
	for _, doc := range specDocs {
		forEachProseLine(docLines(t, root, doc), func(_ int, line string) {
			if !strings.HasPrefix(line, "#") {
				return
			}
			h := strings.ReplaceAll(strings.TrimLeft(line, "# "), "`", "")
			if i := strings.LastIndex(h, " ("); i > 0 && strings.HasSuffix(strings.TrimSpace(h), ")") {
				h = h[:i]
			}
			if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
				headings[doc] = append(headings[doc], h)
			}
		})
	}

	isWordPrefix := func(hs []string, tail string) bool {
		for _, h := range hs {
			if !strings.HasPrefix(tail, h) {
				continue
			}
			rest := tail[len(h):]
			if rest == "" {
				return true
			}
			c := rest[0]
			if !(c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
				return true
			}
		}
		return false
	}

	refs := 0
	for _, doc := range specDocs {
		forEachProseLine(docLines(t, root, doc), func(lineNo int, line string) {
			for idx := 0; ; {
				k := strings.Index(line[idx:], "§")
				if k < 0 {
					return
				}
				pos := idx + k
				before := line[:pos]
				after := strings.TrimLeft(line[pos+len("§"):], " ")
				idx = pos + len("§")
				if rfcRe.MatchString(before) {
					continue // RFC 6265 §4.1.2.3
				}
				if after == "" || (after[0] >= '0' && after[0] <= '9') {
					continue // spec §10
				}
				target := doc
				if m := docNameRe.FindStringSubmatch(before); m != nil {
					target = m[1]
				}
				if j := strings.IndexByte(after, '`'); j >= 0 {
					after = after[:j]
				}
				if _, known := headings[target]; !known {
					t.Logf("%s:%d references a heading in %s, which is outside the checked set", doc, lineNo, target)
					continue
				}
				refs++
				if !isWordPrefix(headings[target], strings.ToLower(after)) {
					t.Errorf("%s:%d references §%.40s in %s, which has no such heading", doc, lineNo, after, target)
				}
			}
		})
	}
	if refs < 10 {
		t.Errorf("only %d §-references were checked -- the scan is broken (there were 18 when this test was written)", refs)
	}
}

// parsedFile is one non-test Go file of the module.
type parsedFile struct {
	rel  string // slash-separated, repo-relative
	file *ast.File
}

// nonTestGoFiles parses every non-_test.go file in the module. Build tags are
// irrelevant: a Linux-only writer counts as much as a Windows one.
func nonTestGoFiles(t *testing.T, root string) []parsedFile {
	t.Helper()
	fset := token.NewFileSet()
	var out []parsedFile
	skip := map[string]bool{"references": true, "node_modules": true, "bgutil-sidecar": true}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || skip[name]) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, parsedFile{rel: filepath.ToSlash(rel), file: f})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(out) < 100 {
		t.Fatalf("only %d non-test Go files found -- the walk is wrong, and every absence check below would pass vacuously", len(out))
	}
	return out
}

// absenceClaim is a doc sentence that asserts something is NOT there (or is
// pinned to one value). Each is located by key (line numbers drift), and a
// key that no longer appears is itself a failure: the sentence was reworded
// and the check must be re-aimed.
type absenceClaim struct {
	doc   string
	key   string
	why   string
	check func(t *testing.T, files []parsedFile)
}

// pilotDisarmed re-verifies "const livenessRecoveryArmed = false", which two
// docs quote verbatim. The day the owner flips the constant, both sentences
// must flip with it -- this is the check that says so.
func pilotDisarmed(t *testing.T, files []parsedFile) {
	found := false
	for _, pf := range files {
		for _, d := range pf.file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs, ok := s.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, n := range vs.Names {
					if n.Name != "livenessRecoveryArmed" {
						continue
					}
					found = true
					if i >= len(vs.Values) {
						t.Errorf("%s declares livenessRecoveryArmed without a value", pf.rel)
						continue
					}
					if id, ok := vs.Values[i].(*ast.Ident); !ok || id.Name != "false" {
						t.Errorf("%s declares livenessRecoveryArmed with a value other than the literal false -- the docs quote `const livenessRecoveryArmed = false` and must change with it", pf.rel)
					}
				}
			}
		}
	}
	if !found {
		t.Error("no production const named livenessRecoveryArmed exists -- the docs quote it; re-aim this check")
	}
}

func TestSpecDocAbsenceClaimsHold(t *testing.T) {
	root := repoRoot(t)
	files := nonTestGoFiles(t, root)

	claims := []absenceClaim{{
		doc: "data-and-storage.md",
		key: "so nothing in production feeds the interval parameter",
		why: "RefreshService's interval parameter is dead in production",
		check: func(t *testing.T, files []parsedFile) {
			calls := 0
			for _, pf := range files {
				ast.Inspect(pf.file, func(n ast.Node) bool {
					ce, ok := n.(*ast.CallExpr)
					if !ok || !isCallTo(ce.Fun, "cookies", "NewRefreshService") {
						return true
					}
					calls++
					if len(ce.Args) < 2 {
						return true
					}
					lit, ok := ce.Args[1].(*ast.BasicLit)
					if !ok || lit.Kind != token.INT || lit.Value != "0" {
						t.Errorf("%s passes a non-zero interval to NewRefreshService -- data-and-storage.md's \"nothing in production feeds the interval parameter\" is now false", pf.rel)
					}
					return true
				})
			}
			if calls == 0 {
				t.Error("no production call to cookies.NewRefreshService exists at all -- the claim's subject is gone; re-verify the sentence")
			}
		},
	}, {
		doc: "data-and-storage.md",
		key: "nothing in production ever wired it",
		why: "the Logger type's per-job buffer API was removed in 2026-07",
		check: func(t *testing.T, files []parsedFile) {
			banned := map[string]bool{"LogForJob": true, "GetJobLogs": true, "PruneJobLogs": true}
			for _, pf := range files {
				for _, d := range pf.file.Decls {
					fd, ok := d.(*ast.FuncDecl)
					if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 {
						continue
					}
					if receiverTypeName(fd.Recv.List[0].Type) == "Logger" && banned[fd.Name.Name] {
						t.Errorf("%s declares Logger.%s -- the doc says that API was removed (the *Database methods of the same name are the live pipeline and are fine)", pf.rel, fd.Name.Name)
					}
				}
				ast.Inspect(pf.file, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok && id.Name == "LogForJob" {
						t.Errorf("%s mentions LogForJob -- the doc says the Logger per-job buffer API is gone", pf.rel)
					}
					return true
				})
			}
		},
	}, {
		doc: "data-and-storage.md",
		key: "Nothing automatic ever prunes it",
		why: "every writer of cfg.Cookies.Platforms is known, and the only one that can shrink the list is the operator's PUT /api/config",
		check: func(t *testing.T, files []parsedFile) {
			// The three known writers, each verified by reading:
			//   cmd/moombox/services.go       -- the first-run seed (only when
			//                                    the list is empty) and the
			//                                    wizard's FinishSetup merge
			//                                    (union with the verified
			//                                    platforms: adds, never removes)
			//   internal/config/config.go     -- migrateOldFormat: copies the
			//                                    legacy [auto_cookies] list only
			//                                    when [cookies] has none
			//   internal/web/routes/config_routes.go -- PUT /api/config: the
			//                                    operator REPLACING the list,
			//                                    which is the sole removal path
			//                                    the sentence names
			want := map[string]bool{
				"cmd/moombox/services.go":              true,
				"internal/config/config.go":            true,
				"internal/web/routes/config_routes.go": true,
			}
			got := map[string]bool{}
			for _, pf := range files {
				ast.Inspect(pf.file, func(n ast.Node) bool {
					as, ok := n.(*ast.AssignStmt)
					if !ok {
						return true
					}
					for _, lhs := range as.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "Platforms" {
							continue
						}
						if outer, ok := sel.X.(*ast.SelectorExpr); ok && outer.Sel.Name == "Cookies" {
							got[pf.rel] = true
						}
					}
					return true
				})
			}
			for f := range got {
				if !want[f] {
					t.Errorf("%s assigns cfg.Cookies.Platforms and is not a known writer -- read it: if it can REMOVE a platform without an operator's request, the doc's \"Nothing automatic ever prunes it\" is false; otherwise add it to the known set with its reason", f)
				}
			}
			for f := range want {
				if !got[f] {
					t.Errorf("expected writer %s no longer assigns cfg.Cookies.Platforms -- the claim's evidence moved; re-verify it", f)
				}
			}
		},
	}, {
		doc:   "data-and-storage.md",
		key:   "const livenessRecoveryArmed = false",
		why:   "the automatic-recovery pilot is disarmed, and the doc quotes the constant",
		check: pilotDisarmed,
	}, {
		doc:   "operations.md",
		key:   "const livenessRecoveryArmed = false",
		why:   "same constant, quoted by the operations doc",
		check: pilotDisarmed,
	}}

	for _, c := range claims {
		body := strings.Join(docLines(t, root, c.doc), "\n")
		if !strings.Contains(body, c.key) {
			t.Errorf("%s no longer contains %q -- the claim (%s) was reworded or removed; re-aim this check", c.doc, c.key, c.why)
			continue
		}
		c.check(t, files)
	}
}

// isCallTo matches pkg.Name(...) and, inside the declaring package, Name(...).
func isCallTo(fun ast.Expr, pkg, name string) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name == name
	case *ast.SelectorExpr:
		x, ok := f.X.(*ast.Ident)
		return ok && x.Name == pkg && f.Sel.Name == name
	}
	return false
}
```

- [ ] **Step 4: Run it and read the failures**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/docs/ -count=1 -v
```
Expected: `TestCitationAllowlistParsing`, `TestCitationShapes`, `TestSpecDocHeadingReferencesResolve` and `TestSpecDocAbsenceClaimsHold` PASS (the last one passes only because the known-writer set names `internal/web/routes/config_routes.go` — with the two-file set a first draft of this task carried, it failed on that file); `TestSpecDocCitationsResolve` FAILS with **exactly these five**:

| # | Failure | Truth |
|---|---|---|
| 1 | `architecture.md:219 cites 'internal/errors', which is not a directory` | package deleted in `2e234f9` |
| 2 | `architecture.md:918 cites 'internal/errors/errors.go', which does not exist` | same |
| 3 | `platform-services.md:789 cites 'internal/goja/dom-real.js', which does not exist` | it is `internal/goja/js/dom-real.js` |
| 4 | `platform-services.md:907 cites 'validateChallengeOrigin' … neither declares nor mentions it` | renamed into `canonicalizeChallenge`; only an orphaned doc comment kept the name alive |
| 5 | `operations.md:130 cites 'OnCancelAutoCookie' (…tui_wiring.go) … neither declares nor mentions it` | the field is `internal/tui/setup_wizard.go:262`; `tui_wiring.go` supplies an anonymous callback, `internal/tui/app.go:628` binds it |

If you see MORE than five, `main` moved under you (Arc 12c merged): treat each extra one the same way — find the truth, cite it, fix the doc. If you see FEWER, the checker is broken: something in Steps 2-3 was mistyped. Do not weaken a check to make a failure go away.

- [ ] **Step 5: Fix rot 1+2 — the deleted `internal/errors` package**

Confirm first: `git log --oneline --diff-filter=D -- internal/errors` names `2e234f9`. Then, in `docs/spec/architecture.md`, delete these three lines (the third is inside a fenced block, so the checker does not see it — it is the same untruth and goes with them):

- `:201` — `internal/errors     (1 file,  ~230)    -- Typed error hierarchy, sentinel codes`
- `:219` — ``- `internal/errors` imports: nothing from internal``
- `:918` — ``- `internal/errors/errors.go` -- Complete error type hierarchy``

Do not touch the "Total: approximately 49,400 lines…" line below the fence: volatile counts belong to `appendix-metrics.md`, and the checker deliberately does not check them.

- [ ] **Step 6: Fix rot 3 — the DOM shim's path**

In `docs/spec/platform-services.md:789`, replace `` `internal/goja/dom-real.js` `` with `` `internal/goja/js/dom-real.js` ``.

- [ ] **Step 7: Fix rot 4 — the renamed origin check**

In `docs/spec/platform-services.md:907`, replace `` `validateChallengeOrigin` in `internal/youtube/watch_page.go` `` with `` `canonicalizeChallenge` in `internal/youtube/watch_page.go` ``.

Then fix the rot's source. `internal/youtube/watch_page.go:824-830` is an orphaned stanza: it opens `canonicalizeChallenge`'s doc comment with the name of a function that no longer exists anywhere in the tree. Replace:

```go
// validateChallengeOrigin enforces that a page-sourced challenge points its
// interpreter at a Google host. A challenge carrying inline
```
with:
```go
// The origin rule canonicalizeChallenge enforces: a page-sourced challenge
// must point its interpreter at a Google host. A challenge carrying inline
```

Leave the remaining five lines of that stanza and the `canonicalizeChallenge validates …` paragraph below it exactly as they are. This is comment-only.

- [ ] **Step 8: Fix rot 5 — where the wizard's cancel callback lives**

In `docs/spec/operations.md:130`, replace:

```markdown
in-process, through `OnCancelAutoCookie` (wired at `cmd/moombox/tui_wiring.go`, no HTTP: the TUI shares this process)
```
with:
```markdown
in-process, through the wizard's `OnCancelAutoCookie` (`internal/tui/setup_wizard.go`), whose callback is supplied at `cmd/moombox/tui_wiring.go` and bound in `internal/tui/app.go` — no HTTP: the TUI shares this process
```

The same imprecision sits at `docs/spec/user-interfaces.md:688`, in a shape the checker does not pair (the gap "` (all bound in `" is not a bare connector). Fix it while you are here, so the two sentences agree: replace `(all bound in `cmd/moombox/tui_wiring.go`)` with `(all supplied at `cmd/moombox/tui_wiring.go` and bound onto the wizard in `internal/tui/app.go`)`.

- [ ] **Step 9: Green**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/docs/ -count=1 -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/docs/ ./internal/youtube/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./internal/youtube/
```
Expected: all five tests PASS, no vet output, `internal/youtube` builds. If `TestSpecDocCitationsResolve` still fails on a citation Task 3 added to `security.md`, fix that citation — it is yours.

- [ ] **Step 10: Three mutation checks**

1. **A citation pointed at a deleted symbol.** In `docs/spec/data-and-storage.md`, temporarily change `` `detectCookiePlatforms` (`cmd/moombox/services.go`) `` to `` `detectCookiePlatformsGone` (`cmd/moombox/services.go`) ``. Re-run Step 9's test command → `TestSpecDocCitationsResolve` must FAIL naming `data-and-storage.md:529` (or wherever that row now sits). Put the original text back BY HAND — never `git checkout` a file in this tree — and re-run to confirm PASS.
2. **A false absence claim.** In `citations_test.go`, temporarily insert this element into `claims`, immediately before the `data-and-storage.md` / `const livenessRecoveryArmed = false` entry:
   ```go
   {doc: "data-and-storage.md", key: "Nothing automatic ever prunes it", why: "deliberate mutant", check: func(t *testing.T, files []parsedFile) {
       for _, pf := range files {
           ast.Inspect(pf.file, func(n ast.Node) bool {
               if ce, ok := n.(*ast.CallExpr); ok && isCallTo(ce.Fun, "cookies", "NewCookieJar") {
                   t.Errorf("%s calls NewCookieJar", pf.rel)
               }
               return true
           })
       }
   }},
   ```
   Re-run → `TestSpecDocAbsenceClaimsHold` must FAIL with `cmd/moombox/services.go calls NewCookieJar`, proving the walker actually reaches production code. Delete the mutant and re-run to confirm PASS.
3. **The pilot constant read wrong.** In `pilotDisarmed`, temporarily change `id.Name != "false"` to `id.Name != "true"`. Re-run → `TestSpecDocAbsenceClaimsHold` must FAIL twice (once per quoting doc) naming `internal/cookies/refresh.go`, proving the check reads the real constant. Restore and re-run to confirm PASS. Do not touch `refresh.go` itself.

- [ ] **Step 11: Re-baseline against the merged tree — the CONTROLLER's step**

Global Constraints say the implementer never merges; this step belongs to whoever merges the branch. At the merge gate, after Arc 12c has landed on `main`: merge `main` into `cookie-housekeeping-h1` (`--no-ff`; conflicts resolved by reading, never by `checkout`; never a rebase — the branch's reviewed history stays as it is), then re-run Step 9's commands. Arc 12c edits `docs/spec/data-and-storage.md` at `:530-533` (the `[cookies]` table rows after `Platforms` — an `Acquisition` row with fresh citations) and `:870-897` (Timing / Auto-Cookie Service), and adds `Acquisition` to `internal/config/types.go` beside the comment Task 4 rewrote — non-overlapping, so the merge should be clean. New citations the checker has never seen may fail: fix each one on the branch, in a follow-up commit, with the truth cited, exactly as Steps 5-8 did. The five claim keys are sentence substrings, so the line drift the map recorded (`:865`/`:1156` on the 12c tree are `:864`/`:1151` on `main`) cannot break them; a key that 12c reworded fails loudly and is re-aimed, not deleted. Record in the merge report whether this step found anything.

- [ ] **Step 12: Line endings and commit**

```bash
perl -0777 -ne 'print tr/\r//' internal/docs/doc.go internal/docs/citations_test.go internal/docs/citation_allowlist.txt internal/youtube/watch_page.go docs/spec/architecture.md docs/spec/operations.md docs/spec/platform-services.md docs/spec/user-interfaces.md
git add internal/docs docs/spec/architecture.md docs/spec/operations.md docs/spec/platform-services.md docs/spec/user-interfaces.md internal/youtube/watch_page.go
git commit -m "$(cat <<'EOF'
test(docs): a citation-rot test guards the six spec docs

Parses the six heavily-cited deep-dive docs and resolves every citation
against the tree: 310 file paths, 60 directories, 155 symbol/path pairs, 18
§-heading cross-references, and five absence/state claims re-verified by
walking non-test ASTs (NewRefreshService's dead interval parameter, the
removed Logger per-job buffer API, the three-file writer set behind "nothing
automatic ever prunes Cookies.Platforms" -- seed, migration, and the
operator's PUT /api/config -- and the disarmed livenessRecoveryArmed
constant two docs quote verbatim). A cited symbol must appear in the file's
CODE, not merely in a comment -- the docs legitimately cite wiring and call
sites, and requiring a declaration flagged five correct citations.

First run found five rots, all fixed here with the truth cited: the deleted
internal/errors package (three lines), internal/goja/js/dom-real.js's path,
validateChallengeOrigin (renamed into canonicalizeChallenge -- an orphaned
doc comment in watch_page.go kept the name alive, now reworded), and where
the wizard's OnCancelAutoCookie callback is actually supplied and bound.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Self-Review

**1. Spec coverage.** R1 → Task 1 (call placed before `cancelCtx`, fifth test, header enumeration, mutant). R2 → Task 2 (`noteSegmentProgress` with the four parameters the real types demand, closure unchanged in behaviour, two mutants). R3 → Task 3 (six callers checked, body dropped from the error at all five arms, retry line reports `prev_status`, args-rendering logger, all five arms driven against a stubbed GQL — the retried two to exhaustion through the `gqlBaseRetryDelay` seam — `security.md` bullet with file + function, seven mutants). R4 → Task 4 Steps 1-3. R5 → Task 4 Steps 4-8. R6 → Task 5 (package placement argued, patterns defined precisely and pinned by `TestCitationShapes`, allowlist file, `go/parser` walks, five absence/state claims, heading check, predicted first-run table verified by an actual run at `383ed7d`, three mutants, controller-owned re-baseline step). Non-goals honoured: no `internal/cookies/**` file is touched, `SetExpectedPlatforms` is untouched, and the orchestrator's behaviour is byte-identical.

**Where the code contradicted the spec draft — four places:**

1. **R1's mechanism.** The draft says `MarkStreamEnded` "calls `setLiveContinuationOpen(false)` like its three sibling permanent exits". It cannot: `setLiveContinuationOpen` acquires `cd.mu`, and `MarkStreamEnded` already holds it — `sync.Mutex` is not reentrant, so that call deadlocks. `Stop()`, the closest sibling, assigns `cd.liveContinuationOpen = false` directly under the held lock; the plan follows `Stop()`. The R still binds and the observable behaviour is exactly what the draft asks for.

2. **R2's fake clock.** The draft says "test it with a fake clock". `atomicTimeValue` exposes `Store(time.Time)` and `StoreNow()` and takes no injected now; adding a `now` parameter would put an unconditional `time.Now()` into a ~60 Hz progress callback and break the "byte-identical behaviour" invariant in the same breath. The plan keeps `StoreNow()` and brackets the call (`before <= stored <= after`), which is an exact assertion, not a weaker one — and which the task prompt sanctions.

3. **R3's scope.** The draft names the two retried error sites (`:233`, `:239`). The plan changes all five `string(respData)` interpolations, because the three un-retried ones (401/403 with and without the sentinel, other 4xx) are precisely the errors that RETURN to callers who log them; leaving them would satisfy the letter and miss the hazard. No caller reads the body from the error (all six do `if err != nil { return err }`; no `strings.Contains` on a GQL error exists in `internal/twitch`), so the safer shape costs nothing. `parseGQLBody`'s 200-path errors keep interpolating `errors[0].message` — a modelled GQL field, not an intermediary's page — deliberately.

4. **R6's symbol rule.** The draft (and the task prompt) says a cited symbol must be "declared in that file". Measured against `main` with a top-level-declaration rule, seven of the first draft's 134 pairs fail: the two real rots, plus five correct citations of WIRING or CALL sites (`AutoCookieService.VerifyYouTubeAuth` at `cmd/moombox/services.go`, `runtime.ReadMemStats` at `internal/web/routes/jobs.go`, `FetchVariantsFn` at `internal/worker/worker.go`, `finishCtx` in `cmd/moombox/tui_wiring.go`, `js_to_json` at `internal/utils/jsjson.go`). The plan therefore accepts *declared or referenced in code*, and excludes comments — which still fails the two real rots (`validateChallengeOrigin` survives only in a comment; `OnCancelAutoCookie` appears in `tui_wiring.go` not at all) and still fails the deleted-symbol mutant. The allowlist file exists for the residue and is empty today.

Two smaller notes. The draft's line numbers for the map's three absence claims (`:865`, `:1156`, `:529`) are the 12c tree's; on `main` they are `:864`, `:1151`, `:529` — which is why the checker keys them by sentence substring instead of line number, and fails loudly if a key stops matching. And `.gitattributes` is CRLF in the Windows working tree (nothing covers itself) while its index blob is LF; Task 4 Step 7 checks the blob, which is what ships.

**2. Placeholder scan.** No "TBD", no "add error handling", no "similar to Task N". Every code step carries the literal text to write; every doc fix carries the exact before and after; the one place a number is predicted rather than stated (Task 5 Step 4) prints the five failures it must be, with the truth for each, and tells the implementer what to do if the count differs.

**3. Type consistency.** `noteSegmentProgress(p engine.DownloadProgress, lastBytes *atomic.Int64, lastSegTime *atomicTimeValue, consecutiveLiveChecks *atomic.Int32) bool` — the call site passes `lastSegTime` (already a `*atomicTimeValue` parameter of `runLiveStreamDownload`) and `&consecutiveLiveChecks` (a local `atomic.Int32`), matching. `gqlBodySize([]byte) string` is used at five sites and asserted by both hygiene tests with the same `"%d-byte body"` format; `gqlBaseRetryDelay` as a `time.Duration` var still feeds the `min(… << (attempt-1), …)` at `api.go:195`. `installProbeStub(t, status, body) *atomic.Int64` matches `internal/twitch/liveness_probe_test.go:31`'s signature (the returned counter is asserted exactly: 4 calls for a retried arm, 1 for an un-retried one) and its no-`t.Parallel` rule is restated. In Task 5, `fileFacts`/`parsedFile`/`receiverTypeName`/`isCallTo`/`parseAllowlist`/`symbolName`/`pilotDisarmed` are each defined once and used with the same shapes; `receiverTypeName` is shared by the symbol walk and the Logger claim; `pilotDisarmed` is shared by the two quoting docs' claims.

**4. Review round (2026-09-03, `h1-plan-review.md`).** Every task's code was executed in a throwaway worktree at `383ed7d`. Changes made to this plan by the review: Task 3's tests were restructured (the drafted ctx-cut test let its own stated mutant survive; the `gqlBaseRetryDelay` var seam was added; seven mutants, all verified killed); the `classifyProbeErr` claims were corrected (the auth prefix is NOT parsed by it, and the body-dependence ran the other way); `27-byte body` → 42; Task 5's known-writer set gained `config_routes.go` (the drafted two-file set failed on the first run), the symbol rule gained call shapes and one-word wiring connectors (134 → 155 pairs, all resolving), `TestCitationShapes` and the `livenessRecoveryArmed` state claim were added, Step 10's `git checkout` became a by-hand revert, and Step 11 became the controller's merge-gate step; Task 1's `LiveContinuationOpen` line range was corrected and the RULING NEEDED at the top (with conditional Step 3a) was added. Tasks 2 and 4 executed exactly as written.
