package tui

import (
	"strings"
	"testing"
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
