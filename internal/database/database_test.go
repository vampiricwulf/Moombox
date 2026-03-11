package database

import (
	"path/filepath"
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

func TestLastVideos(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// No last video
	vid, _ := db.GetLastVideo("UC123")
	if vid != "" {
		t.Error("expected empty for unknown channel")
	}

	// Set and get
	db.SetLastVideo("UC123", "vid1")
	vid, _ = db.GetLastVideo("UC123")
	if vid != "vid1" {
		t.Errorf("expected vid1, got %s", vid)
	}

	// Update
	db.SetLastVideo("UC123", "vid2")
	vid, _ = db.GetLastVideo("UC123")
	if vid != "vid2" {
		t.Errorf("expected vid2, got %s", vid)
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

	// Invalid field name should be silently ignored
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

	// Adding a job should trigger OnJobsChange
	job := &Job{
		ID:      "yt_change1",
		VideoID: "change1",
		URL:     "https://youtube.com/watch?v=change1",
		Status:  StatusUpcoming,
	}
	db.AddJob(job)

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

	// Register multiple subscribers and unsubscribe them to verify trailing nil trimming
	unsubs := make([]func(), 5)
	for i := range unsubs {
		unsubs[i] = db.OnJobUpdate(func(j *Job) {})
	}

	// Unsubscribe all in reverse order
	for i := len(unsubs) - 1; i >= 0; i-- {
		unsubs[i]()
	}

	// After unsubscribing all, the internal slice should be trimmed
	db.subMu.RLock()
	sliceLen := len(db.onJobUpdate)
	db.subMu.RUnlock()
	if sliceLen != 0 {
		t.Errorf("expected subscriber slice trimmed to 0, got %d", sliceLen)
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
