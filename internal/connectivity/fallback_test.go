package connectivity

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The package's tests dial loopback ports only. With the production fallback
// hosts in place, a test whose loopback target is closed would dial YouTube
// and Twitch and read "online" on any machine with a network.
func init() { probeFallbackTargets = nil }

// stubProbeDial replaces the dial with fn and records every address dialled.
func stubProbeDial(t *testing.T, fn func(ctx context.Context, addr string) error) func() []string {
	t.Helper()
	var mu sync.Mutex
	var dialled []string
	orig := probeDial
	t.Cleanup(func() { probeDial = orig })
	probeDial = func(ctx context.Context, _ *net.Dialer, addr string) (net.Conn, error) {
		mu.Lock()
		dialled = append(dialled, addr)
		mu.Unlock()
		if err := fn(ctx, addr); err != nil {
			return nil, err
		}
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), dialled...)
	}
}

func setFallbackDelay(t *testing.T, d time.Duration) {
	t.Helper()
	orig := probeFallbackDelay
	t.Cleanup(func() { probeFallbackDelay = orig })
	probeFallbackDelay = d
}

// A network that blackholes the configured targets but reaches a platform
// host is online, and the answer comes inside the ONE race window: the
// fallback hosts join the race, they are not raced after it.
// Mutant: never start the fallback dials — the probe reads offline.
func TestReachabilityProbe_FallbackJoinsTheSameRace(t *testing.T) {
	setFallbackDelay(t, 20*time.Millisecond)
	stubProbeDial(t, func(ctx context.Context, addr string) error {
		if strings.HasPrefix(addr, "primary") {
			<-ctx.Done() // blackholed: no answer until the race ends
			return ctx.Err()
		}
		return nil
	})
	start := time.Now()
	online, viaFallback := reachabilityProbe(context.Background(), []string{"primary1:443", "primary2:443"}, []string{"fallback:443"})
	if !online || !viaFallback {
		t.Fatalf("online=%v viaFallback=%v, want true true", online, viaFallback)
	}
	if took := time.Since(start); took >= probeRaceTimeout {
		t.Errorf("the fallback answered after %v: it must not wait out the race", took)
	}
}

// A healthy network never dials the fallback hosts.
// Mutant: start the fallback dials with the primaries.
func TestReachabilityProbe_NoFallbackDialWhenATargetAnswers(t *testing.T) {
	setFallbackDelay(t, time.Hour)
	dialled := stubProbeDial(t, func(context.Context, string) error { return nil })
	online, viaFallback := reachabilityProbe(context.Background(), []string{"primary:443"}, []string{"fallback:443"})
	if !online || viaFallback {
		t.Fatalf("online=%v viaFallback=%v, want true false", online, viaFallback)
	}
	for _, a := range dialled() {
		if a == "fallback:443" {
			t.Error("the fallback host was dialled although a configured target answered")
		}
	}
}

// Targets that are refused at once do not make the probe sit out the delay.
// Mutant: start the fallback only on the timer — the probe hangs for an hour.
func TestReachabilityProbe_FallbackStartsWhenEveryTargetHasFailed(t *testing.T) {
	setFallbackDelay(t, time.Hour)
	stubProbeDial(t, func(_ context.Context, addr string) error {
		if addr == "fallback:443" {
			return nil
		}
		return errors.New("refused")
	})
	done := make(chan [2]bool, 1)
	go func() {
		online, via := reachabilityProbe(context.Background(), []string{"primary:443"}, []string{"fallback:443"})
		done <- [2]bool{online, via}
	}()
	select {
	case got := <-done:
		if got != [2]bool{true, true} {
			t.Fatalf("online, viaFallback = %v, want true true", got)
		}
	case <-time.After(probeRaceTimeout + 2*time.Second):
		t.Fatal("the probe waited for the fallback delay after every target had failed")
	}
}

// A real outage fails the fallback too, in the same window.
func TestReachabilityProbe_OfflineWhenFallbackFailsToo(t *testing.T) {
	setFallbackDelay(t, 10*time.Millisecond)
	dialled := stubProbeDial(t, func(context.Context, string) error { return errors.New("no route") })
	online, viaFallback := reachabilityProbe(context.Background(), []string{"primary:443"}, []string{"fb1:443", "fb2:443"})
	if online || viaFallback {
		t.Fatalf("online=%v viaFallback=%v, want false false", online, viaFallback)
	}
	if n := len(dialled()); n != 3 {
		t.Errorf("dialled %d addresses, want 3 (%v)", n, dialled())
	}
}

type recLogger struct {
	mu    sync.Mutex
	warns []string
	infos []string
}

func (l *recLogger) Debug(string, ...any) {}
func (l *recLogger) Error(string, ...any) {}
func (l *recLogger) Info(msg string, _ ...any) {
	l.mu.Lock()
	l.infos = append(l.infos, msg)
	l.mu.Unlock()
}
func (l *recLogger) Warn(msg string, _ ...any) {
	l.mu.Lock()
	l.warns = append(l.warns, msg)
	l.mu.Unlock()
}
func (l *recLogger) count(list *[]string, sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range *list {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

// The real probe closure of a Monitor: blocked targets, a reachable platform
// host. The monitor stays online and says so once, however long it lasts, and
// once more only after the targets have answered in between.
// Mutant: warn on every fallback-only probe — five Warns.
func TestMonitor_FallbackOnlyWarnsOncePerEpisode(t *testing.T) {
	setFallbackDelay(t, time.Hour)
	var blocked atomic.Bool
	blocked.Store(true)
	stubProbeDial(t, func(_ context.Context, addr string) error {
		if addr == "fallback:443" || !blocked.Load() {
			return nil
		}
		return errors.New("refused")
	})
	log := &recLogger{}
	m := NewMonitor(log)
	m.SetProbeTargets([]string{"primary:443"})
	m.SetProbeFallbackTargets([]string{"fallback:443"})

	for range 5 {
		m.poll()
	}
	if !m.IsOnline() {
		t.Fatal("the monitor went offline although a platform host answered")
	}
	if n := log.count(&log.warns, "connectivity.probe_targets"); n != 1 {
		t.Fatalf("%d Warns after five fallback-only probes, want 1: %v", n, log.warns)
	}

	blocked.Store(false)
	m.poll()
	if n := log.count(&log.infos, "answer again"); n != 1 {
		t.Errorf("%d recovery lines, want 1: %v", n, log.infos)
	}
	blocked.Store(true)
	m.poll()
	m.poll()
	if n := log.count(&log.warns, "connectivity.probe_targets"); n != 2 {
		t.Errorf("%d Warns after a second blocked stretch, want 2", n)
	}
}

// A long outage is explained once, naming the setting, and again only for the
// next outage.
// Mutant: drop the offlineExplained latch — one Warn per poll.
func TestMonitor_LongOfflineIsExplainedOnce(t *testing.T) {
	log := &recLogger{}
	m := newTestMonitor(func() bool { return false })
	m.logger = log
	m.poll()
	m.poll() // second failed poll: offline
	if m.IsOnline() {
		t.Fatal("premise lost: not offline")
	}
	if n := len(log.warns); n != 0 {
		t.Fatalf("warned %d times at the start of an outage, want 0", n)
	}
	at := time.Now().Add(-offlineExplainAfter - time.Second)
	m.offlineAt.Store(&at)
	m.poll()
	m.poll()
	if n := log.count(&log.warns, "connectivity.probe_targets"); n != 1 {
		t.Fatalf("%d Warns in one long outage, want 1: %v", n, log.warns)
	}

	m.transition(true)
	m.transition(false)
	at2 := time.Now().Add(-offlineExplainAfter - time.Second)
	m.offlineAt.Store(&at2)
	m.poll()
	if n := log.count(&log.warns, "connectivity.probe_targets"); n != 2 {
		t.Errorf("%d Warns after a second long outage, want 2", n)
	}
}
