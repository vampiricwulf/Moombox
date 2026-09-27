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
const notificationQueueCap = 256

// dropWarnInterval is how often ONE target may say it is shedding.
//
// A backfill re-scan (R B) against a dead Discord overflows the queue on
// nearly every send: 2,000 sends into a 256-slot queue was measured at 1,743
// Warn lines in 35ms. The producer barely notices (17µs a send) — the log
// pays, and 1,743 near-identical lines bury the incident they are reporting.
// The count is the diagnostic, not the line per victim, so the lines coalesce
// and each one carries the total since the last.
const dropWarnInterval = 5 * time.Second

// queued is one embed waiting for one target. The fields mirror Send's
// parameters; the tier is resolved once at enqueue so the overflow policy
// never has to re-derive it.
type queued struct {
	title       string
	description string
	color       int
	fields      []Field
	opts        SendOptions
	tier        Tier
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
	mu      sync.Mutex
	events  map[string]bool // nil means all events
	items   []queued
	closing bool // drain what is queued, then exit (Wait)
	discard bool // drop what is queued, then exit (a removed target)

	// The ping, as buildTargets resolved it. Guarded by mu like events,
	// because a Reload swaps them on a surviving queue while Send reads them.
	// mention is "" when this target never pings; mentionEvents is nil then
	// too, and an EMPTY non-nil map is the explicit "never".
	mention        string
	mentionAllowed *AllowedMentions
	mentionEvents  map[string]bool

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
}

func newTargetQueue(t notificationTarget, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}, shuttingDown *atomic.Bool,
) *targetQueue {
	return &targetQueue{
		sender:         t.sender,
		key:            t.key,
		events:         t.events,
		mention:        t.mention,
		mentionAllowed: t.mentionAllowed,
		mentionEvents:  t.mentionEvents,
		shuttingDown:   shuttingDown,
		logger:         logger,
		wake:           make(chan struct{}, 1),
		done:           make(chan struct{}),
	}
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
// for the more specific one that split from it. Unlike allows, an EMPTY event
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
	if alias, hasAlias := eventAliases[event]; hasAlias && q.mentionEvents[alias] {
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

// pending is the queue depth, for tests and for the discard report.
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
		q.logger.Warn("dropping notification — the target is shutting down",
			"event", it.opts.Event, "title", it.title)
		return
	}
	if len(q.items) < notificationQueueCap {
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
		if warn {
			q.logger.Warn("notification queue full — shedding notifications",
				"cap", notificationQueueCap, "dropped_newest", nNewest,
				"dropped_oldest_low_priority", nOldest,
				"event", it.opts.Event, "title", it.title)
		}
		return
	}
	dropped := q.items[victim]
	q.items = append(q.items[:victim], q.items[victim+1:]...)
	q.items = append(q.items, it)
	nOldest, nNewest, warn := q.noteDrop(true)
	q.mu.Unlock()
	if warn {
		q.logger.Warn("notification queue full — shedding notifications",
			"cap", notificationQueueCap, "dropped_oldest_low_priority", nOldest,
			"dropped_newest", nNewest,
			"event", dropped.opts.Event, "title", dropped.title)
	}
	q.signal()
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
		// the edit asked for.
		n := len(q.items)
		q.items = nil
		q.mu.Unlock()
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
	var err error
	if q.shuttingDown != nil && q.shuttingDown.Load() {
		// Owner ruling: shutdown sends are single-attempt and the 10s
		// force-exit stays. A 2s+5s retry ladder cannot finish inside a window
		// the worker stop may already have spent.
		err = q.sender.SendOnce(it.title, it.description, it.color, it.fields, it.opts)
	} else {
		err = q.sender.Send(it.title, it.description, it.color, it.fields, it.opts)
	}
	if err != nil {
		q.logger.Error("notification send failed", "err", err)
	}
}
