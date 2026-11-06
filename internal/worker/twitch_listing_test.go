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
// Mutant: the guard removed (a Queued row with a NULL channel_id is added).
func TestQueueTwitchVideosRefusesAnOwnerlessVod(t *testing.T) {
	db := openTestDB(t)
	owned := twitch.TwitchVideo{ID: "8", Title: "a", PublishedAt: "2026-10-01T00:00:00Z"}
	owned.Owner = &struct {
		Login       string `json:"login"`
		DisplayName string `json:"displayName"`
	}{Login: "Alice"}
	res := QueueTwitchVideos(db, []twitch.TwitchVideo{{ID: "9", Title: "b", PublishedAt: "2026-10-01T00:00:00Z"}, owned},
		TwitchListingOptions{ManuallyAdded: true}, time.Now())
	if len(res.Added) != 1 || res.Failed != 1 {
		t.Fatalf("added %d, failed %d; want the owned VOD added and the owner-less one failed", len(res.Added), res.Failed)
	}
	if j, _ := db.GetJob("tw_v9"); j != nil {
		t.Errorf("the owner-less VOD was created: %+v", j)
	}
	chans, err := db.QueuedChannels()
	if err != nil || len(chans) != 1 || chans[0] != "alice" {
		t.Errorf("queued channels %v (%v), want alice's", chans, err)
	}
}
