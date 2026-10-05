package notifications

import (
	"net/http"
	"testing"
)

// An EditOnly terminal send closes the job's open lifecycle message and posts
// nothing else: the report it would have carried is suppressed (a failure
// there is nothing to do about), but suppressing the send outright left the
// message reading "Downloading" for good. With no open message, on a
// separate-mode target, or on a transport that cannot edit, it sends nothing
// at all — and the close carries no mention, whose role text would still
// show in the edited message.
//
// Mutants: drop `&& !opts.EditOnly` from the AlsoSeparate post — a third
// request; drop the `plan.MessageID == ""` guard — the no-message and
// separate-mode sends post; drop the non-editable guard — a plain post; keep
// the mention on the close.
func TestAnEditOnlyErrorClosesTheMessageAndPostsNothing(t *testing.T) {
	pinged := func(event string, editOnly bool) Message {
		out := One("t", "d", 0, nil, SendOptions{Event: event, JobID: "yt_1", EditOnly: editOnly})
		out.Mention = "<@&42>"
		out.MentionAllowed = &AllowedMentions{Parse: []string{}, Roles: []string{"42"}}
		return out
	}
	editTgt := func(f *fakeDiscord) notificationTarget {
		return notificationTarget{sender: &DiscordWebhook{URL: f.URL()}, mode: ModeEdit, msgKey: targetMsgKey(f.URL())}
	}

	t.Run("open message", func(t *testing.T) {
		f := newFakeDiscord(t, okCreated("MM1"))
		m := &Manager{logger: testLogger{}}
		tgt := editTgt(f)
		if err := m.dispatchOne(tgt, pinged("downloading", false), false); err != nil {
			t.Fatal(err)
		}
		if err := m.dispatchOne(tgt, pinged("error", true), false); err != nil {
			t.Fatal(err)
		}
		calls := f.calls()
		if len(calls) != 2 || calls[1].Method != http.MethodPatch {
			t.Fatalf("requests = %+v, want the creating POST and one closing PATCH", calls)
		}
		if calls[1].Body.Content != "" {
			t.Errorf("the close carries content %q, want no mention", calls[1].Body.Content)
		}
	})

	t.Run("no open message", func(t *testing.T) {
		f := newFakeDiscord(t, okCreated("MM1"))
		m := &Manager{logger: testLogger{}}
		if err := m.dispatchOne(editTgt(f), pinged("error", true), false); err != nil {
			t.Fatal(err)
		}
		if n := len(f.calls()); n != 0 {
			t.Errorf("%d requests for a job with no open message, want none", n)
		}
	})

	t.Run("separate mode", func(t *testing.T) {
		f := newFakeDiscord(t, okCreated("MM1"))
		m := &Manager{logger: testLogger{}}
		tgt := editTgt(f)
		tgt.mode = ModeSeparate
		if err := m.dispatchOne(tgt, pinged("error", true), false); err != nil {
			t.Fatal(err)
		}
		if n := len(f.calls()); n != 0 {
			t.Errorf("%d requests on a separate-mode target, want none", n)
		}
	})

	t.Run("transport that cannot edit", func(t *testing.T) {
		sent := 0
		m := &Manager{logger: testLogger{}}
		tgt := notificationTarget{sender: senderFunc(func(Message) error { sent++; return nil }), mode: ModeEdit, msgKey: "k"}
		if err := m.dispatchOne(tgt, pinged("error", true), false); err != nil {
			t.Fatal(err)
		}
		if sent != 0 {
			t.Errorf("%d posts on a transport that cannot edit, want none", sent)
		}
	})
}
