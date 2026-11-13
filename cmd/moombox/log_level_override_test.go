package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The helper only holds the rule if the boot path actually uses it that way.
// TestBootKeepsTheLogLevelOverrideOutOfTheConfigFile runs the boot's config
// and logger step (initConfigAndLogger) and reads the file back; this scan
// covers the rest of the package, which a test cannot run (initServices goes
// on to open the database, bind the web server and start the monitors): this
// package writes the boot log level into the LOGGER and never
// into the config struct, which is what keeps the override out of the boot
// auto-persist, the password auto-hash Store.Update and every later UI save
// — all three of which encode whatever cfg.Logs.LogLevel holds.
//
// Mutant: putting `cfg.Logs.LogLevel = logLevelOverride` back into
// initConfigAndLogger, or anywhere else in package main — the assignment
// scan names the file and line. Mutant:
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
