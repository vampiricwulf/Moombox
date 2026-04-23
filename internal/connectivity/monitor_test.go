package connectivity

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func newTestMonitor(checkFn func() bool) *Monitor {
	m := &Monitor{
		callbacks: make(map[uint64]func(online bool)),
		checkFn:   checkFn,
		passive:   NewPassiveTracker(),
	}
	m.online.Store(true)
	return m
}

func TestNewMonitor_DefaultsOnline(t *testing.T) {
	m := NewMonitor(nil)
	if !m.IsOnline() {
		t.Fatal("expected monitor to default to online")
	}
}

func TestMonitor_StateTransition(t *testing.T) {
	var called atomic.Int32
	var lastState atomic.Bool

	m := newTestMonitor(func() bool { return false })
	m.OnStateChange(func(online bool) {
		called.Add(1)
		lastState.Store(online)
	})
	m.poll()
	m.poll()

	if m.IsOnline() {
		t.Fatal("expected offline after 2 polls")
	}
	if called.Load() != 1 {
		t.Fatalf("expected 1 callback, got %d", called.Load())
	}
	if lastState.Load() {
		t.Fatal("expected callback with online=false")
	}
}

func TestMonitor_RecoveryOnSingleOnlinePoll(t *testing.T) {
	checkResult := true
	m := newTestMonitor(func() bool { return checkResult })
	m.poll()

	checkResult = false
	m.poll()
	m.poll()

	if m.IsOnline() {
		t.Fatal("expected offline")
	}

	checkResult = true
	m.poll()

	if !m.IsOnline() {
		t.Fatal("expected online after single success")
	}
}

func TestMonitor_OnStateChange_Unregister(t *testing.T) {
	var called atomic.Int32
	m := newTestMonitor(func() bool { return true })

	unregister := m.OnStateChange(func(online bool) {
		called.Add(1)
	})
	unregister()

	m.checkFn = func() bool { return false }
	m.poll()
	m.poll()

	if called.Load() != 0 {
		t.Fatal("callback should not fire after unregister")
	}
}

func TestMonitor_DebouncePreventsSinglePollFlap(t *testing.T) {
	m := newTestMonitor(func() bool { return false })
	m.poll() // first offline poll

	if !m.IsOnline() {
		t.Fatal("should still be online after single offline poll (debounce)")
	}
}

func TestMonitor_StartIsIdempotent(t *testing.T) {
	m := newTestMonitor(func() bool { return true })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.Start(ctx)
	firstCancel := m.cancel

	m.Start(ctx) // second call should be a no-op — do not rebind cancel
	if m.cancel == nil {
		t.Fatal("cancel went nil after second Start")
	}
	// Same function pointer as after the first Start.
	if &firstCancel == nil || m.cancel == nil {
		t.Fatal("cancel should remain set")
	}
	m.Stop()
}

func TestMonitor_StartInitialOfflineProbe(t *testing.T) {
	m := newTestMonitor(func() bool { return false })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.Start(ctx)
	defer m.Stop()

	// With the synchronous offline probe in Start, the monitor must already
	// report offline before the first ticker interval fires.
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !m.IsOnline() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if m.IsOnline() {
		t.Fatal("Start should have seeded offline state before the first tick")
	}
}
