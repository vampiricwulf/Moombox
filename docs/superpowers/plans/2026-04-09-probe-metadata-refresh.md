# Probe Metadata Refresh Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Use metadata already returned by lightweight probes during the Upcoming phase to keep job data fresh, and unify the two duplicated metadata update functions into one.

**Architecture:** Replace `updateJobMetadata()` (fill-blanks) and `updateJobMetadataOnLive()` (always-overwrite) with a single `updateJobMetadata(job, info, overwrite)`. In the `waitForLive` polling loop, call the unified function with `overwrite=true` after each successful probe. Change detection compares against current job fields to avoid unnecessary DB writes. Add a "Schedule Changed" notification when `stream_start_time` changes, registered as a filterable event in both TUI and Web UI.

**Tech Stack:** Go, SQLite (via `database.Database`), notifications system, Shoelace Web UI

**Spec:** `docs/superpowers/specs/2026-04-09-probe-metadata-refresh-design.md`

---

### Task 1: Register `rescheduled` notification event in TUI and Web UI

**Files:**
- Modify: `internal/tui/settings.go:172`
- Modify: `web/public/modules/settings.js:9-21`

- [ ] **Step 1: Add `rescheduled` to TUI event groups**

In `internal/tui/settings.go`, line 172, add `"rescheduled"` after `"scheduled"` in the Job Lifecycle group:

```go
{"Job Lifecycle", []string{"found", "added", "scheduled", "rescheduled", "live", "downloading", "quality_split", "muxing", "finished", "error", "cancelled", "auth"}},
```

- [ ] **Step 2: Add `rescheduled` to Web UI event groups**

In `web/public/modules/settings.js`, add a new entry after the `scheduled` line (line 12). The Job Lifecycle events array becomes:

```javascript
events: [
  { id: "found", label: "Found" },
  { id: "added", label: "Added" },
  { id: "scheduled", label: "Scheduled" },
  { id: "rescheduled", label: "Rescheduled" },
  { id: "live", label: "Live" },
  { id: "downloading", label: "Downloading" },
  { id: "quality_split", label: "Quality Split" },
  { id: "muxing", label: "Muxing" },
  { id: "finished", label: "Finished" },
  { id: "error", label: "Error" },
  { id: "cancelled", label: "Cancelled" },
  { id: "auth", label: "Auth" },
],
```

- [ ] **Step 3: Build to verify**

Run: `go build ./...`
Expected: Clean build, no errors.

- [ ] **Step 4: Commit**

```bash
git add internal/tui/settings.go web/public/modules/settings.js
git commit -m "feat: register rescheduled notification event in TUI and Web UI"
```

---

### Task 2: Write tests for unified `updateJobMetadata`

**Files:**
- Create: `internal/worker/stream_processor_metadata_test.go`

The tests need a real SQLite database because `StreamProcessor.db` is `*database.Database` (concrete type). Create a test helper and write all test cases before touching implementation.

- [ ] **Step 1: Write test file with helper and all test cases**

Create `internal/worker/stream_processor_metadata_test.go`:

```go
package worker

import (
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// newTestSP creates a StreamProcessor with a real temp database for testing.
func newTestSP(t *testing.T) (*StreamProcessor, *database.Database) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	sp := &StreamProcessor{db: db}
	return sp, db
}

// addTestJob inserts a job and returns a pointer to it.
func addTestJob(t *testing.T, db *database.Database, job *database.Job) *database.Job {
	t.Helper()
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func intPtr(v int) *int { return &v }

func TestUpdateJobMetadata_FillBlanks(t *testing.T) {
	sp, db := newTestSP(t)

	job := addTestJob(t, db, &database.Job{
		ID:      "yt_test1",
		VideoID: "test1",
		Status:  database.StatusUpcoming,
	})

	info := &youtube.VideoInfo{
		Title:              "My Stream",
		ChannelName:        "My Channel",
		ThumbnailURL:       "https://example.com/thumb.jpg",
		Description:        "A description",
		ScheduledStartTime: "2026-04-10T12:00:00Z",
		EndTimestamp:        "2026-04-10T14:00:00Z",
		LengthSeconds:      intPtr(7200),
		IsUpcoming:         true,
	}

	sp.updateJobMetadata(job, info, false)

	got, _ := db.GetJob("yt_test1")
	if got.Title != "My Stream" {
		t.Errorf("title = %q, want %q", got.Title, "My Stream")
	}
	if got.ChannelName != "My Channel" {
		t.Errorf("channel_name = %q, want %q", got.ChannelName, "My Channel")
	}
	if got.ThumbnailURL != "https://example.com/thumb.jpg" {
		t.Errorf("thumbnail_url = %q, want %q", got.ThumbnailURL, "https://example.com/thumb.jpg")
	}
	if got.Description != "A description" {
		t.Errorf("description = %q, want %q", got.Description, "A description")
	}
	if got.StreamStartTime != "2026-04-10T12:00:00Z" {
		t.Errorf("stream_start_time = %q, want %q", got.StreamStartTime, "2026-04-10T12:00:00Z")
	}
	if got.StreamEndTime != "2026-04-10T14:00:00Z" {
		t.Errorf("stream_end_time = %q, want %q", got.StreamEndTime, "2026-04-10T14:00:00Z")
	}
	if got.LengthSeconds == nil || *got.LengthSeconds != 7200 {
		t.Errorf("length_seconds = %v, want 7200", got.LengthSeconds)
	}
}

func TestUpdateJobMetadata_FillBlanksDoesNotOverwrite(t *testing.T) {
	sp, db := newTestSP(t)

	job := addTestJob(t, db, &database.Job{
		ID:              "yt_test2",
		VideoID:         "test2",
		Status:          database.StatusUpcoming,
		Title:           "Original Title",
		StreamStartTime: "2026-04-10T10:00:00Z",
		StreamEndTime:   "2026-04-10T12:00:00Z",
	})

	info := &youtube.VideoInfo{
		Title:              "New Title",
		ScheduledStartTime: "2026-04-10T15:00:00Z",
		EndTimestamp:        "2026-04-10T17:00:00Z",
	}

	sp.updateJobMetadata(job, info, false)

	got, _ := db.GetJob("yt_test2")
	// stream_start_time and stream_end_time should NOT be overwritten in fill-blanks mode
	if got.StreamStartTime != "2026-04-10T10:00:00Z" {
		t.Errorf("stream_start_time = %q, want %q (should not overwrite)", got.StreamStartTime, "2026-04-10T10:00:00Z")
	}
	if got.StreamEndTime != "2026-04-10T12:00:00Z" {
		t.Errorf("stream_end_time = %q, want %q (should not overwrite)", got.StreamEndTime, "2026-04-10T12:00:00Z")
	}
}

func TestUpdateJobMetadata_OverwriteDetectsChanges(t *testing.T) {
	sp, db := newTestSP(t)

	job := addTestJob(t, db, &database.Job{
		ID:              "yt_test3",
		VideoID:         "test3",
		Status:          database.StatusUpcoming,
		Title:           "Old Title",
		ChannelName:     "Old Channel",
		ThumbnailURL:    "https://example.com/old.jpg",
		Description:     "Old description",
		StreamStartTime: "2026-04-10T10:00:00Z",
		StreamEndTime:   "2026-04-10T12:00:00Z",
		LengthSeconds:   intPtr(3600),
	})

	info := &youtube.VideoInfo{
		Title:              "New Title",
		ChannelName:        "New Channel",
		ThumbnailURL:       "https://example.com/new.jpg",
		Description:        "New description",
		ScheduledStartTime: "2026-04-10T15:00:00Z",
		EndTimestamp:        "2026-04-10T17:00:00Z",
		LengthSeconds:      intPtr(7200),
	}

	sp.updateJobMetadata(job, info, true)

	got, _ := db.GetJob("yt_test3")
	if got.Title != "New Title" {
		t.Errorf("title = %q, want %q", got.Title, "New Title")
	}
	if got.ChannelName != "New Channel" {
		t.Errorf("channel_name = %q, want %q", got.ChannelName, "New Channel")
	}
	if got.ThumbnailURL != "https://example.com/new.jpg" {
		t.Errorf("thumbnail_url = %q, want %q", got.ThumbnailURL, "https://example.com/new.jpg")
	}
	if got.Description != "New description" {
		t.Errorf("description = %q, want %q", got.Description, "New description")
	}
	if got.StreamStartTime != "2026-04-10T15:00:00Z" {
		t.Errorf("stream_start_time = %q, want %q", got.StreamStartTime, "2026-04-10T15:00:00Z")
	}
	if got.StreamEndTime != "2026-04-10T17:00:00Z" {
		t.Errorf("stream_end_time = %q, want %q", got.StreamEndTime, "2026-04-10T17:00:00Z")
	}
	if got.LengthSeconds == nil || *got.LengthSeconds != 7200 {
		t.Errorf("length_seconds = %v, want 7200", got.LengthSeconds)
	}
}

func TestUpdateJobMetadata_OverwriteSkipsUnchanged(t *testing.T) {
	sp, db := newTestSP(t)

	job := addTestJob(t, db, &database.Job{
		ID:              "yt_test4",
		VideoID:         "test4",
		Status:          database.StatusUpcoming,
		Title:           "Same Title",
		ChannelName:     "Same Channel",
		ThumbnailURL:    "https://example.com/same.jpg",
		Description:     "Same description",
		StreamStartTime: "2026-04-10T10:00:00Z",
		StreamEndTime:   "2026-04-10T12:00:00Z",
		LengthSeconds:   intPtr(3600),
	})

	// Record the updatedAt before calling update
	beforeUpdate := job.UpdatedAt

	info := &youtube.VideoInfo{
		Title:              "Same Title",
		ChannelName:        "Same Channel",
		ThumbnailURL:       "https://example.com/same.jpg",
		Description:        "Same description",
		ScheduledStartTime: "2026-04-10T10:00:00Z",
		EndTimestamp:        "2026-04-10T12:00:00Z",
		LengthSeconds:      intPtr(3600),
	}

	sp.updateJobMetadata(job, info, true)

	got, _ := db.GetJob("yt_test4")
	// updatedAt should NOT have changed because nothing was different
	if got.UpdatedAt != beforeUpdate {
		t.Errorf("updatedAt changed from %q to %q — expected no DB write when nothing changed", beforeUpdate, got.UpdatedAt)
	}
}

func TestUpdateJobMetadata_IgnoresUnknownTitle(t *testing.T) {
	sp, db := newTestSP(t)

	job := addTestJob(t, db, &database.Job{
		ID:      "yt_test5",
		VideoID: "test5",
		Status:  database.StatusUpcoming,
		Title:   "Good Title",
	})

	info := &youtube.VideoInfo{
		Title:       "Unknown Title",
		ChannelName: "Unknown Channel",
	}

	sp.updateJobMetadata(job, info, true)

	got, _ := db.GetJob("yt_test5")
	if got.Title != "Good Title" {
		t.Errorf("title = %q, want %q (should ignore 'Unknown Title')", got.Title, "Good Title")
	}
	if got.ChannelName != "" {
		t.Errorf("channel_name = %q, want empty (should ignore 'Unknown Channel')", got.ChannelName)
	}
}

func TestUpdateJobMetadata_SyncsLocalJobObject(t *testing.T) {
	sp, db := newTestSP(t)

	job := addTestJob(t, db, &database.Job{
		ID:      "yt_test6",
		VideoID: "test6",
		Status:  database.StatusUpcoming,
	})

	info := &youtube.VideoInfo{
		Title:              "Updated Title",
		ChannelName:        "Updated Channel",
		ThumbnailURL:       "https://example.com/updated.jpg",
		ScheduledStartTime: "2026-04-10T12:00:00Z",
		EndTimestamp:        "2026-04-10T14:00:00Z",
	}

	sp.updateJobMetadata(job, info, false)

	// Verify local job object was updated (not just DB)
	if job.Title != "Updated Title" {
		t.Errorf("local job.Title = %q, want %q", job.Title, "Updated Title")
	}
	if job.ChannelName != "Updated Channel" {
		t.Errorf("local job.ChannelName = %q, want %q", job.ChannelName, "Updated Channel")
	}
	if job.ThumbnailURL != "https://example.com/updated.jpg" {
		t.Errorf("local job.ThumbnailURL = %q, want %q", job.ThumbnailURL, "https://example.com/updated.jpg")
	}
	if job.StreamStartTime != "2026-04-10T12:00:00Z" {
		t.Errorf("local job.StreamStartTime = %q, want %q", job.StreamStartTime, "2026-04-10T12:00:00Z")
	}
	if job.StreamEndTime != "2026-04-10T14:00:00Z" {
		t.Errorf("local job.StreamEndTime = %q, want %q", job.StreamEndTime, "2026-04-10T14:00:00Z")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -v -run TestUpdateJobMetadata ./internal/worker/...`
Expected: Compilation error — `updateJobMetadata` has wrong signature (2 args, not 3).

- [ ] **Step 3: Commit test file**

```bash
git add internal/worker/stream_processor_metadata_test.go
git commit -m "test: add tests for unified updateJobMetadata"
```

---

### Task 3: Implement unified `updateJobMetadata`

**Files:**
- Modify: `internal/worker/stream_processor.go:236-353`

Replace both `updateJobMetadata()` and `updateJobMetadataOnLive()` with a single unified function.

- [ ] **Step 1: Replace both functions with unified implementation**

Delete lines 236-353 (both `updateJobMetadata` and `updateJobMetadataOnLive`) and replace with:

```go
// updateJobMetadata updates job metadata from video info.
//
// When overwrite is false (initial fetch): fills blank fields only.
// When overwrite is true (probe refresh / live transition): overwrites fields
// that have actually changed, skipping DB writes when nothing differs.
//
// In both modes, guards apply: empty strings and "Unknown Title"/"Unknown Channel"
// sentinel values are never written.
func (sp *StreamProcessor) updateJobMetadata(job *database.Job, info *youtube.VideoInfo, overwrite bool) {
	updates := map[string]any{}
	notifyStartTimeConfirmed := false
	notifyScheduleChanged := false
	oldStartTime := ""

	// Title
	if info.Title != "" && info.Title != "Unknown Title" {
		if overwrite {
			if info.Title != job.Title {
				updates["title"] = info.Title
			}
		} else {
			updates["title"] = info.Title
		}
	}

	// Channel name
	if info.ChannelName != "" && info.ChannelName != "Unknown Channel" {
		if overwrite {
			if info.ChannelName != job.ChannelName {
				updates["channel_name"] = info.ChannelName
			}
		} else {
			updates["channel_name"] = info.ChannelName
		}
	}

	// Thumbnail
	if info.ThumbnailURL != "" {
		if overwrite {
			if info.ThumbnailURL != job.ThumbnailURL {
				updates["thumbnail_url"] = info.ThumbnailURL
			}
		} else {
			updates["thumbnail_url"] = info.ThumbnailURL
		}
	}

	// Description
	if info.Description != "" {
		if overwrite {
			if info.Description != job.Description {
				updates["description"] = info.Description
			}
		} else {
			updates["description"] = info.Description
		}
	}

	// Stream start time
	if info.ScheduledStartTime != "" {
		if overwrite {
			if info.ScheduledStartTime != job.StreamStartTime {
				if job.StreamStartTime == "" {
					notifyStartTimeConfirmed = true
				} else {
					notifyScheduleChanged = true
					oldStartTime = job.StreamStartTime
				}
				updates["stream_start_time"] = info.ScheduledStartTime
			}
		} else if job.StreamStartTime == "" {
			updates["stream_start_time"] = info.ScheduledStartTime
			notifyStartTimeConfirmed = true
		}
	}

	// Stream end time
	if info.EndTimestamp != "" {
		if overwrite {
			if info.EndTimestamp != job.StreamEndTime {
				updates["stream_end_time"] = info.EndTimestamp
			}
		} else if job.StreamEndTime == "" {
			updates["stream_end_time"] = info.EndTimestamp
		}
	}

	// Length
	if info.LengthSeconds != nil && *info.LengthSeconds > 0 {
		if overwrite {
			if job.LengthSeconds == nil || *info.LengthSeconds != *job.LengthSeconds {
				updates["length_seconds"] = *info.LengthSeconds
			}
		} else {
			updates["length_seconds"] = *info.LengthSeconds
		}
	}

	if len(updates) > 0 {
		sp.db.UpdateJobFields(job.ID, updates)

		// Sync local job object
		if v, ok := updates["title"].(string); ok {
			job.Title = v
		}
		if v, ok := updates["channel_name"].(string); ok {
			job.ChannelName = v
		}
		if v, ok := updates["thumbnail_url"].(string); ok {
			job.ThumbnailURL = v
		}
		if v, ok := updates["description"].(string); ok {
			job.Description = v
		}
		if v, ok := updates["stream_start_time"].(string); ok {
			job.StreamStartTime = v
		}
		if v, ok := updates["stream_end_time"].(string); ok {
			job.StreamEndTime = v
		}
		if v, ok := updates["length_seconds"].(int); ok {
			job.LengthSeconds = &v
		}
	}

	// Notifications
	if notifyStartTimeConfirmed && (info.IsUpcoming || info.IsLive) && sp.notifier != nil {
		startsAt := info.ScheduledStartTime
		if t, err := time.Parse(time.RFC3339, startsAt); err == nil {
			startsAt = t.Format("2006-01-02 15:04:05")
		}
		sp.notifier.Send("Start Time Confirmed",
			fmt.Sprintf("Scheduled: %s", job.Title),
			notifications.TypeInfo,
			[]notifications.Field{
				{Name: "Channel", Value: job.ChannelName, Inline: true},
				{Name: "Starts At", Value: startsAt, Inline: true},
			},
			notifications.SendOptions{
				URL:       job.URL,
				Thumbnail: job.ThumbnailURL,
				Event:     "scheduled",
			},
		)
	}

	if notifyScheduleChanged && sp.notifier != nil {
		fmtTime := func(raw string) string {
			if t, err := time.Parse(time.RFC3339, raw); err == nil {
				return t.Format("2006-01-02 15:04:05")
			}
			return raw
		}
		sp.notifier.Send("Schedule Changed",
			fmt.Sprintf("Rescheduled: %s", job.Title),
			notifications.TypeInfo,
			[]notifications.Field{
				{Name: "Channel", Value: job.ChannelName, Inline: true},
				{Name: "Old Time", Value: fmtTime(oldStartTime), Inline: true},
				{Name: "New Time", Value: fmtTime(info.ScheduledStartTime), Inline: true},
			},
			notifications.SendOptions{
				URL:       job.URL,
				Thumbnail: job.ThumbnailURL,
				Event:     "rescheduled",
			},
		)
	}
}
```

- [ ] **Step 2: Run the tests**

Run: `go test -v -run TestUpdateJobMetadata ./internal/worker/...`
Expected: All 6 tests pass.

- [ ] **Step 3: Run full test suite to check for breakage**

Run: `go test ./...`
Expected: All tests pass. The call site in `Process()` (line 122) still calls `sp.updateJobMetadata(job, info)` with 2 args — this will fail. That's expected; we fix it in the next task.

- [ ] **Step 4: Commit**

```bash
git add internal/worker/stream_processor.go
git commit -m "feat: unify updateJobMetadata with overwrite mode and change detection"
```

---

### Task 4: Update all call sites

**Files:**
- Modify: `internal/worker/stream_processor.go:122,140`
- Modify: `internal/worker/stream_processor_youtube.go:146-155,190,215,246,257`

- [ ] **Step 1: Update `Process()` initial fetch call site**

In `internal/worker/stream_processor.go`, line 122, change:

```go
sp.updateJobMetadata(job, info)
```

to:

```go
sp.updateJobMetadata(job, info, false)
```

- [ ] **Step 2: Update `handleStreamStatus()` Live transition call site**

In `internal/worker/stream_processor.go`, line 140, change:

```go
sp.updateJobMetadataOnLive(job, info)
```

to:

```go
// Fallback: use current time if YouTube didn't provide a start time
if info.ScheduledStartTime == "" && job.StreamStartTime == "" {
	now := time.Now().UTC().Format(time.RFC3339)
	info.ScheduledStartTime = now
}
sp.updateJobMetadata(job, info, true)
```

- [ ] **Step 3: Update `waitForLive` — replace dedicated stream_start_time block with probe metadata refresh**

In `internal/worker/stream_processor_youtube.go`, replace lines 146-155:

```go
		// Update scheduled start time + persist to DB if not yet stored
		if probeInfo.ScheduledStartTime != "" {
			scheduledStartTime = probeInfo.ScheduledStartTime
			if job.StreamStartTime == "" {
				job.StreamStartTime = probeInfo.ScheduledStartTime
				sp.db.UpdateJobFields(job.ID, map[string]any{
					"stream_start_time": probeInfo.ScheduledStartTime,
				})
			}
		}
```

with:

```go
		// Update local scheduledStartTime for interval calculation
		if probeInfo.ScheduledStartTime != "" {
			scheduledStartTime = probeInfo.ScheduledStartTime
		}
		// Persist any metadata changes (change-detected, zero-cost if nothing differs)
		sp.updateJobMetadata(job, probeInfo, true)
```

- [ ] **Step 4: Update `waitForLive` Live transition call site**

In `internal/worker/stream_processor_youtube.go`, line 190, change:

```go
sp.updateJobMetadataOnLive(job, fullInfo)
```

to:

```go
if fullInfo.ScheduledStartTime == "" && job.StreamStartTime == "" {
	fullInfo.ScheduledStartTime = time.Now().UTC().Format(time.RFC3339)
}
sp.updateJobMetadata(job, fullInfo, true)
```

- [ ] **Step 5: Update `waitForLive` VOD transition call site**

In `internal/worker/stream_processor_youtube.go`, line 215, change:

```go
sp.updateJobMetadataOnLive(job, fullInfo)
```

to:

```go
sp.updateJobMetadata(job, fullInfo, true)
```

- [ ] **Step 6: Update `waitForLive` unclear auth probe — Live and VOD call sites**

In `internal/worker/stream_processor_youtube.go`, line 246, change:

```go
sp.updateJobMetadataOnLive(job, fullInfo)
```

to:

```go
if fullInfo.ScheduledStartTime == "" && job.StreamStartTime == "" {
	fullInfo.ScheduledStartTime = time.Now().UTC().Format(time.RFC3339)
}
sp.updateJobMetadata(job, fullInfo, true)
```

And line 257, change:

```go
sp.updateJobMetadataOnLive(job, fullInfo)
```

to:

```go
sp.updateJobMetadata(job, fullInfo, true)
```

- [ ] **Step 7: Build and test**

Run: `go build ./... && go test ./...`
Expected: Clean build, all tests pass.

- [ ] **Step 8: Commit**

```bash
git add internal/worker/stream_processor.go internal/worker/stream_processor_youtube.go
git commit -m "feat: wire unified updateJobMetadata into all call sites

Use probe metadata during Upcoming phase for free metadata refresh.
Replace dedicated stream_start_time handling with full probe metadata
persistence. Apply start-time fallback at live-transition callers."
```

---

### Task 5: Verify complete system

**Files:** None (verification only)

- [ ] **Step 1: Run full test suite**

Run: `go test ./...`
Expected: All tests pass.

- [ ] **Step 2: Run static analysis**

Run: `go vet ./...`
Expected: No issues.

- [ ] **Step 3: Verify no references to old function remain**

Search for any remaining `updateJobMetadataOnLive` references:

Run: `grep -r "updateJobMetadataOnLive" internal/`
Expected: No results.

- [ ] **Step 4: Build binary**

Run: `go build -o moombox.exe ./cmd/moombox`
Expected: Clean build.
