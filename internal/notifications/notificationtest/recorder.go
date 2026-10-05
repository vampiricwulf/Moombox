// Package notificationtest provides a Recorder that stands in for
// notifications.Sender (and notifications.Notifier) in tests outside
// internal/notifications.
//
// It is an ordinary package rather than a _test.go helper because the
// consumer packages — internal/worker, internal/web/routes and cmd/moombox,
// plus any external notifications_test — cannot import a test-only file across
// a package boundary. internal/notifications' OWN internal tests are the one
// caller it cannot serve: this package imports notifications, so importing it
// back would be a cycle. They use the package-local fakes in queue_test.go
// instead. Nothing in production imports this, so the linker drops it from the
// binary. internal/webtest is the precedent.
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

// clone deep-copies the one reference a Call holds. Copying the outer slice is
// not enough: a Call handed out by value still points at the recorded Fields
// array, so a test that sorts or rewrites `got[0].Fields` would be editing the
// recorder's own record — and the next Calls() would hand the edit to the next
// assertion.
func clone(c Call) Call {
	c.Fields = append([]notifications.Field(nil), c.Fields...)
	return c
}

// Calls returns a copy of everything recorded so far, in send order.
func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Call
	for _, c := range r.calls {
		out = append(out, clone(c))
	}
	return out
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
			out = append(out, clone(c))
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

// ForgetJob and RetainJobs are no-ops: a Recorder keeps no per-job message
// state.
func (r *Recorder) ForgetJob(string) {}

// RetainJobs is a no-op; see ForgetJob.
func (r *Recorder) RetainJobs(map[string]struct{}) {}
