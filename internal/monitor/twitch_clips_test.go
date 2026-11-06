package monitor

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// clipRecorder is a TwitchMonitor's clip listing and OnClipFound, recorded.
type clipRecorder struct {
	mu    sync.Mutex
	asked []string // "<login>/<filter>"
	found []string // slugs handed to OnClipFound
	cards map[string][]twitch.TwitchClipCard
}

func (r *clipRecorder) wire(tm *TwitchMonitor) {
	tm.FetchClips = func(_ context.Context, login string, filter twitch.ClipFilter) ([]twitch.TwitchClipCard, error) {
		r.mu.Lock()
		r.asked = append(r.asked, login+"/"+string(filter))
		r.mu.Unlock()
		return r.cards[login], nil
	}
	tm.OnClipFound = func(card twitch.TwitchClipCard, ch *config.ChannelConfig) {
		r.mu.Lock()
		r.found = append(r.found, ch.ID+":"+card.Slug)
		r.mu.Unlock()
	}
}

func cards(slugs ...string) []twitch.TwitchClipCard {
	out := make([]twitch.TwitchClipCard, len(slugs))
	for i, s := range slugs {
		out[i] = twitch.TwitchClipCard{Slug: s, URL: "https://clips.twitch.tv/" + s}
	}
	return out
}

// TestClipSweepOptInDedupesAndPaces is owner decision D26 follow-up 2's
// monitor opt-in: only a channel whose archive_clips is on is listed; its
// first sweep asks for the last week and every later one for the last day,
// each at most once an hour; and a clip is handed on once — a slug in the
// processing history (a job was made for it) or one that is a job now (it
// was pasted) is skipped, by the clip's job ID.
//
// Mutants: the ArchiveClips filter dropped (the off channel is listed); the
// hourly gate dropped (the second sweep lists again at once); the first-sweep
// filter LAST_DAY; the history check dropped (the processed clip is handed on
// again); the JobExists check dropped (the pasted clip is handed on).
func TestClipSweepOptInDedupesAndPaces(t *testing.T) {
	on := config.ChannelConfig{ID: "vanillatv", Name: "VanillaTV", Platform: "twitch", ArchiveClips: true}
	off := config.ChannelConfig{ID: "quiet", Name: "Quiet", Platform: "twitch"}
	tm := newTestTwitchMonitor(t, nil, on, off)
	rec := &clipRecorder{cards: map[string][]twitch.TwitchClipCard{
		"vanillatv": cards("NewClipSlug", "SeenClipSlug", "PastedClipSlug"),
		"quiet":     cards("NeverAsked"),
	}}
	rec.wire(tm)
	if err := tm.db.AddToHistory(twitch.ClipJobID("SeenClipSlug")); err != nil {
		t.Fatal(err)
	}
	pasted := twitch.ClipJobID("PastedClipSlug")
	if _, err := tm.db.AddJob(&database.Job{ID: pasted, VideoID: pasted, Platform: "twitch", Status: database.StatusFinished}); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	chans := clipChannelsOf(tm.getTwitchChannels())
	tm.sweepClips(ctx, chans, start)
	tm.sweepClips(ctx, chans, start.Add(59*time.Minute))
	tm.sweepClips(ctx, chans, start.Add(time.Hour))

	if want := []string{"vanillatv/LAST_WEEK", "vanillatv/LAST_DAY"}; !equalStrings(rec.asked, want) {
		t.Errorf("listings = %v, want %v", rec.asked, want)
	}
	// The second sweep hands NewClipSlug on again: nothing made a job of it
	// here (OnClipFound is the recorder); in production the receiver's
	// AddToHistory is what the next sweep's HasProcessed sees.
	if want := []string{"vanillatv:NewClipSlug", "vanillatv:NewClipSlug"}; !equalStrings(rec.found, want) {
		t.Errorf("handed on = %v, want only the new clip, once a sweep", rec.found)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestClipSweepRidesThePollOffItsGoroutine: the live poll (doCheck) starts the
// clip sweep, and never waits on it — a channel's paging cannot delay the
// live checks, which lose footage when late — and a sweep still running is
// not started twice, even once its channels are due again.
//
// Mutants: the startClipSweep call dropped from doCheck (nothing is listed);
// the sweep run inline (doCheck blocks on the listing); the sweeping latch
// dropped (two sweeps of the same channel at once).
func TestClipSweepRidesThePollOffItsGoroutine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		on := config.ChannelConfig{ID: "vanillatv", Name: "VanillaTV", Platform: "twitch", ArchiveClips: true}
		offline := func(_ context.Context, logins []string) ([]*twitch.TwitchStreamInfo, []error, error) {
			return make([]*twitch.TwitchStreamInfo, len(logins)), make([]error, len(logins)), nil
		}
		tm := newTestTwitchMonitor(t, offline, on)
		release := make(chan struct{})
		var mu sync.Mutex
		listings := 0
		tm.FetchClips = func(ctx context.Context, login string, filter twitch.ClipFilter) ([]twitch.TwitchClipCard, error) {
			mu.Lock()
			listings++
			mu.Unlock()
			<-release
			return nil, nil
		}
		tm.OnClipFound = func(twitch.TwitchClipCard, *config.ChannelConfig) {}

		ctx := context.Background()
		tm.doCheck(ctx) // returns with the listing still blocked
		synctest.Wait()
		// An hour on, the channel is due again — but the first sweep is
		// still running, so no second one starts.
		time.Sleep(twitchClipSweepEvery)
		tm.doCheck(ctx)
		synctest.Wait()
		mu.Lock()
		got := listings
		mu.Unlock()
		if got != 1 {
			t.Errorf("listings while the first sweep runs = %d, want 1", got)
		}
		close(release)
		synctest.Wait()
		if tm.clips.sweeping.Load() {
			t.Error("the sweep's latch is still set after it finished")
		}
	})
}
