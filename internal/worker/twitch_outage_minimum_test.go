package worker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// minimumRun is an endVerdictHarness whose orchestrator sends to rec.
type minimumRun struct {
	*endVerdictHarness
	rec *notificationtest.Recorder
}

// retryNowChat is a live chat source that counts RetryNow, the call that cuts
// a reconnect backoff short.
type retryNowChat struct {
	stop    chan struct{}
	once    sync.Once
	running atomic.Bool
	retries atomic.Int32
}

func newRetryNowChat() *retryNowChat { return &retryNowChat{stop: make(chan struct{})} }

func (c *retryNowChat) Start(ctx context.Context) error {
	c.running.Store(true)
	defer c.running.Store(false)
	select {
	case <-c.stop:
	case <-ctx.Done():
	}
	return nil
}
func (c *retryNowChat) Stop()             { c.once.Do(func() { close(c.stop) }) }
func (c *retryNowChat) MarkStreamEnded()  { c.Stop() }
func (c *retryNowChat) MessageCount() int { return 0 }
func (c *retryNowChat) IsRunning() bool   { return c.running.Load() }
func (c *retryNowChat) RetryNow()         { c.retries.Add(1) }

// minimumHarness starts a live capture that never ends on an always-live
// broadcast, with cancelAfter as the outage minimum, and counts the
// broadcast rechecks — the first thing the outage recovery does once the link
// is back, so a recheck means the session was cancelled into it. h.rec
// records what the capture sends.
func minimumHarness(t *testing.T, jobID string, cancelAfter time.Duration) (h *minimumRun, chat *retryNowChat, rechecks *atomic.Int32, stop func()) {
	t.Helper()
	srv, first := liveTwitchWindow(t)
	h = &minimumRun{endVerdictHarness: newEndVerdictHarness(t, jobID), rec: notificationtest.New()}
	h.o.notifier = h.rec
	twitchOutageCancelAfter = cancelAfter // the harness restores the production value
	old := postOutageRetryDelay
	postOutageRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { postOutageRetryDelay = old })

	rechecks = &atomic.Int32{}
	h.variant.URL = srv.URL + "/live.m3u8"
	h.variant.CheckStreamFn = func(context.Context) (bool, error) { return true, nil }
	h.variant.RecheckStreamFn = func(context.Context) (*twitch.TwitchStreamInfo, error) {
		rechecks.Add(1)
		return &twitch.TwitchStreamInfo{IsLive: true}, nil
	}
	h.variant.FetchVariantsFn = func(context.Context) ([]twitch.TwitchHLSVariant, error) {
		return []twitch.TwitchHLSVariant{{URL: srv.URL + "/live.m3u8", Name: "720p60", Width: 1280, Height: 720, FPS: 30}}, nil
	}
	h.jobCtx.OutputDir = t.TempDir()
	h.jobCtx.Filename = jobID
	chat = newRetryNowChat()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, chat) }()
	select {
	case <-first:
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("no segment fetched")
	}
	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(45 * time.Second):
			t.Fatal("ExecuteTwitch did not return after its context was cancelled")
		}
	}
	return h, chat, rechecks, stop
}

// TestTwitchBlipDoesNotRestartTheSession is W27-11's Twitch half: every
// offline transition cancelled a live capture's session into the outage
// recovery — a broadcast recheck, a variant refresh and a "Twitch Download
// Resumed" — and a flapping link cycles the monitor every ~15 s. An outage
// shorter than the minimum is ridden out in place, and its restore only asks
// the chat to reconnect now.
//
// Mutants: cancelling on the offline transition whatever the minimum (a
// recheck follows the blip); the restore not asking the chat to reconnect.
func TestTwitchBlipDoesNotRestartTheSession(t *testing.T) {
	h, chat, rechecks, stop := minimumHarness(t, "tw_blip", 2*time.Second)
	defer stop()

	h.conn.set(false)
	time.Sleep(300 * time.Millisecond)
	h.conn.set(true)
	// Past the minimum: a cancel the blip armed and the restore left armed
	// would have fired by now.
	time.Sleep(2500 * time.Millisecond)
	if n := rechecks.Load(); n != 0 {
		t.Errorf("a 300 ms blip sent the capture through the outage recovery (%d rechecks), want it ridden out", n)
	}
	if chat.retries.Load() == 0 {
		t.Error("the restore did not ask the chat to reconnect")
	}
}

// TestTwitchOutagePastTheMinimumRestartsTheSession: an outage that outlasts
// the minimum still cancels the session, the recovery rechecks the broadcast
// once the link is back, and the resume embed dates the pause from when the
// link went down — not from the cancel the minimum later, which would
// understate the outage by the minimum.
//
// Mutants: the armed cancel never firing (no recheck after a long outage);
// the session loop dating the pause where it consumes the cancel (the Paused
// timestamp lands a second or more after the loss).
func TestTwitchOutagePastTheMinimumRestartsTheSession(t *testing.T) {
	h, _, rechecks, stop := minimumHarness(t, "tw_long_outage", 1500*time.Millisecond)
	defer stop()

	before := time.Now()
	h.conn.set(false)
	after := time.Now()
	time.Sleep(2500 * time.Millisecond)
	h.conn.set(true)
	deadline := time.Now().Add(10 * time.Second)
	for len(h.rec.ByEvent("connectivity_resume")) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rechecks.Load() == 0 {
		t.Fatal("an outage past the minimum did not send the capture through the outage recovery")
	}
	resumed := h.rec.ByEvent("connectivity_resume")
	if len(resumed) != 1 {
		t.Fatalf("%d resume notifications, want 1", len(resumed))
	}
	paused, _ := resumed[0].Field("Paused")
	var at int64
	if _, err := fmt.Sscanf(paused, "<t:%d:R>", &at); err != nil {
		t.Fatalf("Paused = %q: %v", paused, err)
	}
	if at < before.Unix() || at > after.Unix() {
		t.Errorf("Paused dates the pause at %d, want the loss (%d..%d) — the cancel came %v after it",
			at, before.Unix(), after.Unix(), twitchOutageCancelAfter)
	}
	if !strings.Contains(paused, "resumed after") {
		t.Errorf("Paused = %q, want the outage duration", paused)
	}
}

// TestTwitchRestoreDisarmsTheOutageCancel: each outage counts its own
// minimum. A restore disarms the cancel the outage before it armed, so a
// second short outage that starts just before the first one's minimum would
// have run out is ridden out too.
//
// Mutants: the callback not disarming the armed cancel on a transition (the
// first outage's timer, still counting when the second outage starts, fires
// 300 ms into it and cancels the session). The timer callback's own IsOnline
// check is a second guard for the one race this test cannot stage — a
// callback already running when the monitor goes online — and a mutant
// removing it survives here, by construction.
func TestTwitchRestoreDisarmsTheOutageCancel(t *testing.T) {
	h, _, rechecks, stop := minimumHarness(t, "tw_two_blips", 1500*time.Millisecond)
	defer stop()

	h.conn.set(false)
	time.Sleep(200 * time.Millisecond)
	h.conn.set(true)
	time.Sleep(1000 * time.Millisecond)
	h.conn.set(false) // 1.2 s after the first loss
	time.Sleep(800 * time.Millisecond)
	h.conn.set(true) // the second outage lasted 0.8 s
	time.Sleep(1200 * time.Millisecond)
	if n := rechecks.Load(); n != 0 {
		t.Errorf("two outages under the minimum sent the capture through the recovery (%d rechecks) — the first one's cancel outlived its restore", n)
	}
}
