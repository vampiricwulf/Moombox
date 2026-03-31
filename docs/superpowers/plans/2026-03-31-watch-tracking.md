# Watch Tracking Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Track watched status and resume position for downloaded videos in the Web UI, with Plex-like resume dialog, auto-watched detection, and batch mark watched/unwatched.

**Architecture:** Two new columns (`watched`, `resume_position`) on the `jobs` table. A lightweight `UpdateResumePosition` DB method bypasses subscriber notifications for frequent 10-second saves. Normal `UpdateJobFields` handles watched toggles. Five new API endpoints handle resume saves, single/batch watched toggles. Frontend adds resume dialog overlay, periodic position saves, auto-watched detection, thumbnail eyeball icons, and batch buttons.

**Tech Stack:** Go (SQLite migration, chi routes), vanilla JS (HTML5 video events, fetch, sendBeacon), CSS (thumbnail overlays, pills), Shoelace (buttons, icons, badges)

**Spec:** `docs/superpowers/specs/2026-03-31-watch-tracking-design.md`

---

### Task 1: Database Migration & Job Struct

**Files:**
- Modify: `internal/database/migrations.go:9` (bump schemaVersion), `:328` (add v8 migration block)
- Modify: `internal/database/types.go:78` (add Watched and ResumePosition fields after QualityPreference)
- Test: `internal/database/database_test.go`

- [ ] **Step 1: Write the failing test for migration and new fields**

In `internal/database/database_test.go`, add a test that creates a job, sets watched and resume_position, then reads them back:

```go
func TestWatchedAndResumePosition(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &Job{
		ID:      "yt_watch1",
		VideoID: "watch1",
		URL:     "https://youtube.com/watch?v=watch1",
		Status:  StatusFinished,
		Title:   "Test Video",
	}
	db.AddJob(job)

	// Update watched and resume_position via UpdateJobFields
	db.UpdateJobFields("yt_watch1", map[string]any{
		"watched":         1,
		"resume_position": 1234.5,
	})

	got, err := db.GetJob("yt_watch1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Watched {
		t.Error("expected watched to be true")
	}
	if got.ResumePosition == nil || *got.ResumePosition != 1234.5 {
		t.Errorf("expected resume_position 1234.5, got %v", got.ResumePosition)
	}

	// Clear resume_position
	db.UpdateJobFields("yt_watch1", map[string]any{
		"resume_position": nil,
	})

	got, err = db.GetJob("yt_watch1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ResumePosition != nil {
		t.Errorf("expected resume_position to be nil, got %v", got.ResumePosition)
	}
	if !got.Watched {
		t.Error("expected watched to still be true after clearing resume_position")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run TestWatchedAndResumePosition ./internal/database/...`
Expected: FAIL — `Watched` and `ResumePosition` fields don't exist on Job struct yet.

- [ ] **Step 3: Add fields to Job struct**

In `internal/database/types.go`, add after the `QualityPreference` field (line 78):

```go
	// Watch tracking
	Watched        bool     `json:"watched"`
	ResumePosition *float64 `json:"resumePosition,omitempty"`
```

- [ ] **Step 4: Add migration v8**

In `internal/database/migrations.go`:

Bump the schema version constant on line 9:
```go
const schemaVersion = 8
```

Add after the `if version < 7 {` block closing brace (after line 328):

```go
	if version < 8 {
		if _, err := db.db.ExecContext(db.getCtx(), `ALTER TABLE jobs ADD COLUMN watched INTEGER DEFAULT 0`); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				return err
			}
		}
		if _, err := db.db.ExecContext(db.getCtx(), `ALTER TABLE jobs ADD COLUMN resume_position REAL`); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				return err
			}
		}

		_, err := db.db.ExecContext(db.getCtx(), "UPDATE schema_version SET version = ?", 8)
		if err != nil {
			return err
		}
	}
```

- [ ] **Step 5: Add to fieldToColumn map**

In `internal/database/database.go`, add after the `"quality_preference"` entry (line 53):

```go
	"watched":            "watched",
	"resume_position":    "resume_position",
```

- [ ] **Step 6: Update all SELECT queries and scan functions**

There are 4 locations where the SELECT column list and scan must be updated to include `watched, resume_position` at the end.

In `internal/database/database.go`:

**Prepared statement** (line 147-157) — Add `watched, resume_position` to the end of the SELECT column list:
```sql
		last_recheck_at, quality_preference,
		watched, resume_position
		FROM jobs WHERE id = ?`
```

**UpdateJobFields read-back query** (line 267-277) — Add `watched, resume_position` to the end of the SELECT column list:
```sql
		last_recheck_at, quality_preference,
		watched, resume_position
		FROM jobs WHERE id = ?`
```

**scanJob function** (line 289-314) — Add scan for new fields. Add a local variable `var watched int` alongside the other int-to-bool vars, and add to the Scan call after `&j.QualityPreference`:
```go
func scanJob(row *sql.Row) (*Job, error) {
	var j Job
	var isVod, manuallyAdded, allowNonStream, watched int
	err := row.Scan(
		&j.ID, &j.VideoID, &j.URL, &j.Title, &j.ChannelName, &j.Platform,
		&j.Status, &j.Progress, &j.Percent, &j.ETA, &j.Speed, &j.Error,
		&j.CreatedAt, &j.UpdatedAt,
		&j.LastVideoSeq, &j.LastAudioSeq, &j.TotalVideoSeq, &j.TotalAudioSeq,
		&isVod, &manuallyAdded, &allowNonStream, &j.StreamStartTime, &j.StreamEndTime,
		&j.LengthSeconds, &j.DownloadStartedAt, &j.ThumbnailURL, &j.Description,
		&j.OutputFile, &j.Filename, &j.OutputDirectory,
		&j.VideoWidth, &j.VideoHeight, &j.VideoFps, &j.FileSize,
		&j.ChatStatus, &j.TotalChatMessages, &j.ChatFilename, &j.ChatFile,
		&j.ThumbnailFile, &j.DescriptionFile,
		&j.TwitchQuality, &j.TwitchCategory, &j.ChannelAvatarURL,
		&j.SelectedVideoItag, &j.SelectedAudioItag, &j.StartTime, &j.EndTime,
		&j.LastRecheckAt, &j.QualityPreference,
		&watched, &j.ResumePosition,
	)
	if err != nil {
		return nil, err
	}
	j.IsVod = isVod != 0
	j.ManuallyAdded = manuallyAdded != 0
	j.AllowNonStream = allowNonStream != 0
	j.Watched = watched != 0
	return &j, nil
}
```

**scanJobRows function** (line 316-341) — Same changes as scanJob:
```go
func scanJobRows(rows *sql.Rows) (*Job, error) {
	var j Job
	var isVod, manuallyAdded, allowNonStream, watched int
	err := rows.Scan(
		&j.ID, &j.VideoID, &j.URL, &j.Title, &j.ChannelName, &j.Platform,
		&j.Status, &j.Progress, &j.Percent, &j.ETA, &j.Speed, &j.Error,
		&j.CreatedAt, &j.UpdatedAt,
		&j.LastVideoSeq, &j.LastAudioSeq, &j.TotalVideoSeq, &j.TotalAudioSeq,
		&isVod, &manuallyAdded, &allowNonStream, &j.StreamStartTime, &j.StreamEndTime,
		&j.LengthSeconds, &j.DownloadStartedAt, &j.ThumbnailURL, &j.Description,
		&j.OutputFile, &j.Filename, &j.OutputDirectory,
		&j.VideoWidth, &j.VideoHeight, &j.VideoFps, &j.FileSize,
		&j.ChatStatus, &j.TotalChatMessages, &j.ChatFilename, &j.ChatFile,
		&j.ThumbnailFile, &j.DescriptionFile,
		&j.TwitchQuality, &j.TwitchCategory, &j.ChannelAvatarURL,
		&j.SelectedVideoItag, &j.SelectedAudioItag, &j.StartTime, &j.EndTime,
		&j.LastRecheckAt, &j.QualityPreference,
		&watched, &j.ResumePosition,
	)
	if err != nil {
		return nil, err
	}
	j.IsVod = isVod != 0
	j.ManuallyAdded = manuallyAdded != 0
	j.AllowNonStream = allowNonStream != 0
	j.Watched = watched != 0
	return &j, nil
}
```

In `internal/database/database_jobs.go`:

**getAllJobsUnlocked query** (line 137-147) — Add `watched, resume_position` to the end of the SELECT column list:
```sql
		last_recheck_at, quality_preference,
		watched, resume_position
		FROM jobs ORDER BY updated_at DESC`
```

**updateJobExec** (line 194-222) — Add `watched=?, resume_position=?` to the SET clause and `boolToInt(job.Watched), job.ResumePosition` to the args:

```go
func updateJobExec(ctx context.Context, exec executor, job *Job) error {
	_, err := exec.ExecContext(ctx, `UPDATE jobs SET
		video_id=?, url=?, title=?, channel_name=?, platform=?, status=?,
		progress=?, percent=?, eta=?, speed=?, error=?, updated_at=?,
		last_video_seq=?, last_audio_seq=?, total_video_seq=?, total_audio_seq=?,
		is_vod=?, manually_added=?, allow_non_stream=?, stream_start_time=?,
		stream_end_time=?, length_seconds=?, download_started_at=?,
		thumbnail_url=?, description=?, output_file=?, filename=?,
		output_directory=?, video_width=?, video_height=?, video_fps=?, file_size=?,
		chat_status=?, total_chat_messages=?, chat_filename=?, chat_file=?,
		thumbnail_file=?, description_file=?,
		twitch_quality=?, twitch_category=?, channel_avatar_url=?,
		selected_video_itag=?, selected_audio_itag=?, start_time=?, end_time=?,
		last_recheck_at=?, quality_preference=?,
		watched=?, resume_position=?
		WHERE id=?`,
		job.VideoID, job.URL, job.Title, job.ChannelName, job.Platform, job.Status,
		job.Progress, job.Percent, job.ETA, job.Speed, job.Error, job.UpdatedAt,
		job.LastVideoSeq, job.LastAudioSeq, job.TotalVideoSeq, job.TotalAudioSeq,
		boolToInt(job.IsVod), boolToInt(job.ManuallyAdded), boolToInt(job.AllowNonStream),
		job.StreamStartTime, job.StreamEndTime, job.LengthSeconds, job.DownloadStartedAt,
		job.ThumbnailURL, job.Description, job.OutputFile, job.Filename,
		job.OutputDirectory, job.VideoWidth, job.VideoHeight, job.VideoFps, job.FileSize,
		job.ChatStatus, job.TotalChatMessages, job.ChatFilename, job.ChatFile,
		job.ThumbnailFile, job.DescriptionFile,
		job.TwitchQuality, job.TwitchCategory, job.ChannelAvatarURL,
		job.SelectedVideoItag, job.SelectedAudioItag, job.StartTime, job.EndTime,
		job.LastRecheckAt, job.QualityPreference,
		boolToInt(job.Watched), job.ResumePosition,
		job.ID)
	return err
}
```

- [ ] **Step 7: Run test to verify it passes**

Run: `go test -v -run TestWatchedAndResumePosition ./internal/database/...`
Expected: PASS

- [ ] **Step 8: Run full database test suite**

Run: `go test -v ./internal/database/...`
Expected: All tests PASS (existing tests should still work since new columns have defaults).

- [ ] **Step 9: Commit**

```bash
git add internal/database/migrations.go internal/database/types.go internal/database/database.go internal/database/database_jobs.go internal/database/database_test.go
git commit -m "feat: add watched and resume_position columns to jobs table (migration v8)"
```

---

### Task 2: UpdateResumePosition & BatchSetWatched Database Methods

**Files:**
- Modify: `internal/database/database.go` (add UpdateResumePosition after UpdateJobFields)
- Modify: `internal/database/database_jobs.go` (add BatchSetWatched)
- Test: `internal/database/database_test.go`

- [ ] **Step 1: Write failing tests**

In `internal/database/database_test.go`:

```go
func TestUpdateResumePosition(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &Job{
		ID:      "yt_resume1",
		VideoID: "resume1",
		URL:     "https://youtube.com/watch?v=resume1",
		Status:  StatusFinished,
	}
	db.AddJob(job)

	// Save a resume position
	db.UpdateResumePosition("yt_resume1", 500.5)

	got, err := db.GetJob("yt_resume1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ResumePosition == nil || *got.ResumePosition != 500.5 {
		t.Errorf("expected resume_position 500.5, got %v", got.ResumePosition)
	}

	// Verify updated_at was NOT bumped (compare with original)
	originalUpdatedAt := job.UpdatedAt
	if got.UpdatedAt != originalUpdatedAt {
		t.Errorf("UpdateResumePosition should not bump updated_at, but it changed from %q to %q", originalUpdatedAt, got.UpdatedAt)
	}
}

func TestBatchSetWatched(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Create 3 jobs
	for i, id := range []string{"yt_bw1", "yt_bw2", "yt_bw3"} {
		job := &Job{
			ID:      id,
			VideoID: fmt.Sprintf("bw%d", i+1),
			URL:     fmt.Sprintf("https://youtube.com/watch?v=bw%d", i+1),
			Status:  StatusFinished,
		}
		db.AddJob(job)
		// Set resume positions
		db.UpdateResumePosition(id, float64((i+1)*100))
	}

	// Batch mark as watched
	db.BatchSetWatched([]string{"yt_bw1", "yt_bw2", "yt_bw3"}, true)

	for _, id := range []string{"yt_bw1", "yt_bw2", "yt_bw3"} {
		got, err := db.GetJob(id)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Watched {
			t.Errorf("job %s: expected watched=true", id)
		}
		if got.ResumePosition != nil {
			t.Errorf("job %s: expected resume_position cleared, got %v", id, got.ResumePosition)
		}
	}

	// Batch mark as unwatched
	db.BatchSetWatched([]string{"yt_bw1", "yt_bw2"}, false)

	got1, _ := db.GetJob("yt_bw1")
	got3, _ := db.GetJob("yt_bw3")
	if got1.Watched {
		t.Error("yt_bw1: expected watched=false after batch unwatched")
	}
	if !got3.Watched {
		t.Error("yt_bw3: should still be watched (not in batch unwatched)")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -v -run "TestUpdateResumePosition|TestBatchSetWatched" ./internal/database/...`
Expected: FAIL — methods don't exist yet.

- [ ] **Step 3: Implement UpdateResumePosition**

In `internal/database/database.go`, add after the `UpdateJobFields` function (after line 287):

```go
// UpdateResumePosition saves the playback position without bumping updated_at
// or triggering subscriber notifications. Designed for frequent periodic saves
// during video playback (every ~10 seconds).
func (db *Database) UpdateResumePosition(jobID string, seconds float64) {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.db.ExecContext(db.getCtx(),
		"UPDATE jobs SET resume_position = ? WHERE id = ?", seconds, jobID)
	if err != nil && db.logger != nil {
		db.logger.Error("UpdateResumePosition failed", "jobID", jobID, "err", err)
	}
}
```

- [ ] **Step 4: Implement BatchSetWatched**

In `internal/database/database_jobs.go`, add after the `UpdateJob` function area:

```go
// BatchSetWatched marks multiple jobs as watched or unwatched and clears
// their resume_position. Triggers OnJobsChange for a full list refresh.
func (db *Database) BatchSetWatched(jobIDs []string, watched bool) {
	if len(jobIDs) == 0 {
		return
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	placeholders := make([]string, len(jobIDs))
	args := make([]any, 0, len(jobIDs)+1)
	args = append(args, boolToInt(watched))
	for i, id := range jobIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}

	query := fmt.Sprintf(
		"UPDATE jobs SET watched = ?, resume_position = NULL WHERE id IN (%s) AND status = ?",
		strings.Join(placeholders, ","),
	)
	args = append(args, string(StatusFinished))
	_, err := db.db.ExecContext(db.getCtx(), query, args...)
	if err != nil {
		if db.logger != nil {
			db.logger.Error("BatchSetWatched failed", "err", err)
		}
		return
	}

	db.notifyJobsChange()
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -v -run "TestUpdateResumePosition|TestBatchSetWatched" ./internal/database/...`
Expected: PASS

- [ ] **Step 6: Run full test suite**

Run: `go test ./internal/database/...`
Expected: All PASS

- [ ] **Step 7: Commit**

```bash
git add internal/database/database.go internal/database/database_jobs.go internal/database/database_test.go
git commit -m "feat: add UpdateResumePosition and BatchSetWatched database methods"
```

---

### Task 3: API Endpoints for Watch Tracking

**Files:**
- Create: `internal/web/routes/watch.go`
- Modify: `cmd/moombox/main.go` (register new routes)
- Test: `internal/database/database_test.go` (integration via DB, no HTTP handler tests — follows existing pattern)

- [ ] **Step 1: Create watch route handler file**

Create `internal/web/routes/watch.go`:

```go
package routes

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// WatchRoutes registers watch tracking API routes.
func WatchRoutes(r chi.Router, db *database.Database, wsHub *web.WebSocketHub) {

	// PUT /api/jobs/{id}/resume-position — lightweight periodic save
	r.Put("/api/jobs/{id}/resume-position", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		var body struct {
			Position float64 `json:"position"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}
		db.UpdateResumePosition(jobID, body.Position)
		rw.WriteHeader(http.StatusNoContent)
	})

	// POST /api/jobs/{id}/resume-position — sendBeacon fallback (beacon only sends POST)
	r.Post("/api/jobs/{id}/resume-position", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		var body struct {
			Position float64 `json:"position"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		db.UpdateResumePosition(jobID, body.Position)
		rw.WriteHeader(http.StatusNoContent)
	})

	// POST /api/jobs/{id}/watched — mark single job as watched
	r.Post("/api/jobs/{id}/watched", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}
		db.UpdateJobFields(jobID, map[string]any{
			"watched":         1,
			"resume_position": nil,
		})
		updated, err := db.GetJob(jobID)
		if err != nil || updated == nil {
			jsonError(rw, "failed to read back job", http.StatusInternalServerError)
			return
		}
		jsonResponse(rw, updated)
	})

	// DELETE /api/jobs/{id}/watched — mark single job as unwatched
	r.Delete("/api/jobs/{id}/watched", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}
		db.UpdateJobFields(jobID, map[string]any{
			"watched":         0,
			"resume_position": nil,
		})
		updated, err := db.GetJob(jobID)
		if err != nil || updated == nil {
			jsonError(rw, "failed to read back job", http.StatusInternalServerError)
			return
		}
		jsonResponse(rw, updated)
	})

	// POST /api/jobs/batch/watched — batch mark as watched
	r.Post("/api/jobs/batch/watched", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			JobIDs []string `json:"jobIds"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}
		if len(body.JobIDs) == 0 {
			jsonError(rw, "jobIds required", http.StatusBadRequest)
			return
		}
		db.BatchSetWatched(body.JobIDs, true)
		jsonResponse(rw, map[string]any{"success": true})
	})

	// DELETE /api/jobs/batch/watched — batch mark as unwatched
	r.Delete("/api/jobs/batch/watched", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			JobIDs []string `json:"jobIds"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}
		if len(body.JobIDs) == 0 {
			jsonError(rw, "jobIds required", http.StatusBadRequest)
			return
		}
		db.BatchSetWatched(body.JobIDs, false)
		jsonResponse(rw, map[string]any{"success": true})
	})
}
```

- [ ] **Step 2: Register routes in main.go**

Find where `routes.JobRoutes(...)` is called in `cmd/moombox/main.go` and add the WatchRoutes call nearby:

```go
routes.WatchRoutes(r, db, wsHub)
```

- [ ] **Step 3: Verify it compiles**

Run: `go build ./cmd/moombox`
Expected: Builds without errors.

- [ ] **Step 4: Commit**

```bash
git add internal/web/routes/watch.go cmd/moombox/main.go
git commit -m "feat: add API endpoints for watch tracking (resume-position, watched, batch)"
```

---

### Task 4: Web UI — Thumbnail Eyeball Icons

**Files:**
- Modify: `web/public/app.js` (renderJobItem, updateJobCard)
- Modify: `web/public/moombox.css` (thumb overlay styles)

- [ ] **Step 1: Add CSS for the eyeball overlay**

In `web/public/moombox.css`, add after the `.thumb .job-checkbox` block (after line 162):

```css
.thumb .watch-indicator {
    position: absolute;
    bottom: 4px;
    right: 4px;
    z-index: 2;
    background: rgba(0, 0, 0, 0.7);
    border-radius: 10px;
    padding: 2px 6px;
    display: flex;
    align-items: center;
    line-height: 1;
}

.thumb .watch-indicator svg {
    width: 14px;
    height: 14px;
}
```

- [ ] **Step 2: Add eyeball icon helper to app.js**

In `web/public/app.js`, add a helper method to the MoomboxApp class (add near other utility methods):

```javascript
  /** Returns eyeball overlay HTML for a job thumbnail, or empty string. */
  watchIndicatorHtml(job) {
    if (job.status !== "Finished") return "";
    if (job.watched) {
      // Filled green eye
      return `<span class="watch-indicator"><svg viewBox="0 0 24 24" fill="#a6e3a1" stroke="#a6e3a1" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3" fill="#166534"/></svg></span>`;
    }
    if (job.resumePosition != null) {
      // Outlined amber eye
      return `<span class="watch-indicator"><svg viewBox="0 0 24 24" fill="none" stroke="#f9e2af" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg></span>`;
    }
    return "";
  }
```

- [ ] **Step 3: Add eyeball icon to renderJobItem**

In `web/public/app.js`, in the `renderJobItem` method (around line 1247-1252), modify the `.thumb` div to include the watch indicator after the image:

Replace the thumbnail HTML block:
```javascript
        <div class="thumb">
          <input type="checkbox" class="job-checkbox" data-job-id="${this.escapeHtml(job.id)}" ${isSelected ? "checked" : ""}>
          ${(thumbnailUrl || fallbackThumb) ? `<img src="${this.escapeHtml(thumbnailUrl || fallbackThumb)}" alt="" loading="lazy" referrerpolicy="no-referrer"
               class="${isAvatarThumb ? "thumb-avatar" : ""}"
               ${fallbackThumb ? `data-fallback="${this.escapeHtml(fallbackThumb)}"` : ""}>` : ""}
        </div>
```

With:
```javascript
        <div class="thumb">
          <input type="checkbox" class="job-checkbox" data-job-id="${this.escapeHtml(job.id)}" ${isSelected ? "checked" : ""}>
          ${(thumbnailUrl || fallbackThumb) ? `<img src="${this.escapeHtml(thumbnailUrl || fallbackThumb)}" alt="" loading="lazy" referrerpolicy="no-referrer"
               class="${isAvatarThumb ? "thumb-avatar" : ""}"
               ${fallbackThumb ? `data-fallback="${this.escapeHtml(fallbackThumb)}"` : ""}>` : ""}
          ${this.watchIndicatorHtml(job)}
        </div>
```

- [ ] **Step 4: Update watch indicator in updateJobCard**

In `web/public/app.js`, in the `updateJobCard` method (around line 1270), add logic to update the watch indicator without full re-render. Add after the thumbnail update section:

```javascript
    // Update watch indicator
    const existingIndicator = card.querySelector(".thumb .watch-indicator");
    const newIndicatorHtml = this.watchIndicatorHtml(job);
    if (newIndicatorHtml) {
      if (existingIndicator) {
        existingIndicator.outerHTML = newIndicatorHtml;
      } else {
        card.querySelector(".thumb")?.insertAdjacentHTML("beforeend", newIndicatorHtml);
      }
    } else if (existingIndicator) {
      existingIndicator.remove();
    }
```

- [ ] **Step 5: Verify with `go build`**

Run: `go build ./cmd/moombox`
Expected: Builds without errors (web assets are embedded at build time).

- [ ] **Step 6: Commit**

```bash
git add web/public/app.js web/public/moombox.css
git commit -m "feat: add eyeball watch indicator on job thumbnails"
```

---

### Task 5: Web UI — Batch Watched/Unwatched Buttons

**Files:**
- Modify: `web/public/index.html` (add batch buttons)
- Modify: `web/public/app.js` (wire up event listeners, add to batchAction switch, update visibility)

- [ ] **Step 1: Add batch buttons to HTML**

In `web/public/index.html`, add after the Delete button (after line 238) and before the `batch-clear` icon-button (line 240):

```html
                        <sl-button id="batch-watched" variant="success" size="small">
                            <sl-icon slot="prefix" name="eye"></sl-icon> Mark Watched
                        </sl-button>
                        <sl-button id="batch-unwatched" variant="neutral" size="small">
                            <sl-icon slot="prefix" name="eye-slash"></sl-icon> Mark Unwatched
                        </sl-button>
```

- [ ] **Step 2: Wire up event listeners in app.js**

In `web/public/app.js`, add after the `batch-delete` event listener (after line 490):

```javascript
    document.getElementById("batch-watched")?.addEventListener("click", () => this.batchAction("watched"));
    document.getElementById("batch-unwatched")?.addEventListener("click", () => this.batchAction("unwatched"));
```

- [ ] **Step 3: Add watched/unwatched cases to batchAction**

In `web/public/app.js`, in the `batchAction` method, add two new cases to the switch statement (after the `"delete"` case, before the closing of the switch):

```javascript
      case "watched":
        targets = selectedJobs.filter(j => j.status === "Finished" && !j.watched);
        confirmMsg = `Mark ${targets.length} job${targets.length !== 1 ? "s" : ""} as watched?`;
        apiCall = () => fetch("/api/jobs/batch/watched", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ jobIds: targets.map(j => j.id) }),
        });
        break;
      case "unwatched":
        targets = selectedJobs.filter(j => j.status === "Finished" && (j.watched || j.resumePosition != null));
        confirmMsg = `Mark ${targets.length} job${targets.length !== 1 ? "s" : ""} as unwatched?`;
        apiCall = () => fetch("/api/jobs/batch/watched", {
          method: "DELETE",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ jobIds: targets.map(j => j.id) }),
        });
        break;
```

Note: The batch watched/unwatched actions use a single API call rather than per-job calls, so `apiCall` is not a function of `id` — it sends all IDs at once. Update the execution section after the switch to handle this. Replace the `Promise.allSettled` block:

After the switch, before the `if (!targets || targets.length === 0) return;` line, the existing code calls `apiCall` per-job. For the batch watched cases, the call is already batched. The simplest approach: adjust the execution. After the confirm, handle the two patterns:

```javascript
    if (!targets || targets.length === 0) return;

    const confirmed = await this.showConfirm(confirmMsg, {
      title: "Batch " + action.charAt(0).toUpperCase() + action.slice(1),
      okLabel: action.charAt(0).toUpperCase() + action.slice(1),
      okVariant: action === "delete" ? "danger" : action === "cancel" ? "warning" : "primary"
    });
    if (!confirmed) return;

    let succeeded, failed;
    if (action === "watched" || action === "unwatched") {
      // Single batch API call
      try {
        const res = await apiCall();
        succeeded = res.ok ? targets.length : 0;
        failed = res.ok ? 0 : targets.length;
      } catch {
        succeeded = 0;
        failed = targets.length;
      }
    } else {
      // Per-job API calls
      const results = await Promise.allSettled(targets.map(j => apiCall(j.id)));
      succeeded = results.filter(r => r.status === "fulfilled" && r.value?.ok).length;
      failed = targets.length - succeeded;
    }

    this._selectedJobs.clear();
    this.updateBatchActionBar();

    if (failed === 0) {
      const verbs = { delete: "Deleted", cancel: "Cancelled", resume: "Resumed", reinitialize: "Reinitialized", watched: "Marked watched", unwatched: "Marked unwatched" };
      const verb = verbs[action] || action;
      this.showToast(`${verb}: ${succeeded} job${succeeded !== 1 ? "s" : ""}`, "success");
    } else {
      this.showToast(`${succeeded} of ${targets.length} succeeded. ${failed} failed.`, "warning");
    }
```

- [ ] **Step 4: Update batch action bar visibility**

In `web/public/app.js`, in `updateBatchActionBar` (around line 3261-3271), add visibility logic for the watch buttons:

```javascript
    const canWatch = selectedJobs.some(j => j.status === "Finished" && !j.watched);
    const canUnwatch = selectedJobs.some(j => j.status === "Finished" && (j.watched || j.resumePosition != null));

    document.getElementById("batch-watched").style.display = canWatch ? "" : "none";
    document.getElementById("batch-unwatched").style.display = canUnwatch ? "" : "none";
```

Add this after the existing `canDelete` line and its corresponding `style.display` assignment.

- [ ] **Step 5: Verify with `go build`**

Run: `go build ./cmd/moombox`
Expected: Builds without errors.

- [ ] **Step 6: Commit**

```bash
git add web/public/index.html web/public/app.js
git commit -m "feat: add batch mark watched/unwatched buttons to action bar"
```

---

### Task 6: Web UI — Job Details (Watch Pill & Action Buttons)

**Files:**
- Modify: `web/public/app.js` (renderJobDetails — add pill and buttons)
- Modify: `web/public/moombox.css` (pill styles)

- [ ] **Step 1: Add CSS for watch pills**

In `web/public/moombox.css`, add:

```css
.watch-pill {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    padding: 2px 8px;
    border-radius: 12px;
    font-size: 0.8rem;
    line-height: 1.4;
}

.watch-pill svg {
    width: 12px;
    height: 12px;
    flex-shrink: 0;
}

.watch-pill.watched {
    background: #dcfce7;
    color: #166534;
}

.watch-pill.in-progress {
    background: #fef3c7;
    color: #92400e;
}
```

- [ ] **Step 2: Add watch pill helper to app.js**

In `web/public/app.js`, add a helper method:

```javascript
  /** Returns the compact watch status pill HTML for job details. */
  watchPillHtml(job) {
    if (job.status !== "Finished") return "";
    const resumeStr = job.resumePosition != null ? this.formatDuration(job.resumePosition) : "";
    if (job.watched && resumeStr) {
      return `<span class="watch-pill watched"><svg viewBox="0 0 24 24" fill="currentColor" stroke="currentColor" stroke-width="2"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3" fill="#dcfce7"/></svg>Watched · Paused at ${this.escapeHtml(resumeStr)}</span>`;
    }
    if (job.watched) {
      return `<span class="watch-pill watched"><svg viewBox="0 0 24 24" fill="currentColor" stroke="currentColor" stroke-width="2"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3" fill="#dcfce7"/></svg>Watched</span>`;
    }
    if (resumeStr) {
      return `<span class="watch-pill in-progress"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg>Paused at ${this.escapeHtml(resumeStr)}</span>`;
    }
    return "";
  }
```

- [ ] **Step 3: Add formatDuration helper if not present**

Check if `formatDuration` already exists. If not, add to app.js:

```javascript
  /** Formats seconds into H:MM:SS or M:SS. */
  formatDuration(totalSeconds) {
    const s = Math.floor(totalSeconds);
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    const sec = s % 60;
    if (h > 0) return `${h}:${String(m).padStart(2, "0")}:${String(sec).padStart(2, "0")}`;
    return `${m}:${String(sec).padStart(2, "0")}`;
  }
```

- [ ] **Step 4: Insert watch pill into renderJobDetails**

In `web/public/app.js`, in `renderJobDetails`, add the watch pill and action buttons after the Status row (after line 1622). Insert inside the details-section, after the status badge row:

```javascript
          ${this.watchPillHtml(job)}
          ${job.status === "Finished" ? `
          <div class="details-row" id="watch-actions-row">
            <span class="details-label"></span>
            <span class="details-value">
              ${!job.watched ? `<sl-button id="details-mark-watched" variant="success" size="small"><sl-icon slot="prefix" name="eye"></sl-icon> Mark Watched</sl-button>` : ""}
              ${job.watched || job.resumePosition != null ? `<sl-button id="details-mark-unwatched" variant="neutral" size="small"><sl-icon slot="prefix" name="eye-slash"></sl-icon> Mark Unwatched</sl-button>` : ""}
            </span>
          </div>` : ""}
```

- [ ] **Step 5: Wire up watch action button click handlers**

In `web/public/app.js`, in the `renderJobDetails` method, after `content.innerHTML = ...` is set, add click handlers (add near where other detail button handlers are wired):

```javascript
    document.getElementById("details-mark-watched")?.addEventListener("click", async () => {
      await fetch(`/api/jobs/${job.id}/watched`, { method: "POST" });
    });
    document.getElementById("details-mark-unwatched")?.addEventListener("click", async () => {
      await fetch(`/api/jobs/${job.id}/watched`, { method: "DELETE" });
    });
```

The WebSocket `job_update` broadcast from the server will automatically trigger `updateJobDetails` to refresh the detail view with the new state.

- [ ] **Step 6: Verify with `go build`**

Run: `go build ./cmd/moombox`
Expected: Builds without errors.

- [ ] **Step 7: Commit**

```bash
git add web/public/app.js web/public/moombox.css
git commit -m "feat: add watch status pill and mark watched/unwatched buttons to job details"
```

---

### Task 7: Web UI — Player Resume Dialog & Periodic Save

**Files:**
- Modify: `web/public/modules/player.js` (resume dialog, periodic save, watched detection)
- Modify: `web/public/moombox.css` (resume overlay styles)

- [ ] **Step 1: Add CSS for resume dialog overlay**

In `web/public/moombox.css`, add:

```css
.resume-overlay {
    position: absolute;
    top: 0;
    left: 0;
    right: 0;
    bottom: 0;
    background: rgba(0, 0, 0, 0.85);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 10;
    border-radius: var(--sl-border-radius-medium);
}

.resume-overlay-content {
    text-align: center;
    color: #fff;
}

.resume-overlay-content p {
    margin-bottom: 1rem;
    font-size: 1.1rem;
}

.resume-overlay-content .resume-actions {
    display: flex;
    gap: 0.75rem;
    justify-content: center;
    flex-wrap: wrap;
}
```

- [ ] **Step 2: Add resume/watch tracking state to PlayerController**

In `web/public/modules/player.js`, in the constructor (around line 7-29), add tracking fields:

```javascript
    this._watchSaveInterval = null;
    this._watchedTriggered = false;
```

- [ ] **Step 3: Add resume dialog logic to onPlayerJobSelect**

In `web/public/modules/player.js`, in `onPlayerJobSelect` (around line 534), after the video source is set (after line 570, after the multi-segment/single-file source assignment), add the resume dialog:

```javascript
    // Watch tracking: show resume dialog or start from beginning
    this._clearWatchTracking();
    this._watchedTriggered = false;
    const resumePos = this.playerJob.resumePosition;

    if (resumePos != null && resumePos > 0) {
      this._showResumeDialog(jobId, resumePos);
    } else {
      this._startWatchTracking(jobId);
    }
```

- [ ] **Step 4: Implement _showResumeDialog method**

Add to the PlayerController class:

```javascript
  _showResumeDialog(jobId, resumeSeconds) {
    const wrapper = document.getElementById("player-video-wrapper");
    // Remove any existing overlay
    wrapper.querySelector(".resume-overlay")?.remove();

    const formatted = this._formatDuration(resumeSeconds);
    const overlay = document.createElement("div");
    overlay.className = "resume-overlay";
    overlay.innerHTML = `
      <div class="resume-overlay-content">
        <p>Resume where you left off?</p>
        <div class="resume-actions">
          <sl-button variant="primary" size="medium" id="resume-continue">
            <sl-icon slot="prefix" name="play-fill"></sl-icon> Resume from ${formatted}
          </sl-button>
          <sl-button variant="neutral" size="medium" id="resume-start">
            Start from beginning
          </sl-button>
        </div>
      </div>
    `;
    wrapper.appendChild(overlay);

    overlay.querySelector("#resume-continue").addEventListener("click", () => {
      overlay.remove();
      const video = document.getElementById("player-video");
      if (this._seg.active) {
        this._seg.seekToGlobalTime(resumeSeconds, video);
      } else {
        video.currentTime = resumeSeconds;
      }
      video.play();
      this._startWatchTracking(jobId);
    });

    overlay.querySelector("#resume-start").addEventListener("click", () => {
      overlay.remove();
      document.getElementById("player-video").play();
      this._startWatchTracking(jobId);
    });
  }

  _formatDuration(totalSeconds) {
    const s = Math.floor(totalSeconds);
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    const sec = s % 60;
    if (h > 0) return `${h}:${String(m).padStart(2, "0")}:${String(sec).padStart(2, "0")}`;
    return `${m}:${String(sec).padStart(2, "0")}`;
  }
```

- [ ] **Step 5: Implement periodic save and watched detection**

Add to the PlayerController class:

```javascript
  _startWatchTracking(jobId) {
    this._clearWatchTracking();

    // Periodic save every 10 seconds
    this._watchSaveInterval = setInterval(() => {
      const video = document.getElementById("player-video");
      if (!video || video.paused) return;
      const pos = this._seg.active ? this._seg.getGlobalTime(video) : video.currentTime;
      this._saveResumePosition(jobId, pos);
      this._checkWatched(jobId, video);
    }, 10000);

    // Save on pause
    const video = document.getElementById("player-video");
    this._onPauseSave = () => {
      const pos = this._seg.active ? this._seg.getGlobalTime(video) : video.currentTime;
      this._saveResumePosition(jobId, pos);
    };
    video.addEventListener("pause", this._onPauseSave);

    // Save on tab close
    this._onBeforeUnload = () => {
      const pos = this._seg.active ? this._seg.getGlobalTime(video) : video.currentTime;
      const blob = new Blob([JSON.stringify({ position: pos })], { type: "application/json" });
      navigator.sendBeacon(`/api/jobs/${jobId}/resume-position`, blob);
    };
    window.addEventListener("beforeunload", this._onBeforeUnload);
  }

  _clearWatchTracking() {
    if (this._watchSaveInterval) {
      clearInterval(this._watchSaveInterval);
      this._watchSaveInterval = null;
    }
    const video = document.getElementById("player-video");
    if (this._onPauseSave) {
      video?.removeEventListener("pause", this._onPauseSave);
      this._onPauseSave = null;
    }
    if (this._onBeforeUnload) {
      window.removeEventListener("beforeunload", this._onBeforeUnload);
      this._onBeforeUnload = null;
    }
  }

  _saveResumePosition(jobId, position) {
    fetch(`/api/jobs/${jobId}/resume-position`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ position }),
    }).catch(() => {}); // Fire-and-forget
  }

  _checkWatched(jobId, video) {
    if (this._watchedTriggered) return;

    let currentPos, totalDuration;
    if (this._seg.active) {
      currentPos = this._seg.getGlobalTime(video);
      totalDuration = this._seg.totalDuration;
    } else {
      currentPos = video.currentTime;
      totalDuration = video.duration;
    }

    if (!totalDuration || !isFinite(totalDuration)) return;

    const withinThreshold =
      (totalDuration - currentPos <= 30) ||
      (currentPos / totalDuration >= 0.95);

    if (withinThreshold) {
      this._watchedTriggered = true;
      this._clearWatchTracking();
      fetch(`/api/jobs/${jobId}/watched`, { method: "POST" }).catch(() => {});
    }
  }
```

- [ ] **Step 6: Clean up watch tracking when player is closed or job changes**

In `web/public/modules/player.js`, find where the player is reset/closed (in `onPlayerJobSelect` at the top, before loading a new job). The `_clearWatchTracking()` call from Step 3 handles the job-change case. Also add cleanup when the player tab is hidden — find the existing player cleanup/reset logic and add `this._clearWatchTracking()`.

- [ ] **Step 7: Handle video `ended` event for watched detection**

In the existing `ended` event listener in `initPlayer` (around line 117), add watched detection as a fallback:

```javascript
    // Add to the existing 'ended' event handler
    video.addEventListener("ended", () => {
      // ... existing segment auto-advance logic ...
      // Watched detection fallback for when the video plays to natural end
      if (this.playerJob && !this._watchedTriggered) {
        this._watchedTriggered = true;
        this._clearWatchTracking();
        fetch(`/api/jobs/${this.playerJob.id}/watched`, { method: "POST" }).catch(() => {});
      }
    });
```

Integrate this with the existing `ended` handler — don't replace it, add the watched logic alongside the existing segment auto-advance code.

- [ ] **Step 8: Verify with `go build`**

Run: `go build ./cmd/moombox`
Expected: Builds without errors.

- [ ] **Step 9: Commit**

```bash
git add web/public/modules/player.js web/public/moombox.css
git commit -m "feat: add resume dialog, periodic position save, and auto-watched detection to player"
```

---

### Task 8: Build Verification & Manual Testing

**Files:** None (verification only)

- [ ] **Step 1: Run full Go test suite**

Run: `go test ./...`
Expected: All tests PASS.

- [ ] **Step 2: Run static analysis**

Run: `go vet ./...`
Expected: No issues.

- [ ] **Step 3: Build the binary**

Run: `go build -o moombox.exe ./cmd/moombox`
Expected: Builds successfully.

- [ ] **Step 4: Manual smoke test checklist**

Launch the binary and verify in the Web UI:

1. Open a finished job's details — watch pill should not appear (unwatched, no resume)
2. Click "Mark as Watched" — green "Watched" pill appears, button changes to "Mark as Unwatched"
3. Click "Mark as Unwatched" — pill disappears, button changes back
4. Open a finished video in the player — plays from beginning (no resume dialog)
5. Watch for 15+ seconds, then close/switch away — resume position is saved
6. Re-open the same video — resume dialog appears with correct timestamp
7. Click "Resume" — video seeks to saved position
8. Watch a video to near the end (within 30s) — auto-marked as watched
9. Verify eyeball icons appear on thumbnails: green filled for watched, outlined amber for partially watched
10. Select multiple finished jobs with checkboxes — "Mark Watched" and "Mark Unwatched" batch buttons appear
11. Use batch "Mark Watched" — all selected finished jobs get green eyeball icons

- [ ] **Step 5: Commit any fixes from manual testing**

If any issues are found during manual testing, fix and commit them individually.
