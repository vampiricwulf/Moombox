package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// The bare forms of the job-work overlays: the action menu, Add Video, Trim,
// Import Archive and Import Chapters (owner rulings R-C10, R-C11).

// reinitSummary is a Reinitialize summary as long as the worker writes one
// (reinitialize_preview.go): six rows of a 30-column terminal.
const reinitSummary = "cancels the pending automatic mux, and deletes 1 set-aside recording, 2 recorded parts (1.2GB), 40 staged files (850.0MB) and 5 imported chapters"

// ---- fixtures ----

func bareFormats() *FormatsData {
	f := &FormatsData{VideoID: "dQw4w9WgXcQ", Title: "Never Gonna Give You Up (Official Video) 4K Remaster", StreamStatus: "vod"}
	res := [][2]int{{3840, 2160}, {1920, 1080}, {1920, 1080}, {1280, 720}, {1280, 720}, {854, 480}, {640, 360}, {426, 240}, {256, 144}, {256, 144}, {256, 144}}
	for i, r := range res {
		mime := "video/webm"
		if i%2 == 1 {
			mime = "video/mp4"
		}
		f.VideoFormats = append(f.VideoFormats, VideoFormat{
			Itag: 100 + i, MimeType: mime, Width: r[0], Height: r[1], Fps: 60, Bitrate: 18400000 / (i + 1),
			Damaged: i == 2, Segmented: i == 2 || i == 4,
		})
	}
	f.AudioFormats = []AudioFormat{
		{Itag: 251, MimeType: "audio/webm; codecs=opus", Bitrate: 160000, AudioSampleRate: "48000"},
		{Itag: 140, MimeType: "audio/mp4; codecs=mp4a", Bitrate: 128000, AudioSampleRate: "44100", Damaged: true},
	}
	f.BestItags = BestItags{BestVideo: 101, BestWebmVideo: 100, BestMp4Video: 101, BestAudio: 251, BestOpusAudio: 251, BestAacAudio: 140}
	return f
}

// bareAddVideo is Add Video at w x h on the step named: "url", "loading",
// "failed", "video", "picking", "audio", "times", "confirm".
func bareAddVideo(w, h int, step string) *AddVideoModel {
	m := NewAddVideoModel()
	m.SetSize(w, h)
	m.Open()
	if step == "url" {
		return m
	}
	m.urlInput = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	m.syncToTextInput()
	m.HandleKey("tab")
	m.HandleKey("enter")
	switch step {
	case "loading":
		return m
	case "failed":
		m.SetFetchError("could not fetch the formats: the video is private, sign in to a member account and try again")
		return m
	}
	m.SetFormats(bareFormats())
	if step == "video" {
		return m
	}
	m.UpdateComponents(namedKey("down"))
	m.UpdateComponents(namedKey("down"))
	if step == "picking" {
		return m
	}
	m.HandleKey("enter")
	if step == "audio" {
		return m
	}
	m.HandleKey("a")
	if step == "times" {
		return m
	}
	for _, r := range "1:30" {
		m.UpdateComponents(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m.HandleKey("enter")
	return m
}

func bareTrims() []TrimInfo {
	return []TrimInfo{
		{ID: "a", StartTime: 90, EndTime: 165, Duration: 75, FileSize: 45 << 20},
		{ID: "b", StartTime: 3900, EndTime: 7560, Duration: 3660, FileSize: 1300 << 20},
		{ID: "c", StartTime: 600, EndTime: 750, Duration: 150, FileSize: 80 << 20},
		{ID: "d", StartTime: 1, EndTime: 3, Duration: 2},
		{ID: "e", StartTime: 5, EndTime: 9, Duration: 4, FileSize: 1 << 20},
		{ID: "f", StartTime: 10, EndTime: 19, Duration: 9, FileSize: 2 << 20},
		{ID: "g", StartTime: 20, EndTime: 29, Duration: 9, FileSize: 3 << 20},
	}
}

// bareTrim is the trim dialog at w x h in the state named: "times",
// "times error", "confirm", "failed", "progress", "delete", "armed",
// "delete error", "deleting", "no trims".
func bareTrim(w, h int, state string) *TrimDialogModel {
	m := NewTrimDialogModel()
	m.SetSize(w, h)
	m.Open("j1", "Karaoke stream 2026-11-01 with a long title")
	m.SetJobMetadata(7425, 1<<30)
	typed := func(s string) {
		for _, r := range s {
			m.UpdateComponents(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
	}
	switch state {
	case "times":
		return m
	case "no trims":
		m.HandleKey("m")
		return m
	case "delete", "armed", "delete error", "deleting":
		m.SetTrims(bareTrims())
		m.HandleKey("m")
		m.HandleKey("down")
		if state != "delete" {
			m.HandleKey("enter")
		}
		switch state {
		case "delete error":
			m.SetError(longError)
		case "deleting":
			m.SetLoading(true)
		}
		return m
	}
	typed("1.30")
	if state == "times error" {
		m.textInput.Validate = nil
		typed("x")
		m.HandleKey("enter")
		return m
	}
	m.startTimeInput = "1:30"
	m.HandleKey("tab")
	typed("2:45")
	m.HandleKey("enter")
	switch state {
	case "failed":
		m.FailCreate("ffmpeg exited with status 1: the encoder could not be opened")
	case "progress":
		m.StartProgress()
		m.SetProgress(42.3, 72*time.Second)
	}
	return m
}

// bareImportDir is a folder with one directory and seven files, three of
// them not archives.
func bareImportDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range []string{"hololive-karaoke-2026-11-01-with-a-long-name.zip", "STREAM.ZIP", "notes.txt", "b.zip", "c.zip", "d.zip", "e.zip"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("PK"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "2026-10"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// bareImport is the archive import at w x h over dir in the state named:
// "pick", "meta", "refused", "importing".
func bareImport(t *testing.T, w, h int, dir, state string) *ImportDialogModel {
	t.Helper()
	m := NewImportDialogModel()
	m.SetSize(w, h)
	m.UpdateComponents(m.Open(dir)())
	if state == "pick" {
		return m
	}
	m.UpdateComponents(namedKey("down")) // past the directory, onto STREAM.ZIP
	m.UpdateComponents(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.step != importStepMeta {
		t.Fatalf("premise lost: Enter did not pick an archive (step %d)", m.step)
	}
	for _, r := range "My title" {
		m.UpdateComponents(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	switch state {
	case "refused":
		m.SetError(longError)
	case "importing":
		m.HandleKey("enter")
	}
	return m
}

// bareChapters is the chapter import at w x h over a four-line paste whose
// preview answered with n chapters, replacing 8, in the state named:
// "reading", "edit", "worst" (a two-row source note and a refused save),
// "parse error", "confirm", "saving".
func bareChapters(w, h, n int, state string) *ChapterImportDialogModel {
	m := NewChapterImportDialogModel()
	m.SetSize(w, h)
	m.Open(&database.Job{ID: "j1", Title: "Karaoke stream 2026-11-01"})
	if state == "reading" {
		return m
	}
	m.text.SetValue("0:00 One\n1:00 Two\n2:00 Three\n3:00 Four")
	m.text.MoveToBegin()
	v := ChapterImportView{Current: make([]ChapterImportRow, 8), OffsetSeconds: -5025, DefaultOffsetText: "-1:23:45", DefaultOffsetNote: "joined 1:23:45 in"}
	for i := range n {
		v.Chapters = append(v.Chapters, ChapterImportRow{Start: float64(i * 60), Title: fmt.Sprintf("chapter-%02d-", i)})
	}
	switch state {
	case "worst":
		v.DefaultOffsetNote = "recorded from the start"
	case "parse error":
		v.Chapters, v.Error = nil, "No timestamps found in the pasted text; a line needs a time such as 12:34 before its title"
	}
	m.SetPreview(m.seq, v, nil)
	switch state {
	case "worst":
		m.SetSaveError(errors.New("The job is Muxing; wait for it to finish"))
	case "confirm", "saving":
		m.HandleKey("ctrl+s")
		if state == "saving" {
			m.SetSaving()
		}
	}
	return m
}

// bareMenuApp is an App of w x h with the compact rig's jobs and the action
// menu open through the M key.
func bareMenuApp(t *testing.T, w, h int) *compactRig {
	t.Helper()
	r := newCompactRig(t, w, h)
	r.app.ReinitializeSummary = func([]string) string { return reinitSummary }
	r.press("m")
	if !r.app.actionMenu.IsVisible() {
		t.Fatalf("premise lost: M did not open the menu at %dx%d: %q", w, h, r.app.feedback.msg)
	}
	return r
}

// menuTo moves the main list's cursor onto chord.
func menuTo(t *testing.T, r *compactRig, chord string) {
	t.Helper()
	m := r.app.actionMenu
	r.press("home")
	for range len(m.mainList.Items()) {
		if it := m.selectedAction(); it != nil && it.Chord == chord {
			return
		}
		r.press("down")
	}
	t.Fatalf("the menu has no %q", chord)
}

var (
	addVideoStates = []string{"url", "loading", "failed", "video", "picking", "audio", "times", "confirm"}
	trimStates     = []string{"times", "times error", "confirm", "failed", "progress", "delete", "armed", "delete error", "deleting", "no trims"}
	importStates   = []string{"pick", "meta", "refused", "importing"}
	chapterStates  = []string{"reading", "edit", "worst", "parse error", "confirm", "saving"}
)

// belowTitle is a view without its first row.
func belowTitle(view string) string {
	_, rest, _ := strings.Cut(view, "\n")
	return rest
}

// ---- exactly width x height, at every compact size ----

// Every state of the five overlays is exactly the terminal's size at every
// compact size at or above the state's minimum, wide-short and narrow-tall
// included.
//
// Mutant: an error appended without taking a row; an input a cell too wide;
// a list page one row too tall.
func TestBareJobWorkFillsEveryCompactSize(t *testing.T) {
	dir := bareImportDir(t)
	compactSizes(func(w, h int) {
		at := func(what, state string) string { return fmt.Sprintf("%s, %s at %dx%d", what, state, w, h) }
		for _, s := range addVideoStates {
			m := bareAddVideo(w, h, s)
			wantExact(t, at("add video", s), m.View(), w, h)
			m.startSubmit()
			wantExact(t, at("add video, submitting", s), m.View(), w, h)
		}
		m := bareAddVideo(w, h, "url")
		m.urlInput = "https://twitch.tv/somebody/clips"
		m.syncToTextInput()
		m.HandleKey("enter")
		wantExact(t, at("add video", "the clips refusal"), m.View(), w, h)
		m = bareAddVideo(w, h, "confirm")
		none := -1
		m.selectedVideoItag, m.selectedAudioItag = &none, &none
		m.HandleKey("enter")
		wantExact(t, at("add video", "none and none"), m.View(), w, h)

		for _, s := range trimStates {
			wantExact(t, at("trim", s), bareTrim(w, h, s).View(), w, h)
		}
		for _, s := range importStates {
			wantExact(t, at("import", s), bareImport(t, w, h, dir, s).View(), w, h)
		}
		empty := bareImport(t, w, h, t.TempDir(), "pick")
		wantExact(t, at("import", "an empty folder"), empty.View(), w, h)

		for _, s := range chapterStates {
			c := bareChapters(w, h, 12, s)
			if nw, nh := c.bareNeed(); w < nw || h < nh {
				continue // held under the notice below its state's size
			}
			wantExact(t, at("chapters", s), c.View(), w, h)
		}

		r := bareMenuApp(t, w, h)
		a := r.app
		wantExact(t, at("menu", "main"), a.actionMenu.View(), w, h)
		for range len(a.actionMenu.mainList.Items()) {
			r.press("down")
			wantExact(t, at("menu", "each row"), a.actionMenu.View(), w, h)
		}
		menuTo(t, r, "A I")
		r.press("enter")
		wantExact(t, at("menu", "jobs"), a.actionMenu.View(), w, h)
		r.press("enter")
		wantExact(t, at("menu", "confirm or its refusal"), a.actionMenu.View(), w, h)
		a.actionMenu.jobConfirm, a.actionMenu.mode = false, menuMain
		menuTo(t, r, "R P")
		r.press("enter")
		wantExact(t, at("menu", "restart confirm"), a.actionMenu.View(), w, h)
	})
}

// A populated overlay shrunk to its smallest drawing, through a wide-short
// and a narrow-tall terminal, and grown back draws the frame of one that
// was never shrunk: nothing the bare form sets on an input, a table, a
// list, the picker or the paste box outlives it.
//
// Mutant: a text input left at its bare width; the paste box's short
// placeholder left in place; the picker left at the bare height.
func TestBareJobWorkSurvivesAShrinkAndBack(t *testing.T) {
	type dialog interface {
		SetSize(w, h int)
		View() string
	}
	dir := bareImportDir(t)
	builds := map[string]func(w, h int) dialog{}
	for _, s := range addVideoStates {
		builds["add video, "+s] = func(w, h int) dialog { return bareAddVideo(w, h, s) }
	}
	for _, s := range trimStates {
		builds["trim, "+s] = func(w, h int) dialog { return bareTrim(w, h, s) }
	}
	for _, s := range importStates {
		builds["import, "+s] = func(w, h int) dialog { return bareImport(t, w, h, dir, s) }
	}
	for _, s := range chapterStates {
		builds["chapters, "+s] = func(w, h int) dialog { return bareChapters(w, h, 12, s) }
	}
	builds["menu"] = func(w, h int) dialog {
		r := newCompactRig(t, w, h)
		r.press("m", "down", "down")
		return r.app.actionMenu
	}
	builds["menu, jobs"] = func(w, h int) dialog {
		r := newCompactRig(t, w, h)
		r.press("m")
		menuTo(t, r, "A D")
		r.press("enter", "down")
		return r.app.actionMenu
	}
	for name, build := range builds {
		for _, normal := range [][2]int{{60, 20}, {120, 40}} {
			want := build(normal[0], normal[1]).View()
			m := build(normal[0], normal[1])
			for _, s := range [][2]int{{30, 10}, {200, 8}, {30, 50}, {30, 6}, {40, 10}} {
				m.SetSize(s[0], s[1])
				wantExact(t, fmt.Sprintf("%s shrunk to %dx%d", name, s[0], s[1]), m.View(), s[0], s[1])
			}
			m.SetSize(normal[0], normal[1])
			if got := m.View(); got != want {
				t.Errorf("%s: the %dx%d frame changed across a shrink:\n%s\nnever shrunk:\n%s", name, normal[0], normal[1], ansi.Strip(got), ansi.Strip(want))
			}
		}
	}
}

// ---- the action menu ----

func TestBareMenuFrames(t *testing.T) {
	r := bareMenuApp(t, 30, 6)
	a := r.app
	wantFrame(t, "menu at 30x6", a.View().Content, 30, 6, []string{
		"Menu · Action             1/10",
		"A A Add Video",
		"A Z Import Archive",
		"A R Resume Job",
		"A I Reinitialize Job",
		"Enter  PgUp/PgDn  Esc",
	})
	// A page is a page of actions: no category row, no spacer, and the
	// title follows the selected action's category.
	r.press("pgdown", "pgdown", "pgdown", "pgdown")
	rows := plainRows(a.View().Content)
	if rows[0] != "Menu · Request            5/10" || !strings.HasPrefix(rows[1], "R ") {
		t.Errorf("four pages down:\n%s", strings.Join(rows, "\n"))
	}
	if it := a.actionMenu.selectedAction(); it == nil || it.Category != "Request" {
		t.Errorf("four pages down the cursor is on %+v", it)
	}
	r.press("pgup")
	if got := plainRows(a.View().Content)[0]; got != "Menu · Action             4/10" {
		t.Errorf("a page back: %q", got)
	}

	// A label too long for the row is cut; the chord never is.
	menuTo(t, r, "A S")
	if got := strings.Join(plainRows(a.View().Content), "\n"); !strings.Contains(got, "\nA S Recover Set-aside Recordi…\n") {
		t.Errorf("a long label:\n%s", got)
	}
}

// An action whose overlay needs a larger terminal is dimmed, says the size
// on the last row, and Enter leaves the menu as it is: the same predicate
// the chord's refusal and Help's list use.
//
// Mutant: sizeRefusal answering "" — Enter closes the menu onto the
// feedback line's refusal.
func TestBareMenuDimsWhatNeedsALargerTerminal(t *testing.T) {
	r := bareMenuApp(t, 40, 9)
	a := r.app
	for chord, need := range map[string]string{"E L": "needs 60x20", "A C": "needs 30x10"} {
		menuTo(t, r, chord)
		rows := plainRows(a.View().Content)
		if rows[len(rows)-1] != need {
			t.Errorf("%s selected: the last row is %q, want %q", chord, rows[len(rows)-1], need)
		}
		r.press("enter")
		if !a.actionMenu.IsVisible() || a.actionMenu.mode != menuMain || a.feedback.msg != "" {
			t.Errorf("%s: Enter did something: visible %v, mode %d, feedback %q", chord, a.actionMenu.IsVisible(), a.actionMenu.mode, a.feedback.msg)
		}
		if refused, _, _ := chordRefusedAt(chord, 40, 9); !refused {
			t.Errorf("%s is dimmed and chordCompact says it opens at 40x9", chord)
		}
	}
	// One that opens here runs.
	menuTo(t, r, "A O")
	if got := plainRows(a.View().Content); got[len(got)-1] != "Enter  PgUp/PgDn  Esc" {
		t.Errorf("A O selected: the last row is %q", got[len(got)-1])
	}
	r.press("enter")
	if !a.filesDlg.IsVisible() || a.actionMenu.IsVisible() {
		t.Error("Enter on A O did not open the orphaned items")
	}
	wantExact(t, "the overlay the menu opened", a.View().Content, 40, 9)
}

// A row with no job to act on says why on the last row, whole: a reason too
// long for one row takes a second, and never the selected row.
func TestBareMenuReasonIsWhole(t *testing.T) {
	r := newCompactRig(t, 30, 6)
	a := r.app
	a.JobAsides = func(string) AsideSummary { return AsideSummary{} }
	a.Update(JobsUpdateMsg{Jobs: []*database.Job{{ID: "q", Title: "queued", Status: database.StatusQueued, Platform: "youtube", VideoID: "aaaaaaaaaaa"}}})
	r.press("m")
	for _, chord := range []string{"A S", "A M", "A D"} {
		menuTo(t, r, chord)
		item := a.actionMenu.selectedAction()
		mi := a.actionMenu.mainList.SelectedItem().(menuActionItem)
		if !mi.noJobs {
			r.press("enter") // the disk probe finds none, and the row dims
		}
		plain := plainRows(a.View().Content)
		wantExact(t, chord+" with no job", a.View().Content, 30, 6)
		joined := strings.Join(plain[1:], " ")
		if !strings.Contains(strings.Join(strings.Fields(joined), " "), item.DisabledReason) {
			t.Errorf("%s: the reason %q is not whole on screen:\n%s", chord, item.DisabledReason, strings.Join(plain, "\n"))
		}
		if !strings.Contains(strings.Join(plain, "\n"), "\n"+chord+" ") {
			t.Errorf("%s: the reason covers the selected row:\n%s", chord, strings.Join(plain, "\n"))
		}
	}
}

// Reinitialize through the menu (owner rulings PR-M1, R-D2, R-C11): where
// the summary of what it deletes does not fit the confirm page whole, the
// first Enter does not arm, the page names the size that shows it, and no
// number of Enters reinitializes. Where it fits, the page shows it whole.
//
// Mutant: bareRefuseConfirm never refusing — the second Enter reinitializes
// with the summary cut. Mutant: the page cutting the detail to its rows.
func TestBareMenuReinitializeNeedsItsSummaryWhole(t *testing.T) {
	r := bareMenuApp(t, 30, 6)
	a := r.app
	menuTo(t, r, "A I")
	r.press("enter")
	if a.actionMenu.mode != menuJobSelect {
		t.Fatalf("premise lost: Enter on A I did not open its jobs: mode %d", a.actionMenu.mode)
	}
	wantFrame(t, "A I's jobs at 30x6", a.View().Content, 30, 6, []string{
		"Reinit: pick (3)",
		"✗ A broken stream",
		"⏣ Members only",
		"⊘ A cancelled job",
		"",
		"Enter: Select  Esc: Back",
	})
	r.press("enter")
	if a.actionMenu.jobConfirm {
		t.Fatal("the confirm was armed with a summary that does not fit")
	}
	// Six rows of summary under the action and the job, above the keys.
	wantFrame(t, "the refusal at 30x6", a.View().Content, 30, 6, []string{
		"Reinit: pick (3)",
		"✗ A broken stream",
		"⏣ Members only",
		"⊘ A cancelled job",
		"Enlarge the terminal to 30x9",
		"for this",
	})
	for range 3 {
		r.pressRunLast("enter")
	}
	if len(r.reinit) != 0 || !a.actionMenu.IsVisible() {
		t.Fatalf("Enter reinitialized %v without showing what it deletes", r.reinit)
	}

	// At the size it names, the same two Enters ask and then act.
	a.Update(tea.WindowSizeMsg{Width: 30, Height: 9})
	r.press("enter")
	if !a.actionMenu.jobConfirm {
		t.Fatal("the confirm was not armed at the size the refusal named")
	}
	page := plainRows(a.View().Content)
	wantExact(t, "the confirm page at 30x9", a.View().Content, 30, 9)
	if page[0] != "Reinitialize Job?" || page[1] != "✗ A broken stream" || page[8] != "Enter confirm  Esc cancel" {
		t.Errorf("the confirm page:\n%s", strings.Join(page, "\n"))
	}
	if got := strings.Join(page[2:8], " "); got != reinitSummary {
		t.Errorf("the page does not show the summary whole, in the worker's words:\n%s", strings.Join(page, "\n"))
	}

	// Armed, then shrunk: the summary is no longer whole, so it is not
	// shown cut and Enter does not confirm.
	a.Update(tea.WindowSizeMsg{Width: 30, Height: 6})
	shrunk := strings.Join(plainRows(a.View().Content), "\n")
	if strings.Contains(shrunk, "deletes") || !strings.Contains(shrunk, "Enlarge the terminal to 30x9") || strings.Contains(shrunk, "Enter confirm") {
		t.Errorf("an armed confirm shrunk below its size:\n%s", shrunk)
	}
	r.pressRunLast("enter")
	if len(r.reinit) != 0 {
		t.Fatalf("Enter confirmed a summary that was not on screen: %v", r.reinit)
	}
	a.Update(tea.WindowSizeMsg{Width: 30, Height: 9})
	r.press("enter")
	r.pressRunLast("enter")
	if len(r.reinit) != 1 || r.reinit[0] != "err" {
		t.Errorf("the confirmed reinitialize ran %v, want [err]", r.reinit)
	}
}

// A confirm with nothing to add (Delete) is a page of its own too, and Esc
// goes back a step at a time.
func TestBareMenuConfirmPagesAndEsc(t *testing.T) {
	r := bareMenuApp(t, 30, 6)
	a := r.app
	menuTo(t, r, "A D")
	r.press("enter", "enter")
	page := plainRows(a.View().Content)
	if page[0] != "Delete Job?" || page[5] != "Enter confirm  Esc cancel" {
		t.Errorf("the delete confirm:\n%s", strings.Join(page, "\n"))
	}
	r.press("esc")
	if !a.actionMenu.IsVisible() || a.actionMenu.mode != menuJobSelect || a.actionMenu.jobConfirm {
		t.Fatal("Esc on the confirm page did not go back to the jobs")
	}
	r.press("esc")
	if a.actionMenu.mode != menuMain {
		t.Fatal("Esc on the jobs did not go back to the menu")
	}
	menuTo(t, r, "R P")
	r.press("enter")
	wantFrame(t, "the restart confirm", a.View().Content, 30, 6, []string{"Restart Program?", "", "", "", "", "Enter confirm  Esc cancel"})
	r.pressRunLast("enter")
	if r.restarted != 1 {
		t.Errorf("the confirmed restart ran %d times", r.restarted)
	}
}

// The mouse over the bare menu: a click on a row selects that action, the
// wheel moves a row, and neither reaches the task list underneath.
func TestBareMenuMouse(t *testing.T) {
	r := bareMenuApp(t, 30, 6)
	a := r.app
	cursor := a.taskList.list.Index()
	a.Update(tea.MouseClickMsg{X: 5, Y: 3, Button: tea.MouseLeft})
	if it := a.actionMenu.selectedAction(); it == nil || it.Chord != "A R" {
		t.Errorf("a click on the third row selected %+v", it)
	}
	a.Update(tea.MouseWheelMsg{X: 5, Y: 3, Button: tea.MouseWheelDown})
	if it := a.actionMenu.selectedAction(); it == nil || it.Chord != "A I" {
		t.Errorf("a wheel notch moved to %+v", it)
	}
	a.Update(tea.MouseClickMsg{X: 5, Y: 0, Button: tea.MouseLeft}) // the title
	a.Update(tea.MouseClickMsg{X: 5, Y: 5, Button: tea.MouseLeft}) // the keys
	if it := a.actionMenu.selectedAction(); it == nil || it.Chord != "A I" || !a.actionMenu.IsVisible() {
		t.Errorf("a click off the list changed the menu: %+v", it)
	}
	if a.taskList.list.Index() != cursor {
		t.Error("the mouse reached the task list under the menu")
	}
}

// ---- Add Video ----

func TestBareAddVideoFrames(t *testing.T) {
	wantFrame(t, "the URL step", bareAddVideo(30, 6, "url").View(), 30, 6, []string{
		"Add Video",
		"URL, ID or Twitch channel:",
		">",
		"[ ] Advanced (Tab)",
		"",
		"Enter: Add  Esc: Cancel",
	})
	wantFrame(t, "the video step on Auto", bareAddVideo(40, 10, "video").View(), 40, 10, []string{
		"Video 1/4                    Enter: Auto",
		"  1  3840x2160 60 WEBM 18400k *",
		"  2  1920x1080 60 MP4  9200k  *",
		"  3  1920x1080 60 WEBM 6133k  !",
		"  4  1280x720  60 MP4  4600k",
		"  5  1280x720  60 WEBM 3680k  !",
		"  6  854x480   60 MP4  3066k",
		"  7  640x360   60 WEBM 2628k",
		"* Best  ! damaged/segmented",
		"Enter/A:Auto ↑↓:Pick N:None 1-9 Esc",
	})
	// The first Down shows the cursor on the Best row, the second moves it
	// to the damaged, segmented one: its note is spelled out under the list.
	wantFrame(t, "the video step, picking", bareAddVideo(30, 6, "picking").View(), 30, 6, []string{
		"Video 1/4                 3/11",
		"  2  1920x1080 60 MP4  *",
		"> 3  1920x1080 60 WEBM !",
		"  4  1280x720  60 MP4",
		"damaged, segmented",
		"Ent:Pick A:Auto N:None 1-9 Esc",
	})
	wantFrame(t, "the audio step", bareAddVideo(30, 6, "audio").View(), 30, 6, []string{
		"Audio 2/4          Enter: Auto",
		"  1 160k 48kHz OPUS *",
		"  2 128k 44kHz AAC  *!",
		"",
		"* Best  ! damaged/segmented",
		"Ent/A:Auto ↑↓ N:None 1-9 Esc",
	})
	wantFrame(t, "the times step", bareAddVideo(30, 6, "times").View(), 30, 6, []string{
		"Times 3/4 (blank = all)",
		"> Start:",
		"  End:",
		"H:MM:SS, MM:SS or seconds",
		"",
		"Tab Enter:Next Esc:Back",
	})
	wantFrame(t, "the confirm", bareAddVideo(30, 6, "confirm").View(), 30, 6, []string{
		"Add 4/4  dQw4w9WgXcQ",
		"Never Gonna Give You Up (Offi…",
		"V: 1920x1080@60 WEBM",
		"A: Auto (best)",
		"T: 1:30 - (end)",
		"Enter: Add  Esc: Back",
	})
}

// Every row of a rendition that is damaged or segmented carries "!", and
// every Best pick "*", whether or not the cursor is on it: a digit key takes
// a row the cursor never visited.
//
// Mutant: the marks drawn for the highlighted row only.
func TestBareFormatRowsKeepTheirMarks(t *testing.T) {
	for _, size := range [][2]int{{30, 6}, {30, 50}, {40, 10}, {200, 8}} {
		w, h := size[0], size[1]
		m := bareAddVideo(w, h, "video")
		f := m.formats
		seen := map[int]bool{}
		for range len(f.VideoFormats) + 1 {
			for _, row := range plainRows(m.View()) {
				fields := strings.Fields(strings.TrimPrefix(row, ">"))
				var n int
				if len(fields) < 2 || !strings.Contains(fields[1], "x") {
					continue
				}
				if _, err := fmt.Sscanf(fields[0], "%d", &n); err != nil {
					continue
				}
				vf := f.VideoFormats[n-1]
				seen[n] = true
				marks := ""
				if last := fields[len(fields)-1]; strings.Trim(last, "*!") == "" {
					marks = last
				}
				best := vf.Itag == f.BestItags.BestVideo || vf.Itag == f.BestItags.BestWebmVideo || vf.Itag == f.BestItags.BestMp4Video
				if strings.Contains(marks, "!") != (vf.Damaged || vf.Segmented) || strings.Contains(marks, "*") != best {
					t.Errorf("%v: row %d is marked %q (damaged %v, segmented %v, best %v): %q", size, n, marks, vf.Damaged, vf.Segmented, best, row)
				}
			}
			m.UpdateComponents(namedKey("down"))
		}
		if len(seen) != len(f.VideoFormats) {
			t.Errorf("%v: walking the list showed %d of %d rows", size, len(seen), len(f.VideoFormats))
		}
	}

	// A digit takes its row from the list as drawn, marked or not.
	m := bareAddVideo(30, 6, "video")
	m.HandleKey("3")
	if m.selectedVideoItag == nil || *m.selectedVideoItag != 102 || m.step != AddStepAudioFormat {
		t.Errorf("3 picked %v", m.selectedVideoItag)
	}
	// The longest note the table writes fits the note row in its short form.
	if got := bareNote("Best, Best WEBM, damaged, segmented", 30); got != "Best, Best WEBM, dmg, seg" {
		t.Errorf("the longest note at 30 columns: %q", got)
	}
	if got := bareNote("Best, Best WEBM, damaged, segmented", 40); got != "Best, Best WEBM, damaged, segmented" {
		t.Errorf("the longest note at 40 columns: %q", got)
	}
}

// PgDn and PgUp move by the rows the bare list draws, and the cursor's row
// is always one of them.
//
// Mutant: the keys left to the table, which pages by its own height.
func TestBareFormatListPagesByWhatItDraws(t *testing.T) {
	m := bareAddVideo(30, 6, "picking")
	at := m.videoTable.Cursor()
	m.UpdateComponents(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if got := m.videoTable.Cursor(); got != at+3 {
		t.Errorf("PgDn moved the cursor from row %d to %d, want %d (three rows are drawn)", at, got, at+3)
	}
	m.UpdateComponents(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if got := m.videoTable.Cursor(); got != at {
		t.Errorf("PgUp came back to row %d, want %d", got, at)
	}
	for range len(m.formats.VideoFormats) {
		if !strings.Contains(ansi.Strip(m.View()), fmt.Sprintf("\n> %d ", m.videoTable.Cursor()+1)) {
			t.Fatalf("the cursor's row %d is not drawn:\n%s", m.videoTable.Cursor()+1, ansi.Strip(m.View()))
		}
		m.UpdateComponents(namedKey("down"))
	}
	// A normal-size table is paged by the table, as before.
	n := bareAddVideo(120, 40, "picking")
	before := n.videoTable.Cursor()
	n.UpdateComponents(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if n.videoTable.Cursor() == before+3 && n.videoTable.Height() != 3 {
		t.Error("the bare page size reached a normal-size table")
	}
}

// An error on the URL step gives the title, then the prompt, then the
// checkbox away, and never the input (owner ruling R-C11).
func TestBareAddVideoErrorsNeverCoverTheInput(t *testing.T) {
	m := bareAddVideo(30, 6, "url")
	m.urlInput = "twitch.tv/x/clips"
	m.syncToTextInput()
	m.HandleKey("enter")
	wantFrame(t, "the clips refusal at 30x6", m.View(), 30, 6, []string{
		"> twitch.tv/x/clips",
		"Twitch clip lists are not",
		"supported yet. Use a channel,",
		"VOD, clip, video list or",
		"collection URL.",
		"Enter: Add  Esc: Cancel",
	})
	m = bareAddVideo(30, 6, "url")
	m.HandleKey("enter") // nothing typed
	wantFrame(t, "a one-row error at 30x6", m.View(), 30, 6, []string{
		"Add Video",
		"URL, ID or Twitch channel:",
		">",
		"[ ] Advanced (Tab)",
		"Please enter a URL or video ID",
		"Enter: Add  Esc: Cancel",
	})
	// A two-row error takes the title's row, a three-row one the prompt's.
	m.errorMsg = "Twitch clip lists are not supported yet. Use a channel."
	if rows := plainRows(m.View()); rows[0] != "URL, ID or Twitch channel:" || rows[1] != ">" || rows[2] != "[ ] Advanced (Tab)" {
		t.Errorf("a two-row error at 30x6:\n%s", strings.Join(rows, "\n"))
	}
	compactSizes(func(w, h int) {
		m := bareAddVideo(w, h, "url")
		m.urlInput = "twitch.tv/x/clips"
		m.syncToTextInput()
		m.HandleKey("enter")
		if got := ansi.Strip(m.View()); !strings.Contains(got, "> twitch.tv/x/clips") {
			t.Errorf("%dx%d: the input is not on screen under the error:\n%s", w, h, got)
		}
	})

	// The confirm's one error: its rows stay, the title gives way, and at 30
	// columns it is worded to fit one row.
	m = bareAddVideo(30, 6, "confirm")
	none := -1
	m.selectedVideoItag, m.selectedAudioItag = &none, &none
	m.HandleKey("enter")
	wantFrame(t, "none and none at 30x6", m.View(), 30, 6, []string{
		"Add 4/4  dQw4w9WgXcQ",
		"V: None (audio only)",
		"A: None (video only)",
		"T: 1:30 - (end)",
		"Video and audio are both None",
		"Enter: Add  Esc: Back",
	})
}

// A long URL typed in the bare form scrolls inside its row: the cursor's
// cell stays on screen.
func TestBareAddVideoInputKeepsItsCursorCell(t *testing.T) {
	r := newCompactRig(t, 30, 6)
	a := r.app
	r.press("a", "a")
	if !a.addVideo.IsVisible() || a.screenHeld() {
		t.Fatalf("A A did not open Add Video bare at 30x6: %q", a.feedback.msg)
	}
	typeIntoApp(a, "https://www.youtube.com/watch?v=dQw4w9WgXcQ")
	wantExact(t, "a long URL at 30x6", a.View().Content, 30, 6)
	row := plainRows(a.View().Content)[2]
	if !strings.HasSuffix(row, "dQw4w9WgXcQ") || ansi.StringWidth(row) > 29 {
		t.Errorf("the input row does not end at the cursor: %q", row)
	}
	if a.addVideo.urlInput != "https://www.youtube.com/watch?v=dQw4w9WgXcQ" {
		t.Errorf("typed once? %q", a.addVideo.urlInput)
	}
	// Grown, it is the dialog a normal terminal draws for the same typing.
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	n := newCompactRig(t, 120, 40)
	n.press("a", "a")
	typeIntoApp(n.app, "https://www.youtube.com/watch?v=dQw4w9WgXcQ")
	if a.View().Content != n.app.View().Content {
		t.Errorf("grown back:\n%s\nwant:\n%s", plainFrame(a), plainFrame(n.app))
	}
}

// ---- Trim ----

func TestBareTrimFrames(t *testing.T) {
	wantFrame(t, "times", bareTrim(30, 6, "times").View(), 30, 6, []string{
		"Trim: Karaoke stream 2026-11-…",
		"Length 2:03:45",
		"> Start:",
		"  End:",
		"H:MM:SS, MM:SS or seconds",
		"Tab Enter M:Del mode Esc",
	})
	// The 58-cell format error is two rows: the job's title gives way, the
	// length (the range errors refer to it) and both fields stay.
	wantFrame(t, "times, the format error", bareTrim(30, 6, "times error").View(), 30, 6, []string{
		"Length 2:03:45",
		"> Start: 1.30x",
		"  End:",
		"Invalid start time format (use",
		"HH:MM:SS, MM:SS, or seconds)",
		"Tab Enter M:Del mode Esc",
	})
	wantFrame(t, "confirm", bareTrim(30, 6, "confirm").View(), 30, 6, []string{
		"Create trim?",
		"1:30 - 2:45  (1m 15s)",
		"About 10.3MB",
		"Re-encodes into a new file.",
		"",
		"Enter:Create M:Del mode Esc",
	})
	// Under the title row, which holds the spinner's frame.
	wantFrame(t, "progress", belowTitle(bareTrim(30, 6, "progress").View()), 30, 5, []string{
		"█████████████░░░░░░░░░░░░░░░░░",
		"Elapsed 1m12s",
		"",
		"",
		"Esc: keep running, close",
	})
	if got := plainRows(bareTrim(30, 6, "progress").View())[0]; !strings.Contains(got, "Creating trim") || !strings.HasSuffix(got, "42.3%") {
		t.Errorf("the progress title: %q", got)
	}
	wantFrame(t, "delete, armed", bareTrim(30, 6, "armed").View(), 30, 6, []string{
		"Delete trim                2/7",
		"  1:30-2:45 1m 15s 45.0MB",
		"> 1:05:00-2:06:00 1h 1m 1.3GB",
		"  10:00-12:30 2m 30s 80.0MB",
		"Enter again deletes this trim",
		"↑↓ Enter:Delete M/Esc:New",
	})
	wantFrame(t, "no trims", bareTrim(30, 6, "no trims").View(), 30, 6, []string{
		"Delete trim", "No trims to delete", "", "", "", "M/Esc: Create mode",
	})
}

// The trim's two confirms are whole at every compact size (owner ruling
// R-C11), a failed create's error never covers the range and is whole in
// one block (the warning gives way to it where both do not fit),
// and the selected trim is always in the delete list's window.
//
// Mutant: a window that pages instead of following the cursor past a
// message row; the warning cut to make room for an error.
func TestBareTrimConfirmsAreWhole(t *testing.T) {
	compactSizes(func(w, h int) {
		for _, state := range []string{"confirm", "failed"} {
			m := bareTrim(w, h, state)
			got := ansi.Strip(m.View())
			wants := []string{"1:30 - 2:45  (1m 15s)", "Re-encodes into a new file."}
			if state == "failed" {
				wants = wants[:1]
				if flat := strings.Join(strings.Fields(got), " "); !strings.Contains(flat, m.errorMsg) {
					t.Errorf("%dx%d: the failure is not whole in one block:\n%s", w, h, got)
				}
			}
			for _, want := range wants {
				if !strings.Contains(got, want) {
					t.Errorf("%dx%d, %s: %q is not on screen:\n%s", w, h, state, want, got)
				}
			}
		}
		for _, state := range []string{"armed", "delete error", "deleting"} {
			m := bareTrim(w, h, state)
			for i := range m.trims {
				m.selectedTrimIdx = i
				if state == "armed" {
					m.deleteConfirmID = m.trims[i].ID
				}
				got := ansi.Strip(m.View())
				row := "> " + formatTimeSeconds(m.trims[i].StartTime) + "-" + formatTimeSeconds(m.trims[i].EndTime)
				if !strings.Contains(got, row) {
					t.Errorf("%dx%d, %s: the selected trim %d is not on screen:\n%s", w, h, state, i, got)
				}
				if state == "armed" && !strings.Contains(got, bareTrimArmed) {
					t.Errorf("%dx%d: the armed line is not whole:\n%s", w, h, got)
				}
			}
		}
		// An error on the times step never covers a field.
		got := ansi.Strip(bareTrim(w, h, "times error").View())
		if !strings.Contains(got, "> Start: 1.30x") || !strings.Contains(got, "  End:") {
			t.Errorf("%dx%d: an error covers a field:\n%s", w, h, got)
		}
	})
	// A range is never what a narrow row cuts.
	long := TrimInfo{StartTime: 36000, EndTime: 86399, Duration: 50399, FileSize: 123 << 30}
	if got := bareTrimRow(long, 28); !strings.HasPrefix(got, "10:00:00-23:59:59") || ansi.StringWidth(got) > 28 {
		t.Errorf("a long row in 28 cells: %q", got)
	}
	if got := bareTrimRow(long, 18); got != "10:00:00-23:59:59" {
		t.Errorf("a long row in 18 cells: %q", got)
	}
}

// The trim through the App in a 30x6 terminal: typed, confirmed, and the
// wheel moves through the delete list without reaching the task list. The
// progress tick stays fast while the bare progress is on screen.
func TestBareTrimThroughTheApp(t *testing.T) {
	r := newCompactRig(t, 30, 6)
	a := r.app
	r.selectJob(t, "done")
	cursor := a.taskList.list.Index()
	r.press("a", "t")
	if !a.trimDlg.IsVisible() || a.screenHeld() {
		t.Fatalf("A T did not open the trim bare at 30x6: %q", a.feedback.msg)
	}
	typeIntoApp(a, "130")
	r.press("tab")
	typeIntoApp(a, "245")
	r.press("enter")
	if a.trimDlg.createStep != trimStepConfirm || a.trimDlg.startTimeInput != "130" {
		t.Fatalf("the times were not taken: step %d, start %q, error %q", a.trimDlg.createStep, a.trimDlg.startTimeInput, a.trimDlg.errorMsg)
	}
	wantExact(t, "the confirm through the App", a.View().Content, 30, 6)

	a.trimDlg.SetTrims(bareTrims())
	r.press("m")
	a.Update(tea.MouseWheelMsg{X: 3, Y: 2, Button: tea.MouseWheelDown})
	if a.trimDlg.selectedTrimIdx != 1 || a.taskList.list.Index() != cursor {
		t.Errorf("a wheel notch: trim %d, the task list at %d (was %d)", a.trimDlg.selectedTrimIdx, a.taskList.list.Index(), cursor)
	}

	a.trimDlg.HandleKey("m")
	a.trimDlg.StartProgress()
	a.trimInProgress = true
	if !a.wantsFastProgress() {
		t.Error("the bare progress is on screen and the tick idles")
	}
}

// ---- Import Archive ----

func TestBareImportFrames(t *testing.T) {
	dir := bareImportDir(t)
	m := bareImport(t, 40, 10, dir, "pick")
	rows := plainRows(m.View())
	if !strings.HasPrefix(rows[0], "Import .zip  …") || !strings.HasSuffix(rows[0], filepath.Base(dir)) {
		t.Errorf("the title does not end with the folder: %q", rows[0])
	}
	want := []string{
		"> 2026-10",
		"  STREAM.ZIP",
		"  b.zip",
		"  c.zip",
		"  d.zip",
		"  e.zip",
		"  hololive-karaoke-2026-11-01-with-a-lon",
		"  notes.txt",
		"↑↓  ←:Up  →/Enter:Open  Esc:Cancel",
	}
	if got := strings.Join(rows[1:], "\n"); got != strings.Join(want, "\n") {
		t.Errorf("the picker at 40x10:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	wantFrame(t, "meta at 30x6", bareImport(t, 30, 6, dir, "meta").View(), 30, 6, []string{
		"Import: STREAM.ZIP",
		"> Title:   My title",
		"  Channel:",
		"Blank keeps the archive's own.",
		"",
		"Tab Enter:Import Esc:Back",
	})
	importing := plainRows(bareImport(t, 30, 6, dir, "importing").View())
	if importing[0] != "Importing" || !strings.HasSuffix(importing[1], " STREAM.ZIP") || importing[5] != "Working. Esc is off." {
		t.Errorf("importing at 30x6:\n%s", strings.Join(importing, "\n"))
	}
	wantFrame(t, "an empty folder at 30x6", belowTitle(bareImport(t, 30, 6, t.TempDir(), "pick").View()), 30, 5, []string{
		"No .zip archives here (← goes",
		"up a folder)",
		"",
		"",
		"←:Up →/Enter:Open Esc",
	})
	// A long name on the meta step keeps its start and its extension.
	long := bareImport(t, 30, 6, dir, "pick")
	long.filePath, long.step = filepath.Join(dir, "hololive-karaoke-2026-11-01-with-a-long-name.zip"), importStepMeta
	if got := plainRows(long.View())[0]; !strings.HasPrefix(got, "Import: hololive-k") || !strings.HasSuffix(got, "name.zip") || !strings.Contains(got, "…") {
		t.Errorf("a long name in the title: %q", got)
	}
}

// The server's refusal wraps under the two fields and never covers them.
func TestBareImportRefusalNeverCoversTheFields(t *testing.T) {
	dir := bareImportDir(t)
	compactSizes(func(w, h int) {
		got := ansi.Strip(bareImport(t, w, h, dir, "refused").View())
		if !strings.Contains(got, "> Title:   My title") || !strings.Contains(got, "  Channel:") {
			t.Errorf("%dx%d: the refusal covers a field:\n%s", w, h, got)
		}
		if !strings.Contains(got, "the database is locked by") {
			t.Errorf("%dx%d: the refusal does not start on screen:\n%s", w, h, got)
		}
	})
	// With room, it is whole.
	got := strings.Join(strings.Fields(ansi.Strip(bareImport(t, 40, 12, dir, "refused").View())), " ")
	if !strings.Contains(strings.ReplaceAll(got, " ", ""), strings.ReplaceAll(longError, " ", "")) {
		t.Errorf("the refusal is not whole at 40x12:\n%s", got)
	}
}

// The picker's cursor row stays on screen across a shrink into the bare
// form and the growth back: its window follows the new height both ways.
//
// Mutant: bareSetSize without pickerShowCursor — the cursor is below the
// last drawn row at 30x6. Mutant: no size message after SetHeight — the
// list is still four rows tall back at 60x20.
func TestBareImportPickerKeepsItsCursorOnScreen(t *testing.T) {
	dir := bareImportDir(t)
	m := bareImport(t, 100, 40, dir, "pick")
	for range 6 {
		m.UpdateComponents(namedKey("down"))
	}
	cursorRow := func() string {
		for _, row := range plainRows(m.View()) {
			if strings.Contains(row, "> ") && strings.Contains(row, "hololive") {
				return row
			}
		}
		return ""
	}
	if cursorRow() == "" {
		t.Fatalf("premise lost: the cursor is not on the long name:\n%s", ansi.Strip(m.View()))
	}
	for _, size := range [][2]int{{30, 6}, {200, 8}, {30, 50}, {30, 6}, {60, 20}, {40, 7}, {100, 40}} {
		m.SetSize(size[0], size[1])
		if cursorRow() == "" {
			t.Errorf("%v: the cursor's row is not on screen:\n%s", size, ansi.Strip(m.View()))
		}
	}
	// Back at the first size the list is as tall as the box again: the
	// entry under the cursor's, which no four-row window showed with it.
	if got := ansi.Strip(m.View()); !strings.Contains(got, "c.zip") || !strings.Contains(got, "notes.txt") {
		t.Errorf("the list did not grow back:\n%s", got)
	}
	// Enter still takes the row the cursor is on.
	m.SetSize(30, 6)
	m.UpdateComponents(tea.KeyPressMsg{Code: tea.KeyEnter})
	if filepath.Base(m.filePath) != "hololive-karaoke-2026-11-01-with-a-long-name.zip" {
		t.Errorf("Enter picked %q", m.filePath)
	}
}

// ---- Import Chapters ----

func TestBareChapterFrames(t *testing.T) {
	wantFrame(t, "editing at 40x10", bareChapters(40, 10, 12, "edit").View(), 40, 10, []string{
		"Chapters: Karaoke stream 2026-11-01",
		"┃ 0:00 One",
		"┃ 1:00 Two",
		"Offset: -1:23:45",
		"Pre-filled: joined 1:23:45 in",
		"12 chapters, replaces current 8",
		"      0:00  chapter-00-",
		"      1:00  chapter-01-",
		"  … and 10 more",
		"Tab:Field  Ctrl+S:Save  Esc:Cancel",
	})
	// The worst case the 30x10 minimum is sized for: a two-row source note
	// and a two-row refusal, with the paste box, the offset and the verdict.
	wantFrame(t, "the worst case at 30x10", bareChapters(30, 10, 12, "worst").View(), 30, 10, []string{
		"Chapters: Karaoke stream 2026…",
		"┃ 0:00 One",
		"┃ 1:00 Two",
		"Offset: -1:23:45",
		"Pre-filled: recorded from the",
		"start",
		"12 new, replaces 8",
		"Not saved: The job is Muxing;",
		"wait for it to finish",
		"Tab ^S:Save Esc",
	})
	wantFrame(t, "the confirm at 30x6", bareChapters(30, 6, 12, "confirm").View(), 30, 6, []string{
		"Chapters: Karaoke stream 2026…",
		"Save 12 chapters, replacing",
		"the current 8?",
		"Offset -1:23:45",
		"",
		"y: Save  any other key: Back",
	})
	wantFrame(t, "saving at 30x6", bareChapters(30, 6, 12, "saving").View(), 30, 6, []string{
		"Chapters: Karaoke stream 2026…",
		"Saving: rewriting the archive",
		"with the chapters.",
		"The save keeps running if you",
		"close.",
		"Esc: Close",
	})
}

// The offset's source note is whole at every size the editing step draws in
// (owner decision CK-2), with the paste box and the offset field, and so is
// the confirm at every compact size.
func TestBareChapterNoteAndConfirmAreWhole(t *testing.T) {
	squash := func(s string) string { return strings.Join(strings.Fields(ansi.Strip(s)), " ") }
	compactSizes(func(w, h int) {
		if got := squash(bareChapters(w, h, 12, "confirm").View()); !strings.Contains(got, "Save 12 chapters, replacing the current 8?") || !strings.Contains(got, "Offset -1:23:45") || !strings.Contains(got, "y: Save") {
			t.Errorf("%dx%d: the confirm is not whole: %s", w, h, got)
		}
		if h < bareChapterEditH {
			return
		}
		for _, state := range []string{"worst", "parse error", "edit"} {
			m := bareChapters(w, h, 12, state)
			got := squash(m.View())
			if note := offsetSourceNote(m.view); !strings.Contains(got, note) {
				t.Errorf("%dx%d, %s: the note %q is not whole: %s", w, h, state, note, got)
			}
			if !strings.Contains(got, "┃ 0:00 One") || !strings.Contains(got, "Offset: -1:23:45") {
				t.Errorf("%dx%d, %s: the paste box or the offset is covered: %s", w, h, state, got)
			}
		}
		if got := squash(bareChapters(w, h, 12, "worst").View()); !strings.Contains(got, "Not saved: The job is Muxing; wait for it to finish") {
			t.Errorf("%dx%d: the refusal is not whole: %s", w, h, got)
		}
	})
}

// The chapter import is the first overlay with a size per state: A C is
// refused below 30x10, an open editing step is held there under a notice
// that names 30x10, and the confirm step is drawn down to 30x6.
//
// Mutant: no "now" on the registry entry — the confirm is held at 30x6, or
// the editing step is drawn in six rows.
func TestChapterImportIsHeldBelowItsStepsSize(t *testing.T) {
	r := newCompactRig(t, 40, 9)
	a := r.app
	r.selectJob(t, "done")
	r.press("a", "c")
	if a.chapterDlg.IsVisible() || a.feedback.msg != "Enlarge the terminal to 30x10 for this" {
		t.Fatalf("A C at 40x9: open %v, feedback %q", a.chapterDlg.IsVisible(), a.feedback.msg)
	}
	a.Update(tea.WindowSizeMsg{Width: 30, Height: 10})
	r.press("a", "c")
	if !a.chapterDlg.IsVisible() || a.screenHeld() {
		t.Fatalf("A C at 30x10 did not open bare: %q", a.feedback.msg)
	}
	wantExact(t, "the editing step at 30x10", a.View().Content, 30, 10)
	typeIntoApp(a, "0:00 One")
	if a.chapterDlg.text.Value() != "0:00 One" {
		t.Fatalf("typed once? %q", a.chapterDlg.text.Value())
	}

	a.Update(tea.WindowSizeMsg{Width: 200, Height: 8})
	if !a.screenHeld() || !strings.HasPrefix(plainFrame(a), "Enlarge to 30x10 to continue\n") || !strings.Contains(plainFrame(a), "Esc closes it.") {
		t.Fatalf("the editing step at 200x8 is not held under its notice:\n%s", plainFrame(a))
	}
	typeIntoApp(a, "zzz")
	a.Update(tea.PasteMsg{Content: "pasted"})
	if a.chapterDlg.text.Value() != "0:00 One" {
		t.Errorf("typing reached the paste box under the notice: %q", a.chapterDlg.text.Value())
	}

	// The confirm step fits six rows: it is drawn, and y saves.
	a.Update(tea.WindowSizeMsg{Width: 30, Height: 10})
	a.chapterDlg.SetPreview(a.chapterDlg.seq, ChapterImportView{Chapters: []ChapterImportRow{{Title: "One"}}}, nil)
	r.press("ctrl+s")
	if a.chapterDlg.step != chapterImportStepConfirm {
		t.Fatal("premise lost: Ctrl+S did not ask to confirm")
	}
	a.Update(tea.WindowSizeMsg{Width: 30, Height: 6})
	if a.screenHeld() {
		t.Fatalf("the confirm step is held at 30x6:\n%s", plainFrame(a))
	}
	wantExact(t, "the confirm step at 30x6", a.View().Content, 30, 6)
	if !strings.Contains(plainFrame(a), "Save 1 chapters, replacing") {
		t.Errorf("the confirm at 30x6:\n%s", plainFrame(a))
	}
	// Any other key goes back to editing, which six rows cannot draw.
	r.press("n")
	if a.chapterDlg.step != chapterImportStepEdit || !a.screenHeld() {
		t.Errorf("back on the editing step at 30x6: step %d, held %v", a.chapterDlg.step, a.screenHeld())
	}
	r.press("esc")
	if a.chapterDlg.IsVisible() {
		t.Error("Esc under the notice did not close the editing step")
	}
}

// In the bare form the wheel below the paste box scrolls a preview that has
// more chapters than fit, and above it moves the paste box's cursor.
func TestBareChapterWheel(t *testing.T) {
	a := wheelPreviewApp(t, 12)
	a.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	if a.screenHeld() {
		t.Fatal("premise lost: the chapter import is held at 40x12")
	}
	m := a.chapterDlg
	if got, want := m.pasteBottom(), m.text.Height(); got != want {
		t.Fatalf("pasteBottom %d, want %d", got, want)
	}
	if !strings.Contains(plainFrame(a), "0:00  chapter-00-") {
		t.Fatalf("premise lost: no preview on screen:\n%s", plainFrame(a))
	}
	wheelAt(a, m.pasteBottom()+3, tea.MouseWheelDown)
	if m.previewTop != 1 || strings.Contains(plainFrame(a), "0:00  chapter-00-") || !strings.Contains(plainFrame(a), "1:00  chapter-01-") {
		t.Errorf("a notch under the paste box: top %d\n%s", m.previewTop, plainFrame(a))
	}
	for range 40 {
		wheelAt(a, m.pasteBottom()+3, tea.MouseWheelDown)
	}
	if !strings.Contains(plainFrame(a), "11:00  chapter-11-") || strings.Contains(plainFrame(a), "more") {
		t.Errorf("at the end the last chapter is not on screen:\n%s", plainFrame(a))
	}
	line := m.text.Line()
	wheelAt(a, 1, tea.MouseWheelDown)
	if m.text.Line() != line+1 {
		t.Errorf("a notch over the paste box moved its cursor from line %d to %d", line, m.text.Line())
	}
}
