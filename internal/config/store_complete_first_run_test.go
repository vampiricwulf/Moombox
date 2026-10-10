package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCompleteFirstRunSavesOnce pins the W27 review of W26-11: both setup
// wizards end in CompleteFirstRun, which saves once. On a first run in TUI
// mode the TUI wizard and the dashboard tab's Web wizard are open together;
// the TUI's OnComplete saved its config with no first-run check and left the
// live config unloaded, so a TUI save after a Web complete overwrote the Web
// wizard's config.toml, and a Web complete after a TUI save passed the
// route's recheck. The first save now marks the live config loaded and
// becomes it; a second is refused with ErrSetupCompleted and writes nothing.
// A save that fails leaves the live config as it was.
//
// Mutants killed: dropping the ConfigLoaded check (the second complete
// saves over the first); not making next the live config (the second
// complete applies, the live port is the default); making it live before
// the save (a failed save is loaded and its port live).
func TestCompleteFirstRunSavesOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	store := NewStore(Defaults(), path)

	first := Defaults()
	first.Network.Port = 7100
	if err := store.CompleteFirstRun(first); err != nil {
		t.Fatalf("first complete: %v", err)
	}
	var loaded bool
	var port int
	store.Read(func(c *MoomboxConfig) { loaded, port = c.ConfigLoaded, c.Network.Port })
	if !loaded || port != 7100 {
		t.Errorf("live config after the first complete: loaded %v port %d, want loaded with port 7100", loaded, port)
	}

	second := Defaults()
	second.Network.Port = 7200
	if err := store.CompleteFirstRun(second); !errors.Is(err, ErrSetupCompleted) {
		t.Errorf("second complete: %v, want ErrSetupCompleted", err)
	}
	onDisk, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if onDisk.Network.Port != 7100 {
		t.Errorf("config.toml port %d after a refused second complete, want the first's 7100", onDisk.Network.Port)
	}

	// A save path whose parent is a file: the save fails.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	failing := NewStore(Defaults(), filepath.Join(blocker, "config.toml"))
	next := Defaults()
	next.Network.Port = 7300
	if err := failing.CompleteFirstRun(next); err == nil || errors.Is(err, ErrSetupCompleted) {
		t.Fatalf("complete onto an unwritable path: %v, want the save's error", err)
	}
	failing.Read(func(c *MoomboxConfig) { loaded, port = c.ConfigLoaded, c.Network.Port })
	if loaded || port == 7300 {
		t.Errorf("a failed save changed the live config: loaded %v port %d", loaded, port)
	}
}
