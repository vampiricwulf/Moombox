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
	"sync"
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

// A chunk whose body breaks off keeps what arrived: the part is written, the
// next request starts where it ended, and every byte is reported to OnFetch
// once. No response here carries more than `partial` bytes, so the file takes
// more breaks than fetchChunkWithRetry has attempts: a link that cannot carry
// a whole chunk in one response still finishes, because only a failure that
// delivered nothing is charged. The part used to be dropped and the whole
// range asked again, so this download failed after eleven attempts.
//
// Mutant: fetchChunkWithRetry without `keep` — Start fails "after 11
// attempts". Mutant: the part kept AND the same range asked again — the
// requests do not advance by `partial`, and the file is longer than the
// origin's.
func TestDirectChunkBrokenOffKeepsWhatArrived(t *testing.T) {
	const partial = 100 * 1024
	const breaks = MaxChunkRetries + 1
	body := headedBody((breaks+1)*partial, 'B')
	full := serveRangeFile(body)
	var mu sync.Mutex
	var starts []int64
	var dropped atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if r.Method != http.MethodGet || end-start+1 <= partial {
			full(w, r) // the size probe, and the last part
			return
		}
		mu.Lock()
		starts = append(starts, start)
		mu.Unlock()
		dropped.Add(1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[start : start+partial]) // short: the server drops the connection
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "video.mp4")
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL + "/video.mp4", OutputFile: out, IsDirectURL: true})
	d.delays = directDelays()
	var total, maxProgress atomic.Int64
	d.OnFetch = func(n int64) { total.Add(n) }
	d.OnProgress = func(p DownloadProgress) {
		if p.Bytes > maxProgress.Load() {
			maxProgress.Store(p.Bytes)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start = %v, want the file finished from the parts that arrived", err)
	}

	if got := dropped.Load(); got != breaks {
		t.Fatalf("the server broke %d responses off, want %d", got, breaks)
	}
	mu.Lock()
	for i, start := range starts {
		if want := int64(i) * partial; start != want {
			t.Errorf("request %d asked from byte %d, want %d: each one starts where the last part ended (all: %v)", i, start, want, starts)
			break
		}
	}
	mu.Unlock()
	if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
		t.Errorf("output is %d bytes, want the %d-byte file", len(got), len(body))
	}
	if got, want := total.Load(), int64(len(body)); got != want {
		t.Errorf("OnFetch reported %d bytes, want %d: each byte once", got, want)
	}
	if got := d.BytesWritten(); got != int64(len(body)) {
		t.Errorf("BytesWritten = %d, want %d", got, len(body))
	}
	if got := maxProgress.Load(); got != int64(len(body)) {
		t.Errorf("progress reached %d bytes, want exactly the file's %d", got, len(body))
	}
}

// A break that delivered part of the range while the device is offline still
// waits the outage out before the next request: the part is kept, and the
// request for the rest goes out only once the link is back.
//
// Mutant: `keep` returning the part ahead of the IsOnline check — no
// ActivityReconnecting, and the next request is made during the outage.
func TestDirectChunkBrokenOffKeepsWhatArrivedAndWaitsOutAnOutage(t *testing.T) {
	const partial = 100 * 1024
	body := headedBody(3*partial, 'C')
	full := serveRangeFile(body)
	var offline, broke, askedOffline atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if r.Method != http.MethodGet || end-start+1 < int64(len(body)) || !broke.CompareAndSwap(false, true) {
			if r.Method == http.MethodGet && offline.Load() {
				askedOffline.Store(true)
			}
			full(w, r)
			return
		}
		offline.Store(true)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[:partial]) // short: the server drops the connection
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "video.mp4")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL: srv.URL + "/video.mp4", OutputFile: out, IsDirectURL: true,
		IsOnline: func() bool { return !offline.Load() },
	})
	d.delays = directDelays()
	var reconnecting atomic.Bool
	var total atomic.Int64
	d.OnFetch = func(n int64) { total.Add(n) }
	d.OnActivity = func(a DownloadActivity) {
		if a == ActivityReconnecting {
			reconnecting.Store(true)
			offline.Store(false) // the link comes back once the wait has begun
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start = %v, want the outage waited out", err)
	}
	if !broke.Load() || !reconnecting.Load() {
		t.Fatalf("broke = %v, reconnecting = %v: want a broken response and a wait for connectivity", broke.Load(), reconnecting.Load())
	}
	if askedOffline.Load() {
		t.Error("the rest of the file was asked for while the device was offline")
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
		t.Errorf("output is %d bytes, want the %d-byte file", len(got), len(body))
	}
	if got, want := total.Load(), int64(len(body)); got != want {
		t.Errorf("OnFetch reported %d bytes, want %d: the kept part is not fetched again", got, want)
	}
}
