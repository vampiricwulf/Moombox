# Shutdown vs Cancel Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Separate shutdown (auto-resume) from user cancel (terminal with Resume/Reinitialize/Mux actions), fix chat header accuracy, and fix Muxing-on-shutdown dead state.

**Architecture:** Replace the single Retry action with three granular actions: Resume (continue from saved state), Reinitialize (fresh start), and Mux (force-mux staging files). Worker gets new `ResumeJob`, `ReinitializeJob`, and `MuxJob` methods. TUI and Web UI both gain the new action buttons/chords. Chat header `messageCount` updates on every flush for accuracy.

**Tech Stack:** Go (worker, chat, TUI, API), Vanilla JS + Shoelace (Web UI), SQLite (job state)

**Spec:** `docs/superpowers/specs/2026-03-19-shutdown-cancel-redesign.md`

---

## File Map

| File | Action | Responsibility |
|---|---|---|
| `internal/chat/downloader.go` | Modify | Add `updateChatFileHeader()` call after each incremental flush |
| `internal/worker/worker.go` | Modify | Add `ResumeJob`, `ReinitializeJob`, `MuxJob` methods; fix Muxing-on-shutdown in `enqueueExistingJobs` |
| `internal/worker/orchestrator.go` | Modify | Guard `download_started_at` to skip when already set |
| `internal/worker/orchestrator_mux.go` | Modify | Add `muxFromStaging()` wrapper |
| `internal/worker/staging.go` | Create | `HasStagingFiles()` and `HasSegmentFiles()` helpers |
| `internal/tui/app.go` | Modify | Replace `OnRetryJob` with `OnResumeJob`, `OnReinitializeJob`, `OnMuxJob` callbacks |
| `internal/tui/app_actions.go` | Modify | Update `buildMenuItems()` and `dispatchAction()` for new chords |
| `internal/web/routes/jobs.go` | Modify | Add `/resume`, `/reinitialize`, `/mux` endpoints; deprecate `/retry`; add `has_staging`/`has_segments` to job response |
| `web/public/index.html` | Modify | Replace Retry button with Resume, Reinitialize, Mux buttons |
| `web/public/app.js` | Modify | Update status sets, button visibility, action handlers |
| `cmd/moombox/main.go` | Modify | Wire new TUI callbacks |

---

### Task 1: Fix chat header accuracy

**Files:**
- Modify: `internal/chat/downloader.go:359-374` (runChatLoop flush block)

This is a standalone fix with no dependencies on other tasks.

- [ ] **Step 1: Write the test**

In `internal/chat/downloader_test.go`, add a test that verifies `messageCount` in the chat.json header is accurate after incremental flushes (not just on completion). The test should:
1. Create a ChatDownloader with a temp output file
2. Simulate multiple flush cycles by adding messages and calling `writeChatFile()`
3. Read the chat.json header after an incremental flush (not completion)
4. Assert that `messageCount` matches the actual number of messages in the array

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run TestChatHeaderAccuracy ./internal/chat/...`
Expected: FAIL — the header count will be stale after incremental appends.

- [ ] **Step 3: Add `updateChatFileHeader()` after each incremental flush**

In `internal/chat/downloader.go`, inside `runChatLoop()` at line 366, add `cd.updateChatFileHeader()` right after `cd.flushedToDisk = true` and before the dedup culling. This must go in the *caller* (not inside `writeChatFile()`) because `writeChatFile()` holds the file open via `defer f.Close()` during the incremental path, and `updateChatFileHeader()` reopens the file — which would fail on Windows due to file locking.

```go
cd.writeChatFile()
cd.lastWriteMs = now
cd.messages = nil // All written to disk, free memory
cd.flushedToDisk = true

// Update header to keep messageCount accurate after incremental flushes
cd.updateChatFileHeader()

// Bound seenIDs to prevent unbounded growth
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v -run TestChatHeaderAccuracy ./internal/chat/...`
Expected: PASS

- [ ] **Step 5: Run full chat test suite**

Run: `go test -v ./internal/chat/...`
Expected: All pass

- [ ] **Step 6: Commit**

```bash
git add internal/chat/downloader.go internal/chat/downloader_test.go
git commit -m "fix: update chat messageCount header on every flush

Previously the header was only updated on clean completion, leaving
it stale after interruptions. Now updates after each incremental
append (~1 second intervals)."
```

---

### Task 2: Fix Muxing-on-shutdown dead state

**Files:**
- Modify: `internal/worker/worker.go:190-202` (`enqueueExistingJobs`)

Standalone fix — jobs stuck in Muxing status on shutdown are reset to Downloading so they re-enter the pipeline.

- [ ] **Step 1: Write the test**

In `internal/worker/worker_test.go` (or a new test file if one doesn't exist for this), add a test that:
1. Creates a job in the DB with status `Muxing`
2. Calls `enqueueExistingJobs()` (or the equivalent startup path)
3. Asserts the job status was changed to `Downloading`
4. Asserts the job was enqueued

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run TestMuxingJobResetOnStartup ./internal/worker/...`
Expected: FAIL — Muxing jobs are not re-enqueued.

- [ ] **Step 3: Add Muxing reset in `enqueueExistingJobs`**

In `internal/worker/worker.go`, modify `enqueueExistingJobs()` (line 190). Before the `ShouldProcess` check, reset Muxing jobs to Downloading:

```go
func (w *DownloadWorker) enqueueExistingJobs() {
	jobs, err := w.db.GetAllJobs()
	if err != nil {
		w.logger.Error("failed to get existing jobs", "err", err)
		return
	}

	for _, job := range jobs {
		// Reset Muxing jobs to Downloading — muxing was interrupted by shutdown
		// and is idempotent (partial output is overwritten).
		if job.Status == database.StatusMuxing {
			w.logger.Info("resetting interrupted mux job", "jobID", job.ID)
			w.db.UpdateJobFields(job.ID, map[string]any{
				"status": database.StatusDownloading,
			})
			job.Status = database.StatusDownloading
		}
		if ShouldProcess(job) {
			w.queue.Enqueue(job.ID, job.Status)
		}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v -run TestMuxingJobResetOnStartup ./internal/worker/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/worker/worker.go internal/worker/worker_test.go
git commit -m "fix: reset Muxing jobs to Downloading on startup

Jobs interrupted during muxing were stuck permanently — ShouldProcess
only accepts Upcoming/Live/Downloading. Now they re-enter the pipeline
and mux again (idempotent)."
```

---

### Task 3: Create staging file detection helpers

**Files:**
- Create: `internal/worker/staging.go`
- Create: `internal/worker/staging_test.go`

These helpers are used by Tasks 4-7 for determining Resume/Mux availability.

- [ ] **Step 1: Write the tests**

Create `internal/worker/staging_test.go` with tests for:
- `HasStagingFiles(stagingBase, jobID)` — returns true when staging dir exists with files, false when empty or missing
- `HasSegmentFiles(stagingBase, jobID)` — returns true when staging dir contains recognized segment files (`video_stream`, `audio_stream`, `video.ts`, `video.mp4`, `audio.m4a`), false otherwise

```go
func TestHasStagingFiles(t *testing.T) {
	dir := t.TempDir()
	// No staging dir
	if HasStagingFiles(dir, "job1") { t.Fatal("expected false for missing dir") }
	// Empty staging dir
	os.MkdirAll(filepath.Join(dir, "job1"), 0o755)
	if HasStagingFiles(dir, "job1") { t.Fatal("expected false for empty dir") }
	// With file
	os.WriteFile(filepath.Join(dir, "job1", "chat.json"), []byte("{}"), 0o644)
	if !HasStagingFiles(dir, "job1") { t.Fatal("expected true") }
}

func TestHasSegmentFiles(t *testing.T) {
	dir := t.TempDir()
	jobDir := filepath.Join(dir, "job1")
	os.MkdirAll(jobDir, 0o755)
	// No segments
	os.WriteFile(filepath.Join(jobDir, "chat.json"), []byte("{}"), 0o644)
	if HasSegmentFiles(dir, "job1") { t.Fatal("expected false for non-segment files") }
	// DASH segment
	os.WriteFile(filepath.Join(jobDir, "video_stream"), []byte("data"), 0o644)
	if !HasSegmentFiles(dir, "job1") { t.Fatal("expected true for video_stream") }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -v -run TestHasStagingFiles ./internal/worker/...`
Expected: FAIL — functions don't exist.

- [ ] **Step 3: Implement the helpers**

Create `internal/worker/staging.go`:

```go
package worker

import (
	"os"
	"path/filepath"
)

// segmentFileNames contains the known filenames produced by download strategies.
var segmentFileNames = map[string]bool{
	"video_stream": true, // DASH video
	"audio_stream": true, // DASH audio
	"video.ts":     true, // HLS
	"video.mp4":    true, // VOD video
	"audio.m4a":    true, // VOD audio
}

// HasStagingFiles returns true if the staging directory for a job exists and is non-empty.
func HasStagingFiles(stagingBase, jobID string) bool {
	dir := filepath.Join(stagingBase, jobID)
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// HasSegmentFiles returns true if the staging directory contains recognized segment files.
func HasSegmentFiles(stagingBase, jobID string) bool {
	dir := filepath.Join(stagingBase, jobID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && segmentFileNames[e.Name()] {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v -run "TestHasStagingFiles|TestHasSegmentFiles" ./internal/worker/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/worker/staging.go internal/worker/staging_test.go
git commit -m "feat: add staging file detection helpers

HasStagingFiles and HasSegmentFiles check the staging directory for
Resume and Mux action availability."
```

---

### Task 4: Add worker methods for Resume, Reinitialize, and Mux

**Files:**
- Modify: `internal/worker/worker.go` — add `ResumeJob`, `ReinitializeJob`, `MuxJob`
- Modify: `internal/worker/orchestrator.go:86-90` — guard `download_started_at`
- Modify: `internal/worker/orchestrator_mux.go` — add `muxFromStaging()`
- Modify: `internal/worker/strategies.go` — no changes needed (DownloadResult struct is fine)

- [ ] **Step 1: Add `download_started_at` guard in orchestrator**

In `internal/worker/orchestrator.go`, modify the `ExecuteWithChat` method around line 86-90. Change the unconditional `download_started_at` set to only set it if it's not already present:

```go
	// Update status
	updates := map[string]any{
		"status": database.StatusDownloading,
	}
	if jobCtx.Job.DownloadStartedAt == "" {
		updates["download_started_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	o.db.UpdateJobFields(jobCtx.Job.ID, updates)
```

- [ ] **Step 2: Add `muxFromStaging` to orchestrator_mux.go**

Add a new method to `internal/worker/orchestrator_mux.go` that discovers files in the staging directory and constructs a synthetic `DownloadResult`:

```go
// muxFromStaging discovers segment files in the staging directory and runs
// the full mux pipeline. Used for the "Mux" action on cancelled/errored jobs
// where no DownloadResult exists from the download pipeline.
func (o *DownloadOrchestrator) muxFromStaging(ctx context.Context, jobCtx *JobContext) error {
	stagingDir := jobCtx.StagingDir

	// Discover segment files in priority order (DASH > HLS > VOD)
	result := &DownloadResult{}

	// DASH segments
	if fileExists(filepath.Join(stagingDir, "video_stream")) {
		result.VideoPath = filepath.Join(stagingDir, "video_stream")
		result.HasVideo = true
	}
	if fileExists(filepath.Join(stagingDir, "audio_stream")) {
		result.AudioPath = filepath.Join(stagingDir, "audio_stream")
		result.HasAudio = true
	}

	// HLS (single muxed stream)
	if !result.HasVideo && fileExists(filepath.Join(stagingDir, "video.ts")) {
		result.VideoPath = filepath.Join(stagingDir, "video.ts")
		result.HasVideo = true
		result.IsHls = true
	}

	// VOD
	if !result.HasVideo && fileExists(filepath.Join(stagingDir, "video.mp4")) {
		result.VideoPath = filepath.Join(stagingDir, "video.mp4")
		result.HasVideo = true
	}
	if !result.HasAudio && fileExists(filepath.Join(stagingDir, "audio.m4a")) {
		result.AudioPath = filepath.Join(stagingDir, "audio.m4a")
		result.HasAudio = true
	}

	if !result.HasVideo && !result.HasAudio {
		return fmt.Errorf("no segment files found in staging directory")
	}

	return o.muxAndFinalize(ctx, jobCtx, result)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
```

- [ ] **Step 3: Add `ResumeJob` method to worker**

In `internal/worker/worker.go`, add:

```go
// ResumeJob resumes a cancelled/errored YouTube job from its saved state.
// Preserves staging files, progress, and seq numbers.
func (w *DownloadWorker) ResumeJob(jobID string) {
	w.db.UpdateJobFields(jobID, map[string]any{
		"status": database.StatusDownloading,
		"error":  "",
	})
	w.EnqueueJob(jobID)
}
```

- [ ] **Step 4: Add `ReinitializeJob` method to worker**

In `internal/worker/worker.go`, add:

```go
// ReinitializeJob resets a job to a fresh state and re-enqueues it.
// Clears all progress fields and deletes the staging directory.
func (w *DownloadWorker) ReinitializeJob(jobID string) {
	// Read config for staging path
	if w.cfgMu != nil {
		w.cfgMu.RLock()
	}
	stagingBase := w.cfg.Paths.StagingDirectory
	if w.cfgMu != nil {
		w.cfgMu.RUnlock()
	}
	if stagingBase == "" {
		stagingBase = "./staging"
	}

	// Delete staging directory
	stagingDir := filepath.Join(stagingBase, jobID)
	if err := os.RemoveAll(stagingDir); err != nil {
		w.logger.Warn("failed to remove staging directory on reinitialize", "path", stagingDir, "err", err)
	}

	// Clear all non-input fields
	w.db.UpdateJobFields(jobID, map[string]any{
		"status":              database.StatusUpcoming,
		"error":               "",
		"progress":            "",
		"percent":             0,
		"speed":               "",
		"eta":                 "",
		"last_video_seq":      nil,
		"last_audio_seq":      nil,
		"total_video_seq":     nil,
		"total_audio_seq":     nil,
		"chat_status":         "",
		"total_chat_messages": nil,
		"download_started_at": "",
		"stream_end_time":     "",
		"output_file":         "",
		"filename":            "",
		"file_size":           nil,
		"chat_file":           "",
		"chat_filename":       "",
		"description_file":    "",
		"thumbnail_file":      "",
		"video_width":         nil,
		"video_height":        nil,
		"video_fps":           nil,
		"length_seconds":      nil,
		"selected_video_itag": nil,
		"selected_audio_itag": nil,
	})
	w.EnqueueJob(jobID)
}
```

- [ ] **Step 5: Add `MuxJob` method to worker**

In `internal/worker/worker.go`, add:

```go
// MuxJob force-muxes a cancelled/errored job's staging files.
// Bypasses the download queue — runs directly in a wg-tracked goroutine.
func (w *DownloadWorker) MuxJob(jobID string) error {
	// Read config for staging check
	if w.cfgMu != nil {
		w.cfgMu.RLock()
	}
	stagingBase := w.cfg.Paths.StagingDirectory
	if w.cfgMu != nil {
		w.cfgMu.RUnlock()
	}
	if stagingBase == "" {
		stagingBase = "./staging"
	}

	if !HasSegmentFiles(stagingBase, jobID) {
		return fmt.Errorf("no segment files found in staging")
	}

	w.db.UpdateJobFields(jobID, map[string]any{
		"status": database.StatusMuxing,
	})

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				w.logger.Error("panic in MuxJob", "jobID", jobID, "panic", fmt.Sprint(r))
				w.db.UpdateJobFields(jobID, map[string]any{
					"status": database.StatusError,
					"error":  fmt.Sprintf("internal panic: %v", r),
				})
			}
		}()

		job, err := w.db.GetJob(jobID)
		if err != nil {
			w.logger.Error("MuxJob: get job failed", "jobID", jobID, "err", err)
			return
		}

		jobCtx := w.buildJobContext(job)
		ctx := context.Background()

		if err := w.orchestrator.muxFromStaging(ctx, jobCtx); err != nil {
			w.logger.Error("MuxJob failed", "jobID", jobID, "err", err)
			w.db.UpdateJobFields(jobID, map[string]any{
				"status": database.StatusError,
				"error":  err.Error(),
			})
		}
	}()

	return nil
}
```

Note: `muxFromStaging` is called from within the worker package, so it can access the unexported method. If it needs to be exported, capitalize it.

- [ ] **Step 6: Build and verify**

Run: `go build ./...`
Expected: Compiles without errors.

- [ ] **Step 7: Commit**

```bash
git add internal/worker/worker.go internal/worker/orchestrator.go internal/worker/orchestrator_mux.go
git commit -m "feat: add ResumeJob, ReinitializeJob, MuxJob worker methods

ResumeJob preserves staging and resumes from saved state.
ReinitializeJob clears all progress and deletes staging.
MuxJob discovers staging files and runs the full mux pipeline.
Also guards download_started_at to preserve original timestamp
on resume."
```

---

### Task 5: Update TUI chords and actions

**Files:**
- Modify: `internal/tui/app.go:254-269` — replace `OnRetryJob` callback with new ones
- Modify: `internal/tui/app_actions.go:16-43` — update `dispatchAction` cases
- Modify: `internal/tui/app_actions.go:332-399` — update `buildMenuItems`

- [ ] **Step 1: Update callback fields in app.go**

In `internal/tui/app.go`, replace `OnRetryJob` (line 258) with three new callbacks. Also add a staging check function and a config reference for staging path:

```go
	OnResumeJob       func(jobID string)
	OnReinitializeJob func(jobID string)
	OnMuxJob          func(jobID string) error
	HasStagingFiles   func(jobID string) bool  // checks if staging dir has files
	HasSegmentFiles   func(jobID string) bool  // checks if staging dir has segment files
```

- [ ] **Step 2: Update `buildMenuItems` in app_actions.go**

Replace the Retry and Import menu items in `buildMenuItems()` (lines 335-340). The new items:

```go
func (a *App) buildMenuItems() []ActionMenuItem {
	items := []ActionMenuItem{
		{Chord: "A A", Label: "Add Video", HintLabel: "Add", Category: "Action"},
		{Chord: "A Z", Label: "Import Archive", HintLabel: "Import", Category: "Action"},
		{Chord: "A R", Label: "Resume Job", HintLabel: "Resume", Category: "Action", NeedsJob: true,
			DisabledReason: "no resumable jobs",
			JobFilter: func(j *database.Job) bool {
				canResume := (j.Status == database.StatusError || j.Status == database.StatusCancelled || j.Status == database.StatusCookies) &&
					j.Platform == "youtube"
				if canResume && a.HasStagingFiles != nil {
					return a.HasStagingFiles(j.ID)
				}
				return false
			}},
		{Chord: "A I", Label: "Reinitialize Job", HintLabel: "Reinit", Category: "Action", NeedsJob: true,
			DisabledReason: "no retriable jobs",
			JobFilter: func(j *database.Job) bool {
				return j.Status == database.StatusError || j.Status == database.StatusCancelled || j.Status == database.StatusCookies
			}},
		{Chord: "A M", Label: "Mux Job", HintLabel: "Mux", Category: "Action", NeedsJob: true, NeedsConfirm: true,
			DisabledReason: "no muxable jobs",
			JobFilter: func(j *database.Job) bool {
				canMux := j.Status == database.StatusCancelled || j.Status == database.StatusError
				if canMux && a.HasSegmentFiles != nil {
					return a.HasSegmentFiles(j.ID)
				}
				return false
			}},
		{Chord: "A C", Label: "Cancel Job", HintLabel: "Cancel", Category: "Action", NeedsJob: true, NeedsConfirm: true,
			// ... unchanged ...
		},
		{Chord: "A D", Label: "Delete Job", HintLabel: "Delete", Category: "Action", NeedsJob: true, NeedsConfirm: true,
			// ... unchanged ...
		},
		// ... rest unchanged ...
	}
	// ... rest of function unchanged ...
}
```

- [ ] **Step 3: Update `dispatchAction` in app_actions.go**

Replace the `"A I"` Import case (line 22) chord to `"A Z"`. Replace the `"A R"` Retry case (lines 31-43) with Resume, and add Reinitialize and Mux cases:

```go
case "A Z": // Import (was A I)
	// ... same import logic ...

case "A R": // Resume
	if job == nil && a.taskList.SelectedCount() > 0 && a.OnResumeJob != nil {
		count := 0
		for _, id := range a.taskList.SelectedIDs() {
			// Filter: only resume YouTube jobs with staging files
			j := a.taskList.GetJobByID(id)
			if j == nil || j.Platform != "youtube" { continue }
			if a.HasStagingFiles != nil && !a.HasStagingFiles(id) { continue }
			if j.Status != database.StatusCancelled && j.Status != database.StatusError && j.Status != database.StatusCookies { continue }
			a.OnResumeJob(id)
			count++
		}
		a.taskList.ClearSelection()
		a.setFeedback(fmt.Sprintf("Resumed %d jobs", count))
	} else if job != nil && a.OnResumeJob != nil {
		a.OnResumeJob(job.ID)
		a.setFeedback(fmt.Sprintf("Resuming: %s", job.Title))
	}

case "A I": // Reinitialize
	if job == nil && a.taskList.SelectedCount() > 0 && a.OnReinitializeJob != nil {
		count := 0
		for _, id := range a.taskList.SelectedIDs() {
			j := a.taskList.GetJobByID(id)
			if j == nil { continue }
			if j.Status != database.StatusError && j.Status != database.StatusCancelled && j.Status != database.StatusCookies { continue }
			a.OnReinitializeJob(id)
			count++
		}
		a.taskList.ClearSelection()
		a.setFeedback(fmt.Sprintf("Reinitialized %d jobs", count))
	} else if job != nil && a.OnReinitializeJob != nil {
		a.OnReinitializeJob(job.ID)
		a.setFeedback(fmt.Sprintf("Reinitializing: %s", job.Title))
	}

case "A M": // Mux (single job only — batch mux is uncommon and risky)
	if job != nil && a.OnMuxJob != nil {
		if err := a.OnMuxJob(job.ID); err != nil {
			a.setFeedback(fmt.Sprintf("Mux failed: %s", err))
		} else {
			a.setFeedback(fmt.Sprintf("Muxing: %s", job.Title))
		}
	}
```

- [ ] **Step 4: Build to verify compilation**

Run: `go build ./...`
Expected: Compiles. (The callbacks are not wired yet — that's Task 7.)

- [ ] **Step 5: Commit**

```bash
git add internal/tui/app.go internal/tui/app_actions.go
git commit -m "feat(tui): replace Retry with Resume/Reinitialize/Mux actions

A R = Resume (continue from saved state, YouTube only)
A I = Reinitialize (fresh start, was Retry)
A M = Mux (force-mux staging files)
A Z = Import (moved from A I to free the chord)"
```

---

### Task 6: Update Web UI (API + frontend)

**Files:**
- Modify: `internal/web/routes/jobs.go` — add new endpoints, add computed fields to job response
- Modify: `web/public/index.html` — replace Retry button with Resume/Reinitialize/Mux
- Modify: `web/public/app.js` — update status sets, button logic, action handlers

- [ ] **Step 1: Add `has_staging` and `has_segments` to job API response**

In `internal/web/routes/jobs.go`, the job endpoints return `database.Job` directly via `jsonResponse(rw, job)`. To add computed fields, create a wrapper type and a helper:

```go
// jobWithStaging wraps a Job with computed staging fields for the API response.
type jobWithStaging struct {
	*database.Job
	HasStaging  bool `json:"hasStaging"`
	HasSegments bool `json:"hasSegments"`
}

func enrichJob(job *database.Job, stagingBase string) jobWithStaging {
	return jobWithStaging{
		Job:         job,
		HasStaging:  worker.HasStagingFiles(stagingBase, job.ID),
		HasSegments: worker.HasSegmentFiles(stagingBase, job.ID),
	}
}
```

The `worker` package is already imported by the routes package. The `stagingBase` path comes from the config dependency already passed to `RegisterJobRoutes`.

Update the **single-job detail** endpoint (GET `/api/jobs/{id}`, line 232) to use `enrichJob(job, stagingBase)`. For list endpoints (GET `/api/jobs` and GET `/api/jobs/archived`), do NOT enrich every job in the list — that would scan the filesystem for every job on every list load. Instead, the frontend fetches enriched data when the user opens the job details dialog (which calls the single-job endpoint). The `hasStaging`/`hasSegments` fields are only needed for button visibility in the details panel, not in the job list cards.

- [ ] **Step 2: Add `/resume` endpoint**

In `internal/web/routes/jobs.go`, add after the `/retry` endpoint:

```go
// POST /api/jobs/:id/resume
r.Post("/api/jobs/{id}/resume", func(rw http.ResponseWriter, req *http.Request) {
	jobID := chi.URLParam(req, "id")
	job, err := db.GetJob(jobID)
	if err != nil || job == nil {
		jsonError(rw, "Job not found", http.StatusNotFound)
		return
	}

	// Validate status
	switch job.Status {
	case database.StatusCancelled, database.StatusError, database.StatusCookies:
		// OK
	default:
		jsonError(rw, "Job cannot be resumed in current state", http.StatusBadRequest)
		return
	}

	// YouTube only
	if job.Platform == "twitch" {
		jsonError(rw, "Twitch jobs cannot be resumed", http.StatusBadRequest)
		return
	}

	// Check staging
	if !HasStagingFiles(stagingBase, jobID) {
		jsonError(rw, "No staging files found — use Reinitialize instead", http.StatusBadRequest)
		return
	}

	if w != nil {
		w.ResumeJob(jobID)
	}

	if wsHub != nil {
		freshJob, _ := db.GetJob(jobID)
		if freshJob != nil {
			wsHub.BroadcastJobUpdate(freshJob.ID, freshJob)
		}
	}

	jsonResponse(rw, map[string]any{"success": true})
})
```

- [ ] **Step 3: Add `/reinitialize` endpoint**

```go
// POST /api/jobs/:id/reinitialize
r.Post("/api/jobs/{id}/reinitialize", func(rw http.ResponseWriter, req *http.Request) {
	jobID := chi.URLParam(req, "id")
	job, err := db.GetJob(jobID)
	if err != nil || job == nil {
		jsonError(rw, "Job not found", http.StatusNotFound)
		return
	}

	switch job.Status {
	case database.StatusError, database.StatusCancelled, database.StatusCookies:
		// OK
	default:
		jsonError(rw, "Job cannot be reinitialized in current state", http.StatusBadRequest)
		return
	}

	if w != nil {
		w.ReinitializeJob(jobID)
	}

	if wsHub != nil {
		freshJob, _ := db.GetJob(jobID)
		if freshJob != nil {
			wsHub.BroadcastJobUpdate(freshJob.ID, freshJob)
		}
	}

	jsonResponse(rw, map[string]any{"success": true})
})
```

- [ ] **Step 4: Add `/mux` endpoint**

```go
// POST /api/jobs/:id/mux
r.Post("/api/jobs/{id}/mux", func(rw http.ResponseWriter, req *http.Request) {
	jobID := chi.URLParam(req, "id")
	job, err := db.GetJob(jobID)
	if err != nil || job == nil {
		jsonError(rw, "Job not found", http.StatusNotFound)
		return
	}

	switch job.Status {
	case database.StatusCancelled, database.StatusError:
		// OK
	default:
		jsonError(rw, "Job cannot be muxed in current state", http.StatusBadRequest)
		return
	}

	if !HasSegmentFiles(stagingBase, jobID) {
		jsonError(rw, "No segment files found in staging", http.StatusBadRequest)
		return
	}

	if w != nil {
		if err := w.MuxJob(jobID); err != nil {
			jsonError(rw, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if wsHub != nil {
		freshJob, _ := db.GetJob(jobID)
		if freshJob != nil {
			wsHub.BroadcastJobUpdate(freshJob.ID, freshJob)
		}
	}

	jsonResponse(rw, map[string]any{"success": true})
})
```

- [ ] **Step 5: Map `/retry` to `/reinitialize` for backward compat**

Update the existing `/retry` handler (line 883) to call `w.ReinitializeJob(jobID)` instead of the inline logic. Keep the endpoint for backward compatibility.

- [ ] **Step 6: Update `web/public/index.html` — replace Retry button**

Replace the Retry button (lines 1131-1142) with three new buttons:

```html
<sl-button variant="success" id="details-resume-btn" style="display: none" outline>
    <sl-icon slot="prefix" name="play-fill"></sl-icon>
    Resume
</sl-button>
<sl-button variant="warning" id="details-reinit-btn" style="display: none" outline>
    <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
    Reinitialize
</sl-button>
<sl-button variant="primary" id="details-mux-btn" style="display: none" outline>
    <sl-icon slot="prefix" name="film"></sl-icon>
    Mux
</sl-button>
```

- [ ] **Step 7: Update `web/public/app.js` — status sets and button logic**

Replace `RETRY_STATUSES` (line 14) and update `updateDetailsButtons` (line 1546):

```javascript
const RESUME_STATUSES = new Set(["Cancelled", "Error", "COOKIES?"]);
const REINIT_STATUSES = new Set(["Error", "Cancelled", "COOKIES?"]);
const MUX_STATUSES = new Set(["Cancelled", "Error"]);
```

In `updateDetailsButtons`:

```javascript
updateDetailsButtons(job) {
    const canCancel = CANCEL_STATUSES.has(job.status);
    const canResume = RESUME_STATUSES.has(job.status) && job.platform === "youtube" && job.hasStaging;
    const canReinit = REINIT_STATUSES.has(job.status);
    const canMux = MUX_STATUSES.has(job.status) && job.hasSegments;
    const canDelete = DELETE_STATUSES.has(job.status);
    // ... rest of existing logic ...

    document.getElementById("details-cancel-btn").style.display = canCancel ? "" : "none";
    document.getElementById("details-resume-btn").style.display = canResume ? "" : "none";
    document.getElementById("details-reinit-btn").style.display = canReinit ? "" : "none";
    document.getElementById("details-mux-btn").style.display = canMux ? "" : "none";
    document.getElementById("details-delete-btn").style.display = canDelete ? "" : "none";
    // ... rest unchanged ...
}
```

- [ ] **Step 8: Update `web/public/app.js` — action handlers and event listeners**

Replace `retryJob()` with `resumeJob()`, `reinitializeJob()`, and `muxJob()`. Update event listeners (around line 163). Each follows the same pattern as the existing `retryJob()` but calls the new endpoints.

```javascript
async resumeJob(jobId) {
    const id = jobId || this.selectedJobId;
    if (!id || this._jobActionsInFlight.has(id)) return;
    this._jobActionsInFlight.add(id);
    try {
        const response = await fetch(`/api/jobs/${id}/resume`, { method: "POST" });
        if (response.ok) {
            this.showToast("Job resumed", "success");
            if (this.selectedJobId === id) {
                const dlg = document.getElementById("details-dialog");
                if (dlg?.open) dlg.hide();
                this.selectedJobId = null;
            }
            const archivedIdx = this.archivedJobs.findIndex(j => j.id === id);
            if (archivedIdx !== -1) {
                this.archivedJobs.splice(archivedIdx, 1);
                this.renderArchivedJobs();
            }
        } else {
            const data = await response.json().catch(() => ({ error: response.statusText }));
            this.showToast(data.error || "Failed to resume job", "danger");
        }
    } catch (e) {
        this.showToast("Failed to resume job: " + e.message, "danger");
    } finally {
        this._jobActionsInFlight.delete(id);
    }
}
```

Similar for `reinitializeJob()` (calls `/reinitialize`) and `muxJob()` (calls `/mux`, does NOT close dialog or remove from archived since mux keeps the job visible).

- [ ] **Step 9: Update web frontend batch operations**

Update the batch action infrastructure in `web/public/app.js`:
- Rename `batch-retry` button in `index.html` to `batch-reinit`. Add `batch-resume` button.
- In `batchAction()` (around line 3237): rename `"retry"` case to `"reinitialize"` (calls `/reinitialize`). Add `"resume"` case (calls `/resume`).
- In `quickAction()` (around line 2758): update `"retry"` dispatch to `"reinitialize"` → `reinitializeJob()`. Add `"resume"` → `resumeJob()` and `"mux"` → `muxJob()`.
- In `updateBatchActionBar()` (around line 3233): replace `batch-retry` visibility with `batch-reinit`. Show `batch-resume` when selected jobs include resumable ones.

- [ ] **Step 10: Build Go and verify**

Run: `go build ./...`
Expected: Compiles.

- [ ] **Step 11: Commit**

```bash
git add internal/web/routes/jobs.go web/public/index.html web/public/app.js
git commit -m "feat(web): add Resume/Reinitialize/Mux endpoints and UI buttons

New API: POST /resume, /reinitialize, /mux
Deprecate: /retry (mapped to /reinitialize)
Add has_staging and has_segments computed fields to job response.
Replace Retry button with Resume, Reinitialize, Mux in job details."
```

---

### Task 7: Wire TUI callbacks in main.go

**Files:**
- Modify: `cmd/moombox/main.go:1224-1230` — replace `OnRetryJob` wiring

- [ ] **Step 1: Replace callback wiring**

In `cmd/moombox/main.go`, replace the `OnRetryJob` wiring (lines 1224-1230) with the three new callbacks:

```go
app.OnResumeJob = func(jobID string) {
	dlWorker.ResumeJob(jobID)
}
app.OnReinitializeJob = func(jobID string) {
	dlWorker.ReinitializeJob(jobID)
}
app.OnMuxJob = func(jobID string) error {
	return dlWorker.MuxJob(jobID)
}
```

Also wire the staging check functions. Read config at call-time (not wire-time) so the closures reflect config changes:

```go
app.HasStagingFiles = func(jobID string) bool {
	cfgMu.RLock()
	base := cfg.Paths.StagingDirectory
	cfgMu.RUnlock()
	if base == "" { base = "./staging" }
	return worker.HasStagingFiles(base, jobID)
}
app.HasSegmentFiles = func(jobID string) bool {
	cfgMu.RLock()
	base := cfg.Paths.StagingDirectory
	cfgMu.RUnlock()
	if base == "" { base = "./staging" }
	return worker.HasSegmentFiles(base, jobID)
}
```

- [ ] **Step 2: Build and verify**

Run: `go build -o moombox.exe ./cmd/moombox`
Expected: Compiles.

- [ ] **Step 3: Run full test suite**

Run: `go test ./...`
Expected: All pass.

- [ ] **Step 4: Commit**

```bash
git add cmd/moombox/main.go
git commit -m "feat: wire Resume/Reinitialize/Mux callbacks in main

Connects TUI callbacks to worker methods and staging file
detection helpers."
```

---

### Task 8: Integration testing and final verification

- [ ] **Step 1: Run full build**

Run: `go build -o moombox.exe ./cmd/moombox`
Expected: Compiles without errors.

- [ ] **Step 2: Run full test suite**

Run: `go test ./...`
Expected: All pass.

- [ ] **Step 3: Run go vet**

Run: `go vet ./...`
Expected: No issues.

- [ ] **Step 4: Manual smoke test (if possible)**

Start the binary, verify:
1. Help screen (`?`) shows Resume, Reinitialize, Mux chords (not Retry)
2. Web UI job details shows correct buttons for different job statuses
3. The old `A I` chord now shows Import as `A Z`

- [ ] **Step 5: Final commit (if any fixes needed)**

```bash
git add -A
git commit -m "fix: address integration test findings"
```
