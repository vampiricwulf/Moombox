package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

type stubCLILister struct {
	videos  []twitch.TwitchVideo
	gotType string
	gotColl string
}

func (s *stubCLILister) ListChannelVideos(_ context.Context, _, broadcastType, _ string) ([]twitch.TwitchVideo, error) {
	s.gotType = broadcastType
	return s.videos, nil
}

func (s *stubCLILister) CollectionVideos(_ context.Context, id string) (string, []twitch.TwitchVideo, error) {
	s.gotColl = id
	return "", s.videos, nil
}

// TestQueueCLITwitchListing is owner decision D18 on `moombox add`: a pasted
// video list is read with its ?filter= type and each VOD queued Queued, a
// collection is read as the collection, and a channel's collection list is
// refused as on every add path.
//
// Mutants: the filter not read from the input (gotType ""); the
// ?filter=collections refusal removed (rows created); a collection read as
// a channel list (gotColl "").
func TestQueueCLITwitchListing(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	lister := &stubCLILister{videos: []twitch.TwitchVideo{{ID: "11", Title: "x"}, {ID: "12", Title: "y"}}}
	ctx := context.Background()
	now := time.Now()

	in := "https://www.twitch.tv/spamfish/videos?filter=highlights"
	res, err := queueCLITwitchListing(ctx, lister, db, []config.ChannelConfig{}, in, utils.ExtractTwitchTarget(in), now)
	if err != nil || len(res.Added) != 2 || lister.gotType != "HIGHLIGHT" {
		t.Fatalf("video list: %+v, %v, type %q", res, err, lister.gotType)
	}
	if j, _ := db.GetJob("tw_v11"); j == nil || j.Status != database.StatusQueued {
		t.Errorf("tw_v11 = %+v, want Queued", j)
	}

	in = "https://www.twitch.tv/spamfish/videos?filter=collections"
	lister.videos = []twitch.TwitchVideo{{ID: "21"}}
	if _, err := queueCLITwitchListing(ctx, lister, db, nil, in, utils.ExtractTwitchTarget(in), now); err == nil || err.Error() != utils.TwitchCollectionsListingRefusal {
		t.Errorf("collection list: err %v, want the refusal", err)
	}
	if db.JobExists("tw_v21") {
		t.Error("a refused collection list queued a VOD")
	}

	in = "https://www.twitch.tv/collections/abc"
	if _, err := queueCLITwitchListing(ctx, lister, db, nil, in, utils.ExtractTwitchTarget(in), now); err != nil || lister.gotColl != "abc" {
		t.Errorf("collection: err %v, read %q", err, lister.gotColl)
	}
}
