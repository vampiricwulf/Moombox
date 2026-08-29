package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// youtubeLooseOnlyCookieFile holds SAPISID but no LOGIN_INFO: the exact shape
// jar.go's youtubeAuthCookieNames doc comment calls "a configured platform
// with broken credentials". HasYouTubeAuthCookies (strict) REJECTS this file
// — it requires SAPISID (or __Secure-3PAPISID) AND LOGIN_INFO.
// HasAnyYouTubeAuthCookie (loose) accepts it. This is the H3 fixture: any
// test asserting detection off of it proves nothing unless the strict
// predicate would actually fail it.
const youtubeLooseOnlyCookieFile = `# Netscape HTTP Cookie File
.youtube.com	TRUE	/	TRUE	0	SAPISID	abc123sapisid
`

// twitchLooseOnlyCookieFile holds twilight-user but no auth-token — the
// Twitch counterpart to youtubeLooseOnlyCookieFile. HasTwitchAuthCookies
// (strict) reads GetTwitchAuthToken(), which this file leaves empty, so it
// rejects. HasAnyTwitchAuthCookie (loose) accepts it via twilight-user.
const twitchLooseOnlyCookieFile = `# Netscape HTTP Cookie File
.twitch.tv	TRUE	/	FALSE	0	twilight-user	someuser
`

// bothLooseOnlyCookieFile combines both fixtures above so a test can prove
// the sidecar overrides the jar guess rather than merely being consulted when
// the jar has nothing to say.
const bothLooseOnlyCookieFile = `# Netscape HTTP Cookie File
.youtube.com	TRUE	/	TRUE	0	SAPISID	abc123sapisid
.twitch.tv	TRUE	/	FALSE	0	twilight-user	someuser
`

// mustLoadJarFixture writes content to a temp cookies.txt and loads it into a
// fresh CookieJar. t.Fatal on any I/O or parse error — these fixtures are
// synthetic and hand-written, so a failure here is a bug in the test, not
// something to tolerate.
func mustLoadJarFixture(t *testing.T, content string) *cookies.CookieJar {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return jar
}

// TestDetectCookiePlatforms covers detectCookiePlatforms's two sources and
// the precedence between them (arc8-task-7). Each case's comment names the
// mutant it catches, per the brief's standing test rule.
func TestDetectCookiePlatforms(t *testing.T) {
	t.Run("sidecar present wins over jar even when jar says more", func(t *testing.T) {
		// Mutant: unioning the sidecar with the jar guess instead of letting
		// the sidecar decide outright. jar here loose-detects BOTH platforms;
		// if the implementation ever unions, this fails by returning both
		// instead of just youtube.
		meta := &cookies.CookieMeta{Platforms: []string{"youtube"}}
		jar := mustLoadJarFixture(t, bothLooseOnlyCookieFile)

		got, source := detectCookiePlatforms(meta, jar)

		if source != "sidecar" {
			t.Errorf("source = %q, want %q", source, "sidecar")
		}
		if !slices.Equal(got, []string{"youtube"}) {
			t.Errorf("platforms = %v, want [youtube] (sidecar must not be unioned with the jar guess)", got)
		}
	})

	t.Run("sidecar present but empty falls through to jar", func(t *testing.T) {
		// Mutant: treating a non-nil *CookieMeta as authoritative even when
		// its Platforms slice is empty, instead of checking len() > 0.
		meta := &cookies.CookieMeta{}
		jar := mustLoadJarFixture(t, youtubeLooseOnlyCookieFile)

		got, source := detectCookiePlatforms(meta, jar)

		if source != "cookie-names" {
			t.Errorf("source = %q, want %q", source, "cookie-names")
		}
		if !slices.Equal(got, []string{"youtube"}) {
			t.Errorf("platforms = %v, want [youtube]", got)
		}
	})

	t.Run("no sidecar, strict-false loose-true youtube file still detects youtube", func(t *testing.T) {
		// THE H3 CLAIM. Mutant: swapping HasAnyYouTubeAuthCookie for
		// HasYouTubeAuthCookies (loose for strict) in the implementation.
		// youtubeLooseOnlyCookieFile is strict-false by construction (no
		// LOGIN_INFO), so that swap makes this fail — the junction-defect
		// trap the brief calls out (~18): a fixture that is strict-true too
		// would let a strict-predicate regression pass silently.
		jar := mustLoadJarFixture(t, youtubeLooseOnlyCookieFile)
		if jar.HasYouTubeAuthCookies() {
			t.Fatal("fixture is broken: the strict predicate must reject this file for the test to prove anything")
		}

		got, source := detectCookiePlatforms(nil, jar)

		if source != "cookie-names" {
			t.Errorf("source = %q, want %q", source, "cookie-names")
		}
		if !slices.Equal(got, []string{"youtube"}) {
			t.Errorf("platforms = %v, want [youtube]", got)
		}
	})

	t.Run("no sidecar, strict-false loose-true twitch file still detects twitch", func(t *testing.T) {
		// Twitch counterpart to the H3 case above. Mutant: swapping
		// HasAnyTwitchAuthCookie for HasTwitchAuthCookies.
		jar := mustLoadJarFixture(t, twitchLooseOnlyCookieFile)
		if jar.HasTwitchAuthCookies() {
			t.Fatal("fixture is broken: the strict predicate must reject this file for the test to prove anything")
		}

		got, source := detectCookiePlatforms(nil, jar)

		if source != "cookie-names" {
			t.Errorf("source = %q, want %q", source, "cookie-names")
		}
		if !slices.Equal(got, []string{"twitch"}) {
			t.Errorf("platforms = %v, want [twitch]", got)
		}
	})

	t.Run("neither source has anything", func(t *testing.T) {
		// Mutant: returning a non-empty slice (e.g. a stray default) when
		// both the sidecar and the jar are empty.
		jar := cookies.NewCookieJar()

		got, _ := detectCookiePlatforms(nil, jar)

		if len(got) != 0 {
			t.Errorf("platforms = %v, want empty", got)
		}
	})
}
