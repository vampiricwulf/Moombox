package main

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// storeWithChannels builds a config store carrying the global
// monitors.archive_slots default plus the given channel list — the only two
// inputs archiveSlotsResolver reads.
func storeWithChannels(t *testing.T, globalSlots int, channels []config.ChannelConfig) *config.Store {
	t.Helper()
	cfg := config.Defaults()
	cfg.Monitors.ArchiveSlots = globalSlots
	cfg.Channels = channels
	return config.NewStore(cfg, "")
}

// TestArchiveSlotsResolver_DisabledChannelGetsNone is MON-3 / owner decision
// O-J: disabling a channel PAUSES its queued backlog. Every discovery path
// already treats disabling as a pause (the three monitors filter on
// ch.Enabled; backfill.go's comment calls it "a pause, not a removal") while
// the resolver kept handing out slots, so the operator watched a channel they
// had just disabled keep starting downloads.
//
// Mutants:
//   - drop the IsEnabled() check -> the disabled channel still gets its slots.
//   - return 0 for an ABSENT channel too -> a removed channel's leftover
//     Queued rows would be stranded with no way out (row 3).
func TestArchiveSlotsResolver_DisabledChannelGetsNone(t *testing.T) {
	no, yes := false, true
	five := 5
	store := storeWithChannels(t, 3, []config.ChannelConfig{
		{ID: "UC_on", Platform: "youtube", Enabled: &yes},
		{ID: "UC_off", Platform: "youtube", Enabled: &no, ArchiveSlots: &five},
	})
	resolve := archiveSlotsResolver(store)

	if got := resolve("UC_on"); got != 3 {
		t.Errorf("enabled channel = %d, want 3 (the monitors.archive_slots default)", got)
	}
	if got := resolve("UC_off"); got != 0 {
		t.Errorf("disabled channel = %d, want 0 — its backlog must rest until it is re-enabled (O-J); the per-channel override must not rescue it either", got)
	}
	if got := resolve("UC_gone"); got != 3 {
		t.Errorf("channel with no config entry = %d, want 3 — a removed channel's leftover Queued rows must still drain", got)
	}
}

// TestArchiveSlotsResolver_OverrideAndDefault keeps the pre-existing behaviour
// honest alongside the new gate: an enabled channel's own archive_slots wins
// over the global default, and a non-positive override falls back to it rather
// than pausing the channel by accident (0 is what "unset" decodes to in TOML,
// and 0 slots is the pause O-J reserves for `enabled = false`).
func TestArchiveSlotsResolver_OverrideAndDefault(t *testing.T) {
	yes := true
	seven, zero := 7, 0
	store := storeWithChannels(t, 3, []config.ChannelConfig{
		{ID: "UC_more", Platform: "youtube", Enabled: &yes, ArchiveSlots: &seven},
		{ID: "UC_zero", Platform: "youtube", ArchiveSlots: &zero},
	})
	resolve := archiveSlotsResolver(store)

	if got := resolve("UC_more"); got != 7 {
		t.Errorf("per-channel override = %d, want 7", got)
	}
	if got := resolve("UC_zero"); got != 3 {
		t.Errorf("non-positive override = %d, want the global default 3 — only `enabled = false` pauses a channel", got)
	}
}
