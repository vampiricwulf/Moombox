package notifications

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// An embed is dated by when its event happened, not by when a target got
// round to delivering it: after a Discord backlog — a long 429, an outage —
// every History line and every embed timestamp used to read the delivery
// time. Manager.Send stamps Embed.At, and both read it.
//
// Mutants: rewriteFields given time.Now() in dispatchOne — the History line;
// toDiscordEmbed's timestamp from time.Now() — the embed timestamp.
func TestALateDeliveryIsDatedByItsEvent(t *testing.T) {
	f := newFakeDiscord(t, okCreated("MM1"))
	m := &Manager{logger: testLogger{}}
	tgt := notificationTarget{sender: &DiscordWebhook{URL: f.URL()}, mode: ModeEdit, msgKey: targetMsgKey(f.URL())}
	happened := time.Unix(1758960000, 0)
	msg := One("t", "d", 0, nil, SendOptions{Event: "downloading", JobID: "yt_1"})
	msg.Embeds[0].At = happened

	if err := m.dispatchOne(tgt, msg, false); err != nil {
		t.Fatal(err)
	}
	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("%d requests, want the creating POST", len(calls))
	}
	if h := historyValue(calls[0].Body); !strings.Contains(h, "<t:1758960000:R>") {
		t.Errorf("History = %q, want the line dated when the event happened", h)
	}
	if got, want := calls[0].Body.Embeds[0].Timestamp, happened.UTC().Format(time.RFC3339); got != want {
		t.Errorf("embed timestamp = %q, want %q", got, want)
	}
}

// Mutant: drop the At stamp in Manager.Send — the queued embed carries none.
func TestSendStampsWhenTheEventHappened(t *testing.T) {
	var mu sync.Mutex
	var got []Message
	m := &Manager{logger: testLogger{}}
	installTargets(t, m, notificationTarget{sender: senderFunc(func(msg Message) error {
		mu.Lock()
		got = append(got, msg)
		mu.Unlock()
		return nil
	})})

	before := time.Now()
	m.Send("t", "d", TypeInfo, nil, SendOptions{Event: "downloading"})
	after := time.Now()
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("%d deliveries, want 1", len(got))
	}
	if at := got[0].Embeds[0].At; at.Before(before) || at.After(after) {
		t.Errorf("At = %v, want the moment Send ran (%v..%v)", at, before, after)
	}
}
