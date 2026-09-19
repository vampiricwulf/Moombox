package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// A failed config.Save must be visible and must not leave the running
// process on values that are not on disk. applyValues writes straight into
// the live *MoomboxConfig the store holds, so the pre-apply snapshot is what
// makes the failure recoverable (CORE-4).
//
// Mutant: dropping the `*m.cfg = snapshot` restore — the live struct keeps
// the typed value after the refusal.
func TestFailedSaveIsReportedAndRolledBack(t *testing.T) {
	cfg := config.Defaults()
	cfg.Downloader.NumParallelDownloads = 4
	store := config.NewStore(cfg, "")

	m := NewSettingsModel()
	m.configStore = store
	m.cfg = cfg
	m.Open(cfg)
	m.values["num_parallel_downloads"] = "9"
	m.dirty = true
	m.OnSave = func(*config.MoomboxConfig) error { return errors.New("disk full") }

	m.saveAndClose()

	if m.status != saveError {
		t.Errorf("status = %v, want saveError", m.status)
	}
	if !strings.Contains(m.errorMsg, "disk full") {
		t.Errorf("errorMsg = %q, want it to carry the save error", m.errorMsg)
	}
	if got := cfg.Downloader.NumParallelDownloads; got != 4 {
		t.Errorf("live config kept %d after a failed save, want the pre-apply 4", got)
	}
}

// The success path is unchanged: the callback's nil is "Saved".
//
// Mutant: treating a nil error as a failure.
func TestSuccessfulSaveStillReportsSaved(t *testing.T) {
	cfg := config.Defaults()
	store := config.NewStore(cfg, "")

	m := NewSettingsModel()
	m.configStore = store
	m.cfg = cfg
	m.Open(cfg)
	m.values["num_parallel_downloads"] = "9"
	m.dirty = true
	called := 0
	m.OnSave = func(*config.MoomboxConfig) error { called++; return nil }

	m.saveAndClose()

	if called != 1 {
		t.Errorf("OnSave called %d times, want 1", called)
	}
	if m.status != saveSaved {
		t.Errorf("status = %v, want saveSaved", m.status)
	}
	if got := cfg.Downloader.NumParallelDownloads; got != 9 {
		t.Errorf("live config = %d after a successful save, want the typed 9", got)
	}
}

// The restore has to cover every field the form can write, not the one the
// test above happens to type. A field-by-field undo list is what drifts: the
// snapshot is a whole-struct copy, so a field added to applyValues later is
// rolled back without anyone remembering to extend anything.
//
// Mutant: restoring only the field the failing save "was about" (or any
// hand-maintained subset) — one of these three comes back changed.
func TestFailedSaveRollsBackEveryTypedField(t *testing.T) {
	cfg := config.Defaults()
	cfg.Downloader.NumParallelDownloads = 4
	cfg.Downloader.MaxVideoResolution = 1080
	cfg.Logs.LogLevel = "INFO"
	cfg.Downloader.DownloadChat = false
	store := config.NewStore(cfg, "")

	m := NewSettingsModel()
	m.configStore = store
	m.cfg = cfg
	m.Open(cfg)
	m.values["num_parallel_downloads"] = "9"
	m.values["max_video_resolution"] = "2160"
	m.values["log_level"] = "DEBUG"
	m.values["download_chat"] = "Yes"
	m.dirty = true
	m.OnSave = func(*config.MoomboxConfig) error { return errors.New("read-only file system") }

	m.saveAndClose()

	if m.status != saveError {
		t.Fatalf("status = %v, want saveError", m.status)
	}
	if got := cfg.Downloader.NumParallelDownloads; got != 4 {
		t.Errorf("NumParallelDownloads = %d after a failed save, want 4", got)
	}
	if got := cfg.Downloader.MaxVideoResolution; got != 1080 {
		t.Errorf("MaxVideoResolution = %d after a failed save, want 1080", got)
	}
	if got := cfg.Logs.LogLevel; got != "INFO" {
		t.Errorf("LogLevel = %q after a failed save, want INFO", got)
	}
	if cfg.Downloader.DownloadChat {
		t.Error("DownloadChat = true after a failed save, want the pre-apply false")
	}
	// The panel stays dirty and open so the user can retry the save.
	if !m.dirty {
		t.Error("dirty = false after a failed save, want the changes kept for a retry")
	}
	if !m.IsVisible() {
		t.Error("the panel closed over a failed save")
	}
}

// A config applyValues refuses must never be announced as saved. Every
// range check runs before applyValues' write block, so the live config is
// untouched on this path — what the save-error branch has to keep doing is
// return before OnSave, or a refused edit reaches disk under a green
// "Saved" (and, with the widened callback, a nil error would then clear the
// error the user is supposed to read).
//
// Mutant: dropping saveAndClose's `if m.status == saveError { return }`
// guard after applyValues — OnSave runs and the panel closes on "Saved".
func TestValidationRefusalNeverReachesSave(t *testing.T) {
	cfg := config.Defaults()
	cfg.Downloader.NumParallelDownloads = 4
	cfg.Disk.WarnPercent = 90
	store := config.NewStore(cfg, "")

	m := NewSettingsModel()
	m.configStore = store
	m.cfg = cfg
	m.Open(cfg)
	m.values["num_parallel_downloads"] = "9"
	// Refused by the range check that mirrors config.Validate.
	m.values["disk_warn_percent"] = "0"
	m.dirty = true
	saved := 0
	m.OnSave = func(*config.MoomboxConfig) error { saved++; return nil }

	m.saveAndClose()

	if m.status != saveError {
		t.Fatalf("status = %v, want saveError", m.status)
	}
	if saved != 0 {
		t.Errorf("OnSave called %d times for a refused config, want 0", saved)
	}
	if got := cfg.Downloader.NumParallelDownloads; got != 4 {
		t.Errorf("NumParallelDownloads = %d after a refused save, want the pre-apply 4", got)
	}
	if !m.IsVisible() {
		t.Error("the panel closed over a refused config")
	}
}
