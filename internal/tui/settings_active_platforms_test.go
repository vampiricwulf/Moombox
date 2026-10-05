package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// openActivePlatforms opens the settings form on a config whose platform
// indicators come from inference (no override), with both platforms inferred
// active from verified cookies.
func openActivePlatforms(t *testing.T) (*SettingsModel, *config.MoomboxConfig) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Cookies.Platforms = []string{"youtube", "twitch"}
	cfg.Cookies.ActivePlatforms = nil
	m := NewSettingsModel()
	m.configStore = config.NewStore(cfg, "")
	m.cfg = cfg
	m.Open(cfg)
	if m.values["active_youtube"] != "Yes" || m.values["active_twitch"] != "Yes" {
		t.Fatalf("toggles loaded as %q/%q, want the inferred Yes/Yes",
			m.values["active_youtube"], m.values["active_twitch"])
	}
	return m, cfg
}

// TestActivePlatformsUneditedStayInferred: the toggles show an inferred
// answer, and a save that never touched them must not freeze it into an
// override — a channel added later would never light its platform.
//
// MUTANT: drop the edited-toggle gate in applyValues — the override is
// written as [youtube twitch] and the assertion names it.
func TestActivePlatformsUneditedStayInferred(t *testing.T) {
	m, cfg := openActivePlatforms(t)
	m.applyValues()
	if m.status == saveError {
		t.Fatalf("applyValues refused a valid config: %s", m.errorMsg)
	}
	if cfg.Cookies.ActivePlatforms != nil {
		t.Errorf("ActivePlatforms = %#v after a save that left both toggles alone, want nil (no override)",
			cfg.Cookies.ActivePlatforms)
	}
}

// TestActivePlatformsBothOffIsAnOverride: turning both toggles off records
// an EMPTY override, which GetActivePlatforms honours instead of falling back
// to the inferred answer — before, both-off was stored as no override and the
// indicators came straight back.
//
// MUTANT: build the list from a nil slice — both-off stores nil, and
// GetActivePlatforms reports the inferred youtube/twitch again.
func TestActivePlatformsBothOffIsAnOverride(t *testing.T) {
	m, cfg := openActivePlatforms(t)
	m.values["active_youtube"] = "No"
	m.values["active_twitch"] = "No"
	m.applyValues()
	if m.status == saveError {
		t.Fatalf("applyValues refused a valid config: %s", m.errorMsg)
	}
	if cfg.Cookies.ActivePlatforms == nil || len(cfg.Cookies.ActivePlatforms) != 0 {
		t.Fatalf("ActivePlatforms = %#v, want an empty, non-nil override", cfg.Cookies.ActivePlatforms)
	}
	if yt, tw := config.GetActivePlatforms(cfg); yt || tw {
		t.Errorf("GetActivePlatforms = %v/%v after both were turned off, want false/false", yt, tw)
	}
}
