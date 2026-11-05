package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestPreferHDRRoundTripsThroughTheSettingsOverlay: the TUI's Prefer HDR
// toggle shows the config's value and writes the operator's back (D13), the
// same field the dashboard's switch writes.
//
// Mutants: the load line reading Prefer60fps (shows "Yes"); the write arm
// writing Prefer60fps (PreferHDR stays false).
func TestPreferHDRRoundTripsThroughTheSettingsOverlay(t *testing.T) {
	cfg := config.Defaults()
	cfg.Downloader.PreferHDR = false
	v := map[string]string{}
	loadSettingsValues(v, cfg)
	if v["prefer_hdr"] != "No" {
		t.Fatalf("prefer_hdr shows %q, want No", v["prefer_hdr"])
	}
	v["prefer_hdr"] = "Yes"
	if !writeSettingsField(cfg, "prefer_hdr", v, settingsParsed{flex: map[string]float64{}}) || !cfg.Downloader.PreferHDR {
		t.Errorf("PreferHDR = %v after writing Yes", cfg.Downloader.PreferHDR)
	}
}
