package notifications

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// outageConn is a Connectivity the test flips by hand. set calls every
// subscriber outside its own lock, the way the monitor's transition does.
type outageConn struct {
	mu     sync.Mutex
	online bool
	subs   map[int]func(bool)
	next   int
}

func newOutageConn(online bool) *outageConn {
	return &outageConn{online: online, subs: map[int]func(bool){}}
}

func (c *outageConn) IsOnline() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.online
}

func (c *outageConn) OnStateChange(fn func(bool)) func() {
	c.mu.Lock()
	id := c.next
	c.next++
	c.subs[id] = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
	}
}

func (c *outageConn) set(online bool) {
	c.mu.Lock()
	c.online = online
	fns := make([]func(bool), 0, len(c.subs))
	for _, fn := range c.subs {
		fns = append(fns, fn)
	}
	c.mu.Unlock()
	for _, fn := range fns {
		fn(online)
	}
}

// heldManager starts a Manager over targets through applyTargets, as
// production does, and wires conn into it as cmd/moombox wires the monitor.
func heldManager(t *testing.T, conn Connectivity, targets ...notificationTarget) *Manager {
	t.Helper()
	m := newTestManager(t, 2*time.Second, targets...)
	m.SetConnectivity(conn)
	return m
}

// webhookTarget is a separate-mode target posting to f.
func webhookTarget(f *fakeDiscord) notificationTarget {
	return notificationTarget{sender: &DiscordWebhook{URL: f.URL()}, key: f.URL()}
}

// msgSender records every Message it is handed, delayed flag and all, and
// sends nothing: a transport with no retry ladder, so only the queue's own
// gate can hold it.
type msgSender struct {
	mu   sync.Mutex
	msgs []Message
}

func (s *msgSender) Send(msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg)
	return nil
}

func (s *msgSender) SendOnce(msg Message) error { return s.Send(msg) }

func (s *msgSender) got() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.msgs...)
}

// dropConn answers 2xx while up and, while down, cuts the connection without
// answering — a transport-level failure, the shape a dead link gives the
// client.
func dropConn(down *atomic.Bool) func(int, recordedReq, http.ResponseWriter) {
	ok := okCreated("M1")
	return func(n int, r recordedReq, rw http.ResponseWriter) {
		if down.Load() {
			if hj, isHJ := rw.(http.Hijacker); isHJ {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
					return
				}
			}
			panic(http.ErrAbortHandler)
		}
		ok(n, r, rw)
	}
}

// assertDelayedNote checks a delivered payload carries the outage note, dated
// by the embed's own timestamp — when the event was raised, not delivered.
func assertDelayedNote(t *testing.T, r recordedReq) {
	t.Helper()
	if !strings.Contains(r.Body.Content, "Delayed by a connectivity outage") {
		t.Fatalf("delivered %q with content %q — want the outage note", r.Body.Embeds[0].Title, r.Body.Content)
	}
	at, err := time.Parse(time.RFC3339, r.Body.Embeds[0].Timestamp)
	if err != nil {
		t.Fatalf("embed timestamp %q: %v", r.Body.Embeds[0].Timestamp, err)
	}
	if want := fmt.Sprintf("<t:%d:f>", at.Unix()); !strings.Contains(r.Body.Content, want) {
		t.Errorf("note %q does not name the time the event was raised (%s)", r.Body.Content, want)
	}
}

// TestNotificationRaisedOfflineIsHeldAndDeliveredMarked is W27-10's main
// case: a Job Failed and a Finished raised while the monitor reports offline
// spend nothing on the dead link, and go out on the online edge in the order
// they were raised, each saying it was delayed and when it was raised.
//
// Mutants: SetConnectivity's subscription not waking the queues (the held
// messages are never delivered); enqueue or beginHoldLocked not marking a
// held message delayed, or buildPayload not rendering the note (no note).
func TestNotificationRaisedOfflineIsHeldAndDeliveredMarked(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M1"))
	conn := newOutageConn(false)
	m := heldManager(t, conn, webhookTarget(f))

	m.Send("Job Failed", "outage", TypeError, nil, SendOptions{Event: "error", JobID: "yt_1"})
	m.Send("Download Finished", "muxed offline", TypeSuccess, nil, SendOptions{Event: "finished", JobID: "yt_2"})
	time.Sleep(200 * time.Millisecond)
	if n := len(f.calls()); n != 0 {
		t.Fatalf("%d requests while the monitor reported offline — nothing may be spent on the dead link", n)
	}

	conn.set(true)
	waitFor(t, "both held notifications delivered", func() bool { return len(f.calls()) == 2 })
	calls := f.calls()
	if got := []string{calls[0].Body.Embeds[0].Title, calls[1].Body.Embeds[0].Title}; got[0] != "Job Failed" || got[1] != "Download Finished" {
		t.Errorf("delivered %v, want [Job Failed Download Finished] — the order they were raised in", got)
	}
	for _, c := range calls {
		assertDelayedNote(t, c)
	}

	// The next notification is an ordinary one again.
	m.Send("Stream Found", "after", TypeInfo, nil, SendOptions{Event: "error", JobID: "yt_3"})
	waitFor(t, "the post-outage notification", func() bool { return len(f.calls()) == 3 })
	if c := f.calls()[2]; c.Body.Content != "" {
		t.Errorf("a notification raised after the outage carries content %q, want none", c.Body.Content)
	}
}

// TestQueueHoldsWhateverTheTransport: the hold is the queue's, not only the
// Discord ladder's — a sender with no ladder at all is held too, and what it
// is handed on the online edge is marked delayed.
//
// Mutant: pop not holding a message at the head while the monitor reports
// offline (the message is handed to the sender at once).
func TestQueueHoldsWhateverTheTransport(t *testing.T) {
	s := &msgSender{}
	conn := newOutageConn(false)
	m := heldManager(t, conn, notificationTarget{sender: s})

	m.Send("Job Failed", "d", TypeError, nil, SendOptions{Event: "error"})
	time.Sleep(150 * time.Millisecond)
	if n := len(s.got()); n != 0 {
		t.Fatalf("the sender was handed %d messages while the monitor reported offline", n)
	}
	conn.set(true)
	waitFor(t, "the held message", func() bool { return len(s.got()) == 1 })
	if !s.got()[0].delayed {
		t.Error("the held message was handed over without its delayed mark")
	}
}

// TestOutageUnderADeliveryHoldsInPlace: the link dies under a delivery the
// monitor still called online. Once the monitor reports offline the ladder
// spends nothing more — the attempt it was making is the only one lost — and
// the link's return delivers the same notification, marked delayed, and then
// the one queued behind it while the monitor still said online, marked too.
//
// Mutants: deliverWith not asking the gate before each attempt (attempts two
// and three go out on the dead link); Send's re-render not marking the held
// delivery; beginHoldLocked not marking what was already queued (the second
// notification arrives unmarked).
func TestOutageUnderADeliveryHoldsInPlace(t *testing.T) {
	prevBackoff := discordRetryBackoff
	discordRetryBackoff = [discordMaxAttempts - 1]time.Duration{300 * time.Millisecond, 300 * time.Millisecond}
	t.Cleanup(func() { discordRetryBackoff = prevBackoff })

	var down atomic.Bool
	down.Store(true)
	f := newFakeDiscord(t, dropConn(&down))
	conn := newOutageConn(true)
	m := heldManager(t, conn, webhookTarget(f))

	m.Send("Job Failed", "d", TypeError, nil, SendOptions{Event: "error", JobID: "yt_1"})
	waitFor(t, "the first attempt", func() bool { return len(f.calls()) == 1 })
	m.Send("Job Cancelled", "d", TypeCancelled, nil, SendOptions{Event: "cancelled", JobID: "yt_2"})
	conn.set(false) // the monitor catches up inside the backoff
	time.Sleep(900 * time.Millisecond)
	if n := len(f.calls()); n != 1 {
		t.Fatalf("%d requests — the ladder kept spending attempts after the monitor reported offline", n)
	}

	down.Store(false)
	conn.set(true)
	waitFor(t, "both notifications", func() bool { return len(f.calls()) == 3 })
	calls := f.calls()
	if calls[1].Body.Embeds[0].Title != "Job Failed" || calls[2].Body.Embeds[0].Title != "Job Cancelled" {
		t.Errorf("delivered %q then %q, want the held delivery first", calls[1].Body.Embeds[0].Title, calls[2].Body.Embeds[0].Title)
	}
	assertDelayedNote(t, calls[1])
	assertDelayedNote(t, calls[2])
}

// TestLateVerdictHoldsTheLastAttempt: three refused dials are over before the
// monitor has a verdict (it needs two failed polls). The ladder waits for one
// before dropping the notification, and the outage it then reports holds it.
//
// Mutant: the final-attempt verdict wait removed (the notification is dropped
// after three attempts, the monitor's verdict landing a moment too late).
func TestLateVerdictHoldsTheLastAttempt(t *testing.T) {
	prevBackoff, prevVerdict := discordRetryBackoff, outageVerdictWait
	discordRetryBackoff = [discordMaxAttempts - 1]time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	outageVerdictWait = 3 * time.Second
	t.Cleanup(func() { discordRetryBackoff, outageVerdictWait = prevBackoff, prevVerdict })

	var down atomic.Bool
	down.Store(true)
	f := newFakeDiscord(t, dropConn(&down))
	conn := newOutageConn(true)
	m := heldManager(t, conn, webhookTarget(f))

	m.Send("Job Failed", "d", TypeError, nil, SendOptions{Event: "error"})
	waitFor(t, "the whole ladder", func() bool { return len(f.calls()) == discordMaxAttempts })
	time.Sleep(50 * time.Millisecond)
	conn.set(false)
	time.Sleep(200 * time.Millisecond)
	down.Store(false)
	conn.set(true)
	waitFor(t, "the held notification", func() bool { return len(f.calls()) == discordMaxAttempts+1 })
	assertDelayedNote(t, f.calls()[discordMaxAttempts])
}

// TestHeldQueueDropsOldestAndReportsOnce: a held queue that fills drops its
// OLDEST — a low-tier one first, then the oldest of all — never the arrival,
// and on the online edge the target hears once how many went, ahead of the
// backlog that follows in order.
//
// Mutants: the held overflow falling back to the ordinary policy (the
// arrivals are dropped and the newest are missing); the low-tier preference
// removed (E0, E1 and E2 go and the low-tier E5 survives); the report not
// sent, or sent with the wrong count.
func TestHeldQueueDropsOldestAndReportsOnce(t *testing.T) {
	s := &msgSender{}
	conn := newOutageConn(false)
	m := heldManager(t, conn, notificationTarget{sender: s})

	total := notificationQueueCap + 3
	for i := range total {
		opts := SendOptions{Event: "error", JobID: fmt.Sprintf("yt_%d", i)}
		if i == 5 {
			opts.Tier = TierLow
		}
		m.Send(fmt.Sprintf("E%d", i), "d", TypeError, nil, opts)
	}
	conn.set(true)
	waitFor(t, "the report and the held backlog", func() bool { return len(s.got()) == notificationQueueCap+1 })

	got := s.got()
	if got[0].logTitle() != "Notifications Dropped During Outage" {
		t.Fatalf("first delivered %q, want the drop report ahead of the backlog", got[0].logTitle())
	}
	if f := got[0].Embeds[0].Fields; len(f) == 0 || f[0].Value != "3" {
		t.Errorf("the report's fields = %+v, want Dropped: 3", f)
	}
	var want []string
	for i := range total {
		if i == 0 || i == 1 || i == 5 {
			continue
		}
		want = append(want, fmt.Sprintf("E%d", i))
	}
	for i, w := range want {
		if g := got[i+1].logTitle(); g != w {
			t.Fatalf("backlog[%d] = %q, want %q (the low-tier E5 and the oldest E0, E1 dropped; the rest in order)", i, g, w)
		}
		if !got[i+1].delayed {
			t.Fatalf("backlog[%d] %q was not marked delayed", i, w)
		}
	}
	if len(got) != len(want)+1 {
		t.Errorf("%d delivered, want the report plus %d", len(got), len(want))
	}
}

// TestEditModeKeepsOneMessageThroughAHold: an edit-mode target's held
// lifecycle events still build ONE message per job — the first creates it,
// the next rewrites it — and neither body carries the content note, which an
// edit that sends no content would leave on the message for good.
//
// Mutant: dispatchOne carrying the delayed mark into the managed body (both
// requests carry the note in their content).
func TestEditModeKeepsOneMessageThroughAHold(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M1"))
	conn := newOutageConn(false)
	m := heldManager(t, conn, editTarget(f, nil, ModeEdit))

	m.Send("Download Starting", "d", TypeDownload, nil, SendOptions{Event: "downloading", JobID: "yt_7"})
	m.Send("Download Finished", "d", TypeSuccess, nil, SendOptions{Event: "finished", JobID: "yt_7"})
	time.Sleep(150 * time.Millisecond)
	if n := len(f.calls()); n != 0 {
		t.Fatalf("%d requests while offline", n)
	}
	conn.set(true)
	waitFor(t, "the create and the rewrite", func() bool { return len(f.calls()) == 2 })
	calls := f.calls()
	if calls[0].Method != http.MethodPost || calls[0].Query != "wait=true" {
		t.Errorf("first request %s ?%s, want the creating POST ?wait=true", calls[0].Method, calls[0].Query)
	}
	if calls[1].Method != http.MethodPatch || !strings.HasSuffix(calls[1].Path, "/messages/M1") {
		t.Errorf("second request %s %s, want the PATCH of message M1", calls[1].Method, calls[1].Path)
	}
	for _, c := range calls {
		if c.Body.Content != "" {
			t.Errorf("%s carried content %q — a lifecycle rewrite must not take the note", c.Method, c.Body.Content)
		}
	}
}

// TestStepRunsWhileHeld: a queued step (a job delete's ForgetJob) needs no
// network, so a hold does not keep it from running once it reaches the head.
//
// Mutant: pop holding whatever is at the head, a step included.
func TestStepRunsWhileHeld(t *testing.T) {
	s := &msgSender{}
	conn := newOutageConn(false)
	m := heldManager(t, conn, notificationTarget{sender: s})
	q := m.targets[0]

	ran := make(chan struct{})
	if !q.enqueueControl(func() { close(ran) }) {
		t.Fatal("enqueueControl refused")
	}
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("a step at the head of a held queue did not run")
	}
}

// TestShutdownWhileOfflineDiscardsHeld: closing a queue while the monitor
// still reports offline discards what it holds at once — single attempts on a
// dead link would only spend the force-exit's seconds — instead of leaving
// Wait to time out on a goroutine that waits for a link that is not coming.
//
// Mutant: pop's closing-while-offline arm removed (the goroutine keeps holding
// and Wait runs to its timeout).
func TestShutdownWhileOfflineDiscardsHeld(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M1"))
	conn := newOutageConn(false)
	m := heldManager(t, conn, webhookTarget(f))

	m.Send("Job Failed", "d", TypeError, nil, SendOptions{Event: "error"})
	time.Sleep(100 * time.Millisecond)
	m.BeginShutdown()
	start := time.Now()
	m.Wait()
	if el := time.Since(start); el > time.Second {
		t.Errorf("Wait took %v while offline — the held queue must discard and exit, not wait out its timeout", el)
	}
	if n := len(f.calls()); n != 0 {
		t.Errorf("%d requests at an offline shutdown, want none", n)
	}
}

// TestNoMonitorNoHold: a Manager no monitor was given behaves exactly as
// before — the CLI's `moombox add` builds one — and a gated webhook with no
// monitor wired runs its ladder unchanged.
//
// Mutant: connSource.offline answering true with no monitor wired.
func TestNoMonitorNoHold(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M1"))
	m := newTestManager(t, 2*time.Second, webhookTarget(f))
	m.Send("Job Failed", "d", TypeError, nil, SendOptions{Event: "error"})
	waitFor(t, "the delivery", func() bool { return len(f.calls()) == 1 })
	if c := f.calls()[0]; c.Body.Content != "" {
		t.Errorf("content %q on an ordinary delivery", c.Body.Content)
	}
}
