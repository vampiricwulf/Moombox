package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/connectivity"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// outageCoalesceWindow is how long the link must stay up after a restore
// before the outage that restore ended is reported. A loss inside it continues
// the same episode, so the restores of a link that flaps coalesce into ONE
// Outage Alert carrying the flap count rather than one per cycle: the monitor
// comes back online on its first good poll, and a link dropping two polls in
// three cycled it — and the alert with it — every ~15 s (W27-11). The cost is
// an alert that lands two minutes after the restore rather than at it, which
// for the report of an outage already over is nothing; the notifications the
// outage held are delivered at the restore itself (internal/notifications,
// Outage hold), so the alert follows them.
const outageCoalesceWindow = 2 * time.Minute

// outageReportMaxWait bounds how long a link that never settles keeps its
// episode open. outageCoalesceWindow starts again at every restore, so a link
// flapping for an afternoon would otherwise never be reported at all: an
// episode this old at a restore is reported then, flaps and all, and the next
// loss opens a new one — one alert per half hour for a link that cannot hold.
const outageReportMaxWait = 30 * time.Minute

// outageEpisode is a run of outages, each restore followed by the next loss
// inside outageCoalesceWindow.
type outageEpisode struct {
	start   time.Time     // the first loss
	end     time.Time     // the last restore
	offline time.Duration // summed over its outages
	outages int
}

// outageAlerts turns connectivity transitions into the Outage Alert
// (connectivity_restored), one per episode.
//
// ONE notification, after the restore only: a "lost" webhook has, by
// definition, no connectivity to ride — it is held until the link returns
// (internal/notifications), and a loss alert would only arrive beside the
// restore's — so a loss just opens or continues the episode, and the report
// tells the whole story (start, end, time offline, flaps). An episode offline
// less than connectivity.MinOutage in all is a blip and sends nothing: the
// engines rode it out in place and the monitors re-checked on every restore.
type outageAlerts struct {
	notify notifications.Sender
	log    interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
	after func(time.Duration, func()) stoppableTimer
	now   func() time.Time

	mu sync.Mutex
	// downAt is when the current outage began; zero while the link is up.
	downAt time.Time
	// episode is the open episode, nil when none is.
	episode *outageEpisode
	// timer is the armed coalescing window, nil when none is.
	timer stoppableTimer
	// epoch invalidates a window that escaped Stop: time.AfterFunc can have
	// entered its callback before a new loss stopped it (sidecarAlerts' rule).
	epoch uint64
	// stopped latches the alerter shut at shutdown (stop).
	stopped bool
}

func newOutageAlerts(notify notifications.Sender, log interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}, after func(time.Duration, func()) stoppableTimer, now func() time.Time,
) *outageAlerts {
	return &outageAlerts{notify: notify, log: log, after: after, now: now}
}

// onState is the connectivity subscriber. It runs on the monitor's goroutine,
// serialised with every other subscriber, and must not block: it arms or
// disarms a timer and, for an episode past outageReportMaxWait, hands one send
// to the notification manager, whose Send queues and returns.
func (a *outageAlerts) onState(online bool) {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return
	}
	now := a.now()
	if !online {
		if a.downAt.IsZero() {
			a.downAt = now
		}
		if a.episode == nil {
			a.episode = &outageEpisode{start: now}
		}
		// The link dropped again inside the window: the same episode goes on.
		a.disarmLocked()
		a.mu.Unlock()
		return
	}
	if a.downAt.IsZero() || a.episode == nil {
		// The startup state, or a repeat: no outage ended here.
		a.mu.Unlock()
		return
	}
	ep := a.episode
	ep.offline += now.Sub(a.downAt)
	ep.outages++
	ep.end = now
	a.downAt = time.Time{}
	a.disarmLocked()
	if now.Sub(ep.start) >= outageReportMaxWait {
		a.episode = nil
		report := *ep
		a.mu.Unlock()
		a.report(report)
		return
	}
	a.epoch++
	e := a.epoch
	a.timer = a.after(outageCoalesceWindow, func() { a.fire(e) })
	a.mu.Unlock()
}

// disarmLocked stops the coalescing window, if one is armed. Caller holds a.mu.
func (a *outageAlerts) disarmLocked() {
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.epoch++
}

// fire runs on the window's own goroutine, so it carries its own recover.
func (a *outageAlerts) fire(epoch uint64) {
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("panic in the outage alert", "panic", fmt.Sprint(r))
		}
	}()
	a.mu.Lock()
	if epoch != a.epoch || a.timer == nil || a.stopped || a.episode == nil {
		// A loss stopped this window while its callback was in flight.
		a.mu.Unlock()
		return
	}
	a.timer = nil
	report := *a.episode
	a.episode = nil
	a.mu.Unlock()
	a.report(report)
}

// report sends the episode's alert, or logs why it does not.
func (a *outageAlerts) report(ep outageEpisode) {
	if ep.offline < connectivity.MinOutage {
		a.log.Info("connectivity outage shorter than the alert minimum — no Outage Alert",
			"offline", ep.offline.Round(time.Second), "outages", ep.outages, "minimum", connectivity.MinOutage)
		return
	}
	title, desc, fields := outageAlert(ep.start, ep.end, ep.offline, ep.outages)
	// Event key stays "connectivity_restored": the alert still follows a
	// restore, and existing target allowlists must keep working (the UIs
	// relabel the toggle "Outage alert").
	a.notify.Send(title, desc, notifications.TypeWarning, fields,
		notifications.SendOptions{Event: "connectivity_restored"},
	)
}

// stop latches the alerter shut, and reports an episode that is over and only
// waiting out its window: an outage that ended just before a restart would
// otherwise never be heard of. An outage still running is not reported — it
// has no end yet, and the queue it would ride discards what it holds at an
// offline shutdown anyway. Called before the notification drain, which this
// send then rides as a single attempt.
func (a *outageAlerts) stop() {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return
	}
	a.stopped = true
	var pending *outageEpisode
	if a.timer != nil && a.downAt.IsZero() {
		pending = a.episode
	}
	a.disarmLocked()
	a.episode = nil
	a.mu.Unlock()
	if pending != nil {
		a.report(*pending)
	}
}

// outageAlert builds the Outage Alert for an episode spanning [start, end]:
// title, description, and the embed fields. Start/end render as Discord
// dynamic timestamps (<t:unix:f> — Discord shows each viewer's local timezone;
// Discord webhooks are the only notification target type, so the markup never
// reaches a renderer that can't display it). Duration is the time offline,
// second-rounded — for a single outage the span itself, for a flapping link
// the outages summed — and a link that dropped more than once says how many
// times (Flaps).
func outageAlert(start, end time.Time, offline time.Duration, outages int) (title, description string, fields []notifications.Field) {
	fields = []notifications.Field{
		{Name: "Started", Value: notifications.Timestamp(start), Inline: true},
		{Name: "Ended", Value: notifications.Timestamp(end), Inline: true},
		{Name: "Duration", Value: offline.Round(time.Second).String(), Inline: true},
	}
	if outages <= 1 {
		return "Outage Alert",
			"Internet connectivity was lost — channel monitoring paused and downloads waited; the monitors re-checked when it returned",
			fields
	}
	fields = append(fields, notifications.Field{Name: "Flaps", Value: fmt.Sprintf("%d", outages), Inline: true})
	return "Outage Alert",
		fmt.Sprintf("Internet connectivity dropped %d times before it settled — channel monitoring paused and downloads waited each time, offline for the duration below in all; the monitors re-checked on every restore", outages),
		fields
}
