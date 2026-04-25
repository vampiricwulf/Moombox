package database

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// --- GetJobStats ---

func TestGetJobStatsEmptyDatabase(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	stats, err := db.GetJobStats()
	if err != nil {
		t.Fatalf("GetJobStats empty: %v", err)
	}
	if stats.FinishedCount != 0 || stats.ErrorCount != 0 || stats.ActiveCount != 0 {
		t.Errorf("empty stats counts: want all 0, got finished=%d error=%d active=%d",
			stats.FinishedCount, stats.ErrorCount, stats.ActiveCount)
	}
	if stats.FinishedSize != 0 || stats.ErrorSize != 0 {
		t.Errorf("empty stats sizes: want all 0, got finished=%d error=%d",
			stats.FinishedSize, stats.ErrorSize)
	}
}

func TestGetJobStatsAggregatesByStatusAndPlatform(t *testing.T) {
	// GetJobStats query computes COALESCE(SUM(CASE WHEN status=? ...))
	// per category. Seed jobs covering every documented bucket and
	// verify the row breakdown matches expectations.
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	type seed struct {
		id     string
		plat   string
		status JobStatus
		size   int64
	}
	seeds := []seed{
		{"f1", "youtube", StatusFinished, 100},
		{"f2", "youtube", StatusFinished, 200},
		{"f3", "twitch", StatusFinished, 50},
		{"e1", "youtube", StatusError, 10},
		{"c1", "youtube", StatusCancelled, 5},
		{"a1", "youtube", StatusDownloading, 0},
		{"a2", "twitch", StatusLive, 0},
		{"m1", "youtube", StatusMuxing, 0},
	}
	for _, s := range seeds {
		if _, err := db.AddJob(&Job{
			ID:       s.id,
			VideoID:  s.id,
			URL:      "u",
			Platform: s.plat,
			Status:   s.status,
		}); err != nil {
			t.Fatalf("AddJob %s: %v", s.id, err)
		}
		// file_size isn't an AddJob field — patch it via UpdateJobFields,
		// matching the production worker's post-mux flow.
		if s.size > 0 {
			if got := db.UpdateJobFields(s.id, map[string]any{"file_size": s.size}); got == nil {
				t.Fatalf("UpdateJobFields %s: nil", s.id)
			}
		}
	}

	stats, err := db.GetJobStats()
	if err != nil {
		t.Fatalf("GetJobStats: %v", err)
	}

	if stats.FinishedCount != 3 {
		t.Errorf("FinishedCount: want 3, got %d", stats.FinishedCount)
	}
	if stats.ErrorCount != 1 {
		t.Errorf("ErrorCount: want 1, got %d", stats.ErrorCount)
	}
	if stats.CancelledCount != 1 {
		t.Errorf("CancelledCount: want 1, got %d", stats.CancelledCount)
	}
	if stats.ActiveCount != 2 { // Downloading + Live
		t.Errorf("ActiveCount: want 2 (downloading+live), got %d", stats.ActiveCount)
	}
	if stats.MuxingCount != 1 {
		t.Errorf("MuxingCount: want 1, got %d", stats.MuxingCount)
	}
	if stats.YouTubeCount != 6 {
		t.Errorf("YouTubeCount: want 6, got %d", stats.YouTubeCount)
	}
	if stats.TwitchCount != 2 {
		t.Errorf("TwitchCount: want 2, got %d", stats.TwitchCount)
	}
	if stats.FinishedSize != 350 {
		t.Errorf("FinishedSize: want 350 (100+200+50), got %d", stats.FinishedSize)
	}
	if stats.ErrorSize != 10 {
		t.Errorf("ErrorSize: want 10, got %d", stats.ErrorSize)
	}
	if stats.CancelledSize != 5 {
		t.Errorf("CancelledSize: want 5, got %d", stats.CancelledSize)
	}
}

func TestGetJobStatsCachesResultsBriefly(t *testing.T) {
	// jobStatsCacheTTL = 5s. A second call within the window returns
	// the cached pointer; mutations between the two reads aren't
	// observed until the cache expires. This is documented behaviour
	// (the cache exists because GetJobStats is a full-table SUM that
	// the UI polls every couple seconds — without caching every poll
	// would scan all jobs).
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// First read: empty
	first, _ := db.GetJobStats()
	if first.FinishedCount != 0 {
		t.Fatalf("first read on empty DB: want 0 finished, got %d", first.FinishedCount)
	}

	// Mutate while cache is warm
	if _, err := db.AddJob(&Job{
		ID: "cached1", VideoID: "x", URL: "u",
		Platform: "youtube", Status: StatusFinished,
	}); err != nil {
		t.Fatal(err)
	}

	// Second read within the TTL: still 0 (cache hit)
	second, _ := db.GetJobStats()
	if second.FinishedCount != 0 {
		t.Errorf("cached read should return stale 0; got %d (cache may be too short or invalidated)", second.FinishedCount)
	}

	// Same pointer is returned (the audit comment notes "Returning
	// the cached pointer is safe because callers only read the value")
	if first != second {
		t.Error("cached call should return same pointer as first call")
	}
}

func TestGetJobStatsConcurrent(t *testing.T) {
	// Race-detector smoke: the cache hot path uses statsMu, fresh
	// reads acquire db.mu RLock. Concurrent GetJobStats from many
	// goroutines must not race on the cache slot.
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.AddJob(&Job{
		ID: "concurrent1", VideoID: "x", URL: "u",
		Platform: "youtube", Status: StatusFinished,
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = db.GetJobStats()
			}
		}()
	}
	wg.Wait()
}

// --- attachTrimsAndGaps coverage ---

func TestAttachTrimsAndGapsLoadsForMultipleJobs(t *testing.T) {
	// GetAllJobs uses attachTrimsAndGaps with a chunked WHERE-IN
	// query. Seed several jobs each with their own gaps + trims; the
	// resulting Job structs should each carry exactly their own
	// associated rows, no cross-contamination.
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, id := range []string{"j1", "j2", "j3"} {
		if _, err := db.AddJob(&Job{
			ID: id, VideoID: id, URL: "u", Status: StatusFinished,
			Gaps: []Gap{{From: 0, To: 100, Stream: "v"}},
		}); err != nil {
			t.Fatalf("AddJob %s: %v", id, err)
		}
		// Add one trim per job
		if err := db.AddTrim(&TrimRecord{
			ID:       "trim_" + id,
			JobID:    id,
			Filename: "out.mp4",
		}); err != nil {
			t.Fatalf("AddTrim %s: %v", id, err)
		}
	}

	jobs, err := db.GetAllJobs()
	if err != nil {
		t.Fatalf("GetAllJobs: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("GetAllJobs count: want 3, got %d", len(jobs))
	}

	for _, job := range jobs {
		if len(job.Gaps) != 1 {
			t.Errorf("%s: want 1 gap, got %d", job.ID, len(job.Gaps))
		}
		// Each job should have exactly one trim, and that trim's JobID
		// must match this job — no cross-contamination from the
		// chunked query.
		if len(job.Trims) != 1 {
			t.Errorf("%s: want 1 trim, got %d", job.ID, len(job.Trims))
		}
		if len(job.Trims) > 0 && job.Trims[0].JobID != job.ID {
			t.Errorf("%s: trim's JobID should match (cross-contamination!), got %q", job.ID, job.Trims[0].JobID)
		}
	}
}

// --- History cap ---

func TestAddToHistoryDeduplicates(t *testing.T) {
	// AddToHistory uses INSERT OR IGNORE so re-adding the same video
	// ID is a no-op. Verify HasProcessed reflects the single entry.
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i := 0; i < 5; i++ {
		if err := db.AddToHistory("dedup-vid"); err != nil {
			t.Fatalf("AddToHistory iter %d: %v", i, err)
		}
	}

	processed, err := db.HasProcessed("dedup-vid")
	if err != nil {
		t.Fatalf("HasProcessed: %v", err)
	}
	if !processed {
		t.Error("HasProcessed: want true after AddToHistory")
	}
	if proc, _ := db.HasProcessed("never-added"); proc {
		t.Error("HasProcessed for never-added videoID: want false")
	}
}

// --- ImportFromJSON ---

func TestImportFromJSONLoadsJobsAndHistory(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	importPath := filepath.Join(dir, "import.json")
	importJSON := map[string]any{
		"jobs": []map[string]any{
			{
				"id":       "imp_a",
				"videoId":  "imp_a",
				"url":      "https://example.com/a",
				"platform": "youtube",
				"status":   "Finished",
			},
			{
				"id":      "imp_b",
				"videoId": "imp_b",
				"url":     "https://example.com/b",
				// platform omitted — should default to "youtube"
				"status": "Finished",
			},
		},
		"history":    []string{"hist_v1", "hist_v2"},
		"lastVideos": map[string]string{"chan_a": "vid_a"},
	}
	data, _ := json.Marshal(importJSON)
	if err := os.WriteFile(importPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := db.ImportFromJSON(importPath); err != nil {
		t.Fatalf("ImportFromJSON: %v", err)
	}

	// Both jobs persisted
	a, _ := db.GetJob("imp_a")
	if a == nil {
		t.Error("job imp_a should exist after import")
	}
	b, _ := db.GetJob("imp_b")
	if b == nil {
		t.Fatal("job imp_b should exist after import")
	}
	if b.Platform != "youtube" {
		t.Errorf("missing-platform default: want youtube, got %q", b.Platform)
	}

	// History entries
	for _, vid := range []string{"hist_v1", "hist_v2"} {
		if proc, _ := db.HasProcessed(vid); !proc {
			t.Errorf("history %s: not present after import", vid)
		}
	}
}

func TestImportFromJSONHandlesMissingFile(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.ImportFromJSON(filepath.Join(dir, "no-such-file.json")); err == nil {
		t.Error("missing file: want error, got nil")
	}
}

func TestImportFromJSONRejectsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	importPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(importPath, []byte("not-json{"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := db.ImportFromJSON(importPath); err == nil {
		t.Error("invalid JSON: want error, got nil")
	}
}

// --- Concurrent writers/readers (race smoke) ---

func TestConcurrentReadsAndWrites(t *testing.T) {
	// Stresses the db.mu RLock/Lock interleaving with parallel
	// readers and a single writer. Run under -race to verify no
	// data races on the SQL connection or the subscriber slices.
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i := 0; i < 5; i++ {
		if _, err := db.AddJob(&Job{
			ID: fmt.Sprintf("seed%d", i), VideoID: fmt.Sprintf("v%d", i),
			URL: "u", Status: StatusFinished,
		}); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers: GetJob + GetAllJobs + HasActiveJob
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = db.GetJob("seed0")
				_, _ = db.GetAllJobs()
				_, _ = db.HasActiveJob("v0")
			}
		}()
	}

	// Writer: UpdateJobFields tickles every seed
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("seed%d", i%5)
		_ = db.UpdateJobFields(id, map[string]any{
			"progress": fmt.Sprintf("iter %d", i),
		})
	}
	close(stop)
	wg.Wait()
}

// --- Subscriber lifecycle ---

func TestUnsubscribeStopsCallback(t *testing.T) {
	// Locks down the unsubscribe contract: after the returned func
	// runs, the callback must NOT fire on subsequent updates. Without
	// this, an old subscriber can keep firing on a slice clone that
	// captured its function pointer.
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.AddJob(&Job{ID: "u1", VideoID: "u", URL: "u", Status: StatusUpcoming}); err != nil {
		t.Fatal(err)
	}

	hits := make(chan *Job, 4)
	unsub := db.OnJobUpdate(func(j *Job) { hits <- j })

	// First update should fire
	db.UpdateJobFields("u1", map[string]any{"status": StatusDownloading})

	select {
	case <-hits:
		// good
	case <-time.After(time.Second):
		t.Fatal("first update should have fired the subscriber")
	}

	// Unsubscribe and trigger again
	unsub()
	db.UpdateJobFields("u1", map[string]any{"status": StatusFinished})

	select {
	case j := <-hits:
		t.Errorf("post-unsubscribe update should NOT fire; got %v", j)
	case <-time.After(200 * time.Millisecond):
		// expected — no callback after unsubscribe
	}
}

// TestOnJobChangeReceivesChangedColumns covers the new fine-grained
// event API added for DECISIONS #21. Each UpdateJobFields call must
// fire OnJobChange with the schema column names that were written —
// minus updated_at, which is bumped on every call and would defeat
// the consumer's "skip identity-only updates" optimisation.
func TestOnJobChangeReceivesChangedColumns(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.AddJob(&Job{
		ID: "ch1", VideoID: "v", URL: "u", Status: StatusUpcoming,
	}); err != nil {
		t.Fatal(err)
	}

	events := make(chan *JobChange, 4)
	unsub := db.OnJobChange(func(c *JobChange) { events <- c })
	defer unsub()

	db.UpdateJobFields("ch1", map[string]any{
		"status":   StatusDownloading,
		"progress": "10%",
	})

	select {
	case ev := <-events:
		if ev.Job == nil || ev.Job.ID != "ch1" {
			t.Errorf("event Job: want ch1, got %+v", ev.Job)
		}
		// updated_at must NOT appear; the writer-set columns must.
		seen := map[string]bool{}
		for _, c := range ev.Changes {
			seen[c] = true
		}
		if !seen["status"] || !seen["progress"] {
			t.Errorf("Changes missing expected columns: %v", ev.Changes)
		}
		if seen["updated_at"] {
			t.Error("Changes should NOT include updated_at — every call bumps it")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnJobChange did not fire within 2s")
	}
}

func TestOnJobChangeFiresAlongsideOnJobUpdate(t *testing.T) {
	// Both APIs coexist during the migration. A single UpdateJobFields
	// call must fan out to BOTH OnJobUpdate and OnJobChange
	// subscribers — neither path can starve the other.
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.AddJob(&Job{
		ID: "dual", VideoID: "v", URL: "u", Status: StatusUpcoming,
	}); err != nil {
		t.Fatal(err)
	}

	updateHits := make(chan *Job, 1)
	changeHits := make(chan *JobChange, 1)

	uu := db.OnJobUpdate(func(j *Job) { updateHits <- j })
	defer uu()
	uc := db.OnJobChange(func(c *JobChange) { changeHits <- c })
	defer uc()

	db.UpdateJobFields("dual", map[string]any{"status": StatusDownloading})

	select {
	case <-updateHits:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("OnJobUpdate did not fire")
	}
	select {
	case <-changeHits:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("OnJobChange did not fire")
	}
}

func TestOnJobChangeUnsubscribeStopsCallbacks(t *testing.T) {
	// Mirror of TestUnsubscribeStopsCallback for the new API. Once
	// the returned unsub func runs, no further events should arrive.
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.AddJob(&Job{
		ID: "uns", VideoID: "v", URL: "u", Status: StatusUpcoming,
	}); err != nil {
		t.Fatal(err)
	}

	hits := make(chan *JobChange, 4)
	unsub := db.OnJobChange(func(c *JobChange) { hits <- c })

	db.UpdateJobFields("uns", map[string]any{"status": StatusDownloading})
	select {
	case <-hits:
	case <-time.After(time.Second):
		t.Fatal("first event should have fired")
	}

	unsub()
	db.UpdateJobFields("uns", map[string]any{"status": StatusFinished})

	select {
	case ev := <-hits:
		t.Errorf("post-unsubscribe event should NOT fire; got %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// expected
	}
}

func TestOnJobChangeSurvivesPanic(t *testing.T) {
	// safeCallJobChange wraps each subscriber in a per-callback recover
	// so one panicking handler doesn't break the rest of the fan-out
	// or the writer path.
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.AddJob(&Job{
		ID: "p", VideoID: "v", URL: "u", Status: StatusUpcoming,
	}); err != nil {
		t.Fatal(err)
	}

	panicked := false
	survived := make(chan struct{}, 1)

	uPanic := db.OnJobChange(func(*JobChange) {
		panicked = true
		panic("test panic")
	})
	defer uPanic()
	uOK := db.OnJobChange(func(*JobChange) { survived <- struct{}{} })
	defer uOK()

	db.UpdateJobFields("p", map[string]any{"status": StatusFinished})

	select {
	case <-survived:
		// good — second subscriber fired despite first one panicking
	case <-time.After(2 * time.Second):
		t.Fatal("second subscriber should fire even if first panics")
	}
	if !panicked {
		t.Error("panicking subscriber should have been invoked")
	}
}

func TestSubscriberSliceShrinksAfterChurn(t *testing.T) {
	// Locks down shrinkJobUpdateSubs: after many subscribe/unsubscribe
	// cycles the underlying slice is rebuilt at smaller capacity so
	// long-running processes don't accumulate dead capacity. The
	// shrink trigger is cap > 4*len; we drive enough churn that the
	// post-shrink cap is meaningfully smaller than the pre-shrink
	// peak.
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Subscribe 32 callbacks, then unsubscribe 30 of them.
	unsubs := make([]func(), 0, 32)
	for i := 0; i < 32; i++ {
		unsubs = append(unsubs, db.OnJobUpdate(func(*Job) {}))
	}
	for i := 0; i < 30; i++ {
		unsubs[i]()
	}

	// After 30 unsubscribes, the surviving 2 should have triggered
	// shrinkJobUpdateSubs (cap > 4*len). Read the surviving slice via
	// the subMu lock.
	db.subMu.RLock()
	defer db.subMu.RUnlock()
	if len(db.onJobUpdate) != 2 {
		t.Errorf("subscriber count: want 2, got %d", len(db.onJobUpdate))
	}
	// Cap should be much smaller than the pre-shrink peak (32+).
	// We don't assert an exact value — just that the shrink ran.
	if cap(db.onJobUpdate) > 4*len(db.onJobUpdate) {
		t.Errorf("cap=%d, len=%d: expected shrink to bring cap within 4x len", cap(db.onJobUpdate), len(db.onJobUpdate))
	}
}
