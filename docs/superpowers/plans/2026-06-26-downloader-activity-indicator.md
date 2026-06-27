# Downloader Activity Indicator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the frozen progress line during downloader wait windows with a reason-specific, ticking message ("Verifying stream ended… (1m 20s)", etc.) so a verifying/waiting download reads as working, not hung.

**Architecture:** The engine `SegmentDownloader` reports a typed `DownloadActivity` (the *reason* it is not pulling segments) via a new `OnActivity` callback, fired from the existing wait points in the DASH and HLS loops. The worker `ProgressTracker` maps the activity to a human message via a pure `activityMessage` formatter, writes it to the existing `progress` field, and blanks `speed`/`eta`; a real segment event (`OnProgress`) reverts to the normal counter. The YouTube/Twitch orchestrators reuse the same formatter for their own verification-sleep windows.

**Tech Stack:** Go 1.26, no new deps. Reuses `utils.FormatDurationHuman`. No new `JobStatus` or DB column.

**Spec:** `docs/superpowers/specs/2026-06-26-downloader-activity-indicator-design.md`

---

## File Structure

- `internal/engine/downloader.go` — MODIFY: add `DownloadActivity` type + consts, `OnActivity` callback field, `emitActivity` helper.
- `internal/engine/downloader_activity_test.go` — CREATE: tests for `emitActivity`.
- `internal/engine/downloader_dash.go` — MODIFY: emit activities at DASH wait points.
- `internal/engine/downloader_dash_activity_test.go` — CREATE: tests for DASH handler emits.
- `internal/engine/downloader_hls.go` — MODIFY: emit activities at HLS wait points.
- `internal/worker/progress.go` — MODIFY: `activityMessage` formatter, `setActivity`, activity fields, clear-on-progress wiring.
- `internal/worker/progress_test.go` — MODIFY: add `activityMessage` tests.
- `internal/worker/orchestrator_youtube.go` — MODIFY: unify the verify-sleep message via `activityMessage`.
- `internal/worker/orchestrator_twitch.go` — MODIFY: same parity (if it has an equivalent verify-sleep message).

---

## Task 1: Engine activity type, callback, and emit helper

**Files:**
- Modify: `internal/engine/downloader.go`
- Test: `internal/engine/downloader_activity_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/engine/downloader_activity_test.go`:

```go
package engine

import "testing"

func TestEmitActivityCallsCallback(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{OutputFile: "x"})
	got := ActivityNone
	d.OnActivity = func(a DownloadActivity) { got = a }

	d.emitActivity(ActivityVerifyingEnd)

	if got != ActivityVerifyingEnd {
		t.Errorf("emitActivity delivered %v, want ActivityVerifyingEnd", got)
	}
}

func TestEmitActivityNilSafe(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{OutputFile: "x"})
	d.OnActivity = nil
	// Must not panic when no callback is registered.
	d.emitActivity(ActivityRateLimited)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestEmitActivity`
Expected: FAIL — `undefined: ActivityNone`, `undefined: DownloadActivity`, `d.OnActivity undefined`, `d.emitActivity undefined`.

- [ ] **Step 3: Add the type, consts, callback field, and helper**

In `internal/engine/downloader.go`, add the type + consts just above `// DownloadProgress holds progress information` (near line 108):

```go
// DownloadActivity describes what the downloader is currently WAITING ON when
// it is not actively pulling segments. The worker maps it to a human-readable
// progress-line message so a verifying/waiting download doesn't read as frozen.
type DownloadActivity int

const (
	ActivityNone                DownloadActivity = iota // actively downloading
	ActivityVerifyingEnd                                // segments stopped; confirming the stream ended
	ActivityReconnecting                                // connectivity lost; waiting for the network
	ActivityRateLimited                                 // 429 backoff
	ActivityFindingFirstSegment                         // pre-first-byte hunt for the first valid segment
)
```

In the `SegmentDownloader` callbacks block, after `OnCipherFailure func() string` (near line 229), add:

```go
	// OnActivity reports the downloader's current wait reason (or
	// ActivityNone when it resumes downloading). Optional; nil to opt out.
	OnActivity func(a DownloadActivity)
```

Add the helper next to `emitHealthUpdate` (after it, near line 276):

```go
// emitActivity reports the current wait reason to OnActivity. Nil-callback safe.
func (d *SegmentDownloader) emitActivity(a DownloadActivity) {
	if d.OnActivity != nil {
		d.OnActivity(a)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/engine/ -run TestEmitActivity -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/downloader.go internal/engine/downloader_activity_test.go
git commit -m "feat(engine): add DownloadActivity type and OnActivity callback"
```

---

## Task 2: Emit activities at the DASH loop wait points

**Files:**
- Modify: `internal/engine/downloader_dash.go` (`handleGoneError`, `handleRateLimitError`, `handleHTTPError`)
- Test: `internal/engine/downloader_dash_activity_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/engine/downloader_dash_activity_test.go`:

```go
package engine

import (
	"context"
	"path/filepath"
	"testing"
)

func newActivityDownloader(t *testing.T) (*SegmentDownloader, *DownloadActivity) {
	t.Helper()
	d := NewSegmentDownloader(DownloaderOptions{OutputFile: filepath.Join(t.TempDir(), "v")})
	got := ActivityNone
	d.OnActivity = func(a DownloadActivity) { got = a }
	return d, &got
}

func TestHandleGoneErrorEmitsFindingFirstSegment(t *testing.T) {
	d, got := newActivityDownloader(t)
	n := 1 // first-segment hunt: !hasStartedDownloading, n <= goneRetryBeforeFirstSegment
	if err := d.handleGoneError(context.Background(), &n, false); err != nil {
		t.Fatalf("handleGoneError returned %v, want nil (continue)", err)
	}
	if *got != ActivityFindingFirstSegment {
		t.Errorf("activity = %v, want ActivityFindingFirstSegment", *got)
	}
}

func TestHandleGoneErrorEmitsVerifyingEnd(t *testing.T) {
	d, got := newActivityDownloader(t)
	n := goneRetryDuringDownload + 1 // sustained gones while downloading
	// IsOnline nil + CheckStreamStatus nil -> emits VerifyingEnd, then declares ended.
	if err := d.handleGoneError(context.Background(), &n, true); err != errStreamDone {
		t.Fatalf("handleGoneError returned %v, want errStreamDone", err)
	}
	if *got != ActivityVerifyingEnd {
		t.Errorf("activity = %v, want ActivityVerifyingEnd", *got)
	}
}

func TestHandleRateLimitErrorEmitsRateLimited(t *testing.T) {
	d, got := newActivityDownloader(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled so the backoff sleep returns immediately
	delay := 0
	_ = d.handleRateLimitError(ctx, &delay, 60)
	if *got != ActivityRateLimited {
		t.Errorf("activity = %v, want ActivityRateLimited", *got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine/ -run 'TestHandleGoneError|TestHandleRateLimit'`
Expected: FAIL — activity stays `ActivityNone` (emits not added yet).

- [ ] **Step 3: Add the emit calls**

In `internal/engine/downloader_dash.go`, `handleGoneError` — add three emits:

```go
func (d *SegmentDownloader) handleGoneError(ctx context.Context, consecutiveGoneErrors *int, hasStartedDownloading bool) error {
	*consecutiveGoneErrors++

	if hasStartedDownloading && *consecutiveGoneErrors > goneRetryDuringDownload {
		if d.opts.IsOnline != nil && !d.opts.IsOnline() {
			d.emitActivity(ActivityReconnecting) // ADD
			d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
			if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
				return err
			}
			*consecutiveGoneErrors = 0
			return nil // Continue loop
		}
		d.emitActivity(ActivityVerifyingEnd) // ADD
		// Check if stream is actually ended, or if our format just disappeared
		if d.opts.CheckStreamStatus != nil {
			ended, checkErr := d.opts.CheckStreamStatus(ctx)
			if checkErr != nil {
				d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
			} else if !ended {
				return ErrQualityLost
			}
		}
		d.streamEnded.Store(true)
		return errStreamDone
	}
	if !hasStartedDownloading && *consecutiveGoneErrors <= goneRetryBeforeFirstSegment {
		d.emitActivity(ActivityFindingFirstSegment) // ADD
		d.currentSeq.Add(1)
		utils.Sleep(ctx, firstSegmentHuntDelay)
		return nil // Continue loop
	}
	if !hasStartedDownloading && *consecutiveGoneErrors > goneRetryBeforeFirstSegment {
		return errStreamDone // Failed to find valid starting segment
	}
	// Single GONE while downloading -- small delay before retry (kept silent;
	// a one-off hiccup must not flash "verifying ended").
	utils.Sleep(ctx, singleGoneRetryDelay)
	return nil // Continue loop
}
```

In `handleRateLimitError`, add the emit just before the backoff sleep:

```go
func (d *SegmentDownloader) handleRateLimitError(ctx context.Context, sameHeadRetryDelay *int, delayCap int) error {
	*sameHeadRetryDelay++
	if *sameHeadRetryDelay > delayCap {
		*sameHeadRetryDelay = delayCap
	}
	const maxShift = 6
	shift := max(*sameHeadRetryDelay-1, 0)
	if shift > maxShift {
		shift = maxShift
	}
	backoff := min(time.Duration(int64(1)<<uint(shift))*time.Second, time.Duration(delayCap)*time.Second)
	d.emitActivity(ActivityRateLimited) // ADD
	d.logger.Warn("segment download rate-limited (429), backing off", "seq", d.currentSeq.Load(), "delay", backoff)
	utils.Sleep(ctx, backoff)
	return nil // Continue loop
}
```

In `handleHTTPError`, add emits in the offline branches and before the at-edge backoff sleep. The three `!IsOnline()` blocks (the threshold check, the cap check, and the no-segment-timeout block) each get `d.emitActivity(ActivityReconnecting)` as their first line; and add `d.emitActivity(ActivityVerifyingEnd)` immediately before the final `utils.Sleep(ctx, time.Duration(*sameHeadRetryDelay)*time.Second)` near the end of the function:

```go
	// At/past live edge and waiting — surface that we're verifying, not stalled.
	d.emitActivity(ActivityVerifyingEnd) // ADD (immediately before the sleep)
	utils.Sleep(ctx, time.Duration(*sameHeadRetryDelay)*time.Second)
	return nil // Continue loop
```

And each offline block in `handleHTTPError`, e.g.:

```go
		if d.opts.IsOnline != nil && !d.opts.IsOnline() {
			d.emitActivity(ActivityReconnecting) // ADD (first line of each offline block)
			d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
			...
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/engine/ -run 'TestHandleGoneError|TestHandleRateLimit' -v`
Expected: PASS (all three).

- [ ] **Step 5: Run the full engine suite for regressions**

Run: `go test ./internal/engine/`
Expected: `ok` — no behavior change to the loops, only added emits.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/downloader_dash.go internal/engine/downloader_dash_activity_test.go
git commit -m "feat(engine): emit wait activities from the DASH download loop"
```

---

## Task 3: Emit activities at the HLS loop wait points

**Files:**
- Modify: `internal/engine/downloader_hls.go` (`runHlsLoop`)

HLS has no distinct 429 handler (rate limiting flows through the generic error path) and no first-segment hunt (it seeds `currentSeq` from the playlist), so only `Reconnecting` and `VerifyingEnd` apply.

- [ ] **Step 1: Add Reconnecting emits to the offline branches**

In `internal/engine/downloader_hls.go`, each `if d.opts.IsOnline != nil && !d.opts.IsOnline()` block (playlist 404/410 at ~line 49, the `consecutiveErrors > 5` block at ~line 70, the parse-failure block at ~line 117, and the stale block at ~line 327) gets `d.emitActivity(ActivityReconnecting)` as its first line, e.g.:

```go
				if d.opts.IsOnline != nil && !d.opts.IsOnline() {
					d.emitActivity(ActivityReconnecting) // ADD
					d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
					if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
						return err
					}
					consecutiveErrors = 0
					continue
				}
```

- [ ] **Step 2: Add a VerifyingEnd emit at the stale-no-segments status check**

In the stale-detection block (`if len(newSegments) == 0 { staleCount++; if staleCount >= 5 {`), after the offline branch and immediately before the `CheckStreamStatus` call (~line 335), add:

```go
				d.emitActivity(ActivityVerifyingEnd) // ADD — segments stopped, confirming end
				if d.opts.CheckStreamStatus != nil {
					ended, _ := d.opts.CheckStreamStatus(ctx)
					if ended {
						d.streamEnded.Store(true)
						return nil
					}
				}
```

Do NOT add an emit to the normal `utils.Sleep(ctx, targetDur...)` at the end of the loop — that is routine live polling, not a wait window.

- [ ] **Step 3: Build and run the engine suite**

Run: `go build ./internal/engine/ && go test ./internal/engine/`
Expected: builds; `ok`. (End-to-end HLS emit assertions need the playlist-server harness in `downloader_hls_gap_test.go`; the emit calls reuse the Task 1-tested `emitActivity`, so a build + no-regression run is the gate here.)

- [ ] **Step 4: Commit**

```bash
git add internal/engine/downloader_hls.go
git commit -m "feat(engine): emit reconnecting/verifying activities from the HLS loop"
```

---

## Task 4: Worker activity formatter + ProgressTracker rendering

**Files:**
- Modify: `internal/worker/progress.go`
- Test: `internal/worker/progress_test.go`

- [ ] **Step 1: Write the failing test for the pure formatter**

Add to `internal/worker/progress_test.go`:

```go
func TestActivityMessage(t *testing.T) {
	elapsed := 80 * time.Second // FormatDurationHuman -> "1m 20s"
	cases := []struct {
		a    engine.DownloadActivity
		want string
	}{
		{engine.ActivityVerifyingEnd, "Verifying stream ended... (1m 20s)"},
		{engine.ActivityReconnecting, "Connection lost - reconnecting... (1m 20s)"},
		{engine.ActivityRateLimited, "Rate-limited - backing off... (1m 20s)"},
		{engine.ActivityFindingFirstSegment, "Waiting for first segment... (1m 20s)"},
		{engine.ActivityNone, ""},
	}
	for _, c := range cases {
		if got := activityMessage(c.a, elapsed); got != c.want {
			t.Errorf("activityMessage(%v) = %q, want %q", c.a, got, c.want)
		}
	}
}
```

Add the `engine` import to `progress_test.go` if not present:

```go
	"github.com/vampiricwulf/Moombox/internal/engine"
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/worker/ -run TestActivityMessage`
Expected: FAIL — `undefined: activityMessage`.

- [ ] **Step 3: Implement the formatter**

In `internal/worker/progress.go`, add (uses the already-imported `engine` and `utils`):

```go
// activityMessage renders a downloader wait reason into the progress-line text.
// ASCII punctuation matches the codebase's existing "..." progress strings.
func activityMessage(a engine.DownloadActivity, elapsed time.Duration) string {
	e := utils.FormatDurationHuman(elapsed)
	switch a {
	case engine.ActivityVerifyingEnd:
		return fmt.Sprintf("Verifying stream ended... (%s)", e)
	case engine.ActivityReconnecting:
		return fmt.Sprintf("Connection lost - reconnecting... (%s)", e)
	case engine.ActivityRateLimited:
		return fmt.Sprintf("Rate-limited - backing off... (%s)", e)
	case engine.ActivityFindingFirstSegment:
		return fmt.Sprintf("Waiting for first segment... (%s)", e)
	default:
		return ""
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/worker/ -run TestActivityMessage -v`
Expected: PASS.

- [ ] **Step 5: Add activity state + setActivity + clear-on-progress wiring**

In `internal/worker/progress.go`, add fields to the `ProgressTracker` struct (after `gaps`):

```go
	activity          engine.DownloadActivity // current wait reason (ActivityNone = downloading)
	activityStart     time.Time               // when the current activity began (for elapsed)
	lastActivityWrite time.Time               // throttle for activity DB writes
```

Add a const next to `progressPersistInterval`:

```go
	activityUpdateInterval = 1 * time.Second // throttle activity progress-line writes
```

Add the `setActivity` method (after `SetChatCount`):

```go
// setActivity surfaces a downloader wait reason in the progress line, blanking
// speed/eta since nothing is downloading. ActivityNone clears the state so the
// next OnProgress restores the normal counter. DB writes are throttled.
func (pt *ProgressTracker) setActivity(a engine.DownloadActivity) {
	pt.mu.Lock()
	if a == engine.ActivityNone {
		pt.activity = engine.ActivityNone
		pt.mu.Unlock()
		return
	}
	now := time.Now()
	if a != pt.activity {
		pt.activity = a
		pt.activityStart = now
	}
	if now.Sub(pt.lastActivityWrite) < activityUpdateInterval {
		pt.mu.Unlock()
		return
	}
	pt.lastActivityWrite = now
	msg := activityMessage(a, now.Sub(pt.activityStart))
	pt.mu.Unlock()

	pt.db.UpdateJobFields(pt.jobID, map[string]any{
		"progress": msg,
		"speed":    "",
		"eta":      "",
	})
}
```

Wire `OnActivity` in BOTH `AttachVideoDownloader` and `AttachAudioDownloader` (after the existing `dl.OnGap = ...` assignment in each):

```go
	dl.OnActivity = func(a engine.DownloadActivity) { pt.setActivity(a) }
```

Clear the activity on a real segment in BOTH `dl.OnProgress` handlers — add this as the FIRST line inside each `pt.mu.Lock()` block in `AttachVideoDownloader`/`AttachAudioDownloader`:

```go
		pt.activity = engine.ActivityNone // a real segment arrived; resume the normal counter
```

(The existing `maybeUpdate` call right after will rewrite `progress`/`speed`/`eta` from the segment counter, overwriting any activity message.)

- [ ] **Step 6: Build and run the worker suite**

Run: `go build ./internal/worker/ && go test ./internal/worker/`
Expected: builds; `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/worker/progress.go internal/worker/progress_test.go
git commit -m "feat(worker): render downloader wait activities in the progress line"
```

---

## Task 5: Unify the orchestrator verification message

**Files:**
- Modify: `internal/worker/orchestrator_youtube.go`
- Modify: `internal/worker/orchestrator_twitch.go` (if it has an equivalent verify-sleep message)

The orchestrator cancels the engine downloaders before its own 5-minute verification sleeps, so it must write the verifying message itself. Reuse `activityMessage` for consistent wording.

- [ ] **Step 1: Replace the static "Waiting for stream to end..." write**

In `internal/worker/orchestrator_youtube.go`, the `case youtube.StreamLive:` branch currently writes (near line 360):

```go
				o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
					"progress": "Waiting for stream to end...",
				})
```

Replace with the unified message (elapsed since the last segment), and blank speed/eta:

```go
				o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
					"progress": activityMessage(engine.ActivityVerifyingEnd, time.Since(time.Unix(0, lastSegTime.Load()))),
					"speed":    "",
					"eta":      "",
				})
```

Confirm `engine` and `time` are imported in `orchestrator_youtube.go` (both already used in this file).

- [ ] **Step 2: Set the message on the other verify-sleep branches**

Add the same write immediately before the two other `utils.Sleep(ctx, streamEndVerifyInterval)` calls in the verify loop — the `err != nil` retry (near line 337) and the refresh-failure retry (near line 383) — so those windows aren't silent either:

```go
			o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
				"progress": activityMessage(engine.ActivityVerifyingEnd, time.Since(time.Unix(0, lastSegTime.Load()))),
				"speed":    "",
				"eta":      "",
			})
			utils.Sleep(ctx, streamEndVerifyInterval)
```

- [ ] **Step 3: Twitch parity**

Open `internal/worker/orchestrator_twitch.go` and find its end-of-stream verification sleep(s) (search for `streamEndVerifyInterval` or an equivalent "waiting"/verify message). Apply the same `activityMessage(engine.ActivityVerifyingEnd, ...)` write before each verify sleep. If the Twitch path has no such loop, note that in the commit message and skip.

- [ ] **Step 4: Build and run the worker suite**

Run: `go build ./internal/worker/ && go test ./internal/worker/`
Expected: builds; `ok`.

- [ ] **Step 5: Full build + vet + test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add internal/worker/orchestrator_youtube.go internal/worker/orchestrator_twitch.go
git commit -m "feat(worker): unify orchestrator stream-end verification message"
```

---

## Self-Review notes (resolved)

- **Spec coverage:** all four activities emitted (Tasks 2-3), rendered (Task 4), and the orchestrator window unified (Task 5). VOD/direct path deliberately excluded (bounded fetch).
- **Type consistency:** `DownloadActivity` + `ActivityNone/VerifyingEnd/Reconnecting/RateLimited/FindingFirstSegment`, `OnActivity func(DownloadActivity)`, `emitActivity`, `activityMessage(engine.DownloadActivity, time.Duration) string`, `setActivity(engine.DownloadActivity)` — used identically across tasks.
- **Known caveat (from spec):** elapsed ticks at retry/check cadence (sub-second to ~60s at deepest 429 backoff); no per-second refresh ticker in this version.
- **Two-downloader edge:** last-writer-wins between counter and activity is cosmetic and self-corrects.
