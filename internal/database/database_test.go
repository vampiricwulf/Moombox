package database

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpenAndClose(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
}

func TestAddAndGetJob(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &Job{
		ID:          "yt_dQw4w9WgXcQ",
		VideoID:     "dQw4w9WgXcQ",
		URL:         "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		Title:       "Test Video",
		ChannelName: "Test Channel",
		Platform:    "youtube",
		Status:      StatusUpcoming,
	}

	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetJob("yt_dQw4w9WgXcQ")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected job, got nil")
	}
	if got.Title != "Test Video" {
		t.Errorf("expected title 'Test Video', got %q", got.Title)
	}
	if got.Status != StatusUpcoming {
		t.Errorf("expected status Upcoming, got %s", got.Status)
	}
}

func TestDeleteJob(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &Job{
		ID:      "yt_test123",
		VideoID: "test123",
		URL:     "https://youtube.com/watch?v=test123",
		Status:  StatusUpcoming,
	}
	db.AddJob(job)

	if err := db.DeleteJob("yt_test123"); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetJob("yt_test123")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Error("expected nil after delete")
	}
}

func TestHistory(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Not processed yet
	has, _ := db.HasProcessed("video1")
	if has {
		t.Error("expected false for unprocessed video")
	}

	// Add to history
	db.AddToHistory("video1")

	has, _ = db.HasProcessed("video1")
	if !has {
		t.Error("expected true after adding to history")
	}
}

func TestGaps(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &Job{
		ID:      "yt_gaptest",
		VideoID: "gaptest",
		URL:     "https://youtube.com/watch?v=gaptest",
		Status:  StatusDownloading,
	}
	db.AddJob(job)

	db.AddGap("yt_gaptest", 10, 15, "video")
	db.AddGap("yt_gaptest", 20, 25, "audio")

	got, _ := db.GetJob("yt_gaptest")
	if len(got.Gaps) != 2 {
		t.Errorf("expected 2 gaps, got %d", len(got.Gaps))
	}
}

// TestClearJobSegmentsAndGaps pins the fresh-start reset: after clearing, a
// re-download must NOT see stale part rows (which muxAndFinalize would finalize
// from, discarding the new media). Also confirms it's job-scoped.
func TestClearJobSegmentsAndGaps(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	db.AddJob(&Job{ID: "yt_keep", VideoID: "keep", URL: "u", Status: StatusDownloading})
	db.AddJob(&Job{ID: "yt_reset", VideoID: "reset", URL: "u", Status: StatusError})
	db.AddSegment(&Segment{JobID: "yt_reset", SegmentIndex: 0, Quality: "1080p60", Filename: "a.mp4"})
	db.AddSegment(&Segment{JobID: "yt_reset", SegmentIndex: 1, Quality: "720p60", Filename: "b.mp4"})
	db.AddGap("yt_reset", 5, 9, "video")
	db.AddSegment(&Segment{JobID: "yt_keep", SegmentIndex: 0, Quality: "1080p60", Filename: "k.mp4"})
	db.AddGap("yt_keep", 1, 2, "audio")

	if err := db.ClearJobSegmentsAndGaps("yt_reset"); err != nil {
		t.Fatalf("ClearJobSegmentsAndGaps: %v", err)
	}

	segs, _ := db.GetSegments("yt_reset")
	if len(segs) != 0 {
		t.Errorf("reset job segments = %d, want 0", len(segs))
	}
	if got, _ := db.GetJob("yt_reset"); got != nil && len(got.Gaps) != 0 {
		t.Errorf("reset job gaps = %d, want 0", len(got.Gaps))
	}
	// Other jobs untouched.
	keepSegs, _ := db.GetSegments("yt_keep")
	if len(keepSegs) != 1 {
		t.Errorf("other job segments = %d, want 1 (must not be cleared)", len(keepSegs))
	}
	if got, _ := db.GetJob("yt_keep"); got == nil || len(got.Gaps) != 1 {
		t.Errorf("other job gaps cleared unexpectedly")
	}
}

func TestTrims(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &Job{
		ID:      "yt_trimtest",
		VideoID: "trimtest",
		URL:     "https://youtube.com/watch?v=trimtest",
		Status:  StatusFinished,
	}
	db.AddJob(job)

	trim := &TrimRecord{
		ID:        "trim_1",
		JobID:     "yt_trimtest",
		StartTime: 10.0,
		EndTime:   30.0,
		Filename:  "trim.mp4",
		CreatedAt: "2024-01-01T00:00:00Z",
		Duration:  20.0,
	}
	db.AddTrim(trim)

	got, _ := db.GetJob("yt_trimtest")
	if len(got.Trims) != 1 {
		t.Errorf("expected 1 trim, got %d", len(got.Trims))
	}

	db.DeleteTrim("trim_1")
	got, _ = db.GetJob("yt_trimtest")
	if len(got.Trims) != 0 {
		t.Errorf("expected 0 trims after delete, got %d", len(got.Trims))
	}
}

func TestGetAllJobs(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Empty database should return empty slice
	jobs, err := db.GetAllJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Errorf("expected 0 jobs in empty db, got %d", len(jobs))
	}

	// Add multiple jobs
	for i, id := range []string{"yt_aaa", "yt_bbb", "yt_ccc"} {
		job := &Job{
			ID:      id,
			VideoID: id[3:],
			URL:     "https://youtube.com/watch?v=" + id[3:],
			Status:  StatusUpcoming,
			Title:   "Video " + id,
		}
		db.AddJob(job)
		// Stagger updated_at so ordering is deterministic
		time.Sleep(time.Duration(i) * 10 * time.Millisecond)
		db.UpdateJobFields(id, map[string]any{"title": "Video " + id})
	}

	jobs, err = db.GetAllJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(jobs))
	}
	// Should be ordered by updated_at DESC — most recently updated first
	if jobs[0].ID != "yt_ccc" {
		t.Errorf("expected first job to be yt_ccc (most recent), got %s", jobs[0].ID)
	}
}

func TestUpdateJobFields(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &Job{
		ID:      "yt_update1",
		VideoID: "update1",
		URL:     "https://youtube.com/watch?v=update1",
		Status:  StatusUpcoming,
		Title:   "Original Title",
	}
	db.AddJob(job)

	// Update multiple fields at once
	db.UpdateJobFields("yt_update1", map[string]any{
		"status":   StatusDownloading,
		"progress": "V:100 A:100",
		"title":    "Updated Title",
	})

	got, err := db.GetJob("yt_update1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDownloading {
		t.Errorf("expected status %s, got %s", StatusDownloading, got.Status)
	}
	if got.Progress != "V:100 A:100" {
		t.Errorf("expected progress 'V:100 A:100', got %q", got.Progress)
	}
	if got.Title != "Updated Title" {
		t.Errorf("expected title 'Updated Title', got %q", got.Title)
	}

	// updated_at should be set
	if got.UpdatedAt == "" {
		t.Error("expected updated_at to be set")
	}
}

func TestUpdateJobFieldsInvalidKey(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &Job{
		ID:      "yt_invalid",
		VideoID: "invalid",
		URL:     "https://youtube.com/watch?v=invalid",
		Status:  StatusUpcoming,
		Title:   "Original",
	}
	db.AddJob(job)

	// Invalid field name should be ignored (with a Debug-level log) without affecting other updates.
	db.UpdateJobFields("yt_invalid", map[string]any{
		"nonexistent_field": "value",
	})

	got, _ := db.GetJob("yt_invalid")
	if got.Title != "Original" {
		t.Errorf("expected title unchanged, got %q", got.Title)
	}
}

func TestUpdateJobFieldsEmpty(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &Job{
		ID:      "yt_empty",
		VideoID: "empty",
		URL:     "https://youtube.com/watch?v=empty",
		Status:  StatusUpcoming,
	}
	db.AddJob(job)

	initialJob, _ := db.GetJob("yt_empty")
	initialUpdatedAt := initialJob.UpdatedAt

	// Empty fields map should be a no-op (no updated_at change)
	db.UpdateJobFields("yt_empty", map[string]any{})

	got, _ := db.GetJob("yt_empty")
	if got.UpdatedAt != initialUpdatedAt {
		t.Error("expected updated_at unchanged for empty fields update")
	}
}

// TestUpdateJobFieldsAutoRetryCount verifies the new auto_retry_count column
// can be read and written via UpdateJobFields. Locks down the column-plumbing
// done in Phase 1 of the Twitch flap auto-recovery feature.
func TestUpdateJobFieldsAutoRetryCount(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &Job{
		ID:      "tw_retry1",
		VideoID: "retry1",
		URL:     "https://twitch.tv/somestreamer",
		Status:  StatusError,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	// Default value on insert
	got, err := db.GetJob("tw_retry1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoRetryCount != 0 {
		t.Errorf("default AutoRetryCount: want 0, got %d", got.AutoRetryCount)
	}

	// Update via UpdateJobFields
	db.UpdateJobFields("tw_retry1", map[string]any{
		"auto_retry_count": 1,
	})
	got, err = db.GetJob("tw_retry1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoRetryCount != 1 {
		t.Errorf("after update: want 1, got %d", got.AutoRetryCount)
	}

	// Update again
	db.UpdateJobFields("tw_retry1", map[string]any{
		"auto_retry_count": 2,
	})
	got, err = db.GetJob("tw_retry1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoRetryCount != 2 {
		t.Errorf("after second update: want 2, got %d", got.AutoRetryCount)
	}
}

func TestOnJobUpdateSubscriber(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &Job{
		ID:      "yt_sub1",
		VideoID: "sub1",
		URL:     "https://youtube.com/watch?v=sub1",
		Status:  StatusUpcoming,
		Title:   "Sub Test",
	}
	db.AddJob(job)

	// Subscribe to job updates
	var receivedJob *Job
	var mu sync.Mutex
	done := make(chan struct{}, 1)
	unsub := db.OnJobUpdate(func(j *Job) {
		mu.Lock()
		receivedJob = j
		mu.Unlock()
		select {
		case done <- struct{}{}:
		default:
		}
	})
	defer unsub()

	// Trigger an update
	db.UpdateJobFields("yt_sub1", map[string]any{
		"status": StatusDownloading,
	})

	// Wait for the async subscriber notification
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OnJobUpdate callback")
	}

	mu.Lock()
	if receivedJob == nil {
		t.Fatal("expected subscriber to receive job update")
	}
	if receivedJob.ID != "yt_sub1" {
		t.Errorf("expected job ID 'yt_sub1', got %q", receivedJob.ID)
	}
	if receivedJob.Status != StatusDownloading {
		t.Errorf("expected status %s, got %s", StatusDownloading, receivedJob.Status)
	}
	mu.Unlock()
}

func TestOnJobUpdateUnsubscribe(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &Job{
		ID:      "yt_unsub1",
		VideoID: "unsub1",
		URL:     "https://youtube.com/watch?v=unsub1",
		Status:  StatusUpcoming,
	}
	db.AddJob(job)

	callCount := 0
	var mu sync.Mutex
	unsub := db.OnJobUpdate(func(j *Job) {
		mu.Lock()
		callCount++
		mu.Unlock()
	})

	// Trigger update, should fire callback
	db.UpdateJobFields("yt_unsub1", map[string]any{"status": StatusDownloading})
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	firstCount := callCount
	mu.Unlock()

	// Unsubscribe
	unsub()

	// Trigger another update — callback should NOT fire
	db.UpdateJobFields("yt_unsub1", map[string]any{"status": StatusMuxing})
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	if callCount != firstCount {
		t.Errorf("expected callback count unchanged after unsubscribe, got %d -> %d",
			firstCount, callCount)
	}
	mu.Unlock()
}

func TestOnJobsChangeSubscriber(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Seed a Finished job — BatchSetWatched only modifies Finished
	// rows, and post-DECISIONS-#21 it's the only OnJobsChange writer
	// left (AddJob/DeleteJob/AddTrim/DeleteTrim all migrated to
	// targeted lifecycle events).
	db.AddJob(&Job{ID: "watched_target", VideoID: "v", URL: "u", Status: StatusFinished})

	var receivedJobs []*Job
	var mu sync.Mutex
	done := make(chan struct{}, 1)
	unsub := db.OnJobsChange(func(jobs []*Job) {
		mu.Lock()
		receivedJobs = jobs
		mu.Unlock()
		select {
		case done <- struct{}{}:
		default:
		}
	})
	defer unsub()

	// BatchSetWatched is the only writer that still fires OnJobsChange
	// post-DECISIONS-#21 consumer migration.
	if err := db.BatchSetWatched([]string{"watched_target"}, true); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OnJobsChange callback")
	}

	mu.Lock()
	if len(receivedJobs) != 1 {
		t.Errorf("expected 1 job in change notification, got %d", len(receivedJobs))
	}
	mu.Unlock()
}

func TestSubscriberSliceTrim(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Register multiple subscribers, then unsubscribe them to verify that
	// the slice length is reduced correctly. Unsubscribe is implemented via
	// slice-out (append(s[:i], s[i+1:]...)) — no trailing-nil trimming.
	unsubs := make([]func(), 5)
	for i := range unsubs {
		unsubs[i] = db.OnJobUpdate(func(j *Job) {})
	}

	// Unsubscribe all in reverse order
	for i := len(unsubs) - 1; i >= 0; i-- {
		unsubs[i]()
	}

	// After unsubscribing all, the internal slice length should be 0.
	db.subMu.RLock()
	sliceLen := len(db.onJobUpdate)
	db.subMu.RUnlock()
	if sliceLen != 0 {
		t.Errorf("expected subscriber slice length 0, got %d", sliceLen)
	}
}

func TestUpdateJobSync(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &Job{
		ID:      "yt_sync1",
		VideoID: "sync1",
		URL:     "https://youtube.com/watch?v=sync1",
		Status:  StatusUpcoming,
		Title:   "Sync Test",
	}
	db.AddJob(job)

	// Update synchronously
	job.Status = StatusDownloading
	job.Title = "Updated Sync"
	err = db.UpdateJobSync(job)
	if err != nil {
		t.Fatal(err)
	}

	got, err := db.GetJob("yt_sync1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDownloading {
		t.Errorf("expected Downloading, got %s", got.Status)
	}
	if got.Title != "Updated Sync" {
		t.Errorf("expected 'Updated Sync', got %q", got.Title)
	}
	if got.UpdatedAt == "" {
		t.Error("expected updated_at to be set after UpdateJobSync")
	}
}

func TestSegments(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &Job{
		ID:      "yt_segtest",
		VideoID: "segtest",
		URL:     "https://youtube.com/watch?v=segtest",
		Status:  StatusDownloading,
	}
	db.AddJob(job)

	seg := &Segment{
		JobID:        "yt_segtest",
		SegmentIndex: 0,
		UnixStart:    1000,
		UnixEnd:      2000,
		Quality:      "1080p60",
		Filename:     "seg0.mp4",
	}
	if err := db.AddSegment(seg); err != nil {
		t.Fatal(err)
	}
	if seg.ID == 0 {
		t.Error("expected segment ID to be set after insert")
	}

	segments, err := db.GetSegments("yt_segtest")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segments))
	}
	if segments[0].Quality != "1080p60" {
		t.Errorf("expected 1080p60, got %s", segments[0].Quality)
	}

	// Segments should also be loaded via GetJob
	got, err := db.GetJob("yt_segtest")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Segments) != 1 {
		t.Errorf("expected 1 segment via GetJob, got %d", len(got.Segments))
	}
}

func TestClientTokenCRUD(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ct := &ClientToken{
		ID:          "tok_1",
		TokenPrefix: "mbox_abc",
		TokenHash:   "hash123",
		Label:       "Test Token",
		CreatedAt:   "2024-01-01T00:00:00Z",
		LastUsedAt:  "",
		LastIP:      "",
	}
	if err := db.AddClientToken(ct); err != nil {
		t.Fatal(err)
	}

	// Get by prefix
	got, err := db.GetClientTokenByPrefix("mbox_abc")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected token, got nil")
	}
	if got.Label != "Test Token" {
		t.Errorf("expected 'Test Token', got %q", got.Label)
	}

	// Update usage
	if err := db.UpdateClientTokenUsage("tok_1", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	got, _ = db.GetClientTokenByPrefix("mbox_abc")
	if got.LastIP != "127.0.0.1" {
		t.Errorf("expected 127.0.0.1, got %q", got.LastIP)
	}

	// List
	tokens, err := db.ListClientTokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 {
		t.Errorf("expected 1 token, got %d", len(tokens))
	}

	// Delete
	if err := db.DeleteClientToken("tok_1"); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetClientTokenByPrefix("mbox_abc")
	if got != nil {
		t.Error("expected nil after delete")
	}

	// Non-existent prefix
	got, err = db.GetClientTokenByPrefix("nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Error("expected nil for non-existent prefix")
	}
}

func TestJobExists(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if db.JobExists("nonexistent") {
		t.Error("expected false for nonexistent job")
	}

	job := &Job{
		ID:      "yt_exists1",
		VideoID: "exists1",
		URL:     "https://youtube.com/watch?v=exists1",
		Status:  StatusUpcoming,
	}
	db.AddJob(job)

	if !db.JobExists("yt_exists1") {
		t.Error("expected true for existing job")
	}
}

func TestHasActiveJob(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// No active job initially
	has, err := db.HasActiveJob("video123")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("expected no active job")
	}

	// Add an active job
	job := &Job{
		ID:      "yt_active1",
		VideoID: "video123",
		URL:     "https://youtube.com/watch?v=video123",
		Status:  StatusDownloading,
	}
	db.AddJob(job)

	has, err = db.HasActiveJob("video123")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("expected active job")
	}

	// Mark as finished — should no longer be active
	db.UpdateJobFields("yt_active1", map[string]any{"status": StatusFinished})

	has, err = db.HasActiveJob("video123")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("expected no active job after finishing")
	}
}

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

	// Create 3 finished jobs
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
	if err := db.BatchSetWatched([]string{"yt_bw1", "yt_bw2", "yt_bw3"}, true); err != nil {
		t.Fatal(err)
	}

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
	if err := db.BatchSetWatched([]string{"yt_bw1", "yt_bw2"}, false); err != nil {
		t.Fatal(err)
	}

	got1, _ := db.GetJob("yt_bw1")
	got3, _ := db.GetJob("yt_bw3")
	if got1.Watched {
		t.Error("yt_bw1: expected watched=false after batch unwatched")
	}
	if !got3.Watched {
		t.Error("yt_bw3: should still be watched (not in batch unwatched)")
	}
}

// TestMigrateFromLegacyVersionTable verifies the v11 cutover: an existing DB
// using the pre-v11 schema_version table is detected, its version carried
// forward to PRAGMA user_version, and the legacy table is dropped.
func TestMigrateFromLegacyVersionTable(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "legacy.db")

	// Bootstrap a full current-schema DB, then forcibly reset it to look like a
	// pre-v11 install (schema_version table present, PRAGMA user_version=0).
	db1, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db1.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL UNIQUE)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db1.db.Exec(`DELETE FROM schema_version`); err != nil {
		t.Fatal(err)
	}
	if _, err := db1.db.Exec(`INSERT INTO schema_version (version) VALUES (6)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db1.db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	db1.Close()

	// Re-open. migrate() should read legacy=6, carry it to PRAGMA, run the
	// remaining migrations (v7..v11), and drop the schema_version table.
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("re-open after simulated-legacy reset: %v", err)
	}
	defer db2.Close()

	got, err := db2.readUserVersion()
	if err != nil {
		t.Fatalf("readUserVersion: %v", err)
	}
	if got != schemaVersion {
		t.Errorf("PRAGMA user_version after migration = %d, want %d", got, schemaVersion)
	}

	// Legacy table must be gone.
	var count int
	err = db2.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_version'`).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("legacy schema_version table still present after migration (found %d entries)", count)
	}
}

// TestFreshInstallUsesPragma verifies that a brand-new DB sets PRAGMA
// user_version without ever creating the legacy schema_version table.
func TestFreshInstallUsesPragma(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fresh.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	got, err := db.readUserVersion()
	if err != nil {
		t.Fatalf("readUserVersion: %v", err)
	}
	if got != schemaVersion {
		t.Errorf("fresh install PRAGMA user_version = %d, want %d", got, schemaVersion)
	}

	var count int
	err = db.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_version'`).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("legacy schema_version table present on fresh install (found %d entries)", count)
	}
}

// schemaInventory returns the set of schema objects (tables/indexes by
// "type:name") plus per-table column lists, for drift comparison.
func schemaInventory(t *testing.T, db *Database) map[string][]string {
	t.Helper()
	inv := make(map[string][]string)

	rows, err := db.db.Query(`SELECT type, name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	var tables []string
	for rows.Next() {
		var typ, name string
		if err := rows.Scan(&typ, &name); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		inv["objects"] = append(inv["objects"], typ+":"+name)
		if typ == "table" {
			tables = append(tables, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("sqlite_master rows: %v", err)
	}

	for _, table := range tables {
		cols, err := db.db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY name`, table)
		if err != nil {
			t.Fatalf("table_info(%s): %v", table, err)
		}
		for cols.Next() {
			var name string
			if err := cols.Scan(&name); err != nil {
				t.Fatalf("scan table_info(%s): %v", table, err)
			}
			inv["columns:"+table] = append(inv["columns:"+table], name)
		}
		cols.Close()
		if err := cols.Err(); err != nil {
			t.Fatalf("table_info(%s) rows: %v", table, err)
		}
	}
	return inv
}

// TestFreshSchemaMatchesMigratedSchema guards against drift between
// createSchema (fresh installs) and the incremental migrations (upgrades):
// a fresh DB and a DB forced back to version 1 and re-migrated must end up
// with identical tables, indexes, and columns. Caught the missing
// idx_jobs_video_id on fresh installs (v4 created it only for upgrades).
func TestFreshSchemaMatchesMigratedSchema(t *testing.T) {
	freshPath := filepath.Join(t.TempDir(), "fresh.db")
	fresh, err := Open(freshPath)
	if err != nil {
		t.Fatal(err)
	}
	freshInv := schemaInventory(t, fresh)
	fresh.Close()

	migPath := filepath.Join(t.TempDir(), "mig.db")
	mig, err := Open(migPath)
	if err != nil {
		t.Fatal(err)
	}
	// Rewind to v1 so the next Open replays migrations 2..current over the
	// full schema. Migrations are idempotent (IF NOT EXISTS / duplicate-
	// column tolerant), so anything they ADD that createSchema lacks shows
	// up as a diff.
	if _, err := mig.db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	mig.Close()

	mig2, err := Open(migPath)
	if err != nil {
		t.Fatalf("re-open with migrations: %v", err)
	}
	migInv := schemaInventory(t, mig2)
	mig2.Close()

	for key, migList := range migInv {
		freshList := freshInv[key]
		if strings.Join(freshList, ",") != strings.Join(migList, ",") {
			t.Errorf("schema drift at %s:\n  fresh:    %v\n  migrated: %v", key, freshList, migList)
		}
	}
	for key := range freshInv {
		if _, ok := migInv[key]; !ok {
			t.Errorf("schema drift: fresh has %s but migrated path lacks it entirely", key)
		}
	}
}

// TestFieldToColumnCoverage asserts that every writable column on Job has an
// entry in fieldToColumn. Drift here caused audit finding database.md C3 where
// six fields (manually_added, allow_non_stream, selected_video_itag,
// selected_audio_itag, start_time, end_time) were silently dropped by
// UpdateJobFields.
//
// The excluded set covers fields that are either auto-managed (id, createdAt,
// updatedAt), loaded via join (trims, gaps, segments), or intentionally
// not-writable partially. Add here when a new column legitimately shouldn't go
// through UpdateJobFields — don't just delete the map entry.
func TestFieldToColumnCoverage(t *testing.T) {
	excluded := map[string]bool{
		"id":        true, // primary key, set at insert
		"videoId":   true, // set at insert
		"url":       true, // set at insert
		"platform":  true, // set at insert
		"createdAt": true, // set at insert
		"updatedAt": true, // auto-managed by UpdateJobFields itself
		"gaps":      true, // loaded via join
		"trims":     true, // loaded via join
		"segments":  true, // loaded via join
		"channelId": true, // set at insert — feed affiliation never changes, and a partial write of "" would fake-empty a NULL
	}

	for field := range reflect.TypeFor[Job]().Fields() {
		tag := field.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		jsonName := strings.SplitN(tag, ",", 2)[0]
		if jsonName == "" {
			continue
		}
		if excluded[jsonName] {
			continue
		}
		column := camelToSnake(jsonName)
		if _, ok := fieldToColumn[column]; !ok {
			t.Errorf("Job field %q (column %q) has no fieldToColumn entry — either add it to the map or to the excluded set in this test", jsonName, column)
		}
	}
}

// camelToSnake converts a camelCase identifier to snake_case. Used only by
// TestFieldToColumnCoverage to bridge Job's JSON tags and the DB schema.
func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestSubscriberCanCallDatabaseFromCallback locks down the contract that
// OnJobUpdate / OnJobsChange callbacks may call back into Database APIs
// without deadlocking. Pre-refactor (audit reports/database.md C1+C2),
// notify fired while db.mu was held, so a subscriber calling db.GetJob
// or any other locking method would hang on the same mutex. The fix
// moves the dispatch to AFTER db.mu.Unlock; this test trips the
// deadlock by forcing the path and waits with a timeout.
func TestSubscriberCanCallDatabaseFromCallback(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &Job{
		ID:      "yt_deadlock",
		VideoID: "deadlock",
		URL:     "https://www.youtube.com/watch?v=deadlock",
		Status:  StatusUpcoming,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	// Subscriber callback that hits db.GetJob — under the old
	// "notify-while-holding-mu" code path, this RLock acquisition would
	// block on the writer's Lock and the test would time out.
	var (
		callbackDone = make(chan struct{}, 1)
		readBackOK   = make(chan bool, 1)
	)
	unsub := db.OnJobUpdate(func(j *Job) {
		got, gerr := db.GetJob(j.ID)
		readBackOK <- gerr == nil && got != nil
		callbackDone <- struct{}{}
	})
	defer unsub()

	// Trigger a job update → fires the subscriber → subscriber calls
	// db.GetJob → must not deadlock.
	go func() {
		db.UpdateJobFields("yt_deadlock", map[string]any{"status": StatusDownloading})
	}()

	select {
	case <-callbackDone:
		// Drain the success flag as well so the test fails clearly if the
		// subscriber's GetJob actually errored.
		select {
		case ok := <-readBackOK:
			if !ok {
				t.Errorf("subscriber's db.GetJob failed inside callback")
			}
		default:
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnJobUpdate callback did not complete within 2s — likely deadlock on db.mu")
	}
}

// TestJobsChangeSubscriberCanCallDatabaseFromCallback mirrors the
// OnJobUpdate sibling for the OnJobsChange path. Post-DECISIONS-#21
// consumer migration, BatchSetWatched is the only OnJobsChange
// writer; the test forces the dispatch path through it. Same
// deadlock surface (snapshotJobsChange runs under db.mu,
// dispatchJobsChange runs after Unlock); the assertion is that a
// callback that calls back into Database doesn't hang.
func TestJobsChangeSubscriberCanCallDatabaseFromCallback(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	db.AddJob(&Job{ID: "cb_target", VideoID: "v", URL: "u", Status: StatusFinished})

	callbackDone := make(chan struct{}, 1)
	readBackOK := make(chan bool, 1)
	unsub := db.OnJobsChange(func(jobs []*Job) {
		// Hit a locking method — RLock would block on writer's Lock under
		// the old code.
		_, gerr := db.GetAllJobs()
		readBackOK <- gerr == nil
		callbackDone <- struct{}{}
	})
	defer unsub()

	go func() {
		db.BatchSetWatched([]string{"cb_target"}, true)
	}()

	select {
	case <-callbackDone:
		select {
		case ok := <-readBackOK:
			if !ok {
				t.Errorf("subscriber's db.GetAllJobs failed inside callback")
			}
		default:
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnJobsChange callback did not complete within 2s — likely deadlock on db.mu")
	}
}

// TestAddJobWritesChannelIDAndQueuePriority pins Plan 4's creator-table
// persistence (spec §10): AddJob writes channel_id and queue_priority
// EXPLICITLY for every creator — the schema DEFAULT 1 exists only for
// pre-v16 legacy rows and must never leak into new inserts. Three creator
// shapes:
//
//   - backlog VOD (feed):        Queued,   priority 1, channel affiliation set
//   - broadcast/new VOD (feed):  Upcoming, priority 0, channel affiliation set
//   - Twitch/manual (no fields): channel_id stays NULL — never "" — and
//     queue_priority is the explicit 0, NOT the column DEFAULT 1
func TestAddJobWritesChannelIDAndQueuePriority(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	chID := "UC_feed_channel"
	for _, j := range []*Job{
		{ID: "backlog1", VideoID: "backlog1", URL: "u", Status: StatusQueued, ChannelID: &chID, QueuePriority: 1},
		{ID: "broadcast1", VideoID: "broadcast1", URL: "u", Status: StatusUpcoming, ChannelID: &chID, QueuePriority: 0},
		{ID: "manual1", VideoID: "manual1", URL: "u", Status: StatusUpcoming},
	} {
		added, err := db.AddJob(j)
		if err != nil || !added {
			t.Fatalf("AddJob(%s): added=%v err=%v", j.ID, added, err)
		}
	}

	assertRow := func(id string, wantStatus JobStatus, wantPriority int, wantChannel *string) {
		t.Helper()
		var status string
		var qp int
		var ch sql.NullString
		if err := db.db.QueryRow(`SELECT status, queue_priority, channel_id FROM jobs WHERE id=?`, id).Scan(&status, &qp, &ch); err != nil {
			t.Fatalf("row %s: %v", id, err)
		}
		if status != string(wantStatus) {
			t.Errorf("%s: status = %q, want %q", id, status, wantStatus)
		}
		if qp != wantPriority {
			t.Errorf("%s: queue_priority = %d, want %d — every creator writes it explicitly, never the column DEFAULT", id, qp, wantPriority)
		}
		if wantChannel == nil {
			if ch.Valid {
				t.Errorf("%s: channel_id = %q, want NULL — a fake-empty affiliation must not appear on Twitch/manual jobs", id, ch.String)
			}
		} else if !ch.Valid || ch.String != *wantChannel {
			t.Errorf("%s: channel_id = (%q, valid=%v), want %q", id, ch.String, ch.Valid, *wantChannel)
		}
	}

	assertRow("backlog1", StatusQueued, 1, &chID)
	assertRow("broadcast1", StatusUpcoming, 0, &chID)
	assertRow("manual1", StatusUpcoming, 0, nil)
}

// TestDeleteJobsAndHistoryForChannel pins the Plan-5 channel prune: the
// channel's pre-download jobs ({Queued, Upcoming, COOKIES?}) AND their history
// rows are deleted together. A deleted job whose history row survives makes
// HasProcessed lie ("a job was created") and blocks re-add forever — the exact
// orphan class that re-armed gr-ZTohjwnQ. Started/terminal jobs keep their
// history, other channels are untouched, and NULL-channel jobs (Twitch/manual,
// no affiliation) must never match.
func TestDeleteJobsAndHistoryForChannel(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	chID := "UC_prune_me"
	otherCh := "UC_other"

	// ID == VideoID mirrors how monitor/backfill-created YouTube jobs are
	// keyed — history rows are keyed by job ID, which for YouTube IS the
	// video ID (see ListOrphanedHistory).
	seed := func(id string, status JobStatus, ch *string) {
		t.Helper()
		added, err := db.AddJob(&Job{ID: id, VideoID: id, URL: "u", Status: status, ChannelID: ch})
		if err != nil || !added {
			t.Fatalf("AddJob(%s): added=%v err=%v", id, added, err)
		}
		// AddToHistory fires at job creation for every disposition,
		// exactly as the host does (monitor_callbacks).
		if err := db.AddToHistory(id); err != nil {
			t.Fatalf("AddToHistory(%s): %v", id, err)
		}
	}

	pruneStatuses := []JobStatus{StatusQueued, StatusUpcoming, StatusCookies}
	doomed := []string{"doomed_queued", "doomed_upcoming", "doomed_cookies"}
	for i, st := range pruneStatuses {
		seed(doomed[i], st, &chID)
	}
	kept := []string{"kept_live", "kept_downloading", "kept_finished"}
	for i, st := range []JobStatus{StatusLive, StatusDownloading, StatusFinished} {
		seed(kept[i], st, &chID)
	}
	seed("other_channel", StatusQueued, &otherCh) // pruned status, different channel
	seed("null_channel", StatusQueued, nil)       // pruned status, NULL channel_id

	deleted, err := db.DeleteJobsAndHistoryForChannel(chID, pruneStatuses)
	if err != nil {
		t.Fatalf("DeleteJobsAndHistoryForChannel: %v", err)
	}
	if deleted != len(doomed) {
		t.Errorf("deleted = %d, want %d (the jobs statement's RowsAffected)", deleted, len(doomed))
	}

	assertState := func(id string, wantJob, wantHistory bool) {
		t.Helper()
		job, err := db.GetJob(id)
		if err != nil {
			t.Fatalf("GetJob(%s): %v", id, err)
		}
		if got := job != nil; got != wantJob {
			t.Errorf("%s: job exists = %v, want %v", id, got, wantJob)
		}
		has, err := db.HasProcessed(id)
		if err != nil {
			t.Fatalf("HasProcessed(%s): %v", id, err)
		}
		if has != wantHistory {
			t.Errorf("%s: history exists = %v, want %v", id, has, wantHistory)
		}
	}

	for _, id := range doomed {
		assertState(id, false, false) // job AND history gone — no orphan
	}
	for _, id := range kept {
		assertState(id, true, true) // started/terminal: job AND history intact
	}
	assertState("other_channel", true, true) // channel-scoped
	assertState("null_channel", true, true)  // NULL channel_id never matches
}
