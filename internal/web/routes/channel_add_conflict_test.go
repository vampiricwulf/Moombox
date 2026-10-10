package routes

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestChannelAddRefusesAConfiguredChannel pins W25-11 on the server: a POST
// /api/config/channels naming a configured channel — by its ID in any case,
// or by a handle that resolves to it — is an add, and refused 409 with the
// stored entry untouched, unless the request carries the edit mark. The
// dashboard's Add Channel posted exactly {id, enabled: true} over such a
// channel and the upsert replaced it whole: terms, output directory and
// overrides gone behind "Channel added". A marked edit replaces the entry;
// a marked edit of a channel removed meanwhile adds it back.
//
// Mutants killed: dropping the !edit refusal (the bare post wipes the
// entry); reading the mark as always true (the same); never reading it
// (the marked edit is refused).
func TestChannelAddRefusesAConfiguredChannel(t *testing.T) {
	stubHandleLookups(t)
	f := newChannelRoutesFixture(t)
	two, thirty := 2, 30
	stored := config.ChannelConfig{
		ID: handleChannelID, Name: "Kept", Terms: config.ChannelTerms{Simple: "(?i)karaoke"},
		OutputDirectory: "D:/special", ArchiveSlots: &two, ArchiveWindowDays: &thirty,
		IncludeNonLiveContent: true, QualityPreference: "720p",
	}
	if err := f.store.Update(func(c *config.MoomboxConfig) { c.Channels = []config.ChannelConfig{stored} }); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, id := range []string{handleChannelID, "uchandlehandlehandlehand", "@SomeHandle"} {
		rec := postChannel(t, f.router, map[string]any{"id": id, "enabled": true})
		if rec.Code != http.StatusConflict {
			t.Errorf("add of %q over a configured channel: %d, want 409 (body %s)", id, rec.Code, rec.Body.String())
		}
	}
	if got := storedChannels(f.store); len(got) != 1 || !reflect.DeepEqual(got[0], stored) {
		t.Fatalf("a refused add changed the stored channel: %+v", got)
	}
	if n := f.channelChange.Load(); n != 0 {
		t.Errorf("a refused add kicked the monitors %d times", n)
	}

	rec := postChannel(t, f.router, map[string]any{"id": handleChannelID, "name": "Edited", "edit": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("marked edit: %d (body %s)", rec.Code, rec.Body.String())
	}
	if got := storedChannels(f.store); len(got) != 1 || got[0].Name != "Edited" {
		t.Errorf("marked edit stored %+v, want the one entry renamed", got)
	}

	// Removed meanwhile (another tab, the TUI): a marked edit adds it back.
	if err := f.store.Update(func(c *config.MoomboxConfig) { c.Channels = nil }); err != nil {
		t.Fatal(err)
	}
	if rec := postChannel(t, f.router, map[string]any{"id": handleChannelID, "name": "Back", "edit": true}); rec.Code != http.StatusOK {
		t.Fatalf("marked edit of a removed channel: %d (body %s)", rec.Code, rec.Body.String())
	}
	if got := storedChannels(f.store); len(got) != 1 || got[0].Name != "Back" {
		t.Errorf("stored %+v, want the edited channel back", got)
	}
}
