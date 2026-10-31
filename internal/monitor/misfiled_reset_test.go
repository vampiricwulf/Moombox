package monitor

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// upgradedFeedStore is a feed store a v23 binary filled — its status probe
// filing a refusal that said nothing not_a_stream — opened by this one:
// seeded, put back to v23's shape (no feed_reprobe_owed, user_version 23)
// and reopened, so Open replays the v24 block that owes the reset.
func upgradedFeedStore(t *testing.T, seed func(db *database.Database)) *database.Database {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v23.db")
	db, err := database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seed(db)
	db.Close()
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE feed_reprobe_owed`, `PRAGMA user_version = 23`} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("make a v23-shaped database (%s): %v", q, err)
		}
	}
	raw.Close()
	db, err = database.Open(path)
	if err != nil {
		t.Fatalf("v23 -> v24 upgrade: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestFeed_TheUpgradeProbesEachMisfiledRowOnce is the owner's W6 follow-up
// (2026-10-31: reset once on upgrade), end to end through a feed cycle.
// Before the inconclusive rule the bot wall's refusal was stored
// not_a_stream, which the walk never probes again, so a stream it hid was
// never recorded. On the first cycle after the upgrade the channel's
// not_a_stream rows inside the walk's scope go back to unknown and that same
// walk probes each once: the misfiled stream is found and announced, the
// genuine upload answers not_a_stream and settles again, and the upload
// dated before the window is neither reset nor probed. The next cycle
// probes no settled row again: the reset is once.
//
// Mutants: checkChannel not making the reset (nothing is probed, nothing
// found); making it with a cutoff of "" (oldUpload is reset to unknown);
// ResetMisfiledFeedItems never clearing the debt (genuineUpload, settled
// again, is reset and probed in cycle 2).
func TestFeed_TheUpgradeProbesEachMisfiledRowOnce(t *testing.T) {
	now := fixedNow()
	db := upgradedFeedStore(t, func(db *database.Database) {
		seedRow(t, db, "UC1", "misfiledStream", now.Add(-24*time.Hour), "exact", "rss", "not_a_stream")
		seedRow(t, db, "UC1", "genuineUpload", now.Add(-48*time.Hour), "exact", "rss", "not_a_stream")
		seedRow(t, db, "UC1", "oldUpload", now.Add(-60*24*time.Hour), "exact", "rss", "not_a_stream")
	})

	var probed []string
	probe := func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		probed = append(probed, videoID)
		if videoID == "misfiledStream" {
			return &VideoProbeResult{StreamStatus: "upcoming", Title: videoID}, nil
		}
		return &VideoProbeResult{StreamStatus: "not_a_stream", Title: videoID}, nil
	}
	cycle := func(at time.Time) []foundCall {
		t.Helper()
		fm := newTestFeedMonitor(t, db, withRSS(rssWith()), withWindowDays(30), withProbe(probe), withNow(at))
		found := recordVideoFound(fm)
		if err := fm.checkChannel(context.Background(), &config.ChannelConfig{ID: "UC1", Name: "UC1"}); err != nil {
			t.Fatalf("checkChannel: %v", err)
		}
		return *found
	}

	found := cycle(now)
	if !slices.Equal(probed, []string{"misfiledStream", "genuineUpload"}) {
		t.Errorf("first cycle probed %v, want [misfiledStream genuineUpload]: each in-scope misfiled row once", probed)
	}
	if len(found) != 1 || found[0].videoID != "misfiledStream" || found[0].d != DispositionBroadcast {
		t.Errorf("first cycle found %v, want misfiledStream as a broadcast", found)
	}
	for id, want := range map[string]string{"misfiledStream": "upcoming", "genuineUpload": "not_a_stream", "oldUpload": "not_a_stream"} {
		if got := mustGetFeedItem(t, db, "UC1", id).Status; got != want {
			t.Errorf("%s = %q after the first cycle, want %q", id, got, want)
		}
	}

	probed = nil
	cycle(now.Add(10 * time.Minute))
	if slices.Contains(probed, "genuineUpload") || slices.Contains(probed, "oldUpload") {
		t.Errorf("second cycle probed %v: a settled not_a_stream row is never probed again, the reset is once", probed)
	}
}
