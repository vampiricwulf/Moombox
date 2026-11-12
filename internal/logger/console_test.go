package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stalledConsole is a stdout that takes nothing until release is closed: a
// classic Windows console with a selection in progress, or a pipe whose
// reader has stopped reading. entered gets one value per write that starts.
type stalledConsole struct {
	release chan struct{}
	entered chan struct{}

	mu  sync.Mutex
	got []string
}

func newStalledConsole() *stalledConsole {
	return &stalledConsole{release: make(chan struct{}), entered: make(chan struct{}, 1024)}
}

func (c *stalledConsole) Write(p []byte) (int, error) {
	c.entered <- struct{}{}
	<-c.release
	c.mu.Lock()
	c.got = append(c.got, string(p))
	c.mu.Unlock()
	return len(p), nil
}

func (c *stalledConsole) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.got...)
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestAStalledConsoleDoesNotBlockLogging is monitor-startup-4: stdout was
// written on the goroutine that logged, inside the lock that serialises log
// lines, so one write to a console that had stopped taking output blocked
// that goroutine and every goroutine that logged after it, and moombox.log
// got nothing more. Now the log calls return, the file and the ring have
// every line, and the console gets them all, in order, once it takes output
// again.
//
// Mutant: the stdout sink writing l.stdout directly again: the log calls
// never return and the test times out waiting for them.
func TestAStalledConsoleDoesNotBlockLogging(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "moombox.log")
	l, err := New(logPath, "INFO", 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	console := newStalledConsole()
	l.stdout.w = console

	const n = 50
	logged := make(chan struct{})
	go func() {
		defer close(logged)
		for i := range n {
			l.Info("while the console is stalled", "n", i)
		}
	}()
	waitClosed(t, logged, "the log calls to return while the console takes nothing")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	ring := strings.Join(l.GetRecentLines(), "\n") + "\n"
	for i := range n {
		want := fmt.Sprintf("n=%d\n", i)
		if !strings.Contains(string(data), want) {
			t.Errorf("moombox.log lacks line %d while the console is stalled", i)
		}
		if !strings.Contains(ring, want) {
			t.Errorf("the ring buffer lacks line %d while the console is stalled", i)
		}
	}
	if got := console.lines(); len(got) != 0 {
		t.Fatalf("the stalled console took %d lines", len(got))
	}

	close(console.release)
	l.Close() // flushes what is queued
	got := console.lines()
	if len(got) != n {
		t.Fatalf("the console got %d lines once it took output again, want %d", len(got), n)
	}
	for i, line := range got {
		if !strings.Contains(line, fmt.Sprintf("n=%d\n", i)) {
			t.Fatalf("console line %d is %q: the order changed", i, line)
		}
	}
}

// TestAFullConsoleQueueDropsConsoleLinesAndSaysSo pins the overflow: the
// queue is bounded, a line that does not fit is left off the console only,
// moombox.log and the ring say when that starts, and the first line that fits
// again is preceded on the console by one that says how many were left off,
// which the file and the ring record too.
//
// Mutants: the bound dropped from push (the console gets every line and no
// notice); the notice not queued (the console jumps from the queued lines to
// "after" with nothing between); consoleNote dropped (the file never says).
func TestAFullConsoleQueueDropsConsoleLinesAndSaysSo(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "moombox.log")
	l, err := New(logPath, "INFO", 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	console := newStalledConsole()
	l.stdout.w = console
	l.console.limit = 4

	// Line 0 is in the console's hands (stalled), lines 1-4 fill the queue,
	// lines 5-19 do not fit: 15 left off.
	l.Info("line", "n", 0)
	waitClosed(t, console.entered, "the console write of the first line to start")
	for i := 1; i < 20; i++ {
		l.Info("line", "n", i)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	const stalledNote = "The console is not taking log output"
	if c := strings.Count(string(data), stalledNote); c != 1 {
		t.Errorf("moombox.log says the console stalled %d times, want once:\n%s", c, data)
	}
	if !strings.Contains(string(data), "n=19\n") {
		t.Errorf("moombox.log lacks a line that was left off the console:\n%s", data)
	}
	if ring := strings.Join(l.GetRecentLines(), "\n"); !strings.Contains(ring, " WARN "+stalledNote) {
		t.Errorf("the ring buffer does not say the console stalled:\n%s", ring)
	}

	close(console.release)
	if !l.FlushConsole(30 * time.Second) {
		t.Fatal("the console queue did not drain once the console took output")
	}
	l.Info("after")
	const recovered = "15 log lines were not shown on it"
	data, _ = os.ReadFile(logPath)
	if !strings.Contains(string(data), recovered) {
		t.Errorf("moombox.log does not record how many lines the console lost:\n%s", data)
	}
	if ring := strings.Join(l.GetRecentLines(), "\n"); !strings.Contains(ring, recovered) {
		t.Errorf("the ring buffer does not record how many lines the console lost:\n%s", ring)
	}
	l.Close()

	got := console.lines()
	if len(got) != 7 {
		t.Fatalf("the console got %d lines, want 5 queued + the notice + 1:\n%s", len(got), strings.Join(got, ""))
	}
	for i := range 5 {
		if !strings.Contains(got[i], fmt.Sprintf("n=%d\n", i)) {
			t.Errorf("console line %d is %q", i, got[i])
		}
	}
	if !strings.Contains(got[5], "level=WARN") || !strings.Contains(got[5], recovered) {
		t.Errorf("the console line after the gap is %q, want the notice", got[5])
	}
	if !strings.Contains(got[6], "msg=after") {
		t.Errorf("the last console line is %q, want the line logged after the gap", got[6])
	}
}

// TestCloseDoesNotWaitForAConsoleThatTakesNothing: Close gives the queued
// console lines a bounded time to print and then goes on, so a stalled
// console cannot hang the shutdown, and the file is closed with every line.
//
// Mutant: an unbounded wait in consoleQueue.flush: Close never returns.
func TestCloseDoesNotWaitForAConsoleThatTakesNothing(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "moombox.log")
	l, err := New(logPath, "INFO", 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	console := newStalledConsole()
	t.Cleanup(func() { close(console.release) })
	l.stdout.w = console
	l.consoleCloseWait = 20 * time.Millisecond

	l.Info("the last line before the quit")
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		l.Close()
	}()
	waitClosed(t, closed, "Close to give up on the stalled console")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "the last line before the quit") {
		t.Errorf("moombox.log lacks the last line:\n%s", data)
	}
}

// TestSuppressStdoutPrintsWhatWasLoggedBeforeIt: the lines logged before the
// TUI takes the terminal are on it before SuppressStdout returns, and nothing
// logged after reaches it.
//
// Mutant: the flush dropped from SuppressStdout: it returns while the line is
// still being written.
func TestSuppressStdoutPrintsWhatWasLoggedBeforeIt(t *testing.T) {
	l, err := New(filepath.Join(t.TempDir(), "moombox.log"), "INFO", 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	console := newStalledConsole()
	l.stdout.w = console

	l.Info("before the TUI")
	waitClosed(t, console.entered, "the console write to start")
	suppressed := make(chan struct{})
	go func() {
		defer close(suppressed)
		l.SuppressStdout()
	}()
	select {
	case <-suppressed:
		t.Fatal("SuppressStdout returned while a line logged before it was still being written")
	case <-time.After(20 * time.Millisecond):
	}
	close(console.release)
	waitClosed(t, suppressed, "SuppressStdout to return")
	l.Info("while the TUI runs")
	l.FlushConsole(30 * time.Second)

	got := console.lines()
	if len(got) != 1 || !strings.Contains(got[0], "before the TUI") {
		t.Errorf("the console got %q, want only the line logged before the suppression", got)
	}
}
