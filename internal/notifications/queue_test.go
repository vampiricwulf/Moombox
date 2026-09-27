package notifications

import (
	"errors"
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
	once   []string  // titles delivered via SendOnce
	msgs   []Message // the whole messages, for the tests that count embeds
}

func newGateSender() *gateSender { return &gateSender{gate: make(chan struct{})} }

func (g *gateSender) Send(msg Message) error {
	<-g.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.titles = append(g.titles, msg.logTitle())
	g.msgs = append(g.msgs, msg)
	return nil
}

func (g *gateSender) SendOnce(msg Message) error {
	<-g.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.titles = append(g.titles, msg.logTitle())
	g.msgs = append(g.msgs, msg)
	g.once = append(g.once, msg.logTitle())
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

func (g *gateSender) deliveredMessages() []Message {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Message(nil), g.msgs...)
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
//
// The low-tier event here is `scheduled`, not `found`: both are TierLow
// (lowTierEvents), but `found` COALESCES (batch.go), so 257 of them would
// reach this queue as 26 ten-embed items and the cap would never be touched.
// This test is about the queue's overflow policy, so it uses the low-tier
// event that still arrives one per item.
func TestQueueOverflowDropsTheOldestLowTier(t *testing.T) {
	g := newGateSender()
	m := newTestManager(t, 5*time.Second, notificationTarget{sender: g, key: "k1"})

	// One send is popped immediately and blocks on the gate; the next
	// notificationQueueCap fill the queue exactly.
	m.Send("low-0", "", TypeInfo, nil, SendOptions{Event: "scheduled"})
	waitFor(t, "the head to be in flight", func() bool { return m.targets[0].pending() == 0 })
	for i := 1; i <= notificationQueueCap; i++ {
		m.Send("low-"+itoa(i), "", TypeInfo, nil, SendOptions{Event: "scheduled"})
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
	if strings.Contains(joined, "low-1,") {
		t.Errorf("low-1 survived — the OLDEST queued low-tier entry is the one that goes: %v", got[:5])
	}
	if !strings.Contains(joined, "low-0") {
		t.Errorf("low-0 was the in-flight item and must still be delivered: %v", got[:5])
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

// TestQueueCapCountsMessagesNotEmbeds is T8-M3: the unit of everything in this
// file changed when batching landed.
//
// notificationQueueCap, dropped_oldest_low_priority, dropped_newest and pop's
// "dropped" total now count MESSAGES of one to ten embeds, not embeds. The two
// overflow tests above reach the cap with `scheduled` precisely BECAUSE
// `found` coalesces; this one is the other side of that sentence — it drives
// the cap with the coalescing event and pins that 256 items is 256 messages,
// and that shedding one sheds every embed it was carrying.
//
// THE MUTANT: counting embeds anywhere — at the cap, or when a victim is shed.
// A cap in embeds would start shedding at the 26th window here, and a shed
// that dropped one embed out of a message would leave a half-message on the
// wire.
func TestQueueCapCountsMessagesNotEmbeds(t *testing.T) {
	const perWindow = 3
	g := newGateSender()
	clk := &fakeBatchClock{}
	m := newTestManagerWithClock(t, clk, 5*time.Second, notificationTarget{sender: g, key: "k1"})

	// window closes one whole coalescing window of `found`s: perWindow sends,
	// then the timer. Each window leaves the batcher as exactly one message.
	window := func(n int) {
		for j := 0; j < perWindow; j++ {
			m.Send("w"+itoa(n)+"-"+itoa(j), "", TypeInfo, nil,
				SendOptions{Event: "found", JobID: "w" + itoa(n) + "-" + itoa(j)})
		}
		clk.fire()
	}

	// The first window is popped immediately and blocks on the gate; the next
	// notificationQueueCap fill the queue exactly.
	window(0)
	waitFor(t, "the head to be in flight", func() bool { return m.targets[0].pending() == 0 })
	for i := 1; i <= notificationQueueCap; i++ {
		window(i)
	}
	if got := m.targets[0].pending(); got != notificationQueueCap {
		t.Fatalf("queue holds %d messages, want %d — %d embeds went in, so a cap counted in embeds "+
			"would have started shedding long ago",
			got, notificationQueueCap, notificationQueueCap*perWindow)
	}

	// The overflow: one more window at a full queue. Everything queued is
	// TierLow, so the OLDEST message goes — all perWindow of its embeds.
	window(notificationQueueCap + 1)
	if got := m.targets[0].pending(); got != notificationQueueCap {
		t.Fatalf("queue holds %d messages after the overflow, want %d", got, notificationQueueCap)
	}

	g.release()
	waitFor(t, "the queue to drain", func() bool {
		return len(g.deliveredMessages()) == notificationQueueCap+1
	})

	got := g.deliveredMessages()
	embeds := 0
	for _, msg := range got {
		embeds += len(msg.Embeds)
		for _, e := range msg.Embeds {
			if strings.HasPrefix(e.Title, "w1-") {
				t.Errorf("embed %q survived — its message was the shed one, and a message is shed whole",
					e.Title)
			}
		}
	}
	if want := (notificationQueueCap + 1) * perWindow; embeds != want {
		t.Errorf("delivered %d embeds in %d messages, want %d — one whole message (%d embeds) is shed, "+
			"no more and no less", embeds, len(got), want, perWindow)
	}
}

// TestOverflowWarnsAreCoalescedWithoutLosingTheCount is T4-m3.
//
// The scenario is a backfill re-scan (R B) while Discord is unreachable: every
// catalogue row produces a `found`, the queue is full within a second, and
// each subsequent send sheds one. Measured at a line per shed, that is 1,743
// Warn lines in 35 milliseconds — the log then costs more than the sends do
// and buries the incident inside its own symptom.
//
// Coalescing alone is not enough, which is the second half of this pin: a
// burst that fits inside ONE window would report "1" and swallow the rest, so
// the queue reports its running total again when it finally empties.
//
// THE MUTANT: warning per drop (the first assertion sees ~1,000 lines), or
// coalescing without the drain-time flush (the total reads 1).
//
// `scheduled` rather than `found` for the reason
// TestQueueOverflowDropsTheOldestLowTier gives: both are TierLow, but `found`
// coalesces into ten-embed items before it ever reaches this queue, and this
// test is about what the QUEUE does when 1,000 items arrive at a full one.
func TestOverflowWarnsAreCoalescedWithoutLosingTheCount(t *testing.T) {
	g := newGateSender()
	lg := &countingLogger{}
	m := newTestManagerWithLogger(t, lg, 5*time.Second, notificationTarget{sender: g, key: "k1"})

	// One in flight against the gate, then exactly cap queued behind it.
	m.Send("low-0", "", TypeInfo, nil, SendOptions{Event: "scheduled"})
	waitFor(t, "the head to be in flight", func() bool { return m.targets[0].pending() == 0 })
	for i := 1; i <= notificationQueueCap; i++ {
		m.Send("low-"+itoa(i), "", TypeInfo, nil, SendOptions{Event: "scheduled"})
	}

	const overflow = 1000
	for i := range overflow {
		m.Send("spill-"+itoa(i), "", TypeInfo, nil, SendOptions{Event: "scheduled"})
	}

	// The burst runs in milliseconds, so it is one coalescing window: one line.
	if got := lg.warnsContaining("shedding notifications"); got > 2 {
		t.Errorf("%d shed-Warn lines for %d drops, want at most 2 — the log is the thing this burst costs", got, overflow)
	}

	g.release()
	waitFor(t, "the queue to drain", func() bool { return len(g.delivered()) == notificationQueueCap+1 })
	waitFor(t, "the drained total to be reported", func() bool {
		return lg.sawWarnContaining("notification queue drained")
	})

	if got := lg.sumWarnArg("dropped_oldest_low_priority"); got != overflow {
		t.Errorf("the Warn lines account for %d shed notifications, want %d — a coalesced count that under-reports "+
			"by three orders of magnitude is worse than the flood it replaced", got, overflow)
	}
	if got := lg.sumWarnArg("dropped_newest"); got != 0 {
		t.Errorf("%d notifications were shed as the-newest, want 0 — the queue was full of low-tier `found`s", got)
	}
}

// fieldSender blocks on a gate before recording, so a test can mutate what it
// passed to Send while the item is queued but not yet read.
type fieldSender struct {
	gate chan struct{}

	mu     sync.Mutex
	fields []Field
}

func (f *fieldSender) Send(msg Message) error {
	<-f.gate
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fields = msg.Embeds[0].Fields
	return nil
}

func (f *fieldSender) SendOnce(msg Message) error {
	return f.Send(msg)
}

func (f *fieldSender) got() []Field {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Field(nil), f.fields...)
}

// TestSendCopiesTheCallersFields is the cost of the queue.
//
// Under the old goroutine-per-send the caller's slice was read within
// microseconds; a queued item can now hold it for seconds, and the usual
// caller hands over a FieldBuilder's buffer it is entitled to reuse the moment
// Send returns. No producer on the branch does reuse one — this makes it safe
// for the one that eventually will, since the symptom would be an embed
// quietly carrying another job's error.
//
// THE MUTANT: `fields: fields` in Manager.Send. The delivered value is then
// the caller's later edit.
func TestSendCopiesTheCallersFields(t *testing.T) {
	fs := &fieldSender{gate: make(chan struct{})}
	m := newTestManager(t, 5*time.Second, notificationTarget{sender: fs, key: "k1"})

	fields := []Field{{Name: "Error", Value: "the original failure"}}
	m.Send("Job Failed", "", TypeError, fields, SendOptions{Event: "error"})
	waitFor(t, "the item to be in flight", func() bool { return m.targets[0].pending() == 0 })

	// The producer reuses its buffer for the next send, as it may.
	fields[0].Value = "a later job's failure"
	close(fs.gate)

	waitFor(t, "the delivery", func() bool { return len(fs.got()) == 1 })
	if got := fs.got()[0].Value; got != "the original failure" {
		t.Errorf("delivered Error = %q, want %q — the queue handed Discord the caller's buffer, not its content", got, "the original failure")
	}
}

// reentrantLogger re-enters the queue from inside Warn, the way a
// notify-on-warn sink would. It is the only way to observe a log line issued
// under q.mu, because sync.Mutex has no reentrancy and nothing else in the
// package takes that lock from a logger.
type reentrantLogger struct {
	q *targetQueue
}

func (l *reentrantLogger) Debug(string, ...any) {}
func (l *reentrantLogger) Info(string, ...any)  {}
func (l *reentrantLogger) Error(string, ...any) {}
func (l *reentrantLogger) Warn(string, ...any) {
	if l.q != nil {
		l.q.pending() // takes q.mu
	}
}

// TestPopLogsOutsideTheQueueLock keeps the package's one lock-order rule.
//
// Every other Warn here is issued after the unlock; pop's discard report was
// not, so it held q.mu across a logger call. No logger on the branch re-enters
// the manager, which is what keeps this a tidy-up rather than a live bug — but
// the rule is cheap to keep and expensive to rediscover, because the failure
// mode is a wedged sender goroutine, not an error.
//
// THE MUTANT: restoring `defer q.mu.Unlock()` at the top of pop. This test
// then times out on its watchdog instead of returning.
func TestPopLogsOutsideTheQueueLock(t *testing.T) {
	lg := &reentrantLogger{}
	q := newTargetQueue(notificationTarget{sender: newGateSender(), key: "k1"}, lg, nil, nil)
	lg.q = q

	q.enqueue(queued{msg: One("doomed", "", 0, nil, SendOptions{}), tier: TierLow})
	q.mu.Lock()
	q.discard = true
	q.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		q.pop()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pop did not return within 2s — it is holding q.mu across the discard Warn, so any logger that " +
			"touches the manager wedges the sender goroutine for good")
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

// attemptSender fails every delivery — the state in which the difference
// between Send and SendOnce is the 2 s + 5 s retry ladder — and counts which
// method the queue chose.
type attemptSender struct {
	mu   sync.Mutex
	send int
	once int
}

func (s *attemptSender) Send(Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.send++
	return errDeliveryFailed
}

func (s *attemptSender) SendOnce(Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.once++
	return errDeliveryFailed
}

func (s *attemptSender) counts() (send, once int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.send, s.once
}

var errDeliveryFailed = errors.New("discord is down")

// TestBeginShutdownFlushesTheOpenWindowSingleAttempt is T8-M1.
//
// BeginShutdown flushes every open coalescing window so the batch it holds is
// not evaporated — but the flush ENQUEUES, and the drain goroutine can pop
// what it enqueued immediately. Storing shuttingDown after the flush therefore
// leaves a window in which that pop reads false and the flushed batch takes
// the full 2 s + 5 s retry ladder, inside the process's 10 s force-exit.
//
// THE MUTANT: moving m.shuttingDown.Store(true) back below the flush loop.
// With one embed the window is a single enqueue and the drain goroutine loses
// the wake-up race almost every time (a probe exposed 0 of 400), so the window
// here is WIDE on purpose: 200 coalesced embeds split into 20 messages, and
// the goroutine has the remaining nineteen emits' worth of wall clock to pop
// the first one while the flag is still false. With the flag stored first
// there is no interleaving at all, so the assertion never flakes the other
// way.
func TestBeginShutdownFlushesTheOpenWindowSingleAttempt(t *testing.T) {
	const embeds = 200
	s := &attemptSender{}
	clk := &fakeBatchClock{}
	m := newTestManagerWithClock(t, clk, 5*time.Second, notificationTarget{sender: s, key: "k1"})

	// Batchable sends: they sit in the open window and nothing is queued yet.
	for i := 0; i < embeds; i++ {
		m.Send("Stream Found", "", TypeInfo, nil, SendOptions{Event: "found", JobID: itoa(i)})
	}
	m.BeginShutdown()
	m.Wait()

	send, once := s.counts()
	wantMessages := embeds / maxEmbedsPerMessage
	if send != 0 || once != wantMessages {
		t.Errorf("the flushed window took %d laddered attempt(s) and %d single attempt(s), want 0 and %d — "+
			"BeginShutdown must store shuttingDown BEFORE it flushes, or the batch it just rescued "+
			"spends the shutdown budget on a 2s+5s ladder", send, once, wantMessages)
	}
}

// TestSendOnANilManagerIsANoOp pins the guard the interface seam depends on: a
// deps struct carrying a typed-nil *Manager defeats every `!= nil` check at the
// producers, so the method itself has to be safe.
func TestSendOnANilManagerIsANoOp(t *testing.T) {
	var m *Manager
	m.Send("t", "d", TypeInfo, nil, SendOptions{Event: "finished"}) // must not panic
}
