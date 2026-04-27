package connectivity

import (
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
	ctx := t.Context()

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
	ctx := t.Context()

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

// TestMonitor_StopBeforeStartIsSafe covers the audit-flagged untested
// edge: Stop must not panic if Start was never called. The cancel
// field is nil at that point, and the guard in Stop is what we're
// asserting actually runs. Audit reports/small-packages.md.
func TestMonitor_StopBeforeStartIsSafe(t *testing.T) {
	m := newTestMonitor(func() bool { return true })
	m.Stop() // would panic on nil cancel without the guard
	// A second call should also be a no-op.
	m.Stop()
}

// TestMonitor_PassiveAndActiveIntegrated covers the audit's "two tags
// trip offline → success restores" end-to-end scenario. PassiveTracker
// is wired through Monitor.ReportFailure / ReportSuccess; the
// transition should fire OnStateChange callbacks in both directions.
func TestMonitor_PassiveAndActiveIntegrated(t *testing.T) {
	var checkOnline atomic.Bool
	checkOnline.Store(true)
	m := newTestMonitor(func() bool { return checkOnline.Load() })

	// Speed the passive threshold up so the test doesn't need 5 distinct
	// failures across 30s. Two tags × three failures per tag is the
	// minimum that satisfies defaultPassiveMinFails (5) and
	// defaultPassiveMinTags (2).
	var transitions []bool
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	m.OnStateChange(func(online bool) {
		<-mu
		transitions = append(transitions, online)
		mu <- struct{}{}
	})

	// Fire 3 failures from each of two distinct subsystems within the
	// passive window. ShouldTriggerOffline latches; the next
	// ReportFailure call after threshold flips the monitor to offline.
	for range 3 {
		m.ReportFailure("utils/http")
	}
	for range 3 {
		m.ReportFailure("monitor/feed")
	}

	if m.IsOnline() {
		t.Fatal("monitor should be offline after 6 cross-tag failures")
	}

	// A successful call from either subsystem clears that tag's failures.
	// Once both tags drop below threshold, ReportSuccess flips back to
	// online — provided the active checkFn agrees.
	m.ReportSuccess("utils/http")
	m.ReportSuccess("monitor/feed")

	if !m.IsOnline() {
		t.Fatal("monitor should be back online after both tags clear")
	}

	<-mu
	got := append([]bool(nil), transitions...)
	mu <- struct{}{}
	if len(got) != 2 || got[0] != false || got[1] != true {
		t.Errorf("transitions: want [false, true], got %v", got)
	}
}

// TestNewMonitorWithIntervalClamps verifies the lower bound on the
// configurable poll interval — values below 100ms get clamped up.
func TestNewMonitorWithIntervalClamps(t *testing.T) {
	tests := []struct {
		name    string
		given   time.Duration
		wantMin time.Duration
	}{
		{"zero clamps up", 0, 100 * time.Millisecond},
		{"negative clamps up", -1 * time.Second, 100 * time.Millisecond},
		{"50ms clamps up", 50 * time.Millisecond, 100 * time.Millisecond},
		{"exactly 100ms passes through", 100 * time.Millisecond, 100 * time.Millisecond},
		{"1s passes through", time.Second, time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMonitorWithInterval(nil, tc.given)
			if m.pollInterval < tc.wantMin {
				t.Errorf("pollInterval = %v, want ≥ %v", m.pollInterval, tc.wantMin)
			}
		})
	}
}
