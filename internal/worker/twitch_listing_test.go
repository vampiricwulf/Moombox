package worker

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// TestQueueTwitchVideosRefusesAnOwnerlessVod: a listed VOD that names no
// owner (a collection node neither the listing nor VideoMetadata gave one)
// is not created — a Queued row with no channel_id is one the archive-slots
// scheduler never admits — and is counted as failed, not added; one that
// names an owner is queued under it.
//
// Each VOD that is not queued is logged with its ID and why (purpose review
// M9): the add's answer counts the failures, and only the log names them.
//
// Mutants: the guard removed (a Queued row with a NULL channel_id is added);
// either not-queued log line removed; the channel field always o.Login
// (logged empty for the owner-less VOD, and for a collection's VOD).
func TestQueueTwitchVideosRefusesAnOwnerlessVod(t *testing.T) {
	db := openTestDB(t)
	owned := twitch.TwitchVideo{ID: "8", Title: "a", PublishedAt: "2026-10-01T00:00:00Z"}
	owned.Owner = &struct {
		Login       string `json:"login"`
		DisplayName string `json:"displayName"`
	}{Login: "Alice"}
	lg := &captureLogger{}
	res := QueueTwitchVideos(db, []twitch.TwitchVideo{{ID: "9", Title: "b", PublishedAt: "2026-10-01T00:00:00Z"}, owned},
		TwitchListingOptions{ManuallyAdded: true, Logger: lg}, time.Now())
	if len(res.Added) != 1 || res.Failed != 1 {
		t.Fatalf("added %d, failed %d; want the owned VOD added and the owner-less one failed", len(res.Added), res.Failed)
	}
	if vod, logged := lg.field("twitch listing: a VOD could not be queued — no channel to pace it under: neither the listing nor Twitch named its owner", "vod"); !logged || vod != "9" {
		t.Errorf("the owner-less VOD's failure logged %v (%v); lines %v", vod, logged, lg.lines())
	}
	if ch, _ := lg.field("twitch listing: a VOD could not be queued — no channel to pace it under: neither the listing nor Twitch named its owner", "channel"); ch != nil {
		t.Errorf("the owner-less VOD's line names channel %q; nothing names one", ch)
	}

	if j, _ := db.GetJob("tw_v9"); j != nil {
		t.Errorf("the owner-less VOD was created: %+v", j)
	}
	chans, err := db.QueuedChannels()
	if err != nil || len(chans) != 1 || chans[0] != "alice" {
		t.Errorf("queued channels %v (%v), want alice's", chans, err)
	}

	// An insert that fails is counted and named too.
	db.Close()
	lg.reset()
	res = QueueTwitchVideos(db, []twitch.TwitchVideo{owned}, TwitchListingOptions{Login: "alice", Logger: lg}, time.Now())
	if res.Failed != 1 || len(res.Added) != 0 {
		t.Errorf("on a closed database: added %d, failed %d; want it failed", len(res.Added), res.Failed)
	}
	if vod, logged := lg.field("twitch listing: a VOD could not be queued — the job could not be created", "vod"); !logged || vod != "8" {
		t.Errorf("the failed insert logged %v (%v); lines %v", vod, logged, lg.lines())
	}
	// A collection's options name no login: the video's owner is the channel.
	lg.reset()
	QueueTwitchVideos(db, []twitch.TwitchVideo{owned}, TwitchListingOptions{Logger: lg}, time.Now())
	if ch, _ := lg.field("twitch listing: a VOD could not be queued — the job could not be created", "channel"); ch != "Alice" {
		t.Errorf("a collection VOD's failure names channel %v, want its owner Alice; lines %v", ch, lg.lines())
	}
}
