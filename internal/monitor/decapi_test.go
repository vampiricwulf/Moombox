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
)

// newTestDecapiMonitor builds a DecapiMonitor over a real (temp-file) db and
// an in-memory config store — the DECAPI counterpart of newTestFeedMonitor.
// Tests drive processResponse directly with a synthesized DECAPI body; the
// HTTP layer (checkChannel) is not under test here.
func newTestDecapiMonitor(t *testing.T, db *database.Database, probe VideoProbeFunc) *DecapiMonitor {
	t.Helper()
	dm := NewDecapiMonitor(
		config.NewStore(config.Defaults(), ""),
		db,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	dm.ProbeVideo = probe
	return dm
}

// recordDecapiVideoFound wires dm.OnVideoFound to a recorder and returns a
// pointer to the recorded calls, in emission order (archive_test.go's
// recordVideoFound, for the DECAPI monitor).
func recordDecapiVideoFound(dm *DecapiMonitor) *[]foundCall {
	calls := &[]foundCall{}
	dm.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig, d JobDisposition) {
		*calls = append(*calls, foundCall{videoID: videoID, d: d})
	}
	return calls
}

// decapiBody renders the DECAPI latest_video response shape processResponse
// parses: "<title> - <video URL>". videoID must be 11 chars (the real ID
// shape decapiVideoIDRe captures).
func decapiBody(videoID, title string) string {
	return fmt.Sprintf("%s - https://youtu.be/%s", title, videoID)
}

// TestDecapi_VodOutsideWindowNotJobbed covers §13's date check: "the newest
// video on the channel" is not the same as "recent" — on a dormant channel
// with include_non_live_content=true, DECAPI's first cycle must not job a
// six-month-old VOD against the default 3-day window (the headline bug
// through a second door).
//
// The DECAPI monitor has no injectable clock — the window check reads
// time.Now() directly — so the fixture's published date sits 6 months behind
// the 3-day window: far enough from the boundary that wall-clock skew cannot
// flip the assertion.
func TestDecapi_VodOutsideWindowNotJobbed(t *testing.T) {
	db := newTestDB(t)
	published := time.Now().UTC().Add(-6 * 30 * 24 * time.Hour).Format(time.RFC3339)
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		return &VideoProbeResult{
			StreamStatus: "vod", Title: "old vod",
			PublishedAt: published, PublishedPrecision: "day",
		}, nil
	})
	found := recordDecapiVideoFound(dm)

	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1", IncludeNonLiveContent: true}
	if err := dm.processResponse(context.Background(), decapiBody("vidDecOld01", "old vod"), ch); err != nil {
		t.Fatalf("processResponse: %v", err)
	}
	if len(*found) != 0 {
		t.Fatalf("a 6-month-old VOD was jobbed against a 3-day window: %v", *found)
	}
}

// TestDecapi_LiveNeverWindowBlocked covers §13's other half: live/upcoming
// job ALWAYS, no date check — this IS the RSS redundancy, and a date must
// never block it. A broadcast probe supplies no date at all (§12), so a
// window rule that consulted the (empty) date here would block every live
// stream DECAPI exists to catch.
func TestDecapi_LiveNeverWindowBlocked(t *testing.T) {
	db := newTestDB(t)
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		return &VideoProbeResult{StreamStatus: "live", Title: "going live"}, nil // dateless, like every broadcast probe
	})
	found := recordDecapiVideoFound(dm)

	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1"}
	if err := dm.processResponse(context.Background(), decapiBody("vidDecLiv02", "going live"), ch); err != nil {
		t.Fatalf("processResponse: %v", err)
	}
	if len(*found) != 1 || (*found)[0].videoID != "vidDecLiv02" || (*found)[0].d != DispositionBroadcast {
		t.Fatalf("found = %v, want [{vidDecLiv02 broadcast}] — a date must never block the redundancy (§13)", *found)
	}
}

// TestDecapi_DatelessVodTreatedAsOutside: a vod-family probe with no date
// cannot verify the window ⇒ treated as outside, no job. Unlike the feed
// path (§12's terminal invariant keeps such a row 'unknown' in the store to
// self-heal on a later dated probe), DECAPI writes no feed_items row, so
// there is nothing to heal — the skip is final for this sighting.
func TestDecapi_DatelessVodTreatedAsOutside(t *testing.T) {
	db := newTestDB(t)
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		return &VideoProbeResult{StreamStatus: "vod", Title: "dateless vod"}, nil
	})
	found := recordDecapiVideoFound(dm)

	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1", IncludeNonLiveContent: true}
	if err := dm.processResponse(context.Background(), decapiBody("vidDecNod03", "dateless vod"), ch); err != nil {
		t.Fatalf("processResponse: %v", err)
	}
	if len(*found) != 0 {
		t.Fatalf("a dateless VOD was jobbed: %v — the window cannot be verified, and DECAPI has no store row to self-heal from", *found)
	}
}
