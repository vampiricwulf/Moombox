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
		EndTimestamp:       "2026-04-10T14:00:00Z",
		LengthSeconds:      new(7200),
		IsUpcoming:         true,
	}

	sp.updateJobMetadata(job, info, false)

	got, _ := db.GetJob("yt_test1")
	// Both the row and the caller's local job object must carry the fill
	// (not just the DB).
	for _, c := range []struct {
		src string
		j   *database.Job
	}{{"db", got}, {"local job", job}} {
		if c.j.Title != "My Stream" {
			t.Errorf("%s: title = %q, want %q", c.src, c.j.Title, "My Stream")
		}
		if c.j.ChannelName != "My Channel" {
			t.Errorf("%s: channel_name = %q, want %q", c.src, c.j.ChannelName, "My Channel")
		}
		if c.j.ThumbnailURL != "https://example.com/thumb.jpg" {
			t.Errorf("%s: thumbnail_url = %q, want %q", c.src, c.j.ThumbnailURL, "https://example.com/thumb.jpg")
		}
		if c.j.Description != "A description" {
			t.Errorf("%s: description = %q, want %q", c.src, c.j.Description, "A description")
		}
		if c.j.StreamStartTime != "2026-04-10T12:00:00Z" {
			t.Errorf("%s: stream_start_time = %q, want %q", c.src, c.j.StreamStartTime, "2026-04-10T12:00:00Z")
		}
		if c.j.StreamEndTime != "2026-04-10T14:00:00Z" {
			t.Errorf("%s: stream_end_time = %q, want %q", c.src, c.j.StreamEndTime, "2026-04-10T14:00:00Z")
		}
		if c.j.LengthSeconds == nil || *c.j.LengthSeconds != 7200 {
			t.Errorf("%s: length_seconds = %v, want 7200", c.src, c.j.LengthSeconds)
		}
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
		EndTimestamp:       "2026-04-10T17:00:00Z",
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
		LengthSeconds:   new(3600),
	})

	info := &youtube.VideoInfo{
		Title:              "New Title",
		ChannelName:        "New Channel",
		ThumbnailURL:       "https://example.com/new.jpg",
		Description:        "New description",
		ScheduledStartTime: "2026-04-10T15:00:00Z",
		EndTimestamp:       "2026-04-10T17:00:00Z",
		LengthSeconds:      new(7200),
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
		LengthSeconds:   new(3600),
	})

	// Record the updatedAt before calling update
	beforeUpdate := job.UpdatedAt

	info := &youtube.VideoInfo{
		Title:              "Same Title",
		ChannelName:        "Same Channel",
		ThumbnailURL:       "https://example.com/same.jpg",
		Description:        "Same description",
		ScheduledStartTime: "2026-04-10T10:00:00Z",
		EndTimestamp:       "2026-04-10T12:00:00Z",
		LengthSeconds:      new(3600),
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
