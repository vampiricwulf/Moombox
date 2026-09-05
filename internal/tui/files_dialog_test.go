package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// itemCounts tallies the list items by concrete type.
func itemCounts(m *FilesDialogModel) (files, history, dividers int) {
	for _, it := range m.list.Items() {
		switch it.(type) {
		case fileItem:
			files++
		case historyItem:
			history++
		case sectionHeaderItem:
			dividers++
		}
	}
	return
}

// TestFilesDialog_FilesErrorKeepsHistory covers the edge case: when the
// orphaned-files fetch fails but history loads, the history must still be shown
// (not blanked by the files error) and the error surfaces as an inline warning.
func TestFilesDialog_FilesErrorKeepsHistory(t *testing.T) {
	m := NewFilesDialogModel()
	m.SetSize(100, 30)
	m.Open()

	// Files fetch fails; history fetch succeeds (order is arbitrary at runtime).
	m.SetFilesError("disk unavailable")
	m.SetHistory([]OrphanedHistoryEntry{{VideoID: "aaaaaaaaaaa"}, {VideoID: "bbbbbbbbbbb"}})

	if m.loading {
		t.Fatal("still loading after both sources reported")
	}
	files, history, dividers := itemCounts(m)
	if files != 0 || history != 2 || dividers != 0 {
		t.Fatalf("want 0 files / 2 history / 0 dividers, got %d/%d/%d", files, history, dividers)
	}
	if got := m.loadErrorText(); !strings.Contains(got, "files") {
		t.Fatalf("load error should name the failed source, got %q", got)
	}
	// The history rows must actually render, not be hidden behind the error.
	view := m.View()
	if !strings.Contains(view, "aaaaaaaaaaa") {
		t.Fatalf("history entry not shown despite files-fetch failure:\n%s", view)
	}
}

// TestFilesDialog_HistoryErrorKeepsFiles is the symmetric case.
func TestFilesDialog_HistoryErrorKeepsFiles(t *testing.T) {
	m := NewFilesDialogModel()
	m.SetSize(100, 30)
	m.Open()

	m.SetFiles([]OrphanedFileEntry{{Path: "/a/one.ts", RelPath: "one.ts"}})
	m.SetHistoryError("db locked")

	files, history, _ := itemCounts(m)
	if files != 1 || history != 0 {
		t.Fatalf("want 1 file / 0 history, got %d/%d", files, history)
	}
	if got := m.loadErrorText(); !strings.Contains(got, "history") {
		t.Fatalf("load error should name the failed source, got %q", got)
	}
	if !strings.Contains(m.View(), "one.ts") {
		t.Fatal("file not shown despite history-fetch failure")
	}
}

// TestFilesDialog_RemoveFileNoResurrect guards the resurrection bug: RemoveFile
// must drop the file from the backing slice so a later rebuild (triggered here
// by removing a history entry) cannot bring it back, and the divider drops once
// the files group empties.
func TestFilesDialog_RemoveFileNoResurrect(t *testing.T) {
	m := NewFilesDialogModel()
	m.SetSize(100, 30)
	m.Open()

	m.SetFiles([]OrphanedFileEntry{
		{Path: "/a/one.ts", RelPath: "one.ts"},
		{Path: "/a/two.ts", RelPath: "two.ts"},
	})
	m.SetHistory([]OrphanedHistoryEntry{{VideoID: "ccccccccccc"}})

	// 2 files + divider + 1 history.
	if f, h, d := itemCounts(m); f != 2 || h != 1 || d != 1 {
		t.Fatalf("initial: want 2/1/1, got %d/%d/%d", f, h, d)
	}

	m.RemoveFile("/a/one.ts")
	if f, _, d := itemCounts(m); f != 1 || d != 1 {
		t.Fatalf("after RemoveFile: want 1 file / 1 divider, got %d files / %d dividers", f, d)
	}

	// Removing the history entry forces a full rebuild from the backing slices.
	m.RemoveHistory("ccccccccccc")
	f, h, d := itemCounts(m)
	if f != 1 || h != 0 || d != 0 {
		t.Fatalf("after rebuild: want 1 file / 0 history / 0 divider, got %d/%d/%d", f, h, d)
	}
	// The surviving file must be two.ts — one.ts must not have resurrected.
	if strings.Contains(m.View(), "one.ts") {
		t.Fatal("deleted file one.ts resurrected after rebuild")
	}
	if !strings.Contains(m.View(), "two.ts") {
		t.Fatal("surviving file two.ts missing after rebuild")
	}
}

// TestFilesDialog_PagingClearsDeleteConfirm guards the fix: paging (not just
// up/down) away from an armed delete must clear the stale "Press D again" hint.
// Previously only keyUp/keyDown reset the confirm, so a PageDown left a
// dangling confirmation.
func TestFilesDialog_PagingClearsDeleteConfirm(t *testing.T) {
	m := NewFilesDialogModel()
	m.SetSize(100, 30)
	m.Open()
	m.SetFiles([]OrphanedFileEntry{{Path: "/a/one.ts", RelPath: "one.ts"}, {Path: "/a/two.ts", RelPath: "two.ts"}})
	m.SetHistory([]OrphanedHistoryEntry{{VideoID: "aaaaaaaaaaa"}})

	m.deleteConfirmID = "/a/one.ts"
	m.feedbackMsg = `Press D again to delete "one.ts"`
	m.HandleKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if m.deleteConfirmID != "" || m.feedbackMsg != "" {
		t.Errorf("paging must clear the delete-confirm arming; got id=%q msg=%q", m.deleteConfirmID, m.feedbackMsg)
	}
}

// TestFilesDialog_NavigationSkipsDivider guards that a cursor move never rests
// on the non-selectable section divider between the files and history groups.
func TestFilesDialog_NavigationSkipsDivider(t *testing.T) {
	m := NewFilesDialogModel()
	m.SetSize(100, 30)
	m.Open()
	m.SetFiles([]OrphanedFileEntry{{Path: "/a/one.ts", RelPath: "one.ts"}})
	m.SetHistory([]OrphanedHistoryEntry{{VideoID: "aaaaaaaaaaa"}})

	// Layout: [file, divider, history]. From the file, a down move would land
	// on the divider; HandleKey must step past it to the history row.
	m.list.Select(0)
	for _, msg := range []tea.KeyPressMsg{{Code: tea.KeyDown}, {Code: tea.KeyUp}, {Code: tea.KeyDown}} {
		m.HandleKey(msg)
		if _, isHeader := m.list.SelectedItem().(sectionHeaderItem); isHeader {
			t.Fatalf("cursor rested on the section divider after a %q move", msg.String())
		}
	}
}

// TestFilesDialog_DeleteAllInSectionNeedsTwoPresses: A arms a section-wide
// confirm distinct from the per-item one; a second A within the window
// returns the bulk action with every entry of THAT section; navigation
// disarms.
func TestFilesDialog_DeleteAllInSectionNeedsTwoPresses(t *testing.T) {
	m := NewFilesDialogModel()
	m.SetSize(80, 24)
	m.SetFiles([]OrphanedFileEntry{{Path: "/a/one.ts", RelPath: "one.ts", Type: "staging"}, {Path: "/a/two.ts", RelPath: "two.ts", Type: "output"}})
	m.SetHistory([]OrphanedHistoryEntry{{VideoID: "vid1"}})
	// cursor starts on the first file
	action, _ := m.HandleKey(keyMsg("A"))
	if action != "" || !strings.Contains(m.feedbackMsg, "Press A again") {
		t.Fatalf("first A must arm, got action %q feedback %q", action, m.feedbackMsg)
	}
	m.HandleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.deleteAllArmed {
		t.Fatal("navigation must disarm the section confirm")
	}
	m.HandleKey(keyMsg("A"))
	action, data := m.HandleKey(keyMsg("A"))
	paths, ok := data.([]string)
	if action != "delete-all-files" || !ok || len(paths) != 2 {
		t.Fatalf("second A = (%q, %#v), want delete-all-files with both paths", action, data)
	}
	// Move onto the history half and repeat. Cursor sits on the second file
	// (two.ts, index 1); a single Down steps over the divider straight onto
	// the sole history row (index 3) — the list wraps on InfiniteScrolling,
	// so a second Down here would wrap back to the first file instead.
	m.HandleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m.HandleKey(keyMsg("A"))
	action, data = m.HandleKey(keyMsg("A"))
	ids, ok := data.([]string)
	if action != "delete-all-history" || !ok || len(ids) != 1 || ids[0] != "vid1" {
		t.Fatalf("history half: (%q, %#v)", action, data)
	}

	// The two confirms retract each other. A live per-item confirm plus a
	// live bulk one is one hint on screen describing whichever of two
	// different deletions the next key happens to reach.
	m2 := NewFilesDialogModel()
	m2.SetSize(80, 24)
	m2.SetFiles([]OrphanedFileEntry{{Path: "/a/one.ts", RelPath: "one.ts", Type: "staging"}, {Path: "/a/two.ts", RelPath: "two.ts", Type: "output"}})
	m2.HandleKey(keyMsg("D"))
	if m2.deleteConfirmID == "" {
		t.Fatal("D did not arm the per-item confirm")
	}
	m2.HandleKey(keyMsg("A"))
	action, data = m2.HandleKey(keyMsg("A"))
	if action != "delete-all-files" {
		t.Fatalf("D then A,A = %q (%#v), want the bulk sweep", action, data)
	}
	if m2.deleteConfirmID != "" {
		t.Errorf("the per-item confirm survived the bulk arm: %q", m2.deleteConfirmID)
	}

	m3 := NewFilesDialogModel()
	m3.SetSize(80, 24)
	m3.SetFiles([]OrphanedFileEntry{{Path: "/a/one.ts", RelPath: "one.ts", Type: "staging"}})
	m3.HandleKey(keyMsg("A"))
	if !m3.deleteAllArmed {
		t.Fatal("A did not arm the bulk confirm")
	}
	m3.HandleKey(keyMsg("D"))
	if m3.deleteAllArmed {
		t.Error("the bulk confirm survived a per-item arm")
	}

	// And it expires on its own window rather than standing until a
	// navigation key wanders past.
	m3.HandleKey(keyMsg("A"))
	if !m3.deleteAllArmed {
		t.Fatal("A did not re-arm the bulk confirm")
	}
	m3.deleteAllTimer = time.Now().Add(-time.Second)
	m3.HandleKey(keyMsg("x")) // any key runs the timeout sweep
	if m3.deleteAllArmed || m3.deleteAllSection != "" || m3.feedbackMsg != "" {
		t.Errorf("expired bulk confirm still armed=%v section=%q hint=%q", m3.deleteAllArmed, m3.deleteAllSection, m3.feedbackMsg)
	}
}

// TestFilesDialog_BulkFailuresAreListed: the dialog shows how many were
// deleted and names the failures — and does it inside the terminal it was
// given. centerBox never truncates, so a message state one row too tall loses
// the bottom border and the footer off the bottom of an 80x24 screen.
func TestFilesDialog_BulkFailuresAreListed(t *testing.T) {
	m := NewFilesDialogModel()
	m.SetSize(80, 24)
	m.visible = true // View() renders nothing while hidden; a real sweep only completes on an open dialog
	m.SetBulkResult(3, []string{"two.ts: permission denied"})
	v := m.View()
	for _, want := range []string{"Deleted 3", "1 failed", "two.ts: permission denied"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}

	// A full list plus the worst message state: four failures on a 40-file
	// sweep, at the smallest terminal the TUI supports.
	full := NewFilesDialogModel()
	full.SetSize(80, 24)
	full.visible = true
	files := make([]OrphanedFileEntry, 0, 40)
	for i := range 40 {
		name := fmt.Sprintf("f%02d.ts", i)
		files = append(files, OrphanedFileEntry{Path: "/a/" + name, RelPath: name, Type: "staging"})
	}
	full.SetFiles(files)
	full.SetHistory([]OrphanedHistoryEntry{{VideoID: "vid1"}})
	full.SetBulkResult(36, []string{"a: x", "b: y", "c: z", "d: w"})
	v = full.View()
	if got := strings.Count(v, "\n") + 1; got > 24 {
		t.Errorf("View() is %d lines on a 24-line terminal:\n%s", got, v)
	}
	for _, want := range []string{"Deleted 36", "4 failed", "…and 2 more"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "c: z") {
		t.Errorf("view names a third failure past the two-name cap:\n%s", v)
	}
}

// TestFilesDialog_BothErrorsEmpty shows the error as the main message when there
// is nothing at all to list.
func TestFilesDialog_BothErrorsEmpty(t *testing.T) {
	m := NewFilesDialogModel()
	m.SetSize(100, 30)
	m.Open()

	m.SetFilesError("disk unavailable")
	m.SetHistoryError("db locked")

	if m.loading {
		t.Fatal("still loading after both errors")
	}
	if f, h, _ := itemCounts(m); f != 0 || h != 0 {
		t.Fatalf("want empty list, got %d files / %d history", f, h)
	}
	if got := m.loadErrorText(); got == "" {
		t.Fatal("expected a combined load error when both sources failed")
	}
}

// TestFilesDialogBulkDeleteThroughTheApp drives A,A through App.handleKey
// rather than the model, which is the only path that exercises the
// data.([]string) assertion in the filesDlg intercept and the re-fetch the
// bulkOrphanResultMsg arm answers with. A model-level test sees neither.
func TestFilesDialogBulkDeleteThroughTheApp(t *testing.T) {
	app := NewApp()

	var attempted []string
	app.OnDeleteOrphan = func(path string) error {
		attempted = append(attempted, path)
		if strings.HasSuffix(path, "two.ts") {
			return errors.New("permission denied")
		}
		return nil
	}
	listCalls := 0
	app.OnListOrphans = func() ([]OrphanedFileEntry, error) {
		listCalls++
		return nil, nil
	}

	app.filesDlg.SetSize(80, 24)
	app.filesDlg.Open()
	app.filesDlg.SetFiles([]OrphanedFileEntry{
		{Path: "/a/one.ts", RelPath: "one.ts", Type: "staging"},
		{Path: "/a/two.ts", RelPath: "two.ts", Type: "output"},
	})
	app.filesDlg.SetHistory(nil)

	if _, cmd := app.handleKey(keyMsg("A")); cmd != nil {
		t.Fatal("the first A must only arm the confirm")
	}
	_, cmd := app.handleKey(keyMsg("A"))
	if cmd == nil {
		t.Fatal("the second A produced no sweep command")
	}
	app.Update(runCmd(t, cmd)) // run the sweep, feed bulkOrphanResultMsg back

	if len(attempted) != 2 {
		t.Fatalf("attempted %v, want both paths — a failure must not stop the sweep", attempted)
	}
	if !slices.Contains(attempted, "/a/one.ts") || !slices.Contains(attempted, "/a/two.ts") {
		t.Errorf("attempted = %v, want both /a/one.ts and /a/two.ts", attempted)
	}
	if v := stripANSI(app.filesDlg.View()); !strings.Contains(v, "Deleted 1 · 1 failed") || !strings.Contains(v, "two.ts: permission denied") {
		t.Errorf("the dialog does not name the partial failure:\n%s", v)
	}

	// The sweep does not know which entries survived, so the arm re-fetches
	// both sources; without it the list keeps showing deleted files.
	before := listCalls
	_, cmd = app.Update(bulkOrphanResultMsg{Deleted: 1, Failures: []string{"two.ts: permission denied"}})
	if cmd == nil {
		t.Fatal("the bulk result arm answered with no re-fetch")
	}
	runCmd(t, cmd)
	if listCalls != before+1 {
		t.Errorf("OnListOrphans called %d times, want %d — the sweep must re-fetch", listCalls, before+1)
	}
}
