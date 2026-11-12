package docs

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPruneGotmpRemovesOnlyWhatGoLeft runs `local-gate.sh --prune-gotmp` over
// a made-up GOTMPDIR and checks what is gone afterwards: the go command's
// build directories and the directories t.TempDir, b.TempDir and f.TempDir
// leave behind when a run is killed, and nothing else.
func TestPruneGotmpRemovesOnlyWhatGoLeft(t *testing.T) {
	if runtime.GOOS == "windows" && os.Getenv("MSYSTEM") == "" {
		t.Skip("needs Git Bash: outside it, `bash` on Windows may be WSL's")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}

	dir := t.TempDir()
	mkdir := func(parts ...string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(append([]string{dir}, parts...)...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(parts ...string) {
		t.Helper()
		mkdir(parts[:len(parts)-1]...)
		if err := os.WriteFile(filepath.Join(append([]string{dir}, parts...)...), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// What Go leaves: <name><digits>/<three digits>/..., and go-build<digits>.
	gone := []string{
		"go-build682577673",
		"TestAutoMux2403801933",
		"TestTable_case-a=b.c3157365795", // a subtest: TestTable/case-a=b.c
		"BenchmarkQ1238395314",
		"FuzzQ4290349777",
		"TestEmptied77", // cleanup removed its numbered directories, then died
	}
	write("go-build682577673", "b001", "pkg.test.exe")
	write("TestAutoMux2403801933", "001", "test.db")
	write("TestAutoMux2403801933", "002", "staging", "seg.ts")
	write("TestTable_case-a=b.c3157365795", "001", "f")
	write("BenchmarkQ1238395314", "001", "f")
	write("FuzzQ4290349777", "001", "f")
	mkdir("TestEmptied77")

	// What it does not: everything a person or an agent put there.
	kept := []string{
		"TestNotes",              // no digits at the end
		"TestPlan2026",           // looks like one, but holds a file of its own
		"TestRepoCopy1",          // holds a directory that is not a number
		"Testing123.txt",         // a file, not a directory
		"verify-clean-1c6c0ea0",  // an agent's copy of the repo
		"12345",                  // a bare number
		"ExampleOutput42",        // examples have no TempDir
		"server.go.bak",          // a file
		"Benchmarks-2026-11-11",  // a results folder: holds a file
		"my go-build notes",      // go-build is not at the start
		"TestHolds_a_long_one99", // its numbered entry is a file
	}
	mkdir("TestNotes", "001")
	write("TestPlan2026", "plan.md")
	write("TestRepoCopy1", "internal", "x.go")
	write("Testing123.txt")
	write("verify-clean-1c6c0ea0", "go.mod")
	write("12345", "001", "f")
	write("ExampleOutput42", "001", "f")
	write("server.go.bak")
	write("Benchmarks-2026-11-11", "results.txt")
	write("my go-build notes")
	write("TestHolds_a_long_one99", "001")

	script, err := filepath.Abs(filepath.Join("..", "..", ".github", "scripts", "local-gate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, filepath.ToSlash(script), "--prune-gotmp")
	cmd.Env = append(os.Environ(), "GOTMPDIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("local-gate.sh --prune-gotmp: %v\n%s", err, out)
	}

	for _, name := range gone {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s is still there (stat error: %v)", name, err)
		}
	}
	for _, name := range kept {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}

	text := string(out)
	for _, want := range []string{
		"pruned 1 build directories and 5 test temp directories",
		"left 11 other entries",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the output does not say %q:\n%s", want, text)
		}
	}
	for _, name := range kept {
		if !strings.Contains(text, name) {
			t.Errorf("the output does not list the entry it left, %s:\n%s", name, text)
		}
	}
}
