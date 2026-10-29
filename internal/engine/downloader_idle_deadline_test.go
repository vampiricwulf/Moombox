package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// trickleServer writes total bytes in `chunks` writes spaced `gap` apart,
// flushing each one, so the transfer is continuously progressing but takes
// far longer overall than the idle bound.
func trickleServer(t *testing.T, chunks int, gap time.Duration, chunkSize int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		buf := make([]byte, chunkSize)
		for range chunks {
			w.Write(buf)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(gap)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSlowButProgressingTransfersSurvive pins ENGINE-4's fix on both paths
// that read a body under the idle bound alone, and the ceiling's other half:
// a transfer that keeps delivering bytes must complete however long it takes
// in total. fetchSegment (segments, playlists, and the hard ceiling layered
// under the idle bound, which must not clip it either) and the whole-file
// streaming fallback (runDirectDownloadFallback, whose one GET for a
// multi-hour VOD has no other bound) read the same trickle side by side, so
// the pair pays its wall clock once. Thirty 40 ms gaps against a 1 s idle
// bound is more than 1.16 s — longer than the bound itself — with no gap
// ever coming near it, under a 10 s ceiling (far out, because it too must not
// fire, and it counts every stall the runner adds to the transfer).
//
// The margins are wide on purpose: the idle timer must NOT fire, and it and
// the trickling server share this process, so a runner that deschedules the
// process for longer than (idle − gap) fails the transfer however correct the
// code is. 960 ms of margin (a 25x ratio) rides that out; the 40 ms of a
// 60 ms bound against 20 ms gaps, which both paths had, did not.
//
// Mutant: restoring `ctx, cancel := context.WithTimeout(parent, SegmentTimeout)`
// in fetchSegment — the fetch dies at 1 s with context.DeadlineExceeded.
// Mutant: a ceiling shorter than the transfer (fetchSegment passing the idle
// bound as its ceiling) — the payload comes back short with errFetchCeiling.
// Mutant: giving streamDirectOnce a total context.WithTimeout(parent,
// SegmentTimeout) instead of the idle deadline — every request of the
// fallback dies at 1 s, mid-file, and its ladder runs out.
func TestSlowButProgressingTransfersSurvive(t *testing.T) {
	prev := SegmentTimeout
	t.Cleanup(func() { SegmentTimeout = prev })
	SegmentTimeout = time.Second

	const chunks = 30
	srv := trickleServer(t, chunks, 40*time.Millisecond, 1024)

	seg := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL})
	seg.delays.fetchHardCeiling = 10 * time.Second

	path := filepath.Join(t.TempDir(), "video.mp4")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	direct := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, OutputFile: path, IsDirectURL: true})
	direct.outputFile = f
	direct.delays = directDelays()

	start := time.Now()
	fallbackDone := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fallbackDone <- fmt.Errorf("runDirectDownloadFallback panicked: %v", r)
			}
		}()
		fallbackDone <- direct.runDirectDownloadFallback(context.Background())
	}()

	data, status, err := seg.fetchSegment(context.Background(), srv.URL+"/seg")
	if err != nil {
		t.Errorf("fetchSegment = %v, want nil for a continuously progressing transfer inside the ceiling", err)
	} else if status != http.StatusOK || len(data) != chunks*1024 {
		t.Errorf("fetchSegment = %d bytes/status %d, want %d/200", len(data), status, chunks*1024)
	}
	if err := <-fallbackDone; err != nil {
		t.Errorf("runDirectDownloadFallback = %v, want nil for a continuously progressing transfer", err)
	} else if got := direct.bytesWritten.Load(); got != chunks*1024 {
		t.Errorf("runDirectDownloadFallback wrote %d bytes, want %d", got, chunks*1024)
	}
	if elapsed := time.Since(start); elapsed < SegmentTimeout {
		t.Fatalf("transfers took %s — shorter than the idle bound, so they never exercised the deadline", elapsed)
	}
}

// TestFetchSegmentFailsOnIdleStall pins the other half: the 30 s value is
// still a deadline, just an idle one. A server that sends headers and then
// nothing must fail, and the error must name the stall rather than read as a
// caller cancellation.
//
// Mutant: dropping the timer entirely (a plain context.WithCancel) — the
// fetch hangs until the test's own 2 s guard fires.
func TestFetchSegmentFailsOnIdleStall(t *testing.T) {
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

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL})
	done := make(chan error, 1)
	go func() {
		_, _, err := d.fetchSegment(context.Background(), srv.URL+"/seg")
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, errFetchIdle) {
			t.Fatalf("fetchSegment = %v, want errFetchIdle", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fetchSegment never returned — the idle deadline did not fire")
	}
}

// TestFetchSegmentCallerCancelIsNotAnIdleStall pins the distinction the
// cause carries: a caller that cancels must still see a cancellation, so
// reportFetchFailure's parent guard and the loops' cancelErr keep working.
//
// Mutant: returning errFetchIdle for every context error instead of
// consulting context.Cause — a clean shutdown is reported as a network stall
// and drags the connectivity oracle toward "offline".
func TestFetchSegmentCallerCancelIsNotAnIdleStall(t *testing.T) {
	prev := SegmentTimeout
	t.Cleanup(func() { SegmentTimeout = prev })
	SegmentTimeout = 5 * time.Second

	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL})
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	_, _, err := d.fetchSegment(ctx, srv.URL+"/seg")
	if errors.Is(err, errFetchIdle) {
		t.Fatalf("fetchSegment = %v, want a cancellation, not an idle stall", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("fetchSegment = %v, want context.Canceled", err)
	}
}

// TestIdleBodyReleasesItsTimerOnClose pins the release half of the wrapper's
// contract: once the fetch is over the armed timer must be stopped, or every
// finished fetch leaves a live runtime timer holding its context's cancel
// closure until the idle bound elapses — and then fires it against a context
// the caller may still be using.
//
// Mutant: dropping `b.timer.Stop()` from idleBody.Close — the context is
// cancelled with errFetchIdle 50 ms after the body was already closed.
func TestIdleBodyReleasesItsTimerOnClose(t *testing.T) {
	const idle = 50 * time.Millisecond
	ctx, timer, cancel := withReadProgressDeadline(context.Background(), idle)
	t.Cleanup(func() { cancel() })

	body := &idleBody{rc: io.NopCloser(strings.NewReader("hello")), timer: timer, idle: idle}
	if _, err := io.ReadAll(body); err != nil {
		t.Fatalf("ReadAll = %v, want nil", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}

	select {
	case <-ctx.Done():
		t.Fatalf("context cancelled after Close (%v) — the timer outlived the body", context.Cause(ctx))
	case <-time.After(4 * idle):
	}
}

// zeroByteReader answers every Read with (0, nil): a body that is being
// polled but delivers nothing. It stands in for the one shape the n > 0 guard
// exists for — a reader that returns rather than blocks while starved.
type zeroByteReader struct{}

func (zeroByteReader) Read(p []byte) (int, error) { return 0, nil }
func (zeroByteReader) Close() error               { return nil }

// TestIdleBodyDoesNotExtendOnAZeroByteRead pins the guard on the reset: only
// a Read that actually delivered bytes is progress. A body polled in a tight
// loop that never yields a byte is precisely the stall the bound exists to
// end, so it must still end.
//
// Mutant: resetting on every Read instead of only when n > 0 — the deadline
// is pushed out forever and this test fails on its 2 s guard.
func TestIdleBodyDoesNotExtendOnAZeroByteRead(t *testing.T) {
	const idle = 60 * time.Millisecond
	ctx, timer, cancel := withReadProgressDeadline(context.Background(), idle)
	t.Cleanup(func() { cancel() })

	body := &idleBody{rc: zeroByteReader{}, timer: timer, idle: idle}
	go func() {
		buf := make([]byte, 64)
		for ctx.Err() == nil {
			body.Read(buf)
		}
	}()

	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), errFetchIdle) {
			t.Fatalf("cause = %v, want errFetchIdle", context.Cause(ctx))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the idle bound never fired — a byte-less Read counted as progress")
	}
}

// TestFetchSegmentLeavesNoGoroutinesBehind is the leak backstop for the new
// per-fetch timer: a run of ordinary fast fetches, each arming and releasing
// one, must leave the goroutine population where it started.
func TestFetchSegmentLeavesNoGoroutinesBehind(t *testing.T) {
	prev := SegmentTimeout
	t.Cleanup(func() { SegmentTimeout = prev })
	SegmentTimeout = 200 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("payload"))
	}))
	t.Cleanup(srv.Close)

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL})
	// Warm up: the first fetch builds the transport's connection machinery,
	// which is long-lived and must not count as growth.
	if _, _, err := d.fetchSegment(context.Background(), srv.URL+"/seg"); err != nil {
		t.Fatalf("warmup fetchSegment = %v", err)
	}
	baseline := settledGoroutines(t, 0)

	for range 40 {
		if _, _, err := d.fetchSegment(context.Background(), srv.URL+"/seg"); err != nil {
			t.Fatalf("fetchSegment = %v", err)
		}
	}
	// Wait past the idle bound: a timer that was never stopped would fire in
	// this window, so any goroutine it spawns is counted.
	time.Sleep(2 * SegmentTimeout)

	if got := settledGoroutines(t, baseline); got > baseline+2 {
		t.Fatalf("goroutines = %d after 40 fetches, baseline %d — the per-fetch timer leaks", got, baseline)
	}
}

// endlessTrickleServer writes one byte every `gap` until the client goes
// away, so nothing but a deadline can end a fetch against it. It is what a
// throttling or half-dead CDN looks like from the downloader's side: the
// read-progress deadline alone can never end this transfer, because every
// gap is shorter than the idle bound.
//
// prepare writes the status line and any headers; nil means a plain 200.
func endlessTrickleServer(t *testing.T, gap time.Duration, prepare func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if prepare != nil {
			prepare(w)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		fl, _ := w.(http.Flusher)
		one := []byte{0}
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(gap):
			}
			if _, err := w.Write(one); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// headersThenSilenceServer answers 200, delivers one byte, and then holds the
// response open forever — the shape the idle bound exists for.
func headersThenSilenceServer(t *testing.T) *httptest.Server {
	t.Helper()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte{0})
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })
	return srv
}

// deadlineTestContext returns a context whose cancel is ALSO registered as a
// t.Cleanup. Cleanups run LIFO, so it fires before the httptest server's own
// Close: a row whose guard tripped (every mutant below) leaves a fetch
// blocked on an endless body, and without this the server's Close waits on
// that still-active connection and the whole package run hangs instead of
// reporting the failure.
func deadlineTestContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx, cancel
}

// TestFetchHardCeilingCapsATrickle pins the ceiling layered UNDER the idle
// deadline (Task 3 fix round 1): a body that delivers a byte just often enough
// to keep resetting the idle timer would otherwise run forever, and on a 24/7
// downloader that wedges one segment worker for the whole stream without ever
// producing the error fetchSegmentWithRetry's ladder needs. It drives both
// fetch paths — fetchSegment, and the VOD chunk path fetchChunk, which shares
// neither the request construction nor the status handling with it — side by
// side against one endless trickle, so the pair pays the ceiling's wait once.
//
// The idle bound must NOT fire here, so it sits 950 ms clear of the 50 ms
// trickle: a process the runner deschedules for longer than (idle − gap)
// sees the idle timer fire first on resume. The 100 ms that a 200 ms bound
// against a 100 ms trickle left failed both paths under 120 ms SIGSTOPs.
//
// Mutant: drop the context.WithTimeout layer (round 1's behaviour) — neither
// fetch returns and both fail on the guard. Mutant: apply the ceiling to
// fetchSegment only — the fetchChunk path fails on the guard while the
// fetchSegment path stays green. Mutant: the idle bound as the ceiling — both
// return before their ceiling.
func TestFetchHardCeilingCapsATrickle(t *testing.T) {
	prev := SegmentTimeout
	t.Cleanup(func() { SegmentTimeout = prev })
	SegmentTimeout = time.Second

	srv := endlessTrickleServer(t, 50*time.Millisecond, func(w http.ResponseWriter) {
		// 206 so fetchChunk takes its ordinary partial-content path;
		// fetchSegment reads any 2xx as its body.
		w.Header().Set("Content-Range", "bytes 0-1048575/8388608")
		w.WriteHeader(http.StatusPartialContent)
	})
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL})
	d.delays.fetchHardCeiling = 1200 * time.Millisecond

	// Cleanups run LIFO, so this cancel fires BEFORE the server's Close: if
	// the guard below trips (the mutant), the still-running fetch would
	// otherwise hold the connection open and wedge httptest's Close.
	ctx, cancel := deadlineTestContext(t)
	defer cancel()

	type result struct {
		path    string
		err     error
		elapsed time.Duration
	}
	start := time.Now()
	results := make(chan result, 2)
	fetches := map[string]func() error{
		"fetchSegment": func() error { _, _, err := d.fetchSegment(ctx, srv.URL+"/seg"); return err },
		"fetchChunk":   func() error { _, _, err := d.fetchChunk(ctx, 0, 1<<20-1); return err },
	}
	for path, fetch := range fetches {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s panicked: %v", path, r)
				}
			}()
			err := fetch()
			results <- result{path, err, time.Since(start)}
		}()
	}

	guard := time.After(10 * time.Second)
	for range fetches {
		select {
		case r := <-results:
			if !errors.Is(r.err, errFetchCeiling) {
				t.Errorf("%s = %v, want errFetchCeiling", r.path, r.err)
			}
			if errors.Is(r.err, errFetchIdle) {
				t.Errorf("%s = %v — a transfer that kept delivering is not an idle stall", r.path, r.err)
			}
			if r.elapsed < d.delays.fetchHardCeiling {
				t.Errorf("%s returned after %s, before its %s ceiling", r.path, r.elapsed, d.delays.fetchHardCeiling)
			}
			// Generous upper margin: the load-bearing assertion is the
			// guard, which is what the mutants trip; this one only catches
			// a ceiling wired far out.
			if r.elapsed > 2*d.delays.fetchHardCeiling {
				t.Errorf("%s returned after %s, far past its %s ceiling", r.path, r.elapsed, d.delays.fetchHardCeiling)
			}
			t.Logf("%s: ceiling fired after %s (ceiling %s, idle %s): %v", r.path, r.elapsed, d.delays.fetchHardCeiling, SegmentTimeout, r.err)
		case <-guard:
			t.Fatal("a fetch never returned — the hard ceiling did not fire on every path")
		}
	}
}

// TestFetchSegmentIdleStillFiresFirst pins the layering order: the ceiling is
// the outer, generous bound, and a genuine stall must still end at the idle
// deadline rather than sitting on a dead socket until the ceiling.
//
// Mutant: swap the two bounds (idle 5 s, ceiling 200 ms) — the error becomes
// errFetchCeiling and this row fails.
func TestFetchSegmentIdleStillFiresFirst(t *testing.T) {
	prev := SegmentTimeout
	t.Cleanup(func() { SegmentTimeout = prev })
	SegmentTimeout = 200 * time.Millisecond

	srv := headersThenSilenceServer(t)
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL})
	d.delays.fetchHardCeiling = 5 * time.Second

	ctx, cancel := deadlineTestContext(t)
	defer cancel()

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, _, err := d.fetchSegment(ctx, srv.URL+"/seg")
		done <- err
	}()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		if !errors.Is(err, errFetchIdle) {
			t.Fatalf("fetchSegment = %v, want errFetchIdle", err)
		}
		if errors.Is(err, errFetchCeiling) {
			t.Fatalf("fetchSegment = %v, want the idle bound to fire first", err)
		}
		if elapsed > d.delays.fetchHardCeiling/2 {
			t.Fatalf("fetchSegment returned after %s — that is the ceiling's clock, not the %s idle bound", elapsed, SegmentTimeout)
		}
		t.Logf("idle bound fired after %s (idle %s, ceiling %s): %v", elapsed, SegmentTimeout, d.delays.fetchHardCeiling, err)
	case <-time.After(2 * time.Second):
		t.Fatal("fetchSegment never returned — neither deadline fired")
	}
}

// TestFetchSegmentWithRetryTreatsBothDeadlinesAsTransient is the differential
// the controller's ruling asks for: the ladder's behaviour on a ceiling
// expiry must be its behaviour on an idle stall — retried, then exhausted —
// because both are "this CDN is not working right now", never "this segment
// is gone". MaxRetries is 1 so the ladder skips its post-attempt sleep.
//
// Mutant: route errFetchCeiling to ErrSegmentPermanent in
// fetchSegmentWithRetry — the ceiling row fails while the idle row passes,
// which is exactly the asymmetry this row exists to forbid.
func TestFetchSegmentWithRetryTreatsBothDeadlinesAsTransient(t *testing.T) {
	prev := SegmentTimeout
	t.Cleanup(func() { SegmentTimeout = prev })

	for _, tc := range []struct {
		name    string
		server  func(t *testing.T) *httptest.Server
		idle    time.Duration
		ceiling time.Duration
	}{
		{
			name:    "idle stall",
			server:  headersThenSilenceServer,
			idle:    200 * time.Millisecond,
			ceiling: 5 * time.Second,
		},
		{
			// The idle bound sits well past the ceiling, so it is the
			// ceiling that ends the attempt even if the runner deschedules
			// the trickle: with 200 ms against a 50 ms trickle a stall of
			// 150 ms let the idle bound end it instead, and this row then
			// proved nothing about the ceiling.
			name: "hard ceiling",
			server: func(t *testing.T) *httptest.Server {
				return endlessTrickleServer(t, 50*time.Millisecond, nil)
			},
			idle:    time.Second,
			ceiling: 400 * time.Millisecond,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			SegmentTimeout = tc.idle
			srv := tc.server(t)
			d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, MaxRetries: 1})
			d.delays.fetchHardCeiling = tc.ceiling

			// Guarded: with a deadline missing the ladder would sit on the
			// socket until go test's own 10-minute panic, which reads as a
			// hung suite rather than a failed assertion.
			ctx, cancel := deadlineTestContext(t)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := d.fetchSegmentWithRetry(ctx, srv.URL+"/seg", nil)
				done <- err
			}()
			var err error
			select {
			case err = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("fetchSegmentWithRetry never returned — neither deadline ended the attempt")
			}

			if errors.Is(err, ErrSegmentPermanent) {
				t.Fatalf("fetchSegmentWithRetry = %v, want a transient outcome — a deadline is never proof the segment is gone", err)
			}
			if !errors.Is(err, ErrSegmentRetriesExhausted) {
				t.Fatalf("fetchSegmentWithRetry = %v, want ErrSegmentRetriesExhausted", err)
			}
		})
	}
}

// settledGoroutines polls runtime.NumGoroutine until it stops shrinking (or
// reaches want), so an idle-connection reaper still winding down from an
// earlier test is not mistaken for a leak.
func settledGoroutines(t *testing.T, want int) int {
	t.Helper()
	last := runtime.NumGoroutine()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n <= want || n == last {
			return n
		}
		last = n
	}
	return last
}
