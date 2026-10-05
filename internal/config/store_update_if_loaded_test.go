package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Background bookkeeping must not be a first run's first Save: Save marks the
// config loaded and both setup wizards key off that flag, so a cookies.txt
// already beside the binary (its platforms detected at boot) skipped setup
// entirely. Returning early from an Update closure is no guard — Update saves
// whatever the closure did, nothing included — which is how PersistPlatforms'
// own `if !c.ConfigLoaded { return }` still created config.toml mid-setup.
//
// Mutant: UpdateIfLoaded calling Update without its ConfigLoaded check.
func TestUpdateIfLoadedWaitsForTheFirstSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Defaults()
	cfg.ConfigLoaded = false
	s := NewStore(cfg, path)

	called := false
	applied, err := s.UpdateIfLoaded(func(c *MoomboxConfig) { called = true; c.Cookies.Platforms = []string{"youtube"} })
	if err != nil || applied || called {
		t.Fatalf("first run: applied=%v called=%v err=%v, want nothing done", applied, called, err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("first run: config.toml was created (stat err %v)", statErr)
	}
	s.Read(func(c *MoomboxConfig) {
		if c.ConfigLoaded {
			t.Error("first run: the config reads as loaded")
		}
	})

	// The trap it replaces: an early-returning Update closure still saves.
	if err := s.Update(func(*MoomboxConfig) {}); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("an empty Update did not save, so the premise of this guard is gone: %v", statErr)
	}

	applied, err = s.UpdateIfLoaded(func(c *MoomboxConfig) { c.Cookies.Platforms = []string{"twitch"} })
	if err != nil || !applied {
		t.Fatalf("loaded: applied=%v err=%v", applied, err)
	}
	s.Read(func(c *MoomboxConfig) {
		if len(c.Cookies.Platforms) != 1 || c.Cookies.Platforms[0] != "twitch" {
			t.Errorf("loaded: platforms %v, want [twitch]", c.Cookies.Platforms)
		}
	})
}
