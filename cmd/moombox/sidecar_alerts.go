package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// sidecarDownDebounce is how long the BotGuard sidecar must stay unhealthy
// before the operator hears about it.
//
// The supervisor's restart ladder handles everything shorter — a child that
// dies and is replaced inside a minute cost nothing anyone needs to act on —
// so the webhook is for the failure the supervisor could NOT fix (§0 ruling).
// The cost is a 60-second-later alert, which is nothing next to the alternative
// of one page per restart.
const sidecarDownDebounce = 60 * time.Second

// stoppableTimer is the slice of *time.Timer this file uses. It is an
// interface so a test can supply a clock it controls: the 59-second case
// cannot be asserted by waiting.
type stoppableTimer interface{ Stop() bool }

// sidecarAlerts turns sidecar health transitions into the sidecar_down /
// sidecar_restored pair.
//
// It is the SECOND subscriber on sidecar.SubscribeHealth; the first is the TUI
// status bar (tui_wiring.go), which is why a headless or Docker install learnt
// about a dead sidecar only from the log (audit A1). Unlike the TUI's, this
// one must run whether or not a TUI exists.
type sidecarAlerts struct {
	notify notifications.Sender
	log    interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
	after func(time.Duration, func()) stoppableTimer

	mu sync.Mutex
	// timer is the armed debounce, nil when none is pending.
	timer stoppableTimer
	// epoch invalidates a timer that already escaped Stop. time.AfterFunc can
	// have entered its callback before Stop is called, and the callback then
	// cannot tell "I am the current outage" from "I am a cancelled one" — the
	// epoch it captured can.
	epoch uint64
	// downSent gates the all-clear: a recovery is only news if the outage was
	// reported. Without it every healthy boot and every supervisor restart
	// would announce a recovery from nothing.
	downSent bool
	// last is the most recent snapshot, read by the debounce when it fires so
	// the embed names the CURRENT reason and restart count.
	last sidecar.Health
}

func newSidecarAlerts(notify notifications.Sender, log interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}, after func(time.Duration, func()) stoppableTimer) *sidecarAlerts {
	return &sidecarAlerts{notify: notify, log: log, after: after}
}

// onHealth is the subscriber. It runs on the PUBLISHER's goroutine (the
// supervisor loop, or startup) and must not block, which is why the only work
// it does is arm or cancel a timer and hand a send to the notification
// manager, whose Send queues and returns.
func (a *sidecarAlerts) onHealth(h sidecar.Health) {
	a.mu.Lock()
	a.last = h

	if !h.Healthy {
		if a.timer == nil && !a.downSent {
			a.epoch++
			ep := a.epoch
			a.timer = a.after(sidecarDownDebounce, func() { a.fireDown(ep) })
		}
		a.mu.Unlock()
		return
	}

	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
		a.epoch++
	}
	if !a.downSent {
		a.mu.Unlock()
		return
	}
	a.downSent = false
	restarts := h.Restarts
	a.mu.Unlock()

	a.log.Info("BotGuard sidecar healthy again", "restarts", restarts)
	a.notify.Send("BotGuard Sidecar Restored",
		"The BotGuard sidecar is answering again — PO token minting is back on the V8 child",
		notifications.TypeSuccess,
		[]notifications.Field{
			{Name: "Restarts", Value: fmt.Sprintf("%d", restarts), Inline: true},
		},
		notifications.SendOptions{Event: "sidecar_restored"},
	)
}

// fireDown runs on the timer's own goroutine, so it carries its own recover.
func (a *sidecarAlerts) fireDown(epoch uint64) {
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("panic in the sidecar down alert", "panic", fmt.Sprint(r))
		}
	}()

	a.mu.Lock()
	if epoch != a.epoch || a.timer == nil {
		// The sidecar came back while this callback was in flight.
		a.mu.Unlock()
		return
	}
	a.timer = nil
	a.downSent = true
	h := a.last
	a.mu.Unlock()

	reason := h.Reason
	if reason == "" {
		reason = "unknown"
	}
	a.notify.Send("BotGuard Sidecar Down",
		fmt.Sprintf("The BotGuard sidecar has been unhealthy for over %s — PO tokens are falling back to the slower in-process solver until it returns",
			sidecarDownDebounce),
		notifications.TypeError,
		[]notifications.Field{
			{Name: "Reason", Value: notifications.EscapeMarkdown(reason)},
			{Name: "Restarts", Value: fmt.Sprintf("%d", h.Restarts), Inline: true},
		},
		notifications.SendOptions{Event: "sidecar_down"},
	)
}

// stop cancels a pending debounce. Called from the unsubscribe wireSidecarAlerts
// returns, so a shutdown cannot fire an alert on the way out.
func (a *sidecarAlerts) stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
		a.epoch++
	}
}

// wireSidecarAlerts subscribes the alerter and returns its teardown.
//
// SubscribeHealth calls back IMMEDIATELY with the current snapshot when one
// exists, which is deliberate here: initServices publishes a failed first start
// long before this runs, and without the immediate call the dominant failure —
// a sidecar that cannot start at all — would arm no debounce, because the only
// later publish is the Healthy:true of a restart that never comes.
//
// A process with `[bgutils] use_sidecar = false` published nothing, so nothing
// is delivered and no alert is ever armed, which is right.
func (s *runState) wireSidecarAlerts() func() {
	a := newSidecarAlerts(s.notifyMgr, s.log, func(d time.Duration, f func()) stoppableTimer {
		return time.AfterFunc(d, f)
	})
	unsub := sidecar.SubscribeHealth(a.onHealth)
	return func() {
		unsub()
		a.stop()
	}
}
