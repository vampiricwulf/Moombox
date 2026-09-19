package tui

import (
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// ctrlCMsg is the message bubbletea v2 actually delivers for Ctrl+C: an
// ORDINARY key press carrying the ctrl modifier. tea.InterruptMsg arrives
// only from a real SIGINT, so the app's own handler is the only thing
// standing between Ctrl+C and the fourteen overlays that intercept every key
// before it (CORE-5, O-M).
func ctrlCMsg() tea.KeyPressMsg { return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl} }

// The census below reaches handleKey's comparison against keyCtrlC through
// msg.String(), so a fixture that stringifies to anything else would pass
// every row while pressing a key the app never sees.
//
// Mutant: building the fixture as tea.KeyPressMsg{Code: 'c'} (no modifier) —
// it stringifies to "c" and this fails.
func TestCtrlCFixtureStringifiesToTheKeyConstant(t *testing.T) {
	if got := ctrlCMsg().String(); got != keyCtrlC {
		t.Fatalf("ctrlCMsg().String() = %q, want %q", got, keyCtrlC)
	}
}

// isQuit reports whether cmd is tea.Quit.
func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// help.go promises "Ctrl+C  Quit immediately", but bubbletea v2 delivers
// Ctrl+C as a plain key and fourteen overlays intercept every key before the
// app's handler — twelve of them swallowed it, and the two that did not
// carried a hand-written copy of the quit apiece. The check is hoisted above
// every overlay intercept (O-M) (CORE-5).
//
// Every overlay handleKey tests for is a row here, opened through its own
// production entry point and asserted VISIBLE before the key is pressed: a
// row whose overlay silently failed to open would assert nothing more than
// that the main view quits.
//
// Mutant: moving the keyCtrlC check back below the overlay intercepts —
// every row except the ffmpeg and setup-wizard ones fails.
func TestCtrlCQuitsThroughEveryOverlay(t *testing.T) {
	newApp := func() *App {
		a := NewApp()
		a.SetConfigStore(config.NewStore(config.Defaults(), ""))
		a.width, a.height = 120, 40
		a.recalcLayout()
		a.taskList.SetJobs([]*database.Job{
			{ID: "j1", Title: "a job", Status: database.StatusFinished, Platform: "youtube", VideoID: "aaaaaaaaaaa"},
		})
		return a
	}

	rows := []struct {
		name    string
		show    func(*App)
		visible func(*App) bool
	}{
		{"settings", func(a *App) { a.settings.Open(a.cfg) }, func(a *App) bool { return a.settings.IsVisible() }},
		{"help", func(a *App) { a.help.Toggle() }, func(a *App) bool { return a.help.IsVisible() }},
		{"release notes", func(a *App) { a.releaseNotesPopup.open("v9.9.9", "# notes", a.width, a.height) },
			func(a *App) bool { return a.releaseNotesPopup.isOpen() }},
		{"ffmpeg check", func(a *App) { a.ffmpegCheck.Open() }, func(a *App) bool { return a.ffmpegCheck.IsVisible() }},
		{"setup wizard", func(a *App) { a.setupWiz.Open() }, func(a *App) bool { return a.setupWiz.IsVisible() }},
		{"action menu", func(a *App) { a.actionMenu.Open(a.buildMenuItems()) }, func(a *App) bool { return a.actionMenu.IsVisible() }},
		{"import", func(a *App) { a.importDlg.Open(t.TempDir()) }, func(a *App) bool { return a.importDlg.IsVisible() }},
		{"cookie import", func(a *App) { a.cookieImportDlg.Open() }, func(a *App) bool { return a.cookieImportDlg.IsVisible() }},
		{"add video", func(a *App) { a.addVideo.Open() }, func(a *App) bool { return a.addVideo.IsVisible() }},
		{"trim", func(a *App) { a.trimDlg.Open("j1", "a job") }, func(a *App) bool { return a.trimDlg.IsVisible() }},
		{"files", func(a *App) { a.filesDlg.Open() }, func(a *App) bool { return a.filesDlg.IsVisible() }},
		{"client tokens", func(a *App) { a.clientTokensDlg.Open() }, func(a *App) bool { return a.clientTokensDlg.IsVisible() }},
		{"ytdlp plugin", func(a *App) { a.ytdlpDlg.Open() }, func(a *App) bool { return a.ytdlpDlg.IsVisible() }},
		{"stats", func(a *App) { a.statsDlg.Open() }, func(a *App) bool { return a.statsDlg.IsVisible() }},
	}

	if len(rows) != 14 {
		t.Fatalf("the census covers %d overlays, but handleKey intercepts 14", len(rows))
	}

	for _, row := range rows {
		a := newApp()
		row.show(a)
		if !row.visible(a) {
			t.Errorf("%s overlay: did not open, so the row proves nothing", row.name)
			continue
		}
		if _, cmd := a.handleKey(ctrlCMsg()); !isQuit(cmd) {
			t.Errorf("%s overlay: Ctrl+C did not quit", row.name)
		}
	}
}

// The panels are the other half of the census: both search boxes consume
// keys before the chord system, and the details and logs viewports scroll on
// whatever routeComponentMsg hands them. None of them may hold Ctrl+C.
//
// The hoist is what makes this robust rather than lucky. Before it, the two
// searching rows passed only because HandleSearchKey happens to return
// keyCtrlC to the app unconsumed — a courtesy either box could drop.
//
// Mutant (verified): un-hoisting the check AND making either HandleSearchKey
// return (nil, true) for keyCtrlC — the two searching rows fail. With the
// check hoisted, neither half of that can reach the quit at all.
func TestCtrlCQuitsFromEveryPanelAndSearchBox(t *testing.T) {
	rows := []struct {
		name  string
		setup func(*App)
	}{
		{"tasks panel", func(a *App) { a.focusedPanel = PanelTasks }},
		{"tasks search box", func(a *App) { a.focusedPanel = PanelTasks; a.taskList.StartSearch() }},
		{"details panel", func(a *App) { a.focusedPanel = PanelDetails }},
		{"logs panel", func(a *App) { a.focusedPanel = PanelLogs }},
		{"logs search box", func(a *App) { a.focusedPanel = PanelLogs; a.logs.StartSearch() }},
	}
	for _, row := range rows {
		a := NewApp()
		a.width, a.height = 120, 40
		a.recalcLayout()
		row.setup(a)
		if _, cmd := a.handleKey(ctrlCMsg()); !isQuit(cmd) {
			t.Errorf("%s: Ctrl+C did not quit", row.name)
		}
	}
}

// An armed chord prefix must not eat the quit either: "A" then Ctrl+C is the
// operator changing their mind mid-chord, and handleChord consumes every
// second key it does not recognise.
//
// Mutant: moving the keyCtrlC check below the chord system.
func TestCtrlCQuitsWithAChordArmed(t *testing.T) {
	a := NewApp()
	a.width, a.height = 120, 40
	a.recalcLayout()
	a.handleKey(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if a.chord.prefix != "a" {
		t.Fatalf("the A prefix did not arm (prefix %q) — the row proves nothing", a.chord.prefix)
	}
	if _, cmd := a.handleKey(ctrlCMsg()); !isQuit(cmd) {
		t.Error("Ctrl+C with the A chord armed did not quit")
	}
}

// Quitting out of the setup wizard has to reap the browser the auto-cookie
// step opened: AutoCookieService holds the acquisition slot until someone
// cancels, and a quit that skipped the cancel would orphan a headed browser
// window (closeCookieLogin exists for exactly that). The hoisted check owns
// the cleanup now, because it returns before the wizard's own intercept ever
// runs.
//
// Mutant: hoisting the quit without the OnCancelAutoCookie call — the
// callback never fires and this fails.
func TestCtrlCCancelsAnInFlightCookieSetupBeforeQuitting(t *testing.T) {
	a := NewApp()
	a.width, a.height = 120, 40
	a.recalcLayout()
	a.setupWiz.Open()
	a.setupWiz.cookieActive = true
	cancelled := 0
	a.setupWiz.OnCancelAutoCookie = func() { cancelled++ }

	_, cmd := a.handleKey(ctrlCMsg())
	if !isQuit(cmd) {
		t.Fatal("Ctrl+C in the setup wizard did not quit")
	}
	if cancelled != 1 {
		t.Errorf("OnCancelAutoCookie fired %d times, want exactly 1", cancelled)
	}
}

// No browser open means nothing to reap — a stray cancel would release an
// acquisition slot this wizard never took.
//
// Mutant: dropping the cookieActive guard from the hoisted check.
func TestCtrlCDoesNotCancelWhenNoCookieSetupIsRunning(t *testing.T) {
	a := NewApp()
	a.width, a.height = 120, 40
	a.recalcLayout()
	a.setupWiz.Open()
	cancelled := 0
	a.setupWiz.OnCancelAutoCookie = func() { cancelled++ }

	if _, cmd := a.handleKey(ctrlCMsg()); !isQuit(cmd) {
		t.Fatal("Ctrl+C in the setup wizard did not quit")
	}
	if cancelled != 0 {
		t.Errorf("OnCancelAutoCookie fired %d times with no browser open, want 0", cancelled)
	}
}

// The hoist moves Ctrl+C and NOTHING else. O-V is rejected: Esc on the
// FFmpeg-not-found overlay keeps quitting the program, because the user
// needs ffmpeg for the muxing half of the pipeline and an overlay that
// dismissed itself would leave a Moombox that cannot finish a download. The
// arc that hoists one key past this overlay is exactly the change that could
// have taken the other with it, so here is the pin.
//
// Mutant: turning ffmpeg_check.go's `if key == keyEsc { return "quit" }`
// into a dismiss, or hoisting a keyEsc arm the way keyCtrlC was hoisted.
func TestEscOnTheFFmpegOverlayStillQuits(t *testing.T) {
	a := NewApp()
	a.width, a.height = 120, 40
	a.recalcLayout()
	a.ffmpegCheck.Open()
	if !a.ffmpegCheck.IsVisible() {
		t.Fatal("the FFmpeg overlay did not open")
	}

	_, cmd := a.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !isQuit(cmd) {
		t.Error("Esc on the FFmpeg-not-found overlay must still quit (O-V is rejected)")
	}
	if !a.ffmpegCheck.IsVisible() {
		t.Error("Esc must not dismiss the FFmpeg overlay")
	}
}

// pagingApp builds an App whose task list is long enough to hold several
// pages at the fixture's terminal size.
func pagingApp(t *testing.T) *App {
	t.Helper()
	a := NewApp()
	a.width, a.height = 120, 40
	a.recalcLayout()
	jobs := make([]*database.Job, 0, 200)
	for i := range 200 {
		jobs = append(jobs, &database.Job{
			ID:       "job-" + strconv.Itoa(i),
			Title:    "job " + strconv.Itoa(i),
			Status:   database.StatusLive,
			Platform: "youtube",
		})
	}
	a.taskList.SetJobs(jobs)
	a.focusedPanel = PanelTasks
	if got := a.taskList.list.Paginator.TotalPages; got < 3 {
		t.Fatalf("the fixture has %d page(s) — paging cannot be observed", got)
	}
	return a
}

// The task list's pgup/pgdown/home/end bindings were configured on the
// bubbles KeyMap and never delivered, so 1,000 rows were navigable only by
// arrow key and a 3-row wheel — dead configuration either way. O-X adds the
// paging rather than deleting the bindings (CORE-16).
//
// Mutant: dropping the four cases from handleTaskKey — End leaves the cursor
// on row 0.
func TestTaskPanelPages(t *testing.T) {
	a := pagingApp(t)
	last := len(a.taskList.list.Items()) - 1

	first := a.taskList.list.Index()
	a.handleKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if a.taskList.list.Index() == first {
		t.Error("PgDn must move the selection off the first page")
	}
	a.handleKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if got := a.taskList.list.Index(); got != first {
		t.Errorf("PgUp must return to the first page, got index %d, want %d", got, first)
	}
	a.handleKey(tea.KeyPressMsg{Code: tea.KeyEnd})
	if got := a.taskList.list.Index(); got != last {
		t.Errorf("End must select the last row, got index %d of %d", got, last)
	}
	a.handleKey(tea.KeyPressMsg{Code: tea.KeyHome})
	if got := a.taskList.list.Index(); got != 0 {
		t.Errorf("Home must select the first row, got index %d", got)
	}
}

// Paging moves the list's OWN paginator, so the header's [start-end/total]
// range follows the cursor. renderHeader reads Paginator.Page directly and
// is the only place the operator is told where in 200 rows they are, so the
// index moving is not on its own enough.
//
// Mutant: dropping the keyEnd case from handleTaskKey — the range still
// reads [1-24/200] after the jump. (list.Select would NOT be a mutant here:
// bubbles' Select sets Paginator.Page from the index, so it moves the range
// too — which is why GoToEnd/GoToStart are the wrappers rather than the
// spelling this test forbids.)
func TestTaskPanelPagingMovesTheHeaderRange(t *testing.T) {
	a := pagingApp(t)
	perPage := a.taskList.list.Paginator.PerPage
	total := len(a.taskList.list.Items())

	a.handleKey(tea.KeyPressMsg{Code: tea.KeyEnd})
	header := stripANSI(a.taskList.renderHeader(a.taskList.width - 2))
	lastStart := (a.taskList.list.Paginator.TotalPages-1)*perPage + 1
	want := "[" + strconv.Itoa(lastStart) + "-" + strconv.Itoa(total) + "/" + strconv.Itoa(total) + "]"
	if !strings.Contains(header, want) {
		t.Errorf("after End the header reads %q, want it to contain %q", header, want)
	}
}

// The four keys refresh the details panel exactly as the arrow keys do — the
// selected row and the panel beside it must never disagree.
//
// Mutant: dropping the updateSelectedJob() calls from the four new cases.
func TestTaskPanelPagingRefreshesTheDetailsPanel(t *testing.T) {
	a := pagingApp(t)
	a.updateSelectedJob()
	before := a.taskList.SelectedJob()
	if before == nil {
		t.Fatal("the fixture selected nothing")
	}

	a.handleKey(tea.KeyPressMsg{Code: tea.KeyEnd})
	sel := a.taskList.SelectedJob()
	if sel == nil {
		t.Fatal("End left no selected job")
	}
	if sel.ID == before.ID {
		t.Fatalf("End did not move the selection off %s — the row proves nothing", before.ID)
	}
	if a.details.job == nil || a.details.job.ID != sel.ID {
		t.Errorf("the details panel shows %v, want the selected job %s", a.details.job, sel.ID)
	}
}

// O C hands the URL to the terminal over OSC 52, which conhost and
// tmux-without-set-clipboard silently drop — so "Copied:" was a promise the
// TUI could not keep. O-W: hedge the wording where OSC 52 is the mechanism,
// and fall back to clip.exe on Windows outside Windows Terminal (CORE-15).
//
// Mutant: returning "Copied: " for the OSC 52 case again.
func TestClipboardFeedbackHedgesOSC52(t *testing.T) {
	const url = "https://example.test/x"

	osc := clipboardFeedback(url, true)
	if !strings.Contains(osc, "OSC 52") {
		t.Errorf("OSC 52 feedback = %q, want it to name the mechanism", osc)
	}
	if strings.HasPrefix(osc, "Copied") {
		t.Errorf("OSC 52 feedback = %q, must not claim a completed copy", osc)
	}
	if !strings.Contains(osc, url) {
		t.Errorf("OSC 52 feedback = %q, want it to carry the URL", osc)
	}

	direct := clipboardFeedback(url, false)
	if !strings.HasPrefix(direct, "Copied") {
		t.Errorf("direct-copy feedback = %q, want it to claim the copy", direct)
	}
	if !strings.Contains(direct, url) {
		t.Errorf("direct-copy feedback = %q, want it to carry the URL", direct)
	}
}

// The chord itself, driven through the osClipboard seam so neither branch
// spawns a real helper or writes the developer's clipboard on any platform.
//
// Helper declines — every non-Windows build, and Windows Terminal, where
// osClipboardFallback stands down because the terminal's own OSC 52 support
// is authoritative: the URL goes to the terminal and the wording hedges.
// Helper takes it — Windows outside Windows Terminal: the copy really
// happened, it is claimed, and OSC 52 is not sent on top.
//
// Mutant: restoring the unconditional a.setFeedback("Copied: " + url).
// Mutant: sending tea.SetClipboard on both arms.
func TestCopyChordSaysWhatItActuallyDid(t *testing.T) {
	job := &database.Job{ID: "j1", Title: "a job", Status: database.StatusFinished, Platform: "youtube", VideoID: "aaaaaaaaaaa"}
	url := streamURL(job)

	orig := osClipboard
	t.Cleanup(func() { osClipboard = orig })

	osClipboard = func(string) bool { return false }
	a := NewApp()
	a.width, a.height = 120, 40
	a.recalcLayout()
	_, cmd := a.dispatchAction("O C", job)
	if cmd == nil {
		t.Fatal("with no OS helper, O C produced no command — the URL never reached the terminal")
	}
	if got, want := cmd(), tea.SetClipboard(url)(); got != want {
		t.Errorf("O C must emit the OSC 52 clipboard command for %q, got %#v", url, got)
	}
	if want := clipboardFeedback(url, true); a.feedback.msg != want {
		t.Errorf("O C feedback on the OSC 52 path = %q, want %q", a.feedback.msg, want)
	}

	handed := ""
	osClipboard = func(text string) bool { handed = text; return true }
	a = NewApp()
	a.width, a.height = 120, 40
	a.recalcLayout()
	if _, cmd := a.dispatchAction("O C", job); cmd != nil {
		t.Errorf("the OS helper took the text, so nothing more is owed to the terminal, got %#v", cmd())
	}
	if handed != url {
		t.Errorf("the OS helper was handed %q, want %q", handed, url)
	}
	if want := clipboardFeedback(url, false); a.feedback.msg != want {
		t.Errorf("O C feedback after a successful OS copy = %q, want %q", a.feedback.msg, want)
	}
}

// A job with no URL says so rather than claiming anything.
//
// Mutant: dropping the empty-URL arm.
func TestCopyChordWithNoURL(t *testing.T) {
	a := NewApp()
	a.width, a.height = 120, 40
	a.recalcLayout()
	job := &database.Job{ID: "j1", Title: "a job", Status: database.StatusFinished, Platform: "youtube"}

	if _, cmd := a.dispatchAction("O C", job); cmd != nil {
		t.Errorf("O C with no URL must emit no clipboard command, got %#v", cmd())
	}
	if a.feedback.msg != "No URL to copy" {
		t.Errorf("O C feedback = %q, want %q", a.feedback.msg, "No URL to copy")
	}
}
