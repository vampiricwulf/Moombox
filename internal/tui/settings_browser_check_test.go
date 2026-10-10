package tui

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// stubBrowserCheck replaces quickBrowserCheck for the test with fn.
func stubBrowserCheck(t *testing.T, fn func(path, typ string) error) {
	t.Helper()
	orig := quickBrowserCheck
	quickBrowserCheck = fn
	t.Cleanup(func() { quickBrowserCheck = orig })
}

// withBrowser seeds a configured browser: an absolute path the stubbed
// check never opens, and its type.
func withBrowser(path string) func(*config.MoomboxConfig) {
	return func(c *config.MoomboxConfig) {
		c.Cookies.BrowserPath = path
		c.Cookies.BrowserType = "firefox"
	}
}

// TestTUISaveChecksTheBrowserWithNoLockHeld: the browser check stats the
// configured browser_path, which on an unreachable share or a hung mount
// blocks for as long as the filesystem does. A save runs it on the merged
// pair — here the live browser the operator did not edit, alongside a log
// level they did — but with no lock of the config store held, so a slow
// stat stalls the TUI's own save and nothing else: under the store's write
// lock every Read and Snapshot in the process (the dashboard's requests,
// workers, monitors) waited behind it.
//
// Mutant killed: validateSettingsValues calling quickBrowserCheck itself
// again, under the write lock — the check finds the lock held.
func TestTUISaveChecksTheBrowserWithNoLockHeld(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "firefox")
	m, store, saves := overlayOverStore(t, withBrowser(exe))
	var calls, locked int
	stubBrowserCheck(t, func(path, typ string) error {
		calls++
		if path != exe || typ != "firefox" {
			t.Errorf("checked %q (%s), want the live %q (firefox)", path, typ, exe)
		}
		// TryLock fails while anyone holds the lock, reader or writer.
		if mu := store.RWMutex(); mu.TryLock() {
			mu.Unlock()
		} else {
			locked++
		}
		return nil
	})
	m.values["log_level"] = "DEBUG"
	saveOverlay(t, m)

	if calls == 0 {
		t.Fatal("the save never checked the merged browser pair")
	}
	if locked > 0 {
		t.Errorf("%d of %d browser checks ran with the config store's lock held: a slow stat stalls every config reader",
			locked, calls)
	}
	if *saves != 1 || store.Snapshot().Logs.LogLevel != "DEBUG" {
		t.Errorf("OnSave calls %d, log level %q; want the save written", *saves, store.Snapshot().Logs.LogLevel)
	}
}

// TestTUISaveBrowserVerdictIsForThePairItChecked: the check runs before the
// write lock, so the live half of the pair the operator did not edit can
// change between the check and the write. The verdict answers only for the
// pair it was taken for: here the operator changes the browser type, the
// dashboard points browser_path somewhere else while the check runs, and
// the save is refused with nothing written rather than storing a pair no
// check has passed. Pressing Save again checks the new pair, and its
// refusal is reported as the check worded it.
//
// Mutants killed: browserVerdict.check answering without comparing the pair
// (the first save writes the unchecked pair); check dropping the verdict's
// error (the second save writes a pair the check refused).
func TestTUISaveBrowserVerdictIsForThePairItChecked(t *testing.T) {
	dir := t.TempDir()
	exe, moved := filepath.Join(dir, "firefox"), filepath.Join(dir, "elsewhere", "chrome")
	m, store, saves := overlayOverStore(t, withBrowser(exe))
	var checked []string
	stubBrowserCheck(t, func(path, typ string) error {
		checked = append(checked, path+" ("+typ+")")
		if len(checked) == 1 {
			onDashboard(t, store, func(c *config.MoomboxConfig) { c.Cookies.BrowserPath = moved })
			return nil
		}
		return errors.New("not a browser")
	})
	m.values["browser_type"] = "chrome"
	m.recheckDirty()
	m.saveAndClose()

	if m.status != saveError || m.errorMsg != browserMovedMsg {
		t.Fatalf("status %v, error %q; want the save refused as %q", m.status, m.errorMsg, browserMovedMsg)
	}
	if live := store.Snapshot(); live.Cookies.BrowserType != "firefox" || live.Cookies.BrowserPath != moved {
		t.Errorf("live browser %q (%s) after a refused save, want the dashboard's %q (firefox)",
			live.Cookies.BrowserPath, live.Cookies.BrowserType, moved)
	}

	// The key that presses Save again clears the error first (handleKey).
	m.status, m.errorMsg = saveIdle, ""
	m.saveAndClose()
	if want := moved + " (chrome)"; len(checked) != 2 || checked[1] != want {
		t.Fatalf("checked %q, want the second save to check %q", checked, want)
	}
	if m.status != saveError || m.errorMsg != "Invalid browser: not a browser" {
		t.Errorf("status %v, error %q; want the check's refusal", m.status, m.errorMsg)
	}
	if *saves != 0 || store.Snapshot().Cookies.BrowserType != "firefox" {
		t.Errorf("OnSave calls %d, live browser_type %q; want nothing written", *saves, store.Snapshot().Cookies.BrowserType)
	}
}
