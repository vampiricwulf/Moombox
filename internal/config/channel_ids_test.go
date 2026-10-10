package config

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Validate refuses a channel ID with surrounding whitespace and one a
// channel already has, compared case-insensitively (W25-15's backstop: every
// writer refuses both, and Save is the last of them). Normalize, which Load
// runs, trims the ID and drops the later duplicate — the entry every lookup
// by ID already passed over — into a NEW slice, so the array a Store
// rollback or a Snapshot reader holds is never written through.
//
// Mutants killed: dropping the duplicate check; comparing case-sensitively;
// dropping the trim check; compacting cfg.Channels in place.
func TestValidateRefusesPaddedAndDuplicateChannelIDs(t *testing.T) {
	orig := []ChannelConfig{
		{ID: "UCabcdefghijklmnopqrstuv", Name: "first"},
		{ID: " shroud ", Platform: "twitch"},
		{ID: "ucABCDEFGHIJKLMNOPQRSTUV", Name: "case twin"},
		{ID: "Shroud", Platform: "twitch", Name: "case twin"},
		{ID: "UCother0000000000000000x"},
	}
	cfg := Defaults()
	cfg.Channels = slices.Clone(orig)
	shared := cfg.Channels

	errs := Validate(cfg)
	var msgs []string
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	joined := strings.Join(msgs, "\n")
	for _, want := range []string{
		`channels[1].id " shroud " has surrounding whitespace`,
		`channels[2].id "ucABCDEFGHIJKLMNOPQRSTUV" duplicates channels[0]`,
		`channels[3].id "Shroud" duplicates channels[1]`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Validate missed %q; got:\n%s", want, joined)
		}
	}
	if len(errs) != 3 {
		t.Errorf("Validate: %d errors, want 3:\n%s", len(errs), joined)
	}

	Normalize(cfg)
	var ids []string
	for _, ch := range cfg.Channels {
		ids = append(ids, ch.ID)
	}
	if want := []string{"UCabcdefghijklmnopqrstuv", "shroud", "UCother0000000000000000x"}; !slices.Equal(ids, want) {
		t.Errorf("Normalize kept %q, want %q", ids, want)
	}
	if !reflect.DeepEqual(shared, orig) {
		t.Errorf("Normalize wrote through the old slice: %+v", shared)
	}
	if errs := Validate(cfg); len(errs) != 0 {
		t.Errorf("Validate after Normalize: %v", errs)
	}
}

// Save refuses a config carrying a duplicate channel ID, so a writer that
// let one through fails loudly rather than storing it.
func TestStoreUpdateRefusesDuplicateChannelID(t *testing.T) {
	s := NewStore(Defaults(), filepath.Join(t.TempDir(), "config.toml"))
	err := s.Update(func(c *MoomboxConfig) {
		c.Channels = []ChannelConfig{{ID: "shroud"}, {ID: "SHROUD"}}
	})
	if err == nil || !strings.Contains(err.Error(), "duplicates channels[0]") {
		t.Fatalf("Update with a duplicate ID: %v, want the duplicate refused", err)
	}
	var n int
	s.Read(func(c *MoomboxConfig) { n = len(c.Channels) })
	if n != 0 {
		t.Errorf("a refused Update left %d channels", n)
	}
}
