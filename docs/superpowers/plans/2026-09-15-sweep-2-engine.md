# Sweep Arc 2 — Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the HLS live loop from finalizing a still-live recording on an inconclusive stream-status check, and land the arc's six smaller engine fixes (resume-save cadence, body pre-sizing, head-probe fallback, 4xx drain, caller-cancel accounting, dead HealthUpdate plumbing) with the two drifted comments corrected.

**Architecture:** Every change is inside `internal/engine` except one guard in `internal/utils/http.go`. The behavioural core is Task 1: the playlist-404/410 exit stops treating a `CheckStreamStatus` ERROR as "ended" and instead defers the verdict into the loop's existing consecutive-error budget, mirroring the DASH loop's already-correct rule. The remaining tasks are local: one new `delays` field, one new `readBody` helper, one sentinel error, two drains, one guard helper, one deletion.

**Tech Stack:** Go 1.27, stdlib `net/http` + `net/http/httptest`, the package's existing `delays`/`fastDelays()` test-scaling seam, `warnCollector` (`internal/engine/downloader_dash_integration_test.go`) for log assertions, `testing.AllocsPerRun` for allocation pins, `httptest.Server.Config.ConnState` for TCP-connection counting.

**Spec:** `docs/superpowers/specs/2026-09-15-sweep-fix-chain-design.md` — **§5 (Arc 2 — Engine, branch `sweep-2-engine`)** is this plan's section; §1–§3 and §11–§12 bind it. Ledger evidence: `reports/sweep-2026-09-15.md` items T1-2, T2-14, T2-17, T3-29, T4-31 (HealthUpdate), T4-34 (retry-comment drift), T4-35 (4xx drain, caller-cancel guard). Read both before starting.

## Global Constraints

Copied verbatim from spec §3 (every task inherits these):

- `go 1.27`, no CGo, pure-Go dependencies; Windows x64 + Linux x64 + Linux arm64 must build.
- LF line endings in every file the chain touches. Commits carry the two trailers
  `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and
  `Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq`.
- The anonymous logger interface (`Debug/Info/Warn/Error(msg string, args ...any)`) stays anonymous
  per struct. Every goroutine has an inline `defer func() { if r := recover(); … }()`.
- `internal/tui` never imports `internal/web/routes` or `internal/bgutils` (compile-time embed check).
- Helpers are defined in the task that first uses them (staticcheck U1000 is a hard gate).
- TDD per task: the failing test is written and run red before the change; every new assertion names
  the mutant that fails it (the reviewer verifies at least one).
- Every JS-touching task gates `go test ./internal/web/routes/` (its tests lift app.js/player.js
  bodies into goja) AND `node --test web/tests/*.test.mjs`. Every task that renames, moves or deletes a
  Go symbol, or edits `docs/spec/*.md`/`SPEC.md`, gates `go test ./internal/docs/` (the citation test
  requires the DECLARING file).
- Behaviour that the owner's rulings protect stays: ~60 Hz progress pipeline and DB write cadence
  (make updates cheaper, never rarer); DB layer untouched for perf; `monitors.probe_cooldown` default 0;
  the BotGuard interpreter gate; `/retry` vs `/resume` gates never shared; the Web cookie import stays
  unbounded (a test forbids WithTimeout).
- Reviewers never edit files (they reproduce in `git archive` scratchpad exports); implementers use git
  only for `add`/`commit` on the arc branch; no stashing, no rebasing, no checkout of other branches.
- User-facing strings: match the twin UI's exact output before mirroring it (read the twin's code).

**This arc's gate list (spec §5, "Gates"), run on the merge candidate:**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/utils/...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/worker/...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/
```

Plus the chain-wide merge gates from spec §2: `gofmt -l ./cmd ./internal ./tools ./web` empty,
`go vet ./...`, `staticcheck ./...` (pinned 2026.2.1) clean, `go build ./...`,
`GOOS=linux GOARCH=amd64 go build ./...`, `GOOS=linux GOARCH=arm64 go build ./...`, and ONE
controller-run `go test -count=1 ./...`. No JS changes in this arc, so the node suite is not required.
No live gates: this arc touches no extraction client, no goja, no sidecar payload, no cipher.

**Workspace (spec §2 worktree recipe):**

```bash
git worktree add -b sweep-2-engine .worktrees/sweep-2-engine main
# then copy the gitignored build inputs a fresh worktree lacks:
#   internal/bgutils/embed/{node-windows-amd64.gz,node-linux-amd64.gz,node-linux-arm64.gz,sidecar.tar.gz}
#   internal/cipher/testdata/*.js
# (web/tests npm ci is not needed — this arc changes no JS)
```

Every `go` command in this plan is prefixed with `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`.

---

## Anchor corrections (verified in source at `main` = `5bbf16e8`)

The spec's line numbers came from the ledger; two were wrong and one was misleading. **Use these:**

| Spec §5 says | Reality at `5bbf16e8` |
|---|---|
| HLS sites `downloader_hls.go:273-282`, `:296-304`, `:344-352` | Correct. Site A is the whole `plStatus == 404` / `410` block at `:262-283` (its status check at `:273-280`); site B `:296-304`; site C `:344-352`. |
| Mirror the DASH comment at `downloader_dash.go:447-462` | The rule is written at `downloader_dash.go:441-462`; the exact log line is `:455`. |
| Resume cadence at `downloader_hls.go:700-703` | Correct — but that site is **already** guarded by `curSeqNow != lastSavedSeq`. A flowing live stream advances the seq every reload, so it still writes every ~2 s. The fix is the time floor, not a progress check. |
| `readBody` call sites `downloader_fetch.go:180`, `:582` | Correct. |
| Head probe `downloader_fetch.go:409-419` | Correct: `probeHeadSequence` `:409-419`, `probeHeadAt` `:421-460`. |
| 4xx drain reusing "the constant the 206 path already uses at `:584-590`" | `maxDrainBytes` is declared at `downloader_fetch.go:78` and used at `:590`. Correct. |
| Caller-cancel guard at **`internal/utils/http.go:235-238`** | **WRONG — that file is 118 lines long.** The site is `FetchWithTimeout`'s `reportConnResult(true)` at `internal/utils/http.go:94`. |
| Caller-cancel guard at `downloader_fetch.go:153-156` | Correct (`fetchSegment`'s `reportFailure("engine/fetch")` at `:155`). Three identical sites exist at `:437`, `:483`, `:561` — see Task 6. |
| Dead code at `downloader.go:274-297, :450-456, :527-557, :903`; `downloader_dash.go:272` | Correct: type `:274-296`, fields `:450-456`, `emitHealthUpdate` `:527-557`, `startedAt.StoreNow()` `:901-903`, sole call site `downloader_dash.go:272`. |
| Doc drift: "**`downloader.go:597-607`** describes the real linear `5 s × n` retry" | **WRONG — `downloader.go` contains no retry prose at all.** `:585-625` is `noteFetch` / `getBaseURL` / `refreshCredentials`. The drifted comment is `internal/engine/downloader_fetch.go:188` ("…retries and **exponential** backoff") against the linear `time.Duration(5*(attempt+1))*time.Second` at `:298`. `fetchChunkWithRetry` (`:513`, `:529`) genuinely IS exponential — do not touch it. |

**One deliberate extension of the spec, flagged for the reviewer.** Spec §5 item 6 names two
caller-cancel sites. The engine has **four** structurally identical ones (`:155`, `:437`, `:483`,
`:561`), and every one of them shadows `ctx` with a derived `context.WithTimeout` BEFORE the point the
guard goes — so the naive `ctx.Err() != nil` guard the spec's wording suggests would also swallow
genuine per-request timeouts, which ARE connectivity evidence. Task 6 therefore introduces one helper
that takes the PARENT context and applies it at all four sites.

**One spec claim corrected, in Task 1.** Spec §5 item 1 says the exhausted-budget exit sends the job to
`Error`, "which `isRecoverableTwitchError` can recover". **Neither HLS consumer does that.** The real
exits are stated per-test in Task 1 with worker citations. The fix is still correct and still valuable
(the resume sidecar survives and the loop re-asks instead of latching) — but no test may assert an
`Error` status, because none is produced.

---

## File Structure

| File | Task | Responsibility |
|---|---|---|
| `internal/engine/downloader_hls.go` | 1, 2 | Live HLS loop: end verdict, resume-save cadence, `hlsResumeSaveInterval` constant |
| `internal/engine/downloader_hls_endverdict_test.go` (new) | 1 | The three verdict exits |
| `internal/engine/downloader_hls_resumecadence_test.go` (new) | 2 | Sidecar write count under the floor |
| `internal/engine/delays.go` | 2 | One new field + default |
| `internal/engine/delays_test.go` | 2 | `want` literal, value pin, `fastDelays()` |
| `internal/engine/downloader_resume.go` | 2 | Fire the `onResumeSaved` seam after a successful rename |
| `internal/engine/downloader.go` | 2, 7 | The `onResumeSaved` seam field; the HealthUpdate deletion |
| `internal/engine/downloader_fetch.go` | 3, 4, 5, 6, 8 | `readBody`, head-probe sentinel, 4xx drain, cancel guard, retry comment |
| `internal/engine/downloader_fetch_readbody_test.go` (new) | 3 | `readBody` shapes + alloc pin + conn-reuse pin + `newConnCountingServer` helper |
| `internal/engine/downloader_fetch_headprobe_test.go` (new) | 4 | Fallback fires only on an answered-but-unusable probe |
| `internal/engine/downloader_fetch_drain_test.go` (new) | 5 | 4xx keeps the socket |
| `internal/engine/downloader_fetch_cancel_test.go` (new) | 6 | Caller cancel is not a connectivity failure |
| `internal/engine/downloader_dash.go` | 7 | Drop the sole `emitHealthUpdate` call |
| `internal/utils/http.go` | 6 | Parent-context guard in `FetchWithTimeout` |
| `internal/utils/http_test.go` | 6 | Cancel-vs-timeout reporter assertions |
| `docs/spec/architecture.md` | 1 | One bullet under "HLS live mode (`runHlsLoop`)" |
| `docs/superpowers/plans/2026-09-15-sweep-2-engine.md` | 8 | Deleted in the arc's last commit (spec §2) |

---

## Task 1: HLS end verdict — a status-check ERROR never finalizes (T1-2)

**Files:**
- Modify: `internal/engine/downloader_hls.go:262-283` (site A, the 404/410 exit), `:288-305` (site B, >5 consecutive fetch errors), `:336-353` (site C, >5 consecutive parse failures)
- Modify: `docs/spec/architecture.md:407-410` (the "HLS live mode (`runHlsLoop`)" bullet list)
- Test: `internal/engine/downloader_hls_endverdict_test.go` (new)

**Interfaces:**
- Consumes (existing, unchanged): `DownloaderOptions.CheckStreamStatus func(ctx context.Context) (bool, error)` — returns `(ended, err)`; `ErrQualityLost` (`internal/engine/downloader.go:19`); `d.streamEnded atomic.Bool`; `d.delays.hlsPlaylistRetry`; the test helpers `fastDelays()` and `warnCollector` (both already in the package's test files).
- Produces: no new exported or unexported symbol. Task 2 and later tasks depend on nothing from here.

### What the three sites are, and what each exit means to the worker

Read this before writing code — the test assertions below are written against it.

`runHlsLoop` consults `CheckStreamStatus` at three playlist-failure sites. All three today do:

```go
if d.opts.CheckStreamStatus != nil {
    ended, checkErr := d.opts.CheckStreamStatus(ctx)
    if checkErr != nil {
        d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
    } else if !ended {
        return ErrQualityLost
    }
}
```

- **Site A (`:262-283`), playlist 404/410.** After the block it runs `d.streamEnded.Store(true); return nil`. So a check ERROR here **latches a clean end**: the deferred `ClearResume` (`:179-184`) deletes the resume sidecar and the orchestrator finalizes a truncated recording as a complete one. This is the bug.
- **Site B (`:288-305`), `consecutiveErrors > 5` on fetch.** After the block it returns
  `fmt.Errorf("HLS playlist fetch failed after %d consecutive errors: %w", …)`. A check error here
  already exits with an error — only the log wording is wrong ("assuming ended" describes a finalize
  this path never performs).
- **Site C (`:336-353`), `consecutiveErrors > 5` on parse.** Same as B, returning
  `fmt.Errorf("failed to parse HLS playlist after %d consecutive errors", …)`.

**What each exit produces downstream** (verified; cite these in review, not the spec's claim):

| Loop exit | Twitch live (`internal/worker/orchestrator_twitch.go`) | YouTube HLS (`internal/worker/orchestrator_youtube.go`) |
|---|---|---|
| `nil` + `streamEnded=true` | `dlErr == nil`, falls to the normal-stop `break` at `:654-658`; sidecar already cleared; job → `Muxing` (`:801-803`) → `Finished`. | `downloadErr == nil` → the "Normal download stop — verify stream ended" block at `:492`; `GetVideoInfo` decides. |
| `ErrQualityLost` | `isQualityLost` at `:511` → the refresh branch at `:524` re-fetches the master playlist and continues the SAME job. | `isQualityLost` at `:247` → `refreshDownload` and continue. |
| any other error (the new budget-exhausted exit) | logged as "Twitch HLS download error" at `:656`, then `break` → finalize captured parts. `streamEnded` stays false, so **the resume sidecar survives** and a later Resume appends the tail. | falls into the same `:492` verify block: `GetVideoInfo` says live → refresh and keep recording (up to `maxConsecutiveLiveChecks`); says ended → finalize. |

So the value of this fix is not a status change — **it is that a transient GQL/probe flap now costs a
retry instead of a truncated archive**, and that the sidecar survives when the verdict never arrives.
No test may assert `database.StatusError`.

- [ ] **Step 1: Write the failing test file**

Create `internal/engine/downloader_hls_endverdict_test.go`:

```go
package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hls404Server serves 404 for every playlist request and counts them. The
// live loop's 404/410 branch is the only exit under test, so no segment
// route is needed.
func hls404Server(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &fetches
}

// newEndVerdictDownloader wires a 404 playlist to a scripted status check.
// check is called with the 1-based call number so a test can script "error
// first, answer second".
func newEndVerdictDownloader(t *testing.T, url string, warns *warnCollector, check func(call int) (bool, error)) (*SegmentDownloader, *atomic.Int32) {
	t.Helper()
	var checks atomic.Int32
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    url,
		OutputFile: filepath.Join(t.TempDir(), "video.ts"),
		StartSeq:   -1,
		IsHls:      true,
		Logger:     warns,
		CheckStreamStatus: func(context.Context) (bool, error) {
			return check(int(checks.Add(1)))
		},
	})
	d.delays = fastDelays()
	return d, &checks
}

// TestHlsEndVerdict_CheckErrorDoesNotFinalize is the T1-2 regression guard.
// A playlist 404 whose status check ERRORS must not be read as "the stream
// ended": the loop must keep asking and finally exit with the consecutive-
// error failure, leaving streamEnded false so the resume sidecar survives
// (runHlsLoop's defer clears it only when streamEnded is set).
//
// Mutant this kills: restoring `d.streamEnded.Store(true); return nil` under
// a non-nil checkErr — Start() then returns nil with streamEnded true, and
// both assertions below fail. A weaker test that only checked "err != nil"
// would also pass on a loop that returned ErrQualityLost, which is a
// DIFFERENT (and wrong) verdict for an unknown status, so the ErrQualityLost
// assertion is here too.
func TestHlsEndVerdict_CheckErrorDoesNotFinalize(t *testing.T) {
	srv, fetches := hls404Server(t)
	warns := &warnCollector{}
	d, checks := newEndVerdictDownloader(t, srv.URL+"/playlist.m3u8", warns,
		func(int) (bool, error) { return false, errors.New("gql flap") })

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err := d.Start(ctx)

	if err == nil {
		t.Fatal("Start() = nil — a failed status check finalized the recording as a clean end (T1-2)")
	}
	if errors.Is(err, ErrQualityLost) {
		t.Fatalf("Start() = ErrQualityLost — an UNKNOWN status must not claim the stream is still live; got %v", err)
	}
	if !strings.Contains(err.Error(), "consecutive errors") {
		t.Errorf("Start() = %v, want the consecutive-error exit", err)
	}
	if d.streamEnded.Load() {
		t.Error("streamEnded set on an unknown verdict — the deferred ClearResume would delete the sidecar the tail needs")
	}
	if got := checks.Load(); got < 2 {
		t.Errorf("CheckStreamStatus called %d times, want >= 2 — the loop must RE-ASK after a failed check, not latch the first answer", got)
	}
	if got := fetches.Load(); got < 2 {
		t.Errorf("playlist fetched %d times, want >= 2 — the 404 must fall into the consecutive-error retry budget", got)
	}
	if j := warns.joined(); !strings.Contains(j, "deferring end verdict") || strings.Contains(j, "assuming ended") {
		t.Errorf("warn wording = %q, want the DASH loop's \"stream status check failed; deferring end verdict\" and no \"assuming ended\"", j)
	}
}

// TestHlsEndVerdict_StillLiveAfterFailedCheckRefreshes pins the recovery the
// fix exists for: the first check fails, the loop retries, the second check
// answers "not ended", and the loop hands the orchestrator ErrQualityLost so
// the variant is refreshed and the SAME job keeps recording
// (orchestrator_twitch.go:511-524, orchestrator_youtube.go:247).
//
// Mutant this kills: a fix that defers the verdict but never re-consults —
// e.g. falling through to `return fmt.Errorf(...)` immediately instead of
// into the retry budget. Start() then never returns ErrQualityLost.
func TestHlsEndVerdict_StillLiveAfterFailedCheckRefreshes(t *testing.T) {
	srv, _ := hls404Server(t)
	warns := &warnCollector{}
	d, _ := newEndVerdictDownloader(t, srv.URL+"/playlist.m3u8", warns, func(call int) (bool, error) {
		if call == 1 {
			return false, errors.New("gql flap")
		}
		return false, nil // still live
	})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err := d.Start(ctx)

	if !errors.Is(err, ErrQualityLost) {
		t.Fatalf("Start() = %v, want ErrQualityLost once the status check answers \"still live\"", err)
	}
	if d.streamEnded.Load() {
		t.Error("streamEnded set on a still-live verdict")
	}
}

// TestHlsEndVerdict_ConfirmedEndStillFinalizes pins the untouched half: a
// CONFIRMED "ended" must still finalize immediately and cleanly, on the very
// first check — no extra retry round, no error.
//
// Mutant this kills: a fix that routes every 404 through the retry budget
// regardless of the verdict (checks would be > 1 and Start() would return an
// error instead of nil).
func TestHlsEndVerdict_ConfirmedEndStillFinalizes(t *testing.T) {
	srv, fetches := hls404Server(t)
	warns := &warnCollector{}
	d, checks := newEndVerdictDownloader(t, srv.URL+"/playlist.m3u8", warns,
		func(int) (bool, error) { return true, nil })

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil (a confirmed end finalizes)", err)
	}
	if !d.streamEnded.Load() {
		t.Error("streamEnded not set after a confirmed end — the sidecar would be left behind")
	}
	if got := checks.Load(); got != 1 {
		t.Errorf("CheckStreamStatus called %d times, want exactly 1 — a confirmed end must not pay a retry round", got)
	}
	if got := fetches.Load(); got != 1 {
		t.Errorf("playlist fetched %d times, want exactly 1", got)
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestHlsEndVerdict' ./internal/engine/
```

Expected RED:
- `TestHlsEndVerdict_CheckErrorDoesNotFinalize` — `Start() = nil — a failed status check finalized the recording as a clean end (T1-2)`.
- `TestHlsEndVerdict_StillLiveAfterFailedCheckRefreshes` — `Start() = <nil>, want ErrQualityLost …`.
- `TestHlsEndVerdict_ConfirmedEndStillFinalizes` — PASSES already (it pins existing behaviour).

- [ ] **Step 3: Fix site A (the 404/410 exit) in `internal/engine/downloader_hls.go`**

Replace the status-check block and the unconditional finalize at `:273-282`:

```go
				if d.opts.CheckStreamStatus != nil {
					ended, checkErr := d.opts.CheckStreamStatus(ctx)
					if checkErr != nil {
						d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
					} else if !ended {
						return ErrQualityLost
					}
				}
				d.streamEnded.Store(true)
				return nil
			}
```

with:

```go
				// The end verdict belongs to the status check. A check ERROR
				// is not a verdict — a Twitch GQL flap and a failed YouTube
				// probe are both routine — so it DEFERS: the 404 falls into
				// the ordinary consecutive-error accounting below and the
				// next reload re-asks. Mirrors the DASH loop's gone-burst
				// verification (downloader_dash.go): only a CONFIRMED
				// "ended" latches streamEnded, and a confirmed "still live"
				// returns ErrQualityLost for the orchestrator's variant
				// refresh. Latching on a failed check is what turned a live
				// recording into a truncated Finished job (sweep T1-2).
				verdictKnown := true
				if d.opts.CheckStreamStatus != nil {
					ended, checkErr := d.opts.CheckStreamStatus(ctx)
					switch {
					case checkErr != nil:
						d.logger.Warn("stream status check failed; deferring end verdict", "err", checkErr)
						verdictKnown = false
					case !ended:
						return ErrQualityLost
					}
				}
				if verdictKnown {
					d.streamEnded.Store(true)
					return nil
				}
				// Verdict unknown: fall through to the shared retry budget.
				// If it runs out with the verdict still unknown, the loop
				// exits with its "N consecutive errors" error — the job
				// finalizes whatever was captured with streamEnded FALSE, so
				// the resume sidecar survives for a later Resume.
			}
```

The `}` shown is the existing close of the `if plStatus == 404 || plStatus == 410 {` block; the very
next statement is the already-present `consecutiveErrors++`, which is what the fall-through reaches.

- [ ] **Step 4: Fix the wording at sites B and C**

At `:296-304` (inside `if consecutiveErrors > 5 {` after the offline branch) and at `:344-352`
(the parse-failure twin), replace each

```go
				if d.opts.CheckStreamStatus != nil {
					ended, checkErr := d.opts.CheckStreamStatus(ctx)
					if checkErr != nil {
						d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
					} else if !ended {
						return ErrQualityLost
					}
				}
```

with

```go
				if d.opts.CheckStreamStatus != nil {
					ended, checkErr := d.opts.CheckStreamStatus(ctx)
					switch {
					case checkErr != nil:
						// Not a verdict (see the 404 site). This exit returns
						// an error either way, so there is nothing to defer
						// TO — but "assuming ended" described a finalize this
						// path never performs, and the operator reading the
						// log needs to know the status is UNKNOWN.
						d.logger.Warn("stream status check failed; deferring end verdict", "err", checkErr)
					case !ended:
						return ErrQualityLost
					}
				}
```

Behaviour at B and C is unchanged; only the message is. Verify with
`grep -n "assuming ended" internal/engine/downloader_hls.go` returning nothing.

- [ ] **Step 5: Run the tests and watch them pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestHlsEndVerdict' ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/ ./internal/worker/
```

Expected: PASS for all three new tests and no regression in the existing HLS suites
(`downloader_hls_maxtimeout_test.go` in particular drives the 404 branch under an offline
`IsOnline`, which this change does not touch).

- [ ] **Step 6: Document the rule in `docs/spec/architecture.md`**

Under `**HLS live mode (`runHlsLoop`):**` (the bullet list at `:407-410`), append one bullet after
the existing "Follows the live edge…" bullet:

```markdown
- End verdict: a playlist 404/410 finalizes ONLY on a confirmed `ended` from `CheckStreamStatus`; a confirmed "still live" returns `ErrQualityLost` for the orchestrator's variant refresh, and a check ERROR defers — the 404 rejoins the consecutive-error retry budget and the next reload re-asks. When the budget runs out with the verdict still unknown the loop exits with its consecutive-error failure and leaves `streamEnded` unset, so the resume sidecar survives for a later Resume. Same rule as the DASH gone-burst verification above (`internal/engine/downloader_hls.go`)
```

Citation-test note (`internal/docs/citations_test.go`): the only symbol immediately followed by a
path citation in that sentence is the closing `internal/engine/downloader_hls.go`, and the token
before it is the word `above` — not identifier-shaped — so no symbol/path pair is formed and no
declaring-file check applies. `runHlsLoop` in the heading is already paired with nothing. Do not
rewrite the bullet so that a backticked symbol sits immediately before the path unless that file
declares it (`ErrQualityLost` is declared in `downloader.go`, not `downloader_hls.go`).

- [ ] **Step 7: Gate the doc change**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp gofmt -l ./internal
```

Expected: PASS, and `gofmt -l` prints nothing.

- [ ] **Step 8: Commit**

```bash
git add internal/engine/downloader_hls.go internal/engine/downloader_hls_endverdict_test.go docs/spec/architecture.md
git commit -m "fix(engine): an HLS status-check error never ends the recording

The live HLS loop treated a CheckStreamStatus ERROR on a playlist 404/410 as
\"assuming ended\": it latched streamEnded, returned nil, and the deferred
ClearResume deleted the sidecar — a transient Twitch GQL flap finalized a
still-live broadcast as a complete archive. The DASH loop has deferred that
verdict since the behind-head guard landed; the HLS loop now does the same.

A check error defers: the 404 rejoins the consecutive-error retry budget and
the next reload re-asks. Only a confirmed \"ended\" latches; a confirmed
\"still live\" still returns ErrQualityLost for the orchestrator's variant
refresh. When the budget runs out with the verdict unknown the loop exits with
its consecutive-error error and leaves streamEnded false, so the captured
parts finalize with the resume sidecar intact. The two escalation sites that
already exited with an error keep their behaviour and lose the misleading
wording.

Sweep 2026-09-15 T1-2.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

## Task 2: HLS resume-save cadence — one sidecar write per 15 s (T2-14)

**Files:**
- Modify: `internal/engine/downloader_hls.go:127-137` (const block — add `hlsResumeSaveInterval`), `:196-201` (loop locals), `:691-703` (the per-iteration save)
- Modify: `internal/engine/delays.go:14-39` (new field + default)
- Modify: `internal/engine/delays_test.go:15-40` (the `want` literal + the literal-value map), `:61-71` (`fastDelays`)
- Modify: `internal/engine/downloader.go:443-448` (the `onResumeSaved` test seam field)
- Modify: `internal/engine/downloader_resume.go:190-193` (fire the seam after a successful rename)
- Test: `internal/engine/downloader_hls_resumecadence_test.go` (new)

**Interfaces:**
- Produces: `delays.hlsResumeSave time.Duration` (default `hlsResumeSaveInterval = 15 * time.Second`);
  `(*SegmentDownloader).onResumeSaved func(lastSeq int)` — an unexported TEST SEAM, nil in production,
  called with `ResumeState.LastSeq` after `saveResume` renames the sidecar into place.
- Consumes: `fastDelays()` (Task-independent; already in `delays_test.go`).

**Why a seam and not a file check:** a clean HLS end (`EXT-X-ENDLIST`) sets `streamEnded`, and
`runHlsLoop`'s defer then calls `ClearResume()` — the sidecar is DELETED before the test can read it.
The seam is the only way to count writes and see the final `LastSeq`. It follows the package's
existing test-knob precedent (`delays`, `catchUpBufferBytesOverride`: "production code never sets
this").

- [ ] **Step 1: Write the failing test**

Create `internal/engine/downloader_hls_resumecadence_test.go`:

```go
package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hlsAdvancingServer serves a one-segment live window whose media sequence
// advances by exactly 1 per reload (so currentSeq advances every iteration
// and never gaps), then an EXT-X-ENDLIST playlist after liveReloads reloads
// so the loop exits on its own.
func hlsAdvancingServer(t *testing.T, liveReloads int) *httptest.Server {
	t.Helper()
	var reloads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		n := int(reloads.Add(1))
		seq := 100 + n - 1
		end := ""
		if n > liveReloads {
			end = "#EXT-X-ENDLIST\n"
		}
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n"+
			"#EXT-X-MEDIA-SEQUENCE:%d\n#EXTINF:1.0,\nseg%d.ts\n%s", seq, seq, end)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "[%s]", strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".ts"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestHlsResumeSaveIsRateLimited pins T2-14: the live loop fsync+renamed the
// resume sidecar on EVERY playlist reload (~2 s on Twitch) because the seq
// advances every reload. With the floor, a burst of reloads inside one
// interval costs ONE write, and the loop's deferred final save still records
// the last sequence.
//
// Mutant this kills: dropping the time floor (or comparing against a clock
// that is reset on every iteration) — writes then equal the reload count,
// 12+ instead of 2.
func TestHlsResumeSaveIsRateLimited(t *testing.T) {
	const liveReloads = 12
	srv := hlsAdvancingServer(t, liveReloads)

	var mu sync.Mutex
	var saved []int
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: filepath.Join(t.TempDir(), "video.ts"),
		StartSeq:   -1,
		IsHls:      true,
	})
	d.delays = fastDelays()
	// A floor far longer than the whole test: every reload after the first
	// falls inside one interval, so only the first save and the deferred
	// final save may write.
	d.delays.hlsResumeSave = 10 * time.Second
	d.onResumeSaved = func(lastSeq int) {
		mu.Lock()
		saved = append(saved, lastSeq)
		mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil (EXT-X-ENDLIST finish)", err)
	}

	mu.Lock()
	got := append([]int(nil), saved...)
	mu.Unlock()

	if len(got) != 2 {
		t.Fatalf("sidecar written %d times (LastSeq %v) across %d reloads, want 2 — one at loop entry and one from the deferred final save", len(got), got, liveReloads)
	}
	if want := 100 + liveReloads; got[len(got)-1] != want {
		t.Errorf("final sidecar LastSeq = %d, want %d — the deferred save must still record the true final position", got[len(got)-1], want)
	}
}

// TestDefaultDelaysIncludesResumeSave is the production-timing pin for the
// new field, in the same spirit as TestDefaultDelaysMatchConstants: a floor
// silently defaulting to 0 would restore the every-reload fsync with every
// other test still green.
func TestDefaultDelaysIncludesResumeSave(t *testing.T) {
	if got, want := defaultDelays().hlsResumeSave, 15*time.Second; got != want {
		t.Errorf("defaultDelays().hlsResumeSave = %v, want %v", got, want)
	}
	d := NewSegmentDownloader(DownloaderOptions{OutputFile: filepath.Join(t.TempDir(), "o.ts")})
	if d.delays.hlsResumeSave != hlsResumeSaveInterval {
		t.Errorf("NewSegmentDownloader installed hlsResumeSave = %v, want %v", d.delays.hlsResumeSave, hlsResumeSaveInterval)
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestHlsResumeSaveIsRateLimited|TestDefaultDelaysIncludesResumeSave' ./internal/engine/
```

Expected RED: compile failure — `d.delays.hlsResumeSave undefined`, `d.onResumeSaved undefined`,
`hlsResumeSaveInterval undefined`.

- [ ] **Step 3: Add the constant, the delays field and its default**

In `internal/engine/downloader_hls.go`, extend the const block at `:127-137`:

```go
	// hlsResumeSaveInterval floors how often the live loop persists the
	// resume sidecar. The loop reaches the save site about once per reload
	// (~2 s on Twitch) and the position advances every time, so the
	// pre-floor code fsync+renamed the sidecar every couple of seconds for
	// the whole broadcast. The cost of the floor is bounded and small: an
	// unclean kill loses at most this much POSITION, and resume truncates
	// the file back to the sidecar's byte count (downloader.go's
	// truncate-for-resume) and re-fetches those segments — still far
	// tighter than the DASH loop's every-50-segments cadence
	// (ResumeSeqInterval, ~100 s of footage).
	hlsResumeSaveInterval = 15 * time.Second
```

In `internal/engine/delays.go`, add the field to the struct (keep the comment column style):

```go
	hlsResumeSave          time.Duration // hlsResumeSaveInterval — live-loop resume sidecar floor
```

and to `defaultDelays()`:

```go
		hlsResumeSave:          hlsResumeSaveInterval,
```

- [ ] **Step 4: Update the delays pins**

In `internal/engine/delays_test.go`, add to the `want` literal in `TestDefaultDelaysMatchConstants`:

```go
		hlsResumeSave:          hlsResumeSaveInterval,
```

to its literal-values map:

```go
		"hlsResumeSave":          {want.hlsResumeSave, 15 * time.Second},
```

and to `fastDelays()`:

```go
		hlsResumeSave:          d.hlsResumeSave / fastScale,
```

(The reflect-based zero check at the end of `TestDefaultDelaysMatchConstants` is what fails if you
forget the default; the `want` literal is what fails if you forget the test update.)

- [ ] **Step 5: Add the `onResumeSaved` test seam**

In `internal/engine/downloader.go`, immediately after the `catchUpBufferBytesOverride` field
(`:443-448`), add:

```go
	// onResumeSaved is a TEST SEAM, like delays and
	// catchUpBufferBytesOverride: production code never sets it. When
	// non-nil, saveResume calls it with the LastSeq it just persisted, after
	// the sidecar rename succeeded. It is the only way a test can count
	// sidecar WRITES — a clean end sets streamEnded and the loop's defer
	// then ClearResume()s the file out from under any on-disk assertion.
	onResumeSaved func(lastSeq int)
```

In `internal/engine/downloader_resume.go`, at the tail of `saveResume` (`:190-193`), turn

```go
	if err := os.Rename(tmpFile, d.opts.ResumeFile); err != nil {
		d.logger.Warn("[Downloader] Failed to rename resume file", "from", tmpFile, "to", d.opts.ResumeFile, "error", err)
		os.Remove(tmpFile)
	}
}
```

into

```go
	if err := os.Rename(tmpFile, d.opts.ResumeFile); err != nil {
		d.logger.Warn("[Downloader] Failed to rename resume file", "from", tmpFile, "to", d.opts.ResumeFile, "error", err)
		os.Remove(tmpFile)
		return
	}
	if d.onResumeSaved != nil {
		d.onResumeSaved(state.LastSeq)
	}
}
```

- [ ] **Step 6: Apply the floor in the live loop**

In `internal/engine/downloader_hls.go`, beside `lastSavedSeq` (`:198-201`) add:

```go
	// lastResumeSave floors the per-iteration save at hlsResumeSaveInterval
	// (see the constant). Zero means "never saved", which always passes the
	// floor, so the first advance still writes immediately.
	var lastResumeSave time.Time
```

and replace the save site at `:691-703`:

```go
		if curSeqNow := int(d.currentSeq.Load()); curSeqNow != lastSavedSeq {
			d.saveResume()
			lastSavedSeq = curSeqNow
		}
```

with:

```go
		curSeqNow := int(d.currentSeq.Load())
		if curSeqNow != lastSavedSeq &&
			(lastResumeSave.IsZero() || time.Since(lastResumeSave) >= d.delays.hlsResumeSave) {
			d.saveResume()
			lastSavedSeq = curSeqNow
			lastResumeSave = time.Now()
		}
```

Extend the existing comment block above it (`:691-699`) with one sentence: "…and at most once per
`hlsResumeSaveInterval`: the position advances on EVERY reload while segments flow, so the
progress check alone still wrote the sidecar every ~2 s. The deferred `saveResume` at loop exit is
unconditional and still guarantees the final flush."

- [ ] **Step 7: Run the tests and watch them pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestHlsResumeSave|TestDefaultDelays|TestFastDelays' ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/
```

Expected: PASS, including the pre-existing `TestDefaultDelaysMatchConstants` and
`TestFastDelaysKeepRatios`, and the resume suites in `downloader_resume_test.go` /
`downloader_hls_fmp4_test.go` (which drive `saveResume` directly and are unaffected by a nil seam).

- [ ] **Step 8: Commit**

```bash
git add internal/engine/downloader_hls.go internal/engine/delays.go internal/engine/delays_test.go internal/engine/downloader.go internal/engine/downloader_resume.go internal/engine/downloader_hls_resumecadence_test.go
git commit -m "perf(engine): floor the HLS resume-sidecar write at 15s

The live HLS loop saved resume state whenever the position advanced — which
on a flowing stream is every playlist reload, so a Twitch recording paid a
write+fsync+rename every ~2 s for hours. The save now also waits out
hlsResumeSaveInterval (15 s, a delays field so tests scale it); the deferred
final save at loop exit stays unconditional.

Worst case on an unclean kill is 15 s of position, which resume re-fetches
after truncating back to the sidecar's byte count — still far tighter than
the DASH loop's every-50-segments cadence.

saveResume gained an unexported onResumeSaved test seam (nil in production):
a clean end clears the sidecar, so counting writes on disk is impossible.

Sweep 2026-09-15 T2-14.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

## Task 3: Pre-size segment and chunk bodies from Content-Length (T2-17)

**Files:**
- Modify: `internal/engine/downloader_fetch.go` — add `readBody` just above `fetchSegment` (`:139`), use it at `:180` (segments/playlists) and `:582` (VOD 206 chunks)
- Test: `internal/engine/downloader_fetch_readbody_test.go` (new)

**Interfaces:**
- Produces: `func readBody(resp *http.Response, capBytes int64) ([]byte, error)` — package-private,
  reads `resp.Body` returning at most `capBytes` bytes, pre-allocating when
  `0 < resp.ContentLength < capBytes`. Also produces the test helper
  `newConnCountingServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32)`, reused
  by Task 5.
- Consumes: the existing `maxSegmentBodyBytes` (`:63`) and `maxDrainBytes` (`:78`) constants.

**The one subtlety that makes this not a one-liner:** `net/http` returns a connection to the idle
pool only once the response body has been read to its OWN `io.EOF`. A sized read that stops exactly
at `Content-Length` never triggers that final `Read`, so the obvious
`io.ReadFull(r, make([]byte, n))` implementation silently kills keep-alive reuse for every segment —
the same trap the 206 path's drain already documents at `:583-589`. The implementation below
allocates `n+1` capacity so the EOF-observing read lands without regrowing.

- [ ] **Step 1: Write the failing test**

Create `internal/engine/downloader_fetch_readbody_test.go`:

```go
package engine

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// newConnCountingServer starts an HTTP/1.1 test server that counts the TCP
// connections opened to it. Used to prove keep-alive reuse: a response body
// that net/http never saw EOF on makes the client throw the socket away and
// dial again, which shows up here as a second StateNew.
func newConnCountingServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &conns
}

// replayBody is a rewindable io.ReadCloser for the allocation pin: the
// measured closure must not allocate anything of its own, so the reader is
// built once and Reset between runs.
type replayBody struct{ r *bytes.Reader }

func (b *replayBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *replayBody) Close() error               { return nil }

// readBodySink defeats an escape analysis that could stack-allocate the
// returned slice and make the allocation pin measure nothing.
var readBodySink []byte

// TestReadBodyShapes covers the three Content-Length shapes readBody must
// handle identically to today's io.ReadAll(io.LimitReader(...)).
//
// Mutant this kills: pre-sizing from a Content-Length larger than the cap
// (case "oversized") would allocate — and return — more than the caller's
// ceiling, defeating maxSegmentBodyBytes.
func TestReadBodyShapes(t *testing.T) {
	payload := bytes.Repeat([]byte("s"), 4096)
	for _, tc := range []struct {
		name       string
		contentLen int64
		capBytes   int64
		wantLen    int
	}{
		{"declared length", int64(len(payload)), maxSegmentBodyBytes, len(payload)},
		{"undeclared (chunked)", -1, maxSegmentBodyBytes, len(payload)},
		{"declared length equals the cap", int64(len(payload)), int64(len(payload)), len(payload)},
		{"oversized declaration is capped", int64(len(payload)), 100, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				ContentLength: tc.contentLen,
				Body:          io.NopCloser(bytes.NewReader(payload)),
			}
			got, err := readBody(resp, tc.capBytes)
			if err != nil {
				t.Fatalf("readBody: %v", err)
			}
			if len(got) != tc.wantLen {
				t.Fatalf("len = %d, want %d", len(got), tc.wantLen)
			}
			if !bytes.Equal(got, payload[:tc.wantLen]) {
				t.Error("bytes differ from the source payload")
			}
		})
	}
}

// TestReadBodySizedPathAllocatesOnce is the point of the change: a declared
// length must produce ONE allocation instead of io.ReadAll's 512-byte start
// and ~1.25x growth ladder (a 4 KB body climbs it several times; a 4 MB
// segment ~20 times, copying ~2x its own size in garbage).
//
// Mutant this kills: deleting the sized branch — the two numbers below then
// collapse onto each other and the strict inequality fails.
func TestReadBodySizedPathAllocatesOnce(t *testing.T) {
	payload := bytes.Repeat([]byte("s"), 8<<10)
	body := &replayBody{r: bytes.NewReader(payload)}

	sized := &http.Response{ContentLength: int64(len(payload)), Body: body}
	sizedAllocs := testing.AllocsPerRun(100, func() {
		body.r.Reset(payload)
		readBodySink, _ = readBody(sized, maxSegmentBodyBytes)
	})
	if sizedAllocs > 1 {
		t.Errorf("sized path allocs/op = %v, want <= 1 (the result buffer) — is the +1 EOF capacity forcing a regrow, or did pre-sizing regress?", sizedAllocs)
	}

	unsized := &http.Response{ContentLength: -1, Body: body}
	unsizedAllocs := testing.AllocsPerRun(100, func() {
		body.r.Reset(payload)
		readBodySink, _ = readBody(unsized, maxSegmentBodyBytes)
	})
	if unsizedAllocs <= sizedAllocs {
		t.Errorf("unsized allocs/op = %v, sized = %v — the sized path must allocate strictly fewer, or Content-Length is being ignored", unsizedAllocs, sizedAllocs)
	}
}

// TestFetchSegmentReusesConnection is the keep-alive pin. net/http returns a
// connection to the idle pool only after the body reaches its OWN io.EOF, so
// the natural io.ReadFull(make([]byte, n)) implementation of readBody — which
// stops exactly at Content-Length — would pay a fresh TCP handshake per
// segment across a whole recording.
//
// Mutant this kills: exactly that implementation (conns becomes 2).
func TestFetchSegmentReusesConnection(t *testing.T) {
	payload := strings.Repeat("m", 4096)
	srv, conns := newConnCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// Explicit Content-Length: this is the sized path under test.
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		io.WriteString(w, payload)
	})

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video.ts"),
	})
	for seq := range 2 {
		data, status, err := d.fetchSegment(t.Context(), d.buildSegmentURL(seq))
		if err != nil || status != http.StatusOK {
			t.Fatalf("fetchSegment(%d) = status %d, err %v", seq, status, err)
		}
		if len(data) != len(payload) {
			t.Fatalf("fetchSegment(%d) returned %d bytes, want %d", seq, len(data), len(payload))
		}
	}
	if got := conns.Load(); got != 1 {
		t.Errorf("server saw %d connections for 2 segment fetches, want 1 — the sized read stopped before net/http observed the body's EOF, so the socket was discarded", got)
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestReadBody|TestFetchSegmentReusesConnection' ./internal/engine/
```

Expected RED: compile failure — `undefined: readBody`.

- [ ] **Step 3: Implement `readBody`**

In `internal/engine/downloader_fetch.go`, immediately above `// fetchSegment downloads a single
segment (or playlist) by URL.` (`:138`):

```go
// readBody reads resp.Body, returns at most capBytes, and pre-allocates the
// result when the server declared a usable Content-Length.
//
// Segment and chunk bodies run 200 KB - 5 MB. io.ReadAll starts at 512 bytes
// and grows by ~1.25x, so an unsized read of a 4 MB segment copies the body
// through ~20 reallocations — about twice the final size in garbage — on
// every one of the thousands of segments a long recording fetches.
//
// The +1 capacity is load-bearing, not slack: net/http returns a connection
// to the idle pool only once the body has been read to its OWN io.EOF, and a
// read that stops exactly at Content-Length never triggers the Read that
// observes it (the same trap the 206 path's drain documents in fetchChunk).
// The extra byte gives that final Read somewhere to land without regrowing
// the buffer, so the sized path stays at one allocation AND keeps keep-alive
// reuse.
//
// A body with no declared length (chunked, or transparently decompressed) or
// one declaring at least capBytes falls back to today's bounded io.ReadAll.
func readBody(resp *http.Response, capBytes int64) ([]byte, error) {
	n := resp.ContentLength
	if n <= 0 || n >= capBytes {
		return io.ReadAll(io.LimitReader(resp.Body, capBytes))
	}
	buf := make([]byte, 0, n+1) // n+1 <= capBytes, so the cap still holds
	for len(buf) < cap(buf) {
		m, err := resp.Body.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+m]
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return buf, err
		}
	}
	// The body outran its own Content-Length (net/http itself caps at the
	// declared length, so this is a hand-built response or a future
	// transport): finish it on the bounded path rather than truncating.
	rest, err := io.ReadAll(io.LimitReader(resp.Body, capBytes-int64(len(buf))))
	return append(buf, rest...), err
}
```

- [ ] **Step 4: Use it at the two call sites**

`internal/engine/downloader_fetch.go:180` (inside `fetchSegment`):

```go
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSegmentBodyBytes))
```
becomes
```go
	data, err := readBody(resp, maxSegmentBodyBytes)
```

`internal/engine/downloader_fetch.go:582` (inside `fetchChunk`, the 206 path):

```go
	data, err := io.ReadAll(io.LimitReader(resp.Body, end-start+1))
```
becomes
```go
	data, err := readBody(resp, end-start+1)
```

**Leave the bounded drain at `:583-590` exactly as it is.** It still matters: `readBody`'s fallback
branch caps with an `io.LimitReader`, which returns EOF at its own counter without the body's, and a
206 whose `Content-Length` equals the cap takes that branch.

- [ ] **Step 5: Run the tests and watch them pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestReadBody|TestFetchSegment|TestCookieHeader' ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/
```

Expected: PASS, including the existing `downloader_direct_*` and `downloader_parallel_test.go`
suites that drive `fetchChunk` and `fetchSegment` end to end.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/downloader_fetch.go internal/engine/downloader_fetch_readbody_test.go
git commit -m "perf(engine): pre-size segment and chunk bodies from Content-Length

Segment and VOD-chunk bodies were read with io.ReadAll, which starts at 512
bytes and grows by ~1.25x — roughly twenty reallocations and twice the body
in garbage for a 4 MB segment, on every segment of a multi-hour recording.
readBody allocates once from Content-Length when the server declares a usable
one and falls back to the bounded io.ReadAll otherwise.

The buffer carries one byte of extra capacity on purpose: net/http only
returns a connection to the idle pool after the body reaches its own io.EOF,
and a read that stops exactly at Content-Length never sees it. A test counts
TCP connections across two fetches to keep that property pinned.

Sweep 2026-09-15 T2-17.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

## Task 4: Head-probe fallback fires only on an answered probe (T3-29)

**Files:**
- Modify: `internal/engine/downloader_fetch.go:391-419` (`probeHeadSequence` + its doc comment), `:449-457` (`probeHeadAt`'s two "unusable header" returns), `:3-15` (import `errors`)
- Test: `internal/engine/downloader_fetch_headprobe_test.go` (new)

**Interfaces:**
- Produces: `var errNoHeadSeqUsable = errors.New("no usable X-Head-Seqnum header")` — the sentinel
  `probeHeadAt` returns when the edge ANSWERED but the answer carried no parseable head.
- Consumes: `newConnCountingServer` is NOT needed here; this task starts its own servers.

- [ ] **Step 1: Write the failing test**

Create `internal/engine/downloader_fetch_headprobe_test.go`:

```go
package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestProbeHeadSequenceSkipsFallbackOnTransportError pins T3-29: during an
// outage the first probe never gets a response at all, and retrying at
// currentSeq+1000 just pays a second doomed round-trip. The fallback exists
// for an edge that ANSWERED without the header, not for a dead network.
//
// The server hijacks and closes the connection, which is a transport error
// (EOF) at the client while still letting the handler count the attempt.
//
// Mutant this kills: the pre-fix `else if cur > 0` with no error
// discrimination — probes becomes 2.
func TestProbeHeadSequenceSkipsFallbackOnTransportError(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			conn.Close() // no response at all: a transport error at the client
		}
	}))
	t.Cleanup(srv.Close)

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
	})
	d.currentSeq.Store(500) // > 0, so the fallback WOULD be reachable

	if _, err := d.probeHeadSequence(t.Context()); err == nil {
		t.Fatal("probeHeadSequence() = nil error against a server that answers nothing")
	}
	if got := probes.Load(); got != 1 {
		t.Errorf("server saw %d probes, want exactly 1 — a transport error must not trigger the currentSeq+1000 fallback", got)
	}
}

// TestProbeHeadSequenceFallsBackOnMissingHeader pins the half that must
// SURVIVE the fix: an edge that answers 200 without X-Head-Seqnum (rejected
// the absurd sequence, served an opaque error page) still gets the
// near-future retry, and its value is returned.
//
// Mutant this kills: deleting the fallback entirely, or widening the
// sentinel check to something a missing header does not satisfy.
func TestProbeHeadSequenceFallsBackOnMissingHeader(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		if strings.Contains(r.URL.String(), "999999999") {
			fmt.Fprint(w, "nope") // answered, but no usable header
			return
		}
		w.Header().Set("X-Head-Seqnum", "777")
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(srv.Close)

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
	})
	d.currentSeq.Store(500)

	seq, err := d.probeHeadSequence(t.Context())
	if err != nil {
		t.Fatalf("probeHeadSequence() = %v, want the fallback's answer", err)
	}
	if seq != 777 {
		t.Errorf("probeHeadSequence() = %d, want 777 (the fallback probe's header)", seq)
	}
	if got := probes.Load(); got != 2 {
		t.Errorf("server saw %d probes, want 2 — the fallback must still fire when the edge answered without a usable header", got)
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestProbeHeadSequence' ./internal/engine/
```

Expected RED: `TestProbeHeadSequenceSkipsFallbackOnTransportError` fails with
`server saw 2 probes, want exactly 1 …`. `TestProbeHeadSequenceFallsBackOnMissingHeader` passes
already (it pins existing behaviour that must survive).

- [ ] **Step 3: Add the sentinel and return it from `probeHeadAt`**

Add `"errors"` to `internal/engine/downloader_fetch.go`'s import block (it is not imported today).

Declare the sentinel immediately above `probeHeadSequence`'s doc comment (`:391`):

```go
// errNoHeadSeqUsable marks a probe that got an HTTP RESPONSE the fallback
// probe could plausibly improve on: no X-Head-Seqnum header, or one that
// does not parse. A TRANSPORT error — no response at all (DNS, refused,
// reset, timeout) — is deliberately NOT this: during an outage every probe
// fails the same way, so a fallback only doubles the doomed round-trips.
var errNoHeadSeqUsable = errors.New("no usable X-Head-Seqnum header")
```

In `probeHeadAt` (`:449-457`) replace:

```go
	headSeqStr := resp.Header.Get("X-Head-Seqnum")
	if headSeqStr == "" {
		return -1, fmt.Errorf("no X-Head-Seqnum header")
	}

	headSeq, err := strconv.Atoi(headSeqStr)
	if err != nil {
		return -1, fmt.Errorf("parse X-Head-Seqnum: %w", err)
	}
```

with:

```go
	headSeqStr := resp.Header.Get("X-Head-Seqnum")
	if headSeqStr == "" {
		return -1, errNoHeadSeqUsable
	}

	headSeq, err := strconv.Atoi(headSeqStr)
	if err != nil {
		return -1, fmt.Errorf("%w: parse %q: %v", errNoHeadSeqUsable, headSeqStr, err)
	}
```

- [ ] **Step 4: Gate the fallback on the sentinel**

Replace `probeHeadSequence`'s body (`:409-419`):

```go
func (d *SegmentDownloader) probeHeadSequence(ctx context.Context) (int, error) {
	if seq, err := d.probeHeadAt(ctx, 999999999); err == nil {
		return seq, nil
	} else if cur := int(d.currentSeq.Load()); cur > 0 {
		// Fallback to a sane near-future probe. Do not propagate the first
		// error — we'll surface the fallback's outcome instead.
		return d.probeHeadAt(ctx, cur+1000)
	} else {
		return -1, err
	}
}
```

with:

```go
func (d *SegmentDownloader) probeHeadSequence(ctx context.Context) (int, error) {
	seq, err := d.probeHeadAt(ctx, 999999999)
	if err == nil {
		return seq, nil
	}
	// Fallback only when the edge ANSWERED and the answer was unusable. A
	// transport error means the network is down or the host is unreachable,
	// and a second probe to the same host fails identically — during an
	// outage that doubled every probe cycle's round-trips for nothing.
	// Do not propagate the first error past the fallback — surface the
	// fallback's own outcome instead.
	if cur := int(d.currentSeq.Load()); cur > 0 && errors.Is(err, errNoHeadSeqUsable) {
		return d.probeHeadAt(ctx, cur+1000)
	}
	return -1, err
}
```

Update the doc comment's numbered item 2 (`:398-403`) so it states the new rule:

```go
//  2. Fallback: if the first probe ANSWERED but carried no usable
//     X-Head-Seqnum (server changed behavior, rejected the absurdly high
//     number, or returned an opaque error page) AND we already have a usable
//     currentSeq, retry at currentSeq+1000 — close enough to be plausible
//     while still being ahead of the head. A transport error (no response at
//     all) skips the fallback: the second probe would fail the same way.
```

- [ ] **Step 5: Run the tests and watch them pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestProbeHeadSequence' ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/
```

Expected: PASS, including `downloader_cookie_test.go`'s `probeHeadAt` case and
`downloader_parallel_test.go`'s probe-count assertions.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/downloader_fetch.go internal/engine/downloader_fetch_headprobe_test.go
git commit -m "fix(engine): head-probe fallback only retries an answered probe

probeHeadSequence retried at currentSeq+1000 whenever the first probe failed
for any reason — including a transport error, where the second probe fails
identically. During an outage that doubled the round-trips of every probe
cycle. probeHeadAt now returns errNoHeadSeqUsable for the case the fallback
exists for (a response with no parseable X-Head-Seqnum), and only that case
retries.

Sweep 2026-09-15 T3-29.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

## Task 5: Drain 4xx bodies so the socket survives (T4-35)

**Files:**
- Modify: `internal/engine/downloader_fetch.go:167-178` (the `resp.StatusCode >= 400` branch of `fetchSegment`)
- Test: `internal/engine/downloader_fetch_drain_test.go` (new)

**Interfaces:**
- Consumes: `newConnCountingServer` (defined in Task 3's test file, same package),
  `errorBodySnippetBytes` (`:72`), `maxDrainBytes` (`:78`).
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

Create `internal/engine/downloader_fetch_drain_test.go`:

```go
package engine

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestFetchSegment4xxKeepsConnection pins T4-35: the 512-byte error snippet
// leaves the rest of the body unread, so net/http never sees the body's EOF
// and throws the socket away. A 403 burst — which is exactly how a stale
// credential or an ended stream announces itself, thousands of times in a
// row across a catch-up window — then pays a TCP (and on the real CDN, TLS)
// handshake per segment.
//
// Mutant this kills: removing the bounded drain after the snippet read —
// the server then sees one connection per request.
func TestFetchSegment4xxKeepsConnection(t *testing.T) {
	// Comfortably past errorBodySnippetBytes (512) so the snippet read
	// genuinely stops mid-body, and well under maxDrainBytes (64 KiB) so
	// the drain completes.
	body := strings.Repeat("e", 2048)
	srv, conns := newConnCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, body)
	})

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
	})
	for seq := range 2 {
		_, status, err := d.fetchSegment(t.Context(), d.buildSegmentURL(seq))
		if status != http.StatusForbidden {
			t.Fatalf("fetchSegment(%d) status = %d, want 403", seq, status)
		}
		if err == nil {
			t.Fatalf("fetchSegment(%d) err = nil, want the HTTP 403 error", seq)
		}
		if !strings.Contains(err.Error(), "HTTP 403") {
			t.Fatalf("fetchSegment(%d) err = %v, want an HTTP 403 message", seq, err)
		}
		if n := len(err.Error()); n > 1024 {
			t.Fatalf("fetchSegment(%d) error is %d bytes — the drain must be DISCARDED, not appended to the snippet", seq, n)
		}
	}
	if got := conns.Load(); got != 1 {
		t.Errorf("server saw %d connections for 2 403s, want 1 — the unread body tail made the client discard the socket", got)
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestFetchSegment4xxKeepsConnection ./internal/engine/
```

Expected RED: `server saw 2 connections for 2 403s, want 1 …`.

- [ ] **Step 3: Drain after the snippet**

In `internal/engine/downloader_fetch.go`, inside the `if resp.StatusCode >= 400 {` branch, after the
snippet read at `:172`:

```go
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodySnippetBytes))
		// Discard the rest (bounded) so the deferred Close returns the
		// connection to the idle pool: net/http reuses a connection only
		// when the body was read to its own EOF, and the snippet read stops
		// 512 bytes in. Without this, a 403 burst — a stale credential, or
		// an ended stream answering every catch-up worker — paid a fresh
		// TCP+TLS handshake per segment. Same policy and same cap as the
		// 206 path in fetchChunk: a pathological error page past
		// maxDrainBytes is not worth pulling in full to reclaim one socket.
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
		if len(snippet) > 0 {
```

(the `if len(snippet) > 0 {` line is the existing next statement — leave it and everything below it
unchanged).

- [ ] **Step 4: Run the test and watch it pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestFetchSegment4xxKeepsConnection ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/
```

Expected: PASS, and no change in `downloader_fetch_403_test.go` (the error TEXT is unchanged — only
the socket's fate differs).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/downloader_fetch.go internal/engine/downloader_fetch_drain_test.go
git commit -m "perf(engine): drain 4xx bodies so the socket returns to the pool

fetchSegment read a 512-byte snippet of an error body for the log line and
then closed. net/http only reuses a connection whose body reached its own
EOF, so every 403 in a burst — a stale credential, or an ended stream
answering each catch-up worker — cost a fresh handshake. Mirror the 206
path: discard the remainder bounded by maxDrainBytes before returning.

Sweep 2026-09-15 T4-35.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

## Task 6: Caller cancellation is not a connectivity failure (T4-35)

**Files:**
- Modify: `internal/utils/http.go:79-99` (`FetchWithTimeout`)
- Modify: `internal/engine/downloader_fetch.go:127-138` (new helper), `:139-157` (`fetchSegment`), `:421-440` (`probeHeadAt`), `:470-486` (`probeFileSize`), `:548-565` (`fetchChunk`)
- Test: `internal/engine/downloader_fetch_cancel_test.go` (new); append two tests to `internal/utils/http_test.go`

**Interfaces:**
- Produces: `func reportFetchFailure(parent context.Context, tag string)` in
  `internal/engine/downloader_fetch.go` — records a connectivity failure unless `parent` is already
  done.
- Consumes: the existing `reportFailure(tag string)` (`:127`), `connectivity.Reporter` via
  `SetConnectivityReporter`, and in the utils test the existing `recordingReporter`
  (`internal/utils/http_test.go:186-192`).

**Why the parent context and not `ctx`:** all five sites shadow their parameter —
`ctx, cancel := context.WithTimeout(ctx, …)` — before the reporting line. Guarding on the shadowed
`ctx` would also suppress a genuine per-request TIMEOUT, which is real connectivity evidence. The
guard must ask the caller's context.

- [ ] **Step 1: Write the failing engine test**

Create `internal/engine/downloader_fetch_cancel_test.go`:

```go
package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// countingReporter is an atomic ConnectivityReporter: the engine's reporter
// is a package global, so a plain-int recorder is not safe to install while
// any other test is in flight.
type countingReporter struct {
	fails     atomic.Int32
	successes atomic.Int32
}

func (c *countingReporter) ReportFailure(string) { c.fails.Add(1) }
func (c *countingReporter) ReportSuccess(string) { c.successes.Add(1) }

// TestFetchSegmentCancelIsNotAConnectivityFailure pins T4-35: a shutdown, a
// quality split or a superseded refresh cancels the download context, the
// in-flight request dies with it, and that is a decision Moombox made — not
// evidence about the network. Counting it drags the connectivity oracle
// toward "offline" on every clean stop.
//
// The handler blocks until the client goes away, so the ONLY way the request
// ends is the caller's cancel.
//
// Mutant this kills: reporting unconditionally (fails becomes 1). A guard
// written against the SHADOWED ctx would pass here and fail the timeout test
// below.
//
// Do not add t.Parallel(): the reporter is a package global.
func TestFetchSegmentCancelIsNotAConnectivityFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	rec := &countingReporter{}
	SetConnectivityReporter(rec)
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
	})

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		defer func() { _ = recover() }()
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, _, err := d.fetchSegment(ctx, d.buildSegmentURL(1)); err == nil {
		t.Fatal("fetchSegment on a cancelled context returned nil error")
	}
	cancel()

	if got := rec.fails.Load(); got != 0 {
		t.Errorf("connectivity failures = %d, want 0 — a caller cancel was recorded as a network failure", got)
	}
}

// TestFetchSegmentTransportErrorIsAConnectivityFailure is the other half:
// with a perfectly healthy caller context, a request that dies on the wire
// IS network evidence and must still be reported.
//
// The server hangs up without answering, which is a transport error at the
// client. Note what this test deliberately does NOT do: it passes t.Context()
// straight through, with no derived deadline of its own, so the only context
// that can be done is the one fetchSegment derives internally.
//
// Mutant this kills: guarding on the SHADOWED ctx (which carries
// SegmentTimeout, and which a future short-timeout path would trip) or
// suppressing every error outright — fails drops to 0 either way.
func TestFetchSegmentTransportErrorIsAConnectivityFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			conn.Close() // no response at all
		}
	}))
	t.Cleanup(srv.Close)

	rec := &countingReporter{}
	SetConnectivityReporter(rec)
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
	})
	if _, _, err := d.fetchSegment(t.Context(), d.buildSegmentURL(1)); err == nil {
		t.Fatal("fetchSegment against a hang-up server returned nil error")
	}
	if got := rec.fails.Load(); got != 1 {
		t.Errorf("connectivity failures = %d, want 1 — a transport error with a healthy caller IS network evidence", got)
	}
}
```

- [ ] **Step 2: Write the failing utils tests**

Append to `internal/utils/http_test.go`:

```go
// TestFetchWithTimeoutCallerCancelIsNotAFailure pins T4-35 for the shared
// helper: the connectivity oracle must not learn "the network is down" from
// a shutdown or a superseded probe cancelling its own fetch.
//
// Mutant this kills: reporting unconditionally (failures becomes 1).
func TestFetchWithTimeoutCallerCancelIsNotAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	rec := &recordingReporter{}
	SetConnectivityReporter(rec)
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, _, err := FetchWithTimeout(ctx, srv.URL, 5*time.Second, nil); err == nil {
		t.Fatal("FetchWithTimeout on a cancelled context returned nil error")
	}
	cancel()

	if got := rec.failures.Load(); got != 0 {
		t.Errorf("failures = %d, want 0 — a caller cancel was recorded as a network failure", got)
	}
}

// TestFetchWithTimeoutTransportErrorIsAFailure is the other half: a real
// transport failure with a healthy caller must still be reported.
//
// Mutant this kills: a guard that suppresses every error, or one written
// against the DERIVED context (which carries the helper's own timeout).
func TestFetchWithTimeoutTransportErrorIsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	t.Cleanup(srv.Close)

	rec := &recordingReporter{}
	SetConnectivityReporter(rec)
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	if _, _, err := FetchWithTimeout(t.Context(), srv.URL, 5*time.Second, nil); err == nil {
		t.Fatal("FetchWithTimeout against a hang-up server returned nil error")
	}
	if got := rec.failures.Load(); got != 1 {
		t.Errorf("failures = %d, want 1 — a transport error with a healthy caller IS network evidence", got)
	}
}
```

- [ ] **Step 3: Run both test sets and watch them fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestFetchSegmentCancel|TestFetchSegmentTransportError' ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestFetchWithTimeout' ./internal/utils/
```

Expected RED: `connectivity failures = 1, want 0 …` (engine) and `failures = 1, want 0 …` (utils).
The two "IS network evidence" tests pass already.

- [ ] **Step 4: Guard `internal/utils/http.go`**

Rename `FetchWithTimeout`'s first parameter to `parent` and guard the report (`:79-96`):

```go
func FetchWithTimeout(parent context.Context, url string, timeout time.Duration, headers map[string]string) (*http.Response, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	...
	resp, err := utilsHTTPClient.Do(req)
	if err != nil {
		// A request the CALLER abandoned (shutdown, a superseded probe, a
		// cancelled job) says nothing about the network — recording it
		// drags the connectivity oracle toward "offline" on every clean
		// stop. The check is against the caller's context on purpose: the
		// derived one above carries this helper's own timeout, and a
		// request that genuinely ran out of time IS network evidence.
		if parent.Err() == nil {
			reportConnResult(true)
		}
		cancel()
		return nil, nil, err
	}
```

Everything else in the function (including `reportConnResult(false)` on success) is unchanged.

- [ ] **Step 5: Add the engine helper and apply it at the four sites**

In `internal/engine/downloader_fetch.go`, directly under `reportFailure` (`:127-131`):

```go
// reportFetchFailure records a connectivity failure unless the CALLER's
// context is already done. A cancelled download (shutdown, user cancel,
// quality split, superseded refresh) kills its in-flight requests by design,
// and counting those as network failures drags the connectivity oracle
// toward "offline" on every clean stop. Every fetch below derives a
// per-request context from its caller's, so the guard must ask the parent:
// the derived one carries the request deadline, and a request that genuinely
// timed out IS evidence.
func reportFetchFailure(parent context.Context, tag string) {
	if parent.Err() != nil {
		return
	}
	reportFailure(tag)
}
```

Then, at each of the four sites, rename the function's context parameter to `parent`, derive `ctx`
from it, and call the helper:

1. `fetchSegment` (`:139-157`):
```go
func (d *SegmentDownloader) fetchSegment(parent context.Context, segURL string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(parent, SegmentTimeout)
	...
	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		reportFetchFailure(parent, "engine/fetch")
		return nil, 0, err
	}
```
2. `probeHeadAt` (`:421-440`): `func (d *SegmentDownloader) probeHeadAt(parent context.Context, probeSeq int) (int, error)`, `ctx, cancel := context.WithTimeout(parent, 10*time.Second)`, `reportFetchFailure(parent, "engine/fetch")`.
3. `probeFileSize` (`:470-486`): `func (d *SegmentDownloader) probeFileSize(parent context.Context) int64`, `ctx, cancel := context.WithTimeout(parent, 10*time.Second)`, `reportFetchFailure(parent, "engine/fetch")`.
4. `fetchChunk` (`:548-565`): `func (d *SegmentDownloader) fetchChunk(parent context.Context, start, end int64) ([]byte, int, error)`, keep the existing `ctx, cancel := context.WithTimeout(parent, …)` line shape, `reportFetchFailure(parent, "engine/fetch")`.

Leave every `reportSuccess("engine/fetch")` call untouched — a response that arrived is evidence
regardless of who cancels next.

- [ ] **Step 6: Run the tests and watch them pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestFetchSegmentCancel|TestFetchSegmentTransportError|TestConnectivityReporter' ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/ ./internal/utils/ ./internal/connectivity/
```

Expected: PASS everywhere, including `internal/connectivity`'s passive-tracker tests (this changes
when a failure is reported, never the tag).

- [ ] **Step 7: Commit**

```bash
git add internal/utils/http.go internal/utils/http_test.go internal/engine/downloader_fetch.go internal/engine/downloader_fetch_cancel_test.go
git commit -m "fix(engine,utils): a cancelled fetch is not a connectivity failure

A request the caller abandoned — shutdown, user cancel, quality split,
superseded refresh — dies by design, and recording it as a network failure
drags the connectivity oracle toward \"offline\" on every clean stop. The
guard asks the CALLER's context, not the per-request one each site derives
from it: a request that genuinely ran out of time is still real evidence.

Applied at utils.FetchWithTimeout and, via one reportFetchFailure helper, at
all four engine fetch sites (segment, head probe, size probe, chunk) — the
spec named two, but all four shadow their context identically and would each
have needed the same reasoning.

Sweep 2026-09-15 T4-35.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

## Task 7: Delete the dead HealthUpdate plumbing (T4-31)

**Files:**
- Modify: `internal/engine/downloader.go:274-296` (the `HealthUpdate` type + its comment), `:450-456` (`startedAt`, `transientRetries`, `lastTransientErr` + their comment), `:463-466` (`OnHealthUpdate`), `:527-557` (`emitHealthUpdate`), `:901-903` (`startedAt.StoreNow()` + comment)
- Modify: `internal/engine/downloader_dash.go:261-272` (the sole `emitHealthUpdate` call + the half of its comment that describes health)

**Interfaces:**
- Produces: nothing. Removes the exported type `HealthUpdate` and the exported field
  `SegmentDownloader.OnHealthUpdate`; no package outside `internal/engine` references either
  (verified below), so no consumer changes.

**Why this is safe:** `OnHealthUpdate` is never assigned anywhere in the repo, so
`emitHealthUpdate` returns at its first line every time; `transientRetries` and `lastTransientErr`
have no writers at all, so two of the three snapshot fields are structurally zero; `startedAt` has
exactly one writer and one reader, both inside this dead path. Nothing in `docs/`, `SPEC.md`,
`CLAUDE.md` or `.claude/skills/` mentions it.

- [ ] **Step 1: Establish the red — prove the symbols exist and have no live consumer**

```bash
grep -rn "HealthUpdate\|emitHealthUpdate\|transientRetries\|lastTransientErr\|startedAt" --include=*.go internal/engine cmd internal/worker internal/web
grep -rn "HealthUpdate" docs SPEC.md CLAUDE.md .claude
```

Expected now: matches only in `internal/engine/downloader.go` (`:274`, `:280`, `:450-456`, `:463`,
`:466`, `:527`, `:531-556`, `:901`, `:903`) and `internal/engine/downloader_dash.go:272`; the docs
grep prints nothing. **If either grep shows anything else, stop and re-plan** — the deletion's
premise is exactly this emptiness. (`startedAt` also matches
`internal/cookies/autocookies_firefox.go` and `internal/worker/orchestrator_mux.go`; those are
unrelated local variables in other packages — do not touch them.)

- [ ] **Step 2: Delete the call site**

In `internal/engine/downloader_dash.go`, replace `:261-272`:

```go
		// Emit progress + aggregate health snapshot. The health update
		// piggy-backs on the same cadence so the UI sees throughput /
		// retry counters tick alongside the per-segment counter. Audit
		// reports/engine.md #31.
		p := DownloadProgress{
			Seq:     writeSeq,
			Bytes:   d.bytesWritten.Load(),
			HeadSeq: int(d.headSeq.Load()),
		}
		if d.OnProgress != nil {
			d.OnProgress(p)
		}
		d.emitHealthUpdate(p)
```

with:

```go
		p := DownloadProgress{
			Seq:     writeSeq,
			Bytes:   d.bytesWritten.Load(),
			HeadSeq: int(d.headSeq.Load()),
		}
		if d.OnProgress != nil {
			d.OnProgress(p)
		}
```

- [ ] **Step 3: Delete the plumbing in `internal/engine/downloader.go`**

Delete, in this order (so the file never references a symbol it has already lost):

1. `emitHealthUpdate` in full, together with its doc comment (`:527-557`).
2. The `OnHealthUpdate` field and its three comment lines (`:463-466`), leaving the surrounding
   `OnStart`/`OnProgress`/`OnGap`/`OnFinish`/`OnCipherFailure` block otherwise untouched.
3. The three fields and their five-line comment (`:450-456`):
   `// startedAt + transientRetries + lastTransientErr feed HealthUpdate` … through
   `lastTransientErr atomic.Pointer[string]`.
4. The `HealthUpdate` type and its whole doc comment (`:274-296`).
5. The `startedAt.StoreNow()` call and its two comment lines (`:901-903`):
   `// Mark the start time so HealthUpdate can derive throughput / ETA.` /
   `// Audit reports/engine.md #31.` / `d.startedAt.StoreNow()`.

Do NOT remove the `atomicTime` type or the `atomic` import — `lastSegTime`, `lastCatchUpFailure`,
`lastStreamStatusCheck`, `lastHeadProbeTime`, `baseURLOverride` and `poTokenOverride` all still use
them.

- [ ] **Step 4: Verify the green**

```bash
grep -rn "HealthUpdate\|emitHealthUpdate\|transientRetries\|lastTransientErr" --include=*.go .
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
staticcheck ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/ ./internal/worker/ ./internal/docs/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp gofmt -l ./cmd ./internal ./tools ./web
```

Expected: the grep prints nothing; build, vet and staticcheck are clean (staticcheck U1000 is the
gate that would have caught a half-deletion); all three suites PASS; `gofmt -l` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/downloader.go internal/engine/downloader_dash.go
git commit -m "refactor(engine): delete the dead HealthUpdate plumbing

OnHealthUpdate was never assigned by any caller, so emitHealthUpdate returned
at its first line on every DASH segment; two of the three metrics it would
have reported (transientRetries, lastTransientErr) had no writers at all.
Remove the type, the callback, the emitter, the three fields and the single
call site. No package outside internal/engine referenced any of it and no doc
cites it.

Sweep 2026-09-15 T4-31.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

## Task 8: Correct the retry-backoff comment, delete the plan, run the arc gates (T4-34)

**Files:**
- Modify: `internal/engine/downloader_fetch.go:188` (the `fetchSegmentWithRetry` doc comment)
- Delete: `docs/superpowers/plans/2026-09-15-sweep-2-engine.md` (spec §2: plans are deleted in the arc's last commit)

**Interfaces:** none.

- [ ] **Step 1: Re-verify the drift before editing**

```bash
sed -n '188p;296,299p' internal/engine/downloader_fetch.go
sed -n '281,287p' internal/engine/downloader_fetch.go
sed -n '513p;529,534p' internal/engine/downloader_fetch.go
```

Expected: `:188` claims "exponential backoff"; `:298` is
`utils.Sleep(ctx, time.Duration(5*(attempt+1))*time.Second)` — linear 5/10/15/20 s with the last
attempt's sleep skipped; `:286` is the 403-only `d.delays.singleGoneRetry<<attempt` doubling ramp;
and `fetchChunkWithRetry` at `:513`/`:531` really is `1<<attempt` seconds capped at 60 — leave that
one alone.

- [ ] **Step 2: Rewrite the comment**

Replace `internal/engine/downloader_fetch.go:188`:

```go
// fetchSegmentWithRetry attempts to fetch a segment with retries and exponential backoff.
```

with:

```go
// fetchSegmentWithRetry attempts to fetch a segment with retries and two
// different backoff ramps, neither of them exponential across the whole
// function: the 403-with-refresh path doubles (singleGoneRetry << attempt —
// 500ms/1s/2s/4s, sized to outlive credentialRefreshCooldown), while every
// other transient failure waits a LINEAR 5s x (attempt+1) — 5s, 10s, 15s, 20s
// over the default MaxSegmentRetries=5, with the final attempt's sleep
// skipped because no fetch follows it.
```

- [ ] **Step 3: Verify**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./internal/engine/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp gofmt -l ./internal
```

Expected: builds, `gofmt -l` prints nothing (a comment-only change must not reflow anything else).

- [ ] **Step 4: Delete the plan**

```bash
rm docs/superpowers/plans/2026-09-15-sweep-2-engine.md
```

- [ ] **Step 5: Run the arc's full gate list**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp gofmt -l ./cmd ./internal ./tools ./web
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
staticcheck ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOOS=linux GOARCH=arm64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/engine/... ./internal/utils/... ./internal/worker/... ./internal/docs/
# and, coordinated with the controller so only one runs chain-wide at a time:
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./...
```

Expected: `gofmt -l` prints nothing; vet, staticcheck and all three builds clean; every suite PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/downloader_fetch.go
git rm docs/superpowers/plans/2026-09-15-sweep-2-engine.md
git commit -m "docs(engine): fetchSegmentWithRetry's transient ramp is linear, not exponential

The doc comment said \"exponential backoff\" for a function whose transient
path sleeps 5s x (attempt+1). The 403-with-refresh path IS a doubling ramp,
so the comment now names both. fetchChunkWithRetry, which really is
exponential, is untouched.

Also deletes this arc's implementation plan per the chain's
delete-implemented-plans rule.

Sweep 2026-09-15 T4-34.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq"
```

---

## Self-review

**1. Spec coverage — every §5 design item and every ledger item this arc owns:**

| Spec §5 item | Ledger item | Task | Covered by |
|---|---|---|---|
| 1. HLS end verdict | T1-2 | 1 | Three sites changed; three tests (check error → no finalize; error-then-live → `ErrQualityLost`; confirmed end → nil); log wording asserted; `architecture.md` bullet |
| 2. Resume save cadence | T2-14 | 2 | `hlsResumeSaveInterval` + `delays.hlsResumeSave` + `onResumeSaved` seam; 12-reload write-count test + the delays pins |
| 3. Body pre-sizing | T2-17 | 3 | `readBody` at both call sites; shape table, `AllocsPerRun` pin, keep-alive `ConnState` pin |
| 4. Head probe | T3-29 | 4 | `errNoHeadSeqUsable`; transport-error test (1 probe) + answered-without-header test (2 probes) |
| 5. 4xx drain | T4-35 | 5 | Bounded drain after the snippet; `ConnState` test counting 1 connection for two 403s |
| 6. Caller cancellation | T4-35 | 6 | `reportFetchFailure` at four engine sites + the `FetchWithTimeout` guard; cancel-vs-transport-error pairs in both packages |
| 7. Dead code | T4-31 | 7 | Type, callback, emitter, three fields, one call site; grep + build + vet + staticcheck + docs gate |
| 8. Doc drift | T4-34 | 8 | `fetchSegmentWithRetry` comment (anchor corrected from `downloader.go:597`) |
| §5 "Gates" | — | 8 | Full gate list, including `./internal/worker/...` (the HLS verdict consumer) and `./internal/docs/` |
| §2 plan deletion | — | 8 | `git rm` in the arc's last commit |

No §5 item is unassigned. Two spec statements are corrected rather than implemented, both flagged in
"Anchor corrections": the `utils/http.go:235-238` anchor (real site `:94`) and the
`downloader.go:597-607` anchor (real site `downloader_fetch.go:188`). One spec claim — that the
exhausted-budget exit produces an `Error` job status — is documented as wrong in Task 1 with the
worker citations that show what actually happens; no test asserts it.

**2. Placeholder scan:** no "TBD", "TODO", "implement later", "add error handling", "similar to Task
N", or test-free steps. Every code step carries the literal Go it installs; every test step carries
the literal test; every run step carries the command and the expected output. The one place a step
says "replace this test's body" (Task 6, Step 1) supplies the replacement code inline rather than
describing it.

**3. Type consistency:**
- `readBody(resp *http.Response, capBytes int64) ([]byte, error)` — declared in Task 3, called in
  Task 3 only; both call sites pass an `int64` (`maxSegmentBodyBytes` is an untyped constant,
  `end-start+1` is `int64`).
- `newConnCountingServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32)` —
  declared in Task 3's test file, used in Task 3 and Task 5 with the same signature.
- `reportFetchFailure(parent context.Context, tag string)` — declared and used only in Task 6, at
  four sites, always with `"engine/fetch"`.
- `errNoHeadSeqUsable` (Task 4) — declared once, matched with `errors.Is`.
- `delays.hlsResumeSave` / `hlsResumeSaveInterval` / `onResumeSaved func(lastSeq int)` (Task 2) —
  spelled identically in `delays.go`, `delays_test.go`, `downloader.go`, `downloader_resume.go`,
  `downloader_hls.go` and the new test.
- `warnCollector` / `fastDelays()` / `fast()` / `activityRecorder` / `awaitActivity` are pre-existing
  package test helpers; this plan adds none with colliding names (`countingReporter` in Task 6 is
  distinct from the existing `fakeReporter` in `downloader_test.go`, deliberately, because the
  existing one uses non-atomic ints).
- Log string reused verbatim from the DASH loop: `"stream status check failed; deferring end verdict"`
  (`downloader_dash.go:455`) — one spelling, three new sites.
