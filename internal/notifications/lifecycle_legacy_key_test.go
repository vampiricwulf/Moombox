package notifications

import (
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// legacyKeyTarget builds the edit-mode target buildTargets makes of urls,
// configured in that order, and aims its sender at the fake. Built through
// buildTargets on purpose: which old keys a target carries is decided there.
func legacyKeyTarget(t *testing.T, f *fakeDiscord, urls ...string) notificationTarget {
	t.Helper()
	cfg := &config.MoomboxConfig{}
	for _, u := range urls {
		cfg.Notifications = append(cfg.Notifications, config.NotificationConfig{URL: u, Mode: ModeEdit})
	}
	built := buildTargets(cfg, testLogger{})
	if len(built) != 1 {
		t.Fatalf("%v built %d targets, want 1", urls, len(built))
	}
	tgt := built[0]
	tgt.sender = &DiscordWebhook{URL: f.URL()}
	return tgt
}

// 2.8.9 and 2.8.10 resolved a webhook by rewriting only discordapp.com, so a
// ptb./canary. host, a trailing slash, a bare "?", and a query's order, stray
// '&' or wait each stored a job's message id under a key of its own.
// canonicalDiscordURL folded them all into
// one https://discord.com/... form, which changed the key: after the upgrade an
// in-progress job's next event found no id and opened a SECOND message, and its
// error or cancel — which never opens one — left the first reading
// "Downloading" for good. A miss now reads the key the old release stored.
//
// storedUnder is written out by hand, not derived through legacyResolvedURL:
// it is what 2.8.10's parseTarget returned, and deriving it would let a wrong
// legacyResolvedURL agree with itself.
//
// Mutants: messageID ignoring its legacy keys, or buildTargets not deriving
// them — every row POSTs; the duplicate branch not appending the collapsed
// spelling's key — both collapsed rows POST.
func TestAnUpgradeKeepsEditingTheMessageAnOldSpellingStored(t *testing.T) {
	const id, tok = "123456789012345678", "abcdefTOKEN"
	plain := "https://discord.com/api/webhooks/" + id + "/" + tok
	ptb := "https://ptb.discord.com/api/webhooks/" + id + "/" + tok
	for _, tc := range []struct {
		name        string
		urls        []string
		storedUnder string
	}{
		{"a ptb. host", []string{ptb}, ptb},
		{"a canary. host on the legacy domain",
			[]string{"https://canary.discordapp.com/api/webhooks/" + id + "/" + tok},
			"https://canary.discord.com/api/webhooks/" + id + "/" + tok},
		{"a trailing slash", []string{plain + "/"}, plain + "/"},
		{"a bare query", []string{plain + "?"}, plain + "?"},
		{"a bare query on the discord:// form", []string{"discord://" + id + "/" + tok + "?"}, plain + "?"},
		{"a spelling that collapsed into an earlier one", []string{plain, ptb}, ptb},
		{"a query carrying wait", []string{plain + "?wait=true&thread_id=9"}, plain + "?wait=true&thread_id=9"},
		{"a query in another order with a stray '&'",
			[]string{plain + "?with_components=true&thread_id=9&"}, plain + "?with_components=true&thread_id=9&"},
		{"a query spelling that collapsed into an earlier one",
			[]string{plain + "?thread_id=9", plain + "?thread_id=9&"}, plain + "?thread_id=9&"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDiscord(t, okCreated("NEW"))
			tgt := legacyKeyTarget(t, f, tc.urls...)
			st := newMemStore()
			const job = "dQw4w9WgXcQ"
			st.rows[job] = map[string]string{targetMsgKey(tc.storedUnder): "M_OLD"}
			m := &Manager{logger: testLogger{}}
			m.SetMessageStore(st)

			msg := One("Downloading", "d", 0, nil, SendOptions{Event: "downloading", JobID: job})
			if err := m.dispatchOne(tgt, msg, false); err != nil {
				t.Fatal(err)
			}
			c := f.calls()
			if len(c) != 1 || c[0].Method != http.MethodPatch || c[0].Path != "/messages/M_OLD" {
				var got []string
				for _, r := range c {
					got = append(got, r.Method+" "+r.Path)
				}
				t.Errorf("a job open across the upgrade: next event = %v, want one PATCH of /messages/M_OLD", got)
			}
		})
	}
}

// A webhook configured in the canonical form resolved the same way before the
// upgrade, so it carries no old key to look up.
func TestACanonicalSpellingCarriesNoOldKey(t *testing.T) {
	const id, tok = "123456789012345678", "abcdefTOKEN"
	f := newFakeDiscord(t, okCreated("NEW"))
	for _, u := range []string{
		"https://discord.com/api/webhooks/" + id + "/" + tok,
		"discord://" + id + "/" + tok,
		"https://discord.com/api/webhooks/" + id + "/" + tok + "?thread_id=9",
		"https://discord.com/api/webhooks/" + id + "/" + tok + "?thread_id=9&with_components=true",
	} {
		if got := legacyKeyTarget(t, f, u).legacyMsgKeys; len(got) != 0 {
			t.Errorf("%q carries old keys %v, want none", u, got)
		}
	}
}

// Adopting an old key's id moves it: the old key leaves the in-memory map, so
// the job's next row write — here another target's first POST for the job —
// stores the id under the current key and the old key is gone; and a
// delivered `finished` releases the job's entry, which an old key nobody will
// ever close would otherwise hold until eviction.
//
// Mutant: messageID adopting the id but keeping the old key — the row keeps
// it.
func TestAnAdoptedOldKeyLeavesTheRowOnItsNextWrite(t *testing.T) {
	const id, tok = "123456789012345678", "abcdefTOKEN"
	ptb := "https://ptb.discord.com/api/webhooks/" + id + "/" + tok
	f := newFakeDiscord(t, okCreated("M_B"))
	tgtA := legacyKeyTarget(t, f, ptb)
	tgtB := legacyKeyTarget(t, f, "https://discord.com/api/webhooks/987654321098765432/"+tok)
	st := newMemStore()
	const job = "dQw4w9WgXcQ"
	oldKey := targetMsgKey(ptb)
	st.rows[job] = map[string]string{oldKey: "M_OLD"}
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)

	send := func(tgt notificationTarget, event string) {
		t.Helper()
		if err := m.dispatchOne(tgt, One(event, "d", 0, nil, SendOptions{Event: event, JobID: job}), false); err != nil {
			t.Fatal(err)
		}
	}
	send(tgtA, "downloading") // adopts M_OLD
	send(tgtB, "downloading") // B's first message: the row's next write

	row := st.NotificationMsgs(job)
	if row[tgtA.msgKey] != "M_OLD" || row[tgtB.msgKey] != "M_B" {
		t.Errorf("row after the next write = %v, want M_OLD under the current key and M_B", row)
	}
	if _, kept := row[oldKey]; kept {
		t.Errorf("row after the next write still holds the old key: %v", row)
	}

	send(tgtA, "finished")
	send(tgtB, "finished")
	if n := m.tracker().trackedJobs(); n != 0 {
		t.Errorf("tracker holds %d jobs after every target's finished edit, want 0", n)
	}
}

// A delete's ordered drop covers a target's old-spelling keys too. A job open
// across the upgrade can hold the target's id under one, loaded into the
// tracker by ANOTHER target's send and not yet adopted. That key counted as
// one no queue would reach and was dropped at once, so the target's queued
// cancel found no id and posted plain: its message read "Downloading" for
// good.
//
// Mutants: editKeys leaving out the old-spelling keys — A's cancel is a plain
// POST in both rows; setDispatch not moving them onto a surviving queue — in
// the reload row.
func TestADeleteDropsAnOldSpellingsKeyInOrder(t *testing.T) {
	const id, tok = "123456789012345678", "abcdefTOKEN"
	plain := "https://discord.com/api/webhooks/" + id + "/" + tok
	ptb := "https://ptb.discord.com/api/webhooks/" + id + "/" + tok
	for _, tc := range []struct {
		name   string
		reload bool
	}{
		{"configured at boot", false},
		{"a spelling added by a reload", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fA, release := gated(t, 0) // A is busy with another job's request
			fB := newFakeDiscord(t, createdInOrder)
			tgtB := legacyKeyTarget(t, fB, "https://discord.com/api/webhooks/987654321098765432/"+tok)
			st := newMemStore()
			const job = "dQw4w9WgXcQ"
			st.rows[job] = map[string]string{targetMsgKey(ptb): "M_OLD_A", tgtB.msgKey: "M_OLD_B"}
			m := &Manager{logger: testLogger{}}
			m.SetMessageStore(st)
			if tc.reload {
				installTargets(t, m, legacyKeyTarget(t, fA, plain), tgtB)
			}
			installTargets(t, m, legacyKeyTarget(t, fA, plain, ptb), tgtB)

			m.Send("Found", "x", TypeInfo, nil, SendOptions{Event: "found", JobID: "otherJob123"})
			if !waitCalls(t, fA, 1, 3*time.Second) {
				t.Fatal("the other job's POST never reached A")
			}
			// Deleting the active job: its cancel is queued on both targets.
			m.Send("Cancelled", "desc", TypeCancelled, nil, SendOptions{Event: "cancelled", JobID: job})
			// B closes its own message, which loads the row — A's id still
			// under its old key — into the tracker.
			if !waitCalls(t, fB, 3, 3*time.Second) {
				t.Fatalf("B: %v, want the found's POST, the cancel's PATCH and its separate POST", requestLines(fB.calls()))
			}
			deleteRow(st, job)
			m.ForgetJob(job)
			release()

			if !waitCalls(t, fA, 3, 3*time.Second) {
				t.Fatalf("A: %v, want the cancel's PATCH and its separate POST", requestLines(fA.calls()))
			}
			if c := fA.calls()[1]; c.Method != http.MethodPatch || c.Path != "/messages/M_OLD_A" {
				t.Errorf("A's queued cancel was %s %s, want a PATCH closing its message M_OLD_A", c.Method, c.Path)
			}
		})
	}
}

// 2.8.9 and 2.8.10 built one target per spelling, so a webhook configured as
// "…/TOKEN" and "…/TOKEN/" posted one lifecycle message per spelling for every
// job, and a job open across the upgrade holds an id under each key. Folded
// into one target, it kept editing the current key's message and dropped the
// other spelling's key without ever touching its message, which read
// "Downloading" for good. Every edit rewrites both now, and the terminal edit
// closes both.
//
// Mutants: messageID dropping an old key that holds a different id — M_SLASH
// is never edited; dispatchOne's release leaving the old key open — the
// tracker keeps the job's entry after its terminal edit.
func TestAnUpgradeKeepsEditingASecondSpellingsMessage(t *testing.T) {
	const id, tok = "123456789012345678", "abcdefTOKEN"
	plain := "https://discord.com/api/webhooks/" + id + "/" + tok
	for _, terminal := range []string{"finished", "cancelled"} {
		t.Run(terminal, func(t *testing.T) {
			f := newFakeDiscord(t, okCreated("NEW"))
			tgt := legacyKeyTarget(t, f, plain, plain+"/")
			st := newMemStore()
			const job = "dQw4w9WgXcQ"
			st.rows[job] = map[string]string{targetMsgKey(plain): "M_PLAIN", targetMsgKey(plain + "/"): "M_SLASH"}
			m := &Manager{logger: testLogger{}}
			m.SetMessageStore(st)

			for _, ev := range []string{"downloading", terminal} {
				if err := m.dispatchOne(tgt, One(ev, "d", 0, nil, SendOptions{Event: ev, JobID: job}), false); err != nil {
					t.Fatal(err)
				}
			}
			want := []string{
				"PATCH /messages/M_PLAIN", "PATCH /messages/M_SLASH",
				"PATCH /messages/M_PLAIN", "PATCH /messages/M_SLASH",
			}
			if terminal == "cancelled" {
				want = append(want, "POST /") // the separate embed
			}
			c := f.calls()
			if got := requestLines(c); !slices.Equal(got, want) {
				t.Fatalf("requests = %v, want %v", got, want)
			}
			if got := statusValue(c[3].Body); got != lifecycleLabels[terminal] {
				t.Errorf("the second message's last Status = %q, want %q", got, lifecycleLabels[terminal])
			}
			if n := m.tracker().trackedJobs(); n != 0 {
				t.Errorf("tracker holds %d jobs after the terminal edit closed both messages, want 0", n)
			}
		})
	}
}

// A second message Discord no longer has is forgotten, with nothing posted in
// its place — the job's own message carries the story — and one whose edit
// failed otherwise stays open, so the failure is reported and a later edit
// can still close it.
//
// Mutants: patchExtra not forgetting a gone message — the next event PATCHes
// it again; patchExtra reporting a gone message as a failure — the event
// errors; patchExtra counting a failed edit as delivered — the entry is
// released with the second message still open.
func TestASecondSpellingsMessageThatIsGoneOrFails(t *testing.T) {
	const id, tok = "123456789012345678", "abcdefTOKEN"
	plain := "https://discord.com/api/webhooks/" + id + "/" + tok
	const job = "dQw4w9WgXcQ"
	setup := func(t *testing.T, status int, body string) (*Manager, notificationTarget, *fakeDiscord) {
		t.Helper()
		f := newFakeDiscord(t, func(n int, r recordedReq, rw http.ResponseWriter) {
			if r.Path == "/messages/M_SLASH" {
				rw.WriteHeader(status)
				io.WriteString(rw, body)
				return
			}
			okCreated("NEW")(n, r, rw)
		})
		st := newMemStore()
		st.rows[job] = map[string]string{targetMsgKey(plain): "M_PLAIN", targetMsgKey(plain + "/"): "M_SLASH"}
		m := &Manager{logger: testLogger{}}
		m.SetMessageStore(st)
		return m, legacyKeyTarget(t, f, plain, plain+"/"), f
	}

	t.Run("gone", func(t *testing.T) {
		m, tgt, f := setup(t, http.StatusNotFound, `{"message":"Unknown Message","code":10008}`)
		for _, ev := range []string{"downloading", "muxing"} {
			if err := m.dispatchOne(tgt, One(ev, "d", 0, nil, SendOptions{Event: ev, JobID: job}), false); err != nil {
				t.Fatalf("%s: %v", ev, err)
			}
		}
		want := []string{"PATCH /messages/M_PLAIN", "PATCH /messages/M_SLASH", "PATCH /messages/M_PLAIN"}
		if got := requestLines(f.calls()); !slices.Equal(got, want) {
			t.Errorf("requests = %v, want %v", got, want)
		}
	})

	t.Run("refused", func(t *testing.T) {
		m, tgt, _ := setup(t, http.StatusForbidden, `{"message":"Missing Permissions","code":50013}`)
		if err := m.dispatchOne(tgt, One("finished", "d", 0, nil, SendOptions{Event: "finished", JobID: job}), false); err == nil {
			t.Error("a refused edit of the second message was reported as success")
		}
		if n := m.tracker().trackedJobs(); n != 1 {
			t.Errorf("tracker holds %d jobs, want the job kept while its second message is still open", n)
		}
	})
}

// An adopted id leaves its old key only in memory — the row keeps it until the
// job's next write — so a re-read of the row brings the old key back holding
// the very id the target now edits under its current key. That is the same
// message, not a second one. Here the re-read follows a RetainJobs whose list
// predates the job: the quick target's step drops its own key, and the slow
// target's queued event re-reads the row.
//
// Mutant: messageID keeping an old key that holds the id key already holds —
// the slow target's next event PATCHes M_OLD_A twice.
func TestAnAdoptedIDReReadUnderItsOldKeyIsNotASecondMessage(t *testing.T) {
	const id, tok = "123456789012345678", "abcdefTOKEN"
	ptb := "https://ptb.discord.com/api/webhooks/" + id + "/" + tok
	fA, release := gated(t, 1) // A: the job's downloading, then another job's slow request
	fB := newFakeDiscord(t, createdInOrder)
	tgtB := legacyKeyTarget(t, fB, "https://discord.com/api/webhooks/987654321098765432/"+tok)
	st := newMemStore()
	const job = "dQw4w9WgXcQ"
	st.rows[job] = map[string]string{targetMsgKey(ptb): "M_OLD_A", tgtB.msgKey: "M_OLD_B"}
	m := &Manager{logger: testLogger{}}
	m.SetMessageStore(st)
	installTargets(t, m, legacyKeyTarget(t, fA, ptb), tgtB)

	m.Send("Downloading", "x", TypeDownload, nil, SendOptions{Event: "downloading", JobID: job})
	if !waitCalls(t, fA, 1, 3*time.Second) || !waitCalls(t, fB, 1, 3*time.Second) {
		t.Fatal("the downloading edits never reached both targets")
	}
	m.Send("Found", "x", TypeInfo, nil, SendOptions{Event: "found", JobID: "otherJob123"})
	if !waitCalls(t, fA, 2, 3*time.Second) {
		t.Fatal("the other job's POST never reached A")
	}
	m.Send("Muxing", "x", TypeDownload, nil, SendOptions{Event: "muxing", JobID: job})
	if !waitCalls(t, fB, 3, 3*time.Second) { // B: downloading, other job's found, muxing
		t.Fatalf("B: %v", requestLines(fB.calls()))
	}
	m.RetainJobs(map[string]struct{}{"otherJob123": {}}) // taken before the job was added
	tr := m.tracker()
	waitFor(t, "B's step", func() bool {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		j := tr.jobs[job]
		return j != nil && j.msgs[tgtB.msgKey] == ""
	})
	release()

	if !waitCalls(t, fA, 3, 3*time.Second) {
		t.Fatalf("A: %v", requestLines(fA.calls()))
	}
	drain(t, m)
	want := []string{"PATCH /messages/M_OLD_A", "POST /", "PATCH /messages/M_OLD_A"}
	if got := requestLines(fA.calls()); !slices.Equal(got, want) {
		t.Errorf("A: %v, want %v — one edit of its one message per event", got, want)
	}
}
