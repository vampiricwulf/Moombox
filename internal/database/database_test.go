package database

import (
	"path/filepath"
	"testing"
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
