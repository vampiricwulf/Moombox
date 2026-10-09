package notifications

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// requestLines renders a fake's requests as "METHOD path" for a failure line.
func requestLines(calls []recordedReq) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Method+" "+c.Path)
	}
	return out
}

// gated is a fake Discord whose n-th request (0-based) waits until release.
func gated(t *testing.T, n int) (f *fakeDiscord, release func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	f = newFakeDiscord(t, func(i int, r recordedReq, rw http.ResponseWriter) {
		if i == n {
			<-gate
		}
		createdInOrder(i, r, rw)
	})
	// Registered AFTER the fake, so it runs BEFORE the server's Close: Close
	// waits for the request held at the gate, and a test that fails before
	// releasing it would otherwise hang until the binary's timeout.
	t.Cleanup(release)
	return f, release
}

// deleteRow stands in for DeleteJob: the row, and the ids stored on it, go.
func deleteRow(st *memStore, jobID string) {
	st.mu.Lock()
	delete(st.rows, jobID)
	st.mu.Unlock()
}

// Deleting an active job is cancel, wait, delete — the Web DELETE route and
// the TUI alike — so the worker's "cancelled" is already queued when the
// delete lands. The drop used to happen at once: if the target was busy
// (another job's request in flight, a rate-limit wait) the cancel was
// dispatched afterwards, found neither the in-memory id nor the row, and
// posted plain, so the lifecycle message read "Downloading" for good. The
// drop now runs on the target's FIFO, behind the cancel. RetainJobs, for the
// bulk prune, is ordered the same way.
//
// Mutants: ForgetJob or RetainJobs dropping at once (forgetInOrder skipped,
// so drop / retain see no queued key) — the cancel is a plain POST.
func TestADeletedJobsQueuedCancelStillClosesItsMessage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delete func(m *Manager, jobID string)
	}{
		{"ForgetJob", func(m *Manager, jobID string) { m.ForgetJob(jobID) }},
		{"RetainJobs", func(m *Manager, _ string) { m.RetainJobs(map[string]struct{}{"otherJob123": {}}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, release := gated(t, 2) // another job's delivery is slow: the FIFO is busy
			st := newMemStore()
			m := editManager(t, f, st, nil)
			const job = "dQw4w9WgXcQ"

			run(t, m, job, "found", "downloading")
			if !waitCalls(t, f, 2, 3*time.Second) {
				t.Fatalf("want 2 requests, got %d", len(f.calls()))
			}
			waitFor(t, "id persisted", func() bool { return st.NotificationMsgs(job) != nil })

			m.Send("Found", "x", TypeInfo, nil, SendOptions{Event: "found", JobID: "otherJob123"})
			if !waitCalls(t, f, 3, 3*time.Second) {
				t.Fatal("the other job's POST never reached the server")
			}
			// The cancel is queued, then the row and the process's copy go.
			m.Send("Cancelled", "desc", TypeCancelled, nil, SendOptions{Event: "cancelled", JobID: job})
			deleteRow(st, job)
			tc.delete(m, job)
			release()

			// The closing PATCH, then the separate embed.
			if !waitCalls(t, f, 5, 3*time.Second) {
				t.Fatalf("after the delete: %v, want the cancel's PATCH and its separate POST", requestLines(f.calls()[3:]))
			}
			if c := f.calls()[3]; c.Method != http.MethodPatch || c.Path != "/messages/M0" {
				t.Errorf("the deleted job's queued cancel was %s %s, want a PATCH of its lifecycle message M0", c.Method, c.Path)
			}
		})
	}
}

// A delete that lands while the job's creating POST is in flight — a slow
// Discord, or the 429 ladder holding the item for over a minute — used to be
// undone: the POST came back and remembered its id afresh, so a re-add of the
// same video id PATCHed the deleted job's message, the exact thing the drop
// is for. Queued behind the in-flight item, the drop now runs after it.
//
// Mutants: ForgetJob or RetainJobs dropping at once — the re-added job's
// first event is a PATCH of M0.
func TestADeleteDuringTheJobsInFlightPostIsNotUndone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delete func(m *Manager, jobID string)
	}{
		{"ForgetJob", func(m *Manager, jobID string) { m.ForgetJob(jobID) }},
		{"RetainJobs", func(m *Manager, _ string) { m.RetainJobs(map[string]struct{}{"another": {}}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, release := gated(t, 0) // the deleted job's "found" POST is slow
			st := newMemStore()
			m := editManager(t, f, st, nil)
			const job = "dQw4w9WgXcQ"

			m.Send("Found", "x", TypeInfo, nil, SendOptions{Event: "found", JobID: job})
			if !waitCalls(t, f, 1, 3*time.Second) {
				t.Fatal("the found POST never reached the server")
			}
			st.mu.Lock()
			st.missing[job] = true
			st.mu.Unlock()
			tc.delete(m, job)
			before := st.writeCount()
			release()
			waitFor(t, "the in-flight POST's remember", func() bool { return st.writeCount() > before })

			st.mu.Lock()
			delete(st.missing, job)
			st.mu.Unlock()
			run(t, m, job, "added")
			if !waitCalls(t, f, 2, 3*time.Second) {
				t.Fatalf("want 2 requests, got %d", len(f.calls()))
			}
			if c := f.calls()[1]; c.Method != http.MethodPost {
				t.Errorf("the re-added job's first event was %s %s, want a POST of a new message", c.Method, c.Path)
			}
		})
	}
}

// The drop is per TARGET. Two edit-mode targets drain at their own pace, so
// the quick one reaches its drop while the slow one still has the job's
// cancel queued; dropping the whole entry there would leave the slow one's
// cancel with no message to close.
//
// Mutant: dropTarget deleting the job's whole entry — target A's cancel is a
// plain POST.
func TestOneTargetsDropLeavesAnotherTargetsMessage(t *testing.T) {
	fA, release := gated(t, 1) // A is busy with another job's request
	fB := newFakeDiscord(t, createdInOrder)
	st := newMemStore()
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)
	installTargets(t, m, editTarget(fA, nil, ModeEdit), editTarget(fB, nil, ModeEdit))
	const job = "dQw4w9WgXcQ"
	keyB := targetMsgKey(fB.URL())

	m.Send("Found", "x", TypeInfo, nil, SendOptions{Event: "found", JobID: job})
	waitFor(t, "both ids persisted", func() bool { return len(st.NotificationMsgs(job)) == 2 })
	m.Send("Found", "x", TypeInfo, nil, SendOptions{Event: "found", JobID: "otherJob123"})
	if !waitCalls(t, fA, 2, 3*time.Second) {
		t.Fatal("the other job's POST never reached A")
	}
	m.Send("Cancelled", "desc", TypeCancelled, nil, SendOptions{Event: "cancelled", JobID: job})
	if !waitCalls(t, fB, 4, 3*time.Second) {
		t.Fatalf("B: %v, want its cancel's PATCH and separate POST", requestLines(fB.calls()))
	}
	deleteRow(st, job)
	m.ForgetJob(job)
	tr := m.tracker()
	waitFor(t, "B's drop", func() bool {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		j := tr.jobs[job]
		return j == nil || j.msgs[keyB] == ""
	})
	release()

	if !waitCalls(t, fA, 4, 3*time.Second) {
		t.Fatalf("A: %v, want its cancel's PATCH and separate POST", requestLines(fA.calls()))
	}
	if c := fA.calls()[2]; c.Method != http.MethodPatch || c.Path != "/messages/M0" {
		t.Errorf("A's queued cancel was %s %s once B had dropped the job, want a PATCH of A's message M0", c.Method, c.Path)
	}
}

// While a slow target still holds a deleted job's id, the same video id can be
// re-added and the quick target post the new job's first message — whose row
// write carries the job's whole map. The slow target's id must stay out of
// it: its drop clears only memory, so after a restart the re-added job would
// edit the deleted job's message on that target.
//
// Mutants: remember's snapshot keeping a dropping key, or ForgetJob not
// marking the deleted job's keys — the new row holds B's old M0; the drop
// not clearing its key's mark — the re-added job's own ids never reach its
// row.
func TestARowWrittenBeforeASlowTargetsDropCarriesNoDeletedID(t *testing.T) {
	fA := newFakeDiscord(t, createdInOrder)
	fB, release := gated(t, 1) // B is busy with another job's request
	st := newMemStore()
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)
	installTargets(t, m, editTarget(fA, nil, ModeEdit), editTarget(fB, nil, ModeEdit))
	const job = "dQw4w9WgXcQ"
	keyA, keyB := targetMsgKey(fA.URL()), targetMsgKey(fB.URL())

	m.Send("Found", "x", TypeInfo, nil, SendOptions{Event: "found", JobID: job})
	waitFor(t, "both ids persisted", func() bool { return len(st.NotificationMsgs(job)) == 2 })
	m.Send("Found", "x", TypeInfo, nil, SendOptions{Event: "found", JobID: "otherJob123"})
	if !waitCalls(t, fB, 2, 3*time.Second) {
		t.Fatal("the other job's POST never reached B")
	}
	deleteRow(st, job)
	m.ForgetJob(job)

	// Re-added: A posts its first message for the new job and writes the row.
	m.Send("Added", "x", TypeDownload, nil, SendOptions{Event: "added", JobID: job})
	waitFor(t, "A's new id on the new row", func() bool { return st.NotificationMsgs(job)[keyA] != "" })
	if row := st.NotificationMsgs(job); row[keyB] != "" {
		t.Errorf("the re-added job's row = %v, want no id for B — %s is the deleted job's message", row, row[keyB])
	}

	// Once B reaches its drop, its own first message for the new job is
	// stored like any other.
	release()
	waitFor(t, "B's new id on the new row", func() bool { return st.NotificationMsgs(job)[keyB] == "M2" })
}

// RetainJobs' steps read the list later, on each target's goroutine, so the
// list is the one the caller handed over, not whatever its map holds by then.
//
// Mutant: RetainJobs handing the steps the caller's map — the job added to it
// after the call survives the drop.
func TestRetainJobsReadsTheListItWasGiven(t *testing.T) {
	f, release := gated(t, 1) // another job's request holds the FIFO
	m := editManager(t, f, newMemStore(), nil)
	const job = "dQw4w9WgXcQ"
	m.Send("Found", "x", TypeInfo, nil, SendOptions{Event: "found", JobID: job})
	if !waitCalls(t, f, 1, 3*time.Second) {
		t.Fatal("the found POST never reached the server")
	}
	m.Send("Found", "x", TypeInfo, nil, SendOptions{Event: "found", JobID: "otherJob123"})
	if !waitCalls(t, f, 2, 3*time.Second) {
		t.Fatal("the other job's POST never reached the server")
	}
	live := map[string]struct{}{"otherJob123": {}}
	m.RetainJobs(live)
	live[job] = struct{}{} // the caller's map, edited after the call
	release()
	drain(t, m)

	tr := m.tracker()
	waitFor(t, "the step", func() bool {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		_, held := tr.jobs[job]
		return !held
	})
}

// A webhook removed from the config while the job's POST is in flight leaves
// a retired queue finishing that delivery. The drop goes onto that queue too,
// and its discard runs it after the delivery, so the id the POST brings back
// does not outlive the delete — the webhook put back and the video re-added
// open a new message.
//
// Mutants: forgetInOrder skipping m.retiring, or pop's discard dropping the
// queued steps with the messages — the re-added job PATCHes M0.
func TestADropReachesARetiredTargetsInFlightPost(t *testing.T) {
	f, release := gated(t, 0)
	st := newMemStore()
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)
	installTargets(t, m, editTarget(f, nil, ModeEdit))
	retired := m.targets[0]
	const job = "dQw4w9WgXcQ"

	m.Send("Found", "x", TypeInfo, nil, SendOptions{Event: "found", JobID: job})
	if !waitCalls(t, f, 1, 3*time.Second) {
		t.Fatal("the found POST never reached the server")
	}
	m.applyTargets(nil) // the webhook is removed mid-POST
	st.mu.Lock()
	st.missing[job] = true
	st.mu.Unlock()
	m.ForgetJob(job)
	release()
	select {
	case <-retired.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the retired queue never finished its in-flight delivery")
	}

	installTargets(t, m, editTarget(f, nil, ModeEdit)) // put back
	st.mu.Lock()
	delete(st.missing, job)
	st.mu.Unlock()
	run(t, m, job, "added")
	if !waitCalls(t, f, 2, 3*time.Second) {
		t.Fatalf("want 2 requests, got %d", len(f.calls()))
	}
	if c := f.calls()[1]; c.Method != http.MethodPost {
		t.Errorf("the re-added job's first event was %s %s, want a POST of a new message", c.Method, c.Path)
	}
}

// Once a target's goroutine has returned — after Wait at shutdown — nothing
// queued on it would ever run, so the drop for that target happens at once
// rather than waiting on a queue nobody drains.
//
// Mutants: enqueueControl accepting a step on an exited queue, pop's closing
// exit not marking the queue exited, or ForgetJob / RetainJobs skipping their
// at-once drop / retain — the entry is never dropped.
func TestADropAfterShutdownHappensAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delete func(m *Manager, jobID string)
	}{
		{"ForgetJob", func(m *Manager, jobID string) { m.ForgetJob(jobID) }},
		{"RetainJobs", func(m *Manager, _ string) { m.RetainJobs(map[string]struct{}{}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDiscord(t, createdInOrder)
			st := newMemStore()
			m := editManager(t, f, st, nil)
			const job = "dQw4w9WgXcQ"
			run(t, m, job, "found")
			waitFor(t, "id persisted", func() bool { return st.NotificationMsgs(job) != nil })
			m.Wait()

			tc.delete(m, job)
			if n := m.tracker().trackedJobs(); n != 0 {
				t.Errorf("tracker holds %d jobs after the delete on a drained manager, want 0", n)
			}
		})
	}
}

// A queued step is not a message: the cap does not shed it, and a retired
// target's discard runs it rather than throwing it away with the deliveries.
//
// Mutants: enqueueControl honouring the cap — the step is not queued; pop's
// discard dropping steps — the step never runs; enqueueControl ignoring
// exited — the last step is accepted by a queue nobody drains.
func TestAQueueStepIsNeverShedOrDiscarded(t *testing.T) {
	rec := &recordingSender{}
	q := newTargetQueue(notificationTarget{sender: rec}, testLogger{}, nil, nil)
	for range notificationQueueCap {
		q.enqueue(queued{msg: One("Download Failed", "", 0, nil, SendOptions{Event: "error"}), tier: TierNormal})
	}
	var ran atomic.Bool
	if !q.enqueueControl(func() { ran.Store(true) }) {
		t.Fatal("a step was refused by a queue that is still running")
	}
	if got := q.pending(); got != notificationQueueCap+1 {
		t.Fatalf("pending = %d after a step on a full queue, want %d", got, notificationQueueCap+1)
	}

	q.stopDiscard()
	go q.run()
	select {
	case <-q.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the retired queue never exited")
	}
	if !ran.Load() {
		t.Error("the retired queue's discard threw its queued step away")
	}
	if got := rec.titles(); len(got) != 0 {
		t.Errorf("the retired queue delivered %d messages, want none", len(got))
	}
	if q.enqueueControl(func() {}) {
		t.Error("a step was accepted by a queue whose goroutine has returned")
	}
}

// A delete queues one step per job on every edit-mode target, and the cap
// counted them as messages: a batch delete of a few hundred jobs while a
// delivery was held (a rate-limit wait, a slow Discord) made the queue "full",
// and the next alert was shed in their place. The cap counts messages only.
//
// Mutant: enqueue comparing len(q.items) against the cap — the alert after the
// deletes is shed.
func TestDeleteStepsDoNotShedAnAlert(t *testing.T) {
	f, release := gated(t, 0)
	m := editManager(t, f, newMemStore(), nil)

	m.Send("Download Failed", "x", TypeError, nil, SendOptions{Event: "error", JobID: "jobBefore"})
	if !waitCalls(t, f, 1, 3*time.Second) {
		t.Fatal("the first alert never reached the server")
	}
	for i := range notificationQueueCap { // the Web UI's batch Delete
		m.ForgetJob(fmt.Sprintf("finishedJob%03d", i))
	}
	m.Send("Download Failed", "x", TypeError, nil, SendOptions{Event: "error", JobID: "jobAfter"})
	release()

	if !waitCalls(t, f, 2, 3*time.Second) {
		t.Errorf("the alert sent after the batch delete was never delivered: %v", requestLines(f.calls()))
	}
}

// A separate-mode target never reads or records a message id, so a delete
// queues it no step. A target flipped out of edit mode keeps getting them
// until a delivery starts under the new mode: the one in flight at the flip
// is still on the edit path, and its POST can still record an id.
//
// Mutants: editKeys ignoring editing — the separate-mode target gets a step;
// setDispatch clearing editing on a separate-mode bind — the flipped target's
// in-flight edit gets none; dispatchFor not clearing it — the flipped target
// gets steps for good.
func TestOnlyATargetThatCanHoldEditStateGetsADeleteStep(t *testing.T) {
	t.Run("separate mode", func(t *testing.T) {
		f, release := gated(t, 0)
		m := &Manager{logger: testLogger{}}
		installTargets(t, m, editTarget(f, nil, ModeSeparate))
		m.Send("Download Failed", "x", TypeError, nil, SendOptions{Event: "error", JobID: "otherJob123"})
		if !waitCalls(t, f, 1, 3*time.Second) {
			t.Fatal("the alert never reached the server")
		}
		m.ForgetJob("dQw4w9WgXcQ")
		m.RetainJobs(map[string]struct{}{})
		if n := m.targets[0].pending(); n != 0 {
			t.Errorf("a separate-mode target holds %d queued steps after a delete, want 0", n)
		}
		release()
	})

	t.Run("flipped out of edit mode", func(t *testing.T) {
		f, release := gated(t, 0)
		m := editManager(t, f, newMemStore(), nil)
		q := m.targets[0]
		m.Send("Found", "x", TypeInfo, nil, SendOptions{Event: "found", JobID: "dQw4w9WgXcQ"})
		if !waitCalls(t, f, 1, 3*time.Second) {
			t.Fatal("the found POST never reached the server")
		}
		installTargets(t, m, editTarget(f, nil, ModeSeparate)) // flipped mid-POST
		if len(q.editKeys()) == 0 {
			t.Error("a target flipped to separate mode while an edit was in flight gets no delete step")
		}
		release()
		run(t, m, "otherJob123", "error") // the first delivery under the new mode
		if keys := q.editKeys(); len(keys) != 0 {
			t.Errorf("a target delivering in separate mode still gets delete steps for %v", keys)
		}
	})
}
