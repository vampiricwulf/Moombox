package main

import (
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestLiveChannelEnabledReadsTheLiveConfig pins the backfill worker's
// ChannelEnabled wiring (W25-16): a queued scan is checked against the
// store's CURRENT channel list, so a channel disabled — or removed — after
// the sweep that queued it is not scanned.
//
// Mutants killed: returning IsEnabled's opposite (every row flips);
// defaulting a missing channel to enabled (the removed row reads true);
// reading a snapshot taken when the closure was built (the disable made
// afterwards is missed).
func TestLiveChannelEnabledReadsTheLiveConfig(t *testing.T) {
	off := false
	cfg := config.Defaults()
	cfg.Channels = []config.ChannelConfig{{ID: "UCon"}, {ID: "UCoff", Enabled: &off}, {ID: "UClater"}}
	store := config.NewStore(cfg, filepath.Join(t.TempDir(), "config.toml"))
	enabled := liveChannelEnabled(store)

	// Whole-slice replacement, as every channel writer does (copy-on-write).
	if err := store.Update(func(c *config.MoomboxConfig) {
		c.Channels = []config.ChannelConfig{{ID: "UCon"}, {ID: "UCoff", Enabled: &off}, {ID: "UClater", Enabled: &off}}
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	for id, want := range map[string]bool{"UCon": true, "UCoff": false, "UClater": false, "UCgone": false} {
		if got := enabled(id); got != want {
			t.Errorf("ChannelEnabled(%s) = %v, want %v", id, got, want)
		}
	}
}
