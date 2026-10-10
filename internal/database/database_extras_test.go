package database

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- GetJobStats ---

func TestGetJobStatsEmptyDatabase(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
		// A Finished incomplete-tail job being Resumed keeps its file_size
		// while it is Downloading again.
		{"a1", "youtube", StatusDownloading, 1000},
		{"a2", "twitch", StatusLive, 0},
		{"m1", "youtube", StatusMuxing, 0},
		{"u1", "youtube", StatusUpcoming, 0},
		{"q1", "youtube", StatusQueued, 0},
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
	if stats.YouTubeCount != 8 {
		t.Errorf("YouTubeCount: want 8, got %d", stats.YouTubeCount)
	}
	if stats.TotalCount != 10 { // every row, upcoming and queued included
		t.Errorf("TotalCount: want 10, got %d", stats.TotalCount)
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
	// The platform sizes count the same settled statuses, so the Stats tab's
	// two cards add up to its Total Recorded — the resuming a1 is in neither.
	if stats.YouTubeSize != 315 || stats.TwitchSize != 50 {
		t.Errorf("platform sizes: want youtube 315 and twitch 50, got %d and %d", stats.YouTubeSize, stats.TwitchSize)
	}
	if settled := stats.FinishedSize + stats.ErrorSize + stats.CancelledSize; stats.YouTubeSize+stats.TwitchSize != settled {
		t.Errorf("platform sizes sum to %d, the settled total is %d", stats.YouTubeSize+stats.TwitchSize, settled)
	}
}

// Total Recording Time and Chat Messages — both dashboards' Activity figures
// — sum length_seconds and total_chat_messages over Finished rows only, and a
// row with an empty platform counts as YouTube in both the count and the
// size. The aggregate test above seeds neither column and no empty platform,
// so zeroing either sum, summing them over every status, or dropping the
// empty platform from the YouTube clauses survived every suite.
//
// Mutants: `THEN length_seconds` or `THEN total_chat_messages` → `THEN 0`;
// either sum widened to Error/Downloading rows; the YouTube count's or size's
// platform clause narrowed to `platform = 'youtube'`.
func TestGetJobStatsSumsFinishedDurationChatAndLegacyPlatform(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	type seed struct {
		id       string
		plat     string
		status   JobStatus
		size     int64
		length   int
		messages int
	}
	seeds := []seed{
		{"f1", "youtube", StatusFinished, 100, 3600, 1200},
		{"f2", "twitch", StatusFinished, 50, 600, 34},
		{"legacy", "youtube", StatusFinished, 7, 60, 5}, // platform blanked below
		// Not Finished: their length and chat must not count.
		{"e1", "youtube", StatusError, 10, 9000, 9000},
		{"a1", "youtube", StatusDownloading, 0, 7000, 7000},
	}
	for _, s := range seeds {
		if _, err := db.AddJob(&Job{ID: s.id, VideoID: s.id, URL: "u", Platform: s.plat, Status: s.status}); err != nil {
			t.Fatalf("AddJob %s: %v", s.id, err)
		}
		if got := db.UpdateJobFields(s.id, map[string]any{
			"file_size": s.size, "length_seconds": s.length, "total_chat_messages": s.messages,
		}); got == nil {
			t.Fatalf("UpdateJobFields %s: nil", s.id)
		}
	}
	// A row whose platform is empty — the stats query counts it as YouTube.
	if _, err := db.db.Exec(`UPDATE jobs SET platform = '' WHERE id = 'legacy'`); err != nil {
		t.Fatal(err)
	}

	stats, err := db.GetJobStats()
	if err != nil {
		t.Fatalf("GetJobStats: %v", err)
	}
	if stats.TotalDuration != 3600+600+60 {
		t.Errorf("TotalDuration = %d, want %d (Finished rows only)", stats.TotalDuration, 3600+600+60)
	}
	if stats.TotalChatMessages != 1200+34+5 {
		t.Errorf("TotalChatMessages = %d, want %d (Finished rows only)", stats.TotalChatMessages, 1200+34+5)
	}
	if stats.YouTubeCount != 4 || stats.TwitchCount != 1 {
		t.Errorf("platform counts = youtube %d, twitch %d; want 4 and 1 — the empty platform is YouTube",
			stats.YouTubeCount, stats.TwitchCount)
	}
	if stats.YouTubeSize != 100+7+10 || stats.TwitchSize != 50 {
		t.Errorf("platform sizes = youtube %d, twitch %d; want %d and 50 — the empty platform is YouTube",
			stats.YouTubeSize, stats.TwitchSize, 100+7+10)
	}
}

func TestJobStatsQueuedCount(t *testing.T) {
	t.Parallel()
	// Queued is a resting state: it surfaces in its own QueuedCount bucket
	// and must NOT inflate ActiveCount (a waiting job is not a running
	// download).
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, s := range []struct {
		id     string
		status JobStatus
	}{
		{"dl1", StatusDownloading},
		{"q1", StatusQueued},
	} {
		if _, err := db.AddJob(&Job{ID: s.id, VideoID: s.id, URL: "u", Status: s.status}); err != nil {
			t.Fatalf("AddJob %s: %v", s.id, err)
		}
	}

	stats, err := db.GetJobStats()
	if err != nil {
		t.Fatalf("GetJobStats: %v", err)
	}
	if stats.ActiveCount != 1 {
		t.Errorf("ActiveCount: want 1 (Downloading only), got %d", stats.ActiveCount)
	}
	if stats.QueuedCount != 1 {
		t.Errorf("QueuedCount: want 1, got %d", stats.QueuedCount)
	}
}

func TestGetJobStatsCachesResultsBriefly(t *testing.T) {
	t.Parallel()
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

// The cache is never invalidated on writes, so expiry is the only way the
// Stats tab and the E T overlay ever see a new figure. Nothing pinned it: a
// cache that never expired showed the first snapshot for the life of the
// process with every suite green.
//
// Mutants: the TTL check → `true` (never expires), or a TTL far longer than
// jobStatsCacheTTL — the row added after the first read never shows.
func TestGetJobStatsCacheExpires(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	first, err := db.GetJobStats()
	if err != nil || first.FinishedCount != 0 {
		t.Fatalf("first read on an empty DB = %+v, %v", first, err)
	}
	if _, err := db.AddJob(&Job{ID: "late", VideoID: "x", URL: "u", Platform: "youtube", Status: StatusFinished}); err != nil {
		t.Fatal(err)
	}
	// Age the cached snapshot just past its TTL rather than sleeping 5 s.
	db.statsMu.Lock()
	db.statsCachedAt = time.Now().Add(-jobStatsCacheTTL - time.Second)
	db.statsMu.Unlock()

	fresh, err := db.GetJobStats()
	if err != nil {
		t.Fatal(err)
	}
	if fresh == first || fresh.FinishedCount != 1 {
		t.Errorf("a read past the TTL served the stale snapshot: FinishedCount %d, want 1", fresh.FinishedCount)
	}
}

func TestGetJobStatsConcurrent(t *testing.T) {
	t.Parallel()
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
	for range 16 {
		wg.Go(func() {
			for range 50 {
				_, _ = db.GetJobStats()
			}
		})
	}
	wg.Wait()
}

// --- attachTrimsAndGaps coverage ---

// TestGetAllJobsFailsWhenAChildLoadFails: a child query that fails must fail
// GetAllJobs rather than hand back jobs stripped of their segments. The orphan
// scanner reads segment chat files through this loader, so "no segments" there
// reads as "these chat files belong to nothing" and makes them deletable.
//
// Mutant: the pre-fix loader logged the query error and returned the jobs.
func TestGetAllJobsFailsWhenAChildLoadFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.AddJob(&Job{ID: "j1", VideoID: "j1", URL: "u", Status: StatusFinished}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if _, err := db.db.Exec(`DROP TABLE segments`); err != nil {
		t.Fatalf("drop segments: %v", err)
	}

	jobs, err := db.GetAllJobs()
	if err == nil {
		t.Fatalf("GetAllJobs returned %d job(s) and no error with the segments load failing", len(jobs))
	}
	if !strings.Contains(err.Error(), "segments") {
		t.Errorf("err = %v, want it to name the failed segments load", err)
	}
}

func TestAttachTrimsAndGapsLoadsForMultipleJobs(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	// AddToHistory uses INSERT OR IGNORE so re-adding the same video
	// ID is a no-op. Verify HasProcessed reflects the single entry.
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i := range 5 {
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

// TestHistoryCapEvictsTheOldestRows fills history to the cap with rows dated
// in the past, one minute apart, then records three more. The cap must drop
// the three oldest and keep everything else: the rows just recorded are the
// archive pass's only guard against re-creating the jobs they were recorded
// for, so a prune that took them would answer HasProcessed=false for videos
// that were just jobbed.
//
// MUTANTS: ORDER BY added_at DESC in pruneHistory (the new rows go, the oldest
// stay); an off-by-one cap (historyCap+1 or -1 in the LIMIT: the count is
// wrong); dropping the pruneHistory call from AddToHistory (the count grows).
func TestHistoryCapEvictsTheOldestRows(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	seed := func(i int) string { return fmt.Sprintf("seed%05d", i) }
	tx, err := db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range historyCap {
		if _, err := tx.Exec(`INSERT INTO history (video_id, added_at) VALUES (?, ?)`,
			seed(i), base.Add(time.Duration(i)*time.Minute).Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	fresh := []string{"fresh-a", "fresh-b", "fresh-c"}
	for _, id := range fresh {
		if err := db.AddToHistory(id); err != nil {
			t.Fatalf("AddToHistory(%s): %v", id, err)
		}
	}

	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != historyCap {
		t.Errorf("history holds %d rows, want the cap %d", n, historyCap)
	}
	for _, id := range fresh {
		if ok, err := db.HasProcessed(id); err != nil || !ok {
			t.Errorf("HasProcessed(%s) = %v, %v: the row just recorded was evicted by the cap", id, ok, err)
		}
	}
	for i := range len(fresh) {
		if ok, _ := db.HasProcessed(seed(i)); ok {
			t.Errorf("%s, among the %d oldest rows, survived the cap", seed(i), len(fresh))
		}
	}
	for _, i := range []int{len(fresh), historyCap - 1} {
		if ok, _ := db.HasProcessed(seed(i)); !ok {
			t.Errorf("%s was evicted, but only the %d oldest rows should go", seed(i), len(fresh))
		}
	}
}

// --- Concurrent writers/readers (race smoke) ---

func TestConcurrentReadsAndWrites(t *testing.T) {
	t.Parallel()
	// Stresses the db.mu RLock/Lock interleaving with parallel
	// readers and a single writer. Run under -race to verify no
	// data races on the SQL connection or the subscriber slices.
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i := range 5 {
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
	for range 4 {
		wg.Go(func() {
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
		})
	}

	// Writer: UpdateJobFields tickles every seed
	for i := range 100 {
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestOnJobAddedFires covers the new lifecycle event added for the
// DECISIONS #21 follow-on. AddJob must deliver a JobAdded carrying
// the inserted Job pointer (post-write — UpdatedAt populated).
func TestOnJobAddedFires(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	events := make(chan *JobAdded, 4)
	unsub := db.OnJobAdded(func(ev *JobAdded) { events <- ev })
	defer unsub()

	job := &Job{ID: "added1", VideoID: "v", URL: "u", Status: StatusUpcoming}
	added, err := db.AddJob(job)
	if err != nil {
		t.Fatal(err)
	}
	if !added {
		t.Fatal("AddJob returned added=false on a fresh row")
	}

	select {
	case ev := <-events:
		if ev.Job == nil || ev.Job.ID != "added1" {
			t.Errorf("event Job: want added1, got %+v", ev.Job)
		}
		if ev.Job.UpdatedAt == "" {
			t.Error("event Job.UpdatedAt should be populated post-write")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnJobAdded did not fire within 2s")
	}
}

// TestOnJobAddedFiresAndOnJobsChangeDoesNot locks the post-migration
// behaviour: AddJob fires OnJobAdded but NO LONGER fires OnJobsChange.
// The legacy full-list dispatch on insert was dropped as part of
// DECISIONS #21 consumer migration once the WS broadcaster + TUI
// both wired OnJobAdded handlers. The OnJobsChange path is still
// the live writer dispatch for DeleteJob / AddTrim / DeleteTrim /
// BatchSetWatched, which TestOnJobsChangeSubscriber covers.
func TestOnJobAddedFiresAndOnJobsChangeDoesNot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	addedHits := make(chan *JobAdded, 1)
	listHits := make(chan []*Job, 1)

	uA := db.OnJobAdded(func(ev *JobAdded) { addedHits <- ev })
	defer uA()
	uL := db.OnJobsChange(func(jobs []*Job) { listHits <- jobs })
	defer uL()

	if _, err := db.AddJob(&Job{
		ID: "post_migration", VideoID: "v", URL: "u", Status: StatusUpcoming,
	}); err != nil {
		t.Fatal(err)
	}

	// OnJobAdded must fire.
	select {
	case <-addedHits:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("OnJobAdded did not fire")
	}

	// OnJobsChange must NOT fire on AddJob any more. A 200ms wait is
	// enough — dispatchJobsChange's goroutine, if it were going to run,
	// would have surfaced by now.
	select {
	case jobs := <-listHits:
		t.Errorf("OnJobsChange should NOT fire on AddJob post-migration; got %d jobs", len(jobs))
	case <-time.After(200 * time.Millisecond):
		// expected — OnJobsChange suppressed for AddJob
	}
}

// TestOnJobAddedDoesNotFireOnDuplicate guards the INSERT OR IGNORE
// path: when AddJob returns added=false because the row already
// existed, no OnJobAdded event must escape — subscribers shouldn't
// see ghost insertions for IDs that didn't actually change.
func TestOnJobAddedDoesNotFireOnDuplicate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// First insert succeeds — drain the event so the channel doesn't
	// trigger a false positive on the second AddJob.
	events := make(chan *JobAdded, 4)
	unsub := db.OnJobAdded(func(ev *JobAdded) { events <- ev })
	defer unsub()

	if _, err := db.AddJob(&Job{
		ID: "dup", VideoID: "v", URL: "u", Status: StatusUpcoming,
	}); err != nil {
		t.Fatal(err)
	}
	<-events // first insert's event

	// Second AddJob with the same ID must report added=false and
	// must NOT fire OnJobAdded.
	added, err := db.AddJob(&Job{
		ID: "dup", VideoID: "v", URL: "u", Status: StatusUpcoming,
	})
	if err != nil {
		t.Fatalf("duplicate AddJob returned error: %v", err)
	}
	if added {
		t.Fatal("duplicate AddJob returned added=true")
	}

	select {
	case ev := <-events:
		t.Errorf("OnJobAdded should not fire for a duplicate; got %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// expected — no event
	}
}

// TestOnJobAddedUnsubscribeStopsCallbacks confirms the returned
// unsub closure removes the subscriber cleanly.
func TestOnJobAddedUnsubscribeStopsCallbacks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	hits := make(chan *JobAdded, 4)
	unsub := db.OnJobAdded(func(ev *JobAdded) { hits <- ev })

	if _, err := db.AddJob(&Job{
		ID: "uns_a", VideoID: "v", URL: "u", Status: StatusUpcoming,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hits:
	case <-time.After(time.Second):
		t.Fatal("first event should have fired")
	}

	unsub()
	if _, err := db.AddJob(&Job{
		ID: "uns_b", VideoID: "v", URL: "u", Status: StatusUpcoming,
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-hits:
		t.Errorf("post-unsubscribe event should NOT fire; got %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// expected
	}
}

// TestOnJobAddedSurvivesPanic locks safeCallJobAdded's per-callback
// recover so one panicking subscriber can't break the rest of the
// fan-out or the writer path.
func TestOnJobAddedSurvivesPanic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	panicked := false
	survived := make(chan struct{}, 1)

	uPanic := db.OnJobAdded(func(*JobAdded) {
		panicked = true
		panic("test panic")
	})
	defer uPanic()
	uOK := db.OnJobAdded(func(*JobAdded) { survived <- struct{}{} })
	defer uOK()

	if _, err := db.AddJob(&Job{
		ID: "pan_a", VideoID: "v", URL: "u", Status: StatusUpcoming,
	}); err != nil {
		t.Fatal(err)
	}

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

// TestOnJobDeletedFires covers the JobDeleted lifecycle event added
// for the DECISIONS #21 follow-on. DeleteJob must deliver a JobDeleted
// carrying the removed job's ID once the row is gone.
func TestOnJobDeletedFires(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.AddJob(&Job{
		ID: "del1", VideoID: "v", URL: "u", Status: StatusUpcoming,
	}); err != nil {
		t.Fatal(err)
	}

	events := make(chan *JobDeleted, 4)
	unsub := db.OnJobDeleted(func(ev *JobDeleted) { events <- ev })
	defer unsub()

	if err := db.DeleteJob("del1"); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-events:
		if ev.JobID != "del1" {
			t.Errorf("event JobID: want del1, got %q", ev.JobID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnJobDeleted did not fire within 2s")
	}
}

// TestOnJobDeletedFiresAndOnJobsChangeDoesNot locks the post-migration
// behaviour: DeleteJob fires OnJobDeleted but no longer fires
// OnJobsChange. The legacy full-list dispatch on delete was dropped
// once the WS broadcaster + TUI consume the targeted lifecycle event
// (DECISIONS #21 consumer migration). BatchSetWatched is the only
// remaining OnJobsChange writer; tests targeting that path use it
// directly.
func TestOnJobDeletedFiresAndOnJobsChangeDoesNot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.AddJob(&Job{
		ID: "post_keep", VideoID: "v1", URL: "u1", Status: StatusUpcoming,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddJob(&Job{
		ID: "post_target", VideoID: "v2", URL: "u2", Status: StatusUpcoming,
	}); err != nil {
		t.Fatal(err)
	}

	deletedHits := make(chan *JobDeleted, 1)
	listHits := make(chan []*Job, 1)

	uD := db.OnJobDeleted(func(ev *JobDeleted) { deletedHits <- ev })
	defer uD()
	uL := db.OnJobsChange(func(jobs []*Job) { listHits <- jobs })
	defer uL()

	if err := db.DeleteJob("post_target"); err != nil {
		t.Fatal(err)
	}

	// OnJobDeleted must fire.
	select {
	case <-deletedHits:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("OnJobDeleted did not fire")
	}
	// OnJobsChange must NOT fire — DeleteJob no longer dispatches it.
	select {
	case jobs := <-listHits:
		t.Errorf("OnJobsChange should NOT fire on DeleteJob post-migration; got %d jobs", len(jobs))
	case <-time.After(200 * time.Millisecond):
		// expected
	}
}

// TestOnJobDeletedDoesNotFireOnMissingID guards the rowsAffected==0
// branch: DeleteJob with a nonexistent ID is a SQL no-op — no
// targeted event must escape. Post-DECISIONS-#21, OnJobsChange also
// no longer fires on DeleteJob, so the missing-ID case is silent
// across both the legacy and new event paths.
func TestOnJobDeletedDoesNotFireOnMissingID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	events := make(chan *JobDeleted, 4)
	unsub := db.OnJobDeleted(func(ev *JobDeleted) { events <- ev })
	defer unsub()

	// DELETE on an ID that doesn't exist — SQL succeeds, 0 rows affected.
	if err := db.DeleteJob("ghost"); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-events:
		t.Errorf("OnJobDeleted should not fire for a missing ID; got %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// expected — no event
	}
}

// TestOnJobDeletedUnsubscribeStopsCallbacks confirms the returned
// unsub closure removes the subscriber cleanly.
func TestOnJobDeletedUnsubscribeStopsCallbacks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.AddJob(&Job{
		ID: "uns_d_a", VideoID: "v", URL: "u", Status: StatusUpcoming,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddJob(&Job{
		ID: "uns_d_b", VideoID: "v", URL: "u", Status: StatusUpcoming,
	}); err != nil {
		t.Fatal(err)
	}

	hits := make(chan *JobDeleted, 4)
	unsub := db.OnJobDeleted(func(ev *JobDeleted) { hits <- ev })

	if err := db.DeleteJob("uns_d_a"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hits:
	case <-time.After(time.Second):
		t.Fatal("first event should have fired")
	}

	unsub()
	if err := db.DeleteJob("uns_d_b"); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-hits:
		t.Errorf("post-unsubscribe event should NOT fire; got %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// expected
	}
}

// TestEmptyDBSnapshotIsNonNil locks the empty-DB normalisation in
// snapshotJobsChange: when a writer that fires OnJobsChange (today
// just BatchSetWatched) leaves zero rows behind, the snapshot must
// be the empty []*Job{} sentinel rather than nil so
// dispatchJobsChange's nil-check doesn't suppress the dispatch
// entirely. Pre-fix, a BatchSetWatched on an empty job set (or a
// future writer that empties the table) would silently skip the
// fan-out; subscribers needed the empty-list signal to clear
// their local state.
//
// (Pre-DECISIONS-#21 this regression test exercised DeleteJob; that
// writer no longer fires OnJobsChange, so the test exercises
// snapshotJobsChange via BatchSetWatched-on-zero-rows instead. The
// underlying invariant is unchanged.)
func TestEmptyDBSnapshotIsNonNil(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	listHits := make(chan []*Job, 1)
	uL := db.OnJobsChange(func(jobs []*Job) { listHits <- jobs })
	defer uL()

	// BatchSetWatched on an empty ID set is a no-op SQL-wise but still
	// goes through snapshotJobsChange + dispatchJobsChange — except
	// that the empty-IDs early return at the top of BatchSetWatched
	// skips the dispatch. Use a non-existent ID instead so the
	// transaction runs (matches zero rows) and dispatch fires.
	if err := db.BatchSetWatched([]string{"nonexistent_id"}, true); err != nil {
		t.Fatal(err)
	}

	select {
	case jobs := <-listHits:
		if jobs == nil {
			t.Errorf("OnJobsChange snapshot must be non-nil when subscribers exist (got nil)")
		}
		if len(jobs) != 0 {
			t.Errorf("OnJobsChange snapshot: want 0 jobs (empty DB), got %d", len(jobs))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnJobsChange did not fire — snapshotJobsChange may have returned nil and suppressed dispatch")
	}
}

// TestOnJobDeletedSurvivesPanic locks safeCallJobDeleted's per-callback
// recover so one panicking subscriber can't break the rest of the
// fan-out or the writer path.
func TestOnJobDeletedSurvivesPanic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.AddJob(&Job{
		ID: "pan_d", VideoID: "v", URL: "u", Status: StatusUpcoming,
	}); err != nil {
		t.Fatal(err)
	}

	panicked := false
	survived := make(chan struct{}, 1)

	uPanic := db.OnJobDeleted(func(*JobDeleted) {
		panicked = true
		panic("test panic")
	})
	defer uPanic()
	uOK := db.OnJobDeleted(func(*JobDeleted) { survived <- struct{}{} })
	defer uOK()

	if err := db.DeleteJob("pan_d"); err != nil {
		t.Fatal(err)
	}

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

// TestOnTrimsChangedFiresOnAddTrim covers the AddTrim writer path of
// the TrimsChanged lifecycle event added for DECISIONS #21.
func TestOnTrimsChangedFiresOnAddTrim(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.AddJob(&Job{
		ID: "trim_parent", VideoID: "v", URL: "u", Status: StatusFinished,
	}); err != nil {
		t.Fatal(err)
	}

	events := make(chan *TrimsChanged, 4)
	unsub := db.OnTrimsChanged(func(ev *TrimsChanged) { events <- ev })
	defer unsub()

	if err := db.AddTrim(&TrimRecord{
		ID:        "trim1",
		JobID:     "trim_parent",
		StartTime: 0,
		EndTime:   10,
		Filename:  "trim1.mp4",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-events:
		if ev.JobID != "trim_parent" {
			t.Errorf("event JobID: want trim_parent, got %q", ev.JobID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnTrimsChanged did not fire on AddTrim within 2s")
	}
}

// TestOnTrimsChangedFiresOnDeleteTrim covers the DeleteTrim writer
// path. The handler must look up the parent job_id BEFORE the DELETE
// so the targeted event still carries it post-removal.
func TestOnTrimsChangedFiresOnDeleteTrim(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.AddJob(&Job{
		ID: "trim_parent_d", VideoID: "v", URL: "u", Status: StatusFinished,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddTrim(&TrimRecord{
		ID: "trim_d_1", JobID: "trim_parent_d", StartTime: 0, EndTime: 5,
		Filename: "d1.mp4", CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	// Subscribe AFTER the AddTrim so the channel doesn't catch its event.
	events := make(chan *TrimsChanged, 4)
	unsub := db.OnTrimsChanged(func(ev *TrimsChanged) { events <- ev })
	defer unsub()

	if err := db.DeleteTrim("trim_d_1"); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-events:
		if ev.JobID != "trim_parent_d" {
			t.Errorf("event JobID: want trim_parent_d, got %q", ev.JobID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnTrimsChanged did not fire on DeleteTrim within 2s")
	}
}

// TestOnTrimsChangedDoesNotFireOnMissingTrim guards the ErrNoRows
// branch in DeleteTrim. The trim doesn't exist, so the SELECT
// short-circuits before the DELETE and no targeted event is emitted.
// (OnJobsChange still dispatches via the snapshot path — that's the
// existing legacy contract this commit doesn't alter.)
func TestOnTrimsChangedDoesNotFireOnMissingTrim(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	events := make(chan *TrimsChanged, 4)
	unsub := db.OnTrimsChanged(func(ev *TrimsChanged) { events <- ev })
	defer unsub()

	if err := db.DeleteTrim("ghost_trim"); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-events:
		t.Errorf("OnTrimsChanged should not fire for a missing trim; got %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// expected — no event
	}
}

// TestOnTrimsChangedUnsubscribeStopsCallbacks confirms the returned
// unsub closure removes the subscriber cleanly across both AddTrim
// and DeleteTrim writer paths.
func TestOnTrimsChangedUnsubscribeStopsCallbacks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.AddJob(&Job{
		ID: "uns_t", VideoID: "v", URL: "u", Status: StatusFinished,
	}); err != nil {
		t.Fatal(err)
	}

	hits := make(chan *TrimsChanged, 4)
	unsub := db.OnTrimsChanged(func(ev *TrimsChanged) { hits <- ev })

	if err := db.AddTrim(&TrimRecord{
		ID: "uns_t_1", JobID: "uns_t", StartTime: 0, EndTime: 5,
		Filename: "u1.mp4", CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hits:
	case <-time.After(time.Second):
		t.Fatal("first AddTrim event should have fired")
	}

	unsub()
	if err := db.AddTrim(&TrimRecord{
		ID: "uns_t_2", JobID: "uns_t", StartTime: 5, EndTime: 10,
		Filename: "u2.mp4", CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-hits:
		t.Errorf("post-unsubscribe event should NOT fire; got %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// expected
	}
}

// TestOnTrimsChangedSurvivesPanic locks safeCallTrimsChanged's
// per-callback recover.
func TestOnTrimsChangedSurvivesPanic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.AddJob(&Job{
		ID: "pan_t", VideoID: "v", URL: "u", Status: StatusFinished,
	}); err != nil {
		t.Fatal(err)
	}

	panicked := false
	survived := make(chan struct{}, 1)

	uPanic := db.OnTrimsChanged(func(*TrimsChanged) {
		panicked = true
		panic("test panic")
	})
	defer uPanic()
	uOK := db.OnTrimsChanged(func(*TrimsChanged) { survived <- struct{}{} })
	defer uOK()

	if err := db.AddTrim(&TrimRecord{
		ID: "pan_t_1", JobID: "pan_t", StartTime: 0, EndTime: 5,
		Filename: "p1.mp4", CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

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
	t.Parallel()
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
	for range 32 {
		unsubs = append(unsubs, db.OnJobUpdate(func(*Job) {}))
	}
	for i := range 30 {
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

// --- Orphaned history ---

func TestOrphanedHistory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, v := range []string{"vidOrphan01", "vidHasJob02", "vidOrphan03"} {
		if err := db.AddToHistory(v); err != nil {
			t.Fatal(err)
		}
	}
	// vidHasJob02 gets a job -> not orphaned.
	if _, err := db.AddJob(&Job{
		ID: "vidHasJob02", VideoID: "vidHasJob02", Platform: "youtube",
		Status: StatusFinished, CreatedAt: "2026-07-14T00:00:00Z", UpdatedAt: "2026-07-14T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	// Twitch regression: history is keyed by the "tw_"-prefixed JOB ID, while
	// the monitor-created Twitch job stores the UNPREFIXED stream ID in
	// jobs.video_id. A job.video_id join would never match, falsely flagging a
	// live Twitch job's history as orphaned. The job.id join must catch it.
	if err := db.AddToHistory("tw_stream999"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddJob(&Job{
		ID: "tw_stream999", VideoID: "stream999", Platform: "twitch",
		Status: StatusLive, CreatedAt: "2026-07-14T00:00:00Z", UpdatedAt: "2026-07-14T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	orphans, err := db.ListOrphanedHistory()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, o := range orphans {
		got[o.VideoID] = true
	}
	if !got["vidOrphan01"] || !got["vidOrphan03"] {
		t.Errorf("expected both orphans listed, got %+v", orphans)
	}
	if got["vidHasJob02"] {
		t.Error("a history row with a matching job must NOT be listed as orphaned")
	}
	if got["tw_stream999"] {
		t.Error("a Twitch history row (keyed by tw_-prefixed job ID) with a live job must NOT be listed as orphaned")
	}

	// Delete one orphan.
	n, err := db.DeleteHistoryEntries([]string{"vidOrphan01"})
	if err != nil || n != 1 {
		t.Fatalf("delete: n=%d err=%v", n, err)
	}
	if p, _ := db.HasProcessed("vidOrphan01"); p {
		t.Error("deleted orphan should no longer be in history")
	}
	if p, _ := db.HasProcessed("vidOrphan03"); !p {
		t.Error("untouched orphan should remain")
	}
	// Empty delete is a no-op.
	if n, err := db.DeleteHistoryEntries(nil); err != nil || n != 0 {
		t.Errorf("empty delete: n=%d err=%v", n, err)
	}
}
