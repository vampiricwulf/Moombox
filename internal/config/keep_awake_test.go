package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestKeepAwakeDefaultsOnAndSurvivesAFileWithoutIt is owner ruling R2-X5's
// default: downloader.keep_awake is on, and a config file written before the
// key existed keeps it on rather than reading the missing key as false.
//
// Mutant: KeepAwake left out of Defaults() (both "on" assertions fail).
func TestKeepAwakeDefaultsOnAndSurvivesAFileWithoutIt(t *testing.T) {
	if !Defaults().Downloader.KeepAwake {
		t.Fatal("Defaults().Downloader.KeepAwake = false, want true")
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		body string
		want bool
	}{
		{"[downloader]\nprefer_60fps = true\n", true},
		{"[downloader]\nkeep_awake = false\n", false},
	} {
		path := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadFromFile(path)
		if err != nil {
			t.Fatalf("loadFromFile(%q): %v", tc.body, err)
		}
		if cfg.Downloader.KeepAwake != tc.want {
			t.Errorf("%q: KeepAwake = %v, want %v", tc.body, cfg.Downloader.KeepAwake, tc.want)
		}
	}
}
