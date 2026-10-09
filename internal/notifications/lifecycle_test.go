package notifications

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memStore is a MessageStore backed by a map, standing in for the jobs table.
type memStore struct {
	mu      sync.Mutex
	rows    map[string]map[string]string
	writes  int
	missing map[string]bool // job ids that answer "row gone"
}

func newMemStore() *memStore {
	return &memStore{rows: map[string]map[string]string{}, missing: map[string]bool{}}
}

func (s *memStore) NotificationMsgs(jobID string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.rows[jobID]
	if src == nil {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func (s *memStore) UpdateNotificationMsgs(jobID string, msgs map[string]string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.missing[jobID] {
		return false
	}
	cp := make(map[string]string, len(msgs))
	for k, v := range msgs {
		cp[k] = v
	}
	s.rows[jobID] = cp
	return true
}

func (s *memStore) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// TestTargetMsgKeyIsStableAndShort: the key must be derived from the RESOLVED
// webhook URL (the dedupe key buildTargets already uses), so the two spellings
// of one webhook share one message id, and it must never be the URL itself —
// the path IS the credential and this value lands in the database.
func TestTargetMsgKeyIsStableAndShort(t *testing.T) {
	const resolved = "https://discord.com/api/webhooks/123/abcTOKEN"
	k := targetMsgKey(resolved)
	if len(k) != 16 {
		t.Errorf("key length = %d, want 16", len(k))
	}
	if k != targetMsgKey(resolved) {
		t.Error("key is not stable across calls")
	}
	if strings.Contains(k, "abcTOKEN") || strings.Contains(k, "discord") {
		t.Errorf("key %q leaks the webhook URL", k)
	}
	if targetMsgKey(resolved) == targetMsgKey(resolved+"x") {
		t.Error("two different webhooks share a key")
	}
	if targetMsgKey("") != "" {
		t.Error("an empty resolved URL must yield an empty key (lifecycle disabled)")
	}
}

// TestLifecycleEventSet pins the spec's §4.2 partition. A key in the wrong half
// either silently stops mentioning (an alert folded into an edit) or doubles a
// job's message count.
func TestLifecycleEventSet(t *testing.T) {
	want := []string{
		"found", "added", "scheduled", "rescheduled", "downloading",
		"quality_split", "gap_split", "connectivity_resume",
		"connectivity_split", "muxing", "finished",
	}
	for _, e := range want {
		if !lifecycleEvents[e] {
			t.Errorf("%q must be a lifecycle event", e)
		}
	}
	for _, e := range []string{"error", "cancelled", "auth", "trim_created", "trim_deleted", "trim_error",
		"disk_warning", "disk_critical", "update_available", "update_applied", "update_failed",
		"crash_recovered", "channel_unhealthy", "connectivity_restored", "connectivity_pause"} {
		if lifecycleEvents[e] {
			t.Errorf("%q must NOT be a lifecycle event — it is posted separately", e)
		}
	}
	if len(lifecycleEvents) != len(want) {
		t.Errorf("lifecycleEvents has %d entries, want exactly %d", len(lifecycleEvents), len(want))
	}
	// Every lifecycle event and both terminal events must render a label, or
	// the Status field would read empty for that state.
	for e := range lifecycleEvents {
		if lifecycleLabels[e] == "" {
			t.Errorf("no Status label for lifecycle event %q", e)
		}
	}
	for _, e := range []string{"error", "cancelled"} {
		if lifecycleLabels[e] == "" {
			t.Errorf("no Status label for terminal event %q", e)
		}
	}
}

// TestPlanRoutesSeparateModeUntouched: with mode unset (the default) nothing
// about delivery changes — this is what keeps every N1/N2 test green.
func TestPlanRoutesSeparateModeUntouched(t *testing.T) {
	m := &Manager{logger: testLogger{}}
	tgt := notificationTarget{mode: ModeSeparate, msgKey: "abc"}
	p := m.planLifecycle(tgt, SendOptions{Event: "downloading", JobID: "yt_1"})
	if p.Manage {
		t.Error("a separate-mode target must never manage a lifecycle message")
	}
}

// TestPlanNeedsJobIDAndKey: a lifecycle send with no JobID (or a transport with
// no resolved URL) is treated as separate — spec §4.4.
func TestPlanNeedsJobIDAndKey(t *testing.T) {
	m := &Manager{logger: testLogger{}}
	for _, tc := range []struct {
		name string
		tgt  notificationTarget
		opts SendOptions
	}{
		{"no job id", notificationTarget{mode: ModeEdit, msgKey: "abc"}, SendOptions{Event: "downloading"}},
		{"no target key", notificationTarget{mode: ModeEdit}, SendOptions{Event: "downloading", JobID: "yt_1"}},
		{"non-lifecycle event", notificationTarget{mode: ModeEdit, msgKey: "abc"}, SendOptions{Event: "trim_created", JobID: "yt_1"}},
		{"no event at all (SendTest)", notificationTarget{mode: ModeEdit, msgKey: "abc"}, SendOptions{JobID: "yt_1"}},
	} {
		if p := m.planLifecycle(tc.tgt, tc.opts); p.Manage {
			t.Errorf("%s: plan managed a lifecycle message, want separate", tc.name)
		}
	}
}

// TestPlanTerminalEditsThenPosts: on error/cancelled the message is edited to
// its terminal look AND the separate embed is still posted, because the
// separate one carries the mention (owner ruling 2026-09-27).
func TestPlanTerminalEditsThenPosts(t *testing.T) {
	m := &Manager{logger: testLogger{}}
	tgt := notificationTarget{mode: ModeEdit, msgKey: "abc"}
	m.tracker().remember("yt_1", "abc", "555")

	for _, e := range []string{"error", "cancelled"} {
		p := m.planLifecycle(tgt, SendOptions{Event: e, JobID: "yt_1"})
		if !p.Manage || !p.AlsoSeparate || p.MessageID != "555" {
			t.Errorf("%s: plan = %+v, want Manage+AlsoSeparate on message 555", e, p)
		}
	}
	// With no message yet, a terminal event must NOT create one — it is only
	// ever a closing edit of a message some earlier event opened.
	p := m.planLifecycle(tgt, SendOptions{Event: "error", JobID: "yt_unknown"})
	if p.Manage {
		t.Error("a terminal event with no existing message must post separately only")
	}
}

// TestHistoryAppendsAndClamps: the History field grows one `<t:x:R> Label` line
// per allowed event and, at the field limit, drops the OLDEST lines. Dropping
// the newest would freeze the field at the first few states.
func TestHistoryAppendsAndClamps(t *testing.T) {
	tr := newLifecycleTracker(nil)
	at := time.Unix(1758960000, 0)
	got := tr.appendHistory("yt_1", "abc", "Downloading", at)
	if got != "<t:1758960000:R> Downloading" {
		t.Fatalf("first history line = %q", got)
	}
	got = tr.appendHistory("yt_1", "abc", "Muxing", at.Add(time.Minute))
	if got != "<t:1758960000:R> Downloading\n<t:1758960060:R> Muxing" {
		t.Fatalf("second history = %q", got)
	}

	// 200 lines is far past the 1000-rune budget; the newest must survive and
	// the oldest must be gone.
	for i := range 200 {
		got = tr.appendHistory("yt_1", "abc", "Quality split", at.Add(time.Duration(i)*time.Second))
	}
	if len([]rune(got)) > historyFieldMax {
		t.Errorf("history is %d runes, want <= %d", len([]rune(got)), historyFieldMax)
	}
	if strings.Contains(got, "Downloading") {
		t.Error("the clamp dropped from the wrong end — the first line survived")
	}
	if !strings.HasSuffix(got, "Quality split") {
		t.Errorf("the newest line is missing: %q", got)
	}
}

// TestLifecycleFieldsLeadWithStatusAndHistory: Status and History come FIRST,
// then the event's own fields.
//
// MUTANT: append them instead. N1's clampEmbed enforces Discord's 6000-char
// embed total by dropping TRAILING fields, so a fat producer would delete the
// History and then the Status — a lifecycle message with no state line, which
// is the one thing it exists to show. The oversized row below is what catches
// it: after the clamp, Status and History must both survive.
func TestLifecycleFieldsLeadWithStatusAndHistory(t *testing.T) {
	tr := newLifecycleTracker(nil)
	base := []Field{{Name: "Channel", Value: "c", Inline: true}}
	out := tr.rewriteFields("yt_1", "abc", "muxing", base, time.Unix(1758960000, 0))
	if len(out) != 3 {
		t.Fatalf("want 3 fields, got %d (%+v)", len(out), out)
	}
	if out[0].Name != statusFieldName || out[0].Value != "Muxing" {
		t.Errorf("field 0 = %+v, want {Status Muxing}", out[0])
	}
	if out[1].Name != historyFieldName || !strings.Contains(out[1].Value, "Muxing") {
		t.Errorf("field 1 = %+v, want the History", out[1])
	}
	if out[2].Name != "Channel" {
		t.Errorf("the event's own fields must follow, got %q", out[2].Name)
	}
	// The caller's slice must not be aliased — a producer reuses its builder.
	if len(base) != 1 {
		t.Errorf("rewriteFields mutated the caller's slice: %+v", base)
	}
}

// TestStatusAndHistorySurviveTheTotalClamp: an oversized producer field list
// (20 × 900-rune values, far past Discord's 6000-char embed total) must not
// cost the message its state line.
func TestStatusAndHistorySurviveTheTotalClamp(t *testing.T) {
	tr := newLifecycleTracker(nil)
	fat := make([]Field, 0, 20)
	for i := range 20 {
		fat = append(fat, Field{Name: fmt.Sprintf("Stat %d", i), Value: strings.Repeat("x", 900)})
	}
	out := tr.rewriteFields("yt_1", "abc", "downloading", fat, time.Unix(1758960000, 0))
	body, err := buildPayload(One("Downloading", "d", TypeDownload.Color(), out, SendOptions{Event: "downloading", JobID: "yt_1"}))
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var p discordPayload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	names := fieldNames(p)
	if len(names) < 2 || names[0] != statusFieldName || names[1] != historyFieldName {
		t.Fatalf("after the clamp the fields are %v — Status and History must lead and survive", names)
	}
}

// TestRememberWritesOncePerJobPerTarget: the ruling allows exactly one silent
// write per (job, target), on the first successful POST. A second remember for
// the same pair (or a re-read) must not write again.
func TestRememberWritesOncePerJobPerTarget(t *testing.T) {
	st := newMemStore()
	tr := newLifecycleTracker(st)

	tr.remember("yt_1", "abc", "111")
	if st.writeCount() != 1 {
		t.Fatalf("writes after first remember = %d, want 1", st.writeCount())
	}
	tr.remember("yt_1", "abc", "111")
	if st.writeCount() != 1 {
		t.Errorf("re-remembering the same id wrote again (%d writes)", st.writeCount())
	}
	// A second target for the same job is a second (job, target) pair, so it
	// does write — and it must carry BOTH keys, not replace the first.
	tr.remember("yt_1", "def", "222")
	if st.writeCount() != 2 {
		t.Errorf("writes after a second target = %d, want 2", st.writeCount())
	}
	row := st.NotificationMsgs("yt_1")
	if row["abc"] != "111" || row["def"] != "222" {
		t.Errorf("stored row = %v, want both targets", row)
	}
}

// TestLoadsFromTheStoreOnce: the row is read on the first lifecycle send for a
// job after boot and never again — a per-event SELECT on the notifier's path
// would be a database read per embed.
func TestLoadsFromTheStoreOnce(t *testing.T) {
	st := newMemStore()
	st.rows["yt_1"] = map[string]string{"abc": "999"}
	tr := newLifecycleTracker(st)

	if id, _ := tr.messageID("yt_1", "abc"); id != "999" {
		t.Fatalf("messageID = %q — the stored id was not loaded", id)
	}
	// Mutate the store behind the tracker's back: a second lookup must NOT
	// re-read it.
	st.rows["yt_1"] = map[string]string{"abc": "different"}
	if id, _ := tr.messageID("yt_1", "abc"); id != "999" {
		t.Errorf("messageID re-read the store: got %q", id)
	}
}

// TestRecordSurvivesMissingRow (Review Focus 1): the job can be deleted between
// the POST and its response. remember must keep the id in memory (so the edits
// still work for the rest of this process) and log rather than fail.
func TestRecordSurvivesMissingRow(t *testing.T) {
	st := newMemStore()
	st.missing["yt_gone"] = true
	tr := newLifecycleTracker(st)

	tr.remember("yt_gone", "abc", "111")
	if id, _ := tr.messageID("yt_gone", "abc"); id != "111" {
		t.Errorf("in-memory id lost when the row write failed: %q", id)
	}
}

// TestTrackerReleasesFinishedJobs (Review Focus 6): the tracker must not be a
// map that only grows. A delivered terminal edit drops the job's entry; the
// STORED id is untouched, so a Retry reloads it once and keeps editing the
// same message — which is exactly what the owner's "never closed" ruling asks
// for. Releasing the cache is not closing the entry.
func TestTrackerReleasesFinishedJobs(t *testing.T) {
	f := newFakeDiscord(t, okCreated("M0"))
	st := newMemStore()
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)
	tgt := notificationTarget{
		sender: &DiscordWebhook{URL: f.URL()},
		mode:   ModeEdit,
		msgKey: targetMsgKey(f.URL()),
	}

	for _, e := range []string{"downloading", "finished"} {
		if err := m.dispatchOne(tgt, One("t", "d", 0, nil, SendOptions{Event: e, JobID: "yt_1"}), false); err != nil {
			t.Fatalf("%s: %v", e, err)
		}
	}
	if n := m.tracker().trackedJobs(); n != 0 {
		t.Errorf("tracked jobs after finished = %d, want 0", n)
	}
	// The id survived in the store, so a Retry edits the SAME message.
	if got := st.NotificationMsgs("yt_1")[targetMsgKey(f.URL())]; got != "M0" {
		t.Fatalf("stored id = %q, want M0 — release must not clear the row", got)
	}
	if err := m.dispatchOne(tgt, One("t", "d", 0, nil, SendOptions{Event: "downloading", JobID: "yt_1"}), false); err != nil {
		t.Fatalf("retry: %v", err)
	}
	last := f.calls()[len(f.calls())-1]
	if last.Method != http.MethodPatch || !strings.HasSuffix(last.Path, "/messages/M0") {
		t.Errorf("the retry did not resume the same message: %s %s", last.Method, last.Path)
	}
	if st.writeCount() != 1 {
		t.Errorf("silent writes = %d, want 1 — a reload must not re-write", st.writeCount())
	}
}

// TestMessagelessTerminalLeavesNoTrackerEntry: a terminal event that opened
// nothing posts plain — and must not leave the entry the lookup created behind
// it. On an edit-mode target filtered to ["error","finished"] that is one
// tracker slot, and the store read that filled it, per failing job.
func TestMessagelessTerminalLeavesNoTrackerEntry(t *testing.T) {
	f := newFakeDiscord(t, okCreated("UNUSED"))
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(newMemStore())
	tgt := notificationTarget{
		sender: &DiscordWebhook{URL: f.URL()},
		mode:   ModeEdit,
		msgKey: targetMsgKey(f.URL()),
	}
	if err := m.dispatchOne(tgt, One("t", "d", 0, nil, SendOptions{Event: "error", JobID: "yt_1"}), false); err != nil {
		t.Fatalf("error: %v", err)
	}
	if calls := f.calls(); len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Query != "" {
		t.Fatalf("calls = %+v, want one plain POST", calls)
	}
	if n := m.tracker().trackedJobs(); n != 0 {
		t.Errorf("tracked jobs after a message-less terminal = %d, want 0", n)
	}
}

// TestSecondTargetKeepsHistoryAfterFirstTargetsRelease: release is per
// (job, TARGET), not per job. Two edit-mode targets — two Discord servers —
// both take a job's whole story; whichever one delivers its `finished` edit
// first must not take the other's History and message id with it.
//
// MUTANT: `release(jobID)` deleting the whole job entry. Target A's finished
// PATCH lands first, B's `finished` then recreates the entry from the row and
// renders a one-line History ("Finished") instead of the four states it saw —
// and with no persisted id to re-read it opens a SECOND message for the job.
func TestSecondTargetKeepsHistoryAfterFirstTargetsRelease(t *testing.T) {
	fa := newFakeDiscord(t, okCreated("MA"))
	fb := newFakeDiscord(t, okCreated("MB"))
	st := newMemStore()
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)
	tgtA := notificationTarget{sender: &DiscordWebhook{URL: fa.URL()}, mode: ModeEdit, msgKey: targetMsgKey(fa.URL())}
	tgtB := notificationTarget{sender: &DiscordWebhook{URL: fb.URL()}, mode: ModeEdit, msgKey: targetMsgKey(fb.URL())}

	send := func(tgt notificationTarget, event string) {
		t.Helper()
		if err := m.dispatchOne(tgt, One("t", "d", 0, nil, SendOptions{Event: event, JobID: "yt_1"}), false); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
	}
	// Both targets see the same three states, then A's terminal edit lands
	// first — B's FIFO was behind a retry ladder or a rate-limit sleep.
	for _, e := range []string{"found", "downloading", "muxing"} {
		send(tgtA, e)
		send(tgtB, e)
	}
	send(tgtA, "finished")
	send(tgtB, "finished")

	last := fb.calls()[len(fb.calls())-1]
	if last.Method != http.MethodPatch || !strings.HasSuffix(last.Path, "/messages/MB") {
		t.Fatalf("target B's finished = %s %s, want a PATCH of its own message MB", last.Method, last.Path)
	}
	var history string
	for _, f := range last.Body.Embeds[0].Fields {
		if f.Name == historyFieldName {
			history = f.Value
		}
	}
	if n := len(strings.Split(history, "\n")); n != 4 {
		t.Errorf("target B's terminal History has %d lines (%q), want the 4 states it saw", n, history)
	}
	// A's own story is closed, and once both are closed the entry is gone.
	if n := m.tracker().trackedJobs(); n != 0 {
		t.Errorf("tracked jobs after BOTH targets finished = %d, want 0", n)
	}
}

// TestClosedFlagClearsWhenATargetOpensANewMessage: `release` marks a target
// closed BY KEY, and a message-less terminal marks one on a job the target
// holds no message for (a filter that excludes every creating event, or a
// create that never landed). That flag must not outlive the next message the
// target opens: `release`'s closed-set loop reads it against the ids in
// `msgs`, so a stale one makes every message-holding target look closed and
// the OTHER target's terminal release takes the whole job entry — and with it
// this target's History.
//
// MUTANT: remove `delete(j.closed, key)` from `remember`. Target B's
// `finished` drops the entry although A had reopened, so A's terminal PATCH
// renders a one-line History ("Finished") instead of the three states it saw.
// Every other test in the suite stays green.
func TestClosedFlagClearsWhenATargetOpensANewMessage(t *testing.T) {
	fa := newFakeDiscord(t, okCreated("MA"))
	fb := newFakeDiscord(t, okCreated("MB"))
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(newMemStore())
	tgtA := notificationTarget{sender: &DiscordWebhook{URL: fa.URL()}, mode: ModeEdit, msgKey: targetMsgKey(fa.URL())}
	tgtB := notificationTarget{sender: &DiscordWebhook{URL: fb.URL()}, mode: ModeEdit, msgKey: targetMsgKey(fb.URL())}

	send := func(tgt notificationTarget, event string) {
		t.Helper()
		if err := m.dispatchOne(tgt, One("t", "d", 0, nil, SendOptions{Event: event, JobID: "yt_1"}), false); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
	}
	// B opens the job's message first, so the entry has an open target and A's
	// message-less terminal cannot simply drop it.
	send(tgtB, "found")
	// A's first word about this job is a failure it holds no message for: a
	// plain post, and a `closed` flag with no message behind it.
	send(tgtA, "error")
	if calls := fa.calls(); len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Query != "" {
		t.Fatalf("target A's error = %+v, want one plain POST", calls)
	}
	// A is retried and now DOES open a message on the same job, accruing its
	// own story over three states.
	send(tgtA, "downloading")
	send(tgtA, "muxing")
	// B's terminal edit closes B. A reopened, so the entry must survive.
	send(tgtB, "finished")
	if n := m.tracker().trackedJobs(); n != 1 {
		t.Errorf("tracked jobs after B's finished = %d, want 1 — A reopened and is still telling its story", n)
	}
	send(tgtA, "finished")

	last := fa.calls()[len(fa.calls())-1]
	if last.Method != http.MethodPatch || !strings.HasSuffix(last.Path, "/messages/MA") {
		t.Fatalf("target A's finished = %s %s, want a PATCH of its own message MA", last.Method, last.Path)
	}
	var history string
	for _, f := range last.Body.Embeds[0].Fields {
		if f.Name == historyFieldName {
			history = f.Value
		}
	}
	if n := len(strings.Split(history, "\n")); n != 3 {
		t.Errorf("target A's terminal History has %d lines (%q), want the 3 states it saw since it reopened", n, history)
	}
	// Both stories are closed now, so the entry goes.
	if n := m.tracker().trackedJobs(); n != 0 {
		t.Errorf("tracked jobs after BOTH targets finished = %d, want 0", n)
	}
}

// TestTrackerCapsTrackedJobs: jobs that never reach a terminal event (cancelled
// outside the notifier, deleted, filtered to mid-lifecycle keys) must not
// retain an entry each, forever, in a process that runs for months.
func TestTrackerCapsTrackedJobs(t *testing.T) {
	tr := newLifecycleTracker(newMemStore())
	for i := range maxTrackedJobs + 100 {
		tr.remember(fmt.Sprintf("yt_%d", i), "abc", "1")
	}
	if n := tr.trackedJobs(); n > maxTrackedJobs {
		t.Errorf("tracked jobs = %d, want <= %d", n, maxTrackedJobs)
	}
	// The newest must have survived the eviction, the oldest must not.
	if id, _ := tr.messageID(fmt.Sprintf("yt_%d", maxTrackedJobs+99), "abc"); id == "" {
		t.Error("the most recently touched job was evicted")
	}
}

// TestNonEditableTransportFallsBack: a target in edit mode whose transport
// cannot create-and-rewrite must PLAIN POST, not drop the event. There is no
// such transport today (Discord is the only one), which is exactly why the
// branch needs a test rather than a reader's confidence.
func TestNonEditableTransportFallsBack(t *testing.T) {
	var got int
	plain := senderFunc(func(Message) error {
		got++
		return nil
	})
	m := &Manager{logger: testLogger{}}
	tgt := notificationTarget{sender: plain, mode: ModeEdit, msgKey: "abc"}

	if err := m.dispatchOne(tgt, One("t", "d", 0, nil, SendOptions{Event: "downloading", JobID: "yt_1"}), false); err != nil {
		t.Fatalf("dispatchOne: %v", err)
	}
	if got != 1 {
		t.Errorf("plain sends = %d, want 1 — a non-editable transport must fall back, not drop", got)
	}
	if id, _ := m.tracker().messageID("yt_1", "abc"); id != "" {
		t.Error("a fallback post must not record a message id")
	}
}

// TestPatch404RePostFailureForgetsTheID: when the recovery POST itself fails,
// the error must reach the caller AND the stale id must stay forgotten, so the
// job's next event creates a message instead of PATCHing a ghost forever.
func TestPatch404RePostFailureForgetsTheID(t *testing.T) {
	f := newFakeDiscord(t, func(_ int, r recordedReq, rw http.ResponseWriter) {
		if r.Method == http.MethodPatch {
			rw.WriteHeader(http.StatusNotFound)
			io.WriteString(rw, `{"message":"Unknown Message","code":10008}`)
			return
		}
		rw.WriteHeader(http.StatusForbidden) // the re-POST is refused too
		io.WriteString(rw, `{"message":"Missing Permissions","code":50013}`)
	})
	st := newMemStore()
	st.rows["yt_1"] = map[string]string{targetMsgKey(f.URL()): "STALE"}
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)
	tgt := notificationTarget{
		sender: &DiscordWebhook{URL: f.URL()},
		mode:   ModeEdit,
		msgKey: targetMsgKey(f.URL()),
	}

	if err := m.dispatchOne(tgt, One("t", "d", 0, nil, SendOptions{Event: "muxing", JobID: "yt_1"}), false); err == nil {
		t.Fatal("a refused re-POST was reported as success")
	}
	if id, _ := m.tracker().messageID("yt_1", targetMsgKey(f.URL())); id != "" {
		t.Errorf("the stale id %q is still held — the next event would PATCH a ghost", id)
	}
	if got := st.NotificationMsgs("yt_1")[targetMsgKey(f.URL())]; got != "STALE" {
		t.Errorf("stored id = %q — a failed re-POST must not rewrite the row", got)
	}
}

// TestShutdownEditIsSingleAttempt: after the queue reports it is shutting down,
// an edit-mode lifecycle event makes exactly ONE request even against a 502 —
// the owner's 15 s force-exit cap must not be spent on a retry ladder.
func TestShutdownEditIsSingleAttempt(t *testing.T) {
	f := newFakeDiscord(t, func(_ int, _ recordedReq, rw http.ResponseWriter) {
		rw.WriteHeader(http.StatusBadGateway)
	})
	m := &Manager{logger: testLogger{}}
	tgt := notificationTarget{
		sender: &DiscordWebhook{URL: f.URL()},
		mode:   ModeEdit,
		msgKey: targetMsgKey(f.URL()),
	}
	if err := m.dispatchOne(tgt, One("t", "d", 0, nil, SendOptions{Event: "finished", JobID: "yt_1"}), true); err == nil {
		t.Fatal("a 502 was reported as success")
	}
	if n := len(f.calls()); n != 1 {
		t.Errorf("requests while shutting down = %d, want exactly 1", n)
	}
}

// TestTrackerConcurrentAccess (Review Focus 5): two jobs and two targets touch
// the tracker from different goroutines while a third reads. Run with -race.
//
// The job and the key advance on DIFFERENT strides, so the eight goroutines
// cover all four (job, target) pairs — which means one job is reached by two
// target keys at once. That is the pair the store's whole-map replacement
// races on, and crossing two jobs with one key each would never reach it.
func TestTrackerConcurrentAccess(t *testing.T) {
	tr := newLifecycleTracker(newMemStore())
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			job := "yt_" + string(rune('a'+i%2))
			key := "k" + string(rune('a'+(i/2)%2))
			for range 50 {
				tr.remember(job, key, "1")
				tr.messageID(job, key)
				tr.appendHistory(job, key, "Downloading", time.Unix(1, 0))
				tr.forget(job, key)
			}
		}(i)
	}
	wg.Wait()
}

// gateStore is a memStore whose FIRST write parks inside the store until the
// test releases it, so the interleaving that loses a target's id is produced
// on purpose rather than waited for. A timing loop does not reproduce it: the
// in-memory write is a map assignment, and 200 concurrent rounds under -race
// never once lost the race (measured).
type gateStore struct {
	*memStore
	// gated is claimed by the first writer with a CAS. NOT a sync.Once:
	// Once.Do blocks every other caller until the first returns, which would
	// park the second writer inside the gate and hide the very interleaving
	// this exists to produce.
	gated     atomic.Bool
	entered   chan struct{} // the first write has parked
	release   chan struct{} // closed to let it finish
	completed chan struct{} // a LATER write finished while the first parked
}

func newGateStore() *gateStore {
	return &gateStore{
		memStore:  newMemStore(),
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		completed: make(chan struct{}, 4),
	}
}

func (g *gateStore) UpdateNotificationMsgs(jobID string, msgs map[string]string) bool {
	parked := g.gated.CompareAndSwap(false, true)
	if parked {
		close(g.entered)
		<-g.release
	}
	ok := g.memStore.UpdateNotificationMsgs(jobID, msgs)
	if !parked {
		select {
		case g.completed <- struct{}{}:
		default:
		}
	}
	return ok
}

// TestTwoTargetsOnOneJobBothPersist (Review Focus 5, the destructive half):
// UpdateNotificationMsgs replaces the WHOLE map, so two targets recording an
// id for the SAME job concurrently must not overwrite each other. Each target
// has its own sender goroutine, so this is the ordinary two-webhook install,
// not a corner.
//
// The gate pins the losing interleaving: target A is parked INSIDE its store
// write, holding a snapshot that knows only its own id, while target B records
// the second id. Unless remember owns the whole read-modify-write, B's row
// lands first and A's parked snapshot then overwrites it — B's message id is
// gone from the database and its message is orphaned at the next restart.
//
// MUTANT: drop the writeMu serialisation in remember and this fails every run.
func TestTwoTargetsOnOneJobBothPersist(t *testing.T) {
	st := newGateStore()
	tr := newLifecycleTracker(st)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tr.remember("yt_1", "abc", "111")
	}()
	<-st.entered // target A is inside its write, holding its snapshot

	wg.Add(1)
	go func() {
		defer wg.Done()
		tr.remember("yt_1", "def", "222")
	}()
	// Give B every chance to get its row in FIRST. With the serialisation it
	// cannot even take a snapshot yet, so this waits out the timeout; without
	// it, B's write lands immediately and the wait ends on the first branch.
	select {
	case <-st.completed:
	case <-time.After(250 * time.Millisecond):
	}
	close(st.release)
	wg.Wait()

	row := st.NotificationMsgs("yt_1")
	if row["abc"] != "111" || row["def"] != "222" {
		t.Fatalf("stored row = %v, want both targets — one target's write clobbered the other's id", row)
	}
}

// TestPatch404RePostsAndOverwrites (Review Focus 2): a stored id Discord does
// not know must produce exactly one PATCH, then one POST, then a stored id
// equal to the new message — and no loop.
func TestPatch404RePostsAndOverwrites(t *testing.T) {
	f := newFakeDiscord(t, func(_ int, r recordedReq, rw http.ResponseWriter) {
		if r.Method == http.MethodPatch {
			rw.WriteHeader(http.StatusNotFound)
			io.WriteString(rw, `{"message":"Unknown Message","code":10008}`)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		io.WriteString(rw, `{"id":"NEW777"}`)
	})

	st := newMemStore()
	st.rows["yt_1"] = map[string]string{targetMsgKey(f.URL()): "STALE111"}
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)
	tgt := notificationTarget{
		sender: &DiscordWebhook{URL: f.URL()},
		mode:   ModeEdit,
		msgKey: targetMsgKey(f.URL()),
	}

	if err := m.dispatchOne(tgt, One("Muxing Starting", "d", TypeMuxing.Color(), nil,
		SendOptions{Event: "muxing", JobID: "yt_1"}), false); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	calls := f.calls()
	if len(calls) != 2 {
		t.Fatalf("want PATCH then POST (2 calls), got %d: %+v", len(calls), calls)
	}
	if calls[0].Method != http.MethodPatch || calls[1].Method != http.MethodPost {
		t.Fatalf("call order = %s,%s — want PATCH,POST", calls[0].Method, calls[1].Method)
	}
	if calls[1].Query != "wait=true" {
		t.Errorf("the re-post did not ask for the message back: query %q", calls[1].Query)
	}
	if got := st.NotificationMsgs("yt_1")[targetMsgKey(f.URL())]; got != "NEW777" {
		t.Errorf("stored id = %q, want NEW777", got)
	}
}

// TestFirstAllowedEventPostsAndStores: the creating POST carries Status and
// History already, so an operator reading the first message sees the same
// shape every later edit keeps.
func TestFirstAllowedEventPostsAndStores(t *testing.T) {
	f := newFakeDiscord(t, okCreated("ID1"))
	st := newMemStore()
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)
	tgt := notificationTarget{
		sender: &DiscordWebhook{URL: f.URL()},
		mode:   ModeEdit,
		msgKey: targetMsgKey(f.URL()),
	}

	if err := m.dispatchOne(tgt, One("Stream Found", "d", TypeInfo.Color(),
		[]Field{{Name: "Channel", Value: "c"}},
		SendOptions{Event: "found", JobID: "yt_1"}), false); err != nil {
		t.Fatalf("dispatchOne: %v", err)
	}
	calls := f.calls()
	if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Query != "wait=true" {
		t.Fatalf("first allowed event did not POST with wait=true: %+v", calls)
	}
	names := fieldNames(calls[0].Body)
	if len(names) != 3 || names[0] != "Status" || names[1] != "History" {
		t.Errorf("created embed fields = %v, want [Status History Channel]", names)
	}
	if got := st.NotificationMsgs("yt_1")[targetMsgKey(f.URL())]; got != "ID1" {
		t.Errorf("stored id = %q, want ID1", got)
	}
}

// TestLifecycleEditKeepsTheTargetMention: the ping is per MESSAGE, and an
// operator can put a lifecycle event in mention_events. Rebuilding the body
// from the embed alone would drop content/allowed_mentions and the ping would
// vanish the moment a target switched to edit mode.
func TestLifecycleEditKeepsTheTargetMention(t *testing.T) {
	f := newFakeDiscord(t, okCreated("MM1"))
	m := &Manager{logger: testLogger{}}
	tgt := notificationTarget{
		sender: &DiscordWebhook{URL: f.URL()},
		mode:   ModeEdit,
		msgKey: targetMsgKey(f.URL()),
	}
	pinged := func(event string) Message {
		out := One("t", "d", 0, nil, SendOptions{Event: event, JobID: "yt_1"})
		out.Mention = "<@&42>"
		out.MentionAllowed = &AllowedMentions{Parse: []string{}, Roles: []string{"42"}}
		return out
	}
	for _, e := range []string{"downloading", "finished"} {
		if err := m.dispatchOne(tgt, pinged(e), false); err != nil {
			t.Fatalf("%s: %v", e, err)
		}
	}
	calls := f.calls()
	if len(calls) != 2 || calls[0].Method != http.MethodPost || calls[1].Method != http.MethodPatch {
		t.Fatalf("methods = %+v, want the creating POST then a PATCH", calls)
	}
	for i, c := range calls {
		if c.Body.Content != "<@&42>" {
			t.Errorf("call %d content = %q, want the target's mention — it is message-level, so the PATCH carries it too",
				i, c.Body.Content)
		}
		if c.Body.AllowedMentions == nil {
			t.Errorf("call %d dropped allowed_mentions — an unresolvable ping renders as literal text", i)
		}
	}
}

// fieldNames pulls the field names out of the first embed of a captured body.
func fieldNames(p discordPayload) []string {
	if len(p.Embeds) == 0 {
		return nil
	}
	out := make([]string, 0, len(p.Embeds[0].Fields))
	for _, f := range p.Embeds[0].Fields {
		out = append(out, f.Name)
	}
	return out
}

// TestNoStoreStillEdits: an install without a MessageStore (the `moombox add`
// side process before its wiring, or a test literal) must still edit within the
// process — only the restart survival is lost.
func TestNoStoreStillEdits(t *testing.T) {
	f := newFakeDiscord(t, okCreated("ID9"))
	m := &Manager{logger: testLogger{}}
	tgt := notificationTarget{
		sender: &DiscordWebhook{URL: f.URL()},
		mode:   ModeEdit,
		msgKey: targetMsgKey(f.URL()),
	}
	for _, e := range []string{"downloading", "muxing", "finished"} {
		if err := m.dispatchOne(tgt, One("t", "d", 0, nil, SendOptions{Event: e, JobID: "yt_1"}), false); err != nil {
			t.Fatalf("%s: %v", e, err)
		}
	}
	calls := f.calls()
	if len(calls) != 3 {
		t.Fatalf("want 3 calls, got %d", len(calls))
	}
	if calls[0].Method != http.MethodPost || calls[1].Method != http.MethodPatch || calls[2].Method != http.MethodPatch {
		t.Errorf("methods = %s,%s,%s — want POST,PATCH,PATCH", calls[0].Method, calls[1].Method, calls[2].Method)
	}
}

// TestTerminalEventDoesNotRecreateAGoneMessage: a terminal event edits the
// lifecycle message and posts its own embed, and by ruling never CREATES a
// lifecycle message. When the operator had deleted that message mid-download,
// the PATCH's "Unknown Message" fell through to the create path: Discord got
// a new "Failed" lifecycle message AND the separate Job Failed embed — two
// posts for one failure — and the stored id became a terminal-look message
// that a later Retry would go on editing.
//
// Mutant: dropping the AlsoSeparate arm — the wire carries a second POST and
// the stored id is the new message's.
func TestTerminalEventDoesNotRecreateAGoneMessage(t *testing.T) {
	f := newFakeDiscord(t, func(_ int, r recordedReq, rw http.ResponseWriter) {
		if r.Method == http.MethodPatch {
			rw.WriteHeader(http.StatusNotFound)
			io.WriteString(rw, `{"message":"Unknown Message","code":10008}`)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		io.WriteString(rw, `{"id":"NEW777"}`)
	})
	st := newMemStore()
	key := targetMsgKey(f.URL())
	st.rows["yt_1"] = map[string]string{key: "STALE111"}
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)
	tgt := notificationTarget{sender: &DiscordWebhook{URL: f.URL()}, mode: ModeEdit, msgKey: key}

	if err := m.dispatchOne(tgt, One("Job Failed", "d", TypeError.Color(), nil,
		SendOptions{Event: "error", JobID: "yt_1"}), false); err != nil {
		t.Fatalf("dispatchOne: %v", err)
	}
	var seq []string
	for _, c := range f.calls() {
		seq = append(seq, c.Method+"?"+c.Query)
	}
	if len(seq) != 2 || seq[0] != http.MethodPatch+"?" || seq[1] != http.MethodPost+"?" {
		t.Errorf("wire = %v, want the PATCH and then only the separate POST", seq)
	}
	if id := st.NotificationMsgs("yt_1")[key]; id == "NEW777" {
		t.Error("the terminal event's separate post was stored as the job's lifecycle message")
	}
}
