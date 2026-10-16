package notifications

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Connectivity is the slice of the connectivity monitor
// (internal/connectivity.Monitor) the manager holds deliveries on: the same two
// methods the worker's orchestrator takes (worker.Connectivity). An interface so
// this package imports no monitor and a test can drive the state by hand.
type Connectivity interface {
	IsOnline() bool
	OnStateChange(fn func(online bool)) func()
}

// outageVerdictWait bounds how long a delivery whose LAST attempt failed at the
// transport level waits for the monitor to call it an outage before giving up:
// three of the monitor's 5 s polls, the window the engine's chunk ladder gives
// it for the same question (awaitOutageVerdict, internal/engine).
//
// The monitor is slower than the ladder it is asked about: it goes offline on
// its second failed poll, and a poll on a dead network spends a few seconds in
// its own probe, while three attempts on a dial that is refused at once are
// over in ~7 s. Without the wait, a notification raised as the link went down
// — a Job Failed for a job the outage itself failed — spent its whole ladder
// before the monitor had a verdict, and was dropped as a webhook that would
// not answer. It is paid only when the link fails under a delivery and the
// monitor still says online: Discord unreachable on a working link, which the
// ladder then drops as before, one window later. A package var so tests can
// shrink it.
var outageVerdictWait = 15 * time.Second

// connSource is the manager's connectivity monitor, shared with every target
// queue by pointer (like shuttingDown): SetConnectivity runs after NewManager
// has built the queues, and a queue must see the monitor from then on.
type connSource struct {
	c atomic.Pointer[connHolder]
}

// connHolder boxes the interface for the atomic pointer.
type connHolder struct{ c Connectivity }

// watching reports whether a monitor is wired. A nil source (a bare queue in a
// test) watches nothing.
func (s *connSource) watching() bool {
	return s != nil && s.c.Load() != nil
}

// offline reports whether the wired monitor says the device is offline. False
// when none is wired: with nothing to consult, every delivery runs as it
// always did.
func (s *connSource) offline() bool {
	if s == nil {
		return false
	}
	h := s.c.Load()
	return h != nil && !h.c.IsOnline()
}

// SetConnectivity gives the manager the connectivity monitor (cmd/moombox
// wires the live one at boot, beside SetMessageStore). From then on a target
// holds its queue while the monitor reports offline, and a delivery whose link
// fails under it holds in place once the monitor calls it an outage — see
// targetQueue.pop and DiscordWebhook.deliverWith. Every transition wakes every
// target, so a held queue resumes on the online edge itself.
//
// Never blocks the monitor: the callback only signals each queue, without
// waiting on anything. A second call replaces the first monitor.
func (m *Manager) SetConnectivity(c Connectivity) {
	if m == nil || c == nil {
		return
	}
	m.conn.c.Store(&connHolder{c: c})
	unsub := c.OnStateChange(func(bool) { m.wakeQueues() })
	m.targetsMu.Lock()
	prev := m.unsubConn
	m.unsubConn = unsub
	m.targetsMu.Unlock()
	if prev != nil {
		prev()
	}
	// The monitor may already be offline — an offline boot seeds it before
	// anything is wired — and a queue that is waiting needs to look again.
	m.wakeQueues()
}

// wakeQueues nudges every live target's goroutine to re-read the monitor.
func (m *Manager) wakeQueues() {
	m.targetsMu.RLock()
	targets := m.targets
	m.targetsMu.RUnlock()
	for _, q := range targets {
		q.signal()
	}
}

// outageResult is what a delivery's wait on the link came to (awaitOutage).
type outageResult int

const (
	// outageNone: the monitor never called it an outage within the wait,
	// which ran its full length.
	outageNone outageResult = iota
	// outageResumed: the monitor reported offline, the delivery was held, and
	// the monitor is reporting online again.
	outageResumed
	// outageGaveUp: the target was retired, or closed for shutdown, while the
	// delivery was held or waiting for a verdict.
	outageGaveUp
)

// outageGate is what a DiscordWebhook's retry ladder asks when the link fails
// under a delivery. The target queue that drains the webhook implements it and
// installs itself on its sender (setOutageGate) when its goroutine starts:
// the ladder runs on that goroutine, so the wait it asks for holds exactly
// that target's queue and nothing else.
type outageGate interface {
	// watchingOutages reports whether a connectivity monitor is wired at all.
	watchingOutages() bool
	// awaitOutage waits up to d for the monitor to report offline (d of zero
	// only looks), and on an outage blocks until the monitor reports online
	// again or the target goes away.
	awaitOutage(d time.Duration) outageResult
}

// gatedSender is a sender whose retry ladder consults an outageGate.
// *DiscordWebhook is the only one.
type gatedSender interface {
	setOutageGate(g outageGate)
}

// delayedNote is the line a held message carries in its content, naming when
// it was raised: the earliest of its embeds, so a batch is dated by its first
// find. Each embed's own timestamp still dates each one.
func delayedNote(embeds []Embed) string {
	var first time.Time
	for _, e := range embeds {
		if at := e.eventTime(); first.IsZero() || at.Before(first) {
			first = at
		}
	}
	if first.IsZero() {
		first = time.Now()
	}
	if len(embeds) > 1 {
		return "Delayed by a connectivity outage — raised from " + Timestamp(first)
	}
	return "Delayed by a connectivity outage — raised " + Timestamp(first)
}

// delayedFieldName names the field an edit-mode lifecycle rewrite carries
// instead of delayedNote (dispatchOne).
const delayedFieldName = "Delayed"

// withDelayedField returns fields with the outage mark inserted after the
// rewrite's Status and History (rewriteFields puts them first), so the clamp,
// which drops trailing fields, keeps it ahead of the event's own. at is when
// the event was raised. The caller's slice is not modified.
func withDelayedField(fields []Field, at time.Time) []Field {
	mark := Field{Name: delayedFieldName, Value: "By a connectivity outage — raised " + Timestamp(at)}
	pos := min(2, len(fields))
	out := make([]Field, 0, len(fields)+1)
	out = append(out, fields[:pos]...)
	out = append(out, mark)
	return append(out, fields[pos:]...)
}

// heldDropReport is the one message a target gets on the online edge when its
// queue overflowed while it held: how many of the oldest it dropped. It goes
// out AHEAD of the held backlog, so the gap is explained before the
// notifications around it arrive. No event: it is about this target's own
// queue, so no filter applies and no mention rides it.
func heldDropReport(n int) Message {
	msg := One("Notifications Dropped During Outage",
		fmt.Sprintf("%d notifications raised while the connectivity monitor reported offline were dropped — a target holds at most %d through an outage and drops the oldest first. The rest follow, each marked as delayed.",
			n, notificationQueueCap),
		TypeWarning.Color(),
		[]Field{{Name: "Dropped", Value: fmt.Sprintf("%d", n), Inline: true}},
		SendOptions{})
	msg.Embeds[0].At = time.Now()
	return msg
}
