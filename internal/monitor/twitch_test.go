package monitor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
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

// recordingHandler is a slog.Handler that keeps every record's level and
// message.
type recordingHandler struct {
	records *[]slog.Record
}

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r)
	return nil
}
func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

// TestTwitch_AWholeBatchFailureStreakIsWarnedOnce: a whole-batch GQL failure
// (Twitch refusing the client, transport down) is kept off every channel's
// health streak by design, and it was logged only at Debug — so a persistent
// one left no Twitch channel checked and nothing above Debug saying so. The
// first failure of a streak now Warns, repeats stay at Debug, and the recovery
// is logged once.
//
// Mutant: log every failure at Debug again — no Warn.
func TestTwitch_AWholeBatchFailureStreakIsWarnedOnce(t *testing.T) {
	failing := true
	tm := newTestTwitchMonitor(t, func(ctx context.Context, logins []string) ([]*twitch.TwitchStreamInfo, []error, error) {
		if failing {
			return nil, nil, fmt.Errorf("gql auth failure (401)")
		}
		return make([]*twitch.TwitchStreamInfo, len(logins)), make([]error, len(logins)), nil
	}, twitchChans(2)...)
	var records []slog.Record
	tm.logger = slog.New(recordingHandler{records: &records})

	chunk := twitchChans(2)
	for range 3 {
		tm.checkChunk(context.Background(), chunk)
	}
	failing = false
	tm.checkChunk(context.Background(), chunk)

	var warns, infos int
	for _, r := range records {
		switch {
		case r.Level == slog.LevelWarn && r.Message != "":
			warns++
		case r.Level == slog.LevelInfo && r.Message == "Twitch batch checks recovered":
			infos++
		}
	}
	if warns != 1 {
		t.Errorf("%d Warn lines for a three-failure streak, want exactly 1", warns)
	}
	if infos != 1 {
		t.Errorf("%d recovery lines, want 1", infos)
	}
}

// A manual add for an offline channel parks a `tw_manual_<login>_<ns>` job in
// waitForTwitchLive. It has no stream ID, so the monitor's dedupe never
// matched it: when the channel went live the monitor created a second job and
// both recorded the broadcast. Once the manual job is over, the monitor takes
// the channel's broadcasts again.
//
// Mutant: processStreamInfo without the manual-job check — OnStreamFound
// fires while the manual job waits.
func TestTwitch_AManualJobWaitingOnTheChannelIsNotDuplicated(t *testing.T) {
	ch := config.ChannelConfig{ID: "streamerx", Name: "streamerx", Platform: "twitch"}
	tm := newTestTwitchMonitor(t, func(ctx context.Context, logins []string) ([]*twitch.TwitchStreamInfo, []error, error) {
		return []*twitch.TwitchStreamInfo{{StreamID: "4242", ChannelLogin: "streamerx", ChannelDisplayName: "StreamerX", Title: "hi", IsLive: true}}, []error{nil}, nil
	}, ch)
	const manualID = "tw_manual_streamerx_1700000000000000001"
	if _, err := tm.db.AddJob(&database.Job{ID: manualID, VideoID: manualID, URL: "https://www.twitch.tv/streamerx",
		Platform: "twitch", Status: database.StatusUpcoming, ManuallyAdded: true}); err != nil {
		t.Fatal(err)
	}
	var found []string
	tm.OnStreamFound = func(info *twitch.TwitchStreamInfo, _ *config.ChannelConfig) { found = append(found, info.StreamID) }

	tm.doCheck(context.Background())
	if len(found) != 0 {
		t.Fatalf("OnStreamFound = %v while a manual job waits on the channel", found)
	}

	if tm.db.UpdateJobFields(manualID, map[string]any{"status": database.StatusFinished}) == nil {
		t.Fatal("UpdateJobFields: no row")
	}
	tm.doCheck(context.Background())
	if len(found) != 1 || found[0] != "4242" {
		t.Errorf("OnStreamFound = %v after the manual job finished, want [4242]", found)
	}
}
