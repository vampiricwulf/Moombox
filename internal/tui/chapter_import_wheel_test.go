package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// wheelPreviewApp opens the chapter import over a four-line paste whose
// preview answered with n chapters.
func wheelPreviewApp(t *testing.T, n int) *App {
	t.Helper()
	a := overlayTestApp(t)
	a.chapterDlg.SetSize(a.width, a.height)
	a.chapterDlg.Open(a.taskList.GetJobByID("j1"))
	a.chapterDlg.text.SetValue("0:00 One\n1:00 Two\n2:00 Three\n3:00 Four")
	a.chapterDlg.text.MoveToBegin()
	v := ChapterImportView{}
	for i := range n {
		v.Chapters = append(v.Chapters, ChapterImportRow{Start: float64(i * 60), Title: fmt.Sprintf("chapter-%02d-", i)})
	}
	a.chapterDlg.SetPreview(a.chapterDlg.seq, v, nil)
	return a
}

func wheelAt(a *App, y int, button tea.MouseButton) {
	_, _ = a.handleMouse(tea.MouseWheelMsg{X: a.width / 2, Y: y, Button: button})
}

// pasteBottom is where the paste box ends on the drawn screen: the offset
// field is the next row.
func TestChapterImportPasteBottomMatchesTheScreen(t *testing.T) {
	for _, h := range []int{20, 24, 40, 60} {
		a := wheelPreviewApp(t, 5)
		a.height = h
		a.chapterDlg.SetSize(a.width, h)
		lines := strings.Split(a.chapterDlg.View(), "\n")
		offsetRow := -1
		for i, l := range lines {
			if strings.Contains(l, "Offset:") {
				offsetRow = i
				break
			}
		}
		if got := a.chapterDlg.pasteBottom(); got+1 != offsetRow {
			t.Errorf("height %d: pasteBottom %d, the offset field is on row %d", h, got, offsetRow)
		}
	}
}

// With the pointer below the paste box, a wheel notch scrolls a preview that
// has more chapters than fit by one row and leaves the paste cursor alone
// (owner ruling R-C2 names the chapter-import preview). It stops at both
// ends, where the last chapter is on screen with no "more" line.
//
// Mutants: scrolling three rows; no clamp at the end; moving the paste
// cursor as well.
func TestWheelScrollsALongChapterPreview(t *testing.T) {
	a := wheelPreviewApp(t, 60)
	d := a.chapterDlg
	below := d.pasteBottom() + 3
	if strings.Contains(d.View(), "chapter-59-") || !strings.Contains(d.View(), "chapter-00-") {
		t.Fatalf("the preview does not start at its first rows, cut")
	}

	wheelAt(a, below, tea.MouseWheelUp) // already at the top
	if d.previewTop != 0 {
		t.Fatalf("a notch up at the top moved to row %d", d.previewTop)
	}
	wheelAt(a, below, tea.MouseWheelDown)
	view := d.View()
	if d.previewTop != 1 || strings.Contains(view, "chapter-00-") || !strings.Contains(view, "chapter-01-") || !strings.Contains(view, "↑ 1 earlier") {
		t.Fatalf("one notch down: row %d, want the preview from its second chapter with the count above", d.previewTop)
	}
	if d.text.Line() != 0 {
		t.Fatalf("the notch moved the paste cursor to line %d", d.text.Line())
	}
	for range 100 {
		wheelAt(a, below, tea.MouseWheelDown)
	}
	view = d.View()
	if d.previewTop != d.previewMaxTop() || !strings.Contains(view, "chapter-59-") || strings.Contains(view, "more") {
		t.Fatalf("at the end: row %d of %d, want the last chapter drawn and no \"more\" line", d.previewTop, d.previewMaxTop())
	}
	wheelAt(a, below, tea.MouseWheelUp)
	if d.previewTop != d.previewMaxTop()-1 {
		t.Fatalf("a notch up from the end left row %d", d.previewTop)
	}

	// Reopening starts at the top again.
	d.Open(a.taskList.GetJobByID("j1"))
	if d.previewTop != 0 {
		t.Fatalf("a reopened dialog kept row %d", d.previewTop)
	}
}

// Over the paste box, and anywhere while the preview fits, the wheel is the
// paste box's as before.
func TestWheelStaysWithThePasteBoxOutsideALongPreview(t *testing.T) {
	a := wheelPreviewApp(t, 60)
	d := a.chapterDlg
	wheelAt(a, d.pasteBottom(), tea.MouseWheelDown)
	if d.previewTop != 0 || d.text.Line() != 1 {
		t.Fatalf("over the paste box: preview row %d, paste line %d, want 0 and 1", d.previewTop, d.text.Line())
	}

	a = wheelPreviewApp(t, 3)
	d = a.chapterDlg
	wheelAt(a, d.pasteBottom()+3, tea.MouseWheelDown)
	if d.previewTop != 0 || d.text.Line() != 1 {
		t.Fatalf("a preview that fits: preview row %d, paste line %d, want 0 and 1", d.previewTop, d.text.Line())
	}
}

// A shorter preview arriving after a scroll is drawn from a row it has.
func TestChapterPreviewTopIsClampedToANewPreview(t *testing.T) {
	a := wheelPreviewApp(t, 60)
	d := a.chapterDlg
	for range 100 {
		wheelAt(a, d.pasteBottom()+3, tea.MouseWheelDown)
	}
	d.SetPreview(d.seq, ChapterImportView{Chapters: []ChapterImportRow{{Title: "only-one"}}}, nil)
	if view := d.View(); !strings.Contains(view, "only-one") || strings.Contains(view, "earlier") {
		t.Fatalf("the one-chapter preview is not drawn from its first row")
	}
}
