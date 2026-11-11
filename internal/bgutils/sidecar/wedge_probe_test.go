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
	// blockPings, when non-nil, holds every ping's Write until it is closed:
	// a pipe the child has stopped draining.
	blockPings chan struct{}

	mu      sync.Mutex
	methods []string
}

func (w *fakeChildStdin) Write(p []byte) (int, error) {
	var req rpcRequest
	if err := json.Unmarshal(p, &req); err != nil {
		return 0, err
	}
	if w.blockPings != nil && req.Method == "ping" {
		<-w.blockPings
		return len(p), nil
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

// waitPingSent waits until the probe's ping has been written to the child.
func waitPingSent(t *testing.T, w *fakeChildStdin) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if w.count("ping") == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the wedge probe never sent its ping")
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
// Mutant this kills: the probe not comparing childGen at its verdict, after
// the ping (the check before the ping has passed by the time of the restart).
func TestALateProbeDoesNotCondemnAReplacementChild(t *testing.T) {
	s, w, reasons := newProbeSidecar(50*time.Millisecond, 300*time.Millisecond, false)

	if err := s.call(context.Background(), "generatePoToken", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call = %v, want context.DeadlineExceeded", err)
	}
	waitPingSent(t, w) // the probe is past its first check and waiting for the answer
	s.childGen.Add(1)  // what startLocked does for a replacement child
	waitProbeDone(t, s)
	select {
	case reason := <-reasons:
		t.Fatalf("a probe armed against the old child condemned its replacement: %q", reason)
	default:
	}
	if !s.IsHealthy() {
		t.Error("IsHealthy() went false for the replacement child")
	}
	if n := w.count("ping"); n != 1 {
		t.Errorf("the probe sent %d pings, want 1", n)
	}
}

// TestAProbeForAChildAlreadyReplacedSendsNoPing: a restart that lands before
// the probe goroutine has started leaves it nothing to ask.
//
// Mutant this kills: the probe not comparing childGen before its ping.
func TestAProbeForAChildAlreadyReplacedSendsNoPing(t *testing.T) {
	s, w, reasons := newProbeSidecar(time.Minute, 50*time.Millisecond, false)

	gen := s.childGen.Load()
	s.childGen.Add(1) // a replacement child has been spawned
	s.probeWedge(gen)
	waitProbeDone(t, s)
	if n := w.count("ping"); n != 0 {
		t.Errorf("a probe armed against a replaced child sent %d pings, want 0", n)
	}
	select {
	case reason := <-reasons:
		t.Fatalf("a probe armed against a replaced child marked the sidecar unhealthy: %q", reason)
	default:
	}
}

// TestAProbeDuringTeardownIsNoVerdict: a child being stopped answers nothing
// either, and Stop is not a wedge.
//
// Mutant this kills: the probe not checking stopping at its verdict, after
// the ping.
func TestAProbeDuringTeardownIsNoVerdict(t *testing.T) {
	s, w, reasons := newProbeSidecar(50*time.Millisecond, 300*time.Millisecond, false)

	if err := s.call(context.Background(), "generatePoToken", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call = %v, want context.DeadlineExceeded", err)
	}
	waitPingSent(t, w)
	s.stopping.Store(true)
	waitProbeDone(t, s)
	select {
	case reason := <-reasons:
		t.Fatalf("a probe that ended during teardown marked the sidecar unhealthy: %q", reason)
	default:
	}
	if !s.healthy.Load() {
		t.Error("the healthy flag went false on a probe that ended during teardown")
	}
	if n := w.count("ping"); n != 1 {
		t.Errorf("the probe sent %d pings, want 1", n)
	}
}

// TestATimeoutWaitingForTheWriteSlotStartsNoProbe: a request that never
// reached the child's stdin has asked the child nothing, so its timeout is no
// reason to ping. An undrained stdin is the stall watchdog's case.
//
// Mutant this kills: call() probing on a timeout whose request was not
// written.
func TestATimeoutWaitingForTheWriteSlotStartsNoProbe(t *testing.T) {
	s, w, reasons := newProbeSidecar(50*time.Millisecond, time.Minute, false)

	s.writeSem <- struct{}{} // another request's write holds the slot
	if err := s.call(context.Background(), "generatePoToken", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call that never got the write slot = %v, want context.DeadlineExceeded", err)
	}
	if s.wedgeProbing.Load() {
		t.Error("a request that was never written started a wedge probe")
	}
	<-s.writeSem
	if n := w.count("ping") + w.count("generatePoToken"); n != 0 {
		t.Errorf("%d requests reached the child, want 0", n)
	}
	select {
	case reason := <-reasons:
		t.Fatalf("a timeout on the write slot marked the sidecar unhealthy: %q", reason)
	default:
	}
}

// TestAProbePingThatNeverGetsTheWriteSlotIsNoVerdict: the probe's ping can
// spend its whole deadline behind another request's write. Nothing was asked
// of the child, so nothing is concluded; the stall watchdog of the write that
// holds the slot owns that case.
//
// Mutant this kills: the probe ignoring roundTrip's written result.
func TestAProbePingThatNeverGetsTheWriteSlotIsNoVerdict(t *testing.T) {
	s, w, reasons := newProbeSidecar(time.Minute, 50*time.Millisecond, false)

	s.writeSem <- struct{}{} // held for the whole probe deadline
	s.probeWedge(s.childGen.Load())
	waitProbeDone(t, s)
	<-s.writeSem
	select {
	case reason := <-reasons:
		t.Fatalf("a ping that never got the write slot condemned the child: %q", reason)
	default:
	}
	if !s.IsHealthy() {
		t.Error("IsHealthy() went false though no ping reached the child")
	}
	if n := w.count("ping"); n != 0 {
		t.Errorf("%d pings reached the child, want 0", n)
	}
}

// TestAProbePingWhoseWriteStallsIsNoVerdict: the same for a ping whose own
// write does not return within the probe deadline. The stall watchdog armed
// for that write condemns the child at RequestTimeout, with its own reason.
//
// Mutant this kills: the probe ignoring roundTrip's written result.
func TestAProbePingWhoseWriteStallsIsNoVerdict(t *testing.T) {
	s, w, reasons := newProbeSidecar(time.Minute, 50*time.Millisecond, false)
	w.blockPings = make(chan struct{})

	s.probeWedge(s.childGen.Load())
	waitProbeDone(t, s)
	select {
	case reason := <-reasons:
		t.Fatalf("a ping whose write stalled condemned the child after the probe deadline: %q", reason)
	default:
	}
	if !s.IsHealthy() {
		t.Error("IsHealthy() went false though the ping's write had not returned")
	}
	close(w.blockPings)
	s.writeSem <- struct{}{} // the stranded write has returned and freed the slot
	<-s.writeSem
}

// TestAVerdictForAnEarlierChildDoesNotLand: the generation check and the flip
// are one step, so a verdict armed against a child that has been replaced, or
// one that a teardown is taking down, leaves the flag alone.
//
// Mutants this kills: markUnhealthyGen not comparing the generation; not
// reading stopping.
func TestAVerdictForAnEarlierChildDoesNotLand(t *testing.T) {
	s, _, reasons := newProbeSidecar(time.Minute, time.Minute, false)
	gen := s.childGen.Load()

	s.childGen.Add(1)
	s.markUnhealthyGen(gen, "old child's wedge")
	if !s.IsHealthy() {
		t.Fatal("a verdict about the previous child marked its replacement unhealthy")
	}
	s.stopping.Store(true)
	s.markUnhealthyGen(gen+1, "wedge during teardown")
	if !s.healthy.Load() {
		t.Fatal("a verdict during teardown flipped the healthy flag")
	}
	s.stopping.Store(false)
	select {
	case reason := <-reasons:
		t.Fatalf("OnUnhealthy fired for a verdict that should not land: %q", reason)
	default:
	}

	s.markUnhealthyGen(gen+1, "this child's wedge")
	if s.IsHealthy() {
		t.Fatal("a verdict about the running child did not mark it unhealthy")
	}
	select {
	case reason := <-reasons:
		if reason != "this child's wedge" {
			t.Errorf("OnUnhealthy reason = %q", reason)
		}
	default:
		t.Fatal("OnUnhealthy did not fire for the running child's verdict")
	}
}
