package sidecar

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// discardStdin is a stdin stand-in that accepts every line: the test below
// needs a request registered as pending, not delivered anywhere.
type discardStdin struct{ io.Writer }

func (discardStdin) Close() error { return nil }

// TestReadPumpNamesAScannerErrorAsTheReason: readPump reported every exit as
// "stdout EOF" — the reason the dashboard, the Discord "BotGuard Sidecar
// Down" embed and the supervisor log all show — even when Scan had stopped
// on an error of its own, a line past the 1 MiB cap being the realistic one,
// with the child still alive behind it. The pending callers are drained with
// the same reason, so they see it too.
//
// Mutants this kills:
//   - markUnhealthy("stdout EOF") unconditionally → the cap case lacks its cause
//   - the real-EOF reason reworded                → the EOF case fails
func TestReadPumpNamesAScannerErrorAsTheReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout []byte
		want   string
	}{
		{"a real EOF", nil, "stdout EOF"},
		{"a line past the cap", bytes.Repeat([]byte("x"), 2<<20), "stdout read: " + bufio.ErrTooLong.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reasons := make(chan string, 1)
			s := New(Config{Logger: silentLogger{}, OnUnhealthy: func(r string) { reasons <- r }})
			s.stdin = discardStdin{io.Discard}
			s.stdout = io.NopCloser(bytes.NewReader(tc.stdout))
			s.healthy.Store(true)
			// Never Started, so readyCh is nil; pre-fire readyOnce as
			// TestGeneratePlayerPoTokenParams does, or the EOF path closes it.
			s.readyOnce.Do(func() {})

			// A request in flight when the pump dies: its drained error must
			// carry the same reason.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			callErr := make(chan error, 1)
			go func() { callErr <- s.call(ctx, "ping", nil, nil) }()
			for pending := 0; pending == 0; {
				s.pendingMu.Lock()
				pending = len(s.pending)
				s.pendingMu.Unlock()
				if pending == 0 {
					time.Sleep(time.Millisecond)
				}
			}

			s.pumpsDone.Add(1)
			go s.readPump()
			select {
			case got := <-reasons:
				if got != tc.want {
					t.Errorf("OnUnhealthy reason = %q, want %q", got, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("readPump never marked the sidecar unhealthy")
			}
			s.pumpsDone.Wait()

			select {
			case err := <-callErr:
				if err == nil || !strings.HasSuffix(err.Error(), tc.want) {
					t.Errorf("drained call error = %v, want it to end with %q", err, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the pending call was never drained")
			}
		})
	}
}
