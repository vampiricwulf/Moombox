package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestPasswordSurfacesRefuseWhatTheLoginRefuses pins W26-10 on the TUI's
// two password surfaces: the wizard checked only the minimum and Settings →
// Security nothing at all, so either could set a password over 128 bytes,
// which /api/auth/login refuses. Both answer with the shared refusals now
// (config.PasswordLengthError) and accept 128.
//
// Mutants killed: the wizard's finishAdvancedSetup checking only the minimum
// (129 saved); handleSetPassword without the length check (7 and 129 set).
func TestPasswordSurfacesRefuseWhatTheLoginRefuses(t *testing.T) {
	wizard := func(pw string) *SetupWizardModel {
		m := NewSetupWizardModel()
		m.OnHashPassword = func(string) (string, error) { return "scrypt:salt:hash", nil }
		m.values["networkAccess"] = "External"
		m.values["password"] = pw
		return m
	}
	m := wizard(strings.Repeat("a", 129))
	if got := m.finishAdvancedSetup(); got != "" || m.errorMsg != config.PasswordTooLongMsg {
		t.Errorf("wizard, 129 bytes: action %q errorMsg %q, want refused with %q", got, m.errorMsg, config.PasswordTooLongMsg)
	}
	if m := wizard(strings.Repeat("a", 128)); m.finishAdvancedSetup() != "save" {
		t.Errorf("wizard, 128 bytes refused: %q", m.errorMsg)
	}

	for _, tc := range []struct {
		pw   string
		want string
	}{
		{strings.Repeat("a", 7), config.PasswordTooShortMsg},
		{strings.Repeat("a", 129), config.PasswordTooLongMsg},
		{strings.Repeat("a", 128), ""},
	} {
		s, cfg := newSecuritySettingsModel(t, "localhost", "")
		s.OnHashPassword = func(string) string { return "scrypt:salt:hash" }
		s.secNewPw, s.secConfirmPw = tc.pw, tc.pw
		s.handleSetPassword()
		if tc.want == "" {
			if cfg.Network.PasswordHash == "" {
				t.Errorf("Security, %d bytes refused: %q", len(tc.pw), s.secMessage)
			}
			continue
		}
		if cfg.Network.PasswordHash != "" || s.secMessage != tc.want {
			t.Errorf("Security, %d bytes: hash %q message %q, want refused with %q",
				len(tc.pw), cfg.Network.PasswordHash, s.secMessage, tc.want)
		}
	}
}

// TestSetupWizardHashesThePasswordAsTyped pins W26-09 in the TUI wizard: it
// read the password through v(), which trims, while the login and every
// password change check it as typed — so " correct horse battery " set a
// password the operator could not log in with. No surface trims a password
// now: what is typed is what is hashed.
//
// Mutant killed: finishAdvancedSetup reading the password through v().
func TestSetupWizardHashesThePasswordAsTyped(t *testing.T) {
	const typed = " correct horse battery "
	m := NewSetupWizardModel()
	var hashed string
	m.OnHashPassword = func(pw string) (string, error) { hashed = pw; return "scrypt:salt:hash", nil }
	m.values["networkAccess"] = "External"
	m.values["password"] = typed
	if got := m.finishAdvancedSetup(); got != "save" {
		t.Fatalf("finishAdvancedSetup = %q (%s)", got, m.errorMsg)
	}
	if hashed != typed {
		t.Errorf("hashed %q, want the password as typed %q", hashed, typed)
	}
}

// TestPasswordSpaceWarningOnBothTUISurfaces: a password with a space at
// either end is kept as typed, so the wizard's Network step and Settings →
// Security's Set Password say so while it is typed — a warning, never a
// refusal (W26-09).
//
// Mutants killed: the wizard's view without the warning; renderSecuritySet
// without it; either showing it for a password with no outer space.
func TestPasswordSpaceWarningOnBothTUISurfaces(t *testing.T) {
	wizardView := func(pw string) string {
		m := NewSetupWizardModel()
		m.Open()
		m.SetSize(120, 50)
		m.HandleKey(keyDown)
		m.HandleKey(keyEnter) // Advanced Setup: its form opens on Network
		m.values["networkAccess"] = "External"
		m.values["password"] = pw
		return ansi.Strip(m.View())
	}
	if v := wizardView(" padded secret "); !strings.Contains(v, config.PasswordOuterSpaceWarning) {
		t.Errorf("wizard: no warning for a padded password:\n%s", v)
	}
	if v := wizardView("plain secret"); strings.Contains(v, config.PasswordOuterSpaceWarning) {
		t.Error("wizard: warning for a password with no outer space")
	}

	securityView := func(pw string) string {
		s, _ := newSecuritySettingsModel(t, "localhost", "")
		s.secMode = securitySet
		s.secNewPw = pw
		return ansi.Strip(s.renderSecuritySet(120))
	}
	if v := securityView("padded secret "); !strings.Contains(v, config.PasswordOuterSpaceWarning) {
		t.Errorf("Security: no warning for a padded password:\n%s", v)
	}
	if v := securityView("plain secret"); strings.Contains(v, config.PasswordOuterSpaceWarning) {
		t.Error("Security: warning for a password with no outer space")
	}
}

// TestSetupSaveCarriesTheLoginCookiesBeforeSaving pins W26-06 on the TUI
// wizard's side: its Cookies step names a cookie file and its Cookie Login
// step then writes the login to the cookie file the running service was
// built with, so the restart loaded an empty jar from the saved one although
// the login had been reported Done. The save command hands the cookie file it
// is about to save to OnCarryCookies first; a carry that fails is reported in
// the wizard and nothing is saved.
//
// Mutants killed: the save command not calling OnCarryCookies; calling it
// after OnComplete; ignoring its error.
func TestSetupSaveCarriesTheLoginCookiesBeforeSaving(t *testing.T) {
	t.Chdir(t.TempDir())
	run := func(carryErr error) (carried []string, saved bool, res setupSaveResultMsg) {
		app := NewApp()
		app.SetSetupCallbacks(
			func(*config.MoomboxConfig) error { saved = true; return nil },
			nil, nil, nil, nil, nil,
		)
		app.SetupWizCarryCookies(func(cookieFile string) error {
			if saved {
				t.Error("OnCarryCookies ran after the config was saved")
			}
			carried = append(carried, cookieFile)
			return carryErr
		})
		// Advanced setup on its channel list, past the Cookies step (which
		// named the cookie file) and the Cookie Login step: Tab finishes.
		app.setupWiz.Open()
		app.setupWiz.mode = setupModeAdvanced
		app.setupWiz.values["cookieFile"] = filepath.Join("data", "my-cookies.txt")
		app.setupWiz.advancedFormDone, app.setupWiz.advancedCookieDone = true, true
		app.setupWiz.channelMode = "list"
		_, cmd := app.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
		if cmd == nil {
			t.Fatal("Tab on the channel list issued no save command")
		}
		res, _ = cmd().(setupSaveResultMsg)
		return carried, saved, res
	}

	carried, saved, res := run(nil)
	if want := filepath.Join("data", "my-cookies.txt"); len(carried) != 1 || carried[0] != want || !saved || res.Err != "" {
		t.Errorf("carried %q saved %v err %q, want the saved cookie file %q carried, then saved", carried, saved, res.Err, want)
	}

	_, saved, res = run(errors.New("disk full"))
	if saved || !strings.Contains(res.Err, "cookies.cookie_file") {
		t.Errorf("a failed carry: saved %v err %q, want refused naming cookies.cookie_file", saved, res.Err)
	}
}

// TestSetupSaveRefusesAnUncreatableDirectory pins W26-13 on the TUI
// wizard's side: an output directory that cannot be created — here
// "./output" is a regular file — used to be discarded after the save, the
// wizard said nothing and restarted, and the first recording downloaded in
// full before failing at mux. Now the directories are made before the save:
// the failure names the field in the wizard, which stays open, nothing is
// saved and nothing restarts.
//
// Mutants killed: discarding config.MakeSetupDirs' error in the save command
// (app_keys.go — saved, restarted); MakeSetupDirs ignoring MkdirAll's error;
// creating the directories after OnComplete (the config is saved).
func TestSetupSaveRefusesAnUncreatableDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "output"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	app := NewApp()
	var saved *config.MoomboxConfig
	restarted := false
	app.SetSetupCallbacks(
		func(c *config.MoomboxConfig) error { saved = c; return nil },
		nil, nil, nil, nil,
		func() { restarted = true },
	)
	app.setupWiz.Open()
	// Mode select: Quick, Advanced, Use Defaults — the defaults name ./output.
	app.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	app.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	_, cmd := app.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Use Defaults issued no save command")
	}
	res, ok := cmd().(setupSaveResultMsg)
	if !ok {
		t.Fatal("the save command did not answer setupSaveResultMsg")
	}
	if !strings.Contains(res.Err, "paths.output_directory") {
		t.Errorf("save result Err %q, want the output directory's failure", res.Err)
	}
	if saved != nil {
		t.Error("the config was saved although its output directory could not be created")
	}

	if _, cmd := app.Update(res); cmd != nil {
		cmd()
	}
	if restarted {
		t.Error("the wizard restarted after a failed save")
	}
	if !app.setupWiz.IsVisible() || !strings.Contains(app.setupWiz.errorMsg, "paths.output_directory") {
		t.Errorf("wizard visible=%v errorMsg %q, want it open naming the output directory",
			app.setupWiz.IsVisible(), app.setupWiz.errorMsg)
	}

	// The output directory is made first, so a setup refused there leaves
	// no staging directory behind either.
	if _, err := os.Stat(filepath.Join(dir, "staging")); err == nil {
		t.Error("the staging directory was created for a setup that was refused at its output directory")
	}
}
