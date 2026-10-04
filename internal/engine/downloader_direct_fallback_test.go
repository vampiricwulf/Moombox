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

	"github.com/vampiricwulf/Moombox/internal/utils"
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
		for range 4096 { // 256 MiB, 4096x maxDrainBytes
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
	//
	// The margin below is MEASURED, not chosen: what the origin gets to push
	// past the LimitReader is socket buffering, not the drain. Observed with
	// the fix in place:
	//   - Windows, two machines (five runs each, two distinct values):
	//     397,303–462,839 bytes served.
	//   - ubuntu-latest (2026-09-29, CI run 36605795801): 2,166,775 bytes —
	//     Linux loopback buffers ~2.07 MiB before the server's writes fail,
	//     which broke the previous 32*maxDrainBytes (2 MiB) cap.
	// The cap is therefore 1024*maxDrainBytes (64 MiB): ~31x the observed Linux
	// slop and ~6x the most the default Linux sysctls can buffer at all
	// (tcp_wmem[2] 4 MiB send + tcp_rmem[2] 6 MiB receive). The body is
	// 256 MiB so the mutant — a bare io.Copy(io.Discard, resp.Body) — drains
	// all of it, 4x above the cap (the cap is 1/4 of what the mutant drains);
	// under the fix the server loop stops at its first failed write after the
	// client closes, so the larger body costs the fixed path nothing. The
	// brief's original 4*maxDrainBytes (256 KiB) sat BELOW the Windows range
	// and 32*maxDrainBytes sat below the Linux one — both failed with the fix
	// in place: do not tighten this back.
	time.Sleep(50 * time.Millisecond)
	if n := served.Load(); n > 1024*maxDrainBytes {
		t.Fatalf("drained %d bytes, want at most %d — the 206 drain is unbounded", n, 1024*maxDrainBytes)
	}
}

// TestDirectURLStagedBytesStillRestartFresh re-pins the shared no-truncate
// guard's IsDirectURL exclusion now that this task has closed the door the
// exclusion's comment points at. The exclusion stands on its own terms: a
// whole-file VOD partial is always re-fetchable from the same static URL, so
// restarting it costs bandwidth, not footage — unlike a live recording, whose
// staged segments the CDN has already evicted. Widening the guard would turn
// the ordinary "interrupted below the first 50 MB sidecar checkpoint" case
// into a hard job error that only a manual Retry clears. That checkpoint is a
// real one as of Task 10: saveResume used to return early on every whole-file
// download (no currentSeq), so until then the case was "interrupted at any
// point whatsoever" — see TestDirectDownloadCheckpointsFreshRunAndResumes and
// TestDirectFallbackCheckpointsMidStream.
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

// TestParseContentRangeStart pins the header parser both Content-Range checks
// stand on, including the two shapes that must NOT read as a start: the
// unsatisfied-range form a 416 carries, and any non-bytes unit.
//
// Mutant: returning ok=true with start 0 for "bytes */16" — a 416's header
// then reads as "the body starts at 0" and takes the discard branch.
func TestParseContentRangeStart(t *testing.T) {
	for _, tc := range []struct {
		header    string
		wantStart int64
		wantOK    bool
	}{
		{"bytes 8-15/16", 8, true},
		{"bytes 0-0/4096", 0, true},
		{"bytes 8-15/*", 8, true},
		{"  bytes  8-15/16  ", 8, true},
		{"bytes */16", 0, false}, // the 416 shape: no start at all
		{"", 0, false},
		{"items 1-2/3", 0, false},
		{"bytes 8-15", 0, false}, // no total
		{"bytes abc-15/16", 0, false},
		{"bytes -8/16", 0, false}, // suffix form, not a first-byte-pos
	} {
		h := http.Header{}
		if tc.header != "" {
			h.Set("Content-Range", tc.header)
		}
		start, ok := parseContentRangeStart(h)
		if start != tc.wantStart || ok != tc.wantOK {
			t.Errorf("parseContentRangeStart(%q) = (%d, %v), want (%d, %v)",
				tc.header, start, ok, tc.wantStart, tc.wantOK)
		}
	}
}

// TestStreamingFallbackAdoptsAnHonest206FromZero pins the review's ZZ2 origin:
// a 206 that answers "bytes=8-" with "Content-Range: bytes 0-15/16" and the
// WHOLE file is sending from byte 0 and saying so, which is the 200 case
// wearing a different status code. It must take the same explicit discard, not
// append 16 bytes behind the staged 8.
//
// Mutant: dropping the Content-Range check from the 206 arm — the reviewer's
// 24-byte splice ("HEADHEADHEADHEADTAILTAIL") comes back, with err nil.
func TestStreamingFallbackAdoptsAnHonest206FromZero(t *testing.T) {
	const head, whole = "HEADHEAD", "HEADHEADTAILTAIL"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Honest about restarting from the top, despite the 206.
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(whole)-1, len(whole)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte(whole))
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
	t.Cleanup(func() { d.outputFile.Close() })

	if err := d.runDirectDownloadFallback(context.Background()); err != nil {
		t.Fatalf("runDirectDownloadFallback = %v, want nil (a 206 from byte 0 discards, it does not fail)", err)
	}
	d.outputFile.Sync()
	got, _ := os.ReadFile(path)
	if string(got) != whole {
		t.Fatalf("file = %q, want %q (a 206 that restarts at 0 must discard, not splice)", got, whole)
	}
}

// TestStreamingFallbackRejectsMisStartingRange pins the other half: a 206 that
// starts at neither the requested offset NOR zero is a protocol violation this
// path cannot absorb — it has no total size to cross-check against, so the
// only safe answer is to write nothing and fail. The staged bytes survive.
//
// Mutant: treating start == 0 as an error instead (folding this case and the
// honest-restart case together) — TestStreamingFallbackAdoptsAnHonest206FromZero
// fails, because the legitimate discard is lost.
func TestStreamingFallbackRejectsMisStartingRange(t *testing.T) {
	const head, whole = "HEADHEAD", "HEADHEADTAILTAIL"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Asked for byte 8; answers from byte 4.
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 4-%d/%d", len(whole)-1, len(whole)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte(whole[4:]))
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
	t.Cleanup(func() { d.outputFile.Close() })

	err = d.runDirectDownloadFallback(context.Background())
	if err == nil {
		t.Fatal("runDirectDownloadFallback = nil, want an error for a 206 that starts at byte 4")
	}
	if !strings.Contains(err.Error(), "Content-Range start 4") || !strings.Contains(err.Error(), "Range 8") {
		t.Errorf("error = %q, want it to name both the requested offset and the answered start", err)
	}
	d.outputFile.Sync()
	got, _ := os.ReadFile(path)
	if string(got) != head {
		t.Fatalf("file = %q, want the staged %q untouched — a refused 206 must not write", got, head)
	}
}

// TestFetchChunkRejectsMisStartingRange pins the same check on the chunked
// path (sweep-2 D-R1), which is the route almost every VOD actually takes.
// The damage there is quieter: the read is bounded to end-start+1, so the file
// keeps exactly the length it should and only its CONTENT is wrong.
//
// Mutant: dropping the check from fetchChunk — err is nil and the caller
// writes the origin's byte-0 data at offset 8.
func TestFetchChunkRejectsMisStartingRange(t *testing.T) {
	const whole = "HEADHEADTAILTAIL"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Whatever is asked for, answer from byte 0 with a matching length.
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-7/%d", len(whole)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte(whole[:8]))
	}))
	t.Cleanup(srv.Close)

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, IsDirectURL: true})
	data, status, err := d.fetchChunk(context.Background(), 8, 15)
	if err == nil {
		t.Fatalf("fetchChunk = (%q, %d, nil), want an error — those are bytes 0-7 answering a request for 8-15",
			data, status)
	}
	if !strings.Contains(err.Error(), "Content-Range start 0") {
		t.Errorf("error = %q, want it to name the answered start", err)
	}
}

// TestStreamingFallbackClearsResumeSidecarOnSuccess pins the review's ZZ3: a
// fallback that completes is a completed download, so the sidecar has to go.
// Both hand-offs return straight to Start, skipping the chunked path's own
// ClearResume — and a stale sidecar truncates the COMPLETE file back to its
// offset on the next run.
//
// Mutant: dropping d.ClearResume() from the fallback's success return — the
// sidecar is still on disk after Start returns nil.
func TestStreamingFallbackClearsResumeSidecarOnSuccess(t *testing.T) {
	const head, tail = "HEADHEAD", "TAILTAIL"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "bytes=0-0" {
			w.WriteHeader(http.StatusInternalServerError) // probe never succeeds
			return
		}
		if rng == "bytes=8-" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 8-15/%d", len(head)+len(tail)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte(tail))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(head + tail))
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "video.mp4")
	resumeFile := seedDirectResume(t, path, head)

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, OutputFile: path, IsDirectURL: true})
	d.delays = fastDelays()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}
	if got, _ := os.ReadFile(path); string(got) != head+tail {
		t.Fatalf("file = %q, want %q", got, head+tail)
	}
	if _, err := os.Stat(resumeFile); !os.IsNotExist(err) {
		t.Fatalf("resume sidecar still present after a completed fallback (stat err = %v)", err)
	}
}

// TestStreamingFallbackCompletesWhenOffsetIsAtEOF pins the review's ZZ4: a
// staging file that already holds the whole VOD asks for bytes past the end
// and gets a 416. The chunked loop reads 416 as "past end of file" and the
// pre-arc fallback sent no Range at all, so this is a finished download, not
// a failed job.
//
// Mutant: dropping the 416 special case — Start fails with
// "HTTP 416 downloading direct URL" over a complete recording.
func TestStreamingFallbackCompletesWhenOffsetIsAtEOF(t *testing.T) {
	const whole = "HEADHEADTAILTAIL"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "bytes=0-0" {
			w.WriteHeader(http.StatusInternalServerError) // probe never succeeds
			return
		}
		if strings.HasPrefix(rng, "bytes=16-") {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(whole))
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "video.mp4")
	resumeFile := seedDirectResume(t, path, whole)

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, OutputFile: path, IsDirectURL: true})
	d.delays = fastDelays()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start = %v, want nil for a resume offset already at EOF", err)
	}
	if got, _ := os.ReadFile(path); string(got) != whole {
		t.Fatalf("file = %q, want the complete %q untouched", got, whole)
	}
	if _, err := os.Stat(resumeFile); !os.IsNotExist(err) {
		t.Fatalf("resume sidecar still present after an at-EOF completion (stat err = %v)", err)
	}
}

// seedDirectResume writes staged bytes plus the matching resume sidecar that
// Start validates, and returns the sidecar's path. No StreamID and a
// non-YouTube URL, so resumeIdentityMismatch trusts the match and Start takes
// the size/append path — the same setup downloader_direct_resume_test.go uses.
func seedDirectResume(t *testing.T, path, staged string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(staged), 0o644); err != nil {
		t.Fatalf("seed staged bytes: %v", err)
	}
	resumeFile := path + ".resume.json"
	store := utils.ResumeStore[ResumeState]{Path: resumeFile}
	if err := store.Save(ResumeState{BytesWritten: int64(len(staged)), Timestamp: time.Now().Unix()}); err != nil {
		t.Fatalf("seed resume sidecar: %v", err)
	}
	return resumeFile
}

// TestFetchChunkWithRetryReportsTheLastCause pins that exhausting the
// per-chunk retries keeps the cause: the error names the last fetch's own
// failure and the status comes back with it. The caller
// (runDirectDownload) hands the error up unwrapped, so this text is what the
// job row shows — it used to read "chunk download failed: chunk download
// failed after 3 retries", with the 503 and its status gone.
//
// Mutant: returning a fresh fmt.Errorf with no %w — the message has no
// "HTTP 503" and the status is 0. Mutant: the caller re-adding its prefix —
// the text carries "chunk download failed" twice.
func TestFetchChunkWithRetryReportsTheLastCause(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, IsDirectURL: true})
	d.delays = fastDelays()
	data, status, err := d.fetchChunkWithRetry(context.Background(), 0, 7)
	if err == nil {
		t.Fatalf("fetchChunkWithRetry = (%q, %d, nil), want an error after %d failed attempts", data, status, MaxChunkRetries)
	}
	if got := attempts.Load(); got != MaxChunkRetries {
		t.Errorf("attempts = %d, want %d (MaxChunkRetries counts attempts)", got, MaxChunkRetries)
	}
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 — the last attempt's status must survive the retry loop", status)
	}
	msg := err.Error()
	if !strings.Contains(msg, "HTTP 503") {
		t.Errorf("error = %q, want it to carry the last cause (HTTP 503)", msg)
	}
	if want := fmt.Sprintf("after %d attempts", MaxChunkRetries); !strings.Contains(msg, want) {
		t.Errorf("error = %q, want %q in it", msg, want)
	}
	if strings.Count(msg, "chunk download failed") != 1 {
		t.Errorf("error = %q, want the prefix exactly once", msg)
	}
}
