package sidecar

import (
	"io"
	"strings"
	"testing"
	"time"
)

// A stderr line past the scanner's 1 MiB cap ended stderrPump, and with no
// reader left a child that kept writing to stderr blocked on the full pipe.
// The pump now drains the rest unread.
//
// Mutant: the io.Copy drain removed — the writer below blocks on the pipe.
func TestStderrPumpKeepsDrainingAfterAnOverlongLine(t *testing.T) {
	r, w := io.Pipe()
	s := New(Config{CacheDir: t.TempDir(), Logger: silentLogger{}})
	s.stderr = r
	s.pumpsDone.Add(1)
	pumpDone := make(chan struct{})
	go func() { defer close(pumpDone); s.stderrPump() }()

	wrote := make(chan error, 1)
	go func() {
		if _, err := io.WriteString(w, strings.Repeat("x", 2<<20)+"\n"); err != nil {
			wrote <- err
			return
		}
		for range 64 {
			if _, err := io.WriteString(w, "still talking\n"); err != nil {
				wrote <- err
				return
			}
		}
		wrote <- w.Close()
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the child's stderr writes blocked after an overlong line")
	}
	select {
	case <-pumpDone:
	case <-time.After(5 * time.Second):
		t.Fatal("stderrPump did not return once stderr closed")
	}
}
