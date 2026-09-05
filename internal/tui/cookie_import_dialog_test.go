package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// typeInto feeds a string to the dialog one keypress at a time, on the route
// App.Update actually takes: routeComponentMsg (→ UpdateComponents) first,
// then handleKey (→ HandleKey). Driving HandleKey alone is what let the
// double-insert bug hide — see TestCookieImportTypingIsNotDoubled.
func typeInto(m *CookieImportDialogModel, s string) {
	for _, r := range s {
		msg := tea.KeyPressMsg{Code: r, Text: string(r)}
		m.UpdateComponents(msg)
		m.HandleKey(msg)
	}
}

// TestCookieImportDialogValidatesThePath: a missing file is refused inline
// (no callback), "~" expands to the home directory, a directory is refused.
func TestCookieImportDialogValidatesThePath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(file, []byte("# Netscape HTTP Cookie File\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewCookieImportDialogModel()
	m.Open()
	typeInto(m, filepath.Join(dir, "missing.txt"))
	if action, _ := m.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter}); action != "" {
		t.Fatalf("missing file must not import, got action %q", action)
	}
	if !strings.Contains(m.View(), "does not exist") {
		t.Errorf("no inline error for a missing file:\n%s", m.View())
	}
	m.Open()
	typeInto(m, dir)
	if action, _ := m.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter}); action != "" {
		t.Fatalf("a directory must not import, got %q", action)
	}
	m.Open()
	typeInto(m, file)
	action, path := m.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if action != "import" || path != file {
		t.Fatalf("existing file: action=%q path=%q", action, path)
	}
	home, _ := os.UserHomeDir()
	if got := expandHome("~/x/cookies.txt"); got != filepath.Join(home, "x", "cookies.txt") {
		t.Errorf("expandHome = %q", got)
	}
	if got := expandHome("relative/cookies.txt"); got != "relative/cookies.txt" {
		t.Errorf("expandHome must leave non-~ paths alone, got %q", got)
	}
}

// TestCookieImportDialogRendersEveryOutcome: the result is worded per platform
// off ImportOutcome.String() and the verdict — never flattened to pass/fail —
// and never includes cookie content.
func TestCookieImportDialogRendersEveryOutcome(t *testing.T) {
	m := NewCookieImportDialogModel()
	m.Open()
	m.SetImporting()
	if !strings.Contains(m.View(), "Importing") {
		t.Errorf("importing step not rendered:\n%s", m.View())
	}
	m.SetResult(cookies.ImportResult{
		YouTubeOutcome: cookies.ImportInstalled, YouTubeAccepted: true,
		TwitchOutcome: cookies.ImportRolledBack, RollbackProtected: true,
	}, nil)
	v := m.View()
	for _, want := range []string{"YouTube", "imported", "Twitch", "rolled-back"} {
		if !strings.Contains(v, want) {
			t.Errorf("result view lacks %q:\n%s", want, v)
		}
	}
	// RollbackProtected says a rollback was POSSIBLE, and is true on almost
	// every import over an existing cookies.txt. The sentence must key off an
	// outcome that actually rolled back, or a clean import reads as a failure.
	m.SetResult(cookies.ImportResult{
		YouTubeOutcome: cookies.ImportInstalled, YouTubeAccepted: true,
		TwitchOutcome: cookies.ImportInstalled, TwitchAccepted: true,
		RollbackProtected: true,
	}, nil)
	if strings.Contains(m.View(), "rolled back") {
		t.Errorf("a clean two-platform import claims a rollback happened:\n%s", m.View())
	}
	m.SetResult(cookies.ImportResult{
		YouTubeOutcome: cookies.ImportInstalled, YouTubeAccepted: true,
		TwitchOutcome: cookies.ImportRolledBack,
	}, nil)
	if !strings.Contains(m.View(), "rolled back") {
		t.Errorf("an actual rollback is not explained:\n%s", m.View())
	}

	m.SetResult(cookies.ImportResult{}, errors.New("cookies.txt: not a Netscape cookie file"))
	if !strings.Contains(m.View(), "not a Netscape cookie file") {
		t.Errorf("error not rendered:\n%s", m.View())
	}
	if action, _ := m.HandleKey(tea.KeyPressMsg{Code: tea.KeyEscape}); action != "close" || m.IsVisible() {
		t.Fatalf("Esc must close the dialog (action %q, visible %v)", action, m.IsVisible())
	}
}

// TestCookieImportTypingIsNotDoubled drives the REAL entry point, because the
// bug it pins is invisible from the dialog alone.
//
// App.Update's tea.KeyPressMsg arm (app_update.go) calls routeComponentMsg AND
// handleKey for the same message: the first reaches UpdateComponents, the
// second reaches HandleKey. A dialog whose HandleKey also feeds its textinput
// therefore inserts every rune twice — "abc" arrives as "aabbcc" and one
// Backspace eats two characters — while every test that calls HandleKey
// directly reports the value as correct. ImportDialogModel avoids it by never
// touching its textinput outside UpdateComponents; this pins that split for
// the cookie prompt.
func TestCookieImportTypingIsNotDoubled(t *testing.T) {
	app := NewApp()
	app.OnImportCookieFile = func(string) (cookies.ImportResult, error) { return cookies.ImportResult{}, nil }
	app.dispatchAction("R I", nil)
	if !app.cookieImportDlg.IsVisible() {
		t.Fatal("R I did not open the dialog")
	}

	for _, r := range "abc" {
		app.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if got := app.cookieImportDlg.input.Value(); got != "abc" {
		t.Fatalf("typed \"abc\" through App.Update, input value = %q — the keypress is being applied twice", got)
	}

	app.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if got := app.cookieImportDlg.input.Value(); got != "ab" {
		t.Fatalf("one Backspace through App.Update left %q, want \"ab\"", got)
	}
}

// TestCookieImportResultSurvivesAClosedOverlay: Esc during a slow import must
// not throw the outcome away, and a result must never land on the overlay of a
// LATER import.
func TestCookieImportResultSurvivesAClosedOverlay(t *testing.T) {
	// Closed while importing — the answer goes to the feedback line.
	app := NewApp()
	app.OnImportCookieFile = func(string) (cookies.ImportResult, error) { return cookies.ImportResult{}, nil }
	app.dispatchAction("R I", nil)
	app.cookieImportDlg.SetImporting()
	app.cookieImportDlg.Close()
	app.Update(cookieImportResultMsg{Result: cookies.ImportResult{
		YouTubeOutcome: cookies.ImportInstalled, YouTubeAccepted: true,
		TwitchOutcome: cookies.ImportUnchanged,
	}})
	if !strings.Contains(app.feedback.msg, "imported (authenticates)") ||
		!strings.Contains(app.feedback.msg, "unchanged") {
		t.Errorf("the outcome vanished when the overlay closed mid-import; feedback = %q", app.feedback.msg)
	}

	// Re-opened after Esc — the stale result must not overwrite the fresh
	// prompt, and it still reaches the feedback line.
	app.dispatchAction("R I", nil)
	app.Update(cookieImportResultMsg{Result: cookies.ImportResult{TwitchOutcome: cookies.ImportRolledBack}})
	if app.cookieImportDlg.step != cookieImportStepPath {
		t.Errorf("a stale result replaced a re-opened path prompt (step %d)", app.cookieImportDlg.step)
	}
	if !strings.Contains(app.feedback.msg, "rolled-back") {
		t.Errorf("the stale result was dropped instead of reported; feedback = %q", app.feedback.msg)
	}
}

// TestImportResultSummaryWordsBothFacts: the one-line form carries the same
// two facts per platform as the overlay rows, and reports an error verbatim.
func TestImportResultSummaryWordsBothFacts(t *testing.T) {
	got := importResultSummary(cookies.ImportResult{
		YouTubeOutcome: cookies.ImportInstalled, YouTubeAccepted: true,
		TwitchOutcome: cookies.ImportUnchanged, Twitch: cookies.RefreshUnknown,
	}, nil)
	want := "Cookie import: YouTube imported (authenticates); Twitch unchanged (unknown)"
	if got != want {
		t.Errorf("importResultSummary = %q, want %q", got, want)
	}

	got = importResultSummary(cookies.ImportResult{}, errors.New("cookies.txt: not a Netscape cookie file"))
	want = "Cookie import failed: cookies.txt: not a Netscape cookie file"
	if got != want {
		t.Errorf("importResultSummary(err) = %q, want %q", got, want)
	}
}

// TestImportCookieChordExistsOnlyWhenWired: nil callback ⇒ no R I in
// dispatch, menu or help; wired ⇒ all three, and dispatch opens the dialog.
func TestImportCookieChordExistsOnlyWhenWired(t *testing.T) {
	app := NewApp()
	app.OnImportCookieFile = nil
	if riOffered(t, app) {
		t.Fatal("R I offered with no callback")
	}
	app.dispatchAction("R I", nil)
	if app.cookieImportDlg.IsVisible() {
		t.Fatal("dispatching R I with no callback opened a dialog whose Enter dead-ends")
	}
	app.OnImportCookieFile = func(string) (cookies.ImportResult, error) { return cookies.ImportResult{}, nil }
	if !riOffered(t, app) {
		t.Fatal("R I missing with the callback wired")
	}
	app.dispatchAction("R I", nil)
	if !app.cookieImportDlg.IsVisible() {
		t.Fatal("dispatching R I did not open the dialog")
	}
}

// riOffered reports whether R I is reachable by the two derived routes an
// operator has — the action menu and the help overlay, which is built from
// the menu. Dispatch is asserted separately by its own visible side effect.
func riOffered(t *testing.T, app *App) bool {
	t.Helper()
	items := app.buildMenuItems()
	if len(items) == 0 {
		t.Fatal("buildMenuItems returned nothing — nothing below can be concluded")
	}
	menu := false
	for _, it := range items {
		if strings.TrimSpace(it.Chord) == "R I" {
			menu = true
		}
	}
	h := NewHelpModel()
	h.SetMenuItems(items)
	help := false
	for _, sec := range h.orderedSections() {
		for _, k := range sec.keys {
			if strings.TrimSpace(k.key) == "R I" {
				help = true
			}
		}
	}
	if menu != help {
		t.Fatalf("R I is in the menu (%v) but not the help overlay (%v) — the two reads disagree", menu, help)
	}
	return menu
}
