package tui

import (
	"strings"
	"testing"
)

// TestStatusBarShowsSidecarDown is the TUI half of YOUTUBE-5's user-visible
// signal, and it is an ALERT: while the sidecar is down, sig-ciphered formats
// cannot be resolved at all. So it must survive the same tier squeeze OFFLINE
// survives (see TestStatusBarAlertsOutliveCounters), abbreviating rather than
// disappearing.
//
// Mutants this kills:
//   - the chip never rendered                     → the wide case finds nothing
//   - the chip gated on t <= tierCompact          → the narrow case finds nothing
//   - the chip rendered when sidecarDown is false → the healthy case finds it
func TestStatusBarShowsSidecarDown(t *testing.T) {
	wide := NewStatusBarModel()
	wide.sidecarDown = true
	wide.SetWidth(200)
	if got := stripANSI(wide.View()); !strings.Contains(got, "SIDECAR DOWN") {
		t.Errorf("wide bar has no sidecar alert: %q", got)
	}

	// busyStatusBar, not a bare model, and width 46 — exactly what
	// TestStatusBarAlertsOutliveCounters uses. A bare bar has so little to
	// render that 46 columns still seat the richest tier, so it would prove
	// nothing about the squeeze.
	narrow := busyStatusBar()
	narrow.sidecarDown = true
	narrow.SetWidth(46)
	if got := stripANSI(narrow.View()); !strings.Contains(got, "POT") {
		t.Errorf("sidecar alert dropped at width 46: %q", got)
	}
	if got := stripANSI(narrow.View()); strings.Contains(got, "Backfill") || strings.Contains(got, "BF:") {
		t.Errorf("informational backfill outlived the sidecar alert at width 46: %q", got)
	}

	healthy := NewStatusBarModel()
	healthy.SetWidth(200)
	if got := stripANSI(healthy.View()); strings.Contains(got, "SIDECAR") || strings.Contains(got, "POT") {
		t.Errorf("healthy bar drew a sidecar alert: %q", got)
	}
}

// TestSidecarStatusMsgReachesTheBar pins the wiring end of the same signal:
// cmd/moombox turns a sidecar.Health into this message, and nothing else does.
// The `case` in app_update.go is the only thing connecting the two halves, so
// it is pinned by execution rather than by reading the source.
//
// Mutant this kills: the app_update case dropped (or inverted) → the bar's
// flag stays false and the alert never appears.
func TestSidecarStatusMsgReachesTheBar(t *testing.T) {
	a := NewApp()

	a.Update(SidecarStatusMsg{Healthy: false})
	if !a.statusBar.sidecarDown {
		t.Error("SidecarStatusMsg{Healthy:false} did not raise the status bar flag")
	}

	a.Update(SidecarStatusMsg{Healthy: true})
	if a.statusBar.sidecarDown {
		t.Error("SidecarStatusMsg{Healthy:true} did not clear the status bar flag")
	}
}

// TestSetSidecarDownSeedsTheBarBeforeRun pins the OTHER half of the wiring:
// the state that already exists when the TUI starts.
//
// cmd/moombox publishes the sidecar's health during initServices — on a failed
// first start, and again from the supervisor within microseconds — long before
// main reaches runTUI. SubscribeHealth calls back immediately with that
// snapshot, but Send is a documented no-op until tui.Run stores the program,
// so that callback is thrown away: in the dominant case (a sidecar that cannot
// start at all) the only later publish is the Healthy:true of a restart that
// never happens, and the bar would stay quiet forever. The first assertion
// below is that no-op, asserted rather than assumed, because it is the whole
// reason the seeding setter exists.
//
// Mutants this kills:
//   - SetSidecarDown made a no-op (the seed dropped) → the bar stays quiet
//   - SetSidecarDown writing the raw bool instead of the down-ness → the
//     healthy case draws the alert
func TestSetSidecarDownSeedsTheBarBeforeRun(t *testing.T) {
	a := NewApp()

	a.Send(SidecarStatusMsg{Healthy: false})
	if a.statusBar.sidecarDown {
		t.Fatal("Send before Run reached the model — if that ever becomes true, this seeding path is unnecessary and the subscription alone would do")
	}

	a.SetSidecarDown(true)
	a.statusBar.SetWidth(200)
	if got := stripANSI(a.statusBar.View()); !strings.Contains(got, "SIDECAR DOWN") {
		t.Errorf("a sidecar-down seeded before Run never reached the bar: %q", got)
	}

	a.SetSidecarDown(false)
	if got := stripANSI(a.statusBar.View()); strings.Contains(got, "SIDECAR DOWN") {
		t.Errorf("seeding a healthy sidecar drew the alert anyway: %q", got)
	}
}
