package routes

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
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

// toggleOf is the body the dashboard's enable switch posts for a card:
// the channel as GET /api/config sent it, the new enabled, the edit mark.
func toggleOf(t *testing.T, ch config.ChannelConfig, enabled bool) map[string]any {
	t.Helper()
	raw, err := json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	body["enabled"] = enabled
	body["edit"] = true
	return body
}

// TestChannelEditMatchesThePostedID: a marked edit names its entry by the ID
// it posts, the stored one, before that ID is normalised. A config written
// before the normaliser holds cards stored as "@SomeHandle" or as a channel
// URL. Matched by the resolved ID, the dashboard's toggle of such a card
// appended a second, disabled entry under that ID and left the card on, or,
// when the operator had added the channel again by its UC ID, replaced that
// working entry: terms, output directory and overrides gone, disabled.
// Now the legacy card takes the resolved ID in place; when another entry
// holds it, the edit is a 409 and both entries stay as they were. A post
// naming an ID no entry has any more (a stale page after the migration)
// still finds the channel by its resolved ID.
//
// Mutants killed: matching the edit by the normalised ID only (the lone
// card gains a twin; the working entry is rewritten); dropping the 409 for
// a resolved ID another entry holds (two entries under one ID, a 500);
// dropping the resolved-ID fallback for an edit (the stale post appends a
// duplicate, a 500).
func TestChannelEditMatchesThePostedID(t *testing.T) {
	stubHandleLookups(t)
	two := 2
	working := config.ChannelConfig{
		ID: handleChannelID, Name: "Working", Terms: config.ChannelTerms{Simple: "(?i)karaoke"},
		OutputDirectory: "D:/special", ArchiveSlots: &two,
	}
	urlCard := config.ChannelConfig{ID: "https://www.youtube.com/channel/" + handleChannelID, Name: "Legacy URL"}
	handleCard := config.ChannelConfig{ID: "@SomeHandle", Name: "Legacy handle"}

	for _, legacy := range []config.ChannelConfig{handleCard, urlCard} {
		t.Run("lone "+legacy.Name, func(t *testing.T) {
			f := newChannelRoutesFixture(t)
			if err := f.store.Update(func(c *config.MoomboxConfig) { c.Channels = []config.ChannelConfig{legacy} }); err != nil {
				t.Fatal(err)
			}
			if rec := postChannel(t, f.router, toggleOf(t, legacy, false)); rec.Code != http.StatusOK {
				t.Fatalf("toggle: %d (body %s)", rec.Code, rec.Body.String())
			}
			got := storedChannels(f.store)
			if len(got) != 1 || got[0].ID != handleChannelID || got[0].Name != legacy.Name || got[0].IsEnabled() {
				t.Errorf("stored %+v, want the one card disabled under its resolved ID", got)
			}
		})
		t.Run(legacy.Name+" beside its working entry", func(t *testing.T) {
			f := newChannelRoutesFixture(t)
			seeded := []config.ChannelConfig{legacy, working}
			if err := f.store.Update(func(c *config.MoomboxConfig) { c.Channels = slices.Clone(seeded) }); err != nil {
				t.Fatal(err)
			}
			rec := postChannel(t, f.router, toggleOf(t, legacy, false))
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), handleChannelID) {
				t.Errorf("toggle: %d (body %s), want a 409 naming %s", rec.Code, rec.Body.String(), handleChannelID)
			}
			if got := storedChannels(f.store); !reflect.DeepEqual(got, seeded) {
				t.Errorf("stored %+v, want both entries as they were", got)
			}
		})
	}

	t.Run("stale post after the migration", func(t *testing.T) {
		f := newChannelRoutesFixture(t)
		if err := f.store.Update(func(c *config.MoomboxConfig) { c.Channels = []config.ChannelConfig{working} }); err != nil {
			t.Fatal(err)
		}
		if rec := postChannel(t, f.router, toggleOf(t, handleCard, false)); rec.Code != http.StatusOK {
			t.Fatalf("toggle: %d (body %s)", rec.Code, rec.Body.String())
		}
		if got := storedChannels(f.store); len(got) != 1 || got[0].ID != handleChannelID || got[0].IsEnabled() {
			t.Errorf("stored %+v, want the one channel, disabled", got)
		}
	})
}
