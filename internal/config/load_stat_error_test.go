package config

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// failStatFor makes statConfig fail with err for the one path named and
// behave as os.Stat for every other.
func failStatFor(t *testing.T, path string, err error) {
	t.Helper()
	prev := statConfig
	statConfig = func(name string) (os.FileInfo, error) {
		if name == path {
			return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
		}
		return prev(name)
	}
	t.Cleanup(func() { statConfig = prev })
}

// A named -config path that cannot be stat'ed for a reason other than "does
// not exist" is an error, never a first-run install on defaults.
//
// Mutant: restore `err == nil` as the only test in Load's customPath branch
// (Load returns Defaults and no error).
func TestLoadNamedPathStatErrorRefuses(t *testing.T) {
	t.Chdir(t.TempDir())
	isolateHomeConfig(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[network]\nport = 8123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	failStatFor(t, path, fs.ErrPermission)

	cfg, err := Load(path)
	if err == nil {
		t.Fatalf("Load returned a config (ConfigLoaded=%v) for a path that could not be stat'ed", cfg.ConfigLoaded)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("error %v does not wrap the Stat failure", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path", err)
	}
}

// The search skips a candidate that cannot be stat'ed, says so on stderr, and
// still answers from a later candidate or from defaults.
func TestLoadSearchSkipsAnUnreadableCandidateWithAWarning(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "config")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(nested, "config.toml")
	if err := os.WriteFile(want, []byte("[network]\nport = 8123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	isolateHomeConfig(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(cwd, "config.toml")
	failStatFor(t, first, fs.ErrPermission)
	want = filepath.Join(cwd, "config", "config.toml")

	var warnings bytes.Buffer
	prev := configWarnings
	configWarnings = &warnings
	t.Cleanup(func() { configWarnings = prev })

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LoadedFrom != want {
		t.Errorf("LoadedFrom = %q, want %q", cfg.LoadedFrom, want)
	}
	got := warnings.String()
	if !strings.HasPrefix(got, "warning: skipping config "+first+": ") || strings.Count(got, "\n") != 1 {
		t.Errorf("warnings = %q; want one line naming %s", got, first)
	}
}

// Candidates that simply do not exist are skipped without a word, as before.
func TestLoadSearchIsSilentAboutAbsentCandidates(t *testing.T) {
	t.Chdir(t.TempDir())
	isolateHomeConfig(t)
	var warnings bytes.Buffer
	prev := configWarnings
	configWarnings = &warnings
	t.Cleanup(func() { configWarnings = prev })

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ConfigLoaded || cfg.LoadedFrom != "" {
		t.Errorf("ConfigLoaded=%v LoadedFrom=%q with nothing on disk", cfg.ConfigLoaded, cfg.LoadedFrom)
	}
	if warnings.Len() != 0 {
		t.Errorf("warnings = %q; want none", warnings.String())
	}
}

// The real os.Stat, on a path whose parent is a regular file. Linux and macOS
// answer ENOTDIR, which is not "does not exist", so the named path refuses.
// Windows answers path-not-found, which Go reads as not-exist, so there the
// same path is a fresh install on defaults, as it always was.
func TestLoadNamedPathUnderARegularFile(t *testing.T) {
	t.Chdir(t.TempDir())
	isolateHomeConfig(t)
	parent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "config.toml")

	cfg, err := Load(path)
	if runtime.GOOS == "windows" {
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ConfigLoaded || cfg.LoadedFrom != "" {
			t.Errorf("ConfigLoaded=%v LoadedFrom=%q; want defaults", cfg.ConfigLoaded, cfg.LoadedFrom)
		}
		return
	}
	if err == nil {
		t.Fatal("Load returned defaults for a path under a regular file")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error %v reads as not-exist", err)
	}
}
