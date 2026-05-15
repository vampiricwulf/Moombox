package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// fakeTwitchFetcher and fakeYouTubeFetcher satisfy the JobRoutes
// metadata interfaces without dragging in the real Twitch/YouTube
// services. Tests configure their fields per-case.
type fakeTwitchFetcher struct {
	streamMeta *TwitchJobMetadata
	streamErr  error
	vodMeta    *TwitchJobMetadata
	vodErr     error
}

func (f *fakeTwitchFetcher) FetchStreamMetadata(ctx context.Context, login string) (*TwitchJobMetadata, error) {
	return f.streamMeta, f.streamErr
}
func (f *fakeTwitchFetcher) FetchVodMetadata(ctx context.Context, vodID string) (*TwitchJobMetadata, error) {
	return f.vodMeta, f.vodErr
}

type fakeYouTubeFetcher struct {
	meta *YouTubeJobMetadata
	err  error
}

func (f *fakeYouTubeFetcher) FetchMetadata(ctx context.Context, videoID string) (*YouTubeJobMetadata, error) {
	return f.meta, f.err
}

type jobsFixture struct {
	router     chi.Router
	db         *database.Database
	store      *config.Store
	outputDir  string
	stagingDir string
	tw         *fakeTwitchFetcher
	yt         *fakeYouTubeFetcher
}

// newJobsFixture wires JobRoutes against real DB + temp filesystem +
// permissive rate limiter + fake metadata fetchers + nil worker (most
// route paths gate worker access with `if w != nil`, so nil is fine
// for tests that don't exercise actual download orchestration).
func newJobsFixture(t *testing.T) *jobsFixture {
	t.Helper()
	dir := t.TempDir()

	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := config.Defaults()
	outputDir := filepath.Join(dir, "output")
	stagingDir := filepath.Join(dir, "staging")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	cfg.Paths.OutputDirectory = outputDir
	cfg.Paths.StagingDirectory = stagingDir
	store := config.NewStore(cfg, filepath.Join(dir, "config.toml"))

	apiRL := web.NewRateLimiter(1000, time.Minute)
	t.Cleanup(apiRL.Close)

	tw := &fakeTwitchFetcher{}
	yt := &fakeYouTubeFetcher{}

	r := chi.NewRouter()
	JobRoutes(r, db, store, nil, apiRL, tw, yt, nil)

	return &jobsFixture{
		router:     r,
		db:         db,
		store:      store,
		outputDir:  outputDir,
		stagingDir: stagingDir,
		tw:         tw,
		yt:         yt,
	}
}

// addJob seeds a job with sensible defaults; per-test overrides are
// applied via the optional mutate callback.
func (f *jobsFixture) addJob(t *testing.T, id string, mutate func(*database.Job)) *database.Job {
	t.Helper()
	job := &database.Job{
		ID:        id,
		VideoID:   id,
		URL:       "https://example.com/" + id,
		Title:     id + " title",
		Platform:  "youtube",
		Status:    database.StatusFinished,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if mutate != nil {
		mutate(job)
	}
	if _, err := f.db.AddJob(job); err != nil {
		t.Fatalf("AddJob %s: %v", id, err)
	}
	return job
}

func doRequest(t *testing.T, router chi.Router, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rdr = bytes.NewReader(buf)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// --- GET /api/jobs (active list) ---

func TestJobsListReturnsAllNonFinished(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "active1", func(j *database.Job) { j.Status = database.StatusDownloading })
	f.addJob(t, "active2", func(j *database.Job) { j.Status = database.StatusUpcoming })

	rec := doRequest(t, f.router, "GET", "/api/jobs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", rec.Code)
	}

	var jobs []database.Job
	if err := json.NewDecoder(rec.Body).Decode(&jobs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(jobs) != 2 {
		t.Errorf("non-finished jobs: want 2, got %d", len(jobs))
	}
}

func TestJobsListIncludesJustFinished(t *testing.T) {
	// AddJob always stamps UpdatedAt=now (DB layer invariant), so seeding
	// "old" finished jobs requires raw SQL access we don't expose here.
	// Test the realistic case: a freshly-finished job stays in the
	// active list under the default 30-day hide threshold.
	f := newJobsFixture(t)
	f.addJob(t, "fresh_finished", func(j *database.Job) {
		j.Status = database.StatusFinished
	})

	rec := doRequest(t, f.router, "GET", "/api/jobs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", rec.Code)
	}
	var jobs []database.Job
	json.NewDecoder(rec.Body).Decode(&jobs)
	if len(jobs) != 1 {
		t.Fatalf("active list: want 1 (fresh-finished within 30d), got %d", len(jobs))
	}
}

func TestJobsListNegativeHideAgeKeepsAll(t *testing.T) {
	// HideFinishedAgeDays < 0 is the documented "never archive" knob;
	// every finished job, no matter how old, stays in the active list.
	// Validate's normalize will floor this to >=0 in production, but
	// the route-level filter still respects the value as-passed.
	f := newJobsFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: -1}
	}); err != nil {
		// Validate will reject negative values; bypass via direct mu.Lock
		// + Save-skip instead. (Acceptable: this test is exercising a
		// route-level branch, not real persistence semantics.)
		mu := f.store.RWMutex()
		mu.Lock()
		f.store.Config().Monitors.HideFinishedAgeDays = config.FlexDuration{Value: -1}
		mu.Unlock()
	}
	f.addJob(t, "any_finished", func(j *database.Job) { j.Status = database.StatusFinished })

	rec := doRequest(t, f.router, "GET", "/api/jobs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", rec.Code)
	}
	var jobs []database.Job
	json.NewDecoder(rec.Body).Decode(&jobs)
	if len(jobs) != 1 {
		t.Errorf("negative hideAge: want 1 finished in active list, got %d", len(jobs))
	}
}

func TestJobsListPagination(t *testing.T) {
	// offset/limit are reflected in the {data, pagination} envelope.
	// Without pagination params the response is a flat array, so this
	// test specifically requests pagination to exercise that branch.
	f := newJobsFixture(t)
	for i := range 5 {
		f.addJob(t, fmt.Sprintf("job%d", i), func(j *database.Job) { j.Status = database.StatusDownloading })
	}

	rec := doRequest(t, f.router, "GET", "/api/jobs?offset=1&limit=2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("paginated list: want 200, got %d", rec.Code)
	}
	var resp struct {
		Data       []database.Job `json:"data"`
		Pagination struct {
			Total   int  `json:"total"`
			Offset  int  `json:"offset"`
			Limit   int  `json:"limit"`
			HasMore bool `json:"hasMore"`
		} `json:"pagination"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Pagination.Total != 5 {
		t.Errorf("total: want 5, got %d", resp.Pagination.Total)
	}
	if len(resp.Data) != 2 {
		t.Errorf("data length: want 2, got %d", len(resp.Data))
	}
	if !resp.Pagination.HasMore {
		t.Error("hasMore: want true (offset 1 + limit 2 of 5 → 3 remaining)")
	}
}

func TestJobsListInvalidOffset(t *testing.T) {
	f := newJobsFixture(t)
	rec := doRequest(t, f.router, "GET", "/api/jobs?offset=-1", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("negative offset: want 400, got %d", rec.Code)
	}
}

func TestJobsListInvalidLimit(t *testing.T) {
	f := newJobsFixture(t)
	rec := doRequest(t, f.router, "GET", "/api/jobs?limit=2000", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit > 1000: want 400, got %d", rec.Code)
	}
}

// --- GET /api/jobs/archived ---

func TestJobsArchivedEmptyWhenNothingOldEnough(t *testing.T) {
	// Mirror of TestJobsListIncludesJustFinished: with the default
	// 30-day threshold and only just-created jobs in the DB, the
	// archived endpoint returns []. Locks down the empty-array
	// envelope so frontend code that .map()s on the response works.
	f := newJobsFixture(t)
	f.addJob(t, "fresh1", func(j *database.Job) { j.Status = database.StatusFinished })
	f.addJob(t, "fresh2", func(j *database.Job) { j.Status = database.StatusFinished })

	rec := doRequest(t, f.router, "GET", "/api/jobs/archived", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("archived: want 200, got %d", rec.Code)
	}
	var jobs []database.Job
	json.NewDecoder(rec.Body).Decode(&jobs)
	if len(jobs) != 0 {
		t.Errorf("archived: want 0 (no aged jobs), got %d", len(jobs))
	}
}

func TestJobsArchivedAllFinishedWhenHideAgeZero(t *testing.T) {
	// HideFinishedAgeDays = 0 archives every finished job
	// immediately (the cutoff is "now" so anything finished trips it).
	f := newJobsFixture(t)
	if err := f.store.Update(func(c *config.MoomboxConfig) {
		c.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: 0}
	}); err != nil {
		t.Fatalf("set HideFinishedAgeDays=0: %v", err)
	}
	// Sleep so the just-finished job has a non-zero age past the
	// "0 days" cutoff. Without this the equality `age > 0` may not
	// fire because age is essentially 0.
	time.Sleep(15 * time.Millisecond)

	f.addJob(t, "x", func(j *database.Job) { j.Status = database.StatusFinished })
	// Add a small delay then re-sleep so the just-added job ages slightly
	// past 0. The route uses `age > hideAge`; with hideAge=0, any
	// non-zero positive duration trips the filter.
	time.Sleep(15 * time.Millisecond)

	rec := doRequest(t, f.router, "GET", "/api/jobs/archived", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("archived: want 200, got %d", rec.Code)
	}
	var jobs []database.Job
	json.NewDecoder(rec.Body).Decode(&jobs)
	if len(jobs) != 1 {
		t.Errorf("archived with hideAge=0: want 1, got %d", len(jobs))
	}
}

// --- GET /api/jobs/{id} ---

func TestJobGetByIDReturnsJob(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "yt_get1", nil)

	rec := doRequest(t, f.router, "GET", "/api/jobs/yt_get1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get by id: want 200, got %d", rec.Code)
	}

	// Finished jobs get long-cache headers
	if cc := rec.Header().Get("Cache-Control"); cc == "" || cc == "no-cache, must-revalidate" {
		t.Errorf("Cache-Control for finished: want immutable, got %q", cc)
	}
}

func TestJobGetByIDNonFinishedNoCache(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "yt_active", func(j *database.Job) { j.Status = database.StatusDownloading })

	rec := doRequest(t, f.router, "GET", "/api/jobs/yt_active", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get by id: want 200, got %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache, must-revalidate" {
		t.Errorf("Cache-Control for non-finished: want no-cache, got %q", cc)
	}
}

func TestJobGetByIDUnknown(t *testing.T) {
	f := newJobsFixture(t)
	rec := doRequest(t, f.router, "GET", "/api/jobs/no-such-job", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown job: want 404, got %d", rec.Code)
	}
}

// --- GET /api/jobs/{id}/video ---

func TestJobVideo404WhenJobMissing(t *testing.T) {
	f := newJobsFixture(t)
	rec := doRequest(t, f.router, "GET", "/api/jobs/no-such/video", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown job: want 404, got %d", rec.Code)
	}
}

func TestJobVideo404WhenNoOutputFile(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "yt_nofile", func(j *database.Job) {
		j.Filename = ""
		j.OutputFile = ""
	})

	rec := doRequest(t, f.router, "GET", "/api/jobs/yt_nofile/video", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("no output file: want 404, got %d", rec.Code)
	}
}

func TestJobVideo404WhenFileMissing(t *testing.T) {
	// Job has Filename set but the file doesn't exist on disk —
	// route returns 404 rather than 200 with a body that hangs forever.
	f := newJobsFixture(t)
	f.addJob(t, "yt_ghost", func(j *database.Job) {
		j.Filename = "ghost-video.mp4"
	})

	rec := doRequest(t, f.router, "GET", "/api/jobs/yt_ghost/video", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("ghost file: want 404, got %d", rec.Code)
	}
}

func TestJobVideoServesExistingFile(t *testing.T) {
	f := newJobsFixture(t)
	videoPath := filepath.Join(f.outputDir, "real.mp4")
	if err := os.WriteFile(videoPath, []byte("fake mp4 bytes"), 0o644); err != nil {
		t.Fatalf("write video: %v", err)
	}
	f.addJob(t, "yt_real", func(j *database.Job) {
		j.Filename = "real.mp4"
	})

	rec := doRequest(t, f.router, "GET", "/api/jobs/yt_real/video", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("serve video: want 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "video/mp4" {
		t.Errorf("Content-Type: want video/mp4, got %q", got)
	}
	if rec.Body.String() != "fake mp4 bytes" {
		t.Errorf("body: want %q, got %q", "fake mp4 bytes", rec.Body.String())
	}
}

// --- GET /api/jobs/{id}/segments ---

func TestJobSegments404WhenJobMissing(t *testing.T) {
	f := newJobsFixture(t)
	rec := doRequest(t, f.router, "GET", "/api/jobs/no-such/segments", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown job: want 404, got %d", rec.Code)
	}
}

func TestJobSegments404WhenNoSegments(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "yt_noseg", nil)

	rec := doRequest(t, f.router, "GET", "/api/jobs/yt_noseg/segments", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("no segments: want 404, got %d", rec.Code)
	}
}

// --- GET /api/jobs/{id}/chat ---

func TestJobChat404WhenJobMissing(t *testing.T) {
	f := newJobsFixture(t)
	rec := doRequest(t, f.router, "GET", "/api/jobs/no-such/chat", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown job: want 404, got %d", rec.Code)
	}
}

func TestJobChat404WhenNoChatFilename(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "yt_nochat", nil)

	rec := doRequest(t, f.router, "GET", "/api/jobs/yt_nochat/chat", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("no ChatFilename: want 404, got %d", rec.Code)
	}
}

func TestJobChatServesValidJSON(t *testing.T) {
	f := newJobsFixture(t)
	chatPath := filepath.Join(f.outputDir, "chat.json")
	chatBody := []byte(`{"messages":[{"text":"hi"}]}`)
	if err := os.WriteFile(chatPath, chatBody, 0o644); err != nil {
		t.Fatalf("write chat: %v", err)
	}
	f.addJob(t, "yt_chat", func(j *database.Job) {
		j.ChatFilename = "chat.json"
	})

	rec := doRequest(t, f.router, "GET", "/api/jobs/yt_chat/chat", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("serve chat: want 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type: want application/json, got %q", got)
	}
	if rec.Body.String() != string(chatBody) {
		t.Errorf("body: want %q, got %q", chatBody, rec.Body.String())
	}
}

func TestJobChatRejectsInvalidJSON(t *testing.T) {
	// Corrupt chat files (interrupted writes, partial flushes) get a
	// 422 so the player UI can show a useful "chat unavailable" rather
	// than rendering broken JSON or returning a 500.
	f := newJobsFixture(t)
	chatPath := filepath.Join(f.outputDir, "broken.json")
	if err := os.WriteFile(chatPath, []byte("not json{"), 0o644); err != nil {
		t.Fatalf("write chat: %v", err)
	}
	f.addJob(t, "yt_brokenchat", func(j *database.Job) {
		j.ChatFilename = "broken.json"
	})

	rec := doRequest(t, f.router, "GET", "/api/jobs/yt_brokenchat/chat", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid JSON: want 422, got %d", rec.Code)
	}
}

// --- GET /api/jobs/{id}/trims ---

func TestJobTrimsReturnsEmptyArray(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "yt_notrims", nil)

	rec := doRequest(t, f.router, "GET", "/api/jobs/yt_notrims/trims", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("trims: want 200, got %d", rec.Code)
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	trims, ok := resp["trims"].([]any)
	if !ok {
		t.Fatalf("trims: want array, got %T", resp["trims"])
	}
	if len(trims) != 0 {
		t.Errorf("trims: want empty, got %d", len(trims))
	}
}

func TestJobTrims404WhenJobMissing(t *testing.T) {
	f := newJobsFixture(t)
	rec := doRequest(t, f.router, "GET", "/api/jobs/no-such/trims", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown job: want 404, got %d", rec.Code)
	}
}

// --- GET /api/jobs/{id}/logs ---

func TestJobLogs404WhenJobMissing(t *testing.T) {
	f := newJobsFixture(t)
	rec := doRequest(t, f.router, "GET", "/api/jobs/no-such/logs", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown job: want 404, got %d", rec.Code)
	}
}

func TestJobLogsReturnsEmptyArray(t *testing.T) {
	// New jobs have no logs yet — return [] not null so frontend
	// .map() doesn't choke.
	f := newJobsFixture(t)
	f.addJob(t, "yt_nologs", nil)

	rec := doRequest(t, f.router, "GET", "/api/jobs/yt_nologs/logs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("logs: want 200, got %d", rec.Code)
	}
	if rec.Body.String() != "[]\n" {
		t.Errorf("logs body: want %q, got %q", "[]\n", rec.Body.String())
	}
}

// --- POST /api/jobs/{id}/cancel ---

func TestJobCancelInvalidState(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "yt_finished", func(j *database.Job) { j.Status = database.StatusFinished })

	rec := doRequest(t, f.router, "POST", "/api/jobs/yt_finished/cancel", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("cancel finished: want 400, got %d", rec.Code)
	}
}

func TestJobCancelDownloadingState(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "yt_downloading", func(j *database.Job) { j.Status = database.StatusDownloading })

	rec := doRequest(t, f.router, "POST", "/api/jobs/yt_downloading/cancel", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel downloading: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	// Status is bumped to Cancelled in the DB
	got, _ := f.db.GetJob("yt_downloading")
	if got.Status != database.StatusCancelled {
		t.Errorf("post-cancel status: want Cancelled, got %s", got.Status)
	}
}

func TestJobCancel404(t *testing.T) {
	f := newJobsFixture(t)
	rec := doRequest(t, f.router, "POST", "/api/jobs/no-such/cancel", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cancel unknown: want 404, got %d", rec.Code)
	}
}

// --- POST /api/jobs/{id}/retry ---

func TestJobRetryAllowsErrorState(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "yt_err", func(j *database.Job) { j.Status = database.StatusError })

	rec := doRequest(t, f.router, "POST", "/api/jobs/yt_err/retry", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("retry error-state: want 200, got %d", rec.Code)
	}
}

func TestJobRetryRejectsActiveState(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "yt_active2", func(j *database.Job) { j.Status = database.StatusDownloading })

	rec := doRequest(t, f.router, "POST", "/api/jobs/yt_active2/retry", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("retry active: want 400, got %d", rec.Code)
	}
}

// --- POST /api/jobs/{id}/resume ---

func TestJobResumeRejectsTwitch(t *testing.T) {
	// Resume preserves staging files which only exist for YouTube
	// segmented downloads. Twitch HLS doesn't have that semantic,
	// so the route hard-rejects.
	f := newJobsFixture(t)
	f.addJob(t, "tw_stream", func(j *database.Job) {
		j.Platform = "twitch"
		j.Status = database.StatusError
	})

	rec := doRequest(t, f.router, "POST", "/api/jobs/tw_stream/resume", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("resume twitch: want 400, got %d", rec.Code)
	}
}

func TestJobResumeRejectsActiveState(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "yt_dl_resume", func(j *database.Job) { j.Status = database.StatusDownloading })

	rec := doRequest(t, f.router, "POST", "/api/jobs/yt_dl_resume/resume", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("resume active: want 400, got %d", rec.Code)
	}
}

func TestJobResume404(t *testing.T) {
	f := newJobsFixture(t)
	rec := doRequest(t, f.router, "POST", "/api/jobs/no-such/resume", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("resume unknown: want 404, got %d", rec.Code)
	}
}

// --- POST /api/jobs/{id}/reinitialize ---

func TestJobReinitializeAllowsCookiesState(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "yt_cookies", func(j *database.Job) { j.Status = database.StatusCookies })

	rec := doRequest(t, f.router, "POST", "/api/jobs/yt_cookies/reinitialize", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("reinit cookies-state: want 200, got %d", rec.Code)
	}
}

func TestJobReinitializeRejectsActive(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "yt_active_reinit", func(j *database.Job) { j.Status = database.StatusDownloading })

	rec := doRequest(t, f.router, "POST", "/api/jobs/yt_active_reinit/reinitialize", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("reinit active: want 400, got %d", rec.Code)
	}
}

// --- POST /api/jobs/{id}/mux ---

func TestJobMuxRejectsActiveState(t *testing.T) {
	// Mux is for re-muxing finished-but-broken artifacts; trying to
	// force-mux a still-downloading job is a state error.
	f := newJobsFixture(t)
	f.addJob(t, "yt_dl_mux", func(j *database.Job) { j.Status = database.StatusDownloading })

	rec := doRequest(t, f.router, "POST", "/api/jobs/yt_dl_mux/mux", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("mux active: want 400, got %d", rec.Code)
	}
}

func TestJobMux404(t *testing.T) {
	f := newJobsFixture(t)
	rec := doRequest(t, f.router, "POST", "/api/jobs/no-such/mux", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("mux unknown: want 404, got %d", rec.Code)
	}
}

// --- DELETE /api/jobs/{id} ---

func TestJobDeleteAllowsTerminalStates(t *testing.T) {
	// StatusCookies has the literal value "COOKIES?" which contains a `?`
	// making it URL-unsafe in path components. We test the cookies-state
	// delete via the explicit chi route param mechanism in a separate
	// test below; this loop covers the URL-safe terminal states.
	f := newJobsFixture(t)
	for _, status := range []database.JobStatus{
		database.StatusFinished,
		database.StatusError,
		database.StatusCancelled,
	} {
		id := fmt.Sprintf("yt_del_%s", status)
		f.addJob(t, id, func(j *database.Job) { j.Status = status })

		rec := doRequest(t, f.router, "DELETE", "/api/jobs/"+id, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("delete %s: want 200, got %d", status, rec.Code)
		}
		// Job is actually gone
		got, _ := f.db.GetJob(id)
		if got != nil {
			t.Errorf("delete %s: row still present", status)
		}
	}
}

func TestJobDeleteAllowsCookiesState(t *testing.T) {
	// Variant of the terminal-states loop for COOKIES? — the literal
	// status value contains `?` so we use a URL-safe job ID and rely
	// on the route's GetJob+status check, not the path.
	f := newJobsFixture(t)
	f.addJob(t, "yt_cookies_del", func(j *database.Job) { j.Status = database.StatusCookies })

	rec := doRequest(t, f.router, "DELETE", "/api/jobs/yt_cookies_del", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("delete cookies-state: want 200, got %d", rec.Code)
	}
	if got, _ := f.db.GetJob("yt_cookies_del"); got != nil {
		t.Error("delete cookies-state: row still present")
	}
}

func TestJobDeleteActiveStateAutoDeletes(t *testing.T) {
	// Active-state jobs are now auto-cancelled (or just deleted when no worker
	// is wired) rather than rejected with 400. Verify the row is removed and
	// 200 is returned.
	f := newJobsFixture(t)
	f.addJob(t, "yt_dl_del", func(j *database.Job) { j.Status = database.StatusDownloading })

	rec := doRequest(t, f.router, "DELETE", "/api/jobs/yt_dl_del", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("delete downloading: want 200, got %d", rec.Code)
	}
	if got, _ := f.db.GetJob("yt_dl_del"); got != nil {
		t.Error("delete downloading: row still present after delete")
	}
}

func TestJobDelete404(t *testing.T) {
	f := newJobsFixture(t)
	rec := doRequest(t, f.router, "DELETE", "/api/jobs/no-such", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete unknown: want 404, got %d", rec.Code)
	}
}

// --- POST /api/jobs (creation) — validation paths ---

func TestJobCreateRejectsEmptyVideoIDAndURL(t *testing.T) {
	f := newJobsFixture(t)
	rec := doRequest(t, f.router, "POST", "/api/jobs", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty body: want 400, got %d", rec.Code)
	}
}

func TestJobCreateRejectsInvalidJSON(t *testing.T) {
	f := newJobsFixture(t)
	req := httptest.NewRequest("POST", "/api/jobs", bytes.NewReader([]byte("garbage")))
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON: want 400, got %d", rec.Code)
	}
}

func TestJobCreateRejectsBothItagsSkipped(t *testing.T) {
	// SelectedVideoItag=-1 + SelectedAudioItag=-1 means "skip both";
	// that yields nothing to download. Surface as 400 before any
	// network work.
	f := newJobsFixture(t)
	rec := doRequest(t, f.router, "POST", "/api/jobs", map[string]any{
		"videoId":           "abcd1234567",
		"selectedVideoItag": -1,
		"selectedAudioItag": -1,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("skip both: want 400, got %d", rec.Code)
	}
}

func TestJobCreateRejectsEndBeforeStart(t *testing.T) {
	f := newJobsFixture(t)
	rec := doRequest(t, f.router, "POST", "/api/jobs", map[string]any{
		"videoId":   "abcd1234567",
		"startTime": 10.0,
		"endTime":   5.0,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("end < start: want 400, got %d", rec.Code)
	}
}

func TestJobCreateRejectsPathTraversal(t *testing.T) {
	f := newJobsFixture(t)
	rec := doRequest(t, f.router, "POST", "/api/jobs", map[string]any{
		"videoId":         "abcd1234567",
		"outputDirectory": "../escape",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("path traversal: want 400, got %d", rec.Code)
	}
}

func TestJobCreateExtractsVideoIDFromYouTubeURL(t *testing.T) {
	// extractVideoIDFromURL is exercised when videoId is empty but URL
	// matches the YouTube pattern. fakeYouTubeFetcher returns nil
	// metadata so the route falls back to constructing a job with
	// just the extracted ID.
	f := newJobsFixture(t)
	f.yt.meta = &YouTubeJobMetadata{
		Title:        "extracted",
		ChannelName:  "ch",
		ChannelID:    "UC",
		ThumbnailURL: "https://example.com/thumb.jpg",
	}

	rec := doRequest(t, f.router, "POST", "/api/jobs", map[string]any{
		"url": "https://www.youtube.com/watch?v=abcd1234567",
	})
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("yt URL → create: want 200/201, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// Job persisted with the extracted ID. YouTube job IDs are NOT
	// prefixed (unlike Twitch's tw_/tw_v_); the bare videoID is the
	// primary key.
	got, _ := f.db.GetJob("abcd1234567")
	if got == nil {
		t.Fatal("expected job 'abcd1234567' to be created")
	}
	if got.VideoID != "abcd1234567" {
		t.Errorf("VideoID: want abcd1234567, got %q", got.VideoID)
	}
}

func TestJobCreateRejectsDuplicate(t *testing.T) {
	// HasActiveJob check: creating the same videoID twice in a row
	// returns 409. Mirrors the import-duplicate test in the import
	// route file.
	f := newJobsFixture(t)
	f.yt.meta = &YouTubeJobMetadata{Title: "first"}

	rec1 := doRequest(t, f.router, "POST", "/api/jobs", map[string]any{
		"videoId": "dup1234abcd",
	})
	if rec1.Code != http.StatusOK && rec1.Code != http.StatusCreated {
		t.Fatalf("first create: want 200/201, got %d (body: %s)", rec1.Code, rec1.Body.String())
	}

	rec2 := doRequest(t, f.router, "POST", "/api/jobs", map[string]any{
		"videoId": "dup1234abcd",
	})
	if rec2.Code != http.StatusConflict {
		t.Errorf("duplicate create: want 409, got %d (body: %s)", rec2.Code, rec2.Body.String())
	}
}
