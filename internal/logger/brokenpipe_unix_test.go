//go:build unix

package logger

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAStdoutWhoseReaderWentAwayNeverKillsTheProcess is W24-10's Unix shape.
// It runs a Logger in a child (the test binary re-executed) whose fd 1 is the
// write end of a pipe; the reader takes the first line and then goes away, as
// an `ssh host moombox --headless` session does when it drops (no pty, so fd
// 1 is a pipe to sshd) or a `| tee` that exits. The Go runtime kills a process
// that writes to a broken pipe on fd 1 or 2 unless it asked for SIGPIPE, so
// the next line's stdout write ended the run: no error ever came back for the
// sinks to swallow, and that line and every later one — and every recording
// in progress — went with the process.
//
// Mutant: New without its SurviveBrokenPipes call — the child dies of
// SIGPIPE, and moombox.log stops before the second line logged after the
// reader went away (the first is on disk already: the file is written first).
func TestAStdoutWhoseReaderWentAwayNeverKillsTheProcess(t *testing.T) {
	if os.Getenv("MOOMBOX_BROKEN_PIPE_CHILD") == "1" {
		l, err := New(os.Getenv("MOOMBOX_BROKEN_PIPE_LOG"), "INFO", 1<<20, 1)
		if err != nil {
			os.Exit(3)
		}
		l.Info("while the reader is alive")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n') // the parent drops the reader
		l.Info("after the reader went away", "n", 1)
		l.Warn("after the reader went away", "n", 2)
		l.Close()
		os.Exit(0)
	}

	logPath := filepath.Join(t.TempDir(), "moombox.log")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAStdoutWhoseReaderWentAwayNeverKillsTheProcess$")
	cmd.Env = append(os.Environ(), "MOOMBOX_BROKEN_PIPE_CHILD=1", "MOOMBOX_BROKEN_PIPE_LOG="+logPath)
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close() // the child holds the only write end now

	first, err := bufio.NewReader(r).ReadString('\n')
	if err != nil || !strings.Contains(first, "while the reader is alive") {
		t.Errorf("stdout's reader took %q (%v), want the first line", first, err)
	}
	r.Close() // the reader goes away
	_, _ = stdin.Write([]byte("go\n"))
	_ = stdin.Close()
	runErr := cmd.Wait()
	if ctx.Err() != nil {
		t.Fatalf("the child did not exit:\n%s", stderr.String())
	}

	if runErr != nil {
		t.Errorf("the child ended with %v writing to a stdout whose reader went away, want exit 0; stderr:\n%s", runErr, stderr.String())
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"while the reader is alive", "n=1", "n=2"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("moombox.log lacks %q after stdout's reader went away:\n%s", want, data)
		}
	}
}
