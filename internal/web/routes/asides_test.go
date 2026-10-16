package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

func (f *jobsFixture) getJSON(t *testing.T, path string, into any) string {
	t.Helper()
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d; body = %s", path, rec.Code, rec.Body.String())
	}
	if into != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatalf("unmarshal %s: %v", path, err)
		}
	}
	return rec.Body.String()
}

// stageAside drops one set-aside recording into a job's staging dir inside
// the fixture's staging base, and returns the dir.
func stageAside(t *testing.T, f *jobsFixture, jobID, stamp string) string {
	t.Helper()
	dir := filepath.Join(f.stagingDir, jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "video.mp4"+engine.StagedRestartSuffix+stamp), make([]byte, 64), 0o644); err != nil {
		t.Fatalf("write aside: %v", err)
	}
	return dir
}

// TestJobPayloadCarriesTheAsideReport: the dashboard learns about set-aside
// recordings from the same enriched fetch that already tells it about staging
// and segments. Without them in the payload the details dialog has nothing to
// render and the Recover button can never appear.
//
// Mutants this kills:
//   - dropping the fields from enrichJob: `asides` is absent from the body.
//   - marshalling a nil slice for a job with nothing set aside: `asides` is
//     null, so the dashboard's `.length` read throws instead of rendering
//     nothing.
func TestJobPayloadCarriesTheAsideReport(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "j1", func(j *database.Job) { j.Status = database.StatusCancelled })
	f.addJob(t, "j2", func(j *database.Job) { j.Status = database.StatusCancelled })
	dir := stageAside(t, f, "j1", "1700000000")
	if err := os.WriteFile(filepath.Join(dir, "chat.json"), []byte(`{"messages":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var withAsides struct {
		Asides []struct {
			Size             int64  `json:"size"`
			Timestamp        string `json:"timestamp"`
			HasResumeSidecar bool   `json:"hasResumeSidecar"`
		} `json:"asides"`
		KeptChatSidecar bool `json:"keptChatSidecar"`
	}
	body := f.getJSON(t, "/api/jobs/j1", &withAsides)
	if len(withAsides.Asides) != 1 {
		t.Fatalf("GET /api/jobs/j1 reported %d asides, want 1; body = %s", len(withAsides.Asides), body)
	}
	if withAsides.Asides[0].Size != 64 || withAsides.Asides[0].Timestamp == "" {
		t.Errorf("aside = %+v, want its byte size and an RFC 3339 timestamp", withAsides.Asides[0])
	}
	if !withAsides.KeptChatSidecar {
		t.Error("keptChatSidecar = false with a chat.json in staging")
	}

	plain := f.getJSON(t, "/api/jobs/j2", nil)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(plain), &raw); err != nil {
		t.Fatalf("unmarshal j2: %v", err)
	}
	if string(raw["asides"]) != "[]" {
		t.Errorf("a job with nothing set aside reports asides = %s, want [] — null breaks the dashboard's .length read", raw["asides"])
	}
}

// TestRecoverAsidesRouteRefusals pins the three answers the route owes a
// caller before the worker is ever reached.
//
// Mutants this kills:
//   - dropping the job lookup: an unknown job answers 200 and the dashboard
//     shows "recovery started" for a row that is gone.
//   - dropping the IsActiveJobStatus gate: a Downloading job's staging is
//     handed to a second FFmpeg.
//   - dropping the ScanAsides precondition: an ordinary job answers 200 and
//     nothing ever happens.
func TestRecoverAsidesRouteRefusals(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "live", func(j *database.Job) { j.Status = database.StatusDownloading })
	f.addJob(t, "bare", func(j *database.Job) { j.Status = database.StatusCancelled })
	stageAside(t, f, "live", "1700000000")

	for _, tc := range []struct {
		name, path string
		want       int
	}{
		{"unknown job", "/api/jobs/nope/recover-asides", http.StatusNotFound},
		{"active job", "/api/jobs/live/recover-asides", http.StatusConflict},
		{"nothing set aside", "/api/jobs/bare/recover-asides", http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			f.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, nil))
			if rec.Code != tc.want {
				t.Errorf("POST %s = %d, want %d; body = %s", tc.path, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestRecoverAsidesRouteAcceptsARecoverableJob: a terminal job whose staging
// holds a set-aside recording is accepted. The fixture's worker is nil, so
// this proves the route's own gates pass — the worker's are pinned in
// internal/worker.
//
// Mutant: gating the route on HasSegmentFiles (i.e. widening /mux's rule onto
// this verb) — an aside-only staging dir is refused, which is the entire bug
// this arc exists to fix.
func TestRecoverAsidesRouteAcceptsARecoverableJob(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "j1", nil) // newJobsFixture's default status is Finished
	stageAside(t, f, "j1", "1700000000")

	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs/j1/recover-asides", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/jobs/j1/recover-asides = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["success"] != true {
		t.Errorf("body = %v, want {\"success\": true} — the same shape /mux answers with", got)
	}
}

// TestJobPayloadCarriesTheMuxGaps: a Finished job whose staging is kept
// because a mux could not carry a staged recording whole says so in GET
// /api/jobs/{id}, from the record beside the recording — the job's own log,
// which said so first, is in memory and gone after a restart, and the shield
// has no age rule. A job with no record answers [].
//
// Mutants this kills:
//   - dropping MuxGaps from enrichJob: `muxGaps` is absent from the body.
//   - ScanMuxGaps returning a nil slice: `muxGaps` is null for j2.
func TestJobPayloadCarriesTheMuxGaps(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "j1", func(j *database.Job) { j.Status = database.StatusFinished })
	f.addJob(t, "j2", func(j *database.Job) { j.Status = database.StatusFinished })
	dir := filepath.Join(f.stagingDir, "j1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(dir, "video_stream")
	if err := os.WriteFile(media, make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}
	// The record a repaired mux leaves (data-and-storage.md, "Damaged
	// fragments"): it holds while the recording is there at its size.
	rec := `{"size":64,"output":"a.mp4","dropped":["bytes 10-20 (10 bytes): video 0:01:40.0 to 0:01:42.0 (2.0s)"],"droppedBytes":10,"gapSeconds":2,"at":"2026-10-16T00:00:00Z"}`
	if err := os.WriteFile(media+".mux-gaps.json", []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}

	var got struct {
		MuxGaps []struct {
			Path   string `json:"path"`
			Reason string `json:"reason"`
		} `json:"muxGaps"`
	}
	body := f.getJSON(t, "/api/jobs/j1", &got)
	if len(got.MuxGaps) != 1 || got.MuxGaps[0].Path != media {
		t.Fatalf("muxGaps = %+v, want the one recording; body = %s", got.MuxGaps, body)
	}
	if r := got.MuxGaps[0].Reason; !strings.Contains(r, "damaged stretch") || !strings.Contains(r, "0:01:40.0 to 0:01:42.0") || !strings.Contains(r, "a.mp4") {
		t.Errorf("reason = %q, want the record's stretches and the file they were left out of", r)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(f.getJSON(t, "/api/jobs/j2", nil)), &raw); err != nil {
		t.Fatalf("unmarshal j2: %v", err)
	}
	if string(raw["muxGaps"]) != "[]" {
		t.Errorf("a job with no mux gap reports muxGaps = %s, want []", raw["muxGaps"])
	}
}
