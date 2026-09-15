package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// countingReporter is an atomic ConnectivityReporter: the engine's reporter
// is a package global, so a plain-int recorder is not safe to install while
// any other test is in flight.
type countingReporter struct {
	fails     atomic.Int32
	successes atomic.Int32
}

func (c *countingReporter) ReportFailure(string) { c.fails.Add(1) }
func (c *countingReporter) ReportSuccess(string) { c.successes.Add(1) }

// TestFetchSegmentCancelIsNotAConnectivityFailure pins T4-35: a shutdown, a
// quality split or a superseded refresh cancels the download context, the
// in-flight request dies with it, and that is a decision Moombox made — not
// evidence about the network. Counting it drags the connectivity oracle
// toward "offline" on every clean stop.
//
// The handler blocks until the client goes away, so the ONLY way the request
// ends is the caller's cancel.
//
// Mutant this kills: reporting unconditionally (fails becomes 1). This test
// alone does NOT discriminate a guard on parent from one on the SHADOWED
// derived ctx: cancelling parent finishes the derived context too, so a
// shadowed-ctx guard suppresses here exactly like the correct one.
// TestFetchSegmentDerivedTimeoutIsAConnectivityFailure below is what kills
// that mutant — it's the only scenario where parent stays alive but the
// derived context is the one that finishes.
//
// Do not add t.Parallel(): the reporter is a package global.
func TestFetchSegmentCancelIsNotAConnectivityFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	rec := &countingReporter{}
	SetConnectivityReporter(rec)
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
	})

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		defer func() { _ = recover() }()
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, _, err := d.fetchSegment(ctx, d.buildSegmentURL(1)); err == nil {
		t.Fatal("fetchSegment on a cancelled context returned nil error")
	}
	cancel()

	if got := rec.fails.Load(); got != 0 {
		t.Errorf("connectivity failures = %d, want 0 — a caller cancel was recorded as a network failure", got)
	}
}

// TestFetchSegmentTransportErrorIsAConnectivityFailure is the other half:
// with a perfectly healthy caller context, a request that dies on the wire
// IS network evidence and must still be reported.
//
// The server hangs up without answering, which is a transport error at the
// client. Note what this test deliberately does NOT do: it passes t.Context()
// straight through, with no derived deadline of its own, so the only context
// that can be done is the one fetchSegment derives internally.
//
// Mutant this kills: suppressing every error outright (fails drops to 0).
// It does NOT discriminate a parent guard from a SHADOWED/derived-ctx
// guard: the connection reset here happens instantly, long before either
// context's deadline, so both are still alive and a shadowed-ctx guard
// reports identically to the correct one.
func TestFetchSegmentTransportErrorIsAConnectivityFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			conn.Close() // no response at all
		}
	}))
	t.Cleanup(srv.Close)

	rec := &countingReporter{}
	SetConnectivityReporter(rec)
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
	})
	if _, _, err := d.fetchSegment(t.Context(), d.buildSegmentURL(1)); err == nil {
		t.Fatal("fetchSegment against a hang-up server returned nil error")
	}
	if got := rec.fails.Load(); got != 1 {
		t.Errorf("connectivity failures = %d, want 1 — a transport error with a healthy caller IS network evidence", got)
	}
}

// TestFetchSegmentDerivedTimeoutIsAConnectivityFailure is the discriminator
// the two tests above cannot be: it is the only scenario where the parent
// stays alive but the DERIVED (shadowed) context is the one that finishes.
// SegmentTimeout is shrunk to a few milliseconds so the derived context
// times out against a server that never answers, while the caller's own
// context (t.Context()) never expires. A request that genuinely ran out of
// time with a healthy caller IS network evidence and must still be
// reported.
//
// Mutant this kills: guarding on the SHADOWED/derived ctx instead of
// parent — fails would stay 0, exactly backwards from what a guard against
// caller-cancellation is supposed to catch.
//
// Do not add t.Parallel(): shrinks the package-global SegmentTimeout, and
// the reporter is a package global.
func TestFetchSegmentDerivedTimeoutIsAConnectivityFailure(t *testing.T) {
	orig := SegmentTimeout
	SegmentTimeout = 20 * time.Millisecond
	t.Cleanup(func() { SegmentTimeout = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	rec := &countingReporter{}
	SetConnectivityReporter(rec)
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
	})
	if _, _, err := d.fetchSegment(t.Context(), d.buildSegmentURL(1)); err == nil {
		t.Fatal("fetchSegment against a server that never answers returned nil error")
	}
	if got := rec.fails.Load(); got != 1 {
		t.Errorf("connectivity failures = %d, want 1 — a derived-context timeout with a healthy caller IS network evidence", got)
	}
}

// TestProbeSegmentAvailableCancelIsNotAConnectivityFailure is the eviction
// probe's counterpart to TestFetchSegmentCancelIsNotAConnectivityFailure:
// ProbeSegmentAvailable (eviction_probe.go) shares fetchSegment's exact
// shape — derive ctx from parent with SegmentTimeout, then an unconditional
// report on error — and must be guarded against caller cancellation the
// same way.
//
// Mutant this kills: reporting unconditionally (fails becomes 1).
//
// Do not add t.Parallel(): the reporter is a package global.
func TestProbeSegmentAvailableCancelIsNotAConnectivityFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	rec := &countingReporter{}
	SetConnectivityReporter(rec)
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
	})

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		defer func() { _ = recover() }()
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, _, err := d.ProbeSegmentAvailable(ctx, 1); err == nil {
		t.Fatal("ProbeSegmentAvailable on a cancelled context returned nil error")
	}
	cancel()

	if got := rec.fails.Load(); got != 0 {
		t.Errorf("connectivity failures = %d, want 0 — a caller cancel was recorded as a network failure", got)
	}
}
