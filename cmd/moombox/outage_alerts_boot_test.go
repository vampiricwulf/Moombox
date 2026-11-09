package main

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/connectivity"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

type nopStoppable struct{}

func (nopStoppable) Stop() bool { return true }

// TestOutageAtBootIsReportedOnRestore is K60: connMon.Start goes offline on
// its synchronous probe inside initServices, before wireMonitorCallbacks
// subscribes the outage alerter, and OnStateChange replays nothing — so the
// alerter never saw the loss, read the restore as "the startup state", and an
// outage that began at boot was never reported. The alerter now subscribes
// with OnStateChangeWithCurrent and hears the offline it was born into.
//
// Driven through a real Monitor and its real probe: the target is a loopback
// port that is closed at Start (offline boot) and listened on afterwards (the
// poll's restore).
//
// Mutant: subscribe with OnStateChange — the restore arms no window and the
// test times out waiting for it.
func TestOutageAtBootIsReportedOnRestore(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	m := connectivity.NewMonitorWithInterval(nil, 100*time.Millisecond)
	m.SetProbeTargets([]string{addr})
	m.Start(t.Context())
	defer m.Stop()
	if m.IsOnline() {
		t.Fatal("premise lost: the monitor did not boot offline")
	}

	var mu sync.Mutex
	now := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	windows := make(chan func(), 4)
	rec := notificationtest.New()
	a := newOutageAlerts(rec, &nopLogger{}, func(d time.Duration, f func()) stoppableTimer {
		if d != outageCoalesceWindow {
			t.Errorf("window = %v, want %v", d, outageCoalesceWindow)
		}
		windows <- f
		return nopStoppable{}
	}, func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	defer subscribeOutageAlerts(m, a)()

	// Five minutes offline, then the network comes back.
	mu.Lock()
	now = now.Add(5 * time.Minute)
	mu.Unlock()
	ln2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("could not re-listen on %s: %v", addr, err)
	}
	defer ln2.Close()
	go func() {
		for {
			c, err := ln2.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	select {
	case fire := <-windows:
		fire() // the link stayed up for the coalescing window
	case <-time.After(15 * time.Second):
		t.Fatalf("the restore armed no Outage Alert window (online=%v)", m.IsOnline())
	}
	calls := rec.ByEvent("connectivity_restored")
	if len(calls) != 1 {
		t.Fatalf("%d Outage Alerts, want 1", len(calls))
	}
	if got, _ := calls[0].Field("Duration"); got != "5m0s" {
		t.Errorf("Duration = %q, want 5m0s", got)
	}
}
