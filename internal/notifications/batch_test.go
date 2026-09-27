package notifications

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// fakeBatchClock arms timers without running them; fire() runs everything
// currently armed. No test in this file sleeps.
type fakeBatchClock struct {
	mu    sync.Mutex
	armed []func()
}

func (c *fakeBatchClock) AfterFunc(_ time.Duration, f func()) batchTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, idx: len(c.armed)}
	c.armed = append(c.armed, f)
	return t
}

func (c *fakeBatchClock) fire() {
	c.mu.Lock()
	due := c.armed
	c.armed = nil
	c.mu.Unlock()
	for _, f := range due {
		if f != nil {
			f()
		}
	}
}

type fakeTimer struct {
	c   *fakeBatchClock
	idx int
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.idx < len(t.c.armed) && t.c.armed[t.idx] != nil {
		t.c.armed[t.idx] = nil
		return true
	}
	return false
}

func collector() (*[]Message, *sync.Mutex, emitFunc) {
	var mu sync.Mutex
	out := &[]Message{}
	return out, &mu, func(msg Message) {
		mu.Lock()
		defer mu.Unlock()
		*out = append(*out, msg)
	}
}

func found(id string) Embed {
	return Embed{Title: "Stream Found", Opts: SendOptions{Event: "found", JobID: id, Tier: TierLow}}
}

// addFound is the no-mention Add the window tests use.
func addFound(b *batcher, id string) { b.Add(found(id), "", nil) }

// TestIsBatchable is the closed set. The auth rows are the subtle ones: the
// per-JOB "Authentication Required" is the burst (one dead cookie parks N
// jobs), while the platform-level cookie family carries no JobID and is
// already cooldown-deduped — making it wait 5 s would delay the only alert
// that tells an operator their credentials are gone.
func TestIsBatchable(t *testing.T) {
	for _, tc := range []struct {
		opts SendOptions
		want bool
		why  string
	}{
		{SendOptions{Event: "found"}, true, "a backfill sweep is the burst this exists for"},
		{SendOptions{Event: "added"}, true, "a bulk add is the same shape"},
		{SendOptions{Event: "auth", JobID: "j1"}, true, "one dead cookie parks N jobs"},
		{SendOptions{Event: "auth"}, false, "the platform-level cookie alerts must not be delayed"},
		{SendOptions{Event: "error", JobID: "j1"}, false, "a failure goes out at once"},
		{SendOptions{Event: "cancelled"}, false, ""},
		{SendOptions{Event: "finished"}, false, ""},
		{SendOptions{Event: "disk_critical"}, false, ""},
		{SendOptions{Event: "trim_created"}, false, ""},
		{SendOptions{Event: ""}, false, "an unfiltered send (SendTest) is never batched"},
	} {
		if got := isBatchable(tc.opts); got != tc.want {
			t.Errorf("isBatchable(%+v) = %v, want %v — %s", tc.opts, got, tc.want, tc.why)
		}
	}
}

// TestBatcherCoalescesOneWindow: three finds inside one window leave the
// batcher as ONE message, and nothing is emitted before the window closes.
func TestBatcherCoalescesOneWindow(t *testing.T) {
	got, mu, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	addFound(b, "a")
	addFound(b, "b")
	addFound(b, "c")
	mu.Lock()
	n := len(*got)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("emitted %d messages before the window closed", n)
	}
	clk.fire()
	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("emitted %d messages, want 1", len(*got))
	}
	if len((*got)[0].Embeds) != 3 {
		t.Errorf("the message carried %d embeds, want 3", len((*got)[0].Embeds))
	}
}

// TestBatcherSplitsAtTen is Discord's documented per-message embed cap:
// overflow rolls into further messages, in order, and nothing is dropped —
// the whole point of batching a backfill sweep is that the sweep stops LOSING
// notifications.
func TestBatcherSplitsAtTen(t *testing.T) {
	got, mu, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	for i := range 23 {
		addFound(b, itoa(i))
	}
	clk.fire()
	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 3 {
		t.Fatalf("emitted %d messages for 23 embeds, want 3", len(*got))
	}
	n := []int{len((*got)[0].Embeds), len((*got)[1].Embeds), len((*got)[2].Embeds)}
	if n[0] != 10 || n[1] != 10 || n[2] != 3 {
		t.Errorf("message sizes %v, want [10 10 3]", n)
	}
	if (*got)[0].Embeds[0].Opts.JobID != "0" || (*got)[2].Embeds[2].Opts.JobID != "22" {
		t.Error("arrival order was not preserved across the split")
	}
}

// TestBatcherSplitsOnTheMessageCharacterBudget is the OTHER Discord cap, and
// the one the first draft of this plan missed. clampEmbed applies its 6000 to
// ONE embed (internal/notifications/limits.go), so ten legally-clamped embeds
// can total 60,000 characters — and discord.go's ladder treats a non-429 4xx
// as permanent, so the whole batch would be one Error line and ten lost
// notifications. Five near-maximal embeds must therefore come out as more
// than one message even though five is under the ten-embed cap.
func TestBatcherSplitsOnTheMessageCharacterBudget(t *testing.T) {
	got, mu, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	big := strings.Repeat("あ", 2000) // 2000 runes each; three of them exceed 6000
	for i := range 5 {
		b.Add(Embed{
			Title:       "Stream Found",
			Description: big,
			Opts:        SendOptions{Event: "found", JobID: itoa(i), Tier: TierLow},
		}, "", nil)
	}
	clk.fire()

	mu.Lock()
	defer mu.Unlock()
	if len(*got) < 2 {
		t.Fatalf("five 2000-rune embeds came out as %d message(s) — the per-MESSAGE 6000 budget is "+
			"not being applied, and Discord would reject the POST as a permanent 400", len(*got))
	}
	seen := 0
	for i, m := range *got {
		if n := messageRunes(m.Embeds); n > limitTotal {
			t.Errorf("message %d is %d runes, over the %d Discord allows per message", i, n, limitTotal)
		}
		for _, e := range m.Embeds {
			if e.Opts.JobID != itoa(seen) {
				t.Errorf("embed out of order: got %q, want %q", e.Opts.JobID, itoa(seen))
			}
			seen++
		}
	}
	if seen != 5 {
		t.Errorf("%d embeds were delivered, want all 5 — splitting must never drop one", seen)
	}
}

// TestBatcherMeasuresTheClampedSize is the other half of the budget rule, and
// the half the previous test cannot see: the splitter must measure what the
// PAYLOAD will carry, which is the embed AFTER clampEmbed has spent its
// per-embed budget. Here one embed is 9,289 raw characters and 5,153 clamped
// (clampEmbed drops trailing fields until the embed fits its own 6000), so the
// pair fits one message by the honest measure and overflows by the raw one.
//
// Measuring raw would only ever split MORE than necessary — never post an
// over-budget message — so nothing in the previous test can fail on it. What
// it costs is a message per embed for exactly the batches batching exists for.
func TestBatcherMeasuresTheClampedSize(t *testing.T) {
	got, mu, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})

	fat := Embed{
		Title:       "Stream Found",
		Description: strings.Repeat("x", limitDescription),
		Opts:        SendOptions{Event: "found", JobID: "a", Tier: TierLow},
	}
	for range 5 {
		fat.Fields = append(fat.Fields, Field{Name: strings.Repeat("n", 10), Value: strings.Repeat("v", limitFieldValue)})
	}
	b.Add(fat, "", nil)
	b.Add(found("b"), "", nil)
	clk.fire()

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("emitted %d messages, want 1 — the splitter sized the embeds BEFORE clampEmbed spent "+
			"their per-embed budget, so it split a pair that fits", len(*got))
	}
	if n := messageRunes((*got)[0].Embeds); n > limitTotal {
		t.Errorf("the message is %d runes, over the %d Discord allows", n, limitTotal)
	}
}

// TestBatcherPassesNonBatchableThrough: an error never waits behind an open
// find window.
func TestBatcherPassesNonBatchableThrough(t *testing.T) {
	got, mu, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	addFound(b, "a")
	b.Add(Embed{Title: "Job Failed", Opts: SendOptions{Event: "error", JobID: "j9"}}, "", nil)
	mu.Lock()
	ok := len(*got) == 1 && len((*got)[0].Embeds) == 1 && (*got)[0].Embeds[0].Title == "Job Failed"
	mu.Unlock()
	if !ok {
		t.Fatalf("the error did not go out immediately: %v", *got)
	}
	clk.fire()
	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 2 {
		t.Fatalf("the find window did not close afterwards: %d messages", len(*got))
	}
}

// TestBatchIsLowTier: one important embed protects the whole message from the
// queue's drop-oldest-low-tier policy. effectiveTier is N1's derivation, and
// TierUnset is its zero value — there is no sentinel to invent here.
func TestBatchIsLowTier(t *testing.T) {
	low := []Embed{found("a"), found("b")}
	if !batchIsLowTier(low) {
		t.Error("an all-low batch is not low-tier")
	}
	mixed := []Embed{found("a"), {Opts: SendOptions{Event: "auth", JobID: "j1"}}}
	if batchIsLowTier(mixed) {
		t.Error("a batch containing a normal-tier member was marked low-tier — the queue would drop it " +
			"under pressure ahead of a lone Stream Found")
	}
	if batchIsLowTier(nil) {
		t.Error("an empty message must not be reported low-tier")
	}
}

// TestBatchPingsOnce: the message carries ONE mention however many members
// were eligible — Discord's content and allowed_mentions are message-level,
// so there is nowhere to put a second, and ten pings for one backfill sweep
// is the noise batching exists to remove.
func TestBatchPingsOnce(t *testing.T) {
	got, mu, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	allowed := MentionParse("@here")
	addFound(b, "a")                    // not mention-eligible
	b.Add(found("b"), "@here", allowed) // eligible
	b.Add(found("c"), "@here", allowed) // eligible too
	addFound(b, "d")                    // not eligible, and AFTER the eligible ones
	clk.fire()

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("emitted %d messages, want 1", len(*got))
	}
	// Exactly the one ping, and it survives the members either side of it: an
	// implementation that took the LAST mention would lose it to the trailing
	// ineligible embed, and one that accumulated them would carry "@here@here".
	if (*got)[0].Mention != "@here" || (*got)[0].MentionAllowed == nil {
		t.Errorf("the batch carried (%q, %v), want the one eligible member's ping",
			(*got)[0].Mention, (*got)[0].MentionAllowed)
	}

	// And a window with no eligible member pings nobody.
	got2, mu2, emit2 := collector()
	clk2 := &fakeBatchClock{}
	b2 := newBatcher(batchWindow, clk2, emit2, testLogger{})
	addFound(b2, "a")
	clk2.fire()
	mu2.Lock()
	defer mu2.Unlock()
	if (*got2)[0].Mention != "" || (*got2)[0].MentionAllowed != nil {
		t.Error("a batch with no mention-eligible member still carried a ping")
	}
}

// TestBatcherFlushOnShutdown: a window open when shutdown begins must go out,
// not evaporate.
func TestBatcherFlushOnShutdown(t *testing.T) {
	got, mu, emit := collector()
	clk := &fakeBatchClock{}
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	addFound(b, "a")
	b.Flush()
	mu.Lock()
	n := len(*got)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("Flush emitted %d messages, want 1", n)
	}
	// And the window is DISARMED, not merely emptied. An emptied window makes
	// the stale timer a no-op only until the next embed arrives: that embed
	// opens a fresh window, and the timer the first one left behind would close
	// it early — a 5 s window that lasts whatever was left of its predecessor's.
	clk.mu.Lock()
	armed := 0
	for _, f := range clk.armed {
		if f != nil {
			armed++
		}
	}
	clk.mu.Unlock()
	if armed != 0 {
		t.Errorf("Flush left %d timer(s) armed — the next window inherits one and closes early", armed)
	}

	clk.fire() // the disarmed timer must not double-emit
	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Errorf("the timer fired after Flush and emitted the batch twice")
	}
}

// TestBatcherWindowIsNotSliding: a steady trickle must not hold the window
// open forever. The timer is armed by the FIRST item only.
func TestBatcherWindowIsNotSliding(t *testing.T) {
	clk := &fakeBatchClock{}
	_, _, emit := collector()
	b := newBatcher(batchWindow, clk, emit, testLogger{})
	addFound(b, "a")
	addFound(b, "b")
	clk.mu.Lock()
	armed := len(clk.armed)
	clk.mu.Unlock()
	if armed != 1 {
		t.Errorf("%d timers armed for two items in one window, want 1 — a re-armed timer is a sliding "+
			"window and a trickle would never flush", armed)
	}
}

// batchSender records the Messages a queue actually delivered.
type batchSender struct {
	mu   sync.Mutex
	msgs []Message
}

func (s *batchSender) Send(msg Message) error     { return s.record(msg) }
func (s *batchSender) SendOnce(msg Message) error { return s.record(msg) }
func (s *batchSender) record(msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg)
	return nil
}
func (s *batchSender) delivered() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.msgs...)
}

// TestSendBatchesThroughTheTarget is the whole seam: three finds become ONE
// delivered message and an error jumps the open window.
func TestSendBatchesThroughTheTarget(t *testing.T) {
	rec := &batchSender{}
	clk := &fakeBatchClock{}
	m := newTestManagerWithClock(t, clk, time.Second, notificationTarget{sender: rec, key: "k1"})

	m.Send("Stream Found", "d", TypeInfo, nil, SendOptions{Event: "found", JobID: "a"})
	m.Send("Stream Found", "d", TypeInfo, nil, SendOptions{Event: "found", JobID: "b"})
	m.Send("Job Failed", "d", TypeError, nil, SendOptions{Event: "error", JobID: "c"})
	clk.fire() // close the find window
	m.Wait()   // flush, closeDrain, and block until the queue goroutine exits

	got := rec.delivered()
	if len(got) != 2 {
		t.Fatalf("delivered %d messages, want 2 (the error, then the coalesced finds)", len(got))
	}
	if got[0].Embeds[0].Title != "Job Failed" {
		t.Errorf("the error did not jump the open find window; first message was %q", got[0].logTitle())
	}
	if len(got[1].Embeds) != 2 {
		t.Errorf("the two finds arrived as %d embeds, want one 2-embed message", len(got[1].Embeds))
	}
}

// TestFilteredEventNeverEntersTheBatch: filters run BEFORE coalescing, so a
// target that does not subscribe to `found` never accumulates one — a batch
// must never deliver an embed the allowlist excluded.
func TestFilteredEventNeverEntersTheBatch(t *testing.T) {
	rec := &batchSender{}
	clk := &fakeBatchClock{}
	m := newTestManagerWithClock(t, clk, time.Second, notificationTarget{
		sender: rec,
		key:    "k1",
		events: map[string]bool{"error": true},
	})

	m.Send("Stream Found", "d", TypeInfo, nil, SendOptions{Event: "found", JobID: "a"})
	clk.fire()
	m.Wait()

	if got := rec.delivered(); len(got) != 0 {
		t.Errorf("a filtered-out event reached the batch: %v", got)
	}
}

// TestReloadFlushesARetiredTargetsWindow is the defect that put the batcher on
// targetQueue rather than on notificationTarget. applyTargets keeps a
// SURVIVING target's queue and discards the freshly built notificationTarget;
// a batcher hung on the latter would take the open window with it, on every
// unrelated config save, with no log line. Here the survivor's window must
// still be there after the Reload and still flush — and the window of the
// target the same Reload REMOVES must be flushed into its queue rather than
// evaporating with the batcher.
func TestReloadFlushesARetiredTargetsWindow(t *testing.T) {
	const keep, drop = "discord://1/aaa", "discord://2/bbb"
	survivor, removed := &batchSender{}, &batchSender{}
	lg := &countingLogger{}
	clk := &fakeBatchClock{}
	m := newTestManagerWithClockFromConfig(t, clk, lg,
		&config.MoomboxConfig{Notifications: []config.NotificationConfig{{URL: keep}, {URL: drop}}},
		survivor, removed)

	m.targetsMu.RLock()
	removedQ := m.targets[1]
	m.targetsMu.RUnlock()

	m.Send("Stream Found", "d", TypeInfo, nil, SendOptions{Event: "found", JobID: "a"})

	// A save that touches nothing about the first target and deletes the second.
	m.Reload(&config.MoomboxConfig{Notifications: []config.NotificationConfig{{URL: keep}}})

	select {
	case <-removedQ.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the removed target's goroutine is still running 5s after Reload")
	}
	m.Wait() // flushes the survivor's still-open window, then drains it

	if got := survivor.delivered(); len(got) != 1 || len(got[0].Embeds) != 1 {
		t.Errorf("the surviving target delivered %v — its open find window did not survive an unrelated "+
			"Reload and reach the wire. Either the batcher is on the build-time notificationTarget, which "+
			"applyTargets throws away, or Wait flushed AFTER closeDrain, which enqueues straight into the "+
			"drop path", got)
	}
	// The retired target's window is flushed into its queue BEFORE stopDiscard,
	// so its embed is accounted for exactly once: either the drain delivered it
	// first, or pop's "target removed" Warn counted it. Which of the two wins is
	// a race between the flush's signal and stopDiscard, and both are honest
	// outcomes for a webhook the operator just deleted. Neither happening is the
	// mutant: without q.batch.Stop() in applyTargets' retired loop the embed is
	// lost with the batcher and this total reads 0.
	if n := len(removed.delivered()) + lg.sumWarnArg("dropped"); n != 1 {
		t.Errorf("the retired target accounted for %d of its 1 batched embed (delivered %d, discarded %d) — "+
			"a removed target's open window must be flushed, not dropped on the floor",
			n, len(removed.delivered()), lg.sumWarnArg("dropped"))
	}
}
