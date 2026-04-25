package routes

import (
	"archive/zip"
	"bytes"
	"encoding/json"
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

type importFixture struct {
	router    chi.Router
	store     *config.Store
	db        *database.Database
	outputDir string
	cleanup   func() // returned by ImportRoutes; stops the per-route rate limiter
}

func newImportFixture(t *testing.T) *importFixture {
	t.Helper()
	dir := t.TempDir()

	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := config.Defaults()
	outputDir := filepath.Join(dir, "output")
	cfg.Paths.OutputDirectory = outputDir
	store := config.NewStore(cfg, filepath.Join(dir, "config.toml"))

	apiRL := web.NewRateLimiter(100, time.Minute)
	t.Cleanup(apiRL.Close)

	r := chi.NewRouter()
	cleanup := ImportRoutes(r, db, store, apiRL)
	t.Cleanup(cleanup)

	return &importFixture{router: r, store: store, db: db, outputDir: outputDir, cleanup: cleanup}
}

// makeImportZip builds an in-memory zip with the given file name → contents
// pairs. Returns the raw bytes ready to POST as application/octet-stream.
func makeImportZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip Create %s: %v", name, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("zip Write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	return buf.Bytes()
}

// importRequest constructs a POST /api/import request with the given body.
// RemoteAddr is loopback so the rate limiter's per-IP keying is stable
// across cases. Headers are exposed for tests that override title/channel.
func importRequest(body []byte) *http.Request {
	req := httptest.NewRequest("POST", "/api/import", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/octet-stream")
	return req
}

// --- content-type guard ---

func TestImportRejectsWrongContentType(t *testing.T) {
	f := newImportFixture(t)

	zipBytes := makeImportZip(t, map[string][]byte{"video [vid_001].mp4": []byte("fake-video-bytes")})
	req := importRequest(zipBytes)
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("text/plain content-type: want 400, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestImportAcceptsApplicationZipContentType(t *testing.T) {
	// Matches both `application/octet-stream` and `application/zip` —
	// frontend sets the latter when the user picks a .zip via the
	// file dialog.
	f := newImportFixture(t)

	zipBytes := makeImportZip(t, map[string][]byte{"video [vid_zip].mp4": []byte("fake")})
	req := importRequest(zipBytes)
	req.Header.Set("Content-Type", "application/zip")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("application/zip: want 201, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

// --- signature peek (audit Q-13) ---

func TestImportRejectsBadZipSignature(t *testing.T) {
	// The 4-byte peek is the cheap-DoS guard. Anything that doesn't
	// start with PK\x03\x04 fails before the 500MB temp file allocation.
	f := newImportFixture(t)

	req := importRequest([]byte("garbage that is not a zip"))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad signature: want 400, got %d", rec.Code)
	}
}

func TestImportRejectsTruncatedSignature(t *testing.T) {
	// Less than 4 bytes — io.ReadFull returns ErrUnexpectedEOF, n < 4,
	// and the route correctly rejects.
	f := newImportFixture(t)

	req := importRequest([]byte("PK"))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("truncated signature: want 400, got %d", rec.Code)
	}
}

// --- happy path ---

func TestImportCreatesJobFromVideoOnly(t *testing.T) {
	// Minimal viable import: a single .mp4 with a [videoID]-style
	// bracket suffix. The route extracts the video ID, builds a job
	// with status=Finished, and persists the file under outputDir/imports.
	f := newImportFixture(t)

	zipBytes := makeImportZip(t, map[string][]byte{
		"My Cool Video [abcd1234567].mp4": []byte("fake mp4 bytes"),
	})

	req := importRequest(zipBytes)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("import: want 201, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var job database.Job
	if err := json.NewDecoder(rec.Body).Decode(&job); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	if job.VideoID != "abcd1234567" {
		t.Errorf("video ID: want abcd1234567, got %q", job.VideoID)
	}
	if job.Status != database.StatusFinished {
		t.Errorf("status: want Finished, got %s", job.Status)
	}
	if job.ManuallyAdded != true {
		t.Errorf("ManuallyAdded: want true for imports, got %v", job.ManuallyAdded)
	}

	// Job persisted to DB
	persisted, err := f.db.GetJob(job.ID)
	if err != nil || persisted == nil {
		t.Fatalf("GetJob: %v %v", err, persisted)
	}

	// Video file extracted to outputDir/imports
	videoPath := filepath.Join(f.outputDir, job.Filename)
	if _, err := os.Stat(videoPath); err != nil {
		t.Errorf("video file missing at %s: %v", videoPath, err)
	}
}

func TestImportExtractsChatJsonAlongsideVideo(t *testing.T) {
	// .chat.json files are auto-detected and extracted to a sibling
	// path; the resulting job's ChatFilename points at them so the
	// player UI can show chat replay.
	f := newImportFixture(t)

	chatJSON, _ := json.Marshal(map[string]any{
		"videoId":     "chat_test_id",
		"videoTitle":  "Title From Chat",
		"channelName": "Channel From Chat",
		"messages":    []map[string]any{{"offsetMs": 1000, "message": "hi"}},
	})

	zipBytes := makeImportZip(t, map[string][]byte{
		"video [chat_test_id].mp4":       []byte("fake mp4"),
		"video [chat_test_id].chat.json": chatJSON,
	})
	req := importRequest(zipBytes)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("import: want 201, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var job database.Job
	json.NewDecoder(rec.Body).Decode(&job)
	if job.ChatFilename == "" {
		t.Fatal("ChatFilename should be set when .chat.json present in zip")
	}

	chatPath := filepath.Join(f.outputDir, job.ChatFilename)
	if _, err := os.Stat(chatPath); err != nil {
		t.Errorf("chat file missing at %s: %v", chatPath, err)
	}
}

func TestImportUsesChatMetadataAsFallback(t *testing.T) {
	// When the video filename has no [videoID] bracket, the route
	// falls back to chat metadata's videoId/title/channelName —
	// otherwise users would get "imp_abcd" generated IDs and lose
	// real video provenance.
	f := newImportFixture(t)

	chatJSON, _ := json.Marshal(map[string]any{
		"videoId":     "fallback_id",
		"videoTitle":  "Real Title",
		"channelName": "Real Channel",
		"messages":    []map[string]any{{"offsetMs": 0, "message": "x"}},
	})
	zipBytes := makeImportZip(t, map[string][]byte{
		"plain-video.mp4":   []byte("fake mp4"),
		"plain-video.chat.json": chatJSON,
	})

	req := importRequest(zipBytes)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("import: want 201, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var job database.Job
	json.NewDecoder(rec.Body).Decode(&job)
	if job.VideoID != "fallback_id" {
		t.Errorf("video ID fallback: want fallback_id, got %q", job.VideoID)
	}
	if job.Title != "Real Title" {
		t.Errorf("title fallback: want 'Real Title', got %q", job.Title)
	}
	if job.ChannelName != "Real Channel" {
		t.Errorf("channel fallback: want 'Real Channel', got %q", job.ChannelName)
	}
}

func TestImportTitleAndChannelHeadersOverrideMetadata(t *testing.T) {
	// X-Import-Title / X-Import-Channel let the user paste a clean
	// title in the upload UI; the route must prefer those over both
	// the filename and the chat-json fallback.
	f := newImportFixture(t)

	chatJSON, _ := json.Marshal(map[string]any{
		"videoId":     "header_test",
		"videoTitle":  "Old Title (chat)",
		"channelName": "Old Channel (chat)",
		"messages":    []map[string]any{{"offsetMs": 0, "message": "x"}},
	})
	zipBytes := makeImportZip(t, map[string][]byte{
		"src [header_test].mp4":     []byte("fake"),
		"src [header_test].chat.json": chatJSON,
	})

	req := importRequest(zipBytes)
	req.Header.Set("X-Import-Title", "User-Supplied Title")
	req.Header.Set("X-Import-Channel", "User-Supplied Channel")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("import: want 201, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var job database.Job
	json.NewDecoder(rec.Body).Decode(&job)
	if job.Title != "User-Supplied Title" {
		t.Errorf("title: header should win, got %q", job.Title)
	}
	if job.ChannelName != "User-Supplied Channel" {
		t.Errorf("channel: header should win, got %q", job.ChannelName)
	}
}

// --- error paths ---

func TestImportRejectsZipWithoutVideo(t *testing.T) {
	// Imports that contain only chat JSON (no .mp4/.mkv/.webm/.ts) get
	// rejected before any DB write — there's nothing to play back and
	// importing chat alone has no UI affordance.
	f := newImportFixture(t)

	chatJSON, _ := json.Marshal(map[string]any{
		"videoId":  "novid",
		"messages": []map[string]any{{"offsetMs": 0, "message": "x"}},
	})
	zipBytes := makeImportZip(t, map[string][]byte{
		"only.chat.json": chatJSON,
		"readme.txt":     []byte("not video"),
	})

	req := importRequest(zipBytes)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("no video file: want 400, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestImportRejectsDuplicateVideoID(t *testing.T) {
	// Importing the same [videoID] twice produces a 409 — JobExists
	// checks ALL jobs (not just active), matching the TS behaviour
	// and preventing accidental data overwrites. The bracket regex
	// requires exactly 11 chars (the YouTube video-ID length) so the
	// test ID must match that shape; shorter strings would skip the
	// bracket extraction and assign a random imp_* ID per import.
	f := newImportFixture(t)

	// First import succeeds
	zipBytes := makeImportZip(t, map[string][]byte{
		"first [dupVideo123].mp4": []byte("first"),
	})
	rec1 := httptest.NewRecorder()
	f.router.ServeHTTP(rec1, importRequest(zipBytes))
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first import: want 201, got %d (body: %s)", rec1.Code, rec1.Body.String())
	}

	// Second import with same video ID fails
	zipBytes2 := makeImportZip(t, map[string][]byte{
		"second [dupVideo123].mp4": []byte("second"),
	})
	rec2 := httptest.NewRecorder()
	f.router.ServeHTTP(rec2, importRequest(zipBytes2))
	if rec2.Code != http.StatusConflict {
		t.Errorf("dup import: want 409, got %d (body: %s)", rec2.Code, rec2.Body.String())
	}
}

func TestImportRejectsZipWithPathTraversal(t *testing.T) {
	// Zip files containing entries with ".." or absolute paths are
	// rejected to prevent extracted files from escaping outputDir.
	f := newImportFixture(t)

	zipBytes := makeImportZip(t, map[string][]byte{
		"../escape.mp4": []byte("malicious"),
	})

	req := importRequest(zipBytes)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("path traversal: want 400, got %d", rec.Code)
	}
}

func TestImportTruncatesOverlongHeaders(t *testing.T) {
	// X-Import-Title / X-Import-Channel cap at 500 chars to bound the
	// size of the resulting filename + DB row. Imports with a 600-char
	// title still succeed; the title is just clipped.
	f := newImportFixture(t)

	zipBytes := makeImportZip(t, map[string][]byte{
		"src [trunc_test].mp4": []byte("fake"),
	})
	longTitle := bytes.Repeat([]byte("A"), 600)

	req := importRequest(zipBytes)
	req.Header.Set("X-Import-Title", string(longTitle))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("long title import: want 201, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var job database.Job
	json.NewDecoder(rec.Body).Decode(&job)
	if len(job.Title) > 500 {
		t.Errorf("title length: want <= 500 (cap before sanitize/extension), got %d", len(job.Title))
	}
}
