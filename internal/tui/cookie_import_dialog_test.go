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

// typeInto feeds a string to the dialog one keypress at a time, the way the
// package's other tests synthesise typed runes (see task_search_test.go).
func typeInto(m *CookieImportDialogModel, s string) {
	for _, r := range s {
		m.HandleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
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
	m.SetResult(cookies.ImportResult{}, errors.New("cookies.txt: not a Netscape cookie file"))
	if !strings.Contains(m.View(), "not a Netscape cookie file") {
		t.Errorf("error not rendered:\n%s", m.View())
	}
	if action, _ := m.HandleKey(tea.KeyPressMsg{Code: tea.KeyEscape}); action != "close" || m.IsVisible() {
		t.Fatalf("Esc must close the dialog (action %q, visible %v)", action, m.IsVisible())
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
