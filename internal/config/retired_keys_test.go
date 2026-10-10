package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// downloader.po_token and downloader.visitor_data were accepted and saved as a
// "manual PO token override" that nothing ever read. They are retired: Load
// records them so boot can say they are ignored, and the next save leaves
// them out of the file.
//
// Mutant: retiredKeysIn returning nothing — the keys go unreported.
func TestRetiredKeysAreReportedAndDroppedOnSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[downloader]\nsegment_workers = 4\npo_token = \"abc\"\nvisitor_data = \"xyz\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"downloader.po_token", "downloader.visitor_data"}; !slices.Equal(cfg.IgnoredOnLoad, want) {
		t.Errorf("IgnoredOnLoad = %q, want %q", cfg.IgnoredOnLoad, want)
	}
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"po_token", "visitor_data"} {
		if strings.Contains(string(saved), key) {
			t.Errorf("the saved file still carries %s", key)
		}
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.IgnoredOnLoad) != 0 {
		t.Errorf("a file without retired keys recorded %q", again.IgnoredOnLoad)
	}
}
