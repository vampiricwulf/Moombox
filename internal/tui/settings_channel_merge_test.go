package tui

import (
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

const (
	mergeA = "UCaaaaaaaaaaaaaaaaaaaaaa"
	mergeB = "UCbbbbbbbbbbbbbbbbbbbbbb"
	mergeC = "UCcccccccccccccccccccccc"
)

// settingsOverStore opens the Settings overlay over a store seeded with
// channel A, wired as runTUI wires it: the overlay holds the store's own
// config, and OnSave is OnSaveConfig's config.Save under the store lock.
func settingsOverStore(t *testing.T) (*SettingsModel, *config.Store) {
	t.Helper()
	store := config.NewStore(config.Defaults(), filepath.Join(t.TempDir(), "config.toml"))
	if err := store.Update(func(c *config.MoomboxConfig) {
		c.Channels = []config.ChannelConfig{{ID: mergeA, Name: "A"}}
	}); err != nil {
		t.Fatal(err)
	}
	m := NewSettingsModel()
	m.configStore = store
	m.OnSave = func(c *config.MoomboxConfig) error {
		mu := store.RWMutex()
		mu.Lock()
		defer mu.Unlock()
		return config.Save(c, store.SavePath())
	}
	m.Open(store.Config())
	return m, store
}

// dashboard changes the channel list as the channel routes do while the
// overlay is open: a whole-slice replacement under the store lock.
func dashboard(t *testing.T, store *config.Store, fn func([]config.ChannelConfig) []config.ChannelConfig) {
	t.Helper()
	if err := store.Update(func(c *config.MoomboxConfig) {
		c.Channels = fn(slices.Clone(c.Channels))
	}); err != nil {
		t.Fatal(err)
	}
}

// saveLogLevel saves the overlay with only the log level changed.
func saveLogLevel(t *testing.T, m *SettingsModel) {
	t.Helper()
	m.values["log_level"] = "debug"
	m.recheckDirty()
	m.saveAndClose()
	if m.status != saveSaved {
		t.Fatalf("save: status %v, error %q", m.status, m.errorMsg)
	}
}

// liveAndOnDisk returns the store's channel list and the one config.toml
// holds, which must agree.
func liveAndOnDisk(t *testing.T, store *config.Store) []config.ChannelConfig {
	t.Helper()
	var live []config.ChannelConfig
	store.Read(func(c *config.MoomboxConfig) { live = slices.Clone(c.Channels) })
	disk, err := config.Load(store.SavePath())
	if err != nil {
		t.Fatal(err)
	}
	if len(disk.Channels) != len(live) {
		t.Fatalf("store holds %d channels, config.toml %d", len(live), len(disk.Channels))
	}
	return live
}

func channelIDs(chs []config.ChannelConfig) []string {
	var ids []string
	for _, ch := range chs {
		ids = append(ids, ch.ID)
	}
	return ids
}

func boolPtr(b bool) *bool { return &b }

// TestTUISaveKeepsDashboardChannelChanges pins W25-10: a TUI Settings save
// whose channel editor was not touched leaves the live channel list exactly
// as the dashboard left it while the overlay was open. applyValues wrote
// the list Open had copied back whole, so saving only the log level dropped
// a channel the dashboard had added (and the sweep then pruned its jobs and
// feed history as departed), re-enabled one it had disabled, and restored
// one it had removed. Opening a channel's editor and pressing Enter without
// changing a field is still not touching it.
//
// Mutants killed: applyValues writing m.channels back unconditionally (all
// three); mergeChannelEdits treating every entry as edited (the removal
// undone); saveCurrentChannel storing valuesToChannel's spelled-out
// defaults for a no-change Enter (the removal undone by the no-op edit,
// which then reads as an edit of a channel the dashboard removed).
func TestTUISaveKeepsDashboardChannelChanges(t *testing.T) {
	t.Run("add", func(t *testing.T) {
		m, store := settingsOverStore(t)
		dashboard(t, store, func(chs []config.ChannelConfig) []config.ChannelConfig {
			return append(chs, config.ChannelConfig{ID: mergeB, Name: "B (added on the web)"})
		})
		saveLogLevel(t, m)
		if got := channelIDs(liveAndOnDisk(t, store)); !slices.Equal(got, []string{mergeA, mergeB}) {
			t.Errorf("channels after the TUI save: %q, want A and the dashboard's B", got)
		}
	})
	t.Run("disable", func(t *testing.T) {
		m, store := settingsOverStore(t)
		dashboard(t, store, func(chs []config.ChannelConfig) []config.ChannelConfig {
			chs[0].Enabled = boolPtr(false)
			return chs
		})
		// Open A's editor and save it unchanged.
		m.switchSection(slices.IndexFunc(sections, func(s settingsSection) bool { return s.name == "Channels" }))
		m.handleChannelKey(keyEnter)
		m.handleChannelKey(keyEnter)
		saveLogLevel(t, m)
		if got := liveAndOnDisk(t, store); len(got) != 1 || got[0].IsEnabled() {
			t.Errorf("channels after the TUI save: %+v, want A still disabled", got)
		}
	})
	t.Run("remove", func(t *testing.T) {
		m, store := settingsOverStore(t)
		dashboard(t, store, func([]config.ChannelConfig) []config.ChannelConfig { return nil })
		// Open A's editor and save it unchanged.
		openChannelEditor(m)
		m.handleChannelKey(keyEnter)
		saveLogLevel(t, m)
		if got := liveAndOnDisk(t, store); len(got) != 0 {
			t.Errorf("channels after the TUI save: %q, want A still removed", channelIDs(got))
		}
	})
}

// TestTUISaveAppliesItsOwnChannelEdits: what the TUI editor did change is
// applied, by ID, to the list as it stands at save time — beside whatever
// the dashboard did meanwhile. An edit of a channel the dashboard removed
// adds it back (the operator saved it on purpose); a channel the TUI deleted
// is removed even if the dashboard edited it; after the save the editor
// shows the merged list.
//
// Mutants killed: mergeChannelEdits ignoring the editor's removals; never
// appending an entry live lacks (the add and the re-add lost); saveAndClose
// not re-syncing the editor's list.
func TestTUISaveAppliesItsOwnChannelEdits(t *testing.T) {
	t.Run("add beside a dashboard add", func(t *testing.T) {
		m, store := settingsOverStore(t)
		dashboard(t, store, func(chs []config.ChannelConfig) []config.ChannelConfig {
			return append(chs, config.ChannelConfig{ID: mergeB})
		})
		m.handleChannelKey("a")
		m.channelEditValues["id"] = mergeC
		m.handleChannelKey(keyEnter)
		m.saveAndClose()
		if got := channelIDs(liveAndOnDisk(t, store)); !slices.Equal(got, []string{mergeA, mergeB, mergeC}) {
			t.Errorf("channels: %q, want A, the dashboard's B, the TUI's C", got)
		}
		if got := channelIDs(m.channels); !slices.Equal(got, []string{mergeA, mergeB, mergeC}) {
			t.Errorf("the editor shows %q after the save, want the merged list", got)
		}
	})
	t.Run("edit of a channel the dashboard removed", func(t *testing.T) {
		m, store := settingsOverStore(t)
		dashboard(t, store, func([]config.ChannelConfig) []config.ChannelConfig {
			return []config.ChannelConfig{{ID: mergeB}}
		})
		m.handleChannelKey(keyEnter)
		m.channelEditValues["name"] = "A renamed in the TUI"
		m.handleChannelKey(keyEnter)
		m.saveAndClose()
		got := liveAndOnDisk(t, store)
		if !slices.Equal(channelIDs(got), []string{mergeB, mergeA}) || got[1].Name != "A renamed in the TUI" {
			t.Errorf("channels: %+v, want B and the TUI's edited A back", got)
		}
	})
	t.Run("delete of a channel the dashboard edited", func(t *testing.T) {
		m, store := settingsOverStore(t)
		dashboard(t, store, func(chs []config.ChannelConfig) []config.ChannelConfig {
			chs[0].Name = "A renamed on the web"
			return append(chs, config.ChannelConfig{ID: mergeB})
		})
		// The editor's state after a confirmed delete of A.
		m.channels = m.channels[1:]
		m.dirty, m.structDirty = true, true
		m.saveAndClose()
		if got := channelIDs(liveAndOnDisk(t, store)); !slices.Equal(got, []string{mergeB}) {
			t.Errorf("channels: %q, want only the dashboard's B", got)
		}
	})
}

// TestMergeChannelEditsMatchesIDsCaseInsensitively: the editor's edit of
// "shroud" is merged into the live entry the dashboard has since re-spelled
// "Shroud" rather than adding a second entry config.Validate would refuse,
// and keeps that spelling, which the editor did not change; an ID the
// editor re-spelled is written; a rename removes the old ID and adds the
// new one; an untouched editor returns live itself, unchanged.
//
// Mutant killed: comparing IDs case-sensitively (two entries).
func TestMergeChannelEditsMatchesIDsCaseInsensitively(t *testing.T) {
	base := []config.ChannelConfig{{ID: "shroud", Platform: "twitch"}}
	live := []config.ChannelConfig{{ID: "Shroud", Platform: "twitch", Name: "web"}, {ID: mergeB}}

	edited := []config.ChannelConfig{{ID: "shroud", Platform: "twitch", Name: "tui"}}
	got, changed := mergeChannelEdits(base, edited, live)
	if !changed || len(got) != 2 || got[0].ID != "Shroud" || got[0].Name != "tui" {
		t.Errorf("edit across a case change: %+v (changed %v), want the TUI's name on the dashboard's Shroud, and B", got, changed)
	}

	respelled := []config.ChannelConfig{{ID: "SHROUD", Platform: "twitch"}}
	got, _ = mergeChannelEdits(base, respelled, live)
	if len(got) != 2 || got[0].ID != "SHROUD" || got[0].Name != "web" {
		t.Errorf("the editor's re-spelling: %+v, want SHROUD keeping the dashboard's name, and B", got)
	}

	renamed := []config.ChannelConfig{{ID: "xqc", Platform: "twitch"}}
	got, _ = mergeChannelEdits(base, renamed, live)
	if ids := channelIDs(got); !slices.Equal(ids, []string{mergeB, "xqc"}) {
		t.Errorf("rename: %q, want B and xqc", ids)
	}

	got, changed = mergeChannelEdits(base, slices.Clone(base), live)
	if changed || len(got) != len(live) || &got[0] != &live[0] {
		t.Errorf("untouched editor: changed %v, %+v; want live itself", changed, got)
	}
	if live[0].Name != "web" {
		t.Errorf("the merge wrote through live: %+v", live)
	}
}

// openChannelEditor opens the Settings overlay's editor on channel A.
func openChannelEditor(m *SettingsModel) {
	m.switchSection(slices.IndexFunc(sections, func(s settingsSection) bool { return s.name == "Channels" }))
	m.handleChannelKey(keyEnter)
}

// TestTUISaveMergesAChannelFieldByField: a TUI edit of one field of a
// channel writes that field into the live entry and leaves every other
// field as it stands at save time. The save put the editor's whole entry —
// every other field as Open copied it — over the live one, so a TUI rename
// re-enabled a channel the dashboard had disabled while the overlay was
// open, or cleared the output directory set there. A field both sides
// changed takes the TUI's value: the operator saved it last.
//
// Mutants killed: mergeChannelEdits writing the editor's entry whole
// (the disable and the output directory reverted); mergeChannelFields
// writing every field rather than the changed ones (the same).
func TestTUISaveMergesAChannelFieldByField(t *testing.T) {
	t.Run("a dashboard disable survives a TUI rename", func(t *testing.T) {
		m, store := settingsOverStore(t)
		dashboard(t, store, func(chs []config.ChannelConfig) []config.ChannelConfig {
			chs[0].Enabled = boolPtr(false)
			return chs
		})
		openChannelEditor(m)
		m.channelEditValues["name"] = "A renamed in the TUI"
		m.handleChannelKey(keyEnter)
		m.saveAndClose()
		if m.status != saveSaved {
			t.Fatalf("save: %v %q", m.status, m.errorMsg)
		}
		got := liveAndOnDisk(t, store)
		if len(got) != 1 || got[0].Name != "A renamed in the TUI" || got[0].IsEnabled() {
			t.Errorf("channels: %+v, want A renamed and still disabled", got)
		}
	})
	t.Run("a dashboard output directory survives a TUI rename", func(t *testing.T) {
		m, store := settingsOverStore(t)
		dashboard(t, store, func(chs []config.ChannelConfig) []config.ChannelConfig {
			chs[0].OutputDirectory = "/archive/a"
			return chs
		})
		openChannelEditor(m)
		m.channelEditValues["name"] = "A renamed in the TUI"
		m.handleChannelKey(keyEnter)
		m.saveAndClose()
		got := liveAndOnDisk(t, store)
		if len(got) != 1 || got[0].Name != "A renamed in the TUI" || got[0].OutputDirectory != "/archive/a" {
			t.Errorf("channels: %+v, want A renamed, its output directory /archive/a", got)
		}
	})
	t.Run("a field both changed takes the TUI's value", func(t *testing.T) {
		m, store := settingsOverStore(t)
		dashboard(t, store, func(chs []config.ChannelConfig) []config.ChannelConfig {
			chs[0].OutputDirectory = "/archive/web"
			chs[0].Name = "A renamed on the web"
			return chs
		})
		openChannelEditor(m)
		m.channelEditValues["output_directory"] = "/archive/tui"
		m.handleChannelKey(keyEnter)
		m.saveAndClose()
		got := liveAndOnDisk(t, store)
		if len(got) != 1 || got[0].OutputDirectory != "/archive/tui" || got[0].Name != "A renamed on the web" {
			t.Errorf("channels: %+v, want the TUI's output directory and the dashboard's name", got)
		}
	})
}

// TestMergeChannelFieldsCoversEveryFormField: each field the editor's form
// shows is written when the editor changed it and kept from live when it
// did not, and a field the form does not show always keeps live's value.
//
// Mutants killed: writeChannelField missing any one of its cases (that
// field's edit is lost); mergeChannelFields iterating a fixed subset.
func TestMergeChannelFieldsCoversEveryFormField(t *testing.T) {
	for k := range channelToValues(config.ChannelConfig{}) {
		if !writeChannelField(&config.ChannelConfig{}, config.ChannelConfig{}, k) {
			t.Errorf("form field %q has no writeChannelField case", k)
		}
	}

	one, three, seven := 1, 3, 7
	base := config.ChannelConfig{ID: mergeA, Name: "A", Platform: "youtube"}
	edited := config.ChannelConfig{
		ID: "UCaaaaaaaaaaaaaaaaaaaaaA", Name: "tui", Platform: "youtube", Enabled: boolPtr(false),
		Terms: config.ChannelTerms{Simple: "(?i)tui"}, IncludeNonLiveContent: true, QualityPreference: "720p",
		OutputDirectory: "/tui", ArchiveWindowDays: &seven, ArchiveSlots: &three,
	}
	live := config.ChannelConfig{ID: mergeA, Name: "web", NumDescLookbehind: &one, OutputDirectory: "/web"}
	got := mergeChannelFields(live, base, edited)
	if !maps.Equal(channelToValues(got), channelToValues(edited)) {
		t.Errorf("every field changed: form shows %v, want the editor's %v", channelToValues(got), channelToValues(edited))
	}
	if got.NumDescLookbehind != &one {
		t.Errorf("a field the form does not show was not kept from live: %v", got.NumDescLookbehind)
	}

	// The editor changed nothing: live comes back as it is.
	if got := mergeChannelFields(live, base, base); !reflect.DeepEqual(got, live) {
		t.Errorf("no field changed: %+v, want live %+v", got, live)
	}
}

// TestTUIAddOfAChannelTheDashboardAddedIsWrittenWhole: an entry the editor
// added has no Open-time copy to diff against, so when the dashboard added
// the same ID meanwhile the TUI's entry replaces it whole, as before.
//
// Mutant killed: merging an add field by field against an empty base (the
// dashboard's output directory kept under the TUI's entry).
func TestTUIAddOfAChannelTheDashboardAddedIsWrittenWhole(t *testing.T) {
	m, store := settingsOverStore(t)
	dashboard(t, store, func(chs []config.ChannelConfig) []config.ChannelConfig {
		return append(chs, config.ChannelConfig{ID: mergeC, OutputDirectory: "/archive/web"})
	})
	m.handleChannelKey("a")
	m.channelEditValues["id"] = mergeC
	m.channelEditValues["name"] = "C (added in the TUI)"
	m.handleChannelKey(keyEnter)
	m.saveAndClose()
	got := liveAndOnDisk(t, store)
	if len(got) != 2 || got[1].ID != mergeC || got[1].Name != "C (added in the TUI)" || got[1].OutputDirectory != "" {
		t.Errorf("channels: %+v, want A and the TUI's C as it added it", got)
	}
}
