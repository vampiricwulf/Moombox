package youtube

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestLiveLoginMarkersPresent checks the premise the liveness arc is built
// on: that these two pages carry a marker livenessVerdict can read.
//
// The anonymous half is the gate. LoggedOut is the ONLY verdict the arc
// acts on, so a page that answers Unknown to a signed-out request is
// useless for this purpose no matter what it does when authenticated.
//
// livenessVerdict, not watchPageSessionAuth, is what production actually
// calls on this path (Tasks 4-6). Pinning the string version alone would
// miss a precise and severe failure: if YouTube ever drops the explicit
// "LOGGED_IN": key but keeps the ytcfg bootstrap, watchPageSessionAuth's
// fallback still returns LoggedOut and this test would keep passing, while
// livenessVerdict — which deliberately has no such fallback, see its
// doc comment — returns Unknown in production and the whole arc goes
// silent. So livenessVerdict is asserted directly; watchPageSessionAuth is
// asserted alongside it only to keep its own coverage of these two pages.
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
		if got := livenessVerdict(body); got != SessionAuthLoggedOut {
			t.Errorf("%s: anonymous livenessVerdict = %q, want LoggedOut.\n"+
				"The arc acts ONLY on LoggedOut. If this page cannot produce it, "+
				"pick a different page before building Tasks 3-6.", name, got)
		}
		if got := watchPageSessionAuth(string(body)); got != SessionAuthLoggedOut {
			t.Errorf("%s: anonymous watchPageSessionAuth = %q, want LoggedOut.", name, got)
		}
	}
}

// TestLiveAuthenticatedAccountProbe is the other half of the premise, and it
// closes a gate the anonymous arm above structurally cannot.
//
// The detector reads a bounded set of value forms — bare or quoted
// true/false, whitespace-tolerant after the colon — and answers Unknown for
// anything else (see sessionAuthValue). It used to require the exact byte
// sequence `"LOGGED_IN":true`, so a single added space made an AUTHENTICATED
// page read as SessionAuthLoggedOut and every healthy install would have been
// told its cookies were dead. That specific hole is closed in the detector
// itself, which is a better remedy than detecting the drift after it lands.
//
// This test still earns its place, because the remaining drift is the shape
// the detector CANNOT be made immune to: a marker renamed, dropped, or moved
// somewhere the key scan does not match. That now reads as Unknown rather
// than LoggedOut — the arc goes quiet instead of raising a false alarm, which
// is the right direction but is still a failure, and a silent one.
// TestLiveLoginMarkersPresent structurally cannot see it: it fetches
// anonymously, where LoggedOut is the answer it wants, so it would keep
// passing throughout.
//
// This runs the real production path — ProbeAccountLiveness against
// accountProbeURL, both admissibility guards included — so it also covers a
// live consent or login redirect appearing on the probe page.
//
// Enable with MOOMBOX_LIVE_YT_COOKIES=<path to a Netscape cookie file for a
// signed-in YouTube session>. The path alone is the opt-in; no second flag.
// The cookie file is read by the jar and never printed: this test asserts a
// verdict and reports presence, never any cookie value, and the Service is
// built with noopLogger so the probe's own logging is discarded too.
func TestLiveAuthenticatedAccountProbe(t *testing.T) {
	path := os.Getenv("MOOMBOX_LIVE_YT_COOKIES")
	if path == "" {
		t.Skip("set MOOMBOX_LIVE_YT_COOKIES=<path to a signed-in Netscape cookie file> to run the authenticated liveness check")
	}

	jar := cookies.NewCookieJar()
	// Load reports only the path on failure, never file contents.
	if err := jar.Load(path); err != nil {
		t.Fatalf("load cookie file: %v", err)
	}
	svc := NewService(jar, noopLogger{})
	if !svc.HasAnyAuthCookie() {
		t.Fatal("that cookie file carries no YouTube auth cookie — check the export, or that the path is the right file")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	got, err := svc.ProbeAccountLiveness(ctx)
	if err != nil {
		t.Fatalf("authenticated probe of %s failed: %v", accountProbeURL, err)
	}
	if got != SessionAuthLoggedIn {
		t.Errorf("authenticated ProbeAccountLiveness = %q, want %q.\n"+
			"If the cookies really are live, the marker YouTube stamps has changed shape "+
			"(spelling, or it is gone) and livenessVerdict can no longer read it. Unknown "+
			"here means every install's liveness check has gone silent; LoggedOut here "+
			"means it is actively alarming on healthy credentials, which is worse.", got, SessionAuthLoggedIn)
	}
}
