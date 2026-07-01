package engine

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// TestDownloadUAsTrackConstants enforces the lockstep invariant on the
// engine's init-time UA captures: with the desktop Web UA randomized once
// per process start, every consumer must observe the SAME value — a
// re-hardcoded uaWeb would silently split the download fingerprint from the
// watch-page/BotGuard/cookie-refresh fingerprint (a bot-detection signal).
// Mirrors bgutils' TestUserAgentFullTracksConstant.
func TestDownloadUAsTrackConstants(t *testing.T) {
	if uaWeb != constants.UserAgents.Web {
		t.Errorf("uaWeb drifted from constants.UserAgents.Web:\n  uaWeb = %q\n  constants.Web = %q",
			uaWeb, constants.UserAgents.Web)
	}
	if uaAndroid != constants.UserAgents.Android {
		t.Errorf("uaAndroid drifted from constants.UserAgents.Android:\n  uaAndroid = %q\n  constants.Android = %q",
			uaAndroid, constants.UserAgents.Android)
	}
}
