package routes

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestUncheckedPayloadReachesTheCheckingBadge is the Go-to-JS seam of owner
// ruling R-H2: the handler's own payload for a status no check has answered
// yet reads "checking" on the dashboard for both platforms, whatever the
// re-login flag and the job list say, and the same status once checked reads
// as it always did. A renamed `checked` key on either side fails here.
//
// Mutants: drop "checked" from either payload (the badge reads no-cookies /
// anonymous); test `!status.checked` in utils.js instead of `=== false` (the
// older-binary row reads checking).
func TestUncheckedPayloadReachesTheCheckingBadge(t *testing.T) {
	vm := utilsVM(t)

	unchecked := cookies.AuthStatus{}
	for _, side := range []struct {
		platform string
		payload  map[string]any
	}{
		{"youtube", CookieStatusPayload(unchecked)},
		{"twitch", TwitchAuthStatusPayload(unchecked)},
	} {
		if side.payload["checked"] != false {
			t.Fatalf("%s: payload %v carries no checked=false", side.platform, side.payload)
		}
		for _, relogin := range []bool{false, true} {
			for _, parked := range []bool{false, true} {
				className, title := indicatorState(t, vm, side.platform, side.payload, relogin, parked)
				if className != "indicator-checking" {
					t.Errorf("%s (relogin %v, parked %v): className = %q (title %q), want indicator-checking",
						side.platform, relogin, parked, className, title)
				}
			}
		}
	}

	checked := cookies.AuthStatus{Checked: true, YouTubeVerification: cookies.RefreshFailed, TwitchVerification: cookies.RefreshFailed}
	if className, _ := indicatorState(t, vm, "youtube", CookieStatusPayload(checked), false); className != "indicator-warn" {
		t.Errorf("checked, nothing configured: youtube className = %q, want indicator-warn", className)
	}
	if className, _ := indicatorState(t, vm, "twitch", TwitchAuthStatusPayload(checked), false); className != "indicator-off" {
		t.Errorf("checked, nothing configured: twitch className = %q, want indicator-off", className)
	}

	// An older binary's payload has no `checked` key at all and must read as
	// it always did.
	older := CookieStatusPayload(checked)
	delete(older, "checked")
	if className, _ := indicatorState(t, vm, "youtube", older, false); className != "indicator-warn" {
		t.Errorf("no checked key: youtube className = %q, want indicator-warn", className)
	}
}
