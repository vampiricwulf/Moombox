package youtube

import (
	"context"
	"testing"
)

// The same-itag tie-break follows yt-dlp's requested-client order, where the
// two can be matched: its first-seen stream id wins, and the authed tuple is
// (web_embedded, tv_samsung, web), the public one (tv_samsung, visionos, web)
// (owner decision, 2026-10-14). The cookieless clients stay last on both,
// against the public tuple's visionos — the 2026-08-15 incident.
//
// Mutants: tv back ahead of web_embedded on the authed path (every cookied
// recording moves onto the Samsung-identity TV URLs, an identity upstream
// ranks second and calls a workaround); a web-family level ahead of tv; a
// cookieless level ahead of any cookied one.
func TestSameItagTieFollowsUpstreamsClientOrder(t *testing.T) {
	order := func(name string, sources ...string) {
		t.Helper()
		levels := map[string]int{
			"web_embedded": AuthLevelWebEmbedded, "tv_auth": AuthLevelTVAuth,
			"watch_page_auth": AuthLevelWatchPageAuth, "web_safari": AuthLevelWebSafari, "web": AuthLevelWeb,
			"web_creator": AuthLevelWebCreator, "tv_public": AuthLevelTVPublic,
			"watch_page_public": AuthLevelWatchPagePublic, "visionos": AuthLevelVisionOS, "android_vr": AuthLevelAndroidVR,
		}
		// Each round drops the previous winner; the pool is listed WORST
		// first, so a tie-break that kept the first-seen copy rather than the
		// lowest level is caught.
		for want := range sources {
			rest := sources[want:]
			pool := make([]Format, 0, len(rest))
			for i := len(rest) - 1; i >= 0; i-- {
				l := levels[rest[i]]
				pool = append(pool, Format{Itag: 137, URL: "https://" + rest[i] + "/137", MimeType: `video/mp4; codecs="avc1"`, Source: rest[i], AuthLevel: &l})
			}
			got := deduplicateFormats(context.Background(), pool)
			if len(got) != 1 || got[0].Source != rest[0] {
				t.Errorf("%s: with %v in the pool the winner is %s, want %s", name, rest, got[0].Source, rest[0])
			}
		}
	}
	order("authed", "web_embedded", "tv_auth", "watch_page_auth", "web_safari", "web", "web_creator", "visionos", "android_vr")
	order("public", "tv_public", "watch_page_public", "visionos", "android_vr")
}
