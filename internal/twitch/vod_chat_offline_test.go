package twitch

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A VOD chat page fetch failing through a connectivity outage used to spend
// the whole consecutive-error budget in about twenty seconds and give the
// archive up, while the video beside it waited the outage out. With a
// connectivity probe installed the pager waits for the network instead and
// then asks for the same page again.
//
// The outage lasts until the probe has been asked three times; every request
// before that fails as a dead network would.
//
// Mutant: the fetch-error branch not consulting the probe — the outage never
// ends, and the run stops with "too many VOD chat errors".
func TestVodChatWaitsOutAnOutage(t *testing.T) {
	pages := []vodCommentPageSpec{
		{count: 3, offset: 10, hasNext: true},
		{count: 2, offset: 20, hasNext: false},
	}
	installVodCommentStub(t, pages)
	oldPoll, oldRetry := vodChatOnlinePoll, gqlBaseRetryDelay
	vodChatOnlinePoll, gqlBaseRetryDelay = time.Millisecond, time.Millisecond
	t.Cleanup(func() { vodChatOnlinePoll, gqlBaseRetryDelay = oldPoll, oldRetry })

	var probes atomic.Int32
	online := func() bool { return probes.Load() > 3 }
	stub := twitchHTTPClient.Transport
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		if !online() {
			return nil, errors.New("dial tcp: network is unreachable")
		}
		return stub.RoundTrip(req)
	})}

	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: filepath.Join(t.TempDir(), "vod.chat.json"),
	}, &testLogger{})
	vcd.SetIsOnline(func() bool {
		probes.Add(1)
		return online()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := vcd.Start(ctx); err != nil {
		t.Fatalf("Start = %v, want the archive completed once the network returned", err)
	}
	if got := vcd.MessageCount(); got != 5 {
		t.Errorf("archived %d comments, want 5", got)
	}
}
