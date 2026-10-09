package notifications

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// notificationQueueCap bounds one target's pending FIFO.
//
// 256 is deliberately generous: the burst this exists for is a backfill
// re-scan (R B), which creates a `found` per catalogue row across every
// configured channel, and a dead cookie, which parks N jobs at once. At
// roughly one delivery per second against a healthy webhook, 256 is over four
// minutes of backlog — long enough that reaching the cap means Discord is
// down, not that Moombox is busy.
//
// The unit is a MESSAGE, and since batching (batch.go) those two bursts arrive
// coalesced: `found`, `added` and the per-job `auth` reach this queue as
// ten-embed messages, so 256 items is up to 2,560 of those embeds. What can
// still fill it one item at a time is the non-batchable families.
const notificationQueueCap = 256

// dropWarnInterval is how often ONE target may say it is shedding.
//
// A backfill re-scan (R B) against a dead Discord overflows the queue on
// nearly every send: 2,000 sends into a 256-slot queue was measured at 1,743
// Warn lines in 35ms. The producer barely notices (17µs a send) — the log
// pays, and 1,743 near-identical lines bury the incident they are reporting.
// The count is the diagnostic, not the line per victim, so the lines coalesce
// and each one carries the total since the last.
//
// That measurement predates batching (batch.go), when a find was one item: the
// same sweep now arrives ten embeds to an item, so it takes roughly ten times
// the catalogue to reach the same line count. The reasoning is unchanged — the
// non-batchable families still arrive one item per send.
const dropWarnInterval = 5 * time.Second

// queued is one MESSAGE waiting for one target. The tier is resolved once at
// enqueue so the overflow policy never has to re-derive it.
type queued struct {
	msg  Message
	tier Tier
	// ctl, when set, makes this item a step for the sender to run rather than
	// a message to deliver: enqueueControl's way of doing something to this
	// target's edit-mode state AFTER every item queued before it. Never
	// delivered, never shed (its tier is not TierLow), never discarded, and
	// never counted toward notificationQueueCap (targetQueue.steps).
	ctl func()
	// unpin lets go of the job's tracker entry the item was pinned with at
	// enqueue (targetQueue.pin) — nil when it was not. Every way an item
	// leaves the queue lets go of it, exactly once (letGo): delivered, shed,
	// refused or discarded. One missed keeps its job's entry for good.
	unpin func()
}

// letGo releases the item's pin, if it holds one.
func (it queued) letGo() {
	if it.unpin != nil {
		it.unpin()
	}
}

// targetQueue is one destination, its FIFO, and the single goroutine that
// drains it.
//
// ONE goroutine is the whole design. It buys three things the old
// goroutine-per-send could not: a job's embeds can never reorder (R3); a burst
// can never put sixteen concurrent POSTs into one webhook's rate bucket and
// self-inflict 429s that each cost a delivery attempt (R1); and a full queue
// can choose WHICH entry to shed instead of always shedding the arrival (R2).
type targetQueue struct {
	sender sender
	// key is the RESOLVED webhook URL — the same identity buildTargets dedupes
	// on. Reload matches on it so a surviving target keeps this queue, its
	// pending items, and the rate bucket its sender has learned.
	key string
	// msgKey is the target's targetMsgKey, the key its edit-mode message ids
	// are held under — what Manager.forgetInOrder drops on this queue. Fixed
	// for the queue's life: it is derived from key, and a survivor keeps both.
	msgKey string
	// shuttingDown is the Manager's flag, shared by pointer. When it is set,
	// deliveries make a single attempt instead of running the retry ladder.
	shuttingDown *atomic.Bool
	logger       interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	// mu guards everything below. A plain mutex over a slice rather than a
	// buffered channel: a channel cannot drop its OLDEST element, which is
	// exactly what the overflow policy has to do.
	mu     sync.Mutex
	events map[string]bool // nil means all events
	items  []queued
	// steps is how many of items are enqueueControl steps. The cap counts
	// messages only: a batch delete queues one step per job on every
	// edit-mode target, and counted, a few hundred of them behind a slow
	// delivery made the queue "full" and shed the next alert.
	steps   int
	closing bool // drain what is queued, then exit (Wait)
	discard bool // drop what is queued, then exit (a removed target)
	// exited is set by the pop that tells the goroutine to return: nothing
	// appended after it would ever run, so enqueueControl refuses from then on.
	exited bool

	// The ping, as buildTargets resolved it. Guarded by mu like events,
	// because a Reload swaps them on a surviving queue while Send reads them.
	// mention is "" when this target never pings; mentionEvents is nil then
	// too, and an EMPTY non-nil map is the explicit "never".
	mention        string
	mentionAllowed *AllowedMentions
	mentionEvents  map[string]bool

	// dispatch is the ONE decision point for a queued message: an edit-mode
	// target's single-embed lifecycle event is created or rewritten here;
	// everything else — including every multi-embed batch — posts exactly as
	// it did before edit mode existed. Guarded by mu like mention/events,
	// because applyTargets rebinds it on a surviving queue.
	dispatch func(msg Message, once bool) error
	// pin is dispatch's twin at enqueue, bound with it: for a message
	// dispatch will manage it pins the job's tracker entry until the item
	// leaves the queue (Manager.pinLifecycle), and returns nil for any other.
	// nil when no Manager bound one. Guarded by mu like dispatch.
	pin func(msg Message) (unpin func())
	// mode is the delivery mode dispatch was bound in, and editing whether
	// this queue may still create or edit a lifecycle message: set by a bind
	// in edit mode, and cleared only when a delivery starts under a
	// separate-mode bind — a flip away from edit mode leaves the delivery in
	// flight on the edit path, and its POST can still record an id. Read by
	// editKeys: a queue that cannot hold a job's edit-mode state gets no
	// ForgetJob step. Both guarded by mu.
	mode    string
	editing bool
	// legacyMsgKeys are the bound target's old-spelling keys
	// (notificationTarget.legacyMsgKeys), which a step drops with msgKey.
	// Guarded by mu: a Reload can add or remove a spelling on a survivor.
	legacyMsgKeys []string

	// The overflow Warn's coalescing state — see dropWarnInterval. Both kinds
	// of shed are counted separately because they mean different things: the
	// oldest-low-priority kind is the policy working (chatter making room for
	// alerts), the newest kind is 256 queued ALERTS and a webhook that has
	// been down long enough to start losing them.
	droppedOldest int
	droppedNewest int
	lastDropWarn  time.Time

	// wake carries one buffered token, so a signal sent while the goroutine is
	// between pop and receive is remembered rather than lost.
	wake chan struct{}
	// done closes when the goroutine has exited.
	done chan struct{}

	// batch is the coalescing stage in FRONT of this queue (batch.go). It has
	// its own mutex and is never guarded by mu — Send calls it, and it calls
	// back in through enqueueBatch.
	batch *batcher
}

func newTargetQueue(t notificationTarget, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}, shuttingDown *atomic.Bool, clock batchClock,
) *targetQueue {
	q := &targetQueue{
		sender:         t.sender,
		key:            t.key,
		msgKey:         t.msgKey,
		events:         t.events,
		mention:        t.mention,
		mentionAllowed: t.mentionAllowed,
		mentionEvents:  t.mentionEvents,
		mode:           normalizeTargetMode(t.mode),
		editing:        normalizeTargetMode(t.mode) == ModeEdit,
		legacyMsgKeys:  t.legacyMsgKeys,
		shuttingDown:   shuttingDown,
		logger:         logger,
		wake:           make(chan struct{}, 1),
		done:           make(chan struct{}),
	}
	// The coalescing stage sits in front of THIS queue. It lives on the queue,
	// not on the notificationTarget that built it, because applyTargets keeps a
	// surviving target's queue and throws the freshly built target away — a
	// batcher hung on the latter would take every open window with it on each
	// unrelated config save, silently.
	q.batch = newBatcher(batchWindow, clock, q.enqueueBatch, logger)
	// The mode is known here, so a queue is never briefly reachable in the
	// wrong one: applyTargets can only call setMode AFTER targetsMu is
	// released, and this queue is published — and draining — from the moment
	// that lock drops. setMode is then a no-op for a fresh queue.
	q.batch.mode = normalizeTargetMode(t.mode)
	return q
}

// enqueueBatch is the batcher's exit: it wraps one coalesced Message in a
// queue item — tier from batchIsLowTier, the message keeping whatever single
// mention the flush chose — and hands it to the ordinary FIFO. Named apart
// from enqueue, which takes an already-built queued item and is what this
// calls.
//
// It is also where an item is pinned (pin), before enqueue takes q.mu: the pin
// can read the store, and enqueue lets go of an item it does not keep.
func (q *targetQueue) enqueueBatch(msg Message) {
	tier := TierNormal
	if batchIsLowTier(msg.Embeds) {
		tier = TierLow
	}
	q.mu.Lock()
	pin := q.pin
	q.mu.Unlock()
	var unpin func()
	if pin != nil {
		unpin = pin(msg)
	}
	q.enqueue(queued{msg: msg, tier: tier, unpin: unpin})
}

// signal nudges the draining goroutine without ever blocking the caller —
// Send runs on worker, monitor and HTTP goroutines and must not wait on
// Discord for anything.
func (q *targetQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// allows reports whether this target's filter admits the event.
//
// An event that split from a broader legacy name (eventAliases) also matches
// targets allowlisting the old name, so pre-split filters keep working after
// an upgrade. The alias lookup needs the ok-check: a bare map miss yields "",
// and a garbage events=[""] entry would then match EVERY non-aliased event,
// inverting the allowlist.
func (q *targetQueue) allows(event string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.events == nil || event == "" {
		return true
	}
	if q.events[event] {
		return true
	}
	alias, hasAlias := eventAliases[event]
	return hasAlias && q.events[alias]
}

// setEvents swaps the filter on a target that survived a Reload.
func (q *targetQueue) setEvents(events map[string]bool) {
	q.mu.Lock()
	q.events = events
	q.mu.Unlock()
}

// mentionFor returns the content mention this target attaches to event and the
// matching allowed_mentions object, or ("", nil) when the target has no
// mention configured or the event is not in its mention filter.
//
// Alias-aware by the same rule and the same eventAliases table as allows, so a
// target that asked to be pinged for the broader legacy event is still pinged
// for the more specific one that split from it — except a close (closeEvents),
// whose all-clear pings nobody through its alert's entry. Unlike allows, an
// EMPTY event
// pings nobody: an empty Event bypasses the delivery filter by design, and
// carrying that exemption over to the ping would mean any send that forgot its
// event name mentioned everyone.
func (q *targetQueue) mentionFor(event string) (string, *AllowedMentions) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.mention == "" || q.mentionAllowed == nil || event == "" {
		return "", nil
	}
	if q.mentionEvents[event] {
		return q.mention, q.mentionAllowed
	}
	// The ok-check matters for the same reason it does in allows: a bare map
	// miss yields "", and an "" key in the filter would then match every
	// non-aliased event.
	if alias, hasAlias := eventAliases[event]; hasAlias && !closeEvents[event] && q.mentionEvents[alias] {
		return q.mention, q.mentionAllowed
	}
	return "", nil
}

// setMention swaps the mention on a target that survived a Reload — the twin
// of setEvents, and required for the same reason: applyTargets keeps a
// survivor's queue and discards the freshly built notificationTarget, so
// without this a save that changes only `mention` or `mention_events` is
// silently ignored for every webhook that survived the diff.
func (q *targetQueue) setMention(t notificationTarget) {
	q.mu.Lock()
	q.mention = t.mention
	q.mentionAllowed = t.mentionAllowed
	q.mentionEvents = t.mentionEvents
	q.mu.Unlock()
}

// setDispatch rebinds the decision function on a target that survived a
// Reload — the twin of setEvents and setMention, and required for the same
// reason: applyTargets keeps a survivor's queue and discards the freshly
// built notificationTarget, so a `mode` change would otherwise be accepted
// by both UIs, written to the file, and ignored until restart. t is the
// target fn was bound for; its mode and old-spelling keys move with fn, under
// the same hold, and so does pin, the enqueue-side half of the same decision.
func (q *targetQueue) setDispatch(t notificationTarget, fn func(msg Message, once bool) error, pin func(msg Message) (unpin func())) {
	q.mu.Lock()
	q.dispatch = fn
	q.pin = pin
	q.mode = normalizeTargetMode(t.mode)
	q.legacyMsgKeys = t.legacyMsgKeys
	if q.mode == ModeEdit {
		q.editing = true
	}
	q.mu.Unlock()
}

// editKeys returns the keys a ForgetJob or RetainJobs step on this queue
// drops, or nil when the queue cannot hold edit-mode state (see editing) —
// a separate-mode target never reads or records a message id, and a step
// there was only a queue slot spent on nothing.
//
// The old-spelling keys too. A job open across the upgrade can hold this
// target's id under one, loaded from its row and not yet adopted (messageID
// adopts on this target's own next send); a key no step covered was dropped
// at once, and this target's queued cancel then found no id and posted
// plain, leaving the message reading "Downloading" for good.
func (q *targetQueue) editKeys() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.msgKey == "" || !q.editing {
		return nil
	}
	return append([]string{q.msgKey}, q.legacyMsgKeys...)
}

// dispatchFor runs the bound decision function under mu (a Reload rebinds it
// on a surviving queue), falling back to the plain send when no Manager
// bound one — a bare targetQueue in a test must still deliver.
func (q *targetQueue) dispatchFor(msg Message) error {
	q.mu.Lock()
	d := q.dispatch
	// The delivery about to start runs under this bind, so a queue flipped
	// to separate mode can hold no new edit-mode state from here on.
	q.editing = q.mode == ModeEdit
	q.mu.Unlock()
	once := q.shuttingDown != nil && q.shuttingDown.Load()
	if d == nil {
		return sendPlain(q.sender, msg, once)
	}
	return d(msg, once)
}

// pending is the queue depth, steps included. Test-facing: pop reads
// len(q.items) itself.
func (q *targetQueue) pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// enqueue appends it, or applies the overflow policy.
//
// OVERFLOW: the OLDEST TierLow entry goes, not the arrival. The old semaphore
// dropped the newest, so a backfill sweep's sixteen in-flight `found`s shut out
// the `Job Failed` behind them — the exact inversion of what an operator needs.
// With nothing low-tier queued, the arrival is what goes: an older alert is the
// FIRST symptom of an incident and a newer one is usually its echo.
func (q *targetQueue) enqueue(it queued) {
	q.mu.Lock()
	if q.closing || q.discard {
		q.mu.Unlock()
		it.letGo()
		q.logger.Warn("dropping notification — the target is shutting down",
			"event", it.msg.logEvent(), "title", it.msg.logTitle())
		return
	}
	if len(q.items)-q.steps < notificationQueueCap {
		q.items = append(q.items, it)
		q.mu.Unlock()
		q.signal()
		return
	}
	victim := -1
	for i := range q.items {
		if q.items[i].tier == TierLow {
			victim = i
			break
		}
	}
	if victim < 0 {
		nOldest, nNewest, warn := q.noteDrop(false)
		q.mu.Unlock()
		it.letGo()
		if warn {
			q.logger.Warn("notification queue full — shedding notifications",
				"cap", notificationQueueCap, "dropped_newest", nNewest,
				"dropped_oldest_low_priority", nOldest,
				"event", it.msg.logEvent(), "title", it.msg.logTitle())
		}
		return
	}
	dropped := q.items[victim]
	q.items = append(q.items[:victim], q.items[victim+1:]...)
	q.items = append(q.items, it)
	nOldest, nNewest, warn := q.noteDrop(true)
	q.mu.Unlock()
	dropped.letGo()
	if warn {
		q.logger.Warn("notification queue full — shedding notifications",
			"cap", notificationQueueCap, "dropped_oldest_low_priority", nOldest,
			"dropped_newest", nNewest,
			"event", dropped.msg.logEvent(), "title", dropped.msg.logTitle())
	}
	q.signal()
}

// enqueueControl queues fn to run on this target's sender goroutine after
// everything already queued — and after the delivery in flight — and reports
// whether it will run. It refuses only once the goroutine has returned.
//
// Outside the policies that govern a message: past the cap, and never counted
// toward it either (a step is not a delivery: shedding one would leave the
// state it exists to clear, and counting one would shed a message in its
// place), onto a queue draining for shutdown (the drain runs it), and onto a
// retired one (pop's discard runs the steps it holds before the goroutine
// returns).
func (q *targetQueue) enqueueControl(fn func()) bool {
	q.mu.Lock()
	if q.exited {
		q.mu.Unlock()
		return false
	}
	q.items = append(q.items, queued{ctl: fn})
	q.steps++
	q.mu.Unlock()
	q.signal()
	return true
}

// runControl runs one enqueueControl step. Its own recover for the reason
// deliver has one: a panicking step must not take the sender with it.
func (q *targetQueue) runControl(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			q.logger.Error("panic in notification queue step", "panic", fmt.Sprint(r))
		}
	}()
	fn()
}

// noteDrop counts one shed notification and reports whether this is the moment
// to say so — at most once per dropWarnInterval, carrying everything shed
// since the last line. Called with q.mu HELD; the Warn itself belongs outside
// it, like every other log line in this file.
//
// The first drop of a burst always speaks (the zero lastDropWarn is far in the
// past), so an operator learns about a shedding queue immediately and then
// hears a running total rather than a line per victim.
func (q *targetQueue) noteDrop(oldest bool) (nOldest, nNewest int, warn bool) {
	if oldest {
		q.droppedOldest++
	} else {
		q.droppedNewest++
	}
	if time.Since(q.lastDropWarn) < dropWarnInterval {
		return 0, 0, false
	}
	q.lastDropWarn = time.Now()
	nOldest, nNewest = q.droppedOldest, q.droppedNewest
	q.droppedOldest, q.droppedNewest = 0, 0
	return nOldest, nNewest, true
}

// flushDrops hands back whatever the coalescing window is still holding,
// ignoring the interval, and reports false when there is nothing to say.
// Called with q.mu HELD.
func (q *targetQueue) flushDrops() (nOldest, nNewest int, report bool) {
	if q.droppedOldest == 0 && q.droppedNewest == 0 {
		return 0, 0, false
	}
	nOldest, nNewest = q.droppedOldest, q.droppedNewest
	q.droppedOldest, q.droppedNewest = 0, 0
	return nOldest, nNewest, true
}

// pop takes the head. ok=false means nothing is queued; exit=true means the
// goroutine should return.
// The lock is released before every log line. This is the one Warn in the
// package that used to be issued UNDER q.mu, and a logger that re-entered the
// manager (a hypothetical notify-on-warn sink) deadlocked on it — enqueue
// wants the same mutex. Proven with a re-entrant logger and a 2s watchdog; no
// logger on the branch does that, which is what keeps this a tidy-up.
func (q *targetQueue) pop() (it queued, ok, exit bool) {
	q.mu.Lock()
	if q.discard {
		// A removed target reports the count and nothing else. The queued
		// items belong to a webhook the operator has just deleted from their
		// config; delivering them after the fact would be the opposite of what
		// the edit asked for. Its queued STEPS still run: each clears state a
		// delivery may have left (ForgetJob), and the in-flight delivery they
		// were queued behind has finished.
		var steps []func()
		var dropped []queued
		for _, it := range q.items {
			if it.ctl != nil {
				steps = append(steps, it.ctl)
				continue
			}
			dropped = append(dropped, it)
		}
		n := len(dropped)
		q.items = nil
		q.steps = 0
		q.exited = true
		q.mu.Unlock()
		for _, it := range dropped {
			it.letGo()
		}
		for _, fn := range steps {
			q.runControl(fn)
		}
		if n > 0 {
			q.logger.Warn("notification target removed — discarding its queued notifications", "dropped", n)
		}
		return queued{}, false, true
	}
	if len(q.items) == 0 {
		// The queue has emptied, so whatever the coalescing window was still
		// holding has no later drop to ride out on. Reporting it here is what
		// keeps the total honest for the case that motivated the coalescing:
		// a burst that sheds 1,743 items in 35ms is ONE window, and without
		// this the log would claim it shed one.
		closing := q.closing
		if closing {
			q.exited = true
		}
		nOldest, nNewest, report := q.flushDrops()
		q.mu.Unlock()
		if report {
			q.logger.Warn("notification queue drained — totals for what it shed while full",
				"dropped_oldest_low_priority", nOldest, "dropped_newest", nNewest)
		}
		return queued{}, false, closing
	}
	it = q.items[0]
	q.items = q.items[1:]
	if it.ctl != nil {
		q.steps--
	}
	q.mu.Unlock()
	return it, true, false
}

// stopDiscard retires a target removed by a Reload: it finishes the item it is
// delivering (cancelling mid-POST would leave Discord's side ambiguous) and
// then exits, discarding the rest.
func (q *targetQueue) stopDiscard() {
	q.mu.Lock()
	q.discard = true
	q.mu.Unlock()
	q.signal()
}

// closeDrain retires a target at shutdown: no new items are accepted, what is
// queued is delivered, then the goroutine exits.
func (q *targetQueue) closeDrain() {
	q.mu.Lock()
	q.closing = true
	q.mu.Unlock()
	q.signal()
}

// run is the single draining goroutine.
func (q *targetQueue) run() {
	defer close(q.done)
	defer func() {
		if r := recover(); r != nil {
			q.logger.Error("panic in notification sender loop", "panic", fmt.Sprint(r))
		}
	}()
	for {
		it, ok, exit := q.pop()
		if exit {
			return
		}
		if !ok {
			<-q.wake
			continue
		}
		q.deliver(it)
	}
}

// deliver posts one item. Its OWN recover, separate from run's: a panic inside
// one send must not kill the goroutine and strand every later notification for
// this target behind a queue nothing drains.
func (q *targetQueue) deliver(it queued) {
	defer func() {
		if r := recover(); r != nil {
			q.logger.Error("panic in notification sender", "panic", fmt.Sprint(r))
		}
	}()
	if it.ctl != nil {
		q.runControl(it.ctl)
		return
	}
	// After the dispatch, which reads the pinned entry, and deferred so a
	// panicking one lets go too.
	defer it.letGo()
	// Owner ruling: shutdown sends are single-attempt and the 15s force-exit
	// stays — dispatch carries the flag through to the edit path too, because
	// a 2s+5s retry ladder cannot finish inside a window the worker stop may
	// already have spent.
	err := q.dispatchFor(it.msg)
	if err != nil {
		q.logger.Error("notification send failed", "err", err)
	}
}
