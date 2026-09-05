# Arc E — Engine Test Speed (injectable loop delays) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `go test ./internal/engine/` drops from ~53 s to under 10 s by making every wait the download loops sleep on injectable, with production timing pinned unchanged.

**Architecture:** `SegmentDownloader` gains an unexported `delays` struct (one field per loop wait, defaults = the existing package constants, assigned in `NewSegmentDownloader`). The eleven-plus sleep sites read `d.delays.X`; `waitForConnectivity` and `hlsReloadDelay` take their time unit as a parameter. Tests poke `d.delays = fastDelays()` (every wait ÷ 20, ratios preserved) and replace wall-clock "still running after N s" waits with `OnActivity`-driven waits, so they assert the loop REACHED a state rather than that time passed.

**Tech Stack:** Go 1.27.1, `internal/utils.Sleep(ctx, d)` (the one sleep primitive), `OnActivity func(DownloadActivity)` hook (`downloader.go:450`).

**Spec:** `docs/superpowers/specs/2026-09-04-improvement-chain-design.md` §7 (Arc E). Survey that grounds every line number below: the Arc E entry in `.superpowers/sdd/2026-09-04-improvement-chain/progress.md` and the controller's slow-test table (reproduced per task). Line numbers were read at main = `e932633`; re-verify with `grep -n` before editing.

## Global Constraints

- Worktree `D:/Git/Moombox/.worktrees/improvement-e-engine-delays`, branch `improvement-e-engine-delays`, cut from `main`. Never push. Never `cd` to the main checkout.
- Every `go` command carries `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`. Implementers run ONLY `./internal/engine/` (and the packages a task names); the controller runs the single full `go test -count=1 ./...`.
- `gofmt -l ./cmd ./internal ./tools ./web` prints nothing; `go vet ./...` silent; `staticcheck ./...` (2026.2.1) prints nothing — it is a hard CI gate since Arc C.
- **Production timing is unchanged.** Every default in `defaultDelays()` equals the constant it replaces; `TestDefaultDelaysMatchConstants` pins it. No constant's value changes. No new exported API on `SegmentDownloader`.
- **Tests assert events, not elapsed time**, wherever a test today waits a wall-clock margin to prove the loop is "still running" — use `awaitActivity`. Elapsed-time *ceilings* that only prove "released promptly" may stay, scaled.
- Scale factor is `fastScale = 20` everywhere (a wait of 500 ms becomes 25 ms). Do not pick per-test scales; ratios between waits are what the loops' escalation counts depend on.
- Every commit ends with both trailers, exactly:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
  ```
  Write the message to a file and `git commit -F <file>` (never `-F -`).

### The waits, where they live, and who waits on them (survey at e932633)

| Field | Constant / literal today | Production sites | Slow tests that wait on it |
|---|---|---|---|
| `singleGoneRetry` | `singleGoneRetryDelay` 500 ms (`downloader_dash.go:44`) | `downloader_dash.go:474,483,530`; `downloader_fetch.go:286` (`<<attempt`) | GoneErrorStall…, BackstopCeiling…, DashLoopBehindHead…, DashLoopTransientBurst…, InterruptionNoStall×2, DashLoopCleanPostLiveEnd, ConfirmedEnded…, DashLoopRecoversFromCredentialExpiry, ForbiddenBehindHeadExhausts… |
| `interruptionStallRetry` | `interruptionStallRetryDelay` 5 s (`:55`) | `downloader_dash.go:491,742` | GoneErrorStall…, BackstopCeiling…, BackstopStallsWhileMayResume |
| `transientFailureRetry` | `transientFailureRetryDelay` 1 s (`:47`) | `downloader_dash.go:636` | NilMayResumeByteCompat |
| `genericRetry` | `genericRetryDelay` 2 s (`:38`) | `downloader_dash.go:351` | (none slow today; same shape) |
| `hlsPlaylistRetry` | literal `5*time.Second` | `downloader_hls.go:295,343` | (none slow today; same shape) |
| `hlsStuckRetry` | literal `2*time.Second` | `downloader_hls.go:541,590` | HlsLive_FMP4StuckSegmentSkipsNotSplitCycle (5×2 s) |
| `connectivityPoll` | `connectivityPollInterval` 5 s (`connectivity_wait.go:9`) | `waitForConnectivity` ticker `:56`; callers `downloader_dash.go:427,665,728`, `downloader_hls.go:121` | HlsLoop_EnforceMaxTimeoutPausesForOfflineOutage, WaitForConnectivity_WaitsAndReturns |
| `atEdgeBackoffUnit` | `time.Second` | `downloader_dash.go:593` (429 backoff: `1<<shift` and `delayCap` seconds), `:755` (`*sameHeadRetryDelay` seconds) | NilMayResumeByteCompat |
| `hlsReloadUnit` | `time.Second` inside `hlsReloadDelay` (`downloader_hls.go:156`) | `downloader_hls.go:714` | HlsLoop_NoEnforceRespectsStatusCheck, Hls_FMP4ResumeDoesNotRewriteInit, HlsLoop_StopOnGap…, HlsLoop_EnforceMaxTimeoutForcesFinalize, HlsLive_FMP4RevertsToTSSplitsPart |

Left as constants on purpose (no slow test waits on them; note in the ledger as residuals): `firstSegmentHuntDelay` (100 ms), `credentialRefreshCooldown` (a claim gate, not a sleep), `downloader_fetch.go:298` `5*(attempt+1)` s and `:534` chunk backoff (direct downloads), `eviction_probe.go:92` `evictionProbeRetryDelay`, `HeadProbeInterval`, `streamStatusCheckInterval`, `catchUpRegrowInterval`, `isOnlineProbeTimeout`.

---

### Task 1: The `delays` struct, its defaults, every production read, and the pin test

**Files:**
- Create: `internal/engine/delays.go`
- Create: `internal/engine/delays_test.go` (pin test + the two test helpers Tasks 2–3 consume)
- Modify: `internal/engine/downloader.go:676-700` (`NewSegmentDownloader`), the `SegmentDownloader` struct (`:331`)
- Modify: `internal/engine/downloader_dash.go:351,474,483,491,530,593,636,742,755` and the `waitForConnectivity` callers `:427,665,728`
- Modify: `internal/engine/downloader_fetch.go:286`
- Modify: `internal/engine/downloader_hls.go:121,143-160,295,343,541,590,714` (+ new constants near the file's top)
- Modify: `internal/engine/connectivity_wait.go:52-66`
- Modify: `internal/engine/downloader_hls_reload_test.go` and `internal/engine/connectivity_wait_test.go` only as far as the two signature changes require (pass `time.Second` / `connectivityPollInterval`) — behaviour edits to those tests belong to Tasks 2–3.

**Interfaces:**
- Produces: `type delays struct{...}` (fields as in the table), `func defaultDelays() delays`, field `SegmentDownloader.delays`, `func waitForConnectivity(ctx context.Context, isOnline func() bool, poll time.Duration) error`, `func hlsReloadDelay(lastSegDur, targetDur float64, hadNewSegments bool, elapsed, unit time.Duration) time.Duration`, constants `hlsPlaylistRetryDelay`, `hlsStuckRetryDelay`; test helpers `fastDelays() delays`, `activityRecorder(d *SegmentDownloader) <-chan DownloadActivity`, `awaitActivity(t, ch, want, within)`.

- [ ] **Step 1: Write the failing pin test** — `internal/engine/delays_test.go`:

```go
package engine

import (
	"reflect"
	"testing"
	"time"
)

// TestDefaultDelaysMatchConstants pins production timing: every loop wait the
// downloader sleeps on equals the constant that documented it before the
// delays struct existed, and NewSegmentDownloader installs exactly those
// defaults. A new field without a default is caught by the zero check.
func TestDefaultDelaysMatchConstants(t *testing.T) {
	want := delays{
		singleGoneRetry:        singleGoneRetryDelay,
		interruptionStallRetry: interruptionStallRetryDelay,
		transientFailureRetry:  transientFailureRetryDelay,
		genericRetry:           genericRetryDelay,
		hlsPlaylistRetry:       hlsPlaylistRetryDelay,
		hlsStuckRetry:          hlsStuckRetryDelay,
		connectivityPoll:       connectivityPollInterval,
		atEdgeBackoffUnit:      time.Second,
		hlsReloadUnit:          time.Second,
	}
	if got := defaultDelays(); got != want {
		t.Fatalf("defaultDelays() = %+v, want %+v", got, want)
	}
	// The literal values, so a constant edit is a visible diff here too.
	for name, pair := range map[string][2]time.Duration{
		"singleGoneRetry":        {want.singleGoneRetry, 500 * time.Millisecond},
		"interruptionStallRetry": {want.interruptionStallRetry, 5 * time.Second},
		"transientFailureRetry":  {want.transientFailureRetry, time.Second},
		"genericRetry":           {want.genericRetry, 2 * time.Second},
		"hlsPlaylistRetry":       {want.hlsPlaylistRetry, 5 * time.Second},
		"hlsStuckRetry":          {want.hlsStuckRetry, 2 * time.Second},
		"connectivityPoll":       {want.connectivityPoll, 5 * time.Second},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %v, want %v (production timing must not move in this arc)", name, pair[0], pair[1])
		}
	}
	d := NewSegmentDownloader(DownloaderOptions{OutputFile: t.TempDir() + "/o.ts"})
	if d.delays != want {
		t.Errorf("NewSegmentDownloader installed %+v, want the defaults", d.delays)
	}
	v := reflect.ValueOf(want)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).Int() == 0 {
			t.Errorf("delays.%s has no default", v.Type().Field(i).Name)
		}
	}
}

// fastScale is how much faster the test loops run than production. One
// factor for every wait, so escalation counts (ten singleGone retries before
// the stall arm, say) and orderings are exactly production's.
const fastScale = 20

// fastDelays is defaultDelays() ÷ fastScale: 500 ms → 25 ms, 5 s → 250 ms.
func fastDelays() delays {
	d := defaultDelays()
	return delays{
		singleGoneRetry:        d.singleGoneRetry / fastScale,
		interruptionStallRetry: d.interruptionStallRetry / fastScale,
		transientFailureRetry:  d.transientFailureRetry / fastScale,
		genericRetry:           d.genericRetry / fastScale,
		hlsPlaylistRetry:       d.hlsPlaylistRetry / fastScale,
		hlsStuckRetry:          d.hlsStuckRetry / fastScale,
		connectivityPoll:       d.connectivityPoll / fastScale,
		atEdgeBackoffUnit:      d.atEdgeBackoffUnit / fastScale,
		hlsReloadUnit:          d.hlsReloadUnit / fastScale,
	}
}

// fast scales a test's own timing knob (a MaxTimeout, a ceiling) by the same
// factor the loop waits were scaled by, so the knob keeps its relationship to
// the loop.
func fast(d time.Duration) time.Duration { return d / fastScale }

// activityRecorder wires OnActivity to a buffered channel so a test can wait
// for the loop to REACH a state instead of sleeping a wall-clock margin and
// hoping. Non-blocking send: a test that stops reading never stalls the loop.
func activityRecorder(d *SegmentDownloader) <-chan DownloadActivity {
	ch := make(chan DownloadActivity, 1024)
	d.OnActivity = func(a DownloadActivity) {
		select {
		case ch <- a:
		default:
		}
	}
	return ch
}

// awaitActivity blocks until want is observed on ch or within elapses.
func awaitActivity(t *testing.T, ch <-chan DownloadActivity, want DownloadActivity, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case a := <-ch:
			if a == want {
				return
			}
		case <-deadline:
			t.Fatalf("downloader never reported %v within %v", want, within)
		}
	}
}

func TestFastDelaysKeepRatios(t *testing.T) {
	f, p := fastDelays(), defaultDelays()
	if f.interruptionStallRetry/f.singleGoneRetry != p.interruptionStallRetry/p.singleGoneRetry {
		t.Fatalf("stall:singleGone ratio changed: fast %v:%v, prod %v:%v", f.interruptionStallRetry, f.singleGoneRetry, p.interruptionStallRetry, p.singleGoneRetry)
	}
	if f.singleGoneRetry < 20*time.Millisecond {
		t.Fatalf("singleGoneRetry %v is below the 20 ms floor timer jitter makes unsafe", f.singleGoneRetry)
	}
}
```
Check `DownloaderOptions.OutputFile` is the field name `NewSegmentDownloader` reads (`downloader.go:687` `opts.ResumeFile = opts.OutputFile + ".resume.json"`) — it is.

- [ ] **Step 2: Run it** — `go test -count=1 -run 'TestDefaultDelaysMatchConstants|TestFastDelaysKeepRatios' ./internal/engine/` → FAIL to compile: `undefined: delays`, `defaultDelays`, `hlsPlaylistRetryDelay`, `hlsStuckRetryDelay`.

- [ ] **Step 3: Create `internal/engine/delays.go`**

```go
package engine

import "time"

// delays is every wait the download loops sleep on, in one place so tests can
// run the same loops at millisecond scale. Production values are the package
// constants named beside each field (pinned by TestDefaultDelaysMatchConstants),
// so the constants remain the documentation of intent and this struct is the
// only knob. Tests poke it directly after NewSegmentDownloader, the way they
// already poke MayResume and OnActivity; nothing outside the package sees it.
type delays struct {
	singleGoneRetry        time.Duration // singleGoneRetryDelay — one 410/404 while behind head
	interruptionStallRetry time.Duration // interruptionStallRetryDelay — the may-resume stall arm
	transientFailureRetry  time.Duration // transientFailureRetryDelay — 5xx/timeouts at the edge
	genericRetry           time.Duration // genericRetryDelay — manifest / unknown-status retries
	hlsPlaylistRetry       time.Duration // hlsPlaylistRetryDelay — playlist fetch or parse failed
	hlsStuckRetry          time.Duration // hlsStuckRetryDelay — a segment or the init keeps failing
	connectivityPoll       time.Duration // connectivityPollInterval — waitForConnectivity's ticker
	atEdgeBackoffUnit      time.Duration // the second the 429 backoff and same-head retry count in
	hlsReloadUnit          time.Duration // the second hlsReloadDelay scales playlist durations by
}

// defaultDelays returns production timing.
func defaultDelays() delays {
	return delays{
		singleGoneRetry:        singleGoneRetryDelay,
		interruptionStallRetry: interruptionStallRetryDelay,
		transientFailureRetry:  transientFailureRetryDelay,
		genericRetry:           genericRetryDelay,
		hlsPlaylistRetry:       hlsPlaylistRetryDelay,
		hlsStuckRetry:          hlsStuckRetryDelay,
		connectivityPoll:       connectivityPollInterval,
		atEdgeBackoffUnit:      time.Second,
		hlsReloadUnit:          time.Second,
	}
}
```

- [ ] **Step 4: New HLS constants** — in `internal/engine/downloader_hls.go`, beside the file's other top-level declarations (before `hlsReloadDelay`), add:

```go
const (
	// hlsPlaylistRetryDelay is the pause after a media-playlist fetch or
	// parse failure before the next attempt (bounded by the consecutive-error
	// budget at the call sites).
	hlsPlaylistRetryDelay = 5 * time.Second
	// hlsStuckRetryDelay is the pause after a segment or init-segment fetch
	// fails before the same one is retried (MaxSegmentRetries rounds, then
	// the skip-forward / ErrQualityLost logic decides).
	hlsStuckRetryDelay = 2 * time.Second
)
```

- [ ] **Step 5: The struct field and the constructor** — `internal/engine/downloader.go`: add to `SegmentDownloader` (after `cipherFailureFired atomic.Bool`, `:345`):
```go
	// delays is every wait the loops sleep on; defaultDelays() in production,
	// fastDelays() in tests (see delays.go).
	delays delays
```
In `NewSegmentDownloader` (`:695-698`) the literal becomes:
```go
	d := &SegmentDownloader{
		opts:   opts,
		logger: logger,
		delays: defaultDelays(),
	}
```

- [ ] **Step 6: Production reads** — each replacement is one token; grep the anchor, keep everything else on the line:

| File:line | Before | After |
|---|---|---|
| `downloader_dash.go:351` | `utils.Sleep(ctx, genericRetryDelay)` | `utils.Sleep(ctx, d.delays.genericRetry)` |
| `:474`, `:483`, `:530` | `utils.Sleep(ctx, singleGoneRetryDelay)` | `utils.Sleep(ctx, d.delays.singleGoneRetry)` |
| `:491`, `:742` | `utils.Sleep(ctx, interruptionStallRetryDelay)` | `utils.Sleep(ctx, d.delays.interruptionStallRetry)` |
| `:636` | `utils.Sleep(ctx, transientFailureRetryDelay)` | `utils.Sleep(ctx, d.delays.transientFailureRetry)` |
| `:593` | `backoff := min(time.Duration(int64(1)<<uint(shift))*time.Second, time.Duration(delayCap)*time.Second)` | `backoff := min(time.Duration(int64(1)<<uint(shift))*d.delays.atEdgeBackoffUnit, time.Duration(delayCap)*d.delays.atEdgeBackoffUnit)` |
| `:755` | `utils.Sleep(ctx, time.Duration(*sameHeadRetryDelay)*time.Second)` | `utils.Sleep(ctx, time.Duration(*sameHeadRetryDelay)*d.delays.atEdgeBackoffUnit)` |
| `:427`, `:665`, `:728` | `waitForConnectivity(ctx, d.opts.IsOnline)` | `waitForConnectivity(ctx, d.opts.IsOnline, d.delays.connectivityPoll)` |
| `downloader_fetch.go:286` | `utils.Sleep(ctx, singleGoneRetryDelay<<attempt)` | `utils.Sleep(ctx, d.delays.singleGoneRetry<<attempt)` |
| `downloader_hls.go:121` | `waitForConnectivity(ctx, d.opts.IsOnline)` | `waitForConnectivity(ctx, d.opts.IsOnline, d.delays.connectivityPoll)` |
| `:295`, `:343` | `utils.Sleep(ctx, 5*time.Second)` | `utils.Sleep(ctx, d.delays.hlsPlaylistRetry)` |
| `:541`, `:590` | `utils.Sleep(ctx, 2*time.Second)` | `utils.Sleep(ctx, d.delays.hlsStuckRetry)` |
| `:714` | `utils.Sleep(ctx, hlsReloadDelay(lastSegDur, pl.TargetDuration, len(newSegments) > 0, time.Since(loadTime)))` | `utils.Sleep(ctx, hlsReloadDelay(lastSegDur, pl.TargetDuration, len(newSegments) > 0, time.Since(loadTime), d.delays.hlsReloadUnit))` |

If any of these sites is a method on a different receiver name (e.g. inside a helper without `d`), say so in the report and thread the value through as a parameter rather than reaching for a package variable. Confirm with `grep -n 'singleGoneRetryDelay\|interruptionStallRetryDelay\|transientFailureRetryDelay\|genericRetryDelay\|5\*time.Second\|2\*time.Second' internal/engine/*.go | grep -v _test` that the only remaining mentions are the constant declarations, `defaultDelays()`, and comments.

- [ ] **Step 7: `hlsReloadDelay` takes its unit** — `downloader_hls.go:143-160`: signature `func hlsReloadDelay(lastSegDur, targetDur float64, hadNewSegments bool, elapsed, unit time.Duration) time.Duration`; body line `remain := time.Duration(interval*float64(time.Second)) - elapsed` → `remain := time.Duration(interval*float64(unit)) - elapsed`. Doc comment gains: `unit is the duration one playlist second maps to — time.Second in production, scaled down by tests (delays.hlsReloadUnit).` Update every call in `downloader_hls_reload_test.go` to pass `time.Second` as the last argument (behaviour unchanged).

- [ ] **Step 8: `waitForConnectivity` takes its poll** — `connectivity_wait.go:52-66`: signature `func waitForConnectivity(ctx context.Context, isOnline func() bool, poll time.Duration) error`; `ticker := time.NewTicker(connectivityPollInterval)` → `ticker := time.NewTicker(poll)`. Doc comment gains one sentence: `poll is how often isOnline is re-asked (connectivityPollInterval in production; tests pass milliseconds).` Update the calls in `connectivity_wait_test.go` to pass `connectivityPollInterval` for now (Task 2 rescales that test).

- [ ] **Step 9: Gates**

```bash
cd D:/Git/Moombox/.worktrees/improvement-e-engine-delays
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
gofmt -l ./internal && go build ./... && go vet ./... && staticcheck ./... && echo STATIC-OK
go test -count=1 -run 'TestDefaultDelaysMatchConstants|TestFastDelaysKeepRatios|TestHlsReloadDelay|TestWaitForConnectivity' -v ./internal/engine/ | grep -E '^(--- |ok|FAIL)'
go test -count=1 ./internal/engine/
```
Expected: `STATIC-OK`; every listed test `--- PASS`; the package `ok` (still ~53 s — nothing scaled yet; that is Tasks 2–3).

Mutant (record it): change `atEdgeBackoffUnit: time.Second` in `defaultDelays()` to `time.Millisecond` → `TestDefaultDelaysMatchConstants` FAILS; revert.

- [ ] **Step 10: Commit**

```
refactor(engine): every loop wait reads from one delays struct

SegmentDownloader.delays holds the nine waits the download loops sleep on,
defaulting to the constants that documented them; waitForConnectivity and
hlsReloadDelay take their unit as a parameter. Production timing is pinned
by TestDefaultDelaysMatchConstants. Tests get fastDelays() (÷20, ratios
kept) and an OnActivity recorder to wait for states instead of seconds.
```

---

### Task 2: Interruption and connectivity tests run at fast scale

**Files:**
- Modify: `internal/engine/downloader_interruption_test.go` (9 tests: `:49` BackstopStallsWhileMayResume, `:146` BackstopCeilingExpires, `:217` NilMayResumeByteCompat, `:270` ConfirmedEndedIgnoresMayResume, `:340` GoneErrorStallReachedViaStatusCheckError, `:515` InterruptionNoStallPromptFinalize, `:590` InterruptionNoStallNoEvidenceFinalizesNormally, plus the two others in the file if any)
- Modify: `internal/engine/connectivity_wait_test.go:21-30` (WaitsAndReturns)

**Interfaces:** Consumes `fastDelays()`, `fast()`, `activityRecorder`, `awaitActivity` (Task 1).

The recipe, applied to EVERY test in the file that starts a downloader (read each test top to bottom before editing it):

1. Immediately after `d := NewSegmentDownloader(DownloaderOptions{...})` add `d.delays = fastDelays()`.
2. Scale the test's own timing knobs by `fast(...)`: the `maxTimeout` constants (`:60,157,228,351,526,601`, all 1–2 s → 50–100 ms) become `maxTimeout := fast(2 * time.Second)` (keep the same pre-scale number the test used); the `ceiling := 2*time.Second` at `:158` → `fast(2 * time.Second)`; any `InterruptionTimeout:` option → `fast(<same>)`.
3. **Replace wall-clock "still running" waits with a state wait.** At `:96` (`case <-time.After(6 * time.Second):` in BackstopStallsWhileMayResume) and `:384` (`9 * time.Second` in GoneErrorStallReachedViaStatusCheckError): install `act := activityRecorder(d)` BEFORE `go d.Start(...)` (merge with the test's existing `OnActivity` closure — if it records `sawWaitingResume`, drop that flag and read the channel instead), then replace the whole `select { case err := <-done: t.Fatalf(...) ; case <-time.After(...): }` with:
   ```go
   	// The stall arm is engaged once the loop reports ActivityWaitingResume;
   	// waiting for the event replaces the old fixed-seconds margin.
   	awaitActivity(t, act, ActivityWaitingResume, 5*time.Second)
   	select {
   	case err := <-done:
   		t.Fatalf("Start returned (err=%v) while MayResume was still true -- the stall did not hold", err)
   	default:
   	}
   ```
   (5 s is a safety net, never reached when the loop works; it does not add wall time.)
4. Scale the release bounds: `case <-time.After(20 * time.Second):` (`:185,:304`), `15*time.Second` (`:553,:624`), `maxTimeout + 5*time.Second` (`:245`), and `:110,:397` become `time.After(5 * time.Second)` safety nets. The elapsed *ceilings* (`:308` `> 15s`, `:563` `> 9s`, `:627` `> 9s`) become `> 2*time.Second` (still "released promptly" at fast scale: the loop's whole escalation is now ~0.3 s). The elapsed *floors* at `:190` and `:250` (`elapsed < maxTimeout` style — "did not return instantly") keep their comparison against the now-scaled `maxTimeout`.
5. Any comment that names a production duration in seconds ("the 5s stall-sleep cycle", "~5.5s escalation") gets rewritten in terms of the wait's NAME (`interruptionStallRetry`, `goneRetryDuringDownload × singleGoneRetry`) so it stays true at both scales.

`connectivity_wait_test.go:21-30` `TestWaitForConnectivity_WaitsAndReturns`: pass `poll := 50 * time.Millisecond` as the third argument, flip `online` after `20 * time.Millisecond` instead of 200 ms, and assert the wait returned within `time.Second` (safety net). Any other call in that file passes `connectivityPollInterval` unchanged.

- [ ] **Step 1: Baseline** — `go test -count=1 -v -run 'Backstop|MayResume|Interruption|GoneErrorStall|ConfirmedEnded|WaitForConnectivity' ./internal/engine/ | grep -E '^--- (PASS|FAIL)'` and record the wall times (expect ~10, 10, 6, 5, 5, 5, 5, 3 s).

- [ ] **Step 2: Apply the recipe** to every test named above. Run the same command after each file save; every test must stay PASS and its time must drop to well under 1 s (the two stall tests to ~0.3–0.6 s).

- [ ] **Step 3: Prove the state waits are real** — mutant: in `awaitActivity`, temporarily make the deadline `time.After(0)` → the two converted tests FAIL with "never reported ActivityWaitingResume"; revert. Second mutant: in `fastDelays()` temporarily return `defaultDelays()` → the file's tests still PASS but take their old seconds again (proves the speed comes from the delays, not from weakened assertions); revert. Record both.

- [ ] **Step 4: Gates**

```bash
gofmt -l ./internal && go vet ./internal/engine/ && staticcheck ./internal/engine/
go test -count=3 -run 'Backstop|MayResume|Interruption|GoneErrorStall|ConfirmedEnded|WaitForConnectivity' ./internal/engine/
go test -count=1 ./internal/engine/
```
Expected: three consecutive `ok` runs (no flake); the package still `ok`, now roughly 53 s minus ~50 s ≈ under 15 s (Task 3 takes the rest).

- [ ] **Step 5: Commit**

```
test(engine): interruption and connectivity tests run at fast scale

The nine interruption tests and the connectivity wait poke fastDelays() and
wait for ActivityWaitingResume instead of a fixed six or nine seconds; their
own MaxTimeout and ceiling knobs scale by the same factor so escalation
counts are production's. ~50 s of sleeping gone.
```

---

### Task 3: DASH, HLS and 403 tests run at fast scale

**Files:**
- Modify: `internal/engine/downloader_dash_integration_test.go` (`:105` CleanPostLiveEnd, `:142` TransientBurstRecovers, `:176` BehindHeadBudgetExhaustionWarns, `:239` RecoversFromCredentialExpiry)
- Modify: `internal/engine/downloader_hls_fmp4_test.go` (`:293` StuckSegmentSkipsNotSplitCycle, `:359` RevertsToTSSplitsPart, `:510` ResumeDoesNotRewriteInit)
- Modify: `internal/engine/downloader_hls_maxtimeout_test.go` (`:37` ForcesFinalize, `:73` NoEnforceRespectsStatusCheck, `:110` PausesForOfflineOutage)
- Modify: `internal/engine/downloader_hls_gap_test.go:22` (StopOnGapReturnsErrGapDetected)
- Modify: `internal/engine/downloader_fetch_403_test.go:235` (ForbiddenBehindHeadExhaustsToPermanent)

**Interfaces:** Consumes Task 1's helpers.

Recipe per test: `d.delays = fastDelays()` right after construction (for `ResumeDoesNotRewriteInit` both `d1` and `d2`; for `ForbiddenBehindHeadExhaustsToPermanent`, which calls `d.fetchSegmentWithRetry` directly, the poke goes before that call). Then scale the test's own knobs:

- `downloader_dash_integration_test.go:195` `MaxTimeout: 7 * time.Second` → `MaxTimeout: fast(7 * time.Second)` and `:205` `elapsed < 6*time.Second` → `elapsed < fast(6 * time.Second)` — read the surrounding comment; this test asserts the loop defers within its budget, and the budget and the wait shrink together so the iteration count is unchanged (10 pre-escalation + ~4). If the assertion flakes under `-count=3`, widen only the ceiling (`fast(6 s)` → `fast(6 s) + 100*time.Millisecond`) and say so.
- `downloader_hls_maxtimeout_test.go:47,85` `MaxTimeout: 300 * time.Millisecond` → `fast(300 * time.Millisecond)` is 15 ms — too close to timer jitter; use `MaxTimeout: 60 * time.Millisecond` and note the deviation from a pure ÷20; `:149` `MaxTimeout: 3 * time.Second` → `fast(3 * time.Second)`; the outer `context.WithTimeout` safety nets (`:55,93,155`) stay as they are.
- Every other outer `context.WithTimeout(…, 20–60 s)` safety net stays.
- HLS playlists in these fixtures declare segment durations of 1.0 s; with `hlsReloadUnit` = 50 ms each reload cycle is 50 ms, so no fixture edit is needed.
- Comments naming seconds → wait names, as in Task 2.

- [ ] **Step 1: Baseline** — `go test -count=1 -v -run 'DashLoop|HlsLoop|HlsLive|Hls_FMP4|ForbiddenBehindHead' ./internal/engine/ | grep -E '^--- (PASS|FAIL)'` (expect 10, 7.5, 7, 6.5, 6, 5, 3, 2.5, 1, 1, 1, 1 s).

- [ ] **Step 2: Apply the recipe**; re-run after each file; every test PASS and under ~0.6 s.

- [ ] **Step 3: Mutant** — `fastDelays()` → `defaultDelays()` temporarily: the tests PASS at their old speeds (assertions intact); revert.

- [ ] **Step 4: Gates**

```bash
gofmt -l ./internal && go vet ./internal/engine/ && staticcheck ./internal/engine/
go test -count=3 -run 'DashLoop|HlsLoop|HlsLive|Hls_FMP4|ForbiddenBehindHead' ./internal/engine/
go test -count=1 ./internal/engine/
```
Expected: three `ok`; the package `ok` in **under 10 s** — paste the `ok … Ns` line.

- [ ] **Step 5: Commit**

```
test(engine): DASH, HLS and 403 loop tests run at fast scale

Twelve integration tests poke fastDelays(); their MaxTimeout knobs scale
with the waits. internal/engine drops under ten seconds.
```

---

### Task 4: Stability gate and documentation

**Files:**
- Modify: `docs/spec/architecture.md` (the engine / SegmentDownloader section — grep `SegmentDownloader` and `singleGoneRetryDelay` / `interruptionStallRetryDelay` to find where the retry timing is described)
- Modify: `docs/spec/appendix-metrics.md` only if it states the engine package's test time (grep `engine`); otherwise untouched.

- [ ] **Step 1: Stability** — `go test -count=5 ./internal/engine/ 2>&1 | tail -5`: five `ok` lines, each under 10 s. If any run fails, the flaky test is a Task 2/3 defect: report it with the `-v` output as BLOCKED rather than loosening an assertion.

- [ ] **Step 2: Race** — `go test -count=1 -race ./internal/engine/` if the toolchain supports it on this host (Windows needs a C toolchain for `-race`; if `go test -race` errors with a cgo/gcc message, record that literal error and skip — the controller runs the ubuntu leg's equivalent through CI).

- [ ] **Step 3: Docs** — in `docs/spec/architecture.md`'s engine section, after the sentence that names the retry constants (or, if none does, at the end of the `SegmentDownloader` description), add: `` Every wait the loops sleep on is a field of the unexported `delays` struct (`internal/engine/delays.go`), defaulting to the named constants and pinned by `TestDefaultDelaysMatchConstants`; the engine tests poke `fastDelays()` (÷20) so `go test ./internal/engine/` runs in seconds while production timing is untouched. `` Run `go test -count=1 ./internal/docs/` (the citation test resolves `delays.go`, `delays`, `fastDelays`, `TestDefaultDelaysMatchConstants`).

- [ ] **Step 4: Gates** — `gofmt -l ./internal`, `go vet ./...`, `staticcheck ./...`, `go test -count=1 ./internal/docs/ ./internal/engine/`.

- [ ] **Step 5: Commit**

```
docs(spec): the engine's delays struct and fast-scale tests

architecture.md names the one knob the loop waits read from and how the
tests use it; five consecutive engine runs under ten seconds recorded in the
arc ledger.
```

---

## Self-Review

**Spec coverage (§7):** `delays` struct on `SegmentDownloader` ✔ (Task 1; the spec's six fields plus `transientFailureRetry`, `genericRetry`, `hlsPlaylistRetry` — the survey shows tests waiting on the first and the other two are the same shape; `hlsReloadFloor` is realised as `hlsReloadUnit`, the second `hlsReloadDelay` multiplies playlist durations by, which is the only way to scale a duration the playlist dictates). Defaults from the constants in `NewSegmentDownloader` ✔. Eleven call sites read `d.delays.X` ✔ (Task 1 table: 14 sites, the spec's eleven plus the three same-shape waits). `waitForConnectivity` takes the poll (4 callers) ✔. Slow tests set millisecond delays and rescale their bounds; `:96` and `:384` switch to the `OnActivity` hook ✔ (Tasks 2–3; 21 tests, not eight — the survey found more). Pin test ✔. Target under 10 s ✔ (Task 3 Step 4, Task 4 Step 1).

**Placeholders:** none — every edit names its before/after; tests give code or an exact recipe with line numbers and target values.

**Type consistency:** `delays` field names identical in `delays.go`, the pin test, `fastDelays()` and the Task 1 table; `waitForConnectivity(ctx, isOnline, poll)` and `hlsReloadDelay(..., elapsed, unit)` used with the same arity everywhere; `activityRecorder`/`awaitActivity`/`fast` defined in Task 1, consumed in Tasks 2–3.
