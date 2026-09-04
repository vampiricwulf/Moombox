package routes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/database"
)

type watchFixture struct {
	router chi.Router
	db     *database.Database
}

func newWatchFixture(t *testing.T) *watchFixture {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	r := chi.NewRouter()
	WatchRoutes(r, db)
	return &watchFixture{router: r, db: db}
}

func (f *watchFixture) addJob(t *testing.T, id string) {
	t.Helper()
	if _, err := f.db.AddJob(&database.Job{
		ID:      id,
		VideoID: id,
		URL:     "https://example.com/" + id,
		Status:  database.StatusFinished,
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
}

// --- GET /api/jobs/{id}/watch-state ---

func TestWatchState404OnUnknown(t *testing.T) {
	f := newWatchFixture(t)
	req := httptest.NewRequest("GET", "/api/jobs/no-such/watch-state", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown job: want 404, got %d", rec.Code)
	}
}

func TestWatchStateReturnsAllFields(t *testing.T) {
	// Lightweight read of the three mutable watch fields. Cache-Control
	// is no-cache so the player UI always sees fresh state when polling.
	f := newWatchFixture(t)
	f.addJob(t, "yt_ws")

	req := httptest.NewRequest("GET", "/api/jobs/yt_ws/watch-state", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("watch-state: want 200, got %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache, must-revalidate" {
		t.Errorf("Cache-Control: want no-cache, got %q", cc)
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	for _, key := range []string{"watched", "resumePosition", "chatOffset"} {
		if _, ok := resp[key]; !ok {
			t.Errorf("response missing field %q", key)
		}
	}
}

// --- PUT /api/jobs/{id}/resume-position ---

func TestResumePositionPutSavesValue(t *testing.T) {
	f := newWatchFixture(t)
	f.addJob(t, "yt_rp")

	body, _ := json.Marshal(map[string]float64{"position": 42.5})
	req := httptest.NewRequest("PUT", "/api/jobs/yt_rp/resume-position", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("PUT resume-position: want 204, got %d", rec.Code)
	}
	got, _ := f.db.GetJob("yt_rp")
	if got.ResumePosition == nil {
		t.Fatal("ResumePosition not saved")
	}
	if *got.ResumePosition != 42.5 {
		t.Errorf("position: want 42.5, got %v", *got.ResumePosition)
	}
}

func TestResumePositionPutRejectsNegative(t *testing.T) {
	f := newWatchFixture(t)
	f.addJob(t, "yt_rp_neg")

	body, _ := json.Marshal(map[string]float64{"position": -1})
	req := httptest.NewRequest("PUT", "/api/jobs/yt_rp_neg/resume-position", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("negative position: want 400, got %d", rec.Code)
	}
}

func TestResumePositionPutRejectsInvalidJSON(t *testing.T) {
	f := newWatchFixture(t)
	req := httptest.NewRequest("PUT", "/api/jobs/yt_x/resume-position", bytes.NewReader([]byte("not-json")))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON: want 400, got %d", rec.Code)
	}
}

// --- POST /api/jobs/{id}/resume-position (sendBeacon fallback) ---

func TestResumePositionPostSavesValue(t *testing.T) {
	// sendBeacon only sends POST, so this endpoint mirrors PUT for the
	// "on tab close, save my progress" path. Behaviour matches PUT
	// except errors return bare-status (no JSON body) per the route.
	f := newWatchFixture(t)
	f.addJob(t, "yt_rpp")

	body, _ := json.Marshal(map[string]float64{"position": 100.25})
	req := httptest.NewRequest("POST", "/api/jobs/yt_rpp/resume-position", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST resume-position: want 204, got %d", rec.Code)
	}
	got, _ := f.db.GetJob("yt_rpp")
	if got.ResumePosition == nil || *got.ResumePosition != 100.25 {
		t.Errorf("position: want 100.25, got %v", got.ResumePosition)
	}
}

// --- POST /api/jobs/{id}/watched ---

func TestWatchedPostMarksAsWatched(t *testing.T) {
	f := newWatchFixture(t)
	f.addJob(t, "yt_w")
	// Seed a resume-position so we can verify it's cleared.
	f.db.UpdateResumePosition("yt_w", 50.0)

	req := httptest.NewRequest("POST", "/api/jobs/yt_w/watched", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST watched: want 200, got %d", rec.Code)
	}
	got, _ := f.db.GetJob("yt_w")
	if !got.Watched {
		t.Error("Watched: want true")
	}
	// Marking watched must clear the resume position so the player
	// starts fresh on the next play.
	if got.ResumePosition != nil {
		t.Errorf("ResumePosition: want nil after watched, got %v", *got.ResumePosition)
	}
}

func TestWatchedPost404OnUnknown(t *testing.T) {
	f := newWatchFixture(t)
	req := httptest.NewRequest("POST", "/api/jobs/no-such/watched", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown job: want 404, got %d", rec.Code)
	}
}

// --- DELETE /api/jobs/{id}/watched ---

func TestWatchedDeleteMarksAsUnwatched(t *testing.T) {
	f := newWatchFixture(t)
	f.addJob(t, "yt_unw")
	// Pre-mark as watched so the delete has an effect to verify.
	f.db.UpdateJobFields("yt_unw", map[string]any{"watched": 1})

	req := httptest.NewRequest("DELETE", "/api/jobs/yt_unw/watched", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE watched: want 200, got %d", rec.Code)
	}
	got, _ := f.db.GetJob("yt_unw")
	if got.Watched {
		t.Error("Watched: want false after DELETE")
	}
}

// --- POST /api/jobs/batch/watched ---

func TestBatchWatchedRequiresJobIDs(t *testing.T) {
	f := newWatchFixture(t)
	body, _ := json.Marshal(map[string][]string{"jobIds": {}})
	req := httptest.NewRequest("POST", "/api/jobs/batch/watched", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty jobIds: want 400, got %d", rec.Code)
	}
}

func TestBatchWatchedMarksMultipleJobs(t *testing.T) {
	f := newWatchFixture(t)
	for _, id := range []string{"yt_b1", "yt_b2", "yt_b3"} {
		f.addJob(t, id)
	}

	body, _ := json.Marshal(map[string][]string{"jobIds": {"yt_b1", "yt_b2", "yt_b3"}})
	req := httptest.NewRequest("POST", "/api/jobs/batch/watched", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("batch watched: want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	for _, id := range []string{"yt_b1", "yt_b2", "yt_b3"} {
		got, _ := f.db.GetJob(id)
		if !got.Watched {
			t.Errorf("%s: Watched should be true", id)
		}
	}
}

func TestBatchWatchedDeleteUnmarksMultipleJobs(t *testing.T) {
	f := newWatchFixture(t)
	for _, id := range []string{"yt_u1", "yt_u2"} {
		f.addJob(t, id)
		f.db.UpdateJobFields(id, map[string]any{"watched": 1})
	}

	body, _ := json.Marshal(map[string][]string{"jobIds": {"yt_u1", "yt_u2"}})
	req := httptest.NewRequest("DELETE", "/api/jobs/batch/watched", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("batch unwatched: want 200, got %d", rec.Code)
	}
	for _, id := range []string{"yt_u1", "yt_u2"} {
		got, _ := f.db.GetJob(id)
		if got.Watched {
			t.Errorf("%s: Watched should be false after batch DELETE", id)
		}
	}
}

// --- chat-offset PUT/DELETE ---

func TestChatOffsetPutSavesValue(t *testing.T) {
	f := newWatchFixture(t)
	f.addJob(t, "yt_co")

	body, _ := json.Marshal(map[string]float64{"chatOffset": 5.0})
	req := httptest.NewRequest("PUT", "/api/jobs/yt_co/chat-offset", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("PUT chat-offset: want 204, got %d", rec.Code)
	}
	got, _ := f.db.GetJob("yt_co")
	if got.ChatOffset != 5.0 {
		t.Errorf("ChatOffset: want 5.0, got %v", got.ChatOffset)
	}
}

func TestChatOffsetPutAcceptsNegative(t *testing.T) {
	// Negative offsets are valid: pre-stream chat (waiting-room
	// messages) timestamps are negative relative to stream start.
	// Audit chat.md C2 / DECISIONS #24.
	f := newWatchFixture(t)
	f.addJob(t, "yt_neg")

	body, _ := json.Marshal(map[string]float64{"chatOffset": -10.0})
	req := httptest.NewRequest("PUT", "/api/jobs/yt_neg/chat-offset", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("negative offset: want 204, got %d", rec.Code)
	}
}

func TestChatOffsetDeleteClearsOffset(t *testing.T) {
	f := newWatchFixture(t)
	f.addJob(t, "yt_clr")
	f.db.UpdateChatOffset("yt_clr", 15.0)

	req := httptest.NewRequest("DELETE", "/api/jobs/yt_clr/chat-offset", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE chat-offset: want 204, got %d", rec.Code)
	}
	got, _ := f.db.GetJob("yt_clr")
	if got.ChatOffset != 0 {
		t.Errorf("ChatOffset: want 0 after DELETE, got %v", got.ChatOffset)
	}
}

func TestResumePositionPut404OnUnknown(t *testing.T) {
	f := newWatchFixture(t)
	body, _ := json.Marshal(map[string]any{"position": 12.5})
	req := httptest.NewRequest("PUT", "/api/jobs/no-such/resume-position", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown job: want 404, got %d", rec.Code)
	}
}

func TestResumePositionPost404OnUnknown(t *testing.T) {
	// The sendBeacon fallback twin of TestResumePositionPut404OnUnknown —
	// same unknown-job behaviour required on the POST path a browser
	// tab-close beacon actually uses.
	f := newWatchFixture(t)
	body, _ := json.Marshal(map[string]float64{"position": 12.5})
	req := httptest.NewRequest("POST", "/api/jobs/no-such/resume-position", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown job: want 404, got %d", rec.Code)
	}
}

func TestChatOffsetPutAndDelete404OnUnknown(t *testing.T) {
	f := newWatchFixture(t)
	body, _ := json.Marshal(map[string]any{"chatOffset": -1.5})
	for _, tc := range []struct {
		method string
		body   []byte
	}{{"PUT", body}, {"DELETE", nil}} {
		req := httptest.NewRequest(tc.method, "/api/jobs/no-such/chat-offset", bytes.NewReader(tc.body))
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s unknown job: want 404, got %d", tc.method, rec.Code)
		}
	}
}
