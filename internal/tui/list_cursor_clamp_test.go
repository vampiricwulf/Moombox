package tui

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestFilesDialogCursorStaysOnARow: the list keeps its cursor index across
// SetItems/RemoveItem, so deleting the last row — or re-opening onto fewer
// rows than last time — left nothing selected, and D and A did nothing
// until Up was pressed. A deleted file's neighbour can also shift onto the
// cursor as the divider, which is not a row.
//
// Mutants: dropping clampListCursor from rebuildList — nothing is selected
// after the deletes; dropping the divider step — the cursor rests on it.
func TestFilesDialogCursorStaysOnARow(t *testing.T) {
	m := NewFilesDialogModel()
	m.SetSize(100, 40)
	var files []OrphanedFileEntry
	for i := range 5 {
		files = append(files, OrphanedFileEntry{Path: fmt.Sprintf("/o/%d", i), RelPath: fmt.Sprintf("%d", i), Type: "output"})
	}
	m.SetFiles(files)
	m.SetHistory(nil)
	for range 4 {
		m.HandleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	m.RemoveFile("/o/4")
	if sel := m.SelectedFile(); sel == nil || sel.Path != "/o/3" {
		t.Errorf("after deleting the last row the selection is %+v, want /o/3", sel)
	}

	m.Open()
	m.SetFiles(files[:2])
	m.SetHistory(nil)
	if sel := m.SelectedFile(); sel == nil || sel.Path != "/o/0" {
		t.Errorf("re-opened onto 2 rows the selection is %+v, want the first row", sel)
	}

	// files f0,f1 | divider | one history row: delete f1 with the cursor on it.
	m.Open()
	m.SetFiles(files[:2])
	m.SetHistory([]OrphanedHistoryEntry{{VideoID: "h0"}})
	m.HandleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m.RemoveFile("/o/1")
	if _, onDivider := m.list.SelectedItem().(sectionHeaderItem); onDivider {
		t.Error("the cursor came to rest on the divider after a delete")
	}
}

// TestClientTokensCursorStaysOnARow: the same, for revoking the last token.
//
// Mutant: dropping clampListCursor from RemoveToken.
func TestClientTokensCursorStaysOnARow(t *testing.T) {
	m := NewClientTokensDialogModel()
	m.SetSize(100, 40)
	var tokens []*database.ClientToken
	for i := range 3 {
		tokens = append(tokens, &database.ClientToken{ID: fmt.Sprintf("t%d", i), Label: fmt.Sprintf("token %d", i)})
	}
	m.SetTokens(tokens)
	for range 2 {
		m.HandleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	m.RemoveToken("t2")
	if sel := m.SelectedToken(); sel == nil || sel.ID != "t1" {
		t.Errorf("after revoking the last token the selection is %+v, want t1", sel)
	}
}

// TestFilesDialogIgnoresDeletesWhileScanning: R shows only "Scanning…" but
// left the old rows in the list, and D and A armed and fired against that
// list the operator could no longer see.
//
// Mutant: dropping the loading gate from D — the second press deletes.
func TestFilesDialogIgnoresDeletesWhileScanning(t *testing.T) {
	m := NewFilesDialogModel()
	m.SetSize(100, 40)
	m.SetFiles([]OrphanedFileEntry{{Path: "/o/0", RelPath: "0", Type: "output"}})
	m.SetHistory([]OrphanedHistoryEntry{{VideoID: "h0"}})
	m.HandleKey(tea.KeyPressMsg{Code: 'r', Text: "r"})
	for _, k := range []tea.KeyPressMsg{{Code: 'd', Text: "d"}, {Code: 'd', Text: "d"}, {Code: 'a', Text: "a"}, {Code: 'a', Text: "a"}} {
		if action, _ := m.HandleKey(k); action != "" {
			t.Errorf("%q during a scan = %q, want nothing", k.Text, action)
		}
	}
}
