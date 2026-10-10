package twitch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// Ten failed reconnects in a row used to end chat capture for the rest of the
// job: Start returned "exceeded max IRC reconnects", and nothing relaunched it
// outside a connectivity outage — so a few minutes of Twitch IRC trouble, the
// video unaffected, cost every message of a marathon stream after it. Past
// the budget the loop now keeps trying at a slow cadence until it is stopped.
//
// The server accepts the socket and drops it at once, so every session fails
// and is charged.
//
// Mutant: giving up once the budget is spent — Start returns after eleven
// sessions.
func TestIRCKeepsRetryingPastTheReconnectBudget(t *testing.T) {
	var dials atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		dials.Add(1)
		conn.CloseNow()
	}))
	t.Cleanup(srv.Close)
	prev := constants.TwitchURLs.IRCWS
	constants.TwitchURLs.IRCWS = "ws" + strings.TrimPrefix(srv.URL, "http")
	t.Cleanup(func() { constants.TwitchURLs.IRCWS = prev })

	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan",
		StreamID:     "stream-1",
		OutputPath:   t.TempDir() + "/chat.json",
	}, &testLogger{})
	cd.delays = fastChatDelays()

	done := make(chan error, 1)
	go func() { done <- cd.Start(context.Background()) }()
	t.Cleanup(func() {
		cd.Stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Start did not return after Stop")
		}
	})

	const want = 16 // well past the eleven attempts the budget allows
	deadline := time.Now().Add(10 * time.Second)
	for dials.Load() < want {
		select {
		case err := <-done:
			done <- err
			t.Fatalf("Start returned after %d sessions (%v), want it still retrying", dials.Load(), err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d sessions in 10 s", dials.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startExhaustedDownloader drives a downloader against a server that drops
// every session until it has spent its reconnect budget and sits in the slow
// post-budget backoff, here an hour long. It returns the downloader, the
// dial counter and Start's result channel.
func startExhaustedDownloader(t *testing.T) (*ChatDownloader, *atomic.Int32, chan error) {
	t.Helper()
	dials := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		dials.Add(1)
		conn.CloseNow()
	}))
	t.Cleanup(srv.Close)
	prev := constants.TwitchURLs.IRCWS
	constants.TwitchURLs.IRCWS = "ws" + strings.TrimPrefix(srv.URL, "http")
	t.Cleanup(func() { constants.TwitchURLs.IRCWS = prev })

	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan",
		StreamID:     "stream-1",
		OutputPath:   t.TempDir() + "/chat.json",
	}, &testLogger{})
	cd.delays = fastChatDelays()
	cd.delays.exhaustedRetry = time.Hour

	done := make(chan error, 1)
	go func() { done <- cd.Start(context.Background()) }()
	t.Cleanup(func() {
		cd.Stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for dials.Load() < 11 { // the eleven attempts the budget allows
		if time.Now().After(deadline) {
			t.Fatalf("only %d sessions before the budget was spent", dials.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // now in the hour-long backoff
	return cd, dials, done
}

// The backoff past the reconnect budget is the one wait no session exists
// for, so Stop's and MarkStreamEnded's session cancel could not reach it: a
// downloader idling there ignored them for the rest of the backoff — holding
// up the finalize's chat wait, and writing into staging after a user cancel
// had moved on. Both now wake it.
//
// Mutant: the backoff select without the wake channel — Start does not return.
func TestStopAndMarkStreamEndedWakeTheReconnectBackoff(t *testing.T) {
	for name, end := range map[string]func(*ChatDownloader){
		"Stop":            (*ChatDownloader).Stop,
		"MarkStreamEnded": (*ChatDownloader).MarkStreamEnded,
	} {
		t.Run(name, func(t *testing.T) {
			cd, _, done := startExhaustedDownloader(t)
			end(cd)
			select {
			case err := <-done:
				done <- err
			case <-time.After(2 * time.Second):
				t.Fatalf("Start did not return within 2 s of %s while in the reconnect backoff", name)
			}
		})
	}
}

// When connectivity returns the orchestrator calls RetryNow, so a downloader
// whose backoff the outage stretched to the slow cadence reconnects at once.
//
// Mutant: RetryNow as a no-op — no new session within the window.
func TestRetryNowCutsTheReconnectBackoffShort(t *testing.T) {
	cd, dials, _ := startExhaustedDownloader(t)
	before := dials.Load()
	cd.RetryNow()
	deadline := time.Now().Add(2 * time.Second)
	for dials.Load() == before {
		if time.Now().After(deadline) {
			t.Fatal("no new session within 2 s of RetryNow")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
