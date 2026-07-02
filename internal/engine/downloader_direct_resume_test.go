package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// serveRangeFile serves body with Range support (206) and HEAD/0-0 probes.
func serveRangeFile(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			w.Write(body)
			return
		}
		// Parse "bytes=start-end".
		var start, end int64
		fmt.Sscanf(rng, "bytes=%d-%d", &start, &end)
		if end <= 0 || end >= int64(len(body)) {
			end = int64(len(body)) - 1
		}
		if start > end || start >= int64(len(body)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[start : end+1])
	}
}

// TestDirectResume_FallbackResetsAvoidsDoubledFile pins the corruption guard:
// a direct download that resumed (partial file on disk, O_APPEND) but then
// finds the server no longer supports Range must NOT stream a second full copy
// after the partial bytes — it resets to a clean full download.
func TestDirectResume_FallbackResetsAvoidsDoubledFile(t *testing.T) {
	body := []byte(strings.Repeat("ABCDEFGH", 4096)) // 32 KB, distinctive
	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video.mp4")

	// Simulate a resumed state: half the body already on disk + a matching
	// resume sidecar Start() will load and validate.
	half := len(body) / 2
	if err := os.WriteFile(outFile, body[:half], 0o644); err != nil {
		t.Fatal(err)
	}
	resumeFile := outFile + ".resume.json"
	store := utils.ResumeStore[ResumeState]{Path: resumeFile}
	// No StreamID and a non-YouTube URL → identity check trusts the match,
	// exercising the size/append path.
	if err := store.Save(ResumeState{BytesWritten: int64(half), Timestamp: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}

	// Server that DROPS Range support (always 200, whole body) — the exact
	// trigger for the fallback on a resumed run.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	defer srv.Close()

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:     srv.URL + "/video.mp4",
		OutputFile:  outFile,
		IsDirectURL: true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	// Must be exactly the body once — not partial+full (48 KB) doubled tail.
	if len(got) != len(body) {
		t.Fatalf("output size = %d, want %d (a doubled file means the fallback appended after the resumed partial)", len(got), len(body))
	}
	if string(got) != string(body) {
		t.Errorf("output content mismatch after fallback reset")
	}
}

// TestDirectResume_RangeContinuesFromOffset covers the happy path: a resumed
// direct download whose server still supports Range continues from the
// persisted offset and produces the correct whole file.
func TestDirectResume_RangeContinuesFromOffset(t *testing.T) {
	body := []byte(strings.Repeat("0123456789", 8192)) // 80 KB
	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video.mp4")

	third := len(body) / 3
	if err := os.WriteFile(outFile, body[:third], 0o644); err != nil {
		t.Fatal(err)
	}
	store := utils.ResumeStore[ResumeState]{Path: outFile + ".resume.json"}
	if err := store.Save(ResumeState{BytesWritten: int64(third), Timestamp: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(serveRangeFile(body))
	defer srv.Close()

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:     srv.URL + "/video.mp4",
		OutputFile:  outFile,
		IsDirectURL: true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("resumed range download produced wrong content (len got=%d want=%d)", len(got), len(body))
	}
	// Resume sidecar cleared on completion.
	if _, err := os.Stat(outFile + ".resume.json"); !os.IsNotExist(err) {
		t.Errorf("resume sidecar should be cleared after completion, stat err = %v", err)
	}
}
