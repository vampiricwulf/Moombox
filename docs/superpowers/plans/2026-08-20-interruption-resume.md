# Broadcast Interruption Resume — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** capture continues seamlessly across a live-broadcast interruption — stall finalize while resume is plausible, preserve resume data when finalizing anyway, auto-resume on monitor re-detection, and losslessly merge same-format parts.

**Architecture:** four valves around existing machinery (spec: `docs/superpowers/specs/2026-08-20-interruption-resume-design.md`). The engine gains a `MayResume` deferral at its two MaxTimeout finalize sites; the worker classifies (player-response interruption signature + chat-open) and preserves via the existing `incomplete_tail` path; the monitor's duplicate-drop gains an auto-`ResumeJob` arm; `finalizeMultiSegmentJob` gains an opportunistic concat-copy merge. Rider: cookie recovery fires on startup-dead auth.

**Tech stack:** Go, FFmpeg concat demuxer / ffprobe, existing fake-GVS test harness.

## Global Constraints

- Confirmed-ended (`CheckStreamStatus` → ended) finalizes IMMEDIATELY — chat-open must never delay a normal stream end.
- Chat closed ⇒ NO information (never an "ended" inference). Fetch errors ≠ closed.
- `MayResume == nil` (feature unwired) must be byte-identical to today's behavior.
- Never auto-resume Cancelled jobs. Never route auto-resume through Retry/Reinitialize (staging-destroying; see retry-vs-resume rule).
- Tier 4 merge is opportunistic: ANY probe disagreement or concat failure leaves parts exactly as today.
- All goroutines carry inline panic recovery (CLAUDE.md). Logger stays the anonymous 4-method interface.
- New config `downloader.interruption_timeout`: FlexDuration, default 120 (minutes = 2h), 0 disables Tier 1 (straight to Tier 2). Not restart-required.
- Run `gofmt -w` on touched packages; `go build ./... && go vet ./...` before every commit.

---

### Task 1: Chat `LiveContinuationOpen` accessor

**Files:**
- Modify: `internal/chat/downloader.go`
- Test: `internal/chat/downloader_livestate_test.go` (create)

**Interfaces:**
- Produces: `func (cd *ChatDownloader) LiveContinuationOpen() bool` — true from the first successful live poll until a definitive close; consumed by Task 4's closure.

- [ ] **Step 1: Write failing tests**

```go
package chat

import "testing"

// Truth table (spec "Signals"): open on live polls returning a
// continuation; closed on a definitive IsComplete/empty-continuation that
// recovery does not rescue; UNCHANGED on fetch errors; never open for
// replay or a downloader that never started.
func TestLiveContinuationOpen(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "x", OutputFile: "unused"})
	if cd.LiveContinuationOpen() {
		t.Fatal("never-started downloader must not report open")
	}
	cd.setLiveContinuationOpen(true)
	if !cd.LiveContinuationOpen() {
		t.Fatal("open after successful live poll")
	}
	cd.setLiveContinuationOpen(false)
	if cd.LiveContinuationOpen() {
		t.Fatal("closed after definitive end")
	}
}

func TestLiveContinuationOpenReplayNeverOpens(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "x", OutputFile: "unused", IsReplay: true})
	cd.noteLivePollResult(true) // even a "successful poll" on replay
	if cd.LiveContinuationOpen() {
		t.Fatal("replay chat must never report live-open")
	}
}
```

- [ ] **Step 2: Run** `go test -run TestLiveContinuation ./internal/chat/` — expect FAIL (undefined methods).

- [ ] **Step 3: Implement.** Add to the struct (near `streamEnded`, guarded by the existing `cd.mu`): `liveContinuationOpen bool`. Add:

```go
// LiveContinuationOpen reports whether the LIVE chat endpoint is still
// issuing continuations — the "chat is open" resume signal (interruption
// spec). Directional by design: true means the broadcast may resume; false
// means NOTHING (streamers disable chat independently, and a downloader
// that never started has no information). Fetch errors do not change it.
func (cd *ChatDownloader) LiveContinuationOpen() bool {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return cd.liveContinuationOpen
}

// setLiveContinuationOpen records the definitive open/closed state.
func (cd *ChatDownloader) setLiveContinuationOpen(open bool) {
	cd.mu.Lock()
	cd.liveContinuationOpen = open
	cd.mu.Unlock()
}

// noteLivePollResult is the runChatLoop hook: a successful LIVE poll with a
// continuation opens the signal; replay polls never do.
func (cd *ChatDownloader) noteLivePollResult(hasContinuation bool) {
	if cd.opts.IsReplay {
		return
	}
	if hasContinuation {
		cd.setLiveContinuationOpen(true)
	}
}
```

Wire into `runChatLoop`: after a successful `fetchOne` with `resp.NextContinuation != ""` and `!resp.IsComplete`, call `cd.noteLivePollResult(true)`. In the end-of-stream branch (`resp.IsComplete || resp.NextContinuation == ""`), call `cd.setLiveContinuationOpen(false)` **before** attempting `recoverStaleContinuation`; if recovery succeeds and polling resumes, the next successful poll re-opens it. `handleFetchError` paths: no state change. (Check the exact `cd.mu` lock name in the struct — if the mutex differs, use it.)

- [ ] **Step 4: Run** `go test ./internal/chat/` — PASS (whole package).
- [ ] **Step 5: Commit** `feat(chat): LiveContinuationOpen — the chat-open resume signal`

---

### Task 2: Config `downloader.interruption_timeout` (7-surface checklist)

**Files:**
- Modify: `internal/config/types.go`, `internal/config/config.go` (Defaults + validate), `internal/web/routes/config_routes.go` (validate + apply), `web/public/index.html` + `web/public/modules/settings.js`, `internal/tui/settings.go`
- Test: `internal/config/config_test.go` (extend existing patterns)

**Interfaces:**
- Produces: `cfg.Downloader.InterruptionTimeout config.FlexDuration` (minutes; `.Minutes()` accessor pattern — copy whichever accessor `FeedCheckInterval` uses). Task 4 consumes it.

- [ ] **Step 1:** `types.go` Downloader struct: `InterruptionTimeout FlexDuration \`toml:"interruption_timeout" json:"interruption_timeout"\`` with doc comment: how long finalize may stall waiting for an interrupted broadcast to resume; 0 disables the stall (Tier 2 preservation still applies).
- [ ] **Step 2:** `Defaults()`: `InterruptionTimeout: FlexDuration{Value: 120}`. `validate()`: negative → reset to default (mirror `ProbeCooldown`'s clamp style; 0 is legal).
- [ ] **Step 3:** API: `validateConfigUpdates` accepts float64 or duration string ≥ 0 (`downloader.interruption_timeout`); `applyConfigUpdates` applies via `config.ParseFlexDuration` (copy the existing FlexDuration field handling verbatim).
- [ ] **Step 4:** Web UI: `cfg-interruption-timeout` input (minutes, `min="0"`) beside the existing downloader fields; populate/gather/dirty-track exactly as `segment_workers` was wired. NOT in `RESTART_REQUIRED_FIELDS`.
- [ ] **Step 5:** TUI: `fieldNumber` in the Downloader section, load `.Minutes()`, apply `FlexDuration{Value: float64(v)}`, range `{0, math.MaxInt}`; help text mentions 0 = disabled.
- [ ] **Step 6:** Test: defaults row (120), validate resets negative, zero survives round-trip. Run `go test ./internal/config/... ./internal/web/...`.
- [ ] **Step 7: Commit** `feat(config): downloader.interruption_timeout (default 2h, 0 disables the resume stall)`

---

### Task 3: Engine — `MayResume` stall + interruption latch

**Files:**
- Modify: `internal/engine/downloader.go`, `internal/engine/downloader_dash.go`
- Test: `internal/engine/downloader_interruption_test.go` (create; reuse `newFakeGVS` from `downloader_dash_integration_test.go`)

**Interfaces:**
- Consumes: nothing new (callback assigned by worker like `OnProgress`).
- Produces: `SegmentDownloader.MayResume func() bool` (public field, callbacks block), `DownloaderOptions.InterruptionTimeout time.Duration` (0 = no ceiling gate — stall while MayResume true is then unbounded by the engine; worker passes the config value), `func (d *SegmentDownloader) FinalizedDuringInterruption() bool`, `ActivityWaitingResume DownloadActivity`.

- [ ] **Step 1: Failing integration test**

```go
// TestBackstopStallsWhileMayResume: budget-expired backstop with
// MayResume()==true must NOT finalize; when the callback flips false the
// next iteration finalizes, latching FinalizedDuringInterruption.
// Build on the fake GVS: serve segments 0..N, then permanent 403s with a
// CheckStreamStatus that returns (false, nil) [still live], MaxTimeout
// shrunk to ~2s via DownloaderOptions, MayResume flipping via atomic.Bool.
// Assert: (a) while mayResume true past MaxTimeout, Start() has not
// returned (poll with deadline); (b) after flipping false, Start returns
// nil, streamEnded/finalize path ran, FinalizedDuringInterruption()==true.
// Also TestBackstopCeilingExpires: InterruptionTimeout=1s, mayResume stays
// true → returns anyway within ~budget+ceiling+slack, latch true.
// Also TestNilMayResumeByteCompat: nil callback → identical to today
// (returns at MaxTimeout; latch FALSE).
// Also TestConfirmedEndedIgnoresMayResume: CheckStreamStatus returns
// (true, nil) → finalize immediately even with mayResume true; latch false.
```

Write these four as real tests (the fake-GVS + tiny-MaxTimeout pattern already exists in `downloader_dash_integration_test.go` — copy its downloader construction).

- [ ] **Step 2: Run** — FAIL (undefined field/method).

- [ ] **Step 3: Implement.** `downloader.go`:
  - struct: `finalizedDuringInterruption atomic.Bool`, `interruptionStallStart atomicTime`, callback field `MayResume func() bool` (doc: "reports whether the broadcast may resume — stall evidence. Called only from the download-loop goroutine. nil = feature off."), option `InterruptionTimeout time.Duration`.
  - accessor mirroring `FinalizedBehindHead`.
  - helper:

```go
// stallForPossibleResume reports whether a budget-expired finalize should
// defer because the broadcast may resume (interruption spec Tier 1). The
// FIRST true observation latches the stall clock; the configured ceiling
// (opts.InterruptionTimeout, 0 = no engine ceiling) bounds the total stall.
// A false MayResume (or expired ceiling) that follows a true observation
// latches finalizedDuringInterruption so the worker preserves resume data
// (Tier 2). Confirmed-ended callers must not consult this at all.
func (d *SegmentDownloader) stallForPossibleResume() bool {
	if d.MayResume == nil || !d.MayResume() {
		if !d.interruptionStallStart.Load().IsZero() {
			d.finalizedDuringInterruption.Store(true) // stalled earlier, evidence gone
		}
		return false
	}
	if d.interruptionStallStart.Load().IsZero() {
		d.interruptionStallStart.StoreNow()
		d.logger.Info("[Downloader] stream interrupted — deferring finalize while resume is plausible")
	}
	if d.opts.InterruptionTimeout > 0 && d.interruptionStallStart.Since() >= d.opts.InterruptionTimeout {
		d.finalizedDuringInterruption.Store(true)
		d.logger.Warn("[Downloader] interruption ceiling expired; finalizing with resume data preserved",
			"ceiling", d.opts.InterruptionTimeout)
		return false
	}
	return true
}
```

  - `downloader_dash.go`, TWO sites, both AFTER their existing behind-head/verdict checks so confirmed-ended still wins:
    1. `handleGoneError`: in the `!verdictKnown` budget-expired fallthrough — immediately before `if d.finalizeBehindHead()`, insert:

```go
			if d.stallForPossibleResume() {
				d.emitActivity(ActivityWaitingResume)
				utils.Sleep(ctx, singleGoneRetryDelay)
				return nil // Continue loop — the refresh path revives in place on resume
			}
```

    Guard it so a `verdictKnown` (confirmed-ended) finalize skips the stall: only invoke when `!d.streamEndVerified`.
    2. `handleHTTPError`'s MaxTimeout backstop: same insertion before `d.logger.Info("[Downloader] maximum timeout reached...")`, likewise gated on `!d.streamEndVerified`.
  - New activity: `ActivityWaitingResume` in the `DownloadActivity` const block (before the closing), comment "broadcast interrupted; deferring finalize while resume is plausible".

- [ ] **Step 4: Run** the four tests + `go test ./internal/engine/` — PASS.
- [ ] **Step 5: Commit** `feat(engine): stall budget-expired finalize while MayResume; latch interruption finalizes`

---

### Task 4: Worker — classification, wiring, Tier 2 preservation

**Files:**
- Modify: `internal/worker/strategies.go` (refresh observation), `internal/worker/job_context.go` or wherever `JobContext` is declared (find with grep; add pointer field), `internal/worker/orchestrator.go` (`ExecuteWithChat` closure + `finalizeIncompleteTail`), `internal/worker/orchestrator_youtube.go` (`attachProgress`), `internal/worker/progress.go` (activity message), strategy files if `JobConfig`→`DownloaderOptions` plumbing for `InterruptionTimeout` follows `SegmentWorkers` (mirror it exactly).
- Test: `internal/worker/interruption_test.go` (create), extend `internal/worker/progress_test.go` activity-message table.

**Interfaces:**
- Consumes: Task 1 `LiveContinuationOpen`, Task 2 config, Task 3 engine fields.
- Produces: `type interruptionSignal struct{ lastSeen atomicTime-equivalent }` shared via `JobContext` pointer (survives the `segCtx := *jobCtx` copies); `mayResume` closure installed on both downloaders in `attachProgress`.

- [ ] **Step 1: Failing tests**
  - `TestInterruptionSignalRecordsZeroFormatsLive`: calling the recorder with a `*youtube.VideoInfo{StreamStatus:"live", Formats:nil}` marks the signal fresh; with formats present or status ended it does not; freshness expires after the stale window (inject time or use short window).
  - `TestMayResumeClosureTruthTable`: closure over (signal fresh?, chat open?, chat nil?) — true when signal fresh; true when chat open; false when neither; chat nil ⇒ falls back to signal only.
  - `TestFinalizeIncompleteTailInterruption`: a `DownloadResult` whose fake downloader reports `FinalizedDuringInterruption()==true` (add tiny seam: the compute function takes booleans — extend `computeIncompleteTail(videoBehind, audioBehind bool)` to `computeIncompleteTail(videoBehind, audioBehind, interrupted bool)`) persists `incomplete_tail=true` even with both behind-head false. Use the existing DB test fixture pattern from `progress_activity_ticker_test.go`.
  - Activity table: `ActivityWaitingResume` renders `"Stream interrupted - waiting for resume... (1m 20s)"` and carries the `"... ("` wait marker (extend `TestActivityMessage` + the marker loop in `TestActivityMessagesCarryWaitMarker`).

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement.**
  - `interruptionSignal` (new small file `internal/worker/interruption.go`):

```go
// interruptionSignal records the last time a player-response fetch showed
// the broadcast-interrupted signature: streamStatus "live" with zero
// formats. YouTube removes streaming data while ingestion is down but
// keeps the page live; a genuinely ended stream keeps post-live formats.
// Shared by pointer on JobContext so strategyCtx value-copies observe one
// truth. staleAfter bounds trust in a signature the refresh path stopped
// re-confirming (refresh cadence is ~20-30s under a stall).
type interruptionSignal struct{ lastSeen atomicTimeValue }

const interruptionSignalStaleAfter = 90 * time.Second

func (s *interruptionSignal) observe(info *youtube.VideoInfo) {
	if info != nil && info.StreamStatus == "live" && len(info.Formats) == 0 {
		s.lastSeen.StoreNow()
	}
}
func (s *interruptionSignal) fresh() bool {
	t := s.lastSeen.Load()
	return !t.IsZero() && time.Since(t) < interruptionSignalStaleAfter
}
```

  (`atomicTimeValue`: local `atomic.Int64`-backed time like engine's `atomicTime` — copy that tiny type here rather than exporting the engine's.)
  - `JobContext`: add `Interruption *interruptionSignal`; initialize once where JobContext is constructed (grep the constructor; nil-guard all reads).
  - `refreshGvsCredentials`: at the point the URL half receives its `GetVideoInfo` result (success or formats-empty), call `job.Interruption.observe(freshInfo)` (nil-safe).
  - `ExecuteWithChat`: after `chatDl` is resolved (line ~337), build

```go
	mayResume := func() bool {
		if jobCtx.Interruption != nil && jobCtx.Interruption.fresh() {
			return true
		}
		return chatDl != nil && chatDl.LiveContinuationOpen()
	}
```

  and pass it into the live path (thread as a parameter or store on the orchestrator's per-call state so `attachProgress` closes over it — `attachProgress` lives in `runLiveStreamDownload`, so pass `mayResume` as an argument alongside `curStart`).
  - `attachProgress`: for each non-nil downloader, `dl.MayResume = mayResume` (VOD path never sets it — nil keeps byte-compat).
  - `InterruptionTimeout` plumbing: mirror `SegmentWorkers` exactly — config read where `JobConfig` is built, into `DownloaderOptions.InterruptionTimeout` at every live `NewSegmentDownloader` site that also got `SegmentWorkers` (grep them).
  - `computeIncompleteTail(videoBehind, audioBehind, interrupted bool)` — OR the third input; update `finalizeIncompleteTail` to pass `result.VideoDownloader/AudioDownloader` latches via a small `downloaderInterrupted(d *engine.SegmentDownloader) bool` nil-safe helper; update its Warn log to name which evidence fired.
  - `activityMessage`: `case engine.ActivityWaitingResume: return fmt.Sprintf("Stream interrupted - waiting for resume... (%s)", e)`.

- [ ] **Step 4: Run** `go test ./internal/worker/... ./internal/engine/...` — PASS.
- [ ] **Step 5: Commit** `feat(worker): interruption classification, MayResume wiring, Tier 2 preservation`

---

### Task 5: Monitor — auto-resume on live re-detection

**Files:**
- Modify: `cmd/moombox/monitor_callbacks.go` (`createYouTubeJob`'s `if !added` branch, line ~314)
- Test: `cmd/moombox/monitor_callbacks_resume_test.go` (create) — if the closure resists unit testing, extract the decision into a pure function and test that:

```go
// resumeOnRedetect decides what a live re-detection of an EXISTING job does.
// Only a Finished job with preserved resume data (incomplete_tail) resumes;
// Cancelled is a human decision; everything else keeps today's silent drop.
func resumeOnRedetect(existing *database.Job, disposition monitor.JobDisposition, lastAutoResume time.Time, now time.Time) bool {
	if existing == nil || disposition != monitor.DispositionBroadcast {
		return false
	}
	if existing.Status != database.StatusFinished || !existing.IncompleteTail {
		return false
	}
	return now.Sub(lastAutoResume) >= 5*time.Minute
}
```

- [ ] **Step 1: Failing test** — table over (status × flag × disposition × cooldown): exactly one true row (Finished, flagged, broadcast, cooldown clear); Cancelled/flagless/VOD-disposition/cooldown-hot all false.
- [ ] **Step 2: Run** — FAIL.
- [ ] **Step 3: Implement.** In the `!added` branch: `existing, _ := s.db.GetJob(videoID)`; guard with `resumeOnRedetect(existing, d, lastResume[videoID], time.Now())` (cooldown map + mutex beside `lastAuthFailNotify`, same pattern). On true: update title if the detection's differs (`s.db.UpdateJobFields(videoID, map[string]any{"title": title})`), `s.dlWorker.ResumeJob(videoID)`, INFO log `"broadcast re-detected live — auto-resuming preserved job"`, and a notification (`"Stream Resumed"`, `notifications.TypeInfo`, Event `"download"` — copy the field/option shape of the nearby "Stream found" sends). Record cooldown timestamp.
- [ ] **Step 4:** `go build ./... && go test ./cmd/...` — PASS.
- [ ] **Step 5: Commit** `feat(monitor): auto-resume preserved Finished jobs on live re-detection`

---

### Task 6: Muxer — exported lossless concat + stream-params probe

**Files:**
- Modify: `internal/engine/muxer_trim.go` (export wrapper), new `internal/worker/probe_params.go`
- Test: `internal/worker/probe_params_test.go` (create; ffprobe-dependent cases behind the same live-tool gate the muxer tests use — grep for how muxer tests skip without ffmpeg)

**Interfaces:**
- Produces: `func (m *Muxer) ConcatCopy(ctx context.Context, inputs []string, outputPath string) error`; `type streamParams struct { VCodec string; Width, Height int; FrameRate string; ACodec string; SampleRate string; Channels int }`; `func probeStreamParams(ctx context.Context, ffprobePath, filePath string) (*streamParams, error)`; `func (p *streamParams) equal(q *streamParams) bool`.

- [ ] **Step 1: Failing unit tests** — `streamParams.equal` matrix (identical true; each field differing false; nil false). Probe test (gated): probe a tiny ffmpeg-generated fixture and assert non-empty codec fields.
- [ ] **Step 2: Implement.** `ConcatCopy`: create a temp dir, delegate to `concatIntermediates` (it already writes the list file and runs `-f concat -safe 0 -c copy -movflags faststart`). `probeStreamParams`: pattern after `probeAudioBitrate` (trim.go:537) but `-show_streams` without `-select_streams`, decode both `codec_type` entries; error (never default) when video stream absent — a probe failure must abort the merge, not fake success.
- [ ] **Step 3:** `go test ./internal/worker/... ./internal/engine/...` — PASS.
- [ ] **Step 4: Commit** `feat(engine,worker): exported lossless ConcatCopy + stream-params probe`

---

### Task 7: DB — `ReplaceJobSegments`

**Files:**
- Modify: `internal/database/database_jobs.go`
- Test: extend `internal/database/database_test.go` (fixture pattern from `TestClearJobSegmentsAndGaps`)

**Interfaces:**
- Produces: `func (db *Database) ReplaceJobSegments(jobID string, segs []Segment) error` — one transaction: `DELETE FROM segments WHERE job_id = ?` then re-insert via the `AddSegment` column list. Gaps untouched (unlike `ClearJobSegmentsAndGaps`).

- [ ] **Step 1: Failing test** — seed 3 rows, replace with 1 merged row, assert `GetSegments` returns exactly the merged row with its fields and gap rows survive.
- [ ] **Step 2: Implement** inside `db.mu`/transaction conventions used by neighbors; fire the same job-update notification `AddSegment` fires if it fires one (check; else none).
- [ ] **Step 3:** `go test ./internal/database/` — PASS.
- [ ] **Step 4: Commit** `feat(db): ReplaceJobSegments for part-merge row collapse`

---

### Task 8: Tier 4 — same-format part merge in finalize

**Files:**
- Create: `internal/worker/part_merge.go`
- Modify: `internal/worker/orchestrator_mux.go` (`finalizeMultiSegmentJob`, before the `len(segments) == 1` rename so a fully-merged job takes the plain name)
- Test: `internal/worker/part_merge_test.go`

**Interfaces:**
- Consumes: Tasks 6+7.
- Produces: `func (o *DownloadOrchestrator) mergeSameFormatParts(ctx context.Context, jobCtx *JobContext, segments []database.Segment) []database.Segment` — returns the (possibly collapsed) slice; on ANY failure returns the input unchanged.

- [ ] **Step 1: Failing tests** (pure logic, no ffmpeg):
  - `groupMergeRuns(params []*streamParams) [][]int` — contiguous identical runs: `[A A B A]` → `[[0 1] [2] [3]]`; all-identical → one run; nil param anywhere → that index is its own run and merges with nothing.
  - `mergedSegmentRow(run []database.Segment, outPath string, size int64) database.Segment` — SegmentIndex = first's, UnixStart = first's, UnixEnd = last's, Duration = sum, Quality/dimensions = first's, ChatFile = merged path or "" when no part had chat.
  - Chat merge: `mergeChatFiles(paths []string, outPath string) error` — reads each `chat.ChatData` JSON, concatenates `Messages` in file order, sums MessageCount, keeps first file's identity fields, writes via `utils.WriteChatFileAtomic`; missing/corrupt input file → error (caller falls back to no chat merge but still merges media, leaving per-part chat files in place and ChatFile = "").
  - Fallback: a `mergeSameFormatParts` run where probe returns error for one part → output slice == input slice (identity, no rows touched). Inject probe/concat via function fields on a small `partMerger` struct so tests stub them.
- [ ] **Step 2: Run** — FAIL. **Step 3: Implement**:

```go
// mergeSameFormatParts opportunistically concat-copies contiguous runs of
// parts whose stream parameters are identical (Tier 4 of the interruption
// spec). Decisions come from ffprobe on the actual files — the Segment
// rows' quality metadata lacks audio parameters — and any probe or concat
// failure returns the input untouched: the merge is an improvement, never
// a gate on finalize. Merged media is written next to the parts as
// "<base> - merged<runIndex>.mp4", then renamed over the run's first part
// name after row replacement succeeds; obsolete later-part files and their
// chat siblings are removed only after the DB replace commits.
```

  Flow: probe every `seg.FilePath` → `groupMergeRuns` → for each run len>1: `ConcatCopy` to temp name in the same dir → stat size → `mergedSegmentRow` → attempt `mergeChatFiles` when ≥1 part has chat (failure ⇒ ChatFile "" and keep part chat files) → build the full replacement slice → `db.ReplaceJobSegments` → on success delete superseded part files/chat best-effort (Warn on failure), rename merged temp to the first part's filename, fix the row's FilePath. Log one INFO per merged run (`"merged N same-format parts"` with jobID, run size, bytes). Call it at the top of `finalizeMultiSegmentJob`: `segments = o.mergeSameFormatParts(ctx, jobCtx, segments)`.
- [ ] **Step 4:** `go test ./internal/worker/...` — PASS. Manual gate note for review: run a real two-part concat behind the ffmpeg gate if the environment has it.
- [ ] **Step 5: Commit** `feat(worker): losslessly merge contiguous same-format parts at finalize`

---

### Task 9: Rider — startup dead-auth recovery

**Files:**
- Modify: `internal/cookies/refresh.go`
- Test: extend the package's existing refresh tests (find the fixture; if none reaches this seam, test via a `RefreshService` with stubbed check functions — the checks are methods, so introduce tiny function fields defaulting to the methods, the package's established stub pattern permitting; otherwise test the extracted decision helper)

**Interfaces:**
- Produces: unauthenticated-at-first-completed-check fires `OnRecoveryNeeded(platform)` once.

- [ ] **Step 1: Failing test** — first completed check (hasChecked false) with `ytAuth=false, ytErr=nil` fires recovery for youtube exactly once; with `ytErr != nil` does not; second check with same state does not re-fire (transition logic unchanged).
- [ ] **Step 2: Implement.** In the block quoted at `refresh.go:281` extend the condition:

```go
	// Startup case: auth already dead when the process began never produces
	// a witnessed transition, so it previously stayed silent forever
	// (field case 2026-08-20: youtube=false on every check, all day, no
	// recovery, no notification). The first CONCLUSIVE check that finds a
	// platform unauthenticated fires the same recovery path once;
	// subsequent checks return to transition-only.
	firstConclusive := !hasChecked
	if rs.OnRecoveryNeeded != nil {
		if ((hasChecked && prevYT) || firstConclusive) && !ytAuth && ytErr == nil {
			rs.logger.Warn("youtube auth lost, triggering recovery")
			rs.OnRecoveryNeeded("youtube")
		}
		if ((hasChecked && prevTW) || firstConclusive) && !twAuth && twErr == nil {
			rs.logger.Warn("twitch auth lost, triggering recovery")
			rs.OnRecoveryNeeded("twitch")
		}
	}
```

  (Replaces the existing two-branch block; preserve its comments' intent. Note `hasChecked` snapshot semantics: it was read under the lock before `rs.hasCheckedOnce = true`.)
- [ ] **Step 3:** `go test ./internal/cookies/` — PASS.
- [ ] **Step 4: Commit** `fix(cookies): fire auth recovery on startup-dead auth, not only witnessed transitions`

---

### Task 10: Docs + full gates

**Files:**
- Modify: `docs/spec/data-and-storage.md` (config table row for `interruption_timeout`), `docs/spec/architecture.md` (finalize deferral + auto-resume valve, one short paragraph each), `docs/spec/platform-services.md` only if it documents the finalize decision (grep first)

- [ ] **Step 1:** Add the config row (copy `segment_workers`'s row format) and the two paragraphs; keep claims implementation-true (cite function names).
- [ ] **Step 2:** Full gates: `gofmt -l internal cmd` clean, `go build ./...`, `go vet ./...`, `go test ./...` green.
- [ ] **Step 3: Commit** `docs: interruption resume — config row + architecture notes`
- [ ] **Step 4:** Report done. Plan + spec docs are deleted only after field verification (owner rule: implemented+verified), so leave both in place and say so.

---

## Field Verification (post-merge)

- Next broadcast interruption: expect `"stream interrupted — deferring finalize"` then in-place continuation (same staging, no new part) when the streamer returns inside the ceiling; the recording spans the outage with a timeline jump.
- Ceiling/chat-closed path: job finishes flagged, staging preserved; a later re-detection logs `"auto-resuming preserved job"` and appends.
- Any multi-part job with unchanged format: expect one `"merged N same-format parts"` INFO and a single output file.
- On the next process start with dead cookies: recovery + notification fire within the first check cycle.
