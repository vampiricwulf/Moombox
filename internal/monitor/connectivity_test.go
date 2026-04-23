package monitor

import (
	"sync/atomic"
	"testing"
)

type fakeReporter struct {
	failures atomic.Int64
	successes atomic.Int64
	lastTag   atomic.Value // string
}

func (f *fakeReporter) ReportFailure(tag string) {
	f.failures.Add(1)
	f.lastTag.Store(tag)
}

func (f *fakeReporter) ReportSuccess(tag string) {
	f.successes.Add(1)
	f.lastTag.Store(tag)
}

func TestSetConnectivityReporterRoundTrip(t *testing.T) {
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	f := &fakeReporter{}
	SetConnectivityReporter(f)

	reportMonitorResult("monitor/feed", true)
	reportMonitorResult("monitor/decapi", false)

	if got := f.failures.Load(); got != 1 {
		t.Errorf("expected 1 failure, got %d", got)
	}
	if got := f.successes.Load(); got != 1 {
		t.Errorf("expected 1 success, got %d", got)
	}
	if got := f.lastTag.Load(); got != "monitor/decapi" {
		t.Errorf("expected last tag 'monitor/decapi', got %v", got)
	}
}

func TestSetConnectivityReporterNilClears(t *testing.T) {
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	f := &fakeReporter{}
	SetConnectivityReporter(f)
	reportMonitorResult("monitor/feed", true)

	// Clear reporter; subsequent reports should be no-ops.
	SetConnectivityReporter(nil)
	reportMonitorResult("monitor/feed", true)
	reportMonitorResult("monitor/feed", false)

	if got := f.failures.Load(); got != 1 {
		t.Errorf("expected 1 failure after clear, got %d", got)
	}
	if got := f.successes.Load(); got != 0 {
		t.Errorf("expected 0 successes after clear, got %d", got)
	}
}

func TestReportMonitorResultNilReporter(t *testing.T) {
	// Sanity: a nil reporter must be a no-op (not a panic).
	t.Cleanup(func() { SetConnectivityReporter(nil) })
	SetConnectivityReporter(nil)
	reportMonitorResult("monitor/feed", true)
	reportMonitorResult("monitor/feed", false)
}
