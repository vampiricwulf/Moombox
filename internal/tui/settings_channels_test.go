package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

func intPtr(v int) *int { return &v }

// fullChannel is a channel carrying every field the TUI editor does NOT
// show, plus named (map) terms. Any of these vanishing after an edit is
// the data-loss bug this file exists to pin.
func fullChannel() config.ChannelConfig {
	enabled := true
	return config.ChannelConfig{
		ID:                    "UC123",
		Name:                  "Old name",
		Platform:              "youtube",
		Enabled:               &enabled,
		Terms:                 config.ChannelTerms{IsMap: true, Named: map[string]string{"stream": "(?i)karaoke", "vod": "(?i)vod"}},
		NumDescLookbehind:     intPtr(5),
		OutputDirectory:       "D:/special",
		IncludeNonLiveContent: true,
		ArchiveWindowDays:     intPtr(7),
		ArchiveSlots:          intPtr(2),
		QualityPreference:     "720p",
	}
}

// TestValuesToChannelPreservesUnshownFields: editing only the display name
// keeps the four hidden fields and the named terms verbatim.
func TestValuesToChannelPreservesUnshownFields(t *testing.T) {
	existing := fullChannel()
	vals := channelToValues(existing)
	vals["name"] = "New name"

	got := valuesToChannel(vals, &existing)

	if got.Name != "New name" {
		t.Errorf("Name = %q, want New name", got.Name)
	}
	if got.NumDescLookbehind == nil || *got.NumDescLookbehind != 5 {
		t.Errorf("NumDescLookbehind = %v, want 5", got.NumDescLookbehind)
	}
	if got.OutputDirectory != "D:/special" {
		t.Errorf("OutputDirectory = %q, want D:/special", got.OutputDirectory)
	}
	if got.ArchiveWindowDays == nil || *got.ArchiveWindowDays != 7 {
		t.Errorf("ArchiveWindowDays = %v, want 7", got.ArchiveWindowDays)
	}
	if got.ArchiveSlots == nil || *got.ArchiveSlots != 2 {
		t.Errorf("ArchiveSlots = %v, want 2", got.ArchiveSlots)
	}
	if !got.Terms.IsMap || got.Terms.Named["vod"] != "(?i)vod" {
		t.Errorf("Terms = %+v, want the named map preserved", got.Terms)
	}
	if !got.IncludeNonLiveContent || got.QualityPreference != "720p" {
		t.Errorf("shown fields drifted: include=%v quality=%q", got.IncludeNonLiveContent, got.QualityPreference)
	}
}

// TestValuesToChannelEditedTermsReplaceNamedMap: typing a new pattern
// replaces the whole terms value with the simple form (the editor shows one
// string, so that is what the operator meant).
func TestValuesToChannelEditedTermsReplaceNamedMap(t *testing.T) {
	existing := fullChannel()
	vals := channelToValues(existing)
	vals["terms"] = "(?i)new"

	got := valuesToChannel(vals, &existing)
	if got.Terms.IsMap || got.Terms.Simple != "(?i)new" {
		t.Errorf("Terms = %+v, want Simple (?i)new", got.Terms)
	}
}

// TestValuesToChannelClearedFieldsClear: clearing terms, switching the
// uploads toggle off and quality back to best must clear, not keep, the
// existing values (a copied-then-conditionally-set field would keep them).
func TestValuesToChannelClearedFieldsClear(t *testing.T) {
	existing := fullChannel()
	existing.Terms = config.ChannelTerms{Simple: "(?i)old"}
	vals := channelToValues(existing)
	vals["terms"] = ""
	vals["include_non_live"] = "No"
	vals["quality_preference"] = "best"
	vals["enabled"] = "No"

	got := valuesToChannel(vals, &existing)
	if got.Terms.Simple != "" || got.Terms.IsMap {
		t.Errorf("Terms = %+v, want empty", got.Terms)
	}
	if got.IncludeNonLiveContent {
		t.Error("IncludeNonLiveContent still true after toggling No")
	}
	if got.QualityPreference != "" {
		t.Errorf("QualityPreference = %q, want empty for best", got.QualityPreference)
	}
	if got.Enabled == nil || *got.Enabled {
		t.Errorf("Enabled = %v, want false", got.Enabled)
	}
}

// TestValuesToChannelNewChannel: nil existing behaves exactly like before —
// a fresh ChannelConfig from the form values only.
func TestValuesToChannelNewChannel(t *testing.T) {
	vals := map[string]string{
		"id": " UC999 ", "name": "N", "platform": "youtube", "enabled": "Yes",
		"terms": "(?i)x", "include_non_live": "Yes", "quality_preference": "best",
	}
	got := valuesToChannel(vals, nil)
	if got.ID != "UC999" || got.Name != "N" || got.Platform != "youtube" {
		t.Errorf("identity fields = %q %q %q", got.ID, got.Name, got.Platform)
	}
	if got.Enabled == nil || !*got.Enabled || !got.IncludeNonLiveContent || got.Terms.Simple != "(?i)x" || got.QualityPreference != "" {
		t.Errorf("form fields drifted: %+v", got)
	}
	if got.NumDescLookbehind != nil || got.OutputDirectory != "" || got.ArchiveWindowDays != nil || got.ArchiveSlots != nil {
		t.Errorf("hidden fields must be zero for a new channel: %+v", got)
	}
}

// TestSaveCurrentChannelPassesExisting: the settings overlay's save path hands
// the channel being edited to valuesToChannel (an edit) but nil for an add.
func TestSaveCurrentChannelPassesExisting(t *testing.T) {
	m := NewSettingsModel()
	m.channels = []config.ChannelConfig{fullChannel()}
	m.channelIndex = 0
	m.channelEditValues = channelToValues(m.channels[0])
	m.channelEditValues["name"] = "Renamed"
	m.saveCurrentChannel()
	if got := m.channels[0]; got.Name != "Renamed" || got.ArchiveSlots == nil || *got.ArchiveSlots != 2 {
		t.Errorf("edit lost fields: %+v", got)
	}

	m.channelIndex = len(m.channels) // add sentinel
	m.channelEditValues = map[string]string{"id": "UC2", "name": "", "platform": "youtube", "enabled": "Yes", "terms": "", "include_non_live": "No", "quality_preference": "best"}
	m.saveCurrentChannel()
	if len(m.channels) != 2 || m.channels[1].ID != "UC2" || m.channels[1].ArchiveSlots != nil {
		t.Errorf("add produced %+v", m.channels)
	}
}
