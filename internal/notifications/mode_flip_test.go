package notifications

import (
	"net/http"
	"testing"
	"time"
)

// A reload that flips a target to edit mode rebinds its dispatch under the
// manager's lock and moves its batcher to edit mode only after the lock drops.
// In that gap the batcher is still coalescing while dispatchOne already
// manages lifecycle messages, so a job's immediate event used to create the
// message ahead of the job's own `found` still held in the window — and the
// window, flushed by the flip, then edited the message back to "Found". The
// immediate event now releases a window holding its own job first
// (holdsPredecessor), so the found creates the message and the later event
// edits it, whichever side of the gap it lands on.
//
// Mutant: holdsPredecessor ignoring JobID — the downloading send creates the
// message and the flushed found rewinds it.
func TestAModeFlipCannotRewindAJobToFound(t *testing.T) {
	f := newFakeDiscord(t, createdInOrder)
	m := &Manager{logger: testLogger{}}
	installTargets(t, m, editTarget(f, nil, ModeSeparate))
	q := m.targets[0]
	const job = "flipJob"

	m.Send("Stream Found", "d", TypeInfo, nil, SendOptions{Event: "found", JobID: job}) // held in the window

	// The gap: dispatch rebound to the edit-mode decision, batcher not yet moved.
	bound := editTarget(f, nil, ModeEdit)
	bound.sender = q.sender
	q.setDispatch(func(msg Message, once bool) error { return m.dispatchOne(bound, msg, once) })
	m.Send("Download Starting", "d", TypeDownload, nil, SendOptions{Event: "downloading", JobID: job})
	q.batch.setMode(ModeEdit)

	if !waitCalls(t, f, 2, 3*time.Second) {
		t.Fatalf("want 2 requests, got %d", len(f.calls()))
	}
	time.Sleep(50 * time.Millisecond) // nothing more may follow
	calls := f.calls()
	if len(calls) != 2 || calls[0].Method != http.MethodPost || calls[1].Method != http.MethodPatch {
		t.Fatalf("requests = %+v, want the found's POST then the downloading PATCH", calls)
	}
	if got := statusValue(calls[1].Body); got != lifecycleLabels["downloading"] {
		t.Errorf("final Status = %q, want Downloading", got)
	}
}
