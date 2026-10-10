package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// The status bar's platform indicators followed only an auth transition, so
// switching a platform off in the settings overlay left its indicator up
// indefinitely. Closing the overlay now re-reads them as it does the
// hide-finished threshold.
//
// Mutant: afterSettingsClose not touching the status bar — Twitch stays shown.
func TestClosingSettingsReappliesTheActivePlatforms(t *testing.T) {
	cfg := config.Defaults()
	cfg.Cookies.ActivePlatforms = []string{"youtube", "twitch"}
	store := config.NewStore(cfg, "")

	a := NewApp()
	a.SetConfigStore(store)
	a.SetConfig(cfg)
	a.statusBar.SetActivePlatforms(true, true)

	if err := store.Update(func(c *config.MoomboxConfig) {
		c.Cookies.ActivePlatforms = []string{"youtube"}
	}); err != nil {
		t.Fatalf("store.Update: %v", err)
	}
	a.afterSettingsClose()
	if !a.statusBar.ytActive || a.statusBar.twActive {
		t.Errorf("status bar active = (yt %v, tw %v), want (true, false)", a.statusBar.ytActive, a.statusBar.twActive)
	}
}
