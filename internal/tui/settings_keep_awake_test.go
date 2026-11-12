package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestKeepAwakeRoundTripsThroughTheSettingsOverlay: the TUI's keep-awake
// toggle shows downloader.keep_awake and writes it back.
//
// Mutants: the load line dropped (the field shows empty); the write case
// dropped or writing another field (KeepAwake stays false).
func TestKeepAwakeRoundTripsThroughTheSettingsOverlay(t *testing.T) {
	cfg := config.Defaults()
	cfg.Downloader.KeepAwake = false
	v := map[string]string{}
	loadSettingsValues(v, cfg)
	if v["keep_awake"] != "No" {
		t.Fatalf("keep_awake shows %q, want No", v["keep_awake"])
	}
	v["keep_awake"] = "Yes"
	if !writeSettingsField(cfg, "keep_awake", v, settingsParsed{flex: map[string]float64{}}) || !cfg.Downloader.KeepAwake {
		t.Errorf("KeepAwake = %v after writing Yes", cfg.Downloader.KeepAwake)
	}
}
