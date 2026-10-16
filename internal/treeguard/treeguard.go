// Package treeguard keeps a package's tests out of its source tree. go test
// runs a package's tests in the package's own directory, so a test that
// writes to a relative path — a config built from config.Defaults() whose
// "./output" and "./staging" a handler then creates, a log or database file
// named without a directory, or a path that is absolute on one OS and only
// a relative name on the other (`C:\Moombox\staging` on Linux) — leaves it
// beside the source, where most such names are gitignored and nothing says
// it happened. Every tested package's TestMain runs its tests through Run,
// which fails the run when they leave anything new in the package directory.
package treeguard

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Run runs m's tests and returns the exit code TestMain hands to os.Exit:
// m's own, or 1 when the run left a file or directory in the package
// directory that was not there before it (each named on stderr).
//
// The directory is the working directory go test starts the binary in, read
// before m.Run and held as an absolute path, so a test's t.Chdir does not
// move it. A binary started anywhere else — a test re-running its own binary
// as a helper process in a temp dir — holds no _test.go file in its working
// directory and is not checked. What is left is not removed: it is the
// evidence, and a later run sees it in its starting snapshot, so it is the
// first run after a leak appears that fails (CI's, on a fresh checkout).
func Run(m *testing.M) int {
	dir, guarded := packageDir()
	var before map[string]bool
	if guarded {
		before = snapshot(dir)
	}
	code := m.Run()
	if !guarded || before == nil {
		return code
	}
	if left := leftBehind(dir, before, runOutputsFrom(flagValue)); len(left) > 0 {
		report(os.Stderr, dir, left)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// packageDir is the working directory, and whether it is a package's source
// directory (it holds a _test.go file).
func packageDir() (string, bool) {
	wd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	abs, err := filepath.Abs(wd)
	if err != nil {
		return "", false
	}
	tests, err := filepath.Glob(filepath.Join(abs, "*_test.go"))
	if err != nil || len(tests) == 0 {
		return abs, false
	}
	return abs, true
}

// snapshot is every path under dir, slash-separated and relative to it. A
// subdirectory that is another package (it holds a .go file, testdata aside)
// is listed but not entered: its files are its own tests' to keep clean, and
// another package's run, possibly in parallel with this one, must not be
// blamed on this one. nil when dir cannot be read.
func snapshot(dir string) map[string]bool {
	paths := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				return err
			}
			return nil // vanished or unreadable mid-walk: nothing to compare
		}
		if path == dir {
			return nil
		}
		rel := relSlash(dir, path)
		paths[rel] = true
		if d.IsDir() && isOtherPackage(path, rel) {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil
	}
	return paths
}

// leftBehind is what is under dir now and was not in before, outermost
// first: a new directory is named once, not with every file in it. Paths in
// skip — what go test itself asked the run to write (runOutputsFrom) — are not
// counted, nor is a fuzz failure's new corpus entry under testdata/fuzz,
// which go test writes there on purpose.
func leftBehind(dir string, before map[string]bool, skip map[string]bool) []string {
	var left []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return nil
		}
		rel := relSlash(dir, path)
		if before[rel] {
			if d.IsDir() && isOtherPackage(path, rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if skip[filepath.Clean(path)] || rel == "testdata/fuzz" || strings.HasPrefix(rel, "testdata/fuzz/") {
			return nil
		}
		if d.IsDir() {
			left = append(left, rel+"/")
			return filepath.SkipDir
		}
		left = append(left, rel)
		return nil
	})
	slices.Sort(left)
	return left
}

// isOtherPackage reports whether the directory at path (rel to the package
// directory) is a package of its own: it holds a .go file and is not
// testdata, which belongs to the package above it.
func isOtherPackage(path, rel string) bool {
	if rel == "testdata" || strings.HasPrefix(rel, "testdata/") {
		return false
	}
	gofiles, err := filepath.Glob(filepath.Join(path, "*.go"))
	return err == nil && len(gofiles) > 0
}

// outputFlags are the testing flags that name a file the run writes when it
// ends — inside m.Run, so before Run looks — and test.outputdir, which go
// test sets to the directory it was started in (the package directory when
// that is where it ran) and the testing package resolves a relative name
// against.
var outputFlags = []string{
	"test.coverprofile", "test.cpuprofile", "test.memprofile",
	"test.blockprofile", "test.mutexprofile", "test.trace",
}

// runOutputsFrom is the absolute paths of the files the run's own flags
// asked for, each flag read through lookup (flagValue): `go test -cpuprofile
// cpu.out` started in the package directory writes cpu.out there, and that
// is no test's leak. Read after m.Run, which is where the flags are parsed.
func runOutputsFrom(lookup func(name string) string) map[string]bool {
	out := map[string]bool{}
	outdir := lookup("test.outputdir")
	for _, name := range outputFlags {
		v := lookup(name)
		if v == "" {
			continue
		}
		if !filepath.IsAbs(v) && outdir != "" {
			v = filepath.Join(outdir, v)
		}
		if abs, err := filepath.Abs(v); err == nil {
			out[filepath.Clean(abs)] = true
		}
	}
	return out
}

// flagValue is the named flag's value, or "" when it is not defined.
func flagValue(name string) string {
	if f := flag.Lookup(name); f != nil {
		return f.Value.String()
	}
	return ""
}

// relSlash is path relative to dir, slash-separated so a report reads the
// same on Windows.
func relSlash(dir, path string) string {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		rel = path
	}
	return filepath.ToSlash(rel)
}

// report names what the run left behind, and where a test should have put it.
func report(w io.Writer, dir string, left []string) {
	fmt.Fprintf(w, "FAIL: the tests left %d new path(s) in the package directory %s:\n", len(left), dir)
	for _, p := range left {
		fmt.Fprintf(w, "  %s\n", p)
	}
	fmt.Fprintln(w, "Write them under t.TempDir(): give a config built from config.Defaults() temp-dir paths "+
		"(output, staging, database, log, cookie file), and use filepath.Join(t.TempDir(), ...) for a path "+
		"that must be absolute on every OS. -run narrows down which test wrote them; they are left in place.")
}
