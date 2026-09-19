package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// -log-level is a one-off diagnostic. Writing it into cfg.Logs.LogLevel (as
// initServices did) made the boot auto-persist, the password auto-hash
// Store.Update and every later UI save write it to disk, so a single
// `-log-level=debug` run permanently changed the configured level (CORE-10).
//
// Mutant: restoring `cfg.Logs.LogLevel = logLevelOverride` — the saved file
// reads DEBUG.
func TestLogLevelOverrideNeverReachesDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[logs]\nlog_level = \"INFO\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(cfg, path)

	// The boot sequence initServices runs, with the override applied the way
	// the fix applies it: to the logger's level only.
	level := effectiveLogLevel(cfg.Logs.LogLevel, "DEBUG")
	if level != "DEBUG" {
		t.Fatalf("effectiveLogLevel = %q, want the override DEBUG", level)
	}
	if err := store.SaveLocked(); err != nil {
		t.Fatalf("SaveLocked: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToUpper(string(data)), "DEBUG") {
		t.Errorf("the override reached disk:\n%s", data)
	}
	// The same struct the store just wrote is what every later saver holds,
	// so the configured level has to have survived the boot untouched.
	if cfg.Logs.LogLevel != "INFO" {
		t.Errorf("cfg.Logs.LogLevel = %q, want the configured INFO", cfg.Logs.LogLevel)
	}
}

// The helper only holds the rule if the boot path actually uses it that way,
// and initServices is not callable from a test (it opens the database, binds
// the web server and starts the monitors). So the call site is pinned where
// it lives: this package writes the boot log level into the LOGGER and never
// into the config struct, which is what keeps the override out of the boot
// auto-persist, the password auto-hash Store.Update and every later UI save
// — all three of which encode whatever cfg.Logs.LogLevel holds.
//
// Mutant: putting `cfg.Logs.LogLevel = logLevelOverride` back into
// initServices — the assignment scan names the file and line. Mutant:
// passing cfg.Logs.LogLevel straight to logger.New — the -log-level flag
// stops working and the effectiveLogLevel argument check fails.
func TestLogLevelOverrideReachesTheLoggerAndNothingElse(t *testing.T) {
	fset := token.NewFileSet()
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	loggerNewCalls := 0
	scanned := 0
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if file.Name.Name != "main" {
			continue
		}
		scanned++
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if ok && sel.Sel.Name == "LogLevel" {
						t.Errorf("%s: assigns the config's LogLevel — the boot override must reach logger.New only, never the struct every saver encodes", fset.Position(sel.Pos()))
					}
				}
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "New" {
					return true
				}
				pkgIdent, ok := sel.X.(*ast.Ident)
				if !ok || pkgIdent.Name != "logger" {
					return true
				}
				loggerNewCalls++
				for _, arg := range node.Args {
					if call, ok := arg.(*ast.CallExpr); ok {
						if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "effectiveLogLevel" {
							return true
						}
					}
				}
				t.Errorf("%s: logger.New is built without effectiveLogLevel, so -log-level cannot reach it", fset.Position(node.Pos()))
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("no package main sources scanned — the checks above proved nothing")
	}
	if loggerNewCalls == 0 {
		t.Error("no logger.New call found in package main — the scan above proved nothing")
	}
}

// effectiveLogLevel is deliberately a pure two-string function so the rule
// can be asserted without a boot: no override means the configured level,
// and nothing else in the process gets to see the override.
//
// Mutant: returning the override unconditionally (an empty -log-level would
// silence the logger entirely) or ignoring it (the flag stops working).
func TestEffectiveLogLevelPrefersTheOverride(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		override   string
		want       string
	}{
		{"no override keeps the configured level", "WARN", "", "WARN"},
		{"override wins", "WARN", "DEBUG", "DEBUG"},
		{"empty config with no override stays empty", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveLogLevel(tc.configured, tc.override); got != tc.want {
				t.Errorf("effectiveLogLevel(%q, %q) = %q, want %q", tc.configured, tc.override, got, tc.want)
			}
		})
	}
}
