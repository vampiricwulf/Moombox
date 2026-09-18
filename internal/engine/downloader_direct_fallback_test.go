package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestProbeFileSizeRetriesBeforeFallback pins ENGINE-6(a): one transient
// failure of the Range probe must not condemn a resumed VOD to the
// from-byte-0 streaming fallback.
//
// Mutant: calling probeFileSize once (the pre-arc shape) — the first 500 wins
// and the function returns 0, so runDirectDownload falls back and discards.
func TestProbeFileSizeRetriesBeforeFallback(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Range", "bytes 0-0/4096")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte{0})
	}))
	t.Cleanup(srv.Close)

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, IsDirectURL: true})
	d.delays = fastDelays()

	if got := d.probeFileSizeWithRetry(context.Background()); got != 4096 {
		t.Fatalf("probeFileSizeWithRetry = %d, want 4096 after one transient failure (calls=%d)", got, calls.Load())
	}
}

// TestStreamingFallbackKeepsResumeOffset pins ENGINE-6's other half: when the
// fallback runs on a partially downloaded file it resumes with a Range header
// and APPENDS, instead of truncating the staged bytes away.
//
// Mutant: restoring the unconditional reset (bytesWritten=0 + O_TRUNC before
// the GET) — no Range goes out, the staged bytes are truncated away and the
// server re-sends the whole file, so both the Range assertion and the
// served-bytes assertion below fail. (The CONTENT alone cannot catch it: a
// server that re-serves from byte 0 rebuilds the same file, which is exactly
// why the bytes the wire carried are asserted too.)
func TestStreamingFallbackKeepsResumeOffset(t *testing.T) {
	const head, tail = "HEADHEAD", "TAILTAIL"
	var gotRange atomic.Value
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		gotRange.Store(rng)
		if strings.HasPrefix(rng, "bytes=8-") {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 8-15/%d", len(head)+len(tail)))
			w.WriteHeader(http.StatusPartialContent)
			n, _ := w.Write([]byte(tail))
			served.Add(int64(n))
			return
		}
		w.WriteHeader(http.StatusOK)
		n, _ := w.Write([]byte(head + tail))
		served.Add(int64(n))
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path, []byte(head), 0o644); err != nil {
		t.Fatalf("seed staged bytes: %v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open staged file: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, OutputFile: path, IsDirectURL: true})
	d.outputFile = f
	d.bytesWritten.Store(int64(len(head)))
	// A discard would swap d.outputFile for a fresh handle; release whichever
	// one is current so TempDir can unlink the file on Windows.
	t.Cleanup(func() { d.outputFile.Close() })

	if err := d.runDirectDownloadFallback(context.Background()); err != nil {
		t.Fatalf("runDirectDownloadFallback = %v, want nil", err)
	}
	d.outputFile.Sync()
	got, _ := os.ReadFile(path)
	if string(got) != head+tail {
		t.Fatalf("file = %q, want %q (the fallback must append from the resume offset)", got, head+tail)
	}
	if r, _ := gotRange.Load().(string); r != "bytes=8-" {
		t.Fatalf("Range header = %q, want bytes=8-", r)
	}
	if n := served.Load(); n != int64(len(tail)) {
		t.Fatalf("server delivered %d bytes, want %d — the staged head was re-downloaded, not kept", n, len(tail))
	}
}

// TestStreamingFallbackDiscardsWhenRangeIgnored pins the one explicit discard
// left on this path: a server that answers 200 to a resume Range is sending
// the file from byte 0, so the staged bytes must go or the output splices.
//
// Mutant: appending the 200 body at the current offset — the file is
// head+head+tail, a doubled, corrupt archive.
func TestStreamingFallbackDiscardsWhenRangeIgnored(t *testing.T) {
	const head, whole = "HEADHEAD", "HEADHEADTAILTAIL"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // Range deliberately ignored
		w.Write([]byte(whole))
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "video.mp4")
	os.WriteFile(path, []byte(head), 0o644)
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	t.Cleanup(func() { f.Close() })

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, OutputFile: path, IsDirectURL: true})
	d.outputFile = f
	d.bytesWritten.Store(int64(len(head)))
	// The discard closes f and reopens OutputFile onto d.outputFile, so it is
	// the CURRENT handle that has to be released before TempDir is removed —
	// Windows refuses to unlink a file anyone still holds open.
	t.Cleanup(func() { d.outputFile.Close() })

	if err := d.runDirectDownloadFallback(context.Background()); err != nil {
		t.Fatalf("runDirectDownloadFallback = %v, want nil", err)
	}
	d.outputFile.Sync()
	got, _ := os.ReadFile(path)
	if string(got) != whole {
		t.Fatalf("file = %q, want %q (an ignored Range must discard, not splice)", got, whole)
	}
}

// TestStreamingFallbackFailsOnIdleStall pins the deadline the fallback now
// runs under. ENGINE-4 took the client-level five-minute Timeout away, and
// that Timeout was the ONLY bound this GET ever had — a CDN that answers with
// headers and then goes silent would otherwise hold a VOD download open
// forever.
//
// Mutant: dropping the withReadProgressDeadline/idleBody pair from
// runDirectDownloadFallback — the call never returns and this test fails on
// its own 2 s guard.
func TestStreamingFallbackFailsOnIdleStall(t *testing.T) {
	prev := SegmentTimeout
	t.Cleanup(func() { SegmentTimeout = prev })
	SegmentTimeout = 80 * time.Millisecond

	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })

	path := filepath.Join(t.TempDir(), "video.mp4")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, OutputFile: path, IsDirectURL: true})
	d.outputFile = f

	done := make(chan error, 1)
	go func() { done <- d.runDirectDownloadFallback(context.Background()) }()

	select {
	case err := <-done:
		if !errors.Is(err, errFetchIdle) {
			t.Fatalf("runDirectDownloadFallback = %v, want errFetchIdle", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runDirectDownloadFallback never returned — the streaming fallback has no deadline")
	}
}

// TestStreamingFallbackSurvivesSlowButProgressingTransfer pins the other half
// of that bound: it is an IDLE deadline, not a total one, so a multi-hour VOD
// streaming at a steady trickle must still finish. Ten 20 ms gaps against a
// 60 ms bound is more than three times the deadline in wall clock with no
// single gap reaching it.
//
// Mutant: giving the fallback a total context.WithTimeout(parent,
// SegmentTimeout) instead — the transfer dies at 60 ms, mid-file.
func TestStreamingFallbackSurvivesSlowButProgressingTransfer(t *testing.T) {
	prev := SegmentTimeout
	t.Cleanup(func() { SegmentTimeout = prev })
	SegmentTimeout = 60 * time.Millisecond

	srv := trickleServer(t, 10, 20*time.Millisecond, 1024)

	path := filepath.Join(t.TempDir(), "video.mp4")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, OutputFile: path, IsDirectURL: true})
	d.outputFile = f

	start := time.Now()
	if err := d.runDirectDownloadFallback(context.Background()); err != nil {
		t.Fatalf("runDirectDownloadFallback = %v, want nil for a continuously progressing transfer", err)
	}
	if got := d.bytesWritten.Load(); got != 10*1024 {
		t.Fatalf("bytesWritten = %d, want %d", got, 10*1024)
	}
	if elapsed := time.Since(start); elapsed < SegmentTimeout {
		t.Fatalf("transfer took %s — shorter than the idle bound, so it never exercised the deadline", elapsed)
	}
}

// TestProbeFileSizeDrainIsBounded pins ENGINE-14 (report #50): the 1-byte
// 206 drain is capped like every other drain in the file, so a server that
// answers a 1-byte range with megabytes cannot be pulled in full.
//
// Mutant: restoring the bare io.Copy(io.Discard, resp.Body) — served counts
// far past maxDrainBytes.
func TestProbeFileSizeDrainIsBounded(t *testing.T) {
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-0/4096")
		w.WriteHeader(http.StatusPartialContent)
		buf := make([]byte, 64<<10)
		for range 256 { // 16 MiB, 256x maxDrainBytes
			n, err := w.Write(buf)
			served.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, IsDirectURL: true})
	if got := d.probeFileSize(context.Background()); got != 4096 {
		t.Fatalf("probeFileSize = %d, want 4096", got)
	}
	// Give the server goroutine a moment to notice the closed body.
	time.Sleep(50 * time.Millisecond)
	if n := served.Load(); n > 32*maxDrainBytes {
		t.Fatalf("drained %d bytes, want at most %d — the 206 drain is unbounded", n, 32*maxDrainBytes)
	}
}

// TestDirectURLStagedBytesStillRestartFresh re-pins the shared no-truncate
// guard's IsDirectURL exclusion now that this task has closed the door the
// exclusion's comment points at. The exclusion stands on its own terms: a
// whole-file VOD partial is always re-fetchable from the same static URL, so
// restarting it costs bandwidth, not footage — unlike a live recording, whose
// staged segments the CDN has already evicted. Widening the guard would turn
// the ordinary "interrupted below the first 50 MB sidecar checkpoint" case
// into a hard job error that only a manual Retry clears.
//
// Mutant: dropping !d.opts.IsDirectURL from the guard in Start — this Start
// returns ErrStagedMediaPresent instead of downloading.
func TestDirectURLStagedBytesStillRestartFresh(t *testing.T) {
	const body = "WHOLEFILEWHOLEFILE"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(body)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte(body[:1]))
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "video.mp4")
	// Staged bytes with NO resume sidecar: the guard's trigger shape.
	if err := os.WriteFile(path, []byte("STALESTALESTALE"), 0o644); err != nil {
		t.Fatalf("seed staged bytes: %v", err)
	}

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, OutputFile: path, IsDirectURL: true})
	d.delays = fastDelays()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.Start(ctx); errors.Is(err, ErrStagedMediaPresent) {
		t.Fatalf("Start = %v, want the guard to keep standing down for IsDirectURL", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != body {
		t.Fatalf("file = %q, want %q (a stale VOD partial restarts from byte 0)", got, body)
	}
}
