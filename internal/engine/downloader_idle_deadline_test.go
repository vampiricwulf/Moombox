package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

// TestFetchSegmentSurvivesSlowButProgressingTransfer pins ENGINE-4's fix: a
// transfer that keeps delivering bytes must complete however long it takes in
// total. Ten 20 ms gaps against a 60 ms idle bound is 200+ ms of wall clock —
// more than three times the deadline — with no gap ever reaching it.
//
// Mutant: restoring `ctx, cancel := context.WithTimeout(parent, SegmentTimeout)`
// in fetchSegment — the fetch dies at 60 ms with 0 bytes and
// context.DeadlineExceeded.
func TestFetchSegmentSurvivesSlowButProgressingTransfer(t *testing.T) {
	prev := SegmentTimeout
	t.Cleanup(func() { SegmentTimeout = prev })
	SegmentTimeout = 60 * time.Millisecond

	srv := trickleServer(t, 10, 20*time.Millisecond, 1024)
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL})

	start := time.Now()
	data, status, err := d.fetchSegment(context.Background(), srv.URL+"/seg")
	if err != nil {
		t.Fatalf("fetchSegment = %v, want nil for a continuously progressing transfer", err)
	}
	if status != http.StatusOK || len(data) != 10*1024 {
		t.Fatalf("fetchSegment = %d bytes/status %d, want 10240/200", len(data), status)
	}
	if elapsed := time.Since(start); elapsed < SegmentTimeout {
		t.Fatalf("transfer took %s — shorter than the idle bound, so it never exercised the deadline", elapsed)
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
