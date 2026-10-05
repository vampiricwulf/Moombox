package sidecar

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A child that dies during init — a module that will not load, a missing
// shared library, a payload cut short — used to report only "sidecar exited
// before ready": the cause was on its stderr, unprefixed and logged at Debug,
// so the Warn, the health reason and the alert all said nothing useful. And
// with the stamp still valid, the supervisor's retries ran the same broken
// tree forever. The start error now carries the child's last stderr lines,
// and the first such failure drops the stamp so the next start re-extracts —
// which here repairs it.
//
// Mutants: stderrTailSuffix left off the error; the stamp not removed (the
// second start fails the same way).
func TestAChildThatDiesBeforeReadySaysWhyAndIsReextracted(t *testing.T) {
	requireBlobs(t)
	dir := t.TempDir()
	if err := extractIfNeeded(dir); err != nil {
		t.Fatal(err)
	}
	serverJS := filepath.Join(dir, "src", "server.js")
	if err := os.WriteFile(serverJS, []byte(`import "moombox-missing-module";`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(Config{CacheDir: dir, Logger: silentLogger{}, StartupTimeout: 30 * time.Second})
	err := s.Start(context.Background())
	if err == nil {
		_ = s.Stop()
		t.Fatal("a broken server.js started")
	}
	t.Logf("start error: %v", err)
	if !strings.Contains(err.Error(), "moombox-missing-module") {
		t.Errorf("start error %q does not carry the child's own reason", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "version.txt")); !os.IsNotExist(statErr) {
		t.Errorf("the stamp survived the failed start (stat err %v) — the broken tree would be reused", statErr)
	}

	// The next start re-extracts and comes up.
	s2 := New(Config{CacheDir: dir, Logger: silentLogger{}, StartupTimeout: 60 * time.Second})
	if err := s2.Start(context.Background()); err != nil {
		t.Fatalf("the start after re-extraction: %v", err)
	}
	_ = s2.Stop()
}
