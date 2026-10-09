//go:build unix

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMainSurvivesAStdoutWhoseReaderWentAway runs main() in a child (the test
// binary re-executed) whose stdout is a pipe nobody reads any more. On Unix a
// Go process that writes to a broken pipe on fd 1 or 2 dies of SIGPIPE unless
// it asked for the signal, and the logger's own request comes too late for
// two writers: the launcher, which writes its crash-supervision and rollback
// notices to stderr and never builds a Logger — so a launcher whose ssh
// session had dropped died at its next notice instead of respawning the
// child — and the child's banner, printed before its Logger exists. So
// main's first act is logger.SurviveBrokenPipes. The cheapest path through
// main that writes is -version=true; run without _MOOMBOX_CHILD, the way the
// launcher starts.
//
// Mutant: drop the call from main — the child dies of SIGPIPE at the version
// line.
func TestMainSurvivesAStdoutWhoseReaderWentAway(t *testing.T) {
	if os.Getenv("MOOMBOX_CLI_HELPER") == "1" {
		os.Args = append([]string{"moombox"}, strings.Fields(os.Getenv("MOOMBOX_CLI_ARGS"))...)
		main()
		os.Exit(0)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close() // the reader is gone before the first write

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMainSurvivesAStdoutWhoseReaderWentAway$")
	cmd.Dir = t.TempDir()
	env := []string{"MOOMBOX_CLI_HELPER=1", "MOOMBOX_CLI_ARGS=-version=true", "MOOMBOX_NO_TUI=1"}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "_MOOMBOX_CHILD=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	runErr := cmd.Wait()
	if ctx.Err() != nil {
		t.Fatalf("the command did not exit:\n%s", stderr.String())
	}
	if runErr != nil {
		t.Errorf("main ended with %v writing to a stdout whose reader went away, want exit 0; stderr:\n%s", runErr, stderr.String())
	}
}
