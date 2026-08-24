package youtube

import "testing"

// TestCaptureVisitorData pins the watch-page → service visitor-data hand-off
// shared by the authenticated AND public extraction paths. The public path
// firing it too is load-bearing for anonymous (cookie-less) users: after
// invalidate403Caches clears the service cache, Init() never re-runs
// (startup-only call site), so this callback is the ONLY refill source —
// without it one 403 refresh leaves every later probe visitor-less for the
// process lifetime.
func TestCaptureVisitorData(t *testing.T) {
	t.Run("forwards non-empty visitor data", func(t *testing.T) {
		var got string
		p := &PlayerAPI{OnVisitorData: func(vd string) { got = vd }}

		p.captureVisitorData(&YtcfgData{VisitorData: "vd-abc"})

		if got != "vd-abc" {
			t.Errorf("OnVisitorData got %q, want %q", got, "vd-abc")
		}
	})

	t.Run("empty visitor data does not fire the callback", func(t *testing.T) {
		fired := false
		p := &PlayerAPI{OnVisitorData: func(string) { fired = true }}

		p.captureVisitorData(&YtcfgData{})

		if fired {
			t.Error("OnVisitorData fired for empty visitor data")
		}
	})

	t.Run("nil ytcfg and nil callback are safe no-ops", func(t *testing.T) {
		p := &PlayerAPI{OnVisitorData: func(string) {}}
		p.captureVisitorData(nil) // must not panic

		p = &PlayerAPI{} // nil callback
		p.captureVisitorData(&YtcfgData{VisitorData: "x"}) // must not panic
	})
}
