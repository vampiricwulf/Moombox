package main

import (
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// bootConfigAndLogger runs initConfigAndLogger — the boot's config load,
// logger and its config writers — against a config file in a temp directory
// and returns the state it filled in, the config path and the log path. The
// file is one a real upgrade meets: a configured level, no [bgutils] section
// (so the boot auto-persist runs) and a password written by hand in plaintext
// (so the auto-hash Store.Update runs). go_soft_limit_mb is 0 so the test
// binary's own memory limit is left alone.
func bootConfigAndLogger(t *testing.T, override string) (s *runState, configPath, logPath string) {
	t.Helper()
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.toml")
	logPath = filepath.Join(dir, "moombox.log")
	conf := fmt.Sprintf("[logs]\nlog_level = \"INFO\"\n\n[paths]\nlog_file_path = '%s'\n\n[memory]\ngo_soft_limit_mb = 0\n\n[network]\npassword_hash = \"a hand-written password\"\n", logPath)
	if err := os.WriteFile(configPath, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	s = &runState{configPath: configPath}
	// Disarm the crash output and close the log before the temp dir goes:
	// Windows will not delete a file the runtime or the logger holds open.
	t.Cleanup(func() {
		_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
		if s.closeLog != nil {
			s.closeLog()
		}
	})
	if err := s.initConfigAndLogger(override); err != nil {
		t.Fatalf("initConfigAndLogger: %v", err)
	}
	return s, configPath, logPath
}

// TestBootKeepsTheLogLevelOverrideOutOfTheConfigFile is CORE-10 pinned by
// running the boot code itself. -log-level is a one-off diagnostic: it sets
// the level the logger starts at and nothing else. Written into
// cfg.Logs.LogLevel (as initServices once did), it was saved by the boot
// auto-persist, by the password auto-hash and by every later settings save,
// so one `-log-level=debug` run changed the configured level for good.
//
// Here both boot writers run for real against a file on disk, and the file,
// the config struct and the level a later save compares against all still
// hold the configured level, while the logger itself runs at the override.
//
// Mutants: `cfg.Logs.LogLevel = logLevelOverride` back in
// initConfigAndLogger (the file reads DEBUG); logger.New given
// cfg.Logs.LogLevel alone (the debug probe never reaches the log);
// s.configuredLogLevel taken from effectiveLogLevel (the next unrelated save
// would see a "changed" level and end the override).
func TestBootKeepsTheLogLevelOverrideOutOfTheConfigFile(t *testing.T) {
	s, configPath, logPath := bootConfigAndLogger(t, "DEBUG")

	if s.cfg == nil || s.configStore == nil || s.log == nil || s.closeLog == nil {
		t.Fatalf("initConfigAndLogger left state unset: cfg=%v store=%v log=%v closeLog=%v",
			s.cfg != nil, s.configStore != nil, s.log != nil, s.closeLog != nil)
	}
	if s.configPath != configPath {
		t.Errorf("s.configPath = %q, want the file that was loaded, %q", s.configPath, configPath)
	}
	if s.cfg.Logs.LogLevel != "INFO" {
		t.Errorf("cfg.Logs.LogLevel = %q, want the configured INFO: every saver encodes this struct", s.cfg.Logs.LogLevel)
	}
	if s.configuredLogLevel != "INFO" {
		t.Errorf("configuredLogLevel = %q, want the configured INFO", s.configuredLogLevel)
	}
	if s.cfg.NeedsAutoPersist {
		t.Error("NeedsAutoPersist is still set: the boot auto-persist did not run, so this test proved less than it says")
	}

	// The logger runs at the override: a Debug line reaches the file.
	s.log.Debug("override probe line")
	s.closeLog()
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"override probe line",
		"Configuration loaded",
		"Auto-persisted new config sections to disk",
		"Plaintext password detected",
	} {
		if !strings.Contains(string(logged), want) {
			t.Errorf("the log has no %q line:\n%s", want, logged)
		}
	}

	// The file both boot writers wrote.
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"DEBUG"`) {
		t.Errorf("the override reached disk:\n%s", data)
	}
	if strings.Contains(string(data), "a hand-written password") {
		t.Errorf("the password auto-hash did not write the file, so its save was not exercised:\n%s", data)
	}
	if !strings.Contains(string(data), "[bgutils]") {
		t.Errorf("the auto-persist did not write the new sections, so its save was not exercised:\n%s", data)
	}
	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Logs.LogLevel != "INFO" {
		t.Errorf("the file's log_level = %q after a -log-level=DEBUG boot, want INFO", reloaded.Logs.LogLevel)
	}
	if !web.IsScryptHash(reloaded.Network.PasswordHash) {
		t.Errorf("the file's password_hash is not a scrypt hash: %q", reloaded.Network.PasswordHash)
	}
}

// Without an override the logger starts at the configured level, so the
// probe above is evidence of the override and not of a logger that writes
// everything.
//
// Mutant: effectiveLogLevel's result replaced by "DEBUG" at the logger.New
// call.
func TestBootWithoutAnOverrideLogsAtTheConfiguredLevel(t *testing.T) {
	s, _, logPath := bootConfigAndLogger(t, "")
	s.log.Debug("override probe line")
	s.log.Info("info probe line")
	s.closeLog()
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), "override probe line") {
		t.Error("a Debug line reached the log with log_level INFO and no override")
	}
	if !strings.Contains(string(logged), "info probe line") {
		t.Errorf("an Info line did not reach the log:\n%s", logged)
	}
}

// initConfigAndLogger is only the boot's code while initServices runs it,
// first: ahead of the database, which opens at the path the config names and
// logs to the logger it built.
//
// Mutants: drop the s.initConfigAndLogger call from initServices, move it
// below database.Open, or pass it "" in place of logLevelOverride (the
// -log-level flag then reaches nothing).
func TestInitServicesStartsWithTheConfigAndLogger(t *testing.T) {
	calls, fset := callsInFunc(t, "services.go", "initServices")
	boot := firstCall(calls, "s", "initConfigAndLogger")
	if boot == token.NoPos {
		t.Fatal("initServices does not call s.initConfigAndLogger")
	}
	open := firstCall(calls, "database", "Open")
	if open == token.NoPos {
		t.Fatal("initServices does not call database.Open — re-anchor this test rather than deleting it")
	}
	if open < boot {
		t.Errorf("initServices opens the database (%s) before the config and logger exist (%s)", fset.Position(open), fset.Position(boot))
	}
	for _, c := range calls {
		if c.recv != "s" || c.name != "initConfigAndLogger" {
			continue
		}
		if got := strings.Join(c.args, ", "); got != "logLevelOverride" {
			t.Errorf("initServices calls s.initConfigAndLogger(%s) at %s; want its own logLevelOverride, the -log-level flag", got, fset.Position(c.pos))
		}
	}
	for _, c := range calls {
		if c.pos < boot && c.name != "Println" {
			t.Errorf("initServices calls %s.%s at %s before s.initConfigAndLogger", c.recv, c.name, fset.Position(c.pos))
		}
	}
}
