package monitor

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestFeedStatusForAndTheTerminalSet pins the one post_live → vod mapping the
// feed store and DECAPI's memo share, and the one terminal set settledTerminal
// and the memo share (they were two copies, kept equal by a comment). The
// stored bytes are the feed_items values they always were.
func TestFeedStatusForAndTheTerminalSet(t *testing.T) {
	for _, tc := range []struct {
		in       youtube.StreamStatus
		stored   string
		terminal bool
	}{
		{youtube.StreamLive, "live", false},
		{youtube.StreamUpcoming, "upcoming", false},
		{youtube.StreamVOD, "vod", true},
		{youtube.StreamPostLive, "vod", true},
		{youtube.StreamNotAStream, "not_a_stream", true},
		{"", "", false},
	} {
		got := feedStatusFor(tc.in)
		if got != tc.stored {
			t.Errorf("feedStatusFor(%q) = %q, want %q", tc.in, got, tc.stored)
		}
		if isTerminalFeedStatus(got) != tc.terminal {
			t.Errorf("isTerminalFeedStatus(%q) = %v, want %v", got, !tc.terminal, tc.terminal)
		}
	}
	if isTerminalFeedStatus(database.FeedStatusUnknown) {
		t.Error("unknown is not a classification, so never terminal")
	}
	for _, row := range []struct {
		status, recheck string
		want            bool
	}{
		{database.FeedStatusVOD, "", true},
		{database.FeedStatusNotAStream, "", true},
		{database.FeedStatusVOD, "2026-11-09T00:00:00Z", false},
		{database.FeedStatusUpcoming, "", false},
		{database.FeedStatusUnknown, "2026-11-09T00:00:00Z", false},
	} {
		if got := settledTerminal(database.FeedItem{Status: row.status, RecheckAt: row.recheck}); got != row.want {
			t.Errorf("settledTerminal(%s, recheck %q) = %v, want %v", row.status, row.recheck, got, row.want)
		}
	}
}
