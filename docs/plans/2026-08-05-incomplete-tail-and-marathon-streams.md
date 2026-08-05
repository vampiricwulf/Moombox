# Incomplete-Tail Handling & Marathon-Stream (120h Retention) Support — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make truncated post-live downloads visible and recoverable (incomplete-tail flag + retry), survive multi-6h-wall-clock downloads (VOD-branch refresh loop), and diagnose >120h marathon streams truthfully (eviction detection) — with full marathon support explicitly gated on field evidence.

**Architecture:** Three shippable phases + one gated outline. Phase A surfaces the engine's existing "finalized behind head" state (already encoded as `streamEnded=false` after a warned finalize) up through a new precise atomic flag → DB column → routes → both UIs. Phase B adds a bounded rebuild loop to the orchestrator's VOD branch mirroring the live loop's `refreshDownload`, so googlevideo URL expiry (~6h) no longer kills long post-live downloads. Phase C adds a reactive eviction diagnosis: when a download dies pre-first-byte with a huge head, bisect for the true oldest available segment and inspect one boundary segment's box structure, then fail with a precise error. Phase D (jump + init resolver/synthesis) is **not tasked** — it is gated on Phase C's first real-world diagnosis (see §Phase D).

**Tech Stack:** Go 1.25, modernc/sqlite (schema v16→v17), vanilla JS Web UI (go:embed), bubbletea TUI. No new dependencies.

## Global Constraints

- Logger interface: anonymous 4-method interface per struct — never extract a named interface (CLAUDE.md).
- DB writes via `db.UpdateJobFields(jobID, map[string]any{...})`; every new column needs a `fieldToColumn` entry (enforced by `TestFieldToColumnCoverage`).
- All goroutines carry inline panic recovery (existing patterns; no new goroutines in this plan).
- API routes stay under `/api/` with frontend fetch calls in sync; Web UI changes require `go build` (go:embed).
- TUI chords: `buildMenuItems()` + `dispatchAction()` are the single source of truth.
- Job status lifecycle is untouched — incomplete-tail is a **flag on StatusFinished**, not a new status.
- Every commit: run `go build ./... && go vet ./...` plus the named package tests before committing. Commit trailer: `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- Research citations in this plan (file:line) were verified against commit `be74da5`; re-locate by symbol name if lines drift.

---

## Phase A + B ordering note

Task 1 (engine accessors) feeds both phases. Task 2 (VOD refresh loop) lands **before** the flag persistence (Task 3+) so that by the time the flag can be set, in-process recovery has already had its chance — the flag then marks only genuinely unrecoverable-in-session tails.

---

### Task 1: Engine — precise `FinalizedBehindHead` + `HeadSeq` accessors

**Files:**
- Modify: `internal/engine/downloader.go` (struct, near `streamEnded atomic.Bool`)
- Modify: `internal/engine/downloader_dash.go` (the two `warnIfFinalizingBehindHead()` true-branches)
- Test: `internal/engine/downloader_dash_headseq_test.go`

**Interfaces:**
- Produces: `func (d *SegmentDownloader) FinalizedBehindHead() bool` — true iff the downloader finalized while `currentSeq < headSeq` (set exactly when the "finalizing with unfetched tail" warning fired). Valid after `Start` returns nil.
- Produces: `func (d *SegmentDownloader) HeadSeq() int` — last known head (−1 if never learned).
- Consumed by: Task 2 (refresh loop), Task 4 (flag persistence), Task 8 (eviction diagnosis).

Rationale: `!streamEnded.Load()` is too broad a signal — HLS paths, the live-edge MaxTimeout stall backstop, and cancels all leave it false. Only the behind-head warn sites mean "known-incomplete".

- [ ] **Step 1: Write the failing test** (append to `downloader_dash_headseq_test.go`):

```go
// TestFinalizedBehindHeadAccessor pins the precise incomplete signal: it is
// set exactly when a finalize fires the unfetched-tail warning, and NOT on a
// clean past-head finalize.
func TestFinalizedBehindHeadAccessor(t *testing.T) {
	// Incomplete finalize: behind head, budget exhausted.
	d := NewSegmentDownloader(DownloaderOptions{
		OutputFile:        filepath.Join(t.TempDir(), "v"),
		MaxTimeout:        time.Minute,
		CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
	})
	d.currentSeq.Store(50)
	d.headSeq.Store(100)
	d.lastSegTime.Store(time.Now().Add(-2 * time.Minute))
	n := goneRetryDuringDownload + 1
	if err := d.handleGoneError(context.Background(), &n, true); err != errStreamDone {
		t.Fatalf("handleGoneError = %v, want errStreamDone", err)
	}
	if !d.FinalizedBehindHead() {
		t.Error("FinalizedBehindHead() = false after behind-head finalize")
	}
	if got := d.HeadSeq(); got != 100 {
		t.Errorf("HeadSeq() = %d, want 100", got)
	}

	// Clean finalize: past head — flag must stay false.
	d2 := NewSegmentDownloader(DownloaderOptions{
		OutputFile:        filepath.Join(t.TempDir(), "v"),
		MaxTimeout:        time.Minute,
		CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
	})
	d2.currentSeq.Store(101)
	d2.headSeq.Store(100)
	d2.lastSegTime.StoreNow()
	n2 := goneRetryDuringDownload + 1
	if err := d2.handleGoneError(context.Background(), &n2, true); err != errStreamDone {
		t.Fatalf("clean handleGoneError = %v, want errStreamDone", err)
	}
	if d2.FinalizedBehindHead() {
		t.Error("FinalizedBehindHead() = true after clean finalize")
	}
}
```

- [ ] **Step 2: Run** `go test -run TestFinalizedBehindHeadAccessor ./internal/engine/` — expect FAIL (undefined methods).
- [ ] **Step 3: Implement.** In `downloader.go`, next to `streamEnded`:

```go
	// finalizedBehindHead latches when a finalize fired the
	// unfetched-tail warning (currentSeq < headSeq at errStreamDone) —
	// the precise "known-incomplete recording" signal the worker
	// persists as the job's incomplete_tail flag. streamEnded alone is
	// too broad: cancels and live-edge MaxTimeout stalls also leave it
	// unset without implying a missing tail.
	finalizedBehindHead atomic.Bool
```

Accessors (near `CurrentSeq`):

```go
// FinalizedBehindHead reports whether the downloader finalized knowing
// segments below head were left unfetched. Valid after Start returns nil.
func (d *SegmentDownloader) FinalizedBehindHead() bool { return d.finalizedBehindHead.Load() }

// HeadSeq returns the last known head sequence (-1 if never learned).
func (d *SegmentDownloader) HeadSeq() int { return int(d.headSeq.Load()) }
```

In `downloader_dash.go`, both places that call `warnIfFinalizingBehindHead()` on a finalize path set the flag when it returns true. In `handleGoneError`:

```go
		if d.warnIfFinalizingBehindHead() {
			d.finalizedBehindHead.Store(true)
			// Known-incomplete finalize: leave streamEnded unset ... (existing comment)
			return errStreamDone
		}
```

In `handleHTTPError`'s ended branch and MaxTimeout backstop, change the bare calls to:

```go
			if d.warnIfFinalizingBehindHead() {
				d.finalizedBehindHead.Store(true)
			}
```

(three call sites total — keep the surrounding control flow unchanged).

- [ ] **Step 4: Run** `go test -run "TestFinalizedBehindHead|TestDashLoop|TestHandleGone" ./internal/engine/` — expect PASS (integration tests confirm no behavior change).
- [ ] **Step 5: Commit** `feat(engine): expose FinalizedBehindHead/HeadSeq — precise incomplete-recording signal`.

---

### Task 2: Worker — bounded VOD-branch refresh loop (URL-expiry survival)

**Files:**
- Modify: `internal/worker/orchestrator.go` (the `isVod` branch, currently `err = o.runDownloaders(ctx, result)` around line 356)
- Test: `internal/worker/orchestrator_vod_refresh_test.go` (new)

**Interfaces:**
- Consumes: `result.VideoDownloader.FinalizedBehindHead()`, `.CurrentSeq()` (Task 1 + existing).
- Consumes: `o.refreshDownload(ctx, jobCtx, freshInfo, false)` (exists at `orchestrator_youtube.go:456` — rebuilds downloaders with fresh extraction).
- Produces: `runVodDownloadWithRefresh(ctx, jobCtx, result, videoInfo) (*DownloadResult, error)` — replaces the single `runDownloaders` call in the VOD branch; returns the FINAL result (possibly rebuilt) so downstream mux uses the right downloader handles.

Behavior contract: after a nil `runDownloaders` return, if either downloader `FinalizedBehindHead()` **and** the attempt made progress (CurrentSeq advanced vs. pre-run) **and** attempts < `maxVodRefreshAttempts` (const **4** — covers ~24h of 6h URL lifetimes), re-extract via `jobCtx.YT.GetVideoInfo`, seed `jobCtx.VideoStartSeq`/`AudioStartSeq` from `CurrentSeq()`, rebuild via `refreshDownload`, re-attach the progress tracker, and run again. Break on: no progress, error, context cancel, fresh info no longer manifestless-eligible (`!HasManifestlessDashFormats(freshInfo.Formats)` — the stream became a true VOD; the flag path handles the rest), or attempts exhausted. Only YouTube jobs (`jobCtx.Job.Platform == "youtube"`) enter the loop.

- [ ] **Step 1: Write the failing test.** The loop's decision logic must be extracted into a pure helper for testability:

```go
package worker

import "testing"

func TestVodRefreshDecision(t *testing.T) {
	cases := []struct {
		name                       string
		behindHead, progressed     bool
		attempt                    int
		manifestlessStillAvailable bool
		want                       bool
	}{
		{"incomplete with progress refreshes", true, true, 1, true, true},
		{"complete finalize stops", false, true, 1, true, false},
		{"no progress stops (avoid API spin)", true, false, 1, true, false},
		{"attempts exhausted stops", true, true, maxVodRefreshAttempts, true, false},
		{"stream became true VOD stops", true, true, 1, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldRefreshVodDownload(c.behindHead, c.progressed, c.attempt, c.manifestlessStillAvailable)
			if got != c.want {
				t.Errorf("shouldRefreshVodDownload = %v, want %v", got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run** `go test -run TestVodRefreshDecision ./internal/worker/` — expect FAIL (undefined).
- [ ] **Step 3: Implement.** In `orchestrator.go`:

```go
// maxVodRefreshAttempts bounds the post-live URL-refresh loop. Googlevideo
// URLs live ~6h; a marathon post-live backfill can need several refreshes
// to finish. 4 attempts ≈ 24h of wall clock, past which the incomplete_tail
// flag + manual retry take over.
const maxVodRefreshAttempts = 4

// shouldRefreshVodDownload is the pure decision for one more refresh pass:
// the run must have ended knowing a tail is missing, must have actually
// advanced (a zero-progress rebuild would spin extraction calls against a
// dead stream), must have budget left, and the fresh extraction must still
// offer segment-addressable formats (a stream that finished processing into
// a true VOD is handled by the flag + retry path instead).
func shouldRefreshVodDownload(behindHead, progressed bool, attempt int, manifestlessStillAvailable bool) bool {
	return behindHead && progressed && attempt < maxVodRefreshAttempts && manifestlessStillAvailable
}
```

Then the loop wrapper (same file, called from the `isVod` branch in place of the single `runDownloaders` call):

```go
// runVodDownloadWithRefresh runs the VOD-branch downloaders, and — for
// YouTube post-live jobs that finalize behind head (URL expiry, POT decay)
// — re-extracts fresh URLs and continues from the last written segment,
// bounded by maxVodRefreshAttempts. The live branch has had this recovery
// via ErrQualityLost→refreshDownload since the beginning; the VOD branch
// ran exactly once, so any download whose wall clock outlived the ~6h URL
// lifetime was truncated.
func (o *DownloadOrchestrator) runVodDownloadWithRefresh(ctx context.Context, jobCtx *JobContext, result *DownloadResult, videoInfo *youtube.VideoInfo) (*DownloadResult, error) {
	for attempt := 1; ; attempt++ {
		preVideo, preAudio := downloaderSeq(result.VideoDownloader), downloaderSeq(result.AudioDownloader)
		if err := o.runDownloaders(ctx, result); err != nil {
			return result, err
		}
		if jobCtx.Job.Platform != "youtube" || ctx.Err() != nil {
			return result, nil
		}
		behindHead := downloaderBehindHead(result.VideoDownloader) || downloaderBehindHead(result.AudioDownloader)
		progressed := downloaderSeq(result.VideoDownloader) > preVideo || downloaderSeq(result.AudioDownloader) > preAudio
		if !behindHead {
			return result, nil
		}
		freshInfo, err := jobCtx.YT.GetVideoInfo(ctx, jobCtx.Job.VideoID)
		if err != nil || freshInfo == nil {
			o.logger.Warn("VOD refresh: re-extraction failed; keeping incomplete result", "err", err, "jobID", jobCtx.Job.ID)
			return result, nil
		}
		if !shouldRefreshVodDownload(behindHead, progressed, attempt, HasManifestlessDashFormats(freshInfo.Formats)) {
			return result, nil
		}
		if result.VideoDownloader != nil {
			jobCtx.VideoStartSeq = downloaderSeq(result.VideoDownloader)
		}
		if result.AudioDownloader != nil {
			jobCtx.AudioStartSeq = downloaderSeq(result.AudioDownloader)
		}
		o.logger.Info("VOD refresh: tail incomplete, re-extracting and continuing",
			"attempt", attempt, "videoSeq", jobCtx.VideoStartSeq, "audioSeq", jobCtx.AudioStartSeq, "jobID", jobCtx.Job.ID)
		fresh, err := o.refreshDownload(ctx, jobCtx, freshInfo, false)
		if err != nil {
			o.logger.Warn("VOD refresh: rebuild failed; keeping incomplete result", "err", err, "jobID", jobCtx.Job.ID)
			return result, nil
		}
		o.attachTrackerAndProgress(fresh) // extract the existing attach block from ExecuteWithChat into this helper
		result = fresh
	}
}

func downloaderSeq(d *engine.SegmentDownloader) int {
	if d == nil {
		return 0
	}
	return d.CurrentSeq()
}

func downloaderBehindHead(d *engine.SegmentDownloader) bool {
	return d != nil && d.FinalizedBehindHead()
}
```

Note for the implementer: `attachTrackerAndProgress` is a mechanical extraction of the existing `tracker.AttachVideoDownloader/AttachAudioDownloader` + `OnProgress` wrapper block (currently inline in `ExecuteWithChat` around `orchestrator.go:321-326` / `orchestrator_youtube.go:93-114` — the VOD branch uses the orchestrator.go one). Extract exactly; do not redesign it. The `isVod` branch's `err = o.runDownloaders(ctx, result)` becomes `result, err = o.runVodDownloadWithRefresh(ctx, jobCtx, result, videoInfo)` — and every later use of `result` in that branch (mux) uses the returned value.

- [ ] **Step 4: Run** `go test -count=1 ./internal/worker/` — expect PASS.
- [ ] **Step 5: Commit** `feat(worker): bounded refresh loop for post-live downloads — survive URL expiry`.

---

### Task 3: Database — `incomplete_tail` column (schema v17)

**Files:**
- Modify: `internal/database/migrations.go` (`schemaVersion` 16→17, new `migrateV17`, `createSchema` column)
- Modify: `internal/database/types.go` (Job struct), `internal/database/database.go` (`fieldToColumn`, `stmtGetJob` SELECT, `scanJobRow`), `internal/database/database_jobs.go` (`getAllJobsUnlocked` SELECT)
- Test: `internal/database/migrations_test.go` (follow the existing migrateV16 test's shape)

**Interfaces:**
- Produces: `Job.IncompleteTail bool` (JSON `incompleteTail,omitempty`), writable via `UpdateJobFields(id, map[string]any{"incomplete_tail": true})`.

- [ ] **Step 1: Write the failing test** (mirror the v16 migration test in the same file — create a v16 DB, run migration, assert the column exists and defaults to 0):

```go
func TestMigrateV17IncompleteTail(t *testing.T) {
	db := newTestDBAtVersion(t, 16) // use the same fixture helper pattern as the v16 test; if none exists, open a fresh DB and assert post-migration state
	job := mustCreateJob(t, db)     // any existing job-creation helper in this package
	if job.IncompleteTail {
		t.Fatal("fresh job should default IncompleteTail=false")
	}
	if _, err := db.UpdateJobFields(job.ID, map[string]any{"incomplete_tail": true}); err != nil {
		t.Fatalf("UpdateJobFields(incomplete_tail): %v", err)
	}
	got, err := db.GetJob(job.ID)
	if err != nil || !got.IncompleteTail {
		t.Fatalf("IncompleteTail not persisted: job=%+v err=%v", got, err)
	}
}
```

(Adapt helper names to what `migrations_test.go`/`database_test.go` actually provide — read the v16 test first and clone its scaffolding; `TestFieldToColumnCoverage` will additionally fail until the map entry exists, which is part of the red state.)

- [ ] **Step 2: Run** `go test -run "TestMigrateV17|TestFieldToColumnCoverage" ./internal/database/` — expect FAIL.
- [ ] **Step 3: Implement**, following the v16 pattern exactly:
  - `schemaVersion = 17`; `migrateV17()` with guarded `ALTER TABLE jobs ADD COLUMN incomplete_tail INTEGER NOT NULL DEFAULT 0` (`isDuplicateColumnErr` guard); register in the migration switch.
  - Same column in `createSchema`'s `CREATE TABLE jobs`.
  - `types.go`: `IncompleteTail bool \`json:"incompleteTail,omitempty"\`` with a doc comment: `// IncompleteTail marks a Finished job whose recording is known to be missing tail segments (finalized behind head after refresh attempts). Staging + resume sidecar are preserved; Retry/Resume are allowed and clear the flag on a complete re-run.`
  - `fieldToColumn`: `"incomplete_tail": "incomplete_tail"`.
  - Both SELECT lists + `scanJobRow` int→bool scan, in matching column order (the comment at `database.go:487-489` warns about this).
- [ ] **Step 4: Run** `go test -count=1 ./internal/database/` — expect PASS.
- [ ] **Step 5: Commit** `feat(database): incomplete_tail flag on jobs — schema v17`.

---

### Task 4: Worker — persist the flag + preserve staging

**Files:**
- Modify: `internal/worker/orchestrator.go` (VOD branch, after `runVodDownloadWithRefresh` returns)
- Modify: `internal/worker/worker.go` (staging-cleanup carve-out ~line 578)
- Test: `internal/worker/orchestrator_vod_refresh_test.go` (extend)

**Interfaces:**
- Consumes: Task 1 accessors, Task 3 column.
- Produces: after every VOD-branch run, `incomplete_tail` is written (true **or** false — unconditional write so a successful retry self-clears); staging survives cleanup when the flag is set.

- [ ] **Step 1: Write the failing test** for the pure flag computation:

```go
func TestComputeIncompleteTail(t *testing.T) {
	if !computeIncompleteTail(true, false) || !computeIncompleteTail(false, true) {
		t.Error("either downloader behind head must flag the job")
	}
	if computeIncompleteTail(false, false) {
		t.Error("clean finish must not flag")
	}
}
```

- [ ] **Step 2: Run** — FAIL. **Step 3: Implement:**

```go
// computeIncompleteTail: the job is incomplete if EITHER stream finalized
// behind head — video and audio are independent downloaders with
// independent head tracking, and a missing tail on one truncates the mux.
func computeIncompleteTail(videoBehind, audioBehind bool) bool { return videoBehind || audioBehind }
```

In the VOD branch, immediately after the refresh loop returns with `err == nil`:

```go
		incomplete := computeIncompleteTail(
			downloaderBehindHead(result.VideoDownloader),
			downloaderBehindHead(result.AudioDownloader))
		// Unconditional write: a retry that completes cleanly clears the
		// flag by writing false through the same path.
		o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{"incomplete_tail": incomplete})
		if incomplete {
			o.logger.Warn("recording finished with an unfetched tail — staging preserved; Retry will append the missing segments",
				"jobID", jobCtx.Job.ID,
				"videoSeq", downloaderSeq(result.VideoDownloader), "videoHead", downloaderHead(result.VideoDownloader),
				"audioSeq", downloaderSeq(result.AudioDownloader), "audioHead", downloaderHead(result.AudioDownloader))
		}
```

with `func downloaderHead(d *engine.SegmentDownloader) int { if d == nil { return -1 }; return d.HeadSeq() }`.

In `worker.go`'s cleanup site, extend the carve-out (re-fetch the job — the in-scope copy predates the orchestrator's write):

```go
	if jobCtx.StagingDir != "" {
		fresh, _ := w.db.GetJob(job.ID)
		preserveForTail := fresh != nil && fresh.IncompleteTail
		if w.hasUnmuxedParts(job.ID, jobCtx.StagingDir) {
			w.logger.Warn("preserving staging dir: a captured part is still unmuxed after finalize; recover via the Mux action",
				"path", jobCtx.StagingDir, "jobID", job.ID)
		} else if preserveForTail {
			w.logger.Warn("preserving staging dir: recording tail incomplete; Retry will resume from the sidecar",
				"path", jobCtx.StagingDir, "jobID", job.ID)
		} else if err := os.RemoveAll(jobCtx.StagingDir); err != nil {
			// ... existing branches unchanged
```

- [ ] **Step 4: Run** `go test -count=1 ./internal/worker/` — PASS. **Step 5: Commit** `feat(worker): persist incomplete_tail and preserve staging for tail recovery`.

---

### Task 5: API routes — allow Retry/Resume on flagged Finished jobs

**Files:**
- Modify: `internal/web/routes/jobs.go` (`/retry` gate ~1011, `/resume` gate ~1035)
- Test: `internal/web/routes/jobs_test.go` (clone an existing retry-route test's scaffolding)

**Interfaces:**
- Produces: both routes accept `StatusFinished` when `job.IncompleteTail` is true; all other gating (YouTube-only + `HasStagingFiles` on resume) unchanged. The existing `ResumeJob` mechanism is reused verbatim — it preserves staging and seq columns, and Task 4's unconditional flag write clears the flag on a clean re-run.

- [ ] **Step 1: Write the failing test** — POST `/api/jobs/{id}/retry` for a `StatusFinished` job with `IncompleteTail=true` expects 200; with `IncompleteTail=false` expects the existing 400/409 rejection (clone the package's existing route-test harness — see how the current retry-gate test builds its request and DB fixture).
- [ ] **Step 2: Run** — FAIL. **Step 3: Implement.** In both gates, replace the bare status switch's rejection with:

```go
	allowed := false
	switch job.Status {
	case database.StatusError, database.StatusCancelled, database.StatusCookies:
		allowed = true
	case database.StatusFinished:
		// Finished-but-incomplete: the tail is recoverable via the preserved
		// staging + resume sidecar (see Job.IncompleteTail).
		allowed = job.IncompleteTail
	}
	if !allowed {
		// existing error response unchanged
	}
```

- [ ] **Step 4: Run** `go test -count=1 ./internal/web/routes/` — PASS. **Step 5: Commit** `feat(api): allow retry/resume of Finished jobs flagged incomplete_tail`.

---

### Task 6: Web UI — badge + resume availability

**Files:**
- Modify: `web/public/app.js` (`RESUME_STATUSES` usage at :2292/:4544/:4583, `renderJobItem` ~:1870, `updateJobDetails` ~:2229)

**Interfaces:**
- Consumes: `job.incompleteTail` (Task 3 JSON field).

- [ ] **Step 1: Implement** (frontend has no test harness; verification is manual + `go build`):
  - Add a helper next to the status sets: `const canResumeStatus = (job) => RESUME_STATUSES.has(job.status) || (job.status === "Finished" && job.incompleteTail);` and replace the three `RESUME_STATUSES.has(...)` call sites with `canResumeStatus(job)` (keep the platform + `hasStaging` conjunctions as they are).
  - Details dialog: next to the chat-status badge block (~:2229), render `<sl-badge variant="warning">Incomplete tail</sl-badge>` when `job.incompleteTail` (mirror the `chatStatus` badge's show/hide pattern).
  - Job card: clone the `watchIndicatorHtml` overlay pattern (~:1859) into an `incompleteIndicatorHtml(job)` that emits a small warning-colored icon with `title="Recording is missing its tail — Resume to fetch it"` when `job.incompleteTail`.
- [ ] **Step 2: Verify:** `go build ./...` (embed refresh), then run the binary and confirm: flagged job shows badge + Resume enabled; unflagged Finished job shows neither. (Manually flag a job: `UPDATE jobs SET incomplete_tail=1 WHERE id='...';` on a test DB.)
- [ ] **Step 3: Commit** `feat(web): incomplete-tail badge + resume for flagged jobs`.

---

### Task 7: TUI — chord gating + visual marker

**Files:**
- Modify: `internal/tui/app_actions.go` (`buildMenuItems` "A R" JobFilter ~:445; batch path in `dispatchAction` ~:34-49)
- Modify: `internal/tui/task_list.go` (`renderJob` icon treatment ~:1070)
- Test: `internal/tui/app_actions_test.go` if a JobFilter test pattern exists (check; else rely on compile + manual)

**Interfaces:** Consumes `database.Job.IncompleteTail`.

- [ ] **Step 1: Implement:**
  - `"A R"` JobFilter: extend `canResume` to `(j.Status == database.StatusError || j.Status == database.StatusCancelled || j.Status == database.StatusCookies || (j.Status == database.StatusFinished && j.IncompleteTail)) && j.Platform == "youtube"` — keep the `HasStagingFiles` conjunction.
  - Batch path in `dispatchAction` case `"A R"`: add the same `|| (j.Status == database.StatusFinished && j.IncompleteTail)` to its status re-check.
  - `renderJob`: extend the curly-underline treatment (currently `StatusError`/`StatusCookies`) with `|| (job.Status == database.StatusFinished && job.IncompleteTail)` so a flagged Finished job's green icon carries the warning underline.
- [ ] **Step 2: Run** `go build ./... && go test -count=1 ./internal/tui/` — PASS. Manual check in the TUI with a flagged job.
- [ ] **Step 3: Commit** `feat(tui): incomplete-tail resume chord gating + marker`.

---

### Task 8: Engine — segment inspection (box inventory for eviction diagnosis)

**Files:**
- Create: `internal/engine/segment_inspect.go`
- Test: `internal/engine/segment_inspect_test.go`

**Interfaces:**
- Produces:

```go
type SegmentInspection struct {
	HasFtyp, HasMoov, HasSidx bool
	SidxTimescale             uint32 // 0 if no sidx / unparsed
	FirstMediaBox             string // "moof", "mdat", "styp", or "" if none seen
	Boxes                     []string // top-level box types in order (diagnosis logging)
	// SPSPPSHeuristic: "annexb" (00 00 01 start codes with NAL types 7/8 seen
	// in the buffer), "possible-avcc", or "none". Heuristic only — no
	// trun-guided sample walking; used exclusively for diagnosis logs.
	SPSPPSHeuristic string
}
func InspectSegment(data []byte) SegmentInspection
```

- Consumed by: Task 9 (eviction diagnosis) and, later, Phase D's init resolver.

Implementation notes for the engineer: reuse `extractMP4InitBoxes`'s walk skeleton (`downloader_init.go` — 4-byte size + type, 64-bit extended-size form, overshoot guards) but record every top-level box instead of stopping; `sidx` timescale is the big-endian uint32 at byte offset 12 of the box *body* (after the 4-byte version/flags and 4-byte reference_ID — ISO 14496-12 §8.16.3: fullbox header, then reference_ID, then timescale). The SPS/PPS heuristic scans the raw buffer for `00 00 01` / `00 00 00 01` followed by a byte whose low 5 bits are 7 or 8 (Annex-B), and separately for a 4-byte big-endian length in [2,1<<20] followed by such a byte at a `moof`-adjacent offset (label `possible-avcc`) — explicitly heuristic, documented as such.

- [ ] **Step 1: Write the failing tests** with synthetic box streams:

```go
func box(typ string, body []byte) []byte {
	b := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:8], typ)
	copy(b[8:], body)
	return b
}

func TestInspectSegmentInitCarrying(t *testing.T) {
	data := append(box("ftyp", []byte("iso6....")), append(box("moov", make([]byte, 32)), box("moof", make([]byte, 16))...)...)
	got := InspectSegment(data)
	if !got.HasFtyp || !got.HasMoov || got.FirstMediaBox != "moof" {
		t.Errorf("init-carrying segment misread: %+v", got)
	}
}

func TestInspectSegmentBareFragmentWithSidx(t *testing.T) {
	sidxBody := make([]byte, 24)
	binary.BigEndian.PutUint32(sidxBody[8:], 90000) // fullbox(4) + reference_ID(4), then timescale
	data := append(box("styp", []byte("msdh....")), append(box("sidx", sidxBody), append(box("moof", make([]byte, 16)), box("mdat", []byte{0, 0, 0, 1, 0x67, 0xAA})...)...)...)
	got := InspectSegment(data)
	if got.HasMoov || !got.HasSidx || got.SidxTimescale != 90000 {
		t.Errorf("bare fragment misread: %+v", got)
	}
	if got.SPSPPSHeuristic != "annexb" {
		t.Errorf("SPS heuristic = %q, want annexb (buffer contains 00 00 01 + NAL 7)", got.SPSPPSHeuristic)
	}
}

func TestInspectSegmentMalformed(t *testing.T) {
	got := InspectSegment([]byte{0, 0}) // too short for any box
	if got.HasFtyp || got.HasMoov || len(got.Boxes) != 0 {
		t.Errorf("malformed input must yield empty inspection: %+v", got)
	}
}
```

- [ ] **Step 2: Run** — FAIL. **Step 3: Implement** per the notes above (walk loop cloned from `extractMP4InitBoxes` with its guards; on any malformed box, stop and return what was collected so far). **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** `feat(engine): InspectSegment — box inventory + sidx timescale + SPS/PPS heuristic`.

---

### Task 9: Eviction detection — bisection + diagnosis + truthful error (Stage 0)

**Files:**
- Create: `internal/engine/eviction_probe.go`
- Modify: `internal/worker/orchestrator.go` (VOD branch) and `internal/worker/orchestrator_youtube.go` (live branch entry) — post-run zero-byte hook
- Modify: `internal/youtube/types.go` + `internal/youtube/player_api_parsing.go` (parse `targetDurationSec`)
- Test: `internal/engine/eviction_probe_test.go` (fake-GVS harness), `internal/youtube/player_api_parsing_test.go` (targetDuration parse)

**Interfaces:**
- Produces (engine):

```go
// FindOldestAvailableSeq bisects [0, head] for the first sequence the CDN
// still serves. probe returns (available, error); transient errors are
// retried twice per point. Returns (-1, err) if head itself is unavailable
// (URL dead — NOT an eviction signal).
func FindOldestAvailableSeq(ctx context.Context, head int, probe func(ctx context.Context, seq int) (bool, error)) (int, error)
```

  plus a downloader method wiring the probe to a real fetch:

```go
// ProbeSegmentAvailable GETs one segment (pot + headers applied) and
// reports availability by status code; the body (capped 4MB) is returned
// for inspection when available.
func (d *SegmentDownloader) ProbeSegmentAvailable(ctx context.Context, seq int) (bool, []byte, error)
```

- Produces (youtube): `Format.TargetDurationSec int` (JSON `targetDurationSec,omitempty`), parsed via the existing `getInt` in `parseFormats`.
- Produces (worker): `diagnoseEvictedStart(...)` — called only when a YouTube manifestless run ends with `BytesWritten()==0 && HeadSeq() > minEvictionHead` (const `minEvictionHead = 100_000` — a stream must plausibly exceed the retention window; ordinary failed starts skip the probes entirely). It bisects, fetches the boundary segment, runs `InspectSegment`, logs the full diagnosis at Warn, and sets the job error to:
  `"stream exceeds YouTube's ~120h retention window: segments 0..N are evicted (~Xh of the broadcast). From-start archiving of marathon streams requires init recovery — see docs/plans/2026-08-05-incomplete-tail-and-marathon-streams.md Phase D"`

- [ ] **Step 1: Write the failing tests.** Bisection (pure, no HTTP):

```go
func TestFindOldestAvailableSeq(t *testing.T) {
	const front = 437123
	probe := func(_ context.Context, seq int) (bool, error) { return seq >= front, nil }
	got, err := FindOldestAvailableSeq(context.Background(), 500000, probe)
	if err != nil || got != front {
		t.Fatalf("FindOldestAvailableSeq = %d, %v; want %d", got, err, front)
	}
}

func TestFindOldestAvailableSeqDeadURL(t *testing.T) {
	probe := func(context.Context, int) (bool, error) { return false, nil } // everything 403s
	if got, err := FindOldestAvailableSeq(context.Background(), 500000, probe); err == nil {
		t.Fatalf("dead URL must error, got %d", got)
	}
}

func TestFindOldestAvailableSeqRetriesTransient(t *testing.T) {
	flaky := 0
	probe := func(_ context.Context, seq int) (bool, error) {
		if seq == 250000 && flaky == 0 { flaky++; return false, errors.New("timeout") }
		return seq >= 200000, nil
	}
	got, err := FindOldestAvailableSeq(context.Background(), 500000, probe)
	if err != nil || got != 200000 {
		t.Fatalf("= %d, %v; want 200000", got, err)
	}
}
```

  And a fake-GVS integration case in the same file (clone `newFakeGVS` from `downloader_dash_integration_test.go`): head=500000, `decide` returns 403 for seq<437123 and 200 above, assert `ProbeSegmentAvailable`-driven bisection lands on 437123.

- [ ] **Step 2: Run** — FAIL. **Step 3: Implement:**
  - `FindOldestAvailableSeq`: standard lower-bound bisection; first verify `probe(head)` is available (else return `(-1, fmt.Errorf("head segment unavailable — URL problem, not eviction"))`); then verify `probe(0)`; if 0 is available return 0 (no eviction). Each probe point: up to 3 attempts on error, 250ms apart; a persistent per-point error aborts with that error.
  - `ProbeSegmentAvailable`: clone `fetchSegment`'s request construction (pot, headers, timeout) but return `(resp.StatusCode < 400, body(≤4MB), err)` without touching downloader state (no head harvest — this runs outside the loop; actually calling `noteHeadSeqFromResponse` here is harmless and correct — do it).
  - `targetDurationSec` parse: `if td := getInt(f, "targetDurationSec"); td > 0 { format.TargetDurationSec = td }` + struct field + one table row in the existing parseFormats test.
  - `diagnoseEvictedStart` in the worker: guard (`platform==youtube`, zero bytes, `HeadSeq() > minEvictionHead`), then bisect via the VIDEO downloader's probe, fetch boundary body, `engine.InspectSegment`, compute evicted hours as `oldest × targetDuration / 3600` (fall back to duration 1 with a "duration unknown" log note), log every `SegmentInspection` field, `setJobError`-equivalent with the message above. Wire it into the VOD branch (after the refresh loop, before the flag write — an evicted-start job is an Error, not Finished-incomplete) and the live branch's terminal failure path.
- [ ] **Step 4: Run** `go test -count=1 ./internal/engine/ ./internal/youtube/ ./internal/worker/` — PASS.
- [ ] **Step 5: Commit** `feat: eviction detection — bisected retention boundary + segment diagnosis + truthful marathon-stream error`.

---

### Task 10: Documentation + release-notes touchpoints

**Files:**
- Modify: `SPEC.md` (job lifecycle note: incomplete-tail flag semantics), `docs/spec/data-and-storage.md` (schema v17), `docs/spec/architecture.md` (VOD refresh loop paragraph)

- [ ] **Step 1:** Add one short paragraph each (flag semantics + preserved staging; v17 column; VOD-branch bounded refresh). Match the surrounding docs' voice; no marketing language.
- [ ] **Step 2: Commit** `docs: incomplete-tail flag, schema v17, VOD refresh loop`.

---

## Phase D — Marathon jump + init recovery (GATED — do not implement)

**Gate:** the first real-world `diagnoseEvictedStart` report from Task 9 (a genuine >120h stream). Its `SegmentInspection` output decides which variant below is real. Until then this section is design intent, not tasks — the correct code cannot be written without knowing what YouTube's boundary segments contain.

Sketch (for the future planner, informed by this round's research):
- **Jump:** worker-side, pre-`Start`: after `FindOldestAvailableSeq`, seed `jobCtx.VideoStartSeq/AudioStartSeq = oldest + margin` (margin ≈ 30min of segments; duration from `TargetDurationSec`), `ForceStartSeq` — zero engine-loop changes (the ForceStartSeq plumbing already exists). One-shot by construction.
- **Init resolver:** replace the four `manifestlessSq0URL` call sites (`strategy_youtube_manifestless_dash.go` video/audio InitURL derivations) + the jump start with a resolver: inline init in oldest-available segment (per diagnosis) → synthesized moov (only if diagnosis showed in-band SPS/PPS or VP9/AV1) → precise failure. Synthesis gets its oracle by generating an init for a *normal* stream and validating against the real sq=0 (FFmpeg accept + box diff).
- **Mid-download eviction gap-split:** wire the Twitch `ErrGapDetected`→`advanceToNewPart` pattern into the YouTube DASH path, triggered only when a behind-head stall's missing segment is *below* a re-bisected front. Each new part needs the init resolver.
- **Policy:** `max_backfill_hours` config (7-point template per `archive_window_days`: types.go, default, validation, resolver, config_routes, settings.js, tui/settings.go) + `disk.GetDiskSpace` preflight against `estimated hours × observed bitrate` before committing to a jump.
- **Known risks to re-verify at implementation time:** A/V alignment when video/audio jump margins differ (align both to the same wall-clock margin, not the same seq); resume identity across jump retries; the manifest-path `Initialization` gap (DashStrategy has no sq=0 fallback — pre-existing, fix opportunistically).

## Self-review notes

- Spec coverage: owner-approved decision #1 (full incomplete-tail handling) → Tasks 1, 3–7; URL-expiry survival → Task 2; Stage 0 eviction detection + diagnosis → Tasks 8–9; docs → Task 10; marathon jump/init → Phase D (explicitly gated by owner-agreed evidence requirement).
- Type consistency: `FinalizedBehindHead()/HeadSeq()` (Task 1) consumed in Tasks 2, 4, 9; `IncompleteTail`/`incomplete_tail` naming consistent across Tasks 3–7; `InspectSegment`/`SegmentInspection` (Task 8) consumed in Task 9; `shouldRefreshVodDownload`/`runVodDownloadWithRefresh` self-contained in Task 2.
- Deliberate scope exclusions: no new JobStatus value (flag on Finished per decision); no live-edge cadence changes; no post-live −2 cushion; no cross-downloader defer coordination (all owner-ratified "keep current behavior" calls, 2026-08-05).
