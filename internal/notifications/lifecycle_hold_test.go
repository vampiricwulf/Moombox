package notifications

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
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

// isTracked reports whether the tracker holds an entry for jobID.
func isTracked(tr *lifecycleTracker, jobID string) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.jobs[jobID] != nil
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

// A delivered terminal edit releases its job's entry, and a send queued behind
// it on the same FIFO is dispatched after that. Released at once, the entry
// was gone before the pinned send read it, and the delete had taken the row it
// would be rebuilt from: an "error" delivered while a Retry's "downloading"
// and the delete's "cancelled" waited behind it left the downloading to open a
// second message, which the cancel then closed, and the job's own message read
// "Failed" for good — or, with nothing queued between them, the cancel posted
// plain. release keeps a held job's entry until the last pin lets go.
//
// Mutant: release deleting a held job's entry — both rows post where they
// should PATCH M0.
func TestAReleaseKeepsTheEntryAQueuedSendHolds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []string
		want   []string // the requests after the delete
	}{
		{
			"an error, then the delete's cancel",
			[]string{"error", "cancelled"},
			[]string{"PATCH /messages/M0", "POST /", "PATCH /messages/M0", "POST /"},
		},
		{
			"an error, a Retry's downloading, then the delete's cancel",
			[]string{"error", "downloading", "cancelled"},
			[]string{"PATCH /messages/M0", "POST /", "PATCH /messages/M0", "PATCH /messages/M0", "POST /"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, release := gated(t, 2) // another job's delivery is slow: the FIFO is busy
			st := newMemStore()
			m := editManager(t, f, st, nil)
			tr := m.tracker()
			const job = "dQw4w9WgXcQ"

			run(t, m, job, "found", "downloading")
			waitFor(t, "id persisted", func() bool { return st.NotificationMsgs(job) != nil })
			m.Send("Found", "x", TypeInfo, nil, SendOptions{Event: "found", JobID: "otherJob123"})
			if !waitCalls(t, f, 3, 3*time.Second) {
				t.Fatal("the other job's POST never reached the server")
			}
			for _, e := range tc.events {
				m.Send(titleFor(e), "desc", typeFor(e), nil, SendOptions{Event: e, JobID: job})
			}
			deleteRow(st, job)
			m.ForgetJob(job)
			release()

			if !waitCalls(t, f, 3+len(tc.want), 3*time.Second) {
				t.Fatalf("after the delete: %v, want %v", requestLines(f.calls()[3:]), tc.want)
			}
			drain(t, m)
			if got := requestLines(f.calls()[3:]); !slices.Equal(got, tc.want) {
				t.Errorf("after the delete: %v, want %v — every queued send of the job edits its message M0", got, tc.want)
			}
			waitFor(t, "the job's entry let go", func() bool { return !isHeld(tr, job) && !isTracked(tr, job) })
		})
	}
}

// A release deferred for a queued send is finished by the last pin to let go,
// so a job whose sends were all delivered leaves no entry behind — the bound
// release exists for. An entry no release closed stays: a create that failed
// keeps the History its next event's message starts from.
//
// Mutants: the last pin not finishing the release — the first two rows keep
// the entry; dropIfClosedLocked dropping an entry no release closed — the
// failed create's History is gone from the next event's message.
func TestTheLastPinLetsAClosedEntryGo(t *testing.T) {
	const job = "dQw4w9WgXcQ"
	for _, tc := range []struct {
		name   string
		events []string
	}{
		{"a finished story", []string{"found", "downloading", "finished"}},
		{"a terminal event with no message open", []string{"error"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDiscord(t, createdInOrder)
			m := editManager(t, f, newMemStore(), nil)
			run(t, m, job, tc.events...)
			waitFor(t, "every send's pin let go", func() bool { return m.tracker().heldJobs() == 0 })
			if n := m.tracker().trackedJobs(); n != 0 {
				t.Errorf("tracked jobs once every send was delivered = %d, want 0", n)
			}
		})
	}
	t.Run("a create that failed", func(t *testing.T) {
		f := newFakeDiscord(t, func(n int, r recordedReq, rw http.ResponseWriter) {
			if n == 0 {
				rw.WriteHeader(http.StatusBadRequest)
				io.WriteString(rw, `{"message":"Invalid Form Body","code":50035}`)
				return
			}
			createdInOrder(n, r, rw)
		})
		m := editManager(t, f, newMemStore(), nil)
		run(t, m, job, "found")
		waitFor(t, "the found's pin let go", func() bool { return m.tracker().heldJobs() == 0 })
		run(t, m, job, "downloading")
		if !waitCalls(t, f, 2, 3*time.Second) {
			t.Fatalf("requests = %v, want the failed create and the next one", requestLines(f.calls()))
		}
		if h := historyValue(f.calls()[1].Body); strings.Count(h, "\n")+1 != 2 {
			t.Errorf("the next create's History = %q, want the failed found's line and its own", h)
		}
	})
}

// A Retry edits the message its terminal edit closed, so the lookup opens the
// target's story again. Left closed, an entry a release kept for the Retry's
// queued send went when that send's pin let go, and another target's release
// took a kept entry while this target was mid-story: either way the job's next
// event began its History again. A second message (extraMsg) the lookup
// returns opens with it, so one whose closing edit then fails keeps the entry
// as it would on a first close.
//
// Mutants: messageID not clearing the target's closed mark — the first two
// rows render a one-line History; not clearing an old-spelling key's — the
// third row loses the entry with that message still open.
func TestARetryOpensTheStoryItsTerminalEditClosed(t *testing.T) {
	const job = "dQw4w9WgXcQ"
	t.Run("a Retry queued behind the error", func(t *testing.T) {
		f, release := gated(t, 2) // another job's delivery is slow: the FIFO is busy
		st := newMemStore()
		m := editManager(t, f, st, nil)
		run(t, m, job, "found", "downloading")
		waitFor(t, "id persisted", func() bool { return st.NotificationMsgs(job) != nil })
		m.Send("Found", "x", TypeInfo, nil, SendOptions{Event: "found", JobID: "otherJob123"})
		if !waitCalls(t, f, 3, 3*time.Second) {
			t.Fatal("the other job's POST never reached the server")
		}
		for _, e := range []string{"error", "downloading"} {
			m.Send(titleFor(e), "desc", typeFor(e), nil, SendOptions{Event: e, JobID: job})
		}
		release()
		if !waitCalls(t, f, 6, 3*time.Second) { // error's edit and post, downloading's edit
			t.Fatalf("requests = %v", requestLines(f.calls()))
		}
		waitFor(t, "the Retry's pin let go", func() bool { return m.tracker().heldJobs() == 0 })
		run(t, m, job, "muxing")
		if !waitCalls(t, f, 7, 3*time.Second) {
			t.Fatalf("requests = %v", requestLines(f.calls()))
		}
		last := f.calls()[6]
		if last.Method != http.MethodPatch || last.Path != "/messages/M0" {
			t.Fatalf("the muxing was %s %s, want a PATCH of M0", last.Method, last.Path)
		}
		if h := historyValue(last.Body); strings.Count(h, "\n")+1 != 2 {
			t.Errorf("the muxing's History = %q, want the Retry's two states", h)
		}
	})

	t.Run("another target's release", func(t *testing.T) {
		fa := newFakeDiscord(t, okCreated("MA"))
		fb := newFakeDiscord(t, okCreated("MB"))
		m := &Manager{logger: testLogger{}}
		m.SetMessageStore(newMemStore())
		tgtA := notificationTarget{sender: &DiscordWebhook{URL: fa.URL()}, mode: ModeEdit, msgKey: targetMsgKey(fa.URL())}
		tgtB := notificationTarget{sender: &DiscordWebhook{URL: fb.URL()}, mode: ModeEdit, msgKey: targetMsgKey(fb.URL())}
		send := func(tgt notificationTarget, event string) {
			t.Helper()
			if err := m.dispatchOne(tgt, One("t", "d", 0, nil, SendOptions{Event: event, JobID: job}), false); err != nil {
				t.Fatalf("%s: %v", event, err)
			}
		}
		send(tgtA, "found")
		send(tgtB, "found")
		send(tgtA, "error")       // A closes; B, still open, keeps the entry
		send(tgtA, "downloading") // the Retry edits A's message MA again
		send(tgtB, "error")       // B closes, and A is mid-story
		send(tgtA, "muxing")
		last := fa.calls()[len(fa.calls())-1]
		if last.Method != http.MethodPatch || last.Path != "/messages/MA" {
			t.Fatalf("A's muxing was %s %s, want a PATCH of MA", last.Method, last.Path)
		}
		if h := historyValue(last.Body); strings.Count(h, "\n")+1 != 2 {
			t.Errorf("A's muxing History = %q, want the Retry's two states", h)
		}
	})

	t.Run("a second message whose closing edit failed", func(t *testing.T) {
		const id, tok = "123456789012345678", "abcdefTOKEN"
		plain := "https://discord.com/api/webhooks/" + id + "/" + tok
		slashEdits := 0
		fa := newFakeDiscord(t, func(n int, r recordedReq, rw http.ResponseWriter) {
			if r.Path == "/messages/M_SLASH" {
				if slashEdits++; slashEdits == 3 { // the Retry's terminal edit
					rw.WriteHeader(http.StatusForbidden)
					io.WriteString(rw, `{"message":"Missing Permissions","code":50013}`)
					return
				}
			}
			okCreated("NEW")(n, r, rw)
		})
		fb := newFakeDiscord(t, okCreated("NEW"))
		tgtA := legacyKeyTarget(t, fa, plain, plain+"/")
		tgtB := notificationTarget{sender: &DiscordWebhook{URL: fb.URL()}, mode: ModeEdit, msgKey: targetMsgKey(fb.URL())}
		st := newMemStore()
		st.rows[job] = map[string]string{
			targetMsgKey(plain): "M_PLAIN", targetMsgKey(plain + "/"): "M_SLASH", tgtB.msgKey: "M_B",
		}
		m := &Manager{logger: testLogger{}}
		m.SetMessageStore(st)
		send := func(tgt notificationTarget, event string) error {
			return m.dispatchOne(tgt, One("t", "d", 0, nil, SendOptions{Event: event, JobID: job}), false)
		}
		for _, ev := range []string{"finished", "downloading"} {
			if err := send(tgtA, ev); err != nil {
				t.Fatalf("A's %s: %v", ev, err)
			}
		}
		if err := send(tgtA, "finished"); err == nil {
			t.Fatal("the refused edit of the second message was reported as success")
		}
		if err := send(tgtB, "finished"); err != nil {
			t.Fatalf("B's finished: %v", err)
		}
		if n := m.tracker().trackedJobs(); n != 1 {
			t.Errorf("tracked jobs = %d, want the job kept while its second message is still open", n)
		}
	})
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
