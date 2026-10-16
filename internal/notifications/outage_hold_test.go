package notifications

import (
	"fmt"
	"io"
	"net/http"
	"slices"
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
// messages are never delivered); buildPayload not rendering the note (no
// note). Not the marking itself: both messages arrive offline, so enqueue
// marks each, and the goroutine's look that each arrival wakes marks the
// queue again (pop's beginHoldLocked), so the test cannot tell either one
// missing. TestArrivalBehindAnInPlaceHoldIsMarked kills enqueue's mark, and
// TestOutageUnderADeliveryHoldsInPlace and
// TestSecondOutageMarksWhatQueuedInTheOnlineGap kill beginHoldLocked's.
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
// Mutants: deliver not asking the gate before each attempt (attempts two
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

// cutUntil answers request n, for n < len(gates), only once gates[n] is
// closed, and then cuts the connection without answering — a request the
// test holds in flight before it fails at the transport level. Every later
// request gets a 2xx.
func cutUntil(gates ...chan struct{}) func(int, recordedReq, http.ResponseWriter) {
	var down atomic.Bool
	down.Store(true)
	cut := dropConn(&down)
	ok := okCreated("M1")
	return func(n int, r recordedReq, rw http.ResponseWriter) {
		if n >= len(gates) {
			ok(n, r, rw)
			return
		}
		<-gates[n]
		cut(n, r, rw)
	}
}

// isHolding reads the queue's hold flag under its lock.
func isHolding(q *targetQueue) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.holding
}

// TestSecondOutageMarksWhatQueuedInTheOnlineGap: a delivery held in place
// resumes on the restore without passing back through pop, so the queue
// still reads as holding while its fresh ladder retries on a link that is
// still flapping. A notification raised in that online gap queues unmarked —
// rightly, it was raised after the first outage — and is then held through
// a second one, which must mark it: it was queued when the monitor went
// offline again. It went out unmarked, because the second outage found the
// hold already set and beginHoldLocked marked only a new one.
//
// Every step waits on the state it needs rather than on a sleep: the first
// failure is held until the monitor reports offline, so the ladder's next
// look finds the outage, and the resumed attempt is held in flight across
// the arrival and the second loss.
//
// Mutant: beginHoldLocked returning early on a hold already set, before its
// marking (Job Cancelled goes out with no note).
func TestSecondOutageMarksWhatQueuedInTheOnlineGap(t *testing.T) {
	prevBackoff := discordRetryBackoff
	discordRetryBackoff = [discordMaxAttempts - 1]time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { discordRetryBackoff = prevBackoff })

	first, resumed := make(chan struct{}), make(chan struct{})
	f := newFakeDiscord(t, cutUntil(first, resumed))
	conn := newOutageConn(true)
	m := heldManager(t, conn, webhookTarget(f))
	q := m.targets[0]

	m.Send("Job Failed", "d", TypeError, nil, SendOptions{Event: "error", JobID: "yt_1"})
	waitFor(t, "the first attempt", func() bool { return len(f.calls()) == 1 })
	conn.set(false) // outage 1, under the delivery
	close(first)
	waitFor(t, "the delivery held in place", func() bool { return isHolding(q) })

	conn.set(true) // the restore; the link is still flapping
	waitFor(t, "the resumed attempt", func() bool { return len(f.calls()) == 2 })
	m.Send("Job Cancelled", "d", TypeCancelled, nil, SendOptions{Event: "cancelled", JobID: "yt_2"})
	waitFor(t, "the notification raised in the online gap", func() bool { return q.pending() == 1 })
	if !isHolding(q) {
		t.Fatal("the queue no longer reads as holding — the stale hold this test is about did not happen")
	}

	conn.set(false) // outage 2, with Job Cancelled queued
	close(resumed)
	conn.set(true)
	waitFor(t, "both notifications", func() bool { return len(f.calls()) >= 4 })
	calls := f.calls()
	last := calls[len(calls)-1]
	if last.Body.Embeds[0].Title != "Job Cancelled" {
		t.Fatalf("last delivered %q, want Job Cancelled", last.Body.Embeds[0].Title)
	}
	assertDelayedNote(t, last)
}

// TestOutageInsideAnInFlightRequestMarksWhatWaited: the monitor calls an
// outage and clears it while a delivery's request is still in flight, so the
// queue's goroutine never looks while the monitor reports offline — no pop,
// no ladder check — and only the transition itself can see what waited
// behind that request. It was queued when the monitor went offline, and goes
// out marked. An idle target's outage, with nothing queued, holds nothing and
// says nothing.
//
// Mutants: SetConnectivity's subscription not handing the offline transition
// to the queues (noteOffline; Job Cancelled goes out with no note);
// noteOffline starting a hold on a queue with no message in it (the idle
// target logs that it is holding).
func TestOutageInsideAnInFlightRequestMarksWhatWaited(t *testing.T) {
	lg := &countingLogger{}
	inFlight := make(chan struct{})
	ok := okCreated("M1")
	f := newFakeDiscord(t, func(n int, r recordedReq, rw http.ResponseWriter) {
		if n == 0 {
			<-inFlight
		}
		ok(n, r, rw)
	})
	conn := newOutageConn(true)
	m := newTestManagerWithLogger(t, lg, 2*time.Second, webhookTarget(f))
	m.SetConnectivity(conn)
	q := m.targets[0]

	conn.set(false) // an outage on an idle target
	conn.set(true)
	lg.mu.Lock()
	for _, msg := range lg.infos {
		if strings.Contains(msg, "holding this target's notifications") {
			t.Errorf("an idle target's outage logged %q — it holds nothing", msg)
		}
	}
	lg.mu.Unlock()

	m.Send("Job Failed", "d", TypeError, nil, SendOptions{Event: "error", JobID: "yt_1"})
	waitFor(t, "the request in flight", func() bool { return len(f.calls()) == 1 })
	m.Send("Job Cancelled", "d", TypeCancelled, nil, SendOptions{Event: "cancelled", JobID: "yt_2"})
	waitFor(t, "the notification queued behind it", func() bool { return q.pending() == 1 })
	conn.set(false) // called and cleared inside the request
	conn.set(true)
	close(inFlight)

	waitFor(t, "both notifications", func() bool { return len(f.calls()) == 2 })
	calls := f.calls()
	if calls[1].Body.Embeds[0].Title != "Job Cancelled" {
		t.Fatalf("second delivered %q, want Job Cancelled", calls[1].Body.Embeds[0].Title)
	}
	assertDelayedNote(t, calls[1])
}

// TestArrivalBehindAnInPlaceHoldIsMarked: a notification raised while the
// monitor reports offline, behind a delivery the outage holds in place. The
// goroutine waits in awaitOutage's hold, which looks at nothing queued behind
// it, and the offline transition came before the notification did, so
// noteOffline never saw it either: enqueue's own mark is the only one it
// gets. Everywhere else a look of the goroutine's marks an offline arrival
// again, which is why no other test here can tell that mark missing.
//
// Every step waits on the state it needs: the first attempt is held in flight
// until the monitor reports offline, and the arrival comes once the delivery
// is held.
//
// Mutant: enqueue not marking an arrival delayed while the monitor reports
// offline (Job Cancelled goes out with no note).
func TestArrivalBehindAnInPlaceHoldIsMarked(t *testing.T) {
	prevBackoff := discordRetryBackoff
	discordRetryBackoff = [discordMaxAttempts - 1]time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { discordRetryBackoff = prevBackoff })

	first := make(chan struct{})
	f := newFakeDiscord(t, cutUntil(first))
	conn := newOutageConn(true)
	m := heldManager(t, conn, webhookTarget(f))
	q := m.targets[0]

	m.Send("Job Failed", "d", TypeError, nil, SendOptions{Event: "error", JobID: "yt_1"})
	waitFor(t, "the first attempt", func() bool { return len(f.calls()) == 1 })
	conn.set(false) // the outage, under the delivery, with nothing queued
	close(first)
	waitFor(t, "the delivery held in place", func() bool { return isHolding(q) })
	m.Send("Job Cancelled", "d", TypeCancelled, nil, SendOptions{Event: "cancelled", JobID: "yt_2"})
	waitFor(t, "the notification raised during the hold", func() bool { return q.pending() == 1 })

	conn.set(true)
	waitFor(t, "both notifications", func() bool { return len(f.calls()) == 3 })
	calls := f.calls()
	if calls[1].Body.Embeds[0].Title != "Job Failed" || calls[2].Body.Embeds[0].Title != "Job Cancelled" {
		t.Fatalf("delivered %q then %q, want the held delivery first", calls[1].Body.Embeds[0].Title, calls[2].Body.Embeds[0].Title)
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
// the next rewrites it — and each says it was delayed in a Delayed field
// after Status and History, dated by when it was raised, rather than in the
// content note, which an edit that sends no content would leave on the
// message for good. The rewrite after the outage replaces the embed, and the
// mark goes with it.
//
// Mutants: newManagedBody carrying the delayed mark into the managed body's
// content (the requests carry the note); newManagedBody not adding the
// Delayed field to a held message's body (the held rewrites carry none);
// adding it whatever the mark (the post-outage rewrite still says Delayed);
// withDelayedField inserting it anywhere but third.
func TestEditModeKeepsOneMessageThroughAHold(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M1"))
	conn := newOutageConn(false)
	m := heldManager(t, conn, editTarget(f, nil, ModeEdit))

	m.Send("Download Starting", "d", TypeDownload, nil, SendOptions{Event: "downloading", JobID: "yt_7"})
	m.Send("Muxing", "d", TypeDownload, []Field{{Name: "Parts", Value: "1"}}, SendOptions{Event: "muxing", JobID: "yt_7"})
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
		fields := c.Body.Embeds[0].Fields
		if len(fields) < 3 || fields[2].Name != delayedFieldName {
			t.Errorf("%s fields %+v — want Status, History, then the Delayed mark", c.Method, fields)
			continue
		}
		at, err := time.Parse(time.RFC3339, c.Body.Embeds[0].Timestamp)
		if err != nil {
			t.Fatalf("embed timestamp %q: %v", c.Body.Embeds[0].Timestamp, err)
		}
		if want := fmt.Sprintf("<t:%d:f>", at.Unix()); !strings.Contains(fields[2].Value, want) {
			t.Errorf("Delayed = %q, want it to name when the event was raised (%s)", fields[2].Value, want)
		}
	}

	m.Send("Download Finished", "d", TypeSuccess, nil, SendOptions{Event: "finished", JobID: "yt_7"})
	waitFor(t, "the post-outage rewrite", func() bool { return len(f.calls()) == 3 })
	for _, fl := range f.calls()[2].Body.Embeds[0].Fields {
		if fl.Name == delayedFieldName {
			t.Errorf("the rewrite raised after the outage still carries %q", fl.Value)
		}
	}
}

// outageMark names the outage mark a delivered request carries: "field" for
// the Delayed field a lifecycle create or rewrite takes, "note" for the
// content line every other post takes, "" for neither, and "both" for a
// request that wrongly carries the two.
func outageMark(r recordedReq) string {
	field := false
	for _, e := range r.Body.Embeds {
		for _, fl := range e.Fields {
			field = field || fl.Name == delayedFieldName
		}
	}
	note := strings.Contains(r.Body.Content, "Delayed by a connectivity outage")
	switch {
	case field && note:
		return "both"
	case field:
		return "field"
	case note:
		return "note"
	}
	return ""
}

// outageMarks is outageMark for each of calls.
func outageMarks(calls []recordedReq) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, outageMark(c))
	}
	return out
}

// TestEditModeCloseHeldInPlaceMarksTheRestOfTheEvent is W27-10's motivating
// case on an edit-mode target: the job's lifecycle message is open when the
// link dies, and the job fails because of it. Its Job Failed was queued while
// the monitor still said online, so nothing marks it when its delivery
// starts; the closing PATCH fails on the dead link and is held in place once
// the monitor calls the outage. Everything the event sends after the outage
// says it was delayed — the PATCH's resend in its Delayed field, and the
// separate post that carries the ping in its content note, as a
// separate-mode target marks the same held Job Failed. The post went out
// unmarked: only Send's own re-render marked a resumed body, and the outage
// held the PATCH, not the post.
//
// Every step waits on the state it needs: the PATCH's first attempt is held
// in flight until the monitor reports offline, so the ladder's next look
// finds the outage.
//
// Mutants: dispatchOne not carrying the hold to the separate post (it goes
// out with no note); postOrPatch handing patchMessage no re-render, or
// patchMessage handing deliver none (the resent PATCH has no Delayed field);
// newManagedBody marking the body from the start (the PATCH's first attempt,
// made before the outage, carries the field).
func TestEditModeCloseHeldInPlaceMarksTheRestOfTheEvent(t *testing.T) {
	prevBackoff := discordRetryBackoff
	discordRetryBackoff = [discordMaxAttempts - 1]time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { discordRetryBackoff = prevBackoff })

	patched := make(chan struct{})
	var down atomic.Bool
	down.Store(true)
	cut := dropConn(&down)
	ok := okCreated("M1")
	f := newFakeDiscord(t, func(n int, r recordedReq, rw http.ResponseWriter) {
		if n == 1 { // the closing PATCH's first attempt
			<-patched
			cut(n, r, rw)
			return
		}
		ok(n, r, rw)
	})
	conn := newOutageConn(true)
	m := heldManager(t, conn, editTarget(f, nil, ModeEdit))
	q := m.targets[0]

	m.Send("Download Starting", "d", TypeDownload, nil, SendOptions{Event: "downloading", JobID: "yt_9"})
	waitFor(t, "the creating POST", func() bool { return len(f.calls()) == 1 })
	m.Send("Job Failed", "d", TypeError, nil, SendOptions{Event: "error", JobID: "yt_9"})
	waitFor(t, "the closing PATCH in flight", func() bool { return len(f.calls()) == 2 })
	conn.set(false) // the outage the job failed of, called under the PATCH
	close(patched)
	waitFor(t, "the PATCH held in place", func() bool { return isHolding(q) })
	conn.set(true)

	waitFor(t, "the PATCH's resend and the separate post", func() bool { return len(f.calls()) == 4 })
	calls := f.calls()
	if got, want := requestLines(calls), []string{"POST /", "PATCH /messages/M1", "PATCH /messages/M1", "POST /"}; !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	if got, want := outageMarks(calls), []string{"", "", "field", "note"}; !slices.Equal(got, want) {
		t.Errorf("marks = %q, want %q — the create and the PATCH's first attempt went before the outage, its resend and the separate post after it", got, want)
	}
	if sep := calls[3]; sep.Body.Embeds[0].Title == "Job Failed" {
		assertDelayedNote(t, sep)
	} else {
		t.Errorf("the last request carries %q, want the separate Job Failed", sep.Body.Embeds[0].Title)
	}
}

// resumeGate is an outageGate whose next look after arm finds a hold the
// monitor has already let go — an outage called and cleared between two
// attempts — and whose every other look finds no outage.
type resumeGate struct{ armed atomic.Bool }

func (g *resumeGate) watchingOutages() bool { return true }

func (g *resumeGate) awaitOutage(time.Duration) outageResult {
	if g.armed.CompareAndSwap(true, false) {
		return outageResumed
	}
	return outageNone
}

// TestEditModeHoldMarksEveryLaterRequest: an edit-mode event can make several
// requests — the create or rewrite of the job's message, the rewrite of a
// message an older spelling of the webhook opened (patchExtra), the separate
// post of a terminal event — and an outage that holds any one of them in
// place holds every one after it. Each request made after the hold carries
// the mark, the held one's own resend included; each made before it, none.
//
// Mutants: postWait handing deliver no re-render (the held create is resent
// unmarked); patchExtra rewriting with the unmarked body after a hold (the
// second spelling's message, rewritten after the outage, is unmarked);
// patchExtra handing patchMessage no re-render (its held rewrite is resent
// unmarked, and the separate post after it too); postOrPatch re-creating a
// gone message with the unmarked body.
func TestEditModeHoldMarksEveryLaterRequest(t *testing.T) {
	prevBackoff := discordRetryBackoff
	discordRetryBackoff = [discordMaxAttempts - 1]time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { discordRetryBackoff = prevBackoff })

	const id, tok = "123456789012345678", "abcdefTOKEN"
	plain := "https://discord.com/api/webhooks/" + id + "/" + tok
	const job = "dQw4w9WgXcQ"
	for _, tc := range []struct {
		name  string
		event string
		// stored is the job's message on this target; spelled adds one an
		// older spelling of the webhook opened, M_SLASH.
		stored  string
		spelled bool
		// cut is the request whose attempt the outage takes; gone, one
		// Discord answers Unknown Message (-1: none).
		cut, gone int
		want      []string
		marks     []string
	}{
		{"a create held in place", "downloading", "", false, 0, -1,
			[]string{"POST /", "POST /"}, []string{"", "field"}},
		{"a close held in place", "error", "M_PLAIN", true, 0, -1,
			[]string{"PATCH /messages/M_PLAIN", "PATCH /messages/M_PLAIN", "PATCH /messages/M_SLASH", "POST /"},
			[]string{"", "field", "field", "note"}},
		{"the second spelling's rewrite held in place", "error", "M_PLAIN", true, 1, -1,
			[]string{"PATCH /messages/M_PLAIN", "PATCH /messages/M_SLASH", "PATCH /messages/M_SLASH", "POST /"},
			[]string{"", "", "field", "note"}},
		{"a message gone when the held rewrite is resent", "muxing", "M_PLAIN", false, 0, 1,
			[]string{"PATCH /messages/M_PLAIN", "PATCH /messages/M_PLAIN", "POST /"},
			[]string{"", "field", "field"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &resumeGate{}
			var down atomic.Bool
			down.Store(true)
			cut := dropConn(&down)
			ok := okCreated("NEW")
			f := newFakeDiscord(t, func(n int, r recordedReq, rw http.ResponseWriter) {
				switch n {
				case tc.cut:
					g.armed.Store(true) // the ladder's next look finds the hold
					cut(n, r, rw)
				case tc.gone:
					rw.WriteHeader(http.StatusNotFound)
					io.WriteString(rw, `{"message":"Unknown Message","code":10008}`)
				default:
					ok(n, r, rw)
				}
			})
			urls := []string{plain}
			if tc.spelled {
				urls = append(urls, plain+"/")
			}
			tgt := legacyKeyTarget(t, f, urls...)
			tgt.sender.(*DiscordWebhook).setOutageGate(g)
			st := newMemStore()
			if tc.stored != "" {
				st.rows[job] = map[string]string{targetMsgKey(plain): tc.stored}
				if tc.spelled {
					st.rows[job][targetMsgKey(plain+"/")] = "M_SLASH"
				}
			}
			m := &Manager{logger: testLogger{}}
			m.SetMessageStore(st)

			if err := m.dispatchOne(tgt, One(tc.event, "d", 0, nil, SendOptions{Event: tc.event, JobID: job}), false); err != nil {
				t.Fatal(err)
			}
			calls := f.calls()
			if got := requestLines(calls); !slices.Equal(got, tc.want) {
				t.Fatalf("requests = %v, want %v", got, tc.want)
			}
			if got := outageMarks(calls); !slices.Equal(got, tc.marks) {
				t.Errorf("marks = %q, want %q", got, tc.marks)
			}
		})
	}
}

// TestRaisedAfterTheRestoreIsNotMarked: the queue holds from the online edge
// until its goroutine next looks, and the Outage Alert is sent from that same
// transition — so a notification queued in between was raised after the
// outage, and goes out unmarked, behind the held one that is.
//
// Mutant: enqueue marking an arrival delayed while the queue holds rather
// than while the monitor reports offline (the post-restore notification
// carries the note).
func TestRaisedAfterTheRestoreIsNotMarked(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M1"))
	conn := newOutageConn(false)
	m := heldManager(t, conn, webhookTarget(f))

	m.Send("Job Failed", "d", TypeError, nil, SendOptions{Event: "error", JobID: "yt_1"})
	time.Sleep(150 * time.Millisecond) // the goroutine finds it offline and parks
	// The monitor reports online, and the alert is queued before the
	// transition reaches the queue's own wake.
	conn.mu.Lock()
	conn.online = true
	conn.mu.Unlock()
	m.Send("Outage Alert", "d", TypeWarning, nil, SendOptions{Event: "connectivity_restored"})
	waitFor(t, "both notifications", func() bool { return len(f.calls()) == 2 })
	calls := f.calls()
	if calls[0].Body.Embeds[0].Title != "Job Failed" || calls[1].Body.Embeds[0].Title != "Outage Alert" {
		t.Fatalf("delivered %q then %q, want the held one first", calls[0].Body.Embeds[0].Title, calls[1].Body.Embeds[0].Title)
	}
	assertDelayedNote(t, calls[0])
	if c := calls[1].Body.Content; c != "" {
		t.Errorf("the alert raised after the restore carries content %q, want none", c)
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
