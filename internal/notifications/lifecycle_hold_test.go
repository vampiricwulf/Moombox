package notifications

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// touchOthers touches n other jobs' entries under key, as the dispatches of n
// other jobs' lifecycle events do — a backfill re-scan's `found`s on an
// edit-mode target, which never batches, are one each.
func touchOthers(tr *lifecycleTracker, n int, key string) {
	for i := range n {
		tr.messageID(fmt.Sprintf("other%04d", i), key)
	}
}

// isHeld reports whether a queued send still pins jobID's entry.
func isHeld(tr *lifecycleTracker, jobID string) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.held[jobID] > 0
}

// A job's tracker entry is evicted past maxTrackedJobs and rebuilt from the
// row on its next touch — unless the row is gone by then. Deleting an active
// job is cancel, wait, delete, so its "cancelled" is queued when the row goes;
// with the entry evicted, before that cancel was queued or while it waited
// behind a busy FIFO, the cancel found neither the in-memory id nor the row
// and posted plain, and the lifecycle message read "Downloading" for good. A
// lifecycle event queued ahead of it opened a second message the same way.
// A queued managed send now pins its job's entry from enqueue, loaded while
// the row is still there, until it leaves the queue.
//
// Mutants: pinLifecycle pinning nothing — every row fails; hold counting the
// pin without loading the entry — the first row posts plain; evictLocked
// passing over no held job — the last two rows fail.
func TestAnEvictedEntryStillClosesTheDeletedJobsMessage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event string
		// evictBefore evicts the entry before the send is queued, otherwise
		// while it waits.
		evictBefore bool
		// wantCalls is how many requests the queued send makes: a terminal
		// edit posts its separate embed after it.
		wantCalls int
	}{
		{"a cancel, evicted before it was queued", "cancelled", true, 2},
		{"a cancel, evicted while it waited", "cancelled", false, 2},
		{"a lifecycle event, evicted while it waited", "muxing", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, release := gated(t, 2) // another job's delivery is slow: the FIFO is busy
			st := newMemStore()
			m := editManager(t, f, st, nil)
			tr := m.tracker()
			key := targetMsgKey(f.URL())
			const job = "dQw4w9WgXcQ"

			run(t, m, job, "found", "downloading")
			waitFor(t, "id persisted", func() bool { return st.NotificationMsgs(job) != nil })
			if tc.evictBefore {
				touchOthers(tr, maxTrackedJobs, key)
			}

			m.Send("Found", "x", TypeInfo, nil, SendOptions{Event: "found", JobID: "otherJob123"})
			if !waitCalls(t, f, 3, 3*time.Second) {
				t.Fatal("the other job's POST never reached the server")
			}
			m.Send(titleFor(tc.event), "desc", typeFor(tc.event), nil, SendOptions{Event: tc.event, JobID: job})
			if !tc.evictBefore {
				touchOthers(tr, maxTrackedJobs, key)
			}
			deleteRow(st, job)
			m.ForgetJob(job)
			release()

			if !waitCalls(t, f, 3+tc.wantCalls, 3*time.Second) {
				t.Fatalf("after the delete: %v, want %d requests", requestLines(f.calls()[3:]), tc.wantCalls)
			}
			if c := f.calls()[3]; c.Method != http.MethodPatch || c.Path != "/messages/M0" {
				t.Errorf("the deleted job's queued %s was %s %s, want a PATCH of its lifecycle message M0", tc.event, c.Method, c.Path)
			}
			drain(t, m)
			if isHeld(tr, job) {
				t.Error("the job is still pinned once its send has gone")
			}
		})
	}
}

// pinnedQueue is an edit-mode target's queue bound to m as applyTargets binds
// one — its sends pin m's tracker — with no goroutine draining it and a
// dispatch that delivers nothing: the test moves every item itself.
func pinnedQueue(m *Manager) *targetQueue {
	tgt := notificationTarget{
		sender: &DiscordWebhook{URL: "https://discord.com/api/webhooks/1/TOKEN"},
		key:    "https://discord.com/api/webhooks/1/TOKEN",
		mode:   ModeEdit,
		msgKey: targetMsgKey("https://discord.com/api/webhooks/1/TOKEN"),
	}
	q := newTargetQueue(tgt, testLogger{}, nil, nil)
	q.setDispatch(tgt, func(Message, bool) error { return nil },
		func(msg Message) func() { return m.pinLifecycle(tgt, msg) })
	return q
}

// sendTo hands q one send, as Manager.Send does.
func sendTo(q *targetQueue, event, jobID string) {
	q.batch.Add(One(titleFor(event), "d", 0, nil, SendOptions{Event: event, JobID: jobID}).Embeds[0], "", nil)
}

// A pin no queue lets go of keeps its job's entry for good, so every way a
// pinned item leaves the queue lets go of it — and only the item that left:
// the rest of the queue keeps its jobs.
//
// Mutants: deliver, enqueue's shutdown refusal, its shed of the oldest
// low-priority item, its drop of an arrival that finds the queue full of
// alerts, or pop's discard not letting go — that row's job stays pinned.
func TestEveryWayOutOfTheQueueLetsGoOfItsPin(t *testing.T) {
	t.Run("delivered", func(t *testing.T) {
		m := &Manager{logger: testLogger{}}
		q := pinnedQueue(m)
		sendTo(q, "downloading", "job")
		if !isHeld(m.tracker(), "job") {
			t.Fatal("a queued managed send pinned nothing")
		}
		it, ok, _ := q.pop()
		if !ok {
			t.Fatal("nothing queued")
		}
		q.deliver(it)
		if isHeld(m.tracker(), "job") {
			t.Error("a delivered send's job is still pinned")
		}
	})
	t.Run("refused at shutdown", func(t *testing.T) {
		m := &Manager{logger: testLogger{}}
		q := pinnedQueue(m)
		q.closeDrain()
		sendTo(q, "cancelled", "job")
		if isHeld(m.tracker(), "job") {
			t.Error("a refused send's job is still pinned")
		}
	})
	t.Run("shed as the oldest low-priority item", func(t *testing.T) {
		m := &Manager{logger: testLogger{}}
		q := pinnedQueue(m)
		for i := range notificationQueueCap {
			sendTo(q, "found", fmt.Sprintf("found%03d", i))
		}
		sendTo(q, "cancelled", "job")
		if isHeld(m.tracker(), "found000") {
			t.Error("the shed send's job is still pinned")
		}
		if !isHeld(m.tracker(), "found001") || !isHeld(m.tracker(), "job") {
			t.Error("a send still queued lost its pin")
		}
	})
	t.Run("dropped on arrival at a queue full of alerts", func(t *testing.T) {
		m := &Manager{logger: testLogger{}}
		q := pinnedQueue(m)
		for i := range notificationQueueCap {
			sendTo(q, "downloading", fmt.Sprintf("dl%03d", i))
		}
		sendTo(q, "cancelled", "job")
		if isHeld(m.tracker(), "job") {
			t.Error("the dropped arrival's job is still pinned")
		}
		if !isHeld(m.tracker(), "dl000") {
			t.Error("a send still queued lost its pin")
		}
	})
	t.Run("discarded with a removed target", func(t *testing.T) {
		m := &Manager{logger: testLogger{}}
		q := pinnedQueue(m)
		sendTo(q, "downloading", "job")
		q.stopDiscard()
		if _, _, exit := q.pop(); !exit {
			t.Fatal("the removed target's queue did not exit")
		}
		if isHeld(m.tracker(), "job") {
			t.Error("a discarded send's job is still pinned")
		}
	})
}

// Only a send dispatchOne manages pins: a separate-mode target never reads the
// entry, and a pin there would cost a store read and an unevictable slot for
// nothing.
//
// Mutant: pinLifecycle skipping managesLifecycle — the separate-mode send and
// the jobless one pin.
func TestOnlyAManagedSendPins(t *testing.T) {
	m := &Manager{logger: testLogger{}}
	tgt := notificationTarget{
		sender: &DiscordWebhook{URL: "https://discord.com/api/webhooks/1/TOKEN"},
		mode:   ModeSeparate,
		msgKey: targetMsgKey("https://discord.com/api/webhooks/1/TOKEN"),
	}
	msg := func(event, jobID string) Message {
		return One("t", "d", 0, nil, SendOptions{Event: event, JobID: jobID})
	}
	if letGo := m.pinLifecycle(tgt, msg("cancelled", "job")); letGo != nil {
		t.Error("a separate-mode target's send pinned its job")
	}
	tgt.mode = ModeEdit
	if letGo := m.pinLifecycle(tgt, msg("auth", "job")); letGo != nil {
		t.Error("a send no lifecycle message carries pinned its job")
	}
	if letGo := m.pinLifecycle(tgt, msg("cancelled", "")); letGo != nil {
		t.Error("a jobless send pinned something")
	}
	if n := m.tracker().heldJobs(); n != 0 {
		t.Errorf("held jobs = %d, want 0", n)
	}
	letGo := m.pinLifecycle(tgt, msg("cancelled", "job"))
	if letGo == nil || !isHeld(m.tracker(), "job") {
		t.Fatal("an edit-mode target's cancel did not pin its job")
	}
	letGo()
	letGo() // a second call is a no-op, not a second release
	if n := m.tracker().heldJobs(); n != 0 {
		t.Errorf("held jobs after letting go = %d, want 0", n)
	}
}

// The cap still holds once the pins are gone, and while every entry but the
// one being created is pinned, that one is kept rather than evicted out from
// under its caller, which would lose what it records there.
//
// Mutants: evictLocked evicting keep — the fresh entry is gone; evictLocked
// passing over every job, held or not — the map stays past the cap.
func TestEvictionPassesOverHeldJobsAndTheEntryBeingCreated(t *testing.T) {
	tr := newLifecycleTracker(newMemStore())
	letGos := make([]func(), 0, maxTrackedJobs)
	for i := range maxTrackedJobs {
		letGos = append(letGos, tr.hold(fmt.Sprintf("held%04d", i)))
	}
	tr.appendHistory("fresh", "k", "Found", time.Unix(1, 0))
	if n := tr.trackedJobs(); n != maxTrackedJobs+1 {
		t.Errorf("tracked jobs = %d, want every held one and the fresh one, %d", n, maxTrackedJobs+1)
	}
	tr.mu.Lock()
	fresh := tr.jobs["fresh"]
	tr.mu.Unlock()
	if fresh == nil || len(fresh.history["k"]) != 1 {
		t.Error("the entry being created was evicted, and the History it recorded with it")
	}

	for _, letGo := range letGos {
		letGo()
	}
	tr.messageID("another", "k")
	if n := tr.trackedJobs(); n > maxTrackedJobs {
		t.Errorf("tracked jobs once the pins are gone = %d, want <= %d", n, maxTrackedJobs)
	}
}
