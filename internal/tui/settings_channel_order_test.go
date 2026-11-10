package tui

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// orderSettings opens the Settings overlay on its Channels section over a
// store holding channels A, B and C in that order, wired as
// settingsOverStore wires it.
func orderSettings(t *testing.T) (*SettingsModel, *config.Store) {
	t.Helper()
	store := config.NewStore(config.Defaults(), filepath.Join(t.TempDir(), "config.toml"))
	if err := store.Update(func(c *config.MoomboxConfig) {
		c.Channels = []config.ChannelConfig{{ID: mergeA, Name: "A"}, {ID: mergeB, Name: "B"}, {ID: mergeC, Name: "C"}}
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
	m.width, m.height = 100, 30
	m.switchSection(sectionIndexByName(t, "Channels"))
	return m, store
}

func diskChannelIDs(t *testing.T, store *config.Store) []string {
	t.Helper()
	disk, err := config.Load(store.SavePath())
	if err != nil {
		t.Fatal(err)
	}
	return channelIDs(disk.Channels)
}

// Shift+Down and Shift+Up move the selected channel in the Settings channel
// list, the cursor stays on it, and Save writes the new order to the store
// and to config.toml (owner ruling R-C3). The merge by ID alone carried no
// order, so before applyChannelOrder a reorder saved nothing.
//
// Mutants: the swap without moving channelIndex; a move that does not set
// dirty; a save that skips applyChannelOrder.
func TestSettingsShiftArrowsMoveAChannelAndSaveTheOrder(t *testing.T) {
	m, store := orderSettings(t)

	m.HandleKey("shift+down") // A below B
	if got, want := channelIDs(m.channels), []string{mergeB, mergeA, mergeC}; !slices.Equal(got, want) {
		t.Fatalf("after Shift+Down: %v, want %v", got, want)
	}
	if m.channelIndex != 1 {
		t.Fatalf("the cursor is on row %d, not on the moved channel", m.channelIndex)
	}
	m.HandleKey("shift+down") // A below C
	m.HandleKey("shift+down") // already last: nothing
	if got, want := channelIDs(m.channels), []string{mergeB, mergeC, mergeA}; !slices.Equal(got, want) {
		t.Fatalf("after three Shift+Down: %v, want %v", got, want)
	}
	m.HandleKey(keyUp)      // cursor to C
	m.HandleKey("shift+up") // C above B
	m.HandleKey("shift+up") // already first: nothing
	want := []string{mergeC, mergeB, mergeA}
	if got := channelIDs(m.channels); !slices.Equal(got, want) || m.channelIndex != 0 {
		t.Fatalf("after Shift+Up: %v at row %d, want %v at row 0", got, m.channelIndex, want)
	}
	if !m.dirty {
		t.Fatalf("a move did not mark the settings as changed")
	}

	if m.saveAndClose() != "close" || m.status != saveSaved {
		t.Fatalf("save: status %v, error %q", m.status, m.errorMsg)
	}
	if got := channelIDs(liveAndOnDisk(t, store)); !slices.Equal(got, want) {
		t.Errorf("the store holds %v, want %v", got, want)
	}
	if got := diskChannelIDs(t, store); !slices.Equal(got, want) {
		t.Errorf("config.toml holds %v, want %v", got, want)
	}
	// The save moved the entries whole: nothing but the order changed.
	for _, ch := range liveAndOnDisk(t, store) {
		if ch.Name != map[string]string{mergeA: "A", mergeB: "B", mergeC: "C"}[ch.ID] {
			t.Errorf("channel %s was saved with name %q", ch.ID, ch.Name)
		}
	}
}

// Esc after a move asks before throwing it away, as it does after an add.
func TestSettingsChannelMoveIsAskedAboutOnEsc(t *testing.T) {
	m, _ := orderSettings(t)
	m.HandleKey("shift+down")
	m.HandleKey(keyEsc)
	if !m.closeConfirm {
		t.Fatalf("Esc after a move closed without asking")
	}
}

// A save moves only the channels the editor holds, among the places they
// have now: a channel the dashboard added while the overlay was open stays
// where the dashboard put it, and one it removed stays removed.
func TestChannelMoveKeepsDashboardChanges(t *testing.T) {
	const mergeD = "UCdddddddddddddddddddddd"
	m, store := orderSettings(t)
	dashboard(t, store, func(chs []config.ChannelConfig) []config.ChannelConfig {
		// D added in second place, B removed: A D C.
		return []config.ChannelConfig{chs[0], {ID: mergeD, Name: "D"}, chs[2]}
	})
	m.HandleKey("shift+down") // editor: B A C
	m.HandleKey("shift+down") // editor: B C A
	m.saveAndClose()
	if m.status != saveSaved {
		t.Fatalf("save: status %v, error %q", m.status, m.errorMsg)
	}
	want := []string{mergeC, mergeD, mergeA}
	if got := channelIDs(liveAndOnDisk(t, store)); !slices.Equal(got, want) {
		t.Errorf("the store holds %v, want %v", got, want)
	}
}

// A save that moved no channel leaves the live order alone, whatever order
// the dashboard gave it by drag while the overlay was open; and a move put
// back before the save writes nothing.
func TestChannelOrderIsWrittenOnlyAfterAMove(t *testing.T) {
	m, store := orderSettings(t)
	dashboard(t, store, func(chs []config.ChannelConfig) []config.ChannelConfig {
		slices.Reverse(chs)
		return chs
	})
	saveLogLevel(t, m)
	want := []string{mergeC, mergeB, mergeA}
	if got := channelIDs(liveAndOnDisk(t, store)); !slices.Equal(got, want) {
		t.Fatalf("a save with no move reordered the channels: %v, want %v", got, want)
	}

	// Moved down and back up: the editor's order is the live order again.
	m, store = orderSettings(t)
	saves := 0
	save := m.OnSave
	m.OnSave = func(c *config.MoomboxConfig) error { saves++; return save(c) }
	m.HandleKey("shift+down")
	m.HandleKey("shift+up")
	m.saveAndClose()
	if saves != 0 {
		t.Errorf("a move put back was written (%d saves)", saves)
	}
}

func TestApplyChannelOrder(t *testing.T) {
	ch := func(ids ...string) []config.ChannelConfig {
		var out []config.ChannelConfig
		for _, id := range ids {
			out = append(out, config.ChannelConfig{ID: id})
		}
		return out
	}
	live := ch("a", "x", "b", "c")
	got, changed := applyChannelOrder(live, ch("C", "a", "b", "gone"))
	if !changed || !slices.Equal(channelIDs(got), []string{"c", "x", "a", "b"}) {
		t.Errorf("got %v changed=%v", channelIDs(got), changed)
	}
	if !reflect.DeepEqual(live, ch("a", "x", "b", "c")) {
		t.Errorf("live was reordered in place: %v", channelIDs(live))
	}
	if _, changed := applyChannelOrder(live, ch("a", "b", "c")); changed {
		t.Errorf("an order already in place reported a change")
	}
}

// The channel list names the move keys, in its action bar and in the footer.
func TestChannelListHintNamesTheMoveKeys(t *testing.T) {
	m, _ := orderSettings(t)
	const move = "Shift+↑/↓: Move"
	if hint := m.renderHintText(); !strings.Contains(hint, move) {
		t.Errorf("footer hint %q does not name the move keys", hint)
	}
	if view := stripANSI(m.View()); strings.Count(view, move) < 1 {
		t.Errorf("the channel list does not show %q:\n%s", move, view)
	}
	// At 80 columns the footer keeps the move keys ahead of the generic
	// entries it drops.
	m.width = 80
	found := false
	for line := range strings.SplitSeq(stripANSI(m.View()), "\n") {
		if strings.Contains(line, "Esc: Close") {
			found = strings.Contains(line, move) && strings.Contains(line, "D: Delete")
		}
	}
	if !found {
		t.Errorf("at 80 columns the footer lost the move keys or D: Delete:\n%s", stripANSI(m.View()))
	}
}
