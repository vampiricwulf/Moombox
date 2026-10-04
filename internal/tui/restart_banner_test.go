package tui

import (
	"strings"
	"testing"
)

// TestRestartBannerNamesTheRestartChord: the banner used to send the operator
// to "` then Save & Restart", a button the settings overlay has never had (its
// buttons are Save & Return / Return Without Saving / Return). The affordance
// that exists is the R P chord, and the banner's wording is checked against
// the chord table so the two cannot drift apart again.
func TestRestartBannerNamesTheRestartChord(t *testing.T) {
	app := NewApp()
	app.OnRestart = func() {}
	item := menuItem(app, "R P")
	if item == nil || item.Label != "Restart Program" {
		t.Fatalf("premise lost: R P is not the Restart Program chord (%+v)", item)
	}

	got := stripANSI(restartBanner(200))
	if !strings.Contains(got, "R P") {
		t.Errorf("the banner does not name the R P chord: %q", got)
	}
	if strings.Contains(got, "Save & Restart") {
		t.Errorf("the banner still names a button that does not exist: %q", got)
	}
}
