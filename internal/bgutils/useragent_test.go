package bgutils

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// TestUserAgentFullTracksConstant enforces the lockstep invariant: the UA
// the BotGuard VM reports as navigator.userAgent and the UA sent on every
// PO-token HTTP request must equal the canonical desktop Web UA. If a future
// edit re-hardcodes UserAgentFull, the VM self-view and the server-observed
// fingerprint can drift apart (a bot-detection signal) — this fails loudly.
func TestUserAgentFullTracksConstant(t *testing.T) {
	if UserAgentFull != constants.UserAgents.Web {
		t.Errorf("UserAgentFull drifted from constants.UserAgents.Web:\n  UserAgentFull = %q\n  constants.Web = %q",
			UserAgentFull, constants.UserAgents.Web)
	}
}
