package tui

import (
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestImportPickerReopenStartsAtTheTop: the picker's cursor survived Close,
// and bubbles indexes its file list with it on Enter — so moving to the third
// zip, closing, and re-opening A Z in a directory with one zip panicked
// ("index out of range [2] with length 1"), which runTUI turns into a shutdown
// of the whole process, every recording included.
//
// Mutant: dropping the fresh picker from Open — Enter panics.
func TestImportPickerReopenStartsAtTheTop(t *testing.T) {
	dirWith := func(names ...string) string {
		t.Helper()
		dir := t.TempDir()
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(dir, n), []byte("PK"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	feed := func(m *ImportDialogModel, cmd tea.Cmd) {
		t.Helper()
		if cmd == nil {
			t.Fatal("Open returned no readDir cmd")
		}
		m.UpdateComponents(cmd())
	}

	m := NewImportDialogModel()
	m.SetSize(100, 40)
	feed(m, m.Open(dirWith("a.zip", "b.zip", "c.zip")))
	for range 2 {
		m.UpdateComponents(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	m.HandleKey(keyEsc)

	only := dirWith("x.zip")
	feed(m, m.Open(only))
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Enter in the re-opened picker panicked: %v", r)
		}
	}()
	m.UpdateComponents(tea.KeyPressMsg{Code: tea.KeyEnter})
	if want := filepath.Join(only, "x.zip"); m.filePath != want {
		t.Errorf("Enter selected %q, want the only zip %q", m.filePath, want)
	}
}
