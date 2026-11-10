package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeChildStdin stands in for the stdin of a child whose pipe drains fine.
// It records every request line. With answerPings set it plays a live event
// loop: a ping is answered at once while every other request is left
// hanging (a mint stuck on the network). Without it nothing is ever
// answered: an event loop stuck in synchronous code.
type fakeChildStdin struct {
	s           *Sidecar
	answerPings bool

	mu      sync.Mutex
	methods []string
}

func (w *fakeChildStdin) Write(p []byte) (int, error) {
	var req rpcRequest
	if err := json.Unmarshal(p, &req); err != nil {
		return 0, err
	}
	w.mu.Lock()
	w.methods = append(w.methods, req.Method)
	w.mu.Unlock()
	if w.answerPings && req.Method == "ping" {
		w.s.pendingMu.Lock()
		ch := w.s.pending[req.ID]
		w.s.pendingMu.Unlock()
		if ch != nil {
			ch <- rpcResponse{ID: req.ID, Result: json.RawMessage(`"pong"`)}
		}
	}
	return len(p), nil
}

func (w *fakeChildStdin) Close() error { return nil }

func (w *fakeChildStdin) count(method string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, m := range w.methods {
		if m == method {
			n++
		}
	}
	return n
}

// newProbeSidecar builds a Sidecar over a fakeChildStdin, with both timeouts
// shrunk so no test waits out the production 90 s + 30 s.
func newProbeSidecar(requestTimeout, probeTimeout time.Duration, answerPings bool) (*Sidecar, *fakeChildStdin, chan string) {
	reasons := make(chan string, 4)
	s := New(Config{
		Logger:         silentLogger{},
		RequestTimeout: requestTimeout,
		OnUnhealthy:    func(reason string) { reasons <- reason },
	})
	s.wedgeProbeTimeout = probeTimeout
	w := &fakeChildStdin{s: s, answerPings: answerPings}
	s.stdin = w
	s.healthy.Store(true)
	return s, w, reasons
}

// waitProbeDone waits for the probe a timed-out call started to finish. call()
// sets wedgeProbing before it returns, so there is no window to miss.
func waitProbeDone(t *testing.T, s *Sidecar) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !s.wedgeProbing.Load() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the wedge probe never ran to its end")
}

// TestTheWedgeProbeDeadlineIsThirtySeconds pins the production figure: long
// enough that a synchronous ejs preprocess on a slow board is not read as a
// wedge (owner ruling, youtube-5).
func TestTheWedgeProbeDeadlineIsThirtySeconds(t *testing.T) {
	if got := New(Config{Logger: silentLogger{}}).wedgeProbeTimeout; got != 30*time.Second {
		t.Fatalf("New set the wedge probe deadline to %s, want 30s", got)
	}
}

// TestAnUnansweredProbeMarksTheSidecarUnhealthy: a child stuck in synchronous
// JS keeps stdout open and (with small requests) its stdin pipe unfilled, so
// neither readPump nor the stall watchdog sees it. A request that times out
// on RequestTimeout is followed by a ping, and a ping nobody answers is the
// wedge.
//
// Mutants this kills:
//   - call() not probing after its own timeout → OnUnhealthy never fires
//   - the probe not marking unhealthy          → same
func TestAnUnansweredProbeMarksTheSidecarUnhealthy(t *testing.T) {
	s, w, reasons := newProbeSidecar(50*time.Millisecond, 50*time.Millisecond, false)

	if err := s.call(context.Background(), "generatePoToken", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call to a child that answers nothing = %v, want context.DeadlineExceeded", err)
	}
	select {
	case reason := <-reasons:
		if reason != "event loop unresponsive: no ping answer in 50ms" {
			t.Errorf("OnUnhealthy reason = %q, want the wedge named with the probe's deadline", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an unanswered wedge probe never marked the sidecar unhealthy")
	}
	if s.IsHealthy() {
		t.Error("IsHealthy() is true after an unanswered wedge probe")
	}
	if n := w.count("ping"); n != 1 {
		t.Errorf("the probe sent %d pings, want 1", n)
	}
}

// TestAnAnsweredProbeLeavesTheSidecarHealthy: a request can time out with the
// loop alive (a mint stuck on the network). The ping is answered, and the
// child keeps its minter and its preprocessed players.
//
// Mutant this kills: marking unhealthy on the RPC timeout itself.
func TestAnAnsweredProbeLeavesTheSidecarHealthy(t *testing.T) {
	s, w, reasons := newProbeSidecar(50*time.Millisecond, 2*time.Second, true)

	if err := s.call(context.Background(), "generatePoToken", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call = %v, want context.DeadlineExceeded", err)
	}
	waitProbeDone(t, s)
	select {
	case reason := <-reasons:
		t.Fatalf("a child that answered its ping was marked unhealthy: %q", reason)
	default:
	}
	if !s.IsHealthy() {
		t.Error("IsHealthy() went false though the probe's ping was answered")
	}
	if n := w.count("ping"); n != 1 {
		t.Errorf("the probe sent %d pings, want 1", n)
	}
}

// TestACallersShorterBudgetStartsNoProbe: a caller that brings a budget
// shorter than RequestTimeout (the 10-45 s refresh mint) and runs out of it
// has shown nothing about the child. No ping is sent.
//
// Mutant this kills: probing on any context error rather than on call()'s
// own RequestTimeout.
func TestACallersShorterBudgetStartsNoProbe(t *testing.T) {
	s, w, reasons := newProbeSidecar(time.Minute, 20*time.Millisecond, false)

	for _, name := range []string{"deadline", "cancel"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		if name == "cancel" {
			ctx, cancel = context.WithCancel(context.Background())
			time.AfterFunc(30*time.Millisecond, cancel)
		}
		err := s.call(ctx, "generatePoToken", nil, nil)
		cancel()
		if err == nil {
			t.Fatalf("%s: call to a child that answers nothing returned nil", name)
		}
	}
	time.Sleep(200 * time.Millisecond) // ten probe deadlines
	if n := w.count("ping"); n != 0 {
		t.Errorf("a caller's own budget running out sent %d pings, want 0", n)
	}
	select {
	case reason := <-reasons:
		t.Fatalf("a caller's own budget running out marked the sidecar unhealthy: %q", reason)
	default:
	}
	if !s.IsHealthy() {
		t.Error("IsHealthy() went false on a caller's own budget")
	}
}

// TestOnlyOneProbeIsInFlight: several calls timing out together are one
// wedge, and one ping.
//
// Mutant this kills: no in-flight guard → one ping per timed-out call.
func TestOnlyOneProbeIsInFlight(t *testing.T) {
	s, w, reasons := newProbeSidecar(50*time.Millisecond, 500*time.Millisecond, false)

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.call(context.Background(), "generatePoToken", nil, nil)
		}()
	}
	wg.Wait()
	select {
	case <-reasons:
	case <-time.After(5 * time.Second):
		t.Fatal("the wedge probe never marked the sidecar unhealthy")
	}
	if n := w.count("ping"); n != 1 {
		t.Errorf("five calls timing out together sent %d pings, want 1", n)
	}
}

// TestALateProbeDoesNotCondemnAReplacementChild: the probe is armed against
// the child its request was sent to. A restart in the middle of the probe
// leaves its verdict about a child that is already gone.
//
// Mutant this kills: the probe not comparing childGen before its verdict.
func TestALateProbeDoesNotCondemnAReplacementChild(t *testing.T) {
	s, _, reasons := newProbeSidecar(50*time.Millisecond, 200*time.Millisecond, false)

	if err := s.call(context.Background(), "generatePoToken", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call = %v, want context.DeadlineExceeded", err)
	}
	s.childGen.Add(1) // what startLocked does for a replacement child
	waitProbeDone(t, s)
	select {
	case reason := <-reasons:
		t.Fatalf("a probe armed against the old child condemned its replacement: %q", reason)
	default:
	}
	if !s.IsHealthy() {
		t.Error("IsHealthy() went false for the replacement child")
	}
}

// TestAProbeDuringTeardownIsNoVerdict: a child being stopped answers nothing
// either, and Stop is not a wedge.
//
// Mutant this kills: the probe not checking stopping before its verdict.
func TestAProbeDuringTeardownIsNoVerdict(t *testing.T) {
	s, _, reasons := newProbeSidecar(50*time.Millisecond, 200*time.Millisecond, false)

	if err := s.call(context.Background(), "generatePoToken", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call = %v, want context.DeadlineExceeded", err)
	}
	s.stopping.Store(true)
	waitProbeDone(t, s)
	select {
	case reason := <-reasons:
		t.Fatalf("a probe that ended during teardown marked the sidecar unhealthy: %q", reason)
	default:
	}
}
