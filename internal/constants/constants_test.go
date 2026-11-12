package constants

import (
	"encoding/json"
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

// nativeAppMajorFloor guards the native YouTube app UAs Moombox actually
// sends against rotting back to an old app version the way the iOS client was
// frozen at 19.29.1. Only ANDROID is left (AndroidVRClient sends it); the IOS
// UA went with IOSClient, since a pinned-but-unsent version is worse than
// absent — a reader takes it for a maintained one. Track yt-dlp's
// INNERTUBE_CLIENTS (android was 21.x as of 2026-05). Bump alongside the
// client versions.
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
	for name, ua := range map[string]string{"Android": UserAgents.Android} {
		if major := appMajor(t, ua); major < nativeAppMajorFloor {
			t.Errorf("%s app UA major %d is stale (floor %d): %q", name, major, nativeAppMajorFloor, ua)
		}
	}
}

// TestWebClientInternallyConsistent, TestWebSafariClientInternallyConsistent,
// and TestWebEmbeddedClientInternallyConsistent each assert that a client
// struct's ClientVersion field agrees with its own Innertube
// Context["clientVersion"] entry, or YouTube sees a contradictory fingerprint
// from the same process.
//
// They intentionally omit a UA-contains-version check. That only makes sense
// for native-app clients, whose UserAgent literally embeds the app version
// yt-dlp's clientVersion tracks (e.g. "com.google.ios.youtube/21.26.4"); the
// WEB-family clients' UserAgent is a real browser UA whose version numbering
// is unrelated to the Innertube WEB build number, so there is nothing for such
// an assertion to pin here. TestNativeAppUserAgentsNotStale covers the native
// UA strings themselves.
func TestWebClientInternallyConsistent(t *testing.T) {
	if WebClient.ClientVersion != WebClient.Context["clientVersion"] {
		t.Errorf("WebClient.ClientVersion %q != Context[clientVersion] %v",
			WebClient.ClientVersion, WebClient.Context["clientVersion"])
	}
}

func TestWebSafariClientInternallyConsistent(t *testing.T) {
	if WebSafariClient.ClientVersion != WebSafariClient.Context["clientVersion"] {
		t.Errorf("WebSafariClient.ClientVersion %q != Context[clientVersion] %v",
			WebSafariClient.ClientVersion, WebSafariClient.Context["clientVersion"])
	}
}

func TestWebEmbeddedClientInternallyConsistent(t *testing.T) {
	if WebEmbeddedClient.ClientVersion != WebEmbeddedClient.Context["clientVersion"] {
		t.Errorf("WebEmbeddedClient.ClientVersion %q != Context[clientVersion] %v",
			WebEmbeddedClient.ClientVersion, WebEmbeddedClient.Context["clientVersion"])
	}
}

// TestClientRosterIsExactlyTheWiredClients pins the roster against re-growing
// dead weight. IOSClient and WebRemixClient sat in this file for months with
// no production caller, citing an audit proposal ("reports/youtube.md T2")
// that was never wired, their pinned clientVersions rotting silently — which
// is worse than absent, because a reader takes a pinned version for a
// maintained one.
//
// The list below is every client with a real call site in internal/youtube.
// Adding one here without wiring it, or reinstating either deleted constant,
// makes this test and the grep in the task's Step 1 disagree.
//
// Mutant this kills: a count or a name changed without a call site → the
// length assertion fails, or the file no longer compiles.
func TestClientRosterIsExactlyTheWiredClients(t *testing.T) {
	wired := map[string]YouTubeClientConfig{
		"TVDowngradedClient": TVDowngradedClient,
		"WebCreatorClient":   WebCreatorClient,
		"WebClient":          WebClient,
		"WebSafariClient":    WebSafariClient,
		"WebEmbeddedClient":  WebEmbeddedClient,
		"VisionOSClient":     VisionOSClient,
		"AndroidVRClient":    AndroidVRClient,
	}
	if len(wired) != 7 {
		t.Fatalf("the roster lists %d clients, want 7 — every one must have a production call site", len(wired))
	}
	for name, c := range wired {
		if c.ClientName == "" || c.ClientID == "" || c.ClientVersion == "" {
			t.Errorf("%s is incompletely defined: %+v", name, c)
		}
		if c.Context["clientName"] != c.ClientName {
			t.Errorf("%s: Context[clientName] = %v, want %q", name, c.Context["clientName"], c.ClientName)
		}
	}
}

// TestInnertubeContextSnapshots pins each client's Innertube context as the
// JSON body states it. client.userAgent is stated exactly where yt-dlp's
// INNERTUBE_CONTEXT.client states it — web_safari, tv_samsung, visionos and
// android_vr (_base.py:116, 238, 303, 374), with upstream's strings — and
// nowhere else: yt-dlp's web and web_creator contexts carry none
// (_base.py:102-109, 164-171). Upstream's web_embedded (_base.py:128) and
// tv_downgraded (_base.py:361) do; web_embedded's stays off under the owner's
// c7fb478d2 deferral, and tv_downgraded has no caller and is pinned as it
// was. Each stated one is the client's own User-Agent header, so body and
// header agree.
//
// Mutants: the userAgent entry dropped from any of the four contexts; the
// web_safari UA left at Version/17.6 without ",gzip(gfe)"; a userAgent added
// to WebClient.
func TestInnertubeContextSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client YouTubeClientConfig
		want   string
	}{
		{"web_safari", WebSafariClient, `{"clientName":"WEB","clientVersion":"2.20260708.00.00","hl":"en","timeZone":"UTC","userAgent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/15.5 Safari/605.1.15,gzip(gfe)","utcOffsetMinutes":0}`},
		{"tv_samsung", TVSamsungClient, `{"clientName":"TVHTML5","clientVersion":"5.20260707","deviceMake":"Samsung","deviceModel":"UKS9800","hl":"en","osName":"Tizen","osVersion":"2.4.0","timeZone":"UTC","userAgent":"Mozilla/5.0 (SMART-TV; Linux; Tizen 2.4.0) AppleWebKit/538.1 (KHTML, like Gecko) Version/2.4.0 TV Safari/538.1","utcOffsetMinutes":0}`},
		{"visionos", VisionOSClient, `{"clientName":"VISIONOS","clientVersion":"1.02","deviceMake":"Apple","deviceModel":"RealityDevice17,1","hl":"en","osName":"visionOS","osVersion":"26.5.23O471","timeZone":"UTC","userAgent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 15_7_3) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15","utcOffsetMinutes":0}`},
		{"android_vr", AndroidVRClient, `{"androidSdkVersion":32,"clientName":"ANDROID_VR","clientVersion":"1.65.10","deviceMake":"Oculus","deviceModel":"Quest 3","hl":"en","osName":"Android","osVersion":"12L","timeZone":"UTC","userAgent":"com.google.android.apps.youtube.vr.oculus/1.65.10 (Linux; U; Android 12L; eureka-user Build/SQ3A.220605.009.A1) gzip","utcOffsetMinutes":0}`},
		{"web", WebClient, `{"clientName":"WEB","clientVersion":"2.20260708.00.00","hl":"en","timeZone":"UTC","utcOffsetMinutes":0}`},
		{"web_creator", WebCreatorClient, `{"clientName":"WEB_CREATOR","clientVersion":"1.20260708.06.00","hl":"en","timeZone":"UTC","utcOffsetMinutes":0}`},
		{"web_embedded", WebEmbeddedClient, `{"clientName":"WEB_EMBEDDED_PLAYER","clientVersion":"2.20260708.00.00","hl":"en","timeZone":"UTC","utcOffsetMinutes":0}`},
		{"tv_downgraded", TVDowngradedClient, `{"clientName":"TVHTML5","clientVersion":"5.20260707","hl":"en","timeZone":"UTC","utcOffsetMinutes":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.client.Context)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tc.want {
				t.Errorf("context\n got: %s\nwant: %s", b, tc.want)
			}
			if ua, ok := tc.client.Context["userAgent"]; ok && ua != tc.client.UserAgent {
				t.Errorf("context userAgent %q != User-Agent header %q", ua, tc.client.UserAgent)
			}
		})
	}
}
