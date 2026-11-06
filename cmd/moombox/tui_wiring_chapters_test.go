package main

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

// TestTUIChapterViewCarriesEveryField pins the routes → tui conversion the
// A C and A U callbacks answer through: every field of the view, both
// lists' rows, and the zero view for nil.
//
// Mutant: any field dropped from tuiChapterView (EmbedError, HasOriginal,
// Error, Split…) — the TUI would report a failed embed as a success, or
// never offer the restore.
func TestTUIChapterViewCarriesEveryField(t *testing.T) {
	in := &routes.ChapterImportView{
		Chapters:    []routes.Chapter{{Start: 1, End: 2, Title: "a"}},
		Current:     []routes.Chapter{{Start: 0, End: 9, Title: "old"}},
		HasOriginal: true, OffsetSeconds: -5, DefaultOffsetSeconds: 3, DurationSeconds: 9,
		Split: true, Error: "No timestamps found", Embedded: true, EmbedError: "e",
	}
	want := tui.ChapterImportView{
		Chapters:    []tui.ChapterImportRow{{Start: 1, End: 2, Title: "a"}},
		Current:     []tui.ChapterImportRow{{Start: 0, End: 9, Title: "old"}},
		HasOriginal: true, OffsetSeconds: -5, DefaultOffsetSeconds: 3,
		Split: true, Error: "No timestamps found", Embedded: true, EmbedError: "e",
	}
	if got := tuiChapterView(in); !reflect.DeepEqual(got, want) {
		t.Errorf("tuiChapterView = %+v\nwant %+v", got, want)
	}
	if got := tuiChapterView(nil); !reflect.DeepEqual(got, tui.ChapterImportView{}) {
		t.Errorf("tuiChapterView(nil) = %+v", got)
	}
}

// TestChapterImportWiring pins, as text (the closures sit on a tui.App a live
// tea.Program owns — see TestRecoverAsidesWiringForwardsTheWorker), that the
// routes get the worker as their importer, the service is kept for the TUI,
// and the TUI's four callbacks reach that one service.
//
// Mutants: the Importer left out (every save answers 503); a callback wired
// to anything but the shared service.
func TestChapterImportWiring(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	rw := read("routes_wiring.go")
	for _, want := range []string{"chapterImporter = s.dlWorker", "Importer: chapterImporter,", "s.chapterSvc = routes.ChapterRoutes("} {
		if !strings.Contains(rw, want) {
			t.Errorf("routes_wiring.go lacks %q", want)
		}
	}
	tw := read("tui_wiring.go")
	for _, want := range []string{"svc.Preview(", "svc.Save(", "svc.Restore(", "app.HasOriginalChapters = svc.HasOriginal", "if svc := s.chapterSvc; svc != nil {"} {
		if !strings.Contains(tw, want) {
			t.Errorf("tui_wiring.go lacks %q", want)
		}
	}
}
