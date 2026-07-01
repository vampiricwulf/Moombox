package constants

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// chromeMajorFloor guards against the desktop Web User-Agent silently
// rotting back to a stale Chrome version (it was frozen at Chrome/131 for
// ~18 months before anyone noticed). yt-dlp's random_user_agent() targets
// "versions released within the last ~6 months" — its floor was 142 as of
// 2026-05. We use a slightly looser floor here so a deliberate, current
// pick never trips the test, while a regression to an old major (e.g. 131)
// fails loudly. Bump this alongside UserAgents.Web.
const chromeMajorFloor = 140

var chromeUAPattern = regexp.MustCompile(`AppleWebKit/537\.36 \(KHTML, like Gecko\) Chrome/(\d+)\.\d+\.\d+\.\d+ Safari/537\.36`)

func TestWebUserAgentChromeMajorNotStale(t *testing.T) {
	m := chromeUAPattern.FindStringSubmatch(UserAgents.Web)
	if m == nil {
		t.Fatalf("UserAgents.Web does not look like a desktop Chrome UA: %q", UserAgents.Web)
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("could not parse Chrome major from %q: %v", UserAgents.Web, err)
	}
	if major < chromeMajorFloor {
		t.Errorf("UserAgents.Web Chrome major %d is stale (floor %d) — bump it toward yt-dlp's current random_user_agent() window: %q",
			major, chromeMajorFloor, UserAgents.Web)
	}
}

// TestWebUserAgentIsWindowsDesktop locks in the platform token so a future
// edit can't accidentally swap the canonical desktop UA for a mobile or
// non-Windows string (the BotGuard/PO-token path relies on it presenting a
// consistent desktop Chrome fingerprint).
func TestWebUserAgentIsWindowsDesktop(t *testing.T) {
	if !strings.Contains(UserAgents.Web, "Windows NT 10.0; Win64; x64") {
		t.Errorf("UserAgents.Web is no longer a Windows x64 desktop UA: %q", UserAgents.Web)
	}
}

// TestRandomizedWebUAStaysInWindow pins the randomization contract: every
// generated UA parses as a desktop Chrome UA whose major falls inside the
// declared [chromeMajorMin, chromeMajorMax] window — a value outside it
// (stale or not-yet-released) is exactly the implausible fingerprint the
// randomization exists to avoid. UserAgents.Web itself is generated once at
// init and must never be re-rolled mid-process (BotGuard's navigator.userAgent
// and the HTTP layer must agree), which the var-initializer shape guarantees.
func TestRandomizedWebUAStaysInWindow(t *testing.T) {
	if chromeMajorMin > chromeMajorMax {
		t.Fatalf("window inverted: min %d > max %d", chromeMajorMin, chromeMajorMax)
	}
	for range 50 {
		ua := randomizedWebUA()
		m := chromeUAPattern.FindStringSubmatch(ua)
		if m == nil {
			t.Fatalf("randomizedWebUA() is not a desktop Chrome UA: %q", ua)
		}
		major, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("could not parse Chrome major from %q: %v", ua, err)
		}
		if major < chromeMajorMin || major > chromeMajorMax {
			t.Errorf("Chrome major %d outside window [%d, %d]: %q",
				major, chromeMajorMin, chromeMajorMax, ua)
		}
	}
}

// nativeAppMajorFloor guards the native YouTube app clients (IOS / ANDROID)
// against rotting back to an old app version the way the iOS client was
// frozen at 19.29.1. Track yt-dlp's INNERTUBE_CLIENTS (ios/android were
// 21.x as of 2026-05). Bump alongside the client versions.
const nativeAppMajorFloor = 20

var appVersionPattern = regexp.MustCompile(`youtube/(\d+)\.`)

func appMajor(t *testing.T, ua string) int {
	t.Helper()
	m := appVersionPattern.FindStringSubmatch(ua)
	if m == nil {
		t.Fatalf("could not find a youtube/<ver> token in %q", ua)
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("could not parse app major from %q: %v", ua, err)
	}
	return major
}

func TestNativeAppUserAgentsNotStale(t *testing.T) {
	for name, ua := range map[string]string{"IOS": UserAgents.IOS, "Android": UserAgents.Android} {
		if major := appMajor(t, ua); major < nativeAppMajorFloor {
			t.Errorf("%s app UA major %d is stale (floor %d): %q", name, major, nativeAppMajorFloor, ua)
		}
	}
}

// TestIOSClientInternallyConsistent ensures the iOS client version is not
// updated in one place and forgotten in another — the UA, the struct field,
// and the Innertube context must all agree, or YouTube sees a contradictory
// iOS fingerprint.
func TestIOSClientInternallyConsistent(t *testing.T) {
	if IOSClient.ClientVersion != IOSClient.Context["clientVersion"] {
		t.Errorf("IOSClient.ClientVersion %q != Context[clientVersion] %v",
			IOSClient.ClientVersion, IOSClient.Context["clientVersion"])
	}
	if !strings.Contains(UserAgents.IOS, IOSClient.ClientVersion) {
		t.Errorf("UserAgents.IOS %q does not contain client version %q", UserAgents.IOS, IOSClient.ClientVersion)
	}
}
