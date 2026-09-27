// Package notificationtest provides a Recorder that stands in for
// notifications.Sender (and notifications.Notifier) in tests outside
// internal/notifications.
//
// It is an ordinary package rather than a _test.go helper because the
// consumers live in four packages — internal/worker, internal/web/routes,
// cmd/moombox and internal/notifications' own tests — and a test-only file
// cannot be imported across package boundaries. Nothing in production imports
// it, so the linker drops it from the binary. internal/webtest is the
// precedent.
package notificationtest

import (
	"sync"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// Call is one delivered notification, recorded exactly as the manager would
// have handed it to a Discord target.
type Call struct {
	Title       string
	Description string
	Type        notifications.NotificationType
	Fields      []notifications.Field
	Opts        notifications.SendOptions
}

// Field returns the value of the named embed field and whether it was present.
// Assertions read better as `got, ok := call.Field("Channel")` than as a loop
// at every site.
func (c Call) Field(name string) (string, bool) {
	for _, f := range c.Fields {
		if f.Name == name {
			return f.Value, true
		}
	}
	return "", false
}

// Recorder implements notifications.Notifier (and therefore Sender) by
// recording every Send. Safe for concurrent use: producers notify from monitor,
// worker and HTTP goroutines, and a test that installs one recorder across
// several of them would otherwise race.
type Recorder struct {
	mu    sync.Mutex
	calls []Call
}

// New returns an empty Recorder.
func New() *Recorder { return &Recorder{} }

// Send records the notification. It never blocks and never fails — the whole
// point is that a producer's hot path behaves in a test exactly as it does in
// production, where Send is a queue append.
func (r *Recorder) Send(title, description string, ntype notifications.NotificationType,
	fields []notifications.Field, opts notifications.SendOptions,
) {
	// Copy the fields slice: producers build it with append and several reuse
	// a FieldBuilder, so keeping the caller's backing array would let a later
	// send rewrite an earlier recorded call.
	cp := append([]notifications.Field(nil), fields...)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{
		Title:       title,
		Description: description,
		Type:        ntype,
		Fields:      cp,
		Opts:        opts,
	})
}

// Calls returns a copy of everything recorded so far, in send order.
func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// ByEvent returns the recorded calls whose SendOptions.Event equals event, in
// send order. Exact match, deliberately: alias resolution is the manager's
// filter concern, and a test asserting "the producer emitted disk_critical"
// must not pass because a disk_warning went out.
func (r *Recorder) ByEvent(event string) []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Call
	for _, c := range r.calls {
		if c.Opts.Event == event {
			out = append(out, c)
		}
	}
	return out
}

// Reset drops everything recorded. For a table test that drives one producer
// through several states with one recorder.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

// HasTargets reports true so a producer's `if notifier.HasTargets()` cost gate
// does not skip the send under test. A test that wants the gate closed passes
// no notifier at all.
func (r *Recorder) HasTargets() bool { return true }

// Reload, BeginShutdown and Wait are the owner-surface no-ops that let a
// Recorder stand in for runState.notifyMgr, which is typed notifications.Notifier.
func (r *Recorder) Reload(*config.MoomboxConfig) {}

// BeginShutdown is a no-op: a Recorder has no delivery to degrade.
func (r *Recorder) BeginShutdown() {}

// Wait is a no-op: a Recorder has no queue to drain.
func (r *Recorder) Wait() {}
