package notifications

import (
	"io"
	"net/http"
	"strconv"
	"strings"
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
