package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// A whole-file chunk that arrives slowly is reported to OnFetch while it
// arrives, in several pieces that add up to the chunk, not once after its
// last byte. The file and the written-bytes counters are what they were.
//
// Mutant: `body.onBytes = meter.add` removed — one OnFetch call, made with
// the whole chunk.
func TestDirectChunkReportsArrivalWhileItIsRead(t *testing.T) {
	const slices, sliceBytes = 8, 50 * 1024
	body := headedBody(slices*sliceBytes, 'A')
	full := serveRangeFile(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if r.Method != http.MethodGet || end-start+1 < int64(len(body)) {
			full(w, r) // the size probe
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		for i := 0; i < slices; i++ {
			w.Write(body[i*sliceBytes : (i+1)*sliceBytes])
			w.(http.Flusher).Flush()
			time.Sleep(2 * arrivalFlushInterval)
		}
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "video.mp4")
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL + "/video.mp4", OutputFile: out, IsDirectURL: true})
	d.delays = directDelays()
	var calls, total, early atomic.Int64
	d.OnFetch = func(n int64) {
		calls.Add(1)
		total.Add(n)
		if d.BytesWritten() == 0 {
			early.Add(1) // reported before the chunk was written
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start = %v", err)
	}

	if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
		t.Errorf("output is %d bytes, want the %d-byte file", len(got), len(body))
	}
	if got := total.Load(); got != int64(len(body)) {
		t.Errorf("OnFetch reported %d bytes in all, want %d: each byte once", got, len(body))
	}
	if got := calls.Load(); got < slices/2 {
		t.Errorf("OnFetch fired %d times for a chunk that took %d slices to arrive, want several", got, slices)
	}
	if early.Load() < slices/2 {
		t.Errorf("only %d OnFetch calls came before the chunk was written, want the arrival reported while it is read", early.Load())
	}
	if got := d.BytesWritten(); got != int64(len(body)) {
		t.Errorf("BytesWritten = %d, want %d", got, len(body))
	}
}

// A chunk whose body breaks off is fetched again. What arrived of the broken
// attempt is reported to OnFetch (it did arrive), and nothing else moves:
// no byte of it is written, progress never exceeds the file, and the file is
// the origin's.
//
// Mutant: the loop's old `d.noteFetch(len(data))` restored — the retried
// chunk is counted twice. Mutant: the partial body written — the file is
// longer than the origin's.
func TestDirectChunkBrokenOffIsCountedAsArrivalOnly(t *testing.T) {
	const partial = 100 * 1024
	body := headedBody(600*1024, 'B')
	full := serveRangeFile(body)
	var dropped atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if r.Method != http.MethodGet || end-start+1 < int64(len(body)) || !dropped.CompareAndSwap(false, true) {
			full(w, r)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[:partial]) // short: the server drops the connection
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "video.mp4")
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL + "/video.mp4", OutputFile: out, IsDirectURL: true})
	d.delays = directDelays()
	var total, writtenDuringBroken, maxProgress atomic.Int64
	d.OnFetch = func(n int64) {
		if total.Add(n) <= partial {
			writtenDuringBroken.Add(d.BytesWritten())
		}
	}
	d.OnProgress = func(p DownloadProgress) {
		if p.Bytes > maxProgress.Load() {
			maxProgress.Store(p.Bytes)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start = %v, want the broken chunk fetched again", err)
	}

	if !dropped.Load() {
		t.Fatal("the server never broke a chunk off")
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
		t.Errorf("output is %d bytes, want the %d-byte file", len(got), len(body))
	}
	if got, want := total.Load(), int64(len(body)+partial); got != want {
		t.Errorf("OnFetch reported %d bytes, want %d: the file once and the broken attempt's %d", got, want, partial)
	}
	if writtenDuringBroken.Load() != 0 {
		t.Error("bytes were written while only the broken attempt had arrived")
	}
	if got := d.BytesWritten(); got != int64(len(body)) {
		t.Errorf("BytesWritten = %d, want %d", got, len(body))
	}
	if got := maxProgress.Load(); got != int64(len(body)) {
		t.Errorf("progress reached %d bytes, want exactly the file's %d", got, len(body))
	}
}
