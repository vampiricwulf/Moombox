package bgutils

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dop251/goja"

	gojahelpers "github.com/vampiricwulf/Moombox/internal/goja"
)

// newSnapshotClient builds a BotGuardClient around a goja runtime with the
// package's shims and a snapshot function written in JS, so Snapshot's wait
// can be exercised without BotGuard's interpreter or the network. The JS is
// an expression evaluating to a function of (callback, args), the shape
// BotGuard's asyncSnapshotFunction has.
func newSnapshotClient(t *testing.T, snapshotJS string) *BotGuardClient {
	t.Helper()
	vm, tm, err := gojahelpers.NewRuntimeWithShims(context.Background(), UserAgentFull)
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	t.Cleanup(tm.CancelAll)
	v, err := vm.RunString(snapshotJS)
	if err != nil {
		t.Fatalf("snapshot JS: %v", err)
	}
	fn, ok := goja.AssertFunction(v)
	if !ok {
		t.Fatalf("snapshot JS did not evaluate to a function: %v", v)
	}
	return &BotGuardClient{vm: vm, timerMgr: tm, asyncSnapshot: fn}
}

// TestSnapshotDrainsADeferredResult: BotGuard may hand the result to the
// callback from a setTimeout, and goja has no event loop to run it. Snapshot
// drained the timer queue exactly once, right after the call — before a timer
// deferred by even a millisecond had fired — and then blocked on the result
// channel, so a deferred result sat in the queue for the whole 30 s budget and
// came back as a timeout.
//
// Mutants this kills:
//   - the single drain + blocking select restored → Snapshot returns the
//     timeout error after the full budget instead of the result
func TestSnapshotDrainsADeferredResult(t *testing.T) {
	c := newSnapshotClient(t, `(function (cb, args) { setTimeout(function () { cb("deferred"); }, 20); })`)

	started := time.Now()
	result, signals, err := c.Snapshot(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if result != "deferred" {
		t.Errorf("result = %q, want the value the deferred callback delivered", result)
	}
	if signals == nil {
		t.Error("webPoSignalOutput is nil")
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("Snapshot took %s to pick up a result deferred by 20 ms", took)
	}
}

// TestSnapshotTimesOutWhenTheResultNeverComes: the budget still bounds the
// wait when nothing ever calls back, and the error still carries ErrTimeout.
func TestSnapshotTimesOutWhenTheResultNeverComes(t *testing.T) {
	c := newSnapshotClient(t, `(function (cb, args) {})`)

	_, _, err := c.Snapshot(context.Background(), 100*time.Millisecond)
	var bgErr *BGError
	if !errors.As(err, &bgErr) || bgErr.Code != ErrTimeout {
		t.Fatalf("Snapshot = %v, want a BGError with Code %q", err, ErrTimeout)
	}
}

// TestSnapshotReturnsOnContextCancel: ctx.Done() ends the wait the same way.
func TestSnapshotReturnsOnContextCancel(t *testing.T) {
	c := newSnapshotClient(t, `(function (cb, args) {})`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := c.Snapshot(ctx, 5*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Snapshot = %v, want context.Canceled", err)
	}
}
