package monitor

import (
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// feedStatusFor is the feed_items status a probe classification is stored
// and remembered as: post_live is a broadcast the probe found over, so it is
// the vod it will be, and every other classification is its own value. The
// feed walk's applyProbe stores it and DECAPI's terminal memo keeps it, so
// the two agree on what an ended broadcast is: recorded raw, post_live was
// never terminal and DECAPI probed a channel's ended newest video every 15 s.
// The empty status (no probe ran) stays empty.
func feedStatusFor(s youtube.StreamStatus) string {
	if s == youtube.StreamPostLive {
		return database.FeedStatusVOD
	}
	return string(s)
}

// isTerminalFeedStatus reports whether a stored (or memoised) classification
// can no longer change: vod or not_a_stream. Upcoming becomes live becomes
// vod; unknown is not a classification. The feed walk's settledTerminal adds
// "no re-check pending" to it, and DECAPI's memo answers a pending re-check
// through its recheckAt instead.
func isTerminalFeedStatus(status string) bool {
	return status == database.FeedStatusVOD || status == database.FeedStatusNotAStream
}
