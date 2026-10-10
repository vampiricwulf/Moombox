package tui

import (
	"path/filepath"
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
// three); mergeChannelEdits treating every entry as edited (the disable
// reverted); saveCurrentChannel storing valuesToChannel's spelled-out
// defaults for a no-change Enter (the disable reverted by the no-op edit).
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
// "shroud" replaces the live entry the dashboard has since re-spelled
// "Shroud" rather than adding a second entry config.Validate would refuse;
// a rename removes the old ID and adds the new one; an untouched editor
// returns live itself, unchanged.
//
// Mutant killed: comparing IDs case-sensitively (two entries).
func TestMergeChannelEditsMatchesIDsCaseInsensitively(t *testing.T) {
	base := []config.ChannelConfig{{ID: "shroud", Platform: "twitch"}}
	live := []config.ChannelConfig{{ID: "Shroud", Platform: "twitch", Name: "web"}, {ID: mergeB}}

	edited := []config.ChannelConfig{{ID: "shroud", Platform: "twitch", Name: "tui"}}
	got, changed := mergeChannelEdits(base, edited, live)
	if !changed || len(got) != 2 || got[0].ID != "shroud" || got[0].Name != "tui" {
		t.Errorf("edit across a case change: %+v (changed %v), want the TUI's entry in place and B", got, changed)
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
