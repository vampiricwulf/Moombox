package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPreferHDRDefaultsOnAndSurvivesAFileWithoutIt is owner decision D13's
// default: downloader.prefer_hdr is on, and a config file written before the
// key existed reads it as on — loadFromFile decodes over Defaults(), which is
// why the key needs no migration — while an explicit false is kept.
//
// Mutants: PreferHDR left out of Defaults() (both "on" assertions fail); the
// toml tag misspelt (the explicit false reads as true).
func TestPreferHDRDefaultsOnAndSurvivesAFileWithoutIt(t *testing.T) {
	if !Defaults().Downloader.PreferHDR {
		t.Fatal("Defaults().Downloader.PreferHDR = false, want true")
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		body string
		want bool
	}{
		{"[downloader]\nprefer_60fps = true\n", true},
		{"[downloader]\nprefer_hdr = false\n", false},
	} {
		path := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadFromFile(path)
		if err != nil {
			t.Fatalf("loadFromFile(%q): %v", tc.body, err)
		}
		if cfg.Downloader.PreferHDR != tc.want {
			t.Errorf("%q: PreferHDR = %v, want %v", tc.body, cfg.Downloader.PreferHDR, tc.want)
		}
	}
}
