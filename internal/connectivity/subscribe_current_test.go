package connectivity

import (
	"sync"
	"testing"
	"time"
)

// TestOnStateChangeWithCurrentReplaysAnOfflineBoot is K60: Start goes offline
// on its synchronous probe before any subscriber exists, and OnStateChange
// replays nothing, so a subscriber wired after Start never heard of an outage
// that began at boot — and its restore looked like a repeat "online".
//
// Mutant: register without delivering the current state — the first delivery
// is the restore's true, not false.
func TestOnStateChangeWithCurrentReplaysAnOfflineBoot(t *testing.T) {
	m := newTestMonitor(func() bool { return false })
	m.pollInterval = time.Hour
	m.Start(t.Context())
	defer m.Stop()
	if m.IsOnline() {
		t.Fatal("premise lost: Start did not seed offline")
	}

	var got []bool
	unsub := m.OnStateChangeWithCurrent(func(online bool) { got = append(got, online) })
	m.transition(true)
	unsub()
	m.transition(false)

	if len(got) != 2 || got[0] != false || got[1] != true {
		t.Errorf("deliveries = %v, want [false true]", got)
	}
}

// TestOnStateChangeWithCurrentNeverLosesOrDoublesATransition: a subscription
// racing transitions must hear a sequence that alternates (no state twice in
// a row, which a delivery of the current state plus the same transition's
// fan-out would be) and ends at IsOnline() (no transition lost between the
// read and the registration).
func TestOnStateChangeWithCurrentNeverLosesOrDoublesATransition(t *testing.T) {
	for range 200 {
		m := newTestMonitor(func() bool { return true })
		var mu sync.Mutex
		var got []bool
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := range 20 {
				m.transition(i%2 == 1)
			}
		}()
		go func() {
			defer wg.Done()
			m.OnStateChangeWithCurrent(func(online bool) {
				mu.Lock()
				got = append(got, online)
				mu.Unlock()
			})
		}()
		wg.Wait()
		mu.Lock()
		if len(got) == 0 {
			t.Fatal("no delivery at all")
		}
		for i := 1; i < len(got); i++ {
			if got[i] == got[i-1] {
				t.Fatalf("deliveries %v repeat a state at %d", got, i)
			}
		}
		if last := got[len(got)-1]; last != m.IsOnline() {
			t.Fatalf("subscriber last heard %v, IsOnline() = %v (%v)", last, m.IsOnline(), got)
		}
		mu.Unlock()
	}
}
