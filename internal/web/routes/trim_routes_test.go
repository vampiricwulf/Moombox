package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/ffmpegtest"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// trimFixture wires TrimRoutes against a real *worker.TrimService — the
// validation paths run before any ffmpeg invocation, and DELETE just
// touches the DB through the service. Tests confine themselves to those
// non-ffmpeg paths.
type trimFixture struct {
	router  chi.Router
	db      *database.Database
	trimSvc *worker.TrimService
}

func newTrimFixture(t *testing.T) *trimFixture {
	t.Helper()
	dir := t.TempDir()

	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// ffmpeg path doesn't have to exist; tests don't reach the actual
	// trim execution. TrimService caches it for later use.
	trimSvc := worker.NewTrimService(db, "ffmpeg-not-used-in-tests", silentLogger{})

	r := chi.NewRouter()
	TrimRoutes(r, db, trimSvc, nil)
	return &trimFixture{router: r, db: db, trimSvc: trimSvc}
}

// seedJob inserts a test job so the trim-create handler's GetJob doesn't
// 404. Returns the job ID for handler URLs.
func (f *trimFixture) seedJob(t *testing.T, id string) string {
	t.Helper()
	if _, err := f.db.AddJob(&database.Job{
		ID:      id,
		VideoID: id,
		URL:     "https://example.com/" + id,
		Status:  database.StatusFinished,
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	return id
}

func trimRequest(t *testing.T, jobID string, body any) *http.Request {
	t.Helper()
	buf, _ := json.Marshal(body)
	return httptest.NewRequest("POST", "/api/jobs/"+jobID+"/trims", bytes.NewReader(buf))
}

// --- POST /api/jobs/:id/trims — happy-path validation ---

func TestTrimCreateUnknownJobReturns404(t *testing.T) {
	f := newTrimFixture(t)

	req := trimRequest(t, "no-such-job", map[string]float64{"startTime": 0, "endTime": 5})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown job: want 404, got %d", rec.Code)
	}
}

func TestTrimCreateInvalidJSON(t *testing.T) {
	f := newTrimFixture(t)
	jid := f.seedJob(t, "yt_trim_invalid")

	req := httptest.NewRequest("POST", "/api/jobs/"+jid+"/trims", bytes.NewReader([]byte("garbage")))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON: want 400, got %d", rec.Code)
	}
}

func TestTrimCreateMissingTimes(t *testing.T) {
	// Both startTime and endTime are required (*float64 nil-ness check).
	// Sending an empty object should fail the "startTime and endTime are
	// required" branch.
	f := newTrimFixture(t)
	jid := f.seedJob(t, "yt_trim_missing")

	req := trimRequest(t, jid, map[string]any{})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing times: want 400, got %d", rec.Code)
	}
}

func TestTrimCreateNaNStartTime(t *testing.T) {
	// JSON treats NaN/Inf as invalid by default, so the only way these
	// reach the route is via raw bytes; round-trip through encoding/json
	// won't produce a valid request. Test the validator directly via
	// math.NaN() — Go's json.Marshal would error on NaN. Bypass with a
	// manual JSON payload.
	f := newTrimFixture(t)
	jid := f.seedJob(t, "yt_trim_nan")

	// Build a payload with literal NaN string — json.Decoder produces
	// startTime=NaN when parsing "NaN" with json.Number? Actually no.
	// Go's encoding/json rejects NaN/Inf at decode time. So this test
	// exercises the decode-failure path which still returns 400 (just
	// via the json.Decoder error rather than the IsNaN guard).
	body := []byte(`{"startTime": NaN, "endTime": 5}`)
	req := httptest.NewRequest("POST", "/api/jobs/"+jid+"/trims", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("NaN startTime: want 400, got %d", rec.Code)
	}
}

func TestTrimCreateNegativeStartTime(t *testing.T) {
	f := newTrimFixture(t)
	jid := f.seedJob(t, "yt_trim_neg")

	req := trimRequest(t, jid, map[string]float64{"startTime": -1, "endTime": 5})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("negative start: want 400, got %d", rec.Code)
	}
}

func TestTrimCreateEndBeforeStart(t *testing.T) {
	// endTime must be strictly > startTime — a zero-length or reversed
	// trim is meaningless and the validator catches it before ffmpeg
	// gets a bad command line.
	f := newTrimFixture(t)
	jid := f.seedJob(t, "yt_trim_reversed")

	req := trimRequest(t, jid, map[string]float64{"startTime": 10, "endTime": 5})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("end < start: want 400, got %d", rec.Code)
	}
}

func TestTrimCreateEqualStartAndEnd(t *testing.T) {
	f := newTrimFixture(t)
	jid := f.seedJob(t, "yt_trim_equal")

	req := trimRequest(t, jid, map[string]float64{"startTime": 5, "endTime": 5})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("end == start: want 400, got %d", rec.Code)
	}
}

func TestTrimCreateDurationUnderOneSecond(t *testing.T) {
	// The "at least 1 second" rule prevents users from creating
	// effectively-empty clips that ffmpeg might treat oddly.
	f := newTrimFixture(t)
	jid := f.seedJob(t, "yt_trim_short")

	req := trimRequest(t, jid, map[string]float64{"startTime": 0, "endTime": 0.5})
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("duration < 1s: want 400, got %d", rec.Code)
	}
}

// TestTrimServiceAnswersCarryTheirReason: every TrimService failure used to be
// a 400 "Failed to create trim" / "failed to delete trim". The dashboard toasts
// the response's message, so an end time past the video, a duplicate range or
// an unfinished job all read the same, a missing trim was a 400 rather than a
// 404, and an ffmpeg or database fault was reported as the request's fault.
// Refusals the user can act on now carry their reason (400, or 409 for a clash
// with current state); a missing trim is a 404.
func TestTrimServiceAnswersCarryTheirReason(t *testing.T) {
	f := newTrimFixture(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(out, []byte("not really a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	length := 60
	if _, err := f.db.AddJob(&database.Job{
		ID: "fin", VideoID: "fin", URL: "u", Status: database.StatusFinished,
		OutputFile: out, LengthSeconds: &length,
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if _, err := f.db.AddJob(&database.Job{ID: "live", VideoID: "live", URL: "u", Status: database.StatusLive}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := f.db.AddTrim(&database.TrimRecord{ID: "t1", JobID: "fin", StartTime: 10, EndTime: 20, Filename: "x.mp4"}); err != nil {
		t.Fatalf("AddTrim: %v", err)
	}

	for _, tc := range []struct {
		name     string
		jobID    string
		start    float64
		end      float64
		wantCode int
		wantMsg  string
	}{
		{"unfinished job", "live", 0, 10, http.StatusBadRequest, "job must be finished to trim"},
		{"end past the video", "fin", 0, 120, http.StatusBadRequest, "exceeds video duration"},
		{"duplicate range", "fin", 10, 20, http.StatusConflict, "trim already exists"},
	} {
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, trimRequest(t, tc.jobID, map[string]float64{"startTime": tc.start, "endTime": tc.end}))
		if rec.Code != tc.wantCode {
			t.Errorf("%s: want %d, got %d (body: %s)", tc.name, tc.wantCode, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), tc.wantMsg) {
			t.Errorf("%s: body %s does not carry %q", tc.name, rec.Body.String(), tc.wantMsg)
		}
	}

	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/jobs/fin/trims/no-such", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete unknown trim: want 404, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	f.router.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/jobs/no-such-job/trims/t1", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete trim of unknown job: want 404, got %d", rec.Code)
	}
}

// TestTrimCreateOutlivesThePageThatAskedForIt: a dashboard trim used to run
// its whole encode under req.Context(), so a reload, a closed tab or a
// dropped connection killed FFmpeg mid-encode — and left the partial output
// in trim/ under the finished trim's name, with no row, no trim_error and no
// page left to toast. The route now starts the trim as the trim service's
// own task and answers 202 at once with its id; the trim finishes after the
// page is gone.
//
// Mutant: run the encode in the handler (`trimSvc.CreateTrim(req.Context(),
// …)`) — the answer waits for the gated encode, the page gives up, and the
// trim dies with its request.
func TestTrimCreateOutlivesThePageThatAskedForIt(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	out := filepath.Join(dir, "out", "Title [page12345].mp4")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("src"), 0o644); err != nil {
		t.Fatal(err)
	}
	length := 600
	if _, err := db.AddJob(&database.Job{ID: "page12345", VideoID: "page12345", URL: "u",
		Status: database.StatusFinished, OutputFile: out, Filename: "Title [page12345].mp4", LengthSeconds: &length}); err != nil {
		t.Fatal(err)
	}
	// A stand-in FFmpeg that opens its output, then waits for the gate (30 s
	// at most, so nothing it is left running outlives the test).
	ff, standIn := ffmpegtest.Gated(t)
	gate := filepath.Join(standIn, "gate")
	svc := worker.NewTrimService(db, ff, silentLogger{})
	t.Cleanup(svc.Stop)
	r := chi.NewRouter()
	TrimRoutes(r, db, svc, nil)
	srv := httptest.NewServer(r)
	defer srv.Close()

	// The page: it waits two seconds for its answer, then is gone.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]float64{"startTime": 60, "endTime": 300})
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/jobs/page12345/trims", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if werr := os.WriteFile(gate, nil, 0o644); werr != nil {
			t.Error(werr)
		}
		t.Fatalf("the trim request got no answer while the encode ran: %v", err)
	}
	var answer struct {
		Trim worker.TrimTask `json:"trim"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&answer)
	resp.Body.Close()
	cancel() // the tab is closed
	if resp.StatusCode != http.StatusAccepted || decodeErr != nil || answer.Trim.ID == "" {
		t.Fatalf("answer: %d %+v (decode: %v), want 202 with the trim's id", resp.StatusCode, answer, decodeErr)
	}

	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	trimFile := filepath.Join(dir, "out", "trim", "page12345 [60s-300s].mp4")
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, _ := db.GetTrimsForJob("page12345")
		if len(rows) == 1 {
			if rows[0].ID != answer.Trim.ID {
				t.Errorf("the stored trim is %q, the route answered %q", rows[0].ID, answer.Trim.ID)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the trim never finished once its page was gone")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if b, _ := os.ReadFile(trimFile); string(b) != "whole" {
		t.Errorf("%s holds %q, want the whole encode", filepath.Base(trimFile), b)
	}
}

// TestTrimDeleteStopsARunningTrim: DELETE on a trim still encoding answered
// 404 (it searched stored trims only), so a running trim had no stop but a
// restart of Moombox. It now stops it — the same TrimService.StopTrim the
// TUI's stop calls — and answers {stopped: true}; the partial file goes and
// nothing is stored. A stored trim is deleted as before (here: none, 404).
//
// Mutant: drop the handler's StopTrim — the running trim answers 404.
func TestTrimDeleteStopsARunningTrim(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	out := filepath.Join(dir, "out", "Title [stop12345].mp4")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("src"), 0o644); err != nil {
		t.Fatal(err)
	}
	length := 600
	if _, err := db.AddJob(&database.Job{ID: "stop12345", VideoID: "stop12345", URL: "u",
		Status: database.StatusFinished, OutputFile: out, Filename: "Title [stop12345].mp4", LengthSeconds: &length}); err != nil {
		t.Fatal(err)
	}
	// A stand-in FFmpeg that opens its output and waits for a gate that never
	// opens (30 s at most, so nothing it is left running outlives the test).
	ff, _ := ffmpegtest.Gated(t)
	svc := worker.NewTrimService(db, ff, silentLogger{})
	t.Cleanup(svc.Stop)
	r := chi.NewRouter()
	TrimRoutes(r, db, svc, nil)

	task, err := svc.StartTrim(mustGetJob(t, db, "stop12345"), 60, 300)
	if err != nil {
		t.Fatalf("StartTrim: %v", err)
	}
	trimDir := filepath.Join(dir, "out", "trim")
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	trimFiles := func() int {
		entries, _ := os.ReadDir(trimDir)
		return len(entries)
	}
	waitFor("the encode to open its output", func() bool { return trimFiles() > 0 })

	del := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/jobs/stop12345/trims/"+task.ID, nil))
		return rec
	}
	rec := del()
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"stopped":true`) {
		t.Fatalf("DELETE on the running trim: %d %s, want 200 with stopped true", rec.Code, rec.Body)
	}
	waitFor("the stopped trim to end", func() bool { return len(svc.RunningTrims()) == 0 })
	if n := trimFiles(); n != 0 {
		t.Errorf("the stopped trim left %d files in trim/", n)
	}
	if rows, _ := db.GetTrimsForJob("stop12345"); len(rows) != 0 {
		t.Errorf("the stopped trim stored %d rows", len(rows))
	}
	if rec := del(); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE once the trim has stopped: %d %s, want 404", rec.Code, rec.Body)
	}
}

func mustGetJob(t *testing.T, db *database.Database, id string) *database.Job {
	t.Helper()
	j, err := db.GetJob(id)
	if err != nil || j == nil {
		t.Fatalf("GetJob(%q): %v %v", id, j, err)
	}
	return j
}

// TestTrimStopNeverDeletesAStoredTrim: a stored trim carries the id its
// encode ran under, so a dashboard Stop that lands just after the trim
// finished named a stored trim, and the DELETE fell through to deleting the
// clip it had just made. With ?stop=1 (what the dashboard's Stop sends) a
// trim no longer running is 404 and the stored one is kept; without it the
// DELETE still deletes.
//
// Mutant: drop the handler's stop=1 case — the stored trim is deleted.
func TestTrimStopNeverDeletesAStoredTrim(t *testing.T) {
	f := newTrimFixture(t)
	jobID := f.seedJob(t, "stored12345")
	if err := f.db.AddTrim(&database.TrimRecord{ID: "trim_stored12345_1", JobID: jobID, StartTime: 1, EndTime: 9,
		Filename: "trim/x.mp4", CreatedAt: "2026-11-25T00:00:00Z", Duration: 8}); err != nil {
		t.Fatalf("AddTrim: %v", err)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/jobs/"+jobID+"/trims/trim_stored12345_1?stop=1", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no longer running") {
		t.Errorf("a stop for a trim that has finished: %d %s, want 404 \"the trim is no longer running\"", rec.Code, rec.Body)
	}
	if rows, _ := f.db.GetTrimsForJob(jobID); len(rows) != 1 {
		t.Fatalf("a stop deleted the stored trim: %d rows left", len(rows))
	}

	rec = httptest.NewRecorder()
	f.router.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/jobs/"+jobID+"/trims/trim_stored12345_1", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("a plain DELETE of the stored trim: %d %s, want 200", rec.Code, rec.Body)
	}
	if rows, _ := f.db.GetTrimsForJob(jobID); len(rows) != 0 {
		t.Errorf("the plain DELETE kept the stored trim: %d rows", len(rows))
	}
}
