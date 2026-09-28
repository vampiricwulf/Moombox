package notifications

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// editManager builds a Manager with one edit-mode target pointed at f, plus
// the given event filter (nil = all events).
//
// buildTargets accepts only Discord-shaped URLs and httptest hands out
// http://127.0.0.1:PORT, so the target is installed directly rather than
// through a config — the SAME shape buildTargets produces. Installing it goes
// through N1's own seam (installTargets below wraps applyTargets), because a
// bare `m.targets = …` leaves a queue nothing is draining.
//
// newTestManagerWithClockAndLogger / newTestManager(t, 5*time.Second,
// targets...) (manager_test.go:37, :64) are the package's existing equivalents
// for the FIRST install and do the same applyTargets + cleanup; installTargets
// is what the later, mode-flipping installs need.
func editManager(t *testing.T, f *fakeDiscord, st MessageStore, events []string) *Manager {
	t.Helper()
	m := &Manager{logger: testLogger{}}
	if st != nil {
		m.SetMessageStore(st)
	}
	installTargets(t, m, editTarget(f, events, ModeEdit))
	return m
}

// editTarget is one notificationTarget aimed at the fake.
func editTarget(f *fakeDiscord, events []string, mode string) notificationTarget {
	return notificationTarget{
		sender: &DiscordWebhook{URL: f.URL()},
		events: eventSet(events),
		// key is what applyTargets diffs on. WITHOUT it every install builds a
		// brand-new queue and never retires the old one (applyTargets' own doc
		// comment says so), so a mode-flip test would silently exercise the
		// new-target path instead of the survivor path the operator hits.
		key:    f.URL(),
		mode:   mode,
		msgKey: targetMsgKey(f.URL()),
	}
}

// installTargets replaces the manager's targets through applyTargets — the
// inner half of Reload, which builds each targetQueue, starts its goroutine
// and binds its dispatch closure. Calling it a second time with the same
// `key` exercises the SURVIVOR arm, which is the path a hot reload takes.
func installTargets(t *testing.T, m *Manager, targets ...notificationTarget) {
	t.Helper()
	m.applyTargets(targets)
	t.Cleanup(func() {
		for _, q := range m.targets {
			q.stopDiscard()
		}
	})
}

func eventSet(events []string) map[string]bool {
	if len(events) == 0 {
		return nil
	}
	m := make(map[string]bool, len(events))
	for _, e := range events {
		m[e] = true
	}
	return m
}

// run pushes a sequence of lifecycle events through the manager and waits
// for the FIFO to drain.
//
// It must NOT call m.Wait(): Wait is single-call — it closeDrain()s every
// queue, and enqueue drops with a Warn once q.closing is set, so a second
// phase of a test would deliver nothing and assert against a stale
// recording. Poll the queue depth plus the fake's call count instead.
func run(t *testing.T, m *Manager, jobID string, events ...string) {
	t.Helper()
	for _, e := range events {
		m.Send(titleFor(e), "desc", typeFor(e), []Field{{Name: "Channel", Value: "c"}},
			SendOptions{Event: e, JobID: jobID})
	}
	drain(t, m)
}

// drain waits until every target's FIFO is empty and its goroutine is idle.
func drain(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		pending := 0
		m.targetsMu.RLock()
		for _, q := range m.targets {
			q.batch.Flush()
			pending += q.pending()
		}
		m.targetsMu.RUnlock()
		if pending == 0 {
			// One more scheduler turn so the item popped last finishes.
			time.Sleep(5 * time.Millisecond)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue did not drain: %d items pending", pending)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitCalls waits for the fake to have recorded at least n requests, WITHOUT
// touching the batcher — the difference from drain, and the whole point.
//
// drain flushes every open coalescing window on each poll, so a row that uses
// it can never observe whether something else (setMode, a timer) did the
// flush: the test helper always does it first. Any row where the batcher is
// part of what is under test must wait with this instead. It reports whether
// the count arrived so the caller can phrase its own failure.
func waitCalls(t *testing.T, f *fakeDiscord, n int, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if len(f.calls()) >= n {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// The STORE assertions in this file poll with queue_test.go's waitFor rather
// than reading straight after drain. drain cannot cover them: pop() removes
// the head from q.items BEFORE deliver runs, so pending() reads 0 while the
// POST, its response parse and the store write are all still in flight — with
// a 50 ms handler the naked read fails 5/5.

// titleFor names the embed for a test send. lifecycleLabels is the same map
// the Status field reads, so "quality_split" reads "Quality split" rather
// than the "Quality_split" a naive upper-first would produce.
func titleFor(e string) string {
	if l := lifecycleLabels[e]; l != "" {
		return l
	}
	return e
}

func typeFor(e string) NotificationType {
	switch e {
	case "error":
		return TypeError
	case "cancelled":
		return TypeCancelled
	case "finished":
		return TypeSuccess
	case "muxing":
		return TypeMuxing
	default:
		return TypeDownload
	}
}

func methods(calls []recordedReq) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Method)
	}
	return out
}

func statusValue(p discordPayload) string {
	if len(p.Embeds) == 0 {
		return ""
	}
	for _, f := range p.Embeds[0].Fields {
		if f.Name == statusFieldName {
			return f.Value
		}
	}
	return ""
}

func historyValue(p discordPayload) string {
	if len(p.Embeds) == 0 {
		return ""
	}
	for _, f := range p.Embeds[0].Fields {
		if f.Name == historyFieldName {
			return f.Value
		}
	}
	return ""
}

// TestStateMachineFirstPostsThenEdits is the spine: one POST, then a PATCH per
// later lifecycle event, with Status tracking the newest state and History
// carrying every state that reached this target.
func TestStateMachineFirstPostsThenEdits(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M1"))
	st := newMemStore()
	m := editManager(t, f, st, nil)

	run(t, m, "yt_1", "found", "scheduled", "downloading", "muxing", "finished")

	calls := f.calls()
	if got := methods(calls); len(got) != 5 || got[0] != http.MethodPost ||
		got[1] != http.MethodPatch || got[4] != http.MethodPatch {
		t.Fatalf("methods = %v, want POST then four PATCHes", got)
	}
	// The create must ask for the message back: without ?wait=true Discord
	// answers 204 with no body (API docs, resources/webhook.mdx) and there is
	// no id to edit. Pinned here too, not only on the unit tests, because this
	// is the row that claims the whole POST-then-PATCH spine works.
	if calls[0].Query != "wait=true" {
		t.Errorf("the creating POST's query = %q, want wait=true", calls[0].Query)
	}
	for i, c := range calls[1:] {
		if !strings.HasSuffix(c.Path, "/messages/M1") {
			t.Errorf("edit %d targeted %q, want .../messages/M1", i, c.Path)
		}
	}
	if got := statusValue(calls[4].Body); got != "Finished" {
		t.Errorf("final Status = %q, want Finished", got)
	}
	hist := historyValue(calls[4].Body)
	for _, want := range []string{"Found", "Scheduled", "Downloading", "Muxing", "Finished"} {
		if !strings.Contains(hist, want) {
			t.Errorf("History is missing %q: %q", want, hist)
		}
	}
	// The store write trails the wire: pending() is already 0 while the POST's
	// response is being parsed and remembered, so these two read through
	// waitFor rather than through drain's 5 ms settle.
	waitFor(t, "the created id to be stored", func() bool {
		return st.NotificationMsgs("yt_1")[targetMsgKey(f.URL())] == "M1"
	})
	waitFor(t, "the silent write", func() bool { return st.writeCount() >= 1 })
	if st.writeCount() != 1 {
		t.Errorf("silent writes = %d, want exactly 1 (one per job per target)", st.writeCount())
	}
}

// TestFilteredEventsNeitherCreateNorEdit: the allowlist semantics survive
// unchanged — the FIRST ALLOWED event creates the message, and a filtered
// event does nothing at all.
func TestFilteredEventsNeitherCreateNorEdit(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M2"))
	st := newMemStore()
	m := editManager(t, f, st, []string{"downloading", "finished"})

	run(t, m, "yt_1", "found", "scheduled", "downloading", "muxing", "finished")

	calls := f.calls()
	if got := methods(calls); len(got) != 2 || got[0] != http.MethodPost || got[1] != http.MethodPatch {
		t.Fatalf("methods = %v, want POST (downloading) then PATCH (finished)", got)
	}
	if got := statusValue(calls[0].Body); got != "Downloading" {
		t.Errorf("the creating message's Status = %q, want Downloading", got)
	}
	if hist := historyValue(calls[1].Body); strings.Contains(hist, "Muxing") {
		t.Errorf("History leaked a filtered event: %q", hist)
	}
}

// TestRestartResumesFromTheStoredID: a new Manager with the row already
// carrying an id must EDIT, not post — that is the whole point of persisting.
func TestRestartResumesFromTheStoredID(t *testing.T) {
	f := newFakeDiscord(t, okCreated("SHOULD_NOT_BE_USED"))
	st := newMemStore()
	st.rows["yt_1"] = map[string]string{targetMsgKey(f.URL()): "BOOT1"}

	m := editManager(t, f, st, nil)
	run(t, m, "yt_1", "downloading", "finished")

	calls := f.calls()
	if got := methods(calls); len(got) != 2 || got[0] != http.MethodPatch || got[1] != http.MethodPatch {
		t.Fatalf("methods = %v, want two PATCHes", got)
	}
	// EVERY edit, not just the first: "keeps editing the same message after a
	// restart" is only half-said if the second PATCH's target is unchecked.
	for i, c := range calls {
		if !strings.HasSuffix(c.Path, "/messages/BOOT1") {
			t.Errorf("edit %d targeted %q, want .../messages/BOOT1", i, c.Path)
		}
	}
	if st.writeCount() != 0 {
		t.Errorf("a resumed job wrote %d times, want 0 — the id was already stored", st.writeCount())
	}
	// A restart starts the History fresh; the message keeps being edited.
	if hist := historyValue(calls[1].Body); strings.Contains(hist, "Found") {
		t.Errorf("History claimed pre-restart states it cannot know: %q", hist)
	}
}

// TestFreshProcessWithNoIDPosts: the same job on an install that has never
// edit-delivered it simply posts.
func TestFreshProcessWithNoIDPosts(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M3"))
	m := editManager(t, f, newMemStore(), nil)
	run(t, m, "yt_1", "downloading")
	if got := methods(f.calls()); len(got) != 1 || got[0] != http.MethodPost {
		t.Fatalf("methods = %v, want one POST", got)
	}
}

// TestRemovedTargetIsIgnored: ids are keyed on the resolved URL, so a target
// the operator deleted leaves an orphaned entry that nothing ever reads — and
// a DIFFERENT webhook must not inherit it.
//
// BOTH targets are installed first and `gone` is then removed by a reload, so
// "the removed target received 0 requests" is a real statement about the
// retirement path (applyTargets → stopDiscard) rather than a tautology about a
// queue that never existed.
func TestRemovedTargetIsIgnored(t *testing.T) {
	gone := newFakeDiscord(t, okCreated("OLD"))
	live := newFakeDiscord(t, okCreated("NEW"))
	st := newMemStore()
	st.rows["yt_1"] = map[string]string{targetMsgKey(gone.URL()): "OLDMSG"}

	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)
	installTargets(t, m, editTarget(gone, nil, ModeEdit), editTarget(live, nil, ModeEdit))
	// The operator deletes the first webhook: a reload with only `live`.
	installTargets(t, m, editTarget(live, nil, ModeEdit))

	run(t, m, "yt_1", "downloading")

	if got := methods(live.calls()); len(got) != 1 || got[0] != http.MethodPost {
		t.Fatalf("the live target = %v, want one POST (it must not inherit the removed target's id)", got)
	}
	if n := len(gone.calls()); n != 0 {
		t.Errorf("the removed target received %d requests", n)
	}
	waitFor(t, "the live target's id to be stored", func() bool {
		return st.NotificationMsgs("yt_1")[targetMsgKey(live.URL())] == "NEW"
	})
	row := st.NotificationMsgs("yt_1")
	if row[targetMsgKey(gone.URL())] != "OLDMSG" {
		t.Errorf("the orphaned id was disturbed: %v", row)
	}
}

// TestTerminalEditsThenPostsSeparately (the ruling): two messages on failure —
// the lifecycle message edited to its terminal look, and the separate embed
// that carries the mention.
func TestTerminalEditsThenPostsSeparately(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M4"))
	m := editManager(t, f, newMemStore(), nil)

	run(t, m, "yt_1", "downloading", "error")

	calls := f.calls()
	if got := methods(calls); len(got) != 3 ||
		got[0] != http.MethodPost || got[1] != http.MethodPatch || got[2] != http.MethodPost {
		t.Fatalf("methods = %v, want POST, PATCH (terminal edit), POST (separate embed)", got)
	}
	if got := statusValue(calls[1].Body); got != "Failed" {
		t.Errorf("terminal edit Status = %q, want Failed", got)
	}
	if statusValue(calls[2].Body) != "" {
		t.Error("the separate embed must NOT carry the Status/History rewrite")
	}
	if calls[2].Query != "" {
		t.Errorf("the separate embed asked for the message back (query %q)", calls[2].Query)
	}
}

// TestNonLifecycleEventsNeverTouchTheMessage: auth, trim_* and System keep
// posting as they always did, even on an edit-mode target.
//
// It waits with waitCalls rather than m.Wait(), because the per-job `auth` is
// the one batchable key here (isBatchable: found/added, plus auth with a
// JobID) and a terminal Wait would flush the window for the batcher. With the
// non-flushing waiter, the edit-mode bypass at the head of batcher.Add has to
// be the thing that delivers the auth embed inside the deadline — the window
// is 5 s and nothing else opens it.
func TestNonLifecycleEventsNeverTouchTheMessage(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M5"))
	m := editManager(t, f, newMemStore(), nil)

	run(t, m, "yt_1", "downloading")
	m.Send("Authentication Required", "d", TypeWarning, nil, SendOptions{Event: "auth", JobID: "yt_1"})
	m.Send("Disk Space Warning", "d", TypeWarning, nil, SendOptions{Event: "disk_warning"})
	if !waitCalls(t, f, 3, 2*time.Second) {
		t.Fatalf("only %v arrived — an edit-mode target must not coalesce its `auth`", methods(f.calls()))
	}

	got := methods(f.calls())
	if len(got) != 3 || got[0] != http.MethodPost || got[1] != http.MethodPost || got[2] != http.MethodPost {
		t.Fatalf("methods = %v, want three POSTs (one creating, two separate)", got)
	}
}

// TestStaleIDReplacedThroughTheQueue (spec §4.5 "404 re-POSTs and re-stores",
// Review Focus 2): a stored id Discord does not know must PATCH once, re-POST,
// overwrite the id — and the job's NEXT event must edit the NEW message, not
// the forgotten one and not a third.
//
// The unit twin (TestPatch404RePostsAndOverwrites) calls dispatchOne directly
// and stops at the re-POST. This row drives the whole Manager and the
// per-target FIFO, which is the only place the "next event uses the new id"
// half can be observed.
func TestStaleIDReplacedThroughTheQueue(t *testing.T) {
	f := newFakeDiscord(t, func(n int, r recordedReq, rw http.ResponseWriter) {
		if r.Method == http.MethodPatch && strings.HasSuffix(r.Path, "/messages/STALE111") {
			// Discord's "this message no longer exists" (code 10008) — the one
			// 4xx on the edit path that is recoverable.
			rw.WriteHeader(http.StatusNotFound)
			io.WriteString(rw, `{"message":"Unknown Message","code":10008}`)
			return
		}
		okCreated("NEW777")(n, r, rw)
	})
	st := newMemStore()
	key := targetMsgKey(f.URL())
	st.rows["yt_1"] = map[string]string{key: "STALE111"}
	m := editManager(t, f, st, nil)

	run(t, m, "yt_1", "muxing", "finished")
	if !waitCalls(t, f, 3, 2*time.Second) {
		t.Fatalf("only %v arrived, want PATCH, POST, PATCH", methods(f.calls()))
	}

	calls := f.calls()
	if got := methods(calls); len(got) != 3 || got[0] != http.MethodPatch ||
		got[1] != http.MethodPost || got[2] != http.MethodPatch {
		t.Fatalf("methods = %v, want PATCH (404), POST (re-create), PATCH (the new id)", got)
	}
	if !strings.HasSuffix(calls[0].Path, "/messages/STALE111") {
		t.Errorf("the first edit targeted %q, want .../messages/STALE111", calls[0].Path)
	}
	if calls[1].Query != "wait=true" {
		t.Errorf("the re-post did not ask for the message back: query %q", calls[1].Query)
	}
	if !strings.HasSuffix(calls[2].Path, "/messages/NEW777") {
		t.Errorf("the next event edited %q, want .../messages/NEW777 — the re-posted id did not take",
			calls[2].Path)
	}
	waitFor(t, "the replacement id to be stored", func() bool {
		return st.NotificationMsgs("yt_1")[key] == "NEW777"
	})
}

// TestEditsKeepFIFOOrderAcrossARetry: with a retried PATCH in the middle, the
// later event must still land last. Before the per-target FIFO a retried embed
// could arrive after a newer one and freeze the message on a stale state.
func TestEditsKeepFIFOOrderAcrossARetry(t *testing.T) {
	old := discordRetryBackoff
	discordRetryBackoff = [discordMaxAttempts - 1]time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { discordRetryBackoff = old })

	f := newFakeDiscord(t, func(n int, r recordedReq, rw http.ResponseWriter) {
		if n == 1 { // the first PATCH (muxing) fails once
			rw.WriteHeader(http.StatusBadGateway)
			return
		}
		okCreated("M6")(n, r, rw)
	})
	m := editManager(t, f, newMemStore(), nil)

	run(t, m, "yt_1", "downloading", "muxing", "finished")

	calls := f.calls()
	if len(calls) != 4 {
		t.Fatalf("want 4 requests (POST, PATCH-502, PATCH-retry, PATCH), got %d: %v", len(calls), methods(calls))
	}
	if statusValue(calls[1].Body) != "Muxing" || statusValue(calls[2].Body) != "Muxing" {
		t.Errorf("the retry did not repeat the SAME state: %q then %q",
			statusValue(calls[1].Body), statusValue(calls[2].Body))
	}
	if statusValue(calls[3].Body) != "Finished" {
		t.Errorf("last request Status = %q, want Finished — the retry reordered the states",
			statusValue(calls[3].Body))
	}
}

// TestModeFlipMidJob (Review Focus 3): a hot reload that turns edit mode off
// must leave the stored id alone and post separately; turning it back on must
// resume editing the SAME message.
func TestModeFlipMidJob(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M7"))
	st := newMemStore()
	m := editManager(t, f, st, nil)

	run(t, m, "yt_1", "downloading")

	// Hot reload to separate — through installTargets, the same path Reload
	// takes, so the target's sender goroutine is replaced rather than having a
	// field mutated underneath it.
	installTargets(t, m, editTarget(f, nil, ModeSeparate))
	run(t, m, "yt_1", "muxing")

	// ...and back to edit.
	installTargets(t, m, editTarget(f, nil, ModeEdit))
	run(t, m, "yt_1", "finished")

	got := methods(f.calls())
	if len(got) != 3 || got[0] != http.MethodPost || got[1] != http.MethodPost || got[2] != http.MethodPatch {
		t.Fatalf("methods = %v, want POST (create), POST (separate while off), PATCH (resumed)", got)
	}
	if !strings.HasSuffix(f.calls()[2].Path, "/messages/M7") {
		t.Errorf("the resumed edit targeted %q, want .../messages/M7", f.calls()[2].Path)
	}
	waitFor(t, "the create's silent write", func() bool { return st.writeCount() >= 1 })
	if st.writeCount() != 1 {
		t.Errorf("silent writes = %d, want 1 — the flip must not re-write the id", st.writeCount())
	}

	// A window opened under the OLD mode must be DELIVERED on the flip, never
	// dropped and never folded into another job's message: arm one with a
	// `found` while the target is separate-mode and flip to edit WITHOUT
	// draining first. setMode is what flushes it.
	//
	// The wait is waitCalls, NOT drain: drain calls q.batch.Flush() on every
	// poll, so with drain here the row passes even with setMode's flush
	// deleted — the helper does the flush the row is trying to observe.
	// batchWindow is 5 s, so anything arriving inside 500 ms cannot be the
	// window's own timer either.
	//
	// Controller ruling (Task 7 review, recorded in global-constraints.md):
	// because applyTargets rebinds `dispatch` inside targetsMu and flushes
	// after the unlock, the flushed ONE-embed window goes out through the new
	// edit-mode path and BECOMES that job's editable lifecycle message. That
	// is the accepted outcome, so this row asserts it exactly — one embed,
	// ?wait=true, and an id stored for yt_2.
	before := len(f.calls())
	installTargets(t, m, editTarget(f, nil, ModeSeparate))
	m.Send("Found", "desc", TypeInfo, nil, SendOptions{Event: "found", JobID: "yt_2"})
	installTargets(t, m, editTarget(f, nil, ModeEdit)) // setMode flushes the window

	if !waitCalls(t, f, before+1, 500*time.Millisecond) {
		t.Fatalf("the open window was not flushed by the mode change within 500ms (calls=%d)", len(f.calls()))
	}
	flushed := f.calls()[before:]
	if len(flushed) != 1 {
		t.Fatalf("the flush produced %v, want exactly one message", methods(flushed))
	}
	if n := len(flushed[0].Body.Embeds); n != 1 {
		t.Errorf("the flushed window carried %d embeds — it must be its own message, not folded in", n)
	}
	if flushed[0].Method != http.MethodPost || flushed[0].Query != "wait=true" {
		t.Errorf("the flushed window went out as %s %q, want POST wait=true — a single-embed flush "+
			"becomes that job's lifecycle message", flushed[0].Method, flushed[0].Query)
	}
	waitFor(t, "the flushed window's id to be stored for yt_2", func() bool {
		return st.NotificationMsgs("yt_2")[targetMsgKey(f.URL())] == "M7"
	})
}

// TestShutdownFlushIsSingleAttempt: the whole path, not just dispatchOne —
// after BeginShutdown a queued lifecycle edit against a wedged Discord makes
// ONE request, so the owner's 10 s force-exit cap survives an edit-mode job.
//
// The one test that legitimately ends the manager, so the single trailing
// m.Wait() stays — every other test here drains instead, because Wait is
// terminal.
func TestShutdownFlushIsSingleAttempt(t *testing.T) {
	f := newFakeDiscord(t, func(n int, r recordedReq, rw http.ResponseWriter) {
		if r.Method == http.MethodPost && n == 0 {
			okCreated("MS")(n, r, rw)
			return
		}
		rw.WriteHeader(http.StatusBadGateway)
	})
	m := editManager(t, f, newMemStore(), nil)
	run(t, m, "yt_1", "downloading") // creates the message

	m.BeginShutdown()
	m.Send("Finished", "d", TypeSuccess, nil, SendOptions{Event: "finished", JobID: "yt_1"})
	m.Wait()

	if n := len(f.calls()); n != 2 {
		t.Errorf("requests = %d, want 2 (the create, then ONE shutdown edit attempt)", n)
	}
}

// TestHistoryClampInARealRun: a job that flaps for a long time must keep the
// newest states visible and shed the oldest, never exceeding the field budget.
func TestHistoryClampInARealRun(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M8"))
	m := editManager(t, f, newMemStore(), nil)

	events := []string{"downloading"}
	for range 120 {
		events = append(events, "quality_split")
	}
	events = append(events, "finished")
	run(t, m, "yt_1", events...)

	last := f.calls()[len(f.calls())-1]
	hist := historyValue(last.Body)
	if len([]rune(hist)) > historyFieldMax {
		t.Errorf("History is %d runes, want <= %d", len([]rune(hist)), historyFieldMax)
	}
	if !strings.HasSuffix(hist, "Finished") {
		t.Errorf("History does not end on the newest state: %q", hist[max(0, len(hist)-60):])
	}
	if strings.Contains(hist, "Downloading") {
		t.Error("the clamp kept the oldest line instead of dropping it")
	}
}
