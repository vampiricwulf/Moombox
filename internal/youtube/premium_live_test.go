package youtube

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestLivePremiumVerdict is the YouTube half of the live-test runner's
// session probe (.github/scripts/live-tests.sh): it fetches a watch page with
// the session the way the extraction cascade does (Auth.PageCookieHeader into
// FetchWatchPage) and reports the two verdicts that page carries, YouTube's
// own LOGGED_IN marker and the Premium topbar logo (WatchPageResult.IsPremium,
// yt-dlp's _is_premium_subscriber), on one machine line the runner reads:
//
//	LIVE-SESSION youtube=<signed_in|not_signed_in|unknown|no_cookies|no_auth_cookie|fetch_failed> premium=<yes|no|unknown>
//
// Premium is "unknown" unless the page says signed in: the logo is read on a
// signed-in page only. The test fails when the session is not signed in, and,
// with MOOMBOX_LIVE_YT_EXPECT_PREMIUM=yes|no, when the Premium verdict is not
// the one the account is known to have — the only way a renamed logo shows,
// since it reads as a plain "no".
//
// Enable with MOOMBOX_LIVE_YT_COOKIES=<path to a Netscape cookie file for a
// signed-in YouTube session>. Nothing from the file is printed; the reply's
// renewed cookies are dropped (nothing offers them to a store).
func TestLivePremiumVerdict(t *testing.T) {
	path := os.Getenv("MOOMBOX_LIVE_YT_COOKIES")
	if path == "" {
		t.Log("LIVE-SESSION youtube=no_cookies premium=unknown")
		t.Skip("set MOOMBOX_LIVE_YT_COOKIES=<path to a signed-in Netscape cookie file> to run the session and Premium probe")
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Log("LIVE-SESSION youtube=no_cookies premium=unknown")
		t.Fatalf("load cookie file: %v", err)
	}
	svc := NewService(jar, noopLogger{})
	if !svc.HasAnyAuthCookie() {
		t.Log("LIVE-SESSION youtube=no_auth_cookie premium=unknown")
		t.Fatal("that cookie file carries no YouTube auth cookie")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Big Buck Bunny, as TestLivePublicExtraction: stable and unrestricted.
	wp, err := FetchWatchPage(ctx, "aqz-KE-bpKQ", svc.Auth.PageCookieHeader())
	if err != nil {
		t.Log("LIVE-SESSION youtube=fetch_failed premium=unknown")
		t.Fatalf("watch page fetch failed: %.300v", err)
	}

	session, premium := "unknown", "unknown"
	switch wp.SessionAuth {
	case SessionAuthLoggedIn:
		session, premium = "signed_in", "no"
		if wp.IsPremium {
			premium = "yes"
		}
	case SessionAuthLoggedOut:
		session = "not_signed_in"
	}
	t.Logf("LIVE-SESSION youtube=%s premium=%s", session, premium)

	if wp.SessionAuth != SessionAuthLoggedIn {
		t.Errorf("watch page session verdict = %q, want %q: the cookies are not signed in, "+
			"or the LOGGED_IN marker changed shape", wp.SessionAuth, SessionAuthLoggedIn)
	}
	if want := os.Getenv("MOOMBOX_LIVE_YT_EXPECT_PREMIUM"); want != "" && want != premium {
		t.Errorf("Premium verdict = %s, want %s (MOOMBOX_LIVE_YT_EXPECT_PREMIUM): if the account's "+
			"subscription did not change, the topbar logo extractPremiumSubscriber reads has", premium, want)
	}
}
