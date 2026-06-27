package engine

import "testing"

func TestEmitActivityCallsCallback(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{OutputFile: "x"})
	got := ActivityNone
	d.OnActivity = func(a DownloadActivity) { got = a }

	d.emitActivity(ActivityVerifyingEnd)

	if got != ActivityVerifyingEnd {
		t.Errorf("emitActivity delivered %v, want ActivityVerifyingEnd", got)
	}
}

func TestEmitActivityNilSafe(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{OutputFile: "x"})
	d.OnActivity = nil
	// Must not panic when no callback is registered.
	d.emitActivity(ActivityRateLimited)
}
