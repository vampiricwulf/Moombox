package notifications

import (
	"slices"
	"testing"
	"time"
)

// A send's Fallback reaches exactly the targets that filter its event out but
// allow the fallback's: a target allowing the event gets the event, one
// allowing neither gets nothing. This is how a YouTube discovery's schedule
// still reaches a target filtered to `scheduled` but not `found`.
//
// Mutants: ignoring Fallback (the scheduled-only target receives nothing);
// sending the fallback to every target (the unfiltered and found-only targets
// receive two); routing it to a target that allows neither.
func TestFallbackReachesOnlyTargetsThatFilterTheEventOut(t *testing.T) {
	all, found, sched, neither := &recordingSender{}, &recordingSender{}, &recordingSender{}, &recordingSender{}
	m := newTestManager(t, time.Second,
		notificationTarget{sender: all, key: "all"},
		notificationTarget{sender: found, key: "found", events: map[string]bool{"found": true}},
		notificationTarget{sender: sched, key: "sched", events: map[string]bool{"scheduled": true}},
		notificationTarget{sender: neither, key: "neither", events: map[string]bool{"finished": true}},
	)

	m.Send("Stream Found", "Upcoming: T", TypeInfo, nil, SendOptions{
		Event: "found", JobID: "j1",
		Fallback: &Fallback{Title: "YouTube Start Time Confirmed", Description: "Scheduled: T", Type: TypeInfo, Event: "scheduled"},
	})
	m.Wait()

	for _, tc := range []struct {
		name string
		rec  *recordingSender
		want []string
	}{
		{"unfiltered", all, []string{"Stream Found"}},
		{"found only", found, []string{"Stream Found"}},
		{"scheduled only", sched, []string{"YouTube Start Time Confirmed"}},
		{"neither", neither, nil},
	} {
		if got := tc.rec.titles(); !slices.Equal(got, tc.want) {
			t.Errorf("%s target received %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A send with no Fallback filters exactly as it always has.
func TestNoFallbackFiltersAsBefore(t *testing.T) {
	sched := &recordingSender{}
	m := newTestManager(t, time.Second,
		notificationTarget{sender: sched, key: "sched", events: map[string]bool{"scheduled": true}},
	)
	m.Send("Stream Found", "", TypeInfo, nil, SendOptions{Event: "found", JobID: "j1"})
	m.Wait()
	if got := sched.titles(); len(got) != 0 {
		t.Errorf("a scheduled-only target received %q from a send with no fallback", got)
	}
}

// mentionSender records each delivered message's title and ping.
type mentionSender struct {
	recordingSender
	mentions []string
}

func (s *mentionSender) Send(msg Message) error {
	s.mu.Lock()
	s.mentions = append(s.mentions, msg.Mention)
	s.mu.Unlock()
	return s.recordingSender.Send(msg)
}

func (s *mentionSender) SendOnce(msg Message) error { return s.Send(msg) }

// A target that takes both events but pings only on `scheduled` was pinged by
// the confirmation that used to follow every scheduled discovery. The merged
// "Stream Found" carries that schedule, so it pings that target too. Without a
// fallback it does not.
//
// Mutant: dropping the fallback-event mention lookup in Manager.Send.
func TestPrimaryPingsOnTheFallbacksMentionEvent(t *testing.T) {
	newTarget := func(s sender) notificationTarget {
		return notificationTarget{
			sender: s, key: "k", events: map[string]bool{"found": true, "scheduled": true},
			mention: "@here", mentionAllowed: &AllowedMentions{Parse: []string{"everyone"}},
			mentionEvents: map[string]bool{"scheduled": true},
		}
	}

	with := &mentionSender{}
	m := newTestManager(t, time.Second, newTarget(with))
	m.Send("Stream Found", "", TypeInfo, nil, SendOptions{
		Event: "found", JobID: "j1",
		Fallback: &Fallback{Title: "YouTube Start Time Confirmed", Type: TypeInfo, Event: "scheduled"},
	})
	m.Wait()
	if got := with.titles(); !slices.Equal(got, []string{"Stream Found"}) {
		t.Fatalf("received %q, want only the primary", got)
	}
	if with.mentions[0] != "@here" {
		t.Errorf("mention = %q, want the target's ping for the schedule it carries", with.mentions[0])
	}

	without := &mentionSender{}
	m2 := newTestManager(t, time.Second, newTarget(without))
	m2.Send("Stream Found", "", TypeInfo, nil, SendOptions{Event: "found", JobID: "j2"})
	m2.Wait()
	if len(without.mentions) != 1 || without.mentions[0] != "" {
		t.Errorf("mentions = %q, want one unpinged message for a find with no schedule", without.mentions)
	}
}

// optsSender records each delivered message's first embed options.
type optsSender struct {
	recordingSender
	opts []SendOptions
}

func (s *optsSender) Send(msg Message) error {
	s.mu.Lock()
	s.opts = append(s.opts, msg.Embeds[0].Opts)
	s.mu.Unlock()
	return s.recordingSender.Send(msg)
}

func (s *optsSender) SendOnce(msg Message) error { return s.Send(msg) }

// The fallback goes out as its own event — what an edit-mode target's
// lifecycle planning keys on — with the primary's job and link, and neither
// delivered embed still carries the fallback into a queue.
//
// Mutants: the fallback embed keeping the primary's event; opts.Fallback not
// cleared before the embeds are built.
func TestFallbackEmbedCarriesItsOwnEvent(t *testing.T) {
	found, sched := &optsSender{}, &optsSender{}
	m := newTestManager(t, time.Second,
		notificationTarget{sender: found, key: "found", events: map[string]bool{"found": true}},
		notificationTarget{sender: sched, key: "sched", events: map[string]bool{"scheduled": true}},
	)
	m.Send("Stream Found", "", TypeInfo, nil, SendOptions{
		Event: "found", JobID: "j1", URL: "https://www.youtube.com/watch?v=j1",
		Fallback: &Fallback{Title: "YouTube Start Time Confirmed", Type: TypeInfo, Event: "scheduled"},
	})
	m.Wait()

	if len(found.opts) != 1 || len(sched.opts) != 1 {
		t.Fatalf("delivered %d and %d messages, want one each", len(found.opts), len(sched.opts))
	}
	if got := sched.opts[0]; got.Event != "scheduled" || got.JobID != "j1" || got.URL != "https://www.youtube.com/watch?v=j1" {
		t.Errorf("fallback opts = event %q job %q url %q; want scheduled, the primary's job and link", got.Event, got.JobID, got.URL)
	}
	if found.opts[0].Event != "found" {
		t.Errorf("primary event = %q, want found", found.opts[0].Event)
	}
	for _, o := range []SendOptions{found.opts[0], sched.opts[0]} {
		if o.Fallback != nil {
			t.Errorf("a delivered %q embed still carries the fallback", o.Event)
		}
	}
}
