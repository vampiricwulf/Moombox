package worker

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobfilter"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// TestOneClipPredicateAnswersAsBothDid (K16): there were two clip
// predicates — jobfilter.IsClipJob (the ID's prefix alone) and the worker's
// IsTwitchClipJob (platform twitch and a slug in the video ID). The worker's
// was the stricter, and jobfilter.IsClipJob is now it, read off the ID; the
// worker's is gone. Both old forms are re-stated verbatim here and compared
// with it over every job-ID shape a creation path writes — ID and video ID
// equal, platform "twitch" for every tw_ ID — where all three agree. The
// shapes no path writes (an empty slug, a clip ID on a YouTube row) are
// where the old ID-only form said "clip" and the predicate kept does not.
func TestOneClipPredicateAnswersAsBothDid(t *testing.T) {
	oldFilter := func(j *database.Job) bool {
		return j != nil && strings.HasPrefix(j.ID, twitch.ClipJobIDPrefix)
	}
	oldWorker := func(j *database.Job) bool {
		if j == nil || j.Platform != "twitch" {
			return false
		}
		_, ok := twitch.ClipSlugFromJobID(j.VideoID)
		return ok
	}
	written := []struct {
		id, platform string
		clip         bool
	}{
		{twitch.ClipJobID("FaintLightGullWholeWheat"), "twitch", true},
		{twitch.ClipJobID("VibrantCatLikeAVod"), "twitch", true}, // a slug starting with "v"
		{twitch.VodJobID("123456789"), "twitch", false},
		{"tw_987654321", "twitch", false},        // a live capture
		{"tw_manual_somelogin", "twitch", false}, // a manual live add
		{"somelogin", "twitch", false},           // a channel add before the worker resolves it
		{"dQw4w9WgXcQ", "youtube", false},
		{"import_1a2b3c", "youtube", false},
	}
	for _, s := range written {
		j := &database.Job{ID: s.id, VideoID: s.id, Platform: s.platform}
		got := jobfilter.IsClipJob(j)
		if got != s.clip || oldFilter(j) != s.clip || oldWorker(j) != s.clip {
			t.Errorf("%s (%s): IsClipJob %v, old jobfilter %v, old worker %v, want %v",
				s.id, s.platform, got, oldFilter(j), oldWorker(j), s.clip)
		}
	}
	for _, j := range []*database.Job{
		nil,
		{ID: twitch.ClipJobIDPrefix, VideoID: twitch.ClipJobIDPrefix, Platform: "twitch"},
		{ID: twitch.ClipJobID("Slug"), VideoID: twitch.ClipJobID("Slug"), Platform: "youtube"},
	} {
		if jobfilter.IsClipJob(j) || oldWorker(j) {
			t.Errorf("unwritten shape %+v read as a clip", j)
		}
	}
}

// TestVodJobIDRoundTrips pins the VOD prefix helpers the literals became.
func TestVodJobIDRoundTrips(t *testing.T) {
	if id := twitch.VodJobID("42"); id != "tw_v42" {
		t.Errorf("VodJobID(42) = %q", id)
	}
	if v, ok := twitch.VodIDFromJobID("tw_v42"); !ok || v != "42" {
		t.Errorf("VodIDFromJobID(tw_v42) = %q, %v", v, ok)
	}
	for _, id := range []string{"tw_v", "tw_42", twitch.ClipJobID("vSlug"), "v42"} {
		if _, ok := twitch.VodIDFromJobID(id); ok {
			t.Errorf("VodIDFromJobID(%q) read a VOD", id)
		}
	}
}
