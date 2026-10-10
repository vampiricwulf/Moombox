package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestImportPickerTakesAZipInAnyCase: the picker's AllowedTypes was ".zip"
// alone, and bubbles matches it case-sensitively — "STREAM.ZIP" was drawn
// dimmed and Enter on it did nothing, while the dashboard and the server both
// take it. Every case of the extension is pickable and drawn as ".zip" is; a
// file that is not a zip still is not.
//
// Mutant: AllowedTypes back to []string{".zip"}, or caseVariants dropping the
// upper-case branch — STREAM.ZIP is dimmed and Enter selects nothing.
func TestImportPickerTakesAZipInAnyCase(t *testing.T) {
	open := func(name string) *ImportDialogModel {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("PK\x03\x04"), 0o600); err != nil {
			t.Fatal(err)
		}
		m := NewImportDialogModel()
		m.SetSize(100, 40)
		cmd := m.Open(dir)
		if cmd == nil {
			t.Fatal("Open returned no readDir cmd")
		}
		m.UpdateComponents(cmd())
		return m
	}

	lower := open("stream.zip").View()
	for _, name := range []string{"STREAM.ZIP", "Stream.Zip", "stream.zIp"} {
		m := open(name)
		if got := strings.ReplaceAll(m.View(), name, "stream.zip"); got != lower {
			t.Errorf("%s is not drawn as stream.zip is:\n%s", name, m.View())
		}
		m.UpdateComponents(tea.KeyPressMsg{Code: tea.KeyEnter})
		if m.step != 1 || filepath.Base(m.filePath) != name {
			t.Errorf("Enter on %s: step %d, selected %q — want the metadata step for it", name, m.step, m.filePath)
		}
	}

	for _, name := range []string{"notes.txt", "stream.zip.part"} {
		m := open(name)
		m.UpdateComponents(tea.KeyPressMsg{Code: tea.KeyEnter})
		if m.step != 0 || m.filePath != "" {
			t.Errorf("Enter on %s selected %q (step %d) — only a zip is pickable", name, m.filePath, m.step)
		}
	}
}
