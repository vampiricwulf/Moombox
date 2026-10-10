package notifications

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// createdInOrder answers every POST with a created message whose id is M<n>,
// n the request's index, and every PATCH with 204.
func createdInOrder(n int, r recordedReq, rw http.ResponseWriter) {
	if r.Method == http.MethodPatch {
		rw.WriteHeader(http.StatusNoContent)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	io.WriteString(rw, `{"id":"M`+strconv.Itoa(n)+`"}`)
}

// A YouTube job's id is its video id, so a deleted job's id comes back on a
// re-add, or when a channel removed and re-added re-detects the video. The
// tracker kept the deleted job's message id and History in memory — the row
// that stored them was gone, but nothing told the manager — and the new job's
// first event PATCHed the old message, far up the channel where an edit
// notifies nobody. A restarted process opened a new one, as the spec says.
// Both delete paths now drop the state: ForgetJob for a single delete,
// RetainJobs for the bulk prune that fires only a jobs-list change.
//
// Mutants: ForgetJob / RetainJobs doing nothing — the re-added job's first
// event is a PATCH of M0.
func TestADeletedJobsMessageIsNotEditedAfterItsIDComesBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delete func(m *Manager, jobID string)
	}{
		{"ForgetJob", func(m *Manager, jobID string) { m.ForgetJob(jobID) }},
		{"RetainJobs", func(m *Manager, jobID string) { m.RetainJobs(map[string]struct{}{"another": {}}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDiscord(t, createdInOrder)
			st := newMemStore()
			m := editManager(t, f, st, nil)
			const job = "dQw4w9WgXcQ"

			run(t, m, job, "found", "downloading")
			if !waitCalls(t, f, 2, 3*time.Second) {
				t.Fatalf("first life: want 2 requests, got %d", len(f.calls()))
			}
			waitFor(t, "id persisted", func() bool { return st.NotificationMsgs(job) != nil })

			// The row, and the ids stored on it, are deleted.
			st.mu.Lock()
			delete(st.rows, job)
			st.mu.Unlock()
			tc.delete(m, job)

			run(t, m, job, "added")
			if !waitCalls(t, f, 3, 3*time.Second) {
				t.Fatalf("second life: want 3 requests, got %d", len(f.calls()))
			}
			if last := f.calls()[2]; last.Method != http.MethodPost {
				t.Errorf("the re-added job's first event was %s %s, want a POST of a new message", last.Method, last.Path)
			}
		})
	}
}

// RetainJobs drops only what the list no longer holds: a job still listed
// keeps its in-process History, which only memory holds (its message id
// would be re-read from the row).
//
// Mutant: retain dropping every entry — the edit's History starts over.
func TestRetainJobsKeepsAListedJobsMessage(t *testing.T) {
	f := newFakeDiscord(t, createdInOrder)
	m := editManager(t, f, newMemStore(), nil)
	const job = "keepThisJob"

	run(t, m, job, "found")
	if !waitCalls(t, f, 1, 3*time.Second) {
		t.Fatal("no first request")
	}
	m.RetainJobs(map[string]struct{}{job: {}})
	run(t, m, job, "downloading")
	if !waitCalls(t, f, 2, 3*time.Second) {
		t.Fatal("no second request")
	}
	c := f.calls()[1]
	if c.Method != http.MethodPatch {
		t.Fatalf("a listed job's next event was %s %s, want a PATCH of its message", c.Method, c.Path)
	}
	if h := historyValue(c.Body); !strings.Contains(h, titleFor("found")) {
		t.Errorf("History = %q, want it to keep the found line", h)
	}
}

// Retiring a target lets its goroutine finish the delivery in flight;
// re-adding the same webhook — mute then unmute, remove then paste back —
// built a new queue and sender at once, so two goroutines delivered for one
// webhook: the job's next event POSTed a second lifecycle message beside the
// one still being created. The re-added queue now takes the old sender and
// waits for the old goroutine to exit.
//
// Mutant: applyTargets starting the new queue without waiting — 2 POSTs.
func TestAReAddedWebhookWaitsForItsRetiredDelivery(t *testing.T) {
	gate := make(chan struct{})
	var gateOnce sync.Once
	release := func() { gateOnce.Do(func() { close(gate) }) }
	t.Cleanup(release)
	f := newFakeDiscord(t, func(n int, r recordedReq, rw http.ResponseWriter) {
		if n == 0 {
			<-gate // Discord is slow on the first POST
		}
		createdInOrder(n, r, rw)
	})
	st := newMemStore()
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)
	m.applyTargets([]notificationTarget{editTarget(f, nil, ModeEdit)})
	t.Cleanup(func() {
		m.targetsMu.RLock()
		defer m.targetsMu.RUnlock()
		for _, q := range m.targets {
			q.stopDiscard()
		}
	})

	const job = "tw_123456789"
	m.Send("Stream Found", "x", TypeInfo, nil, SendOptions{Event: "found", JobID: job})
	if !waitCalls(t, f, 1, 3*time.Second) {
		t.Fatal("found POST never reached the server")
	}
	// Muted, then unmuted while the found POST is still in flight.
	m.applyTargets(nil)
	m.applyTargets([]notificationTarget{editTarget(f, nil, ModeEdit)})

	m.Send("Download Starting", "y", TypeDownload, nil, SendOptions{Event: "downloading", JobID: job})
	time.Sleep(50 * time.Millisecond) // the new queue's chance to jump the gate
	release()
	if !waitCalls(t, f, 2, 3*time.Second) {
		t.Fatal("downloading never reached the server")
	}
	posts := 0
	for _, c := range f.calls() {
		if c.Method == http.MethodPost {
			posts++
		}
	}
	if posts != 1 {
		t.Errorf("one job got %d lifecycle POSTs on one webhook, want 1", posts)
	}
}
