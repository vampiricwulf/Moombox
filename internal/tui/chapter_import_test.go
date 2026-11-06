package tui

import (
	"errors"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// chapterRig is an App with the chapter import's four callbacks wired to
// fakes that record what they were asked, and one Finished job selected.
type chapterRig struct {
	app *App
	job *database.Job

	mu        sync.Mutex
	previews  []string // "text|offset"
	saves     []string
	restores  []string
	original  bool
	saveErr   error
	previewFn func(text, offset string) ChapterImportView
}

func newChapterRig(t *testing.T) *chapterRig {
	t.Helper()
	r := &chapterRig{job: &database.Job{ID: "yt_a", Title: "Irys tournament", Status: database.StatusFinished, OutputFile: "/out/a.mp4", Filename: "a.mp4"}}
	app := NewApp()
	app.OnPreviewChapters = func(jobID, text, offset string) (ChapterImportView, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.previews = append(r.previews, text+"|"+offset)
		if r.previewFn != nil {
			return r.previewFn(text, offset), nil
		}
		return ChapterImportView{Current: []ChapterImportRow{{Start: 0, End: 10, Title: "old"}}, Error: "No timestamps found"}, nil
	}
	app.OnSaveChapters = func(jobID, text, offset string) (ChapterImportView, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.saves = append(r.saves, jobID+"|"+text+"|"+offset)
		if r.saveErr != nil {
			return ChapterImportView{}, r.saveErr
		}
		return ChapterImportView{Chapters: []ChapterImportRow{{Start: 0, End: 5, Title: "a"}, {Start: 5, End: 10, Title: "b"}}, Embedded: true}, nil
	}
	app.OnRestoreChapters = func(jobID string) (ChapterImportView, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.restores = append(r.restores, jobID)
		return ChapterImportView{Chapters: []ChapterImportRow{{Start: 0, End: 10, Title: "old"}}, Embedded: true}, nil
	}
	app.HasOriginalChapters = func(string) bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.original
	}
	app.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	app.Update(JobsUpdateMsg{Jobs: []*database.Job{r.job}})
	r.app = app
	return r
}

func menuChord(items []ActionMenuItem, chord string) *ActionMenuItem {
	for i := range items {
		if items[i].Chord == chord {
			return &items[i]
		}
	}
	return nil
}

func helpHasChord(app *App, chord string) bool {
	h := NewHelpModel()
	h.SetMenuItems(app.buildMenuItems())
	for _, sec := range h.orderedSections() {
		for _, k := range sec.keys {
			if strings.TrimSpace(k.key) == chord {
				return true
			}
		}
	}
	return false
}

// TestChapterImportChordsInMenuHelpAndHints: A C "Import Chapters" and A U
// "Restore Original Chapters" are in the action menu, the help overlay and
// the A hint line once wired, and nowhere without their callbacks; A U is
// offered for a job only while it has an original (owner decision CI-4).
//
// Mutants: either buildMenuItems entry dropped or ungated; A U's JobFilter
// not asking HasOriginalChapters; chapterImportable taking an active job.
func TestChapterImportChordsInMenuHelpAndHints(t *testing.T) {
	r := newChapterRig(t)
	items := r.app.buildMenuItems()
	h, u := menuChord(items, "A C"), menuChord(items, "A U")
	if h == nil || h.Label != "Import Chapters" || u == nil || u.Label != "Restore Original Chapters" {
		t.Fatalf("menu items: A C %+v, A U %+v", h, u)
	}
	if !helpHasChord(r.app, "A C") || !helpHasChord(r.app, "A U U") {
		t.Error("the help overlay is missing A C or A U U (its confirm chord)")
	}
	if !h.JobFilter(r.job) {
		t.Error("A C refuses a Finished job with an archive")
	}
	if h.JobFilter(&database.Job{Status: database.StatusDownloading, OutputFile: "x"}) {
		t.Error("A C offered for an active job")
	}
	if h.JobFilter(&database.Job{Status: database.StatusError}) {
		t.Error("A C offered for an Error row with no archive")
	}
	if !u.NeedsConfirm {
		t.Error("A U restores without a confirm keypress")
	}

	hint := r.app.chordFeedback("a")
	if !strings.Contains(hint, "C Chapters") || strings.Contains(hint, "U Restore Chapters") {
		t.Errorf("hint with no original = %q", hint)
	}
	r.original = true
	if hint := r.app.chordFeedback("a"); !strings.Contains(hint, "U Restore Chapters") {
		t.Errorf("hint with an original = %q", hint)
	}

	bare := NewApp()
	if menuChord(bare.buildMenuItems(), "A C") != nil || menuChord(bare.buildMenuItems(), "A U") != nil || helpHasChord(bare, "A C") {
		t.Error("the chords are offered with no callbacks wired")
	}
}

func typeIntoApp(app *App, s string) {
	for _, r := range s {
		if r == '\n' {
			app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			continue
		}
		app.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// previewNow fires the dialog's newest debounce tick through App.Update and
// delivers the preview it asks for, as the program would.
func previewNow(t *testing.T, app *App) {
	t.Helper()
	_, cmd := app.Update(chapterPreviewTickMsg{Seq: app.chapterDlg.seq})
	if cmd == nil {
		t.Fatal("the newest edit's tick asked for no preview")
	}
	app.Update(cmd())
}

// TestChapterImportPreviewRenders drives A C through App.Update: the paste
// and the offset are typed once each (never doubled), the preview is the
// callback's answer for the newest edit, drawn with its start times, and an
// older edit's tick or answer changes nothing.
//
// Mutants: HandleKey feeding the textarea too (doubled text); Tab not
// moving the typing to the offset; the tick asking for an overtaken edit;
// SetPreview drawing a stale answer.
func TestChapterImportPreviewRenders(t *testing.T) {
	r := newChapterRig(t)
	r.previewFn = func(text, offset string) ChapterImportView {
		if text == "" {
			return ChapterImportView{Current: []ChapterImportRow{{Start: 0, End: 10, Title: "old"}}, Error: "No timestamps found"}
		}
		return ChapterImportView{
			Chapters: []ChapterImportRow{{Start: 0, End: 60, Title: "Intro"}, {Start: 3725, End: 4000, Title: "Numb - Linkin Park"}},
			Current:  []ChapterImportRow{{Start: 0, End: 10, Title: "old"}}, OffsetSeconds: -5,
		}
	}
	r.app.dispatchAction("A C", r.job)
	if !r.app.chapterDlg.IsVisible() {
		t.Fatal("A C did not open the overlay")
	}
	previewNow(t, r.app) // the opening preview
	if v := r.app.chapterDlg.View(); !strings.Contains(v, "Current: 1 chapters") || !strings.Contains(v, "Paste a timestamp comment") {
		t.Errorf("opening view:\n%s", v)
	}

	typeIntoApp(r.app, "├1:02:10 Numb")
	staleSeq := r.app.chapterDlg.seq
	typeIntoApp(r.app, " - Linkin Park")
	r.app.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	typeIntoApp(r.app, "-5")
	if got := r.app.chapterDlg.text.Value(); got != "├1:02:10 Numb - Linkin Park" {
		t.Fatalf("paste = %q (typed once?)", got)
	}
	if got := r.app.chapterDlg.offset.Value(); got != "-5" {
		t.Fatalf("offset = %q", got)
	}
	if _, cmd := r.app.Update(chapterPreviewTickMsg{Seq: staleSeq}); cmd != nil {
		t.Error("an overtaken edit's tick asked for a preview")
	}
	previewNow(t, r.app)
	if last := r.previews[len(r.previews)-1]; last != "├1:02:10 Numb - Linkin Park|-5" {
		t.Errorf("preview asked for %q", last)
	}
	view := r.app.chapterDlg.View()
	for _, want := range []string{"Preview: 2 chapters", "0:00  Intro", "1:02:05  Numb - Linkin Park", "(offset -5s)"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	// A stale answer does not replace it.
	r.app.Update(chapterPreviewResultMsg{Seq: staleSeq, View: ChapterImportView{Error: "No timestamps found"}})
	if !strings.Contains(r.app.chapterDlg.View(), "Preview: 2 chapters") {
		t.Error("a stale preview answer replaced the newest")
	}
}

// TestChapterImportConfirmSaves: Ctrl+S asks for a confirm keypress; any
// other key goes back without saving; y saves the paste and the offset
// through OnSaveChapters, closes the overlay and reports the count. A save
// refused goes back into the overlay with the reason, the paste kept.
//
// Mutants: Ctrl+S saving at once (no confirm step); "n" saving; the result
// not closing the overlay; a refusal closing it.
func TestChapterImportConfirmSaves(t *testing.T) {
	r := newChapterRig(t)
	r.previewFn = func(text, _ string) ChapterImportView {
		if text == "" {
			return ChapterImportView{Error: "No timestamps found"}
		}
		return ChapterImportView{Chapters: []ChapterImportRow{{Start: 0, End: 5, Title: "a"}, {Start: 5, End: 10, Title: "b"}}, Current: []ChapterImportRow{{Title: "old"}}}
	}
	r.app.dispatchAction("A C", r.job)
	previewNow(t, r.app)
	ctrlS := tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}
	// Nothing to save yet: Ctrl+S does nothing.
	r.app.Update(ctrlS)
	if r.app.chapterDlg.step != chapterImportStepEdit {
		t.Fatal("Ctrl+S with no preview moved on")
	}
	typeIntoApp(r.app, "0:00 a\n0:05 b")
	previewNow(t, r.app)

	r.app.Update(ctrlS)
	if r.app.chapterDlg.step != chapterImportStepConfirm || !strings.Contains(r.app.chapterDlg.View(), "Save 2 chapters, replacing the current 1?") {
		t.Fatalf("Ctrl+S did not ask to confirm:\n%s", r.app.chapterDlg.View())
	}
	typeIntoApp(r.app, "n")
	if r.app.chapterDlg.step != chapterImportStepEdit || len(r.saves) != 0 {
		t.Fatal("n saved, or did not go back")
	}
	if r.app.chapterDlg.text.Value() != "0:00 a\n0:05 b" {
		t.Errorf("the confirm's keypress reached the paste: %q", r.app.chapterDlg.text.Value())
	}

	// A refused save comes back into the overlay.
	r.saveErr = errors.New("The job is Muxing; chapters can be imported once it has finished")
	r.app.Update(ctrlS)
	typeIntoApp(r.app, "y")
	if !r.app.chapterDlg.IsSaving() {
		t.Fatal("y did not start the save")
	}
	r.app.Update(r.app.saveChaptersCmd(r.app.chapterDlg.saves)())
	if !r.app.chapterDlg.IsVisible() || !strings.Contains(r.app.chapterDlg.View(), "Not saved: The job is Muxing") {
		t.Fatalf("a refused save:\n%s", r.app.chapterDlg.View())
	}

	r.saveErr = nil
	r.app.Update(ctrlS)
	r.app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !r.app.chapterDlg.IsSaving() {
		t.Fatal("Enter at the confirm did not start the save")
	}
	r.app.Update(r.app.saveChaptersCmd(r.app.chapterDlg.saves)())
	if r.app.chapterDlg.IsVisible() {
		t.Error("a saved import left the overlay open")
	}
	if got := r.saves[len(r.saves)-1]; got != "yt_a|0:00 a\n0:05 b|" {
		t.Errorf("saved %q", got)
	}
	if !strings.Contains(r.app.feedback.msg, "Imported 2 chapters: Irys tournament") {
		t.Errorf("feedback = %q", r.app.feedback.msg)
	}
}

// TestChapterImportLateSaveResultIsNotTheOverlays: a save keeps running when
// its overlay is closed with Esc, and its late answer belongs to the feedback
// line, never to an overlay since reopened for a later save — a success must
// not close it while its own save runs, and a refusal must not be shown in
// it as its own.
//
// Mutant: handleChapterSaveResult matching on IsSaving() alone (the late
// success closes the second save's overlay; the late refusal is shown in it).
func TestChapterImportLateSaveResultIsNotTheOverlays(t *testing.T) {
	r := newChapterRig(t)
	r.previewFn = func(text, _ string) ChapterImportView {
		if text == "" {
			return ChapterImportView{Error: "No timestamps found"}
		}
		return ChapterImportView{Chapters: []ChapterImportRow{{Start: 0, End: 5, Title: "a"}}}
	}
	ctrlS := tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}
	startSave := func() int {
		t.Helper()
		r.app.dispatchAction("A C", r.job)
		previewNow(t, r.app)
		typeIntoApp(r.app, "0:00 a")
		previewNow(t, r.app)
		r.app.Update(ctrlS)
		typeIntoApp(r.app, "y")
		if !r.app.chapterDlg.IsSaving() {
			t.Fatal("the save did not start")
		}
		return r.app.chapterDlg.saves
	}
	first := startSave()
	r.app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if r.app.chapterDlg.IsVisible() {
		t.Fatal("Esc did not close the saving overlay")
	}
	second := startSave()

	r.app.Update(chapterSaveResultMsg{Title: "first", Save: first, Err: errors.New("first refused")})
	if !r.app.chapterDlg.IsSaving() || strings.Contains(r.app.chapterDlg.View(), "first refused") {
		t.Errorf("the first save's refusal was taken as the open overlay's:\n%s", r.app.chapterDlg.View())
	}
	if !strings.Contains(r.app.feedback.msg, "Chapters not saved (first): first refused") {
		t.Errorf("feedback = %q", r.app.feedback.msg)
	}
	r.app.Update(chapterSaveResultMsg{Title: "first", Save: first, View: ChapterImportView{Chapters: []ChapterImportRow{{}}}})
	if !r.app.chapterDlg.IsSaving() {
		t.Error("the first save's success closed the overlay waiting for the second")
	}
	r.app.Update(chapterSaveResultMsg{Title: "second", Save: second, View: ChapterImportView{Chapters: []ChapterImportRow{{}}}})
	if r.app.chapterDlg.IsVisible() {
		t.Error("the second save's own success left its overlay open")
	}
}

// TestChapterImportRestoreChord: A U runs OnRestoreChapters for the job and
// reports it; a save whose re-mux failed is reported as kept beside the file.
//
// Mutants: A U's dispatch not calling OnRestoreChapters; the embed failure
// reported as a plain success.
func TestChapterImportRestoreChord(t *testing.T) {
	r := newChapterRig(t)
	r.original = true
	_, cmd := r.app.dispatchAction("A U", r.job)
	if cmd == nil {
		t.Fatal("A U started nothing")
	}
	r.app.Update(cmd())
	if len(r.restores) != 1 || r.restores[0] != "yt_a" {
		t.Fatalf("restores = %v", r.restores)
	}
	if !strings.Contains(r.app.feedback.msg, "Restored the original 1 chapters: Irys tournament") {
		t.Errorf("feedback = %q", r.app.feedback.msg)
	}

	r.app.Update(chapterSaveResultMsg{Title: "T", View: ChapterImportView{Chapters: []ChapterImportRow{{}}, EmbedError: "the re-mux carries 0 chapter(s)"}})
	if !strings.Contains(r.app.feedback.msg, "saved beside the file, which could not be rewritten: the re-mux carries 0 chapter(s)") {
		t.Errorf("feedback = %q", r.app.feedback.msg)
	}
}

// TestChapterImportChordsFromTheKeyboard: "a h" on the selected job opens
// the overlay, and "a u u" — the confirm keypress — restores; "a u" alone
// does not.
//
// Mutant: the dispatchAction case for A C or A U dropped.
func TestChapterImportChordsFromTheKeyboard(t *testing.T) {
	r := newChapterRig(t)
	r.original = true
	press := func(s string) tea.Cmd {
		var last tea.Cmd
		for _, c := range s {
			_, last = r.app.Update(tea.KeyPressMsg{Code: c, Text: string(c)})
		}
		return last
	}
	press("au")
	if len(r.restores) != 0 {
		t.Fatal("A U restored without its confirm keypress")
	}
	cmd := press("u")
	if cmd == nil {
		t.Fatal("A U U started nothing")
	}
	for _, msg := range flattenCmd(cmd) {
		if m, ok := msg.(chapterSaveResultMsg); ok {
			r.app.Update(m)
		}
	}
	if len(r.restores) != 1 {
		t.Fatalf("restores = %v", r.restores)
	}
	r.app.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	press("ac")
	if !r.app.chapterDlg.IsVisible() {
		t.Fatal("A C from the keyboard did not open the overlay")
	}
}

// TestChapterImportPrefillsTheJoinPoint is owner decision CJ-3 in the A C
// overlay: the opening preview's default offset — a late-joined live
// capture's join point, negated — fills the empty offset field as a signed
// clock (owner decision CK-2: "-1:23:45", not "-5025"), and the overlay says
// where it came from, as the Web dialog does.
//
// Mutants: SetPreview not filling the field (empty); the field filled from
// the seconds ("-5025"); the note not drawn.
func TestChapterImportPrefillsTheJoinPoint(t *testing.T) {
	r := newChapterRig(t)
	r.previewFn = func(text, offset string) ChapterImportView {
		return ChapterImportView{Error: "No timestamps found", DefaultOffsetSeconds: -5025,
			DefaultOffsetText: "-1:23:45", DefaultOffsetNote: "joined 1:23:45 in"}
	}
	r.app.dispatchAction("A C", r.job)
	previewNow(t, r.app)
	if got := r.app.chapterDlg.offset.Value(); got != "-1:23:45" {
		t.Errorf("offset field = %q, want the pre-filled -1:23:45", got)
	}
	if v := r.app.chapterDlg.View(); !strings.Contains(v, "Pre-filled: joined 1:23:45 in") {
		t.Errorf("the overlay does not say where the offset came from:\n%s", v)
	}
}

// TestChapterImportOffsetNoteVariants: the overlay always says where the
// offset came from (owner decision CK-2) — after "Pre-filled: " when the
// field was pre-filled (a capture from the start and a VOD at "0:00"), and
// bare when the start is unknown and the field is left empty for the
// operator.
//
// Mutants: the note drawn only for a non-zero offset (the from-start and
// VOD notes missing); "Pre-filled: " in front of the unknown note; the
// field filled from DefaultOffsetSeconds ("0", not "0:00").
func TestChapterImportOffsetNoteVariants(t *testing.T) {
	for _, c := range []struct {
		name, text, note, wantField, wantLine, notLine string
	}{
		{"from the start", "0:00", "recorded from the start", "0:00", "Pre-filled: recorded from the start", ""},
		{"VOD", "0:00", "VOD", "0:00", "Pre-filled: VOD", ""},
		{"unknown", "", "start unknown — enter it by hand", "", "start unknown — enter it by hand", "Pre-filled: start unknown"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newChapterRig(t)
			r.previewFn = func(text, offset string) ChapterImportView {
				return ChapterImportView{Error: "No timestamps found", DefaultOffsetText: c.text, DefaultOffsetNote: c.note}
			}
			r.app.dispatchAction("A C", r.job)
			previewNow(t, r.app)
			if got := r.app.chapterDlg.offset.Value(); got != c.wantField {
				t.Errorf("offset field = %q, want %q", got, c.wantField)
			}
			v := r.app.chapterDlg.View()
			if !strings.Contains(v, c.wantLine) {
				t.Errorf("the overlay does not show %q:\n%s", c.wantLine, v)
			}
			if c.notLine != "" && strings.Contains(v, c.notLine) {
				t.Errorf("the overlay shows %q:\n%s", c.notLine, v)
			}
		})
	}
}
