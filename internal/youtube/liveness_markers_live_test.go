package youtube

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestLiveLoginMarkersPresent checks the premise the liveness arc is built
// on: that these two pages carry a marker watchPageSessionAuth can read.
//
// The anonymous half is the gate. LoggedOut is the ONLY verdict the arc
// acts on, so a page that answers Unknown to a signed-out request is
// useless for this purpose no matter what it does when authenticated.
//
// Enable with MOOMBOX_LIVE_YT_TEST=1 (matches extraction_live_test.go).
func TestLiveLoginMarkersPresent(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_YT_TEST") != "1" {
		t.Skip("set MOOMBOX_LIVE_YT_TEST=1 to run the live login-marker check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	headers := map[string]string{
		"User-Agent":      constants.UserAgents.Web,
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.5",
	}
	// A channel that offers memberships. Any will do — we are reading the
	// login marker, not the sponsorships tab.
	urls := map[string]string{
		"account feed":   constants.YouTubeURLs.Base + "/feed/subscriptions",
		"membership tab": constants.YouTubeURLs.Base + "/channel/UCBR8-60-B28hp2BmDPdntcQ/membership",
	}

	for name, u := range urls {
		body, err := utils.FetchBody(ctx, u, 20*time.Second, headers)
		if err != nil {
			t.Fatalf("%s: anonymous fetch failed: %v", name, err)
		}
		got := watchPageSessionAuth(string(body))
		if got != SessionAuthLoggedOut {
			t.Errorf("%s: anonymous verdict = %q, want LoggedOut.\n"+
				"The arc acts ONLY on LoggedOut. If this page cannot produce it, "+
				"pick a different page before building Tasks 3-6.", name, got)
		}
	}
}
