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
	"sync/atomic"
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

// TestDirectDownload_MidStream200DoesNotSplice pins the mid-download Range-
// ignored guard: a server that answers chunk 0 with a proper 206 but then
// returns 200 (the WHOLE file from byte 0) for a later, non-zero-offset chunk
// must NOT get that byte-0 payload written at the current offset — that would
// splice the file's leading bytes into the middle (doubled/corrupt). The guard
// resets and streams a single clean copy instead.
func TestDirectDownload_MidStream200DoesNotSplice(t *testing.T) {
	// 12 MB (> DownloadChunkSize of 5 MB) so at least chunk 1 starts past 0.
	body := []byte(strings.Repeat("ABCDEFGH", 12*1024*1024/8))
	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video.mp4")

	var nonZeroRangeHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "" {
			// Streaming fallback GET (no Range) — serve the whole file cleanly.
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			w.Write(body)
			return
		}
		var start, end int64
		fmt.Sscanf(rng, "bytes=%d-%d", &start, &end)
		// bytes=0-0 probe: 206 carrying the total via Content-Range.
		if start == 0 && end == 0 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(body)))
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(body[0:1])
			return
		}
		if end <= 0 || end >= int64(len(body)) {
			end = int64(len(body)) - 1
		}
		if start > 0 {
			// A non-zero-offset chunk: pretend the server ignores Range and
			// dumps the whole file from byte 0 with a 200 — the corruption
			// trigger. (Under the 50 MB read cap, so it's read in full.)
			atomic.AddInt32(&nonZeroRangeHits, 1)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			w.Write(body)
			return
		}
		// start == 0: honest 206 for chunk 0.
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[start : end+1])
	}))
	defer srv.Close()

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:     srv.URL + "/video.mp4",
		OutputFile:  outFile,
		IsDirectURL: true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if atomic.LoadInt32(&nonZeroRangeHits) == 0 {
		t.Fatal("test never exercised the mid-download 200 path (no non-zero-offset chunk requested)")
	}

	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	// A splice would make the file larger than the body (chunk-0 bytes + a full
	// second copy). It must be exactly one clean copy.
	if len(got) != len(body) {
		t.Fatalf("output size = %d, want %d (larger means byte-0 data was spliced at offset>0)", len(got), len(body))
	}
	if string(got) != string(body) {
		t.Error("output content mismatch after the mid-download 200 reset")
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
