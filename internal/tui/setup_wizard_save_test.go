package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
)

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
