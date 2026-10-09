package notifications

import (
	"net/http"
	"testing"

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
// ptb./canary. host, a trailing slash or a bare "?" each stored a job's
// message id under a key of its own. canonicalDiscordURL folded them all into
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
// spelling's key — the last row POSTs.
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
// it, and the entry outlives the job's terminal edit.
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
