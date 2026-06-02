package worker

import "testing"

type fakeReporter struct{ fails, oks int }

func (f *fakeReporter) ReportFailure(string) { f.fails++ }
func (f *fakeReporter) ReportSuccess(string) { f.oks++ }

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
