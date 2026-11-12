package logger

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// consoleQueueLines is how many lines may wait for the console. At a typical
// 150 bytes a line that is well under a megabyte, and a console that drains
// at all never comes near it: it fills only when the console has stopped
// taking output.
const consoleQueueLines = 4096

// consoleCloseFlush bounds how long Close waits for the queued console lines
// to print, so the last lines before a quit are on the console when the
// process exits, and a console that has stopped taking output cannot hold the
// exit for longer than this.
const consoleCloseFlush = time.Second

// consoleSuppressFlush bounds the same wait in SuppressStdout: the lines
// logged before the TUI takes the terminal print before it does.
const consoleSuppressFlush = 500 * time.Millisecond

// consoleQueue takes the console write off the goroutines that log.
//
// The console (stdout) can stop taking output for as long as it likes: a
// click in a classic Windows console window starts a selection and every
// write to it blocks until a key is pressed, and a pipe whose reader has
// stopped reading blocks once it is full. That write used to run on the
// goroutine that logged, inside the lock that serialises log lines, so the
// first line after the stall blocked there and every download, monitor and
// web handler that logged next blocked behind it: recording stopped with no
// error and nothing more in moombox.log (monitor-startup-4).
//
// Now a log call only appends its line here, which never blocks, and one
// goroutine writes the lines to the console in the order they were appended.
// The goroutine runs only while there is something to write. When the queue
// is full the line is left off the console (never off moombox.log, the ring
// buffer or the subscribers, which are written by the caller as before), and
// the first line that fits again is preceded by one that says how many were
// left off.
type consoleQueue struct {
	out   io.Writer
	limit int

	mu       sync.Mutex
	lines    [][]byte
	draining bool
	// idle is closed when the running drain goroutine has written everything
	// and ended. Valid while draining is true.
	idle chan struct{}
	// dropped counts the lines left off since the queue filled; zero when no
	// overflow is in progress.
	dropped int
}

// push appends one line. stalled is true for the line that starts an
// overflow; recovered is the number of lines left off when this line ends
// one, in which case notice(recovered) is queued ahead of it.
func (q *consoleQueue) push(p []byte, notice func(dropped int) []byte) (stalled bool, recovered int) {
	q.mu.Lock()
	defer q.mu.Unlock()

	switch {
	case q.dropped > 0:
		// Room for the notice and the line, or the overflow goes on.
		if len(q.lines)+2 > q.limit {
			q.dropped++
			return false, 0
		}
		recovered = q.dropped
		q.dropped = 0
		q.lines = append(q.lines, notice(recovered))
	case len(q.lines) >= q.limit:
		q.dropped = 1
		return true, 0
	}
	// The caller's buffer is reused for its next line.
	q.lines = append(q.lines, bytes.Clone(p))
	if !q.draining {
		q.draining = true
		q.idle = make(chan struct{})
		go q.drain()
	}
	return false, recovered
}

// drain writes the queued lines, oldest first, and ends when none is left.
func (q *consoleQueue) drain() {
	defer func() {
		if r := recover(); r != nil {
			q.mu.Lock()
			q.lines = nil
			q.draining = false
			close(q.idle)
			q.mu.Unlock()
			fmt.Fprintf(os.Stderr, "logger: console writer panic: %v\n", r)
		}
	}()
	for {
		q.mu.Lock()
		if len(q.lines) == 0 {
			q.lines = nil
			q.draining = false
			close(q.idle)
			q.mu.Unlock()
			return
		}
		p := q.lines[0]
		q.lines[0] = nil
		q.lines = q.lines[1:]
		q.mu.Unlock()
		_, _ = q.out.Write(p)
	}
}

// flush waits until every line queued so far has been written, for at most
// timeout, and reports whether it was.
func (q *consoleQueue) flush(timeout time.Duration) bool {
	q.mu.Lock()
	if !q.draining {
		q.mu.Unlock()
		return true
	}
	idle := q.idle
	q.mu.Unlock()

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-idle:
		return true
	case <-t.C:
		return false
	}
}

// consoleSink is the stdout half of lineSinks: it queues the line for the
// console and returns at once.
type consoleSink struct {
	l *Logger
}

func (s consoleSink) Write(p []byte) (int, error) {
	l := s.l
	// Suppressed (the TUI owns the terminal, or it hung up): nothing is
	// queued. The gate is checked again when the line is written, for the
	// lines still queued when stdout was suppressed.
	if !l.stdout.enabled.Load() {
		return len(p), nil
	}
	stalled, recovered := l.console.push(p, func(dropped int) []byte {
		return l.fileShapeLine(consoleRecoveredMsg(dropped))
	})
	switch {
	case stalled:
		l.consoleNote("The console is not taking log output, so lines are being left off it until it does. moombox.log and the dashboard have every line.")
	case recovered > 0:
		l.consoleNote(consoleRecoveredMsg(recovered))
	}
	return len(p), nil
}

func consoleRecoveredMsg(dropped int) string {
	return fmt.Sprintf("The console is taking log output again. %d log lines were not shown on it while it was not; moombox.log has them.", dropped)
}
