package connectivity

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMonitor_RealProbeReflectsReachability wires the production reachability
// probe (the default checkFn installed by NewMonitor) through a Monitor and
// asserts it reflects ACTUAL TCP reachability: a live local listener reads as
// online, a closed port as offline. This guards the platform-shared probe that
// replaced the old Windows InternetGetConnectedState heuristic — the root cause
// of the monitor reporting "online" during a real outage.
func TestMonitor_RealProbeReflectsReachability(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	m := NewMonitor(nil) // default checkFn = reachabilityProbe(m.probeTargets)
	m.SetProbeTargets([]string{ln.Addr().String()})
	if !m.checkFn() {
		t.Fatal("expected online: real probe to a live listener should succeed")
	}
	m.SetProbeTargets([]string{"127.0.0.1:1"}) // reserved/closed port
	if m.checkFn() {
		t.Fatal("expected offline: real probe to a closed port should fail")
	}
}

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
	// Note: Go function values aren't comparable beyond nil-checks, so we
	// can't directly assert cancel is the same closure as firstCancel.
	// The non-nil check above is the strongest invariant we can express.
	_ = firstCancel
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

// TestMonitor_RecoversAfterPassiveLatchWhenSubsystemsGoQuiet locks in the
// offline-recovery fix. It reproduces the production deadlock: during an
// outage the passive tracker latches offline, then every subsystem gates off
// its network I/O — so no ReportSuccess / ReportFailure ever fires again and
// poll() is the only live path. Pre-fix, poll() read the non-pruning
// IsTriggered() and stayed offline forever even after the active probe
// recovered. Post-fix, poll() uses IsTriggeredPruned(), so the aged-out latch
// clears and the monitor returns online once the active probe agrees.
func TestMonitor_RecoversAfterPassiveLatchWhenSubsystemsGoQuiet(t *testing.T) {
	var checkOnline atomic.Bool
	checkOnline.Store(true)
	m := newTestMonitor(func() bool { return checkOnline.Load() })
	// Tight passive window so failures age out within the test rather than 30s.
	m.passive = &PassiveTracker{window: 40 * time.Millisecond, minFails: 5, minTags: 2}

	// Outage begins: the active probe drops AND subsystems pile up cross-tag
	// failures, latching the passive tracker offline. ReportFailure trips the
	// transition once the threshold is met.
	checkOnline.Store(false)
	for range 3 {
		m.ReportFailure("monitor/feed")
	}
	for range 3 {
		m.ReportFailure("engine/fetch")
	}
	if m.IsOnline() {
		t.Fatal("expected offline after outage + cross-tag passive latch")
	}
	if !m.passive.IsTriggered() {
		t.Fatal("expected passive latch to be set during the outage")
	}

	// Subsystems gate off — NO further ReportSuccess / ReportFailure fires.
	// Wait for the failure window to age out so a pruning poll can clear it.
	time.Sleep(70 * time.Millisecond)

	// Internet returns; the active probe recovers. The next poll must restore
	// online purely via the pruning read of the passive latch — there is no
	// success report to lean on.
	checkOnline.Store(true)
	m.poll()

	if !m.IsOnline() {
		t.Fatal("monitor stuck offline after recovery — poll() never pruned the stale passive latch")
	}
	if m.passive.IsTriggered() {
		t.Fatal("passive latch should have been cleared by the pruning poll")
	}
}

// TestMonitor_StartProbesOnceWhenOfflineAtBoot pins T3-30: booting offline
// must cost ONE probe, not two.
//
// Start seeded state with a synchronous checkFn and then called poll(), which
// runs checkFn again — so a machine that boots with no network paid two full
// probe timeouts (~6 s in production) before Start returned, to learn what the
// first probe already said.
//
// Mutant: restoring m.poll() makes the probe count 2 and roughly doubles the
// measured Start latency.
func TestMonitor_StartProbesOnceWhenOfflineAtBoot(t *testing.T) {
	const probeCost = 150 * time.Millisecond

	var probes atomic.Int32
	m := newTestMonitor(func() bool {
		probes.Add(1)
		time.Sleep(probeCost) // a probe timeout, in miniature
		return false
	})
	m.pollInterval = time.Hour // the ticker must not fire during this test

	var mu sync.Mutex
	var states []bool
	m.OnStateChange(func(online bool) {
		mu.Lock()
		states = append(states, online)
		mu.Unlock()
	})

	start := time.Now()
	m.Start(t.Context())
	elapsed := time.Since(start)
	t.Cleanup(m.Stop)

	if got := probes.Load(); got != 1 {
		t.Fatalf("boot probes = %d, want 1 — the seed check must not be followed by a second synchronous poll()", got)
	}
	// One probe is ~150ms; two are >=300ms. 250ms leaves ~85ms of scheduler
	// slack while still failing the two-probe mutant.
	if elapsed >= 250*time.Millisecond {
		t.Errorf("Start blocked %v, want ~one probe (%v) — a second synchronous probe doubles the boot stall", elapsed, probeCost)
	}
	if m.IsOnline() {
		t.Error("a monitor that booted offline must report offline")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) != 1 || states[0] {
		t.Errorf("state changes = %v, want exactly one false — the seed must still announce the outage", states)
	}
}
