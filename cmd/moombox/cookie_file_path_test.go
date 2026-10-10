package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// The cookie advice in alerts and the status messages names the file the
// services actually read and write — the jar's — not cookies.cookie_file,
// which after a save without the restart it needs names a file nothing
// touches.
//
// Mutant: cookieFilePath reading the setting first again.
func TestCookieAdviceNamesTheJarsFile(t *testing.T) {
	boot := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(boot, []byte("# Netscape HTTP Cookie File\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(boot); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Cookies.CookieFile = "/somewhere/else/cookies.txt" // saved, restart declined
	s := &runState{jar: jar, configStore: config.NewStore(cfg, "")}
	if got := s.cookieFilePath(); got != boot {
		t.Errorf("cookieFilePath() = %q, want the jar's %q", got, boot)
	}

	s.jar = nil // before the jar exists the setting is all there is
	if got := s.cookieFilePath(); got != cfg.Cookies.CookieFile {
		t.Errorf("without a jar cookieFilePath() = %q, want the setting", got)
	}
}
