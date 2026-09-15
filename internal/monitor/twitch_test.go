package monitor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// newTestTwitchMonitor builds a TwitchMonitor over a real (temp-file) db and
// an in-memory config store, with the GQL batch call replaced by fetch. The
// concrete *twitch.Service is nil on purpose: streamInfoBatch must never reach
// it while FetchBatch is wired, and a nil dereference would say so loudly.
func newTestTwitchMonitor(t *testing.T, fetch StreamInfoBatchFunc, chans ...config.ChannelConfig) *TwitchMonitor {
	t.Helper()
	tm := NewTwitchMonitor(
		config.NewStore(config.Defaults(), ""),
		newTestDB(t),
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	tm.FetchBatch = fetch
	_ = tm.configStore.Update(func(c *config.MoomboxConfig) { c.Channels = chans })
	return tm
}

// twitchChans builds n enabled Twitch channels named tw0..tw(n-1).
func twitchChans(n int) []config.ChannelConfig {
	out := make([]config.ChannelConfig, n)
	for i := range out {
		out[i] = config.ChannelConfig{ID: fmt.Sprintf("tw%d", i), Name: fmt.Sprintf("tw%d", i), Platform: "twitch"}
	}
	return out
}

// shrinkTwitchStagger cuts the inter-chunk pacing sleep so the test measures a
// gap in tens of milliseconds instead of half a second.
func shrinkTwitchStagger(t *testing.T) {
	t.Helper()
	orig := twitchStagger
	twitchStagger = 50 * time.Millisecond
	t.Cleanup(func() { twitchStagger = orig })
}

// TestTwitch_StaggerRunsAfterAWholeBatchFailure pins T3-28: the inter-chunk
// stagger must run even when the batch request failed outright. A GQL 429 or
// 5xx is exactly when pacing matters, and the old `continue` skipped it — with
// >30 channels every remaining chunk fired back to back into a throttled API.
//
// Mutant: restoring the `continue` before the stagger makes the gap ~0.
func TestTwitch_StaggerRunsAfterAWholeBatchFailure(t *testing.T) {
	shrinkTwitchStagger(t)

	var calls []time.Time
	tm := newTestTwitchMonitor(t, func(ctx context.Context, logins []string) ([]*twitch.TwitchStreamInfo, []error, error) {
		calls = append(calls, time.Now())
		return nil, nil, fmt.Errorf("twitch gql: 429 Too Many Requests")
	}, twitchChans(twitchBatchChunk+1)...)

	tm.doCheck(context.Background())

	if len(calls) != 2 {
		t.Fatalf("batch calls = %d, want 2 (%d channels at a chunk of %d)", len(calls), twitchBatchChunk+1, twitchBatchChunk)
	}
	if gap := calls[1].Sub(calls[0]); gap < 40*time.Millisecond {
		t.Fatalf("inter-chunk gap = %v, want >= 40ms — a failed batch must still stagger before the next chunk", gap)
	}
}
