package config

import (
	"maps"
	"slices"
	"testing"
)

// ChannelOverrideErrors is the web writers' copy of Validate's per-channel
// rules, so the two must refuse exactly the same entries: a value one accepts
// and the other refuses is either a bare 500 from Save (the bug this exists
// for) or a 400 on a value the file loader would have kept.
//
// Mutant: shifting either bound by one in channelWindowInRange or
// channelSlotsInRange, or dropping one of the three checks.
func TestChannelOverrideErrorsMatchesValidate(t *testing.T) {
	ip := func(v int) *int { return &v }
	for _, tc := range []struct {
		name string
		ch   ChannelConfig
		want []string
	}{
		{"none", ChannelConfig{}, nil},
		{"all at their bounds", ChannelConfig{QualityPreference: "720p", ArchiveWindowDays: ip(1), ArchiveSlots: ip(100)}, nil},
		{"window max", ChannelConfig{ArchiveWindowDays: ip(3650)}, nil},
		{"slots min", ChannelConfig{ArchiveSlots: ip(1)}, nil},
		{"window zero", ChannelConfig{ArchiveWindowDays: ip(0)}, []string{"archive_window_days"}},
		{"window past max", ChannelConfig{ArchiveWindowDays: ip(3651)}, []string{"archive_window_days"}},
		{"slots zero", ChannelConfig{ArchiveSlots: ip(0)}, []string{"archive_slots"}},
		{"slots past max", ChannelConfig{ArchiveSlots: ip(101)}, []string{"archive_slots"}},
		{"unknown quality", ChannelConfig{QualityPreference: "1080i"}, []string{"quality_preference"}},
		{"all three", ChannelConfig{QualityPreference: "x", ArchiveWindowDays: ip(-1), ArchiveSlots: ip(-1)},
			[]string{"archive_slots", "archive_window_days", "quality_preference"}},
	} {
		got := slices.Sorted(maps.Keys(ChannelOverrideErrors(tc.ch)))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: fields %v, want %v", tc.name, got, tc.want)
		}

		cfg := Defaults()
		ch := tc.ch
		ch.ID, ch.Name = "UCx", "x"
		cfg.Channels = []ChannelConfig{ch}
		if refused := len(Validate(cfg)) > 0; refused != (len(tc.want) > 0) {
			t.Errorf("%s: Validate refused = %v, ChannelOverrideErrors refused = %v", tc.name, refused, len(tc.want) > 0)
		}
	}
}
