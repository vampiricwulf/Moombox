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
