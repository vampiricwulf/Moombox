package treeguard

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(Run(m)) }

// mkfile writes an empty file at rel under dir, making its parents.
func mkfile(t *testing.T, dir, rel string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// What a run leaves is named outermost first: a new file, a new directory
// once (not every file in it), a file new inside a directory that was already
// there (testdata included, which belongs to the package). What was there
// before is not, nor is anything new inside another package's directory, nor
// a fuzz failure's corpus entry, nor a file the run's own flags asked for.
//
// Mutants: a new directory's files listed one by one (staging/a and
// staging/sub/b named); another package's directory entered (sub/pkg/new.go
// named); testdata treated as another package (testdata/new.json missed —
// testdata holds a .go file here); the fuzz carve-out dropped
// (testdata/fuzz/... named); the skip set ignored (cpu.out named); the
// before set ignored (old.txt named).
func TestLeftBehindNamesOnlyWhatTheRunAdded(t *testing.T) {
	dir := t.TempDir()
	mkfile(t, dir, "x_test.go")
	mkfile(t, dir, "old.txt")
	mkfile(t, dir, "testdata/golden.go")
	mkfile(t, dir, "sub/pkg/p.go")
	before := snapshot(dir)
	if before == nil {
		t.Fatal("snapshot: nil")
	}

	mkfile(t, dir, "leaked.log")
	mkfile(t, dir, "staging/a")
	mkfile(t, dir, "staging/sub/b")
	if runtime.GOOS != "windows" {
		// A Windows-style default on Linux: a relative name, made in the
		// package directory. (On Windows it is a drive path, and ':' cannot
		// appear in a file name.)
		mkfile(t, dir, `C:\Moombox\output/x`)
	}
	mkfile(t, dir, "testdata/new.json")
	mkfile(t, dir, "testdata/fuzz/FuzzX/abc123")
	mkfile(t, dir, "sub/pkg/new.go")
	mkfile(t, dir, "cpu.out")
	skip := map[string]bool{filepath.Join(dir, "cpu.out"): true}

	got := leftBehind(dir, before, skip)
	want := []string{"leaked.log", "staging/", "testdata/new.json"}
	if runtime.GOOS != "windows" {
		want = append([]string{`C:\Moombox\output/`}, want...)
	}
	if !slices.Equal(got, want) {
		t.Errorf("leftBehind = %q, want %q", got, want)
	}
}

// A directory holding no _test.go file is not a package's source directory
// — the temp dir a test re-runs its own binary in, say — and is not guarded.
//
// Mutant: packageDir guarding every working directory (the temp dir reads
// guarded).
func TestOnlyAPackageDirectoryIsGuarded(t *testing.T) {
	plain := t.TempDir()
	t.Chdir(plain)
	if _, guarded := packageDir(); guarded {
		t.Errorf("a directory with no _test.go file reads as a package directory")
	}
	pkg := t.TempDir()
	mkfile(t, pkg, "p_test.go")
	t.Chdir(pkg)
	if dir, guarded := packageDir(); !guarded || !filepath.IsAbs(dir) {
		t.Errorf("packageDir = %q, %v; want the absolute package directory, guarded", dir, guarded)
	}
}

// helperEnv selects what TestHelperProcess does when this binary is re-run
// as a package's test binary.
const helperEnv = "TREEGUARD_HELPER"

// TestHelperProcess is the package under guard when TestRunFailsALeakingRun
// re-runs this binary: it leaves a file in its working directory, writes one
// under its own t.TempDir(), or both. Without helperEnv it does nothing.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv(helperEnv) {
	case "leak":
		if err := os.WriteFile("moombox.log", []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	case "clean":
		if err := os.WriteFile(filepath.Join(t.TempDir(), "moombox.log"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Run fails a run whose tests pass but leave a file in the package
// directory, naming it, and passes the same run when the file goes under
// t.TempDir(). The run is this binary, re-run in a temp directory holding a
// _test.go file, as go test runs a package's binary in its directory.
//
// Mutants: Run returning m.Run's code whatever was left (the leaking run
// exits 0); the report not written (stderr names nothing); the snapshot
// taken after m.Run (nothing reads as new).
func TestRunFailsALeakingRun(t *testing.T) {
	run := func(mode string) (int, string, string) {
		t.Helper()
		pkg := t.TempDir()
		mkfile(t, pkg, "fake_test.go")
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.count=1")
		cmd.Dir = pkg
		cmd.Env = append(os.Environ(), helperEnv+"="+mode)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("running the helper: %v", err)
			}
			code = exit.ExitCode()
		}
		return code, stderr.String(), pkg
	}

	code, stderr, pkg := run("leak")
	if code == 0 {
		t.Errorf("a run that left moombox.log in its package directory exited 0")
	}
	if !strings.Contains(stderr, "moombox.log") {
		t.Errorf("stderr does not name the leaked file:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(pkg, "moombox.log")); err != nil {
		t.Errorf("the leaked file was not left in place for inspection: %v", err)
	}

	if code, stderr, _ := run("clean"); code != 0 {
		t.Errorf("a run that wrote only under t.TempDir() exited %d:\n%s", code, stderr)
	}
}

// runOutputs resolves the profile flags go test hands the binary against
// test.outputdir, so a profile written into the package directory is not
// taken for a leak, and a flag left empty names nothing.
//
// Mutant: a relative name not joined with test.outputdir (cpu.out resolved
// against the working directory instead).
func TestRunOutputsResolveAgainstTheOutputDir(t *testing.T) {
	out := t.TempDir()
	abs := filepath.Join(t.TempDir(), "mem.out")
	got := runOutputsFrom(func(name string) string {
		switch name {
		case "test.outputdir":
			return out
		case "test.cpuprofile":
			return "cpu.out"
		case "test.memprofile":
			return abs
		}
		return ""
	})
	want := map[string]bool{filepath.Join(out, "cpu.out"): true, filepath.Clean(abs): true}
	if len(got) != len(want) {
		t.Fatalf("runOutputs = %v, want %v", got, want)
	}
	for p := range want {
		if !got[p] {
			t.Errorf("runOutputs = %v, missing %s", got, p)
		}
	}
}

// moduleRoot is the directory holding go.mod, walked up to from the test's
// working directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}

// Every package with tests runs them through Run, from a TestMain in a file
// with no build constraint — so the guard holds on every OS the suite runs
// on, and a package added without it fails here rather than leaking quietly.
//
// Mutants: a tested package's TestMain calling m.Run directly (that package
// is named); the guard moved into a file with a //go:build line (named).
func TestEveryTestedPackageRunsTheGuard(t *testing.T) {
	root := moduleRoot(t)
	var missing []string
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root {
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules" ||
				name == "vendor" || name == "references" {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir // a nested module is its own
			}
		}
		tests, _ := filepath.Glob(filepath.Join(path, "*_test.go"))
		if len(tests) == 0 {
			return nil
		}
		checked++
		if !runsTheGuard(t, tests) {
			missing = append(missing, filepath.ToSlash(relOrSelf(root, path)))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 30 {
		t.Fatalf("found only %d tested packages under %s; the walk is not seeing the module", checked, root)
	}
	if len(missing) > 0 {
		t.Errorf("these tested packages do not run their tests through treeguard.Run from an unconstrained TestMain "+
			"(add `func TestMain(m *testing.M) { os.Exit(treeguard.Run(m)) }`, or wrap the existing TestMain's m.Run):\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// runsTheGuard reports whether one of files declares a TestMain that calls
// treeguard.Run (Run, in this package) in a file with no //go:build line.
func runsTheGuard(t *testing.T, files []string) bool {
	t.Helper()
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		constrained := false
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				if strings.HasPrefix(c.Text, "//go:build") {
					constrained = true
				}
			}
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" || fn.Body == nil {
				continue
			}
			if !constrained && callsRun(fn.Body, f.Name.Name == "treeguard") {
				return true
			}
		}
	}
	return false
}

// callsRun reports whether body calls treeguard.Run, or Run itself inside
// this package.
func callsRun(body *ast.BlockStmt, self bool) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if x, ok := fun.X.(*ast.Ident); ok && x.Name == "treeguard" && fun.Sel.Name == "Run" {
				found = true
			}
		case *ast.Ident:
			if self && fun.Name == "Run" {
				found = true
			}
		}
		return !found
	})
	return found
}

// relOrSelf is path relative to root, or path when it is not under it.
func relOrSelf(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return rel
	}
	return path
}
