package worker

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeReporter struct{ fails, oks int }

func (f *fakeReporter) ReportFailure(string) { f.fails++ }
func (f *fakeReporter) ReportSuccess(string) { f.oks++ }

// TestReportFetchOutcome verifies the confirmatory/secondary full-fetch error
// reporter feeds the oracle by reachability class — network-class → failure,
// server-class → success (reached the service) — and stays silent on cancel.
func TestReportFetchOutcome(t *testing.T) {
	t.Cleanup(func() { SetConnectivityReporter(nil) })
	f := &fakeReporter{}
	SetConnectivityReporter(f)

	reportFetchOutcome(&net.DNSError{Err: "no such host"}, "probe/youtube")
	if f.fails != 1 || f.oks != 0 {
		t.Fatalf("network err: want fails=1 oks=0, got fails=%d oks=%d", f.fails, f.oks)
	}
	reportFetchOutcome(fmt.Errorf("WEB API error: HTTP 404"), "probe/youtube")
	if f.fails != 1 || f.oks != 1 {
		t.Fatalf("server err: want fails=1 oks=1, got fails=%d oks=%d", f.fails, f.oks)
	}
	reportFetchOutcome(context.Canceled, "probe/youtube")
	if f.fails != 1 || f.oks != 1 {
		t.Fatalf("cancelled: want unchanged fails=1 oks=1, got fails=%d oks=%d", f.fails, f.oks)
	}
}

type atomicCountingReporter struct{ fails, oks atomic.Int64 }

func (a *atomicCountingReporter) ReportFailure(string) { a.fails.Add(1) }
func (a *atomicCountingReporter) ReportSuccess(string) { a.oks.Add(1) }

// TestSetConnectivityReporter_ConcurrentRaceFree proves the atomic.Pointer
// reporter storage is race-free under concurrent Set + read/dispatch. The
// `connReporter.Store(&r)` pattern is safe Go: `go build -gcflags=-m` reports
// "moved to heap: r", i.e. the compiler heap-allocates r because its address
// escapes into the package-level atomic — so &r is a valid heap pointer, not a
// dangling stack pointer. This test guards that under the race detector.
func TestSetConnectivityReporter_ConcurrentRaceFree(t *testing.T) {
	t.Cleanup(func() { SetConnectivityReporter(nil) })
	rep := &atomicCountingReporter{}

	var wg sync.WaitGroup
	for range 200 {
		wg.Add(2)
		go func() { defer wg.Done(); SetConnectivityReporter(rep) }()
		go func() { defer wg.Done(); reportProbeResult("probe/test", true) }()
	}
	wg.Wait()
	// No exact-count assertion: setters race the readers, so early reads may
	// observe nil and no-op. The guarantees under test are (1) -race is clean
	// and (2) no panic from dereferencing the stored &r. The reporter's own
	// counters are atomic, so any observed forwarding is itself race-free.
}

func TestWorkerConnectivityReporterRoundTrip(t *testing.T) {
	t.Cleanup(func() { SetConnectivityReporter(nil) })
	f := &fakeReporter{}
	SetConnectivityReporter(f)

	reportProbeResult("probe/youtube", true)
	reportProbeResult("probe/youtube", false)
	if f.fails != 1 || f.oks != 1 {
		t.Fatalf("want fails=1 oks=1, got fails=%d oks=%d", f.fails, f.oks)
	}

	SetConnectivityReporter(nil)
	reportProbeResult("probe/youtube", true) // must be a no-op, not panic
	if f.fails != 1 {
		t.Fatalf("nil reporter must not forward, got fails=%d", f.fails)
	}
}
