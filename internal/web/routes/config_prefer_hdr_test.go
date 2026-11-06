package routes

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestApplyConfigUpdatesWritesPreferHDR: the dashboard's Prefer HDR switch
// reaches the config (D13). Without the arm a save would drop it.
//
// Mutant: the prefer_hdr arm removed from applyConfigUpdates.
func TestApplyConfigUpdatesWritesPreferHDR(t *testing.T) {
	cfg := config.Defaults()
	applyConfigUpdates(cfg, map[string]any{"downloader": map[string]any{"prefer_hdr": false}})
	if cfg.Downloader.PreferHDR {
		t.Error("PreferHDR = true after a PUT of false")
	}
}
