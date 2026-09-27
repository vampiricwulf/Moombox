package notifications

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// gateSender blocks each delivery on a per-call gate so a test can hold the
// queue's head still while it fills the tail, and records what was delivered
// and by which method.
type gateSender struct {
	gate chan struct{} // closed to release every delivery

	mu     sync.Mutex
	titles []string
	once   []string // titles delivered via SendOnce
}

func newGateSender() *gateSender { return &gateSender{gate: make(chan struct{})} }

func (g *gateSender) Send(title, _ string, _ int, _ []Field, _ SendOptions) error {
	<-g.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.titles = append(g.titles, title)
	return nil
}

func (g *gateSender) SendOnce(title, _ string, _ int, _ []Field, _ SendOptions) error {
	<-g.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.titles = append(g.titles, title)
	g.once = append(g.once, title)
	return nil
}

func (g *gateSender) release() { close(g.gate) }

func (g *gateSender) delivered() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.titles...)
}

func (g *gateSender) singleAttempts() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.once...)
}

// waitFor polls cond until it holds or the deadline passes. The queue is
// asynchronous by construction, so every assertion about delivery needs one;
// a fixed sleep either flakes on a loaded CI box or wastes the wall clock.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out after 5s waiting for %s", what)
}

// TestQueueDeliversInFIFOOrder is R3. Every send was its own goroutine, so two
// embeds for one job raced and a retried one landed after a later one — an
// operator could read "Download Finished" above "Muxing Starting". One
// goroutine per target makes the order free.
//
// THE MUTANT: `go q.deliver(it, ...)` inside the run loop. The three titles
// then arrive in an arbitrary order and the assertion fails (not every run —
// run it with -count=3, which the gate set does).
func TestQueueDeliversInFIFOOrder(t *testing.T) {
	g := newGateSender()
	m := newTestManager(t, time.Second, notificationTarget{sender: g, key: "k1"})

	for _, title := range []string{"first", "second", "third"} {
		m.Send(title, "", TypeInfo, nil, SendOptions{Event: "finished"})
	}
	g.release()
	waitFor(t, "three deliveries", func() bool { return len(g.delivered()) == 3 })

	want := []string{"first", "second", "third"}
	got := g.delivered()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delivery order = %v, want %v — a job's embeds must never reorder", got, want)
		}
	}
}

// TestQueueOverflowDropsTheOldestLowTier is R2. The old semaphore dropped the
// NEWEST notification when 16 were in flight, so a backfill sweep's finds
// could shut out the Job Failed that followed them.
//
// THE MUTANT: dropping the newest unconditionally (the `error` never arrives),
// or dropping the oldest unconditionally (a queue full of alerts starts
// shedding alerts).
func TestQueueOverflowDropsTheOldestLowTier(t *testing.T) {
	g := newGateSender()
	m := newTestManager(t, 5*time.Second, notificationTarget{sender: g, key: "k1"})

	// One send is popped immediately and blocks on the gate; the next
	// notificationQueueCap fill the queue exactly.
	m.Send("found-0", "", TypeInfo, nil, SendOptions{Event: "found"})
	waitFor(t, "the head to be in flight", func() bool { return m.targets[0].pending() == 0 })
	for i := 1; i <= notificationQueueCap; i++ {
		m.Send("found-"+itoa(i), "", TypeInfo, nil, SendOptions{Event: "found"})
	}
	if got := m.targets[0].pending(); got != notificationQueueCap {
		t.Fatalf("queue holds %d, want %d before the overflow", got, notificationQueueCap)
	}

	// The overflow: an alert arrives at a full queue.
	m.Send("the-alert", "", TypeError, nil, SendOptions{Event: "error"})

	g.release()
	waitFor(t, "the queue to drain", func() bool { return len(g.delivered()) == notificationQueueCap+1 })

	got := g.delivered()
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "the-alert") {
		t.Errorf("the alert was dropped at a full queue — that is exactly the old semaphore's bug: %v", got)
	}
	if strings.Contains(joined, "found-1,") {
		t.Errorf("found-1 survived — the OLDEST queued low-tier entry is the one that goes: %v", got[:5])
	}
	if !strings.Contains(joined, "found-0") {
		t.Errorf("found-0 was the in-flight item and must still be delivered: %v", got[:5])
	}
}

// TestQueueOverflowDropsTheNewestWhenNothingIsLowTier is the other half of the
// policy: with nothing sheddable queued, the arrival is what goes, and it says
// so in a Warn. Silently dropping an older ALERT to make room for a newer one
// would lose the first symptom of an incident to its second.
func TestQueueOverflowDropsTheNewestWhenNothingIsLowTier(t *testing.T) {
	g := newGateSender()
	lg := &countingLogger{}
	m := newTestManagerWithLogger(t, lg, 5*time.Second, notificationTarget{sender: g, key: "k1"})

	m.Send("error-0", "", TypeError, nil, SendOptions{Event: "error"})
	waitFor(t, "the head to be in flight", func() bool { return m.targets[0].pending() == 0 })
	for i := 1; i <= notificationQueueCap; i++ {
		m.Send("error-"+itoa(i), "", TypeError, nil, SendOptions{Event: "error"})
	}
	m.Send("the-newest", "", TypeError, nil, SendOptions{Event: "error"})

	g.release()
	waitFor(t, "the queue to drain", func() bool { return len(g.delivered()) == notificationQueueCap+1 })

	if joined := strings.Join(g.delivered(), ","); strings.Contains(joined, "the-newest") {
		t.Error("the newest arrival was kept — with nothing low-tier queued it is what must go")
	}
	if !lg.sawWarnContaining("the-newest") {
		t.Errorf("no Warn named the dropped notification; warns = %v", lg.warns)
	}
}

// TestReloadKeepsSurvivorsAndDiscardsRemovedTargets pins the hot-reload diff.
// A target that survives keeps its goroutine, its pending items and the rate
// bucket its sender learned — restarting it would re-learn the bucket from
// scratch on every config save. A removed one finishes what it is doing and
// exits, reporting the count it dropped.
//
// THE MUTANT: rebuilding every queue on Reload (the survivor's pointer
// changes), or leaving a removed target's goroutine running (its done channel
// never closes).
func TestReloadKeepsSurvivorsAndDiscardsRemovedTargets(t *testing.T) {
	const tok = "tok-en_ABC"
	keep := "https://discord.com/api/webhooks/111111111111111111/" + tok
	drop := "https://discord.com/api/webhooks/222222222222222222/" + tok

	lg := &countingLogger{}
	m := NewManager(notifConfigWithURLs(keep, drop), lg)
	t.Cleanup(m.Wait)
	if len(m.targets) != 2 {
		t.Fatalf("built %d targets, want 2", len(m.targets))
	}
	survivor := m.targets[0]
	removed := m.targets[1]

	m.Reload(notifConfigWithURLs(keep))

	if len(m.targets) != 1 {
		t.Fatalf("after reload: %d targets, want 1", len(m.targets))
	}
	if m.targets[0] != survivor {
		t.Error("the surviving target was rebuilt — its queue, its in-flight item and its learned rate bucket are all thrown away")
	}
	select {
	case <-removed.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the removed target's goroutine is still running 5s after Reload")
	}
}

// TestReloadSwapsTheFilterOnASurvivingTarget: keeping the goroutine must not
// mean keeping the old allowlist.
func TestReloadSwapsTheFilterOnASurvivingTarget(t *testing.T) {
	const url = "https://discord.com/api/webhooks/333333333333333333/tok-en_ABC"
	cfgWith := func(events ...string) *config.MoomboxConfig {
		return &config.MoomboxConfig{Notifications: []config.NotificationConfig{{URL: url, Events: events}}}
	}
	m := NewManager(cfgWith("found"), testLogger{})
	t.Cleanup(m.Wait)
	q := m.targets[0]
	if !q.allows("found") || q.allows("error") {
		t.Fatalf("initial filter wrong: found=%v error=%v", q.allows("found"), q.allows("error"))
	}
	m.Reload(cfgWith("error"))
	if q.allows("found") || !q.allows("error") {
		t.Errorf("after reload the surviving target kept the old filter: found=%v error=%v", q.allows("found"), q.allows("error"))
	}
}

// TestWaitDrainsTheQueue is what makes shutdown step 3 mean something: the
// embeds a worker stop produced must still go out.
func TestWaitDrainsTheQueue(t *testing.T) {
	g := newGateSender()
	m := newTestManager(t, 5*time.Second, notificationTarget{sender: g, key: "k1"})
	for _, title := range []string{"a", "b", "c"} {
		m.Send(title, "", TypeInfo, nil, SendOptions{Event: "finished"})
	}
	g.release()
	m.Wait()
	if got := len(g.delivered()); got != 3 {
		t.Errorf("Wait returned with %d of 3 delivered — it must drain the queue", got)
	}
}

// TestBeginShutdownSendsSingleAttempt is the §0 ruling: the process force-exits
// 10s after shutdown begins, and the worker stop ahead of it can eat the whole
// window, so a three-attempt ladder with a 2s+5s backoff cannot finish. One
// attempt per embed is what fits, and the docs say so.
//
// THE MUTANT: dropping the shuttingDown check in the run loop. The delivery
// then goes through Send and the SendOnce list stays empty.
func TestBeginShutdownSendsSingleAttempt(t *testing.T) {
	g := newGateSender()
	g.release() // nothing blocks; we only care which method is used
	m := newTestManager(t, 5*time.Second, notificationTarget{sender: g, key: "k1"})

	m.BeginShutdown()
	m.Send("shutting-down", "", TypeError, nil, SendOptions{Event: "error"})
	m.Wait()

	if got := g.singleAttempts(); len(got) != 1 || got[0] != "shutting-down" {
		t.Errorf("single-attempt deliveries = %v, want [shutting-down] — a shutdown send must not run the retry ladder", got)
	}
}

// TestSendOnANilManagerIsANoOp pins the guard the interface seam depends on: a
// deps struct carrying a typed-nil *Manager defeats every `!= nil` check at the
// producers, so the method itself has to be safe.
func TestSendOnANilManagerIsANoOp(t *testing.T) {
	var m *Manager
	m.Send("t", "d", TypeInfo, nil, SendOptions{Event: "finished"}) // must not panic
}
