package cookies

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies/dpapi"
)

// dpapiTestLogger records every log call's message AND its key/value args,
// formatted into one line per call. H7's tests need to assert that a
// specific browser/profile NAME and score were actually named in a log
// line — not just that some Info line fired — so (unlike captureLogger /
// capturingLogger elsewhere in this package, which discard args) this one
// keeps them. That is safe here: the args dpapiExtractAsNetscape logs are
// browser ids, profile names, and integer scores — metadata, never a
// cookie value.
type dpapiTestLogger struct {
	debugs []string
	infos  []string
	warns  []string
	errors []string
}

func (l *dpapiTestLogger) format(msg string, args ...any) string {
	var b strings.Builder
	b.WriteString(msg)
	for i := 0; i+1 < len(args); i += 2 {
		fmt.Fprintf(&b, " %v=%v", args[i], args[i+1])
	}
	return b.String()
}

func (l *dpapiTestLogger) Debug(msg string, args ...any) {
	l.debugs = append(l.debugs, l.format(msg, args...))
}
func (l *dpapiTestLogger) Info(msg string, args ...any) {
	l.infos = append(l.infos, l.format(msg, args...))
}
func (l *dpapiTestLogger) Warn(msg string, args ...any) {
	l.warns = append(l.warns, l.format(msg, args...))
}

func dpapiLinesContain(lines []string, subs ...string) bool {
	for _, line := range lines {
		ok := true
		for _, sub := range subs {
			if !strings.Contains(line, sub) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func dpapiAnyContains(lines []string, sub string) bool {
	for _, line := range lines {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

// stubDpapiProfiles swaps both dpapi seams for deterministic synthetic data
// and restores the real ones on cleanup. byPath maps a synthetic
// BrowserProfile.Path to the ([]dpapi.ChromeCookie, error) that profile's
// read should produce — never real filesystem or SQLite I/O.
//
// It also forces runtimeGOOS to "windows" for the test's duration. Every
// caller below exercises dpapiExtractAsNetscape's PROFILE-SELECTION logic —
// scoring, browser-type filtering, tie-breaking — which only runs past the
// Windows-only platform guard at the top of that function (T4-34). Without
// this, the whole suite depended on being run ON a Windows machine to reach
// any of that logic at all, and failed outright on Linux CI once the guard
// was added — the guard reading dpapi.ErrNotSupported instead of whatever
// the test staged. Forcing it here, once, keeps every one of these
// deterministic on any host, matching how they already fake the profiles
// and cookie rows instead of depending on this machine's real GOOS.
func stubDpapiProfiles(t *testing.T, profiles []dpapi.BrowserProfile, byPath map[string][]dpapi.ChromeCookie) {
	t.Helper()
	realGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = realGOOS })
	runtimeGOOS = func() string { return "windows" }

	prevFind, prevRead := dpapiFindBrowserProfiles, dpapiReadChromeCookiesStats
	dpapiFindBrowserProfiles = func() []dpapi.BrowserProfile { return profiles }
	dpapiReadChromeCookiesStats = func(profilePath, originFilter string) ([]dpapi.ChromeCookie, dpapi.ChromeReadStats, error) {
		cookies, ok := byPath[profilePath]
		if !ok {
			return nil, dpapi.ChromeReadStats{}, fmt.Errorf("stubDpapiProfiles: unexpected profile path %q", profilePath)
		}
		return cookies, dpapi.ChromeReadStats{Rows: len(cookies), Decrypted: len(cookies)}, nil
	}
	t.Cleanup(func() {
		dpapiFindBrowserProfiles, dpapiReadChromeCookiesStats = prevFind, prevRead
	})
}

// Fixture profiles shared across the H7 test cases below.
var (
	dpapiProfileA     = dpapi.BrowserProfile{Browser: "chrome", Name: "Default", Path: `C:\fake\chrome\Default`, IsDefault: true}
	dpapiProfileB     = dpapi.BrowserProfile{Browser: "edge", Name: "Default", Path: `C:\fake\edge\Default`, IsDefault: true}
	dpapiProfileBrave = dpapi.BrowserProfile{Browser: "brave", Name: "Default", Path: `C:\fake\brave\Default`, IsDefault: true}
)

// dpapiFullYouTubeSet is profile A's cookies: SAPISID + LOGIN_INFO, a
// COMPLETE YouTube auth set (dpapiProfileScore's top tier).
func dpapiFullYouTubeSet(valuePrefix string) []dpapi.ChromeCookie {
	return []dpapi.ChromeCookie{
		{Host: ".youtube.com", Name: "SAPISID", Value: valuePrefix + "-SAPISID", Path: "/", Secure: true},
		{Host: ".youtube.com", Name: "LOGIN_INFO", Value: valuePrefix + "-LOGIN-INFO", Path: "/", Secure: true},
	}
}

// dpapiSapisidOnly is profile B's cookies: SAPISID alone — a PARTIAL
// YouTube auth set (dpapiProfileScore's lower tier), exactly as the brief
// specifies ("profile B: SAPISID only").
func dpapiSapisidOnly(valuePrefix string) []dpapi.ChromeCookie {
	return []dpapi.ChromeCookie{
		{Host: ".youtube.com", Name: "SAPISID", Value: valuePrefix + "-SAPISID", Path: "/", Secure: true},
	}
}

// TestDpapiExtractChoosesHigherScoringProfile is H7's core claim: with two
// signed-in Chromium profiles, exactly ONE is used — the one with the more
// complete auth set — and the loser's rows are not merged in at all.
//
// The junction defect this guards against: "A's rows are present" is
// satisfied even by the OLD merge-everything code, since A's rows would
// still be in the merged slice. The real assertion is that B's SAPISID
// VALUE — distinguishable from A's — never appears, because B's row was
// never handed to deduplicateAndFormat in the first place. Put the merge
// back (append every profile's cookies into one collected slice again) and
// this test starts seeing B's value: whichever profile FindBrowserProfiles
// lists LAST would win the bare-name dedup.
func TestDpapiExtractChoosesHigherScoringProfile(t *testing.T) {
	stubDpapiProfiles(t,
		[]dpapi.BrowserProfile{dpapiProfileA, dpapiProfileB},
		map[string][]dpapi.ChromeCookie{
			dpapiProfileA.Path: dpapiFullYouTubeSet("A"),
			dpapiProfileB.Path: dpapiSapisidOnly("B"),
		},
	)
	log := &dpapiTestLogger{}

	out, err := dpapiExtractAsNetscape(log, "", "")
	if err != nil {
		t.Fatalf("dpapiExtractAsNetscape = %v, want nil error", err)
	}
	if !strings.Contains(out, "A-SAPISID") || !strings.Contains(out, "A-LOGIN-INFO") {
		t.Errorf("output missing profile A's rows:\n%s", out)
	}
	if strings.Contains(out, "B-SAPISID") {
		t.Errorf("output contains profile B's SAPISID value — profiles were merged instead of one being chosen:\n%s", out)
	}
	if !dpapiLinesContain(log.infos, "chose one profile", "browser=chrome", "profile=Default") {
		t.Errorf("no Info line named the chosen profile (chrome/Default); infos=%v", log.infos)
	}
	if !dpapiAnyContains(log.debugs, "browser=edge") {
		t.Errorf("expected the passed-over profile (edge/Default) logged at Debug; debugs=%v", log.debugs)
	}
}

// TestDpapiExtractChoosesHigherScoringProfileRegardlessOfScanOrder pins that
// the winner is decided by SCORE, not by which profile FindBrowserProfiles
// happens to list first or last.
//
// Arc 8 fix round 1, Finding 5: the original version of this test reversed
// the SCAN ORDER ([]dpapi.BrowserProfile{B, A}) but left A as the
// higher-scoring profile. That is decorative — the merge-all bug's rule is
// "whichever profile is SCANNED LAST wins", and reversing the order moved
// A (already the higher scorer) into last place, so the bug's answer and
// the correct answer both came out "A". A test that can't tell the bug
// from the fix apart is not testing anything. This version instead swaps
// WHICH profile has the complete set: B is scanned first and now holds it,
// A is scanned last and holds only the partial set. Correct behavior
// (highest score wins) still picks B, unconditionally. The buggy behavior
// (last-scanned wins the bare-name dedup) would instead let A's
// last-scanned SAPISID row overwrite B's, while B's LOGIN_INFO survives
// untouched (A never had one to overwrite it) — producing the exact
// "two halves of one session from different profiles" hybrid H7 exists to
// prevent. Asserting B's SAPISID specifically (not merely that A's
// SAPISID is present, and not merely that the output is non-empty) is what
// catches that hybrid.
func TestDpapiExtractChoosesHigherScoringProfileRegardlessOfScanOrder(t *testing.T) {
	stubDpapiProfiles(t,
		[]dpapi.BrowserProfile{dpapiProfileB, dpapiProfileA}, // B scanned first, A scanned LAST
		map[string][]dpapi.ChromeCookie{
			dpapiProfileB.Path: dpapiFullYouTubeSet("B2"), // now the COMPLETE set
			dpapiProfileA.Path: dpapiSapisidOnly("A2"),    // now the PARTIAL set, and scanned last
		},
	)
	log := &dpapiTestLogger{}

	out, err := dpapiExtractAsNetscape(log, "", "")
	if err != nil {
		t.Fatalf("dpapiExtractAsNetscape = %v, want nil error", err)
	}
	// Correct (score-driven): B wins outright, both of B's rows present,
	// neither of A's.
	//
	// Under the old merge-all bug (last-scanned wins the bare-name dedup),
	// A is scanned last and DOES have a same-named "SAPISID" row, so the
	// bug would overwrite B's SAPISID with A's while B's LOGIN_INFO
	// survives untouched (A never had one to overwrite it) — the exact
	// "two halves of one session from different profiles" failure mode H7
	// exists to prevent. Asserting B-SAPISID is present (not A2-SAPISID)
	// catches that hybrid outcome; a bare "output is non-empty" or "some
	// SAPISID line exists" would not.
	if !strings.Contains(out, "B2-SAPISID") || !strings.Contains(out, "B2-LOGIN-INFO") {
		t.Errorf("output missing profile B's rows (the higher scorer, scanned FIRST):\n%s", out)
	}
	if strings.Contains(out, "A2-SAPISID") {
		t.Errorf("output contains profile A's SAPISID value — the last-scanned profile won a shared name instead of the higher scorer:\n%s", out)
	}
}

// TestDpapiExtractConfiguredBrowserFilterOverridesScore is H7 rule 1: when
// the operator has named a browser in settings, ONLY that browser's
// profiles are candidates — even if a different browser's profile scores
// higher. Removing the filter (scoring across every profile regardless of
// configuredBrowserType) makes this test start choosing A instead.
func TestDpapiExtractConfiguredBrowserFilterOverridesScore(t *testing.T) {
	stubDpapiProfiles(t,
		[]dpapi.BrowserProfile{dpapiProfileA, dpapiProfileB},
		map[string][]dpapi.ChromeCookie{
			dpapiProfileA.Path: dpapiFullYouTubeSet("A"),
			dpapiProfileB.Path: dpapiSapisidOnly("B"),
		},
	)
	log := &dpapiTestLogger{}

	// "edge" matches only dpapiProfileB (dpapiProfileA is "chrome").
	out, err := dpapiExtractAsNetscape(log, "edge", "")
	if err != nil {
		t.Fatalf("dpapiExtractAsNetscape = %v, want nil error", err)
	}
	if !strings.Contains(out, "B-SAPISID") {
		t.Errorf("configured browser %q should have chosen profile B, but B's row is missing:\n%s", "edge", out)
	}
	if strings.Contains(out, "A-SAPISID") || strings.Contains(out, "A-LOGIN-INFO") {
		t.Errorf("configured browser %q should have EXCLUDED profile A (better score, wrong browser), but A's rows are present:\n%s", "edge", out)
	}
	if !dpapiLinesContain(log.debugs, "does not match configured browser", "browser=chrome") {
		t.Errorf("expected profile A skipped at Debug for not matching the configured browser; debugs=%v", log.debugs)
	}
}

// TestDpapiExtractConfiguredBrowserFilterMatchesChannelSiblings pins
// dpapiBrowserMatchesConfigured's family rule: "edge" also matches an
// "edge-beta" profile, because knownBrowserTypes (browser_validate.go)
// only ever offers the coarse per-browser name — an operator who
// configured "edge" cannot even express "edge-beta specifically".
//
// Arc 8 fix round 1: this used "chrome"/"chrome-beta" before Finding 1's
// fix. "chrome" is now intercepted earlier as the Web UI's whole-family
// sentinel (see DpapiChromiumFamilyValue) and never reaches
// dpapiBrowserMatchesConfigured at all, which would have made this test
// decorative — it would keep passing (unfiltered scoring also finds the
// beta profile) but for the wrong reason, no longer exercising the
// channel-sibling matching it claims to. "edge" still reaches the real
// per-browser filtering branch, so it still tests the real thing.
func TestDpapiExtractConfiguredBrowserFilterMatchesChannelSiblings(t *testing.T) {
	beta := dpapi.BrowserProfile{Browser: "edge-beta", Name: "Default", Path: `C:\fake\edge-beta\Default`}
	stubDpapiProfiles(t,
		[]dpapi.BrowserProfile{dpapiProfileA, beta},
		map[string][]dpapi.ChromeCookie{
			// "CHR" not "A": "A-SAPISID" is a substring of "BETA-SAPISID"
			// ("BETA" ends in "A"), which would make the exclusion check
			// below pass vacuously against BETA's own row.
			dpapiProfileA.Path: dpapiSapisidOnly("CHR"),
			beta.Path:          dpapiFullYouTubeSet("BETA"),
		},
	)
	log := &dpapiTestLogger{}

	out, err := dpapiExtractAsNetscape(log, "edge", "")
	if err != nil {
		t.Fatalf("dpapiExtractAsNetscape = %v, want nil error", err)
	}
	if !strings.Contains(out, "BETA-SAPISID") {
		t.Errorf("configured browser \"edge\" should match \"edge-beta\" as a candidate:\n%s", out)
	}
	if strings.Contains(out, "CHR-SAPISID") {
		t.Errorf("configured browser \"edge\" should have excluded profile A (\"chrome\"):\n%s", out)
	}
}

// TestDpapiExtractChromeMeansWholeChromiumFamily is Finding 1 (Arc 8 fix
// round 1, CRITICAL): web/public/index.html's cfg-cookies-browser-type
// dropdown has exactly one Chromium option, `<sl-option value="chrome">`,
// covering "chrome, brave, edge, vivaldi, thorium, opera" — so a Web UI
// user who points a custom path at brave.exe stores browser_type="chrome".
// The original fix round narrowed the DPAPI filter to profiles literally
// named "chrome", which excluded that user's real Brave profile — a
// regression the review confirmed against BOTH failure shapes: a
// Brave-only machine hard-errors, and a machine with an unused Chrome
// install silently picks the wrong (signed-out) profile. This covers the
// first shape: only Brave is present, configured type is "chrome", Brave
// must be chosen with no error.
func TestDpapiExtractChromeMeansWholeChromiumFamily(t *testing.T) {
	stubDpapiProfiles(t,
		[]dpapi.BrowserProfile{dpapiProfileBrave},
		map[string][]dpapi.ChromeCookie{
			dpapiProfileBrave.Path: dpapiFullYouTubeSet("BRAVE"),
		},
	)
	log := &dpapiTestLogger{}

	out, err := dpapiExtractAsNetscape(log, "chrome", "")
	if err != nil {
		t.Fatalf(`dpapiExtractAsNetscape(_, "chrome") = %v, want nil error on a Brave-only machine`, err)
	}
	if !strings.Contains(out, "BRAVE-SAPISID") || !strings.Contains(out, "BRAVE-LOGIN-INFO") {
		t.Errorf("output missing the Brave profile's rows:\n%s", out)
	}
}

// TestDpapiExtractChromeMeansWholeChromiumFamilyScoresAcrossIt covers
// Finding 1's second failure shape: a machine with an UNUSED Chrome
// install alongside a signed-in Brave. browser_type="chrome" must not
// narrow to the (signed-out) Chrome profile and report "no relevant
// cookies" — it must score every Chromium-family profile and pick Brave.
func TestDpapiExtractChromeMeansWholeChromiumFamilyScoresAcrossIt(t *testing.T) {
	stubDpapiProfiles(t,
		[]dpapi.BrowserProfile{dpapiProfileA, dpapiProfileBrave},
		map[string][]dpapi.ChromeCookie{
			dpapiProfileA.Path:     nil, // Chrome installed but signed out: no cookies at all
			dpapiProfileBrave.Path: dpapiFullYouTubeSet("BRAVE"),
		},
	)
	log := &dpapiTestLogger{}

	out, err := dpapiExtractAsNetscape(log, "chrome", "")
	if err != nil {
		t.Fatalf(`dpapiExtractAsNetscape(_, "chrome") = %v, want nil error`, err)
	}
	if !strings.Contains(out, "BRAVE-SAPISID") || !strings.Contains(out, "BRAVE-LOGIN-INFO") {
		t.Errorf("output missing the signed-in Brave profile's rows — signed-out Chrome must not have won:\n%s", out)
	}
	if !dpapiLinesContain(log.infos, "chose one profile", "browser=brave") {
		t.Errorf("expected an Info line naming brave/Default as chosen; infos=%v", log.infos)
	}
}

// TestDpapiExtractPerBrowserValueStillNarrowsDespiteLowerScore is the
// reviewer's third fix-round-1 test: a genuine per-browser value ("brave",
// reachable only from the TUI's free-text browser_type field) must still
// narrow to that one browser, even when a DIFFERENT browser's profile
// would have scored higher. Finding 1's fix must not have widened every
// configured value to "unfiltered" — only the "chrome" family sentinel.
func TestDpapiExtractPerBrowserValueStillNarrowsDespiteLowerScore(t *testing.T) {
	stubDpapiProfiles(t,
		[]dpapi.BrowserProfile{dpapiProfileA, dpapiProfileBrave},
		map[string][]dpapi.ChromeCookie{
			dpapiProfileA.Path:     dpapiFullYouTubeSet("CHROME"), // higher score, WRONG browser
			dpapiProfileBrave.Path: dpapiSapisidOnly("BRAVE"),     // lower score, configured browser
		},
	)
	log := &dpapiTestLogger{}

	out, err := dpapiExtractAsNetscape(log, "brave", "")
	if err != nil {
		t.Fatalf(`dpapiExtractAsNetscape(_, "brave") = %v, want nil error`, err)
	}
	if !strings.Contains(out, "BRAVE-SAPISID") {
		t.Errorf(`configured browser "brave" should have chosen the Brave profile despite its lower score:\n%s`, out)
	}
	if strings.Contains(out, "CHROME-SAPISID") || strings.Contains(out, "CHROME-LOGIN-INFO") {
		t.Errorf(`configured browser "brave" should have EXCLUDED the higher-scoring Chrome profile:\n%s`, out)
	}
}

// TestDpapiExtractUnknownDpapiLayoutFallsBackToUnfiltered is Finding 2
// (Arc 8 fix round 1, HIGH): browser_validate.go's knownBrowserTypes
// accepts "opera" and "thorium" as configured browser_type values, and
// neither is Firefox-based, so both can reach dpapiExtractAsNetscape — but
// dpapi/profiles.go's chromiumBrowsers has no layout for either (Opera's
// profile layout differs and is deliberately excluded; Thorium was simply
// never added). Filtering by either would ALWAYS produce zero candidates
// regardless of what's actually installed, which is a dpapi coverage gap,
// not "this browser isn't installed" — it must fall back to unfiltered
// scoring with a Debug line, never a hard error.
func TestDpapiExtractUnknownDpapiLayoutFallsBackToUnfiltered(t *testing.T) {
	stubDpapiProfiles(t,
		[]dpapi.BrowserProfile{dpapiProfileA},
		map[string][]dpapi.ChromeCookie{
			dpapiProfileA.Path: dpapiFullYouTubeSet("A"),
		},
	)
	log := &dpapiTestLogger{}

	out, err := dpapiExtractAsNetscape(log, "opera", "")
	if err != nil {
		t.Fatalf(`dpapiExtractAsNetscape(_, "opera") = %v, want nil error (unfiltered fallback)`, err)
	}
	if !strings.Contains(out, "A-SAPISID") || !strings.Contains(out, "A-LOGIN-INFO") {
		t.Errorf("output missing profile A's rows — \"opera\" should have fallen back to unfiltered scoring:\n%s", out)
	}
	if !dpapiLinesContain(log.debugs, "no dpapi profile layout", "configured=opera") {
		t.Errorf("expected a Debug line naming \"opera\" as having no dpapi layout; debugs=%v", log.debugs)
	}
}

// TestDpapiExtractTieLogsBothProfilesAndFirstWins is H7 rule 2's tie
// clause: two profiles with an IDENTICAL score keep FindBrowserProfiles'
// scan order (first candidate wins) and the ambiguity is logged at Info,
// naming both profiles — not silently resolved.
func TestDpapiExtractTieLogsBothProfilesAndFirstWins(t *testing.T) {
	stubDpapiProfiles(t,
		[]dpapi.BrowserProfile{dpapiProfileA, dpapiProfileB}, // A first
		map[string][]dpapi.ChromeCookie{
			dpapiProfileA.Path: dpapiFullYouTubeSet("A"), // complete: score 10
			dpapiProfileB.Path: dpapiFullYouTubeSet("B"), // also complete: score 10 -- a genuine tie
		},
	)
	log := &dpapiTestLogger{}

	out, err := dpapiExtractAsNetscape(log, "", "")
	if err != nil {
		t.Fatalf("dpapiExtractAsNetscape = %v, want nil error", err)
	}
	if !strings.Contains(out, "A-SAPISID") || !strings.Contains(out, "A-LOGIN-INFO") {
		t.Errorf("tie should have kept scan order (A first), but A's rows are missing:\n%s", out)
	}
	if strings.Contains(out, "B-SAPISID") || strings.Contains(out, "B-LOGIN-INFO") {
		t.Errorf("tie should have kept scan order (A first), but B's rows leaked in too:\n%s", out)
	}
	if !dpapiLinesContain(log.infos, "tied profile score", "chrome/Default", "edge/Default") {
		t.Errorf("expected one Info line naming BOTH tied profiles; infos=%v", log.infos)
	}
}

// TestDpapiExtractNoProfilesForConfiguredBrowser covers the edge the H7
// ruling doesn't spell out: the operator configured a browser that
// FindBrowserProfiles found zero profiles for. This must fail loudly with
// which browsers WERE found, not silently fall back to scoring every
// profile (that would defeat rule 1 entirely).
func TestDpapiExtractNoProfilesForConfiguredBrowser(t *testing.T) {
	stubDpapiProfiles(t,
		[]dpapi.BrowserProfile{dpapiProfileA},
		map[string][]dpapi.ChromeCookie{
			dpapiProfileA.Path: dpapiFullYouTubeSet("A"),
		},
	)
	log := &dpapiTestLogger{}

	_, err := dpapiExtractAsNetscape(log, "brave", "")
	if err == nil {
		t.Fatal("dpapiExtractAsNetscape = nil error, want an error naming the configured browser has no profiles")
	}
	if !strings.Contains(err.Error(), "brave") || !strings.Contains(err.Error(), "chrome") {
		t.Errorf("error should name both the configured browser and what was found, got: %v", err)
	}
}

// --- dpapiProfileScore ---

func TestDpapiProfileScore(t *testing.T) {
	cases := []struct {
		name    string
		cookies []extractedCookie
		want    int
	}{
		{"empty", nil, 0},
		{
			"complete YouTube only",
			[]extractedCookie{
				{domain: ".youtube.com", name: "SAPISID", value: "x"},
				{domain: ".youtube.com", name: "LOGIN_INFO", value: "x"},
			},
			10,
		},
		{
			"partial YouTube (SAPISID only)",
			[]extractedCookie{
				{domain: ".youtube.com", name: "SAPISID", value: "x"},
			},
			1,
		},
		{
			"complete Twitch only",
			[]extractedCookie{
				{domain: ".twitch.tv", name: "auth-token", value: "x"},
			},
			10,
		},
		{
			"partial Twitch (twilight-user only)",
			[]extractedCookie{
				{domain: ".twitch.tv", name: "twilight-user", value: "x"},
			},
			1,
		},
		{
			"complete on both platforms sums",
			[]extractedCookie{
				{domain: ".youtube.com", name: "SAPISID", value: "x"},
				{domain: ".youtube.com", name: "LOGIN_INFO", value: "x"},
				{domain: ".twitch.tv", name: "auth-token", value: "x"},
			},
			20,
		},
		{
			"empty value does not count",
			[]extractedCookie{
				{domain: ".youtube.com", name: "SAPISID", value: ""},
				{domain: ".youtube.com", name: "LOGIN_INFO", value: ""},
			},
			0,
		},
		{
			"wrong domain does not count",
			[]extractedCookie{
				{domain: ".evil.example", name: "SAPISID", value: "x"},
				{domain: ".evil.example", name: "LOGIN_INFO", value: "x"},
			},
			0,
		},
		{
			"__Secure-3PAPISID substitutes for SAPISID",
			[]extractedCookie{
				{domain: ".youtube.com", name: "__Secure-3PAPISID", value: "x"},
				{domain: ".youtube.com", name: "LOGIN_INFO", value: "x"},
			},
			10,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dpapiProfileScore(tc.cookies); got != tc.want {
				t.Errorf("dpapiProfileScore(%+v) = %d, want %d", tc.cookies, got, tc.want)
			}
		})
	}
}

// --- dpapiBrowserMatchesConfigured ---

func TestDpapiBrowserMatchesConfigured(t *testing.T) {
	cases := []struct {
		configured string
		profile    string
		want       bool
	}{
		{"chrome", "chrome", true},
		{"chrome", "chrome-beta", true},
		{"chrome", "chrome-dev", true},
		{"chrome", "chrome-canary", true},
		{"edge", "edge", true},
		{"edge", "edge-beta", true},
		{"edge", "chrome", false},
		{"chrome", "chromium", false}, // distinct browser, not a "chrome" channel
		{"brave", "brave", true},
		{"brave", "chrome", false},
		{"", "chrome", false}, // empty configured type never "matches" — caller skips filtering entirely instead
	}
	for _, tc := range cases {
		t.Run(tc.configured+"_vs_"+tc.profile, func(t *testing.T) {
			if got := dpapiBrowserMatchesConfigured(tc.configured, tc.profile); got != tc.want {
				t.Errorf("dpapiBrowserMatchesConfigured(%q, %q) = %v, want %v", tc.configured, tc.profile, got, tc.want)
			}
		})
	}
}

// --- browserOverrideConfigured ---

// TestBrowserOverrideConfigured is Finding 3 (Arc 8 fix round 1, MEDIUM):
// resolvedBrowser and the DPAPI fallback's configured-browser-type read
// (autocookies.go's refreshCookiesDetailed) must gate on the IDENTICAL
// predicate, or a browser_type set with no browser_path — reachable from
// the TUI's free-text browser_type field (tui/settings.go's Save writes
// BrowserType independently of BrowserPath) — is "no override" in one
// place and a hard filter in the other. This pins the shared predicate
// directly; both call sites are required to use it rather than
// re-deriving their own condition.
func TestBrowserOverrideConfigured(t *testing.T) {
	cases := []struct {
		name        string
		path, btype string
		want        bool
	}{
		{"both set", "/path/to/brave.exe", "brave", true},
		{"type only, no path (TUI free-text without a path)", "", "brave", false},
		{"path only, no type", "/path/to/brave.exe", "", false},
		{"neither set", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := browserOverrideConfigured(tc.path, tc.btype); got != tc.want {
				t.Errorf("browserOverrideConfigured(%q, %q) = %v, want %v", tc.path, tc.btype, got, tc.want)
			}
		})
	}
}

// --- COOKIES-7: cookies.dpapi_profile_dir ---

// Error completes dpapiTestLogger's logger interface so it can stand in for an
// AutoCookieService's own logger (which needs all four levels), not just for
// the three-level interface dpapiExtractAsNetscape takes.
func (l *dpapiTestLogger) Error(msg string, args ...any) {
	l.errors = append(l.errors, l.format(msg, args...))
}

// mkDpapiProfileDir builds a throwaway Chromium PROFILE directory shaped the
// way dpapi.ValidateProfileDir insists on: "Local State" one level up, and a
// "Cookies" store inside. The bytes are irrelevant — no test here opens the
// store, the dpapiReadChromeCookiesStats seam answers for it — only the SHAPE
// matters, and it has to be a real directory because validation stats it.
func mkDpapiProfileDir(t *testing.T) string {
	t.Helper()
	userData := filepath.Join(t.TempDir(), "User Data")
	profile := filepath.Join(userData, "Default")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userData, "Local State"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "Cookies"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return profile
}

// dpapiSeamCalls records what each dpapi seam was ASKED, which is the whole
// assertion for COOKIES-7's routing: "discovery did not run" is not visible in
// the returned cookies, only in whether the seam was called at all.
type dpapiSeamCalls struct {
	discovered bool
	readPaths  []string
}

// stubDpapiSeamsRecording is stubDpapiProfiles' recording cousin: same forced
// Windows GOOS, but discovery answers with one profile AND records that it was
// consulted, and the read records every path handed to it.
func stubDpapiSeamsRecording(t *testing.T) *dpapiSeamCalls {
	t.Helper()
	realGOOS := runtimeGOOS
	runtimeGOOS = func() string { return "windows" }
	prevFind, prevRead := dpapiFindBrowserProfiles, dpapiReadChromeCookiesStats
	calls := &dpapiSeamCalls{}
	dpapiFindBrowserProfiles = func() []dpapi.BrowserProfile {
		calls.discovered = true
		return []dpapi.BrowserProfile{dpapiProfileA}
	}
	dpapiReadChromeCookiesStats = func(profilePath, originFilter string) ([]dpapi.ChromeCookie, dpapi.ChromeReadStats, error) {
		calls.readPaths = append(calls.readPaths, profilePath)
		rows := dpapiFullYouTubeSet("X")
		return rows, dpapi.ChromeReadStats{Rows: len(rows), Decrypted: len(rows)}, nil
	}
	t.Cleanup(func() {
		runtimeGOOS = realGOOS
		dpapiFindBrowserProfiles, dpapiReadChromeCookiesStats = prevFind, prevRead
	})
	return calls
}

// TestDpapiExplicitProfileDirTakesPrecedence is COOKIES-7's routing half. The
// discovery walk knows eleven fixed %LOCALAPPDATA% layouts, so a portable
// Chromium, a --user-data-dir profile or Opera is invisible to it — and the
// pass answered "no Chromium-family profiles found under LOCALAPPDATA" even
// when browser_path named the binary. An explicit directory must SKIP the walk
// entirely, not be appended to it: the operator naming a directory is a
// stronger statement than a scoring pass over whatever else is installed.
//
// Mutants:
//   - append instead of replacing -> the discovery seam is still called.
//   - skip dpapi.ValidateProfileDir -> the bad-directory subtest's error no
//     longer names the directory, and the operator gets "no cookies came out".
//   - fall back to discovery when validation fails -> the bad-directory
//     subtest sees the discovery seam called.
func TestDpapiExplicitProfileDirTakesPrecedence(t *testing.T) {
	t.Run("a valid directory replaces discovery", func(t *testing.T) {
		calls := stubDpapiSeamsRecording(t)
		dir := mkDpapiProfileDir(t)
		log := &dpapiTestLogger{}

		out, err := dpapiExtractAsNetscape(log, "", dir)
		if err != nil {
			t.Fatalf("dpapiExtractAsNetscape = %v, want nil error", err)
		}
		if calls.discovered {
			t.Error("dpapiFindBrowserProfiles was called — the configured directory must REPLACE the " +
				"discovery walk, not be appended to it and scored against whatever else is installed")
		}
		if len(calls.readPaths) != 1 || calls.readPaths[0] != dir {
			t.Errorf("read paths = %v, want exactly [%q]", calls.readPaths, dir)
		}
		if !strings.Contains(out, "X-SAPISID") {
			t.Errorf("the configured profile's rows are missing from the output:\n%s", out)
		}
		if !dpapiLinesContain(log.debugs, "configured profile directory", dir) {
			t.Errorf("expected a Debug line naming the configured directory; debugs=%v", log.debugs)
		}
	})

	t.Run("an invalid directory is an error, never a fall-back to discovery", func(t *testing.T) {
		calls := stubDpapiSeamsRecording(t)
		missing := filepath.Join(t.TempDir(), "Default")
		log := &dpapiTestLogger{}

		if _, err := dpapiExtractAsNetscape(log, "", missing); err == nil {
			t.Fatal("dpapiExtractAsNetscape = nil error for a directory that does not exist")
		} else if !strings.Contains(err.Error(), missing) {
			t.Errorf("error must name the directory the operator configured, got: %v", err)
		}
		if calls.discovered {
			t.Error("discovery ran after the configured directory was refused — the operator would be " +
				"told \"no profiles found under LOCALAPPDATA\" about a setting they had just written, " +
				"which is the exact confusion this setting exists to remove")
		}
		if len(calls.readPaths) != 0 {
			t.Errorf("nothing may be read after validation fails; read paths = %v", calls.readPaths)
		}
	})

	t.Run("an empty directory leaves discovery exactly as it was", func(t *testing.T) {
		calls := stubDpapiSeamsRecording(t)

		if _, err := dpapiExtractAsNetscape(&dpapiTestLogger{}, "", ""); err != nil {
			t.Fatalf("dpapiExtractAsNetscape = %v, want nil error", err)
		}
		if !calls.discovered {
			t.Error("discovery must still run when no directory is configured")
		}
		if len(calls.readPaths) != 1 || calls.readPaths[0] != dpapiProfileA.Path {
			t.Errorf("read paths = %v, want exactly [%q]", calls.readPaths, dpapiProfileA.Path)
		}
	})
}

// TestDpapiExplicitProfileDirAcceptsWhatDiscoveryCannotSee is I-1's pin, and it
// is the feature's motivating case finally having a test.
//
// The first shipping version ran the LAUNCH-boundary deny-list
// (dangerousProfilePathSubstrings) over the explicitly configured directory.
// That list exists so the headless refresh never LAUNCHES a browser against a
// real user profile; cookies.dpapi_profile_dir is a READ location — the value
// reaches a log sentence and a mode=ro SQLite open, and nothing else (pinned by
// TestDpapiProfileDirNeverReachesALaunch below). Applying it here bought
// nothing — discovery walks and reads the same real profiles with no deny-list
// at all — and refused exactly the three profiles the key was built for: a
// portable Chromium under a `Chromium\User Data` path, and any Opera, whose
// standard and portable locations both match `/opera software/opera stable`.
//
// Mutant: re-apply the deny-list to the explicit directory (call
// validateBrowserProfileDirForLaunch, or any equivalent, before the read) ->
// both subtests fail, and the operator's only escape is to rename their
// directory.
func TestDpapiExplicitProfileDirAcceptsWhatDiscoveryCannotSee(t *testing.T) {
	t.Run("a portable Chromium", func(t *testing.T) {
		calls := stubDpapiSeamsRecording(t)
		// …/Chromium/User Data/Default — matches the deny-list's
		// `/chromium/user data` entry, and is NOT under %LOCALAPPDATA%, so the
		// discovery walk cannot see it either. This is COOKIES-7 case one.
		userData := filepath.Join(t.TempDir(), "Chromium", "User Data")
		dir := filepath.Join(userData, "Default")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(userData, "Local State"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Cookies"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		if _, err := dpapiExtractAsNetscape(&dpapiTestLogger{}, "", dir); err != nil {
			t.Fatalf("dpapiExtractAsNetscape = %v, want nil — a portable Chromium is the case this key exists for", err)
		}
		if len(calls.readPaths) != 1 || calls.readPaths[0] != dir {
			t.Errorf("read paths = %v, want exactly [%q]", calls.readPaths, dir)
		}
	})

	t.Run("a portable Opera", func(t *testing.T) {
		calls := stubDpapiSeamsRecording(t)
		// …/Opera Software/Opera Stable — matches the deny-list's
		// `/opera software/opera stable` entry, and has its Local State INSIDE
		// (I-2). Opera is COOKIES-7 case three, and it needs both fixes.
		dir := filepath.Join(t.TempDir(), "Opera Software", "Opera Stable")
		if err := os.MkdirAll(filepath.Join(dir, "Network"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, rel := range []string{"Local State", filepath.Join("Network", "Cookies")} {
			if err := os.WriteFile(filepath.Join(dir, rel), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		if _, err := dpapiExtractAsNetscape(&dpapiTestLogger{}, "opera", dir); err != nil {
			t.Fatalf("dpapiExtractAsNetscape = %v, want nil for Opera's layout", err)
		}
		if len(calls.readPaths) != 1 || calls.readPaths[0] != dir {
			t.Errorf("read paths = %v, want exactly [%q]", calls.readPaths, dir)
		}
	})
}

// TestDpapiProfileDirNeverReachesALaunch is the structural half of I-1: the
// REASON the launch deny-list does not apply here is that this value never
// reaches a launch, and that is a property of the code, not of a comment.
//
// It taints every identifier assigned from something mentioning
// DpapiProfileDir and then records which functions those tainted values are
// passed to, over every non-test .go file in internal/cookies and cmd/moombox.
// The allowed set below IS the consumer trace: a log sentence, the structural
// validator, and the read. Anything else — the refresh launcher, StartSetup,
// exec.Command, validateBrowserProfileDirForLaunch — means the value crossed
// the launch boundary and the deny-list question is open again.
//
// Mutant: pass the directory to validateBrowserProfileDirForLaunch (or to any
// launcher/exec call) -> the new callee is not in the allowlist and this fails,
// naming it.
func TestDpapiProfileDirNeverReachesALaunch(t *testing.T) {
	// Every function a value derived from DpapiProfileDir may legitimately be
	// handed to. Keep this list SHORT and argue for every addition.
	allowed := map[string]string{
		"dpapiExtractAsNetscape":   "the DPAPI read itself — mode=ro, launches nothing",
		"dpapi.ValidateProfileDir": "the structural check on the directory",
		"s.logger.Warn":            "the boot verdict's message",
		"fmt.Errorf":               "wrapping the directory into an error string",
		"filepath.Base":            "the profile NAME for the log line",
		"filepath.Join":            "building a path under the directory",
		"logger.Debug":             "the \"using the configured profile directory\" line",
		"configStore.Read":         "reading the config that holds it",
		"s.configStore.Read":       "reading the config that holds it",
	}

	root := moduleRoot(t)
	var offenders []string
	for _, dir := range []string{
		filepath.Join(root, "internal", "cookies"),
		filepath.Join(root, "cmd", "moombox"),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			for _, callee := range taintedCallees(fset, file, "DpapiProfileDir") {
				if _, ok := allowed[callee]; !ok {
					offenders = append(offenders, name+": "+callee)
				}
			}
		}
	}
	if len(offenders) > 0 {
		t.Errorf("cookies.dpapi_profile_dir is passed to %v — it is a READ location, and the launch-boundary "+
			"deny-list (dangerousProfilePathSubstrings) is deliberately NOT applied to it. Either revert the "+
			"new consumer or re-open I-1", offenders)
	}
}

// moduleRoot walks up from the test's working directory to the directory
// holding go.mod, so this test can read files in a sibling package without
// importing it.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// taintedCallees returns the printed name of every function that is handed an
// expression mentioning `field`, or a local identifier assigned from one.
//
// One hop of taint is enough here and deliberately so: the real flows are
// `dir := s.DpapiProfileDir()` and `explicitProfileDir = s.DpapiProfileDir()`,
// and a second hop would start reporting the whole program. A future consumer
// that laundered the value through two variables to get past this would be
// doing so on purpose, which is a different conversation.
func taintedCallees(fset *token.FileSet, file *ast.File, field string) []string {
	render := func(n ast.Node) string {
		var b bytes.Buffer
		if err := printer.Fprint(&b, fset, n); err != nil {
			return ""
		}
		return b.String()
	}

	var out []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		tainted := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, rhs := range assign.Rhs {
				if i >= len(assign.Lhs) || !strings.Contains(render(rhs), field) {
					continue
				}
				// A field ASSIGNMENT (autoCookieSvc.DpapiProfileDir = …) is
				// the wiring, not a consumer; only plain locals are tainted.
				if id, ok := assign.Lhs[i].(*ast.Ident); ok {
					tainted[id.Name] = true
				}
			}
			return true
		})
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			for _, arg := range call.Args {
				text := render(arg)
				hit := strings.Contains(text, field)
				if !hit {
					if id, ok := arg.(*ast.Ident); ok && tainted[id.Name] {
						hit = true
					}
				}
				if hit {
					out = append(out, render(call.Fun))
					break
				}
			}
			return true
		})
	}
	return out
}

// TestLogDpapiProfileDirVerdict is the boot-time half of COOKIES-7 and the
// ONLY place the setting is checked before a pass runs. It is a Warn and never
// a boot failure: config.Validate checks the path's SHAPE alone and never its
// existence, because a container writes its config.toml before the volume that
// holds the profile is mounted.
//
// Mutants:
//   - stay silent off Windows -> "off Windows the key is accepted and ignored"
//     fails; a Linux operator's setting is accepted and then inert with
//     nothing saying so.
//   - make an unusable directory an ERROR (or a boot failure) -> "an unusable
//     directory warns and does not fail the boot" fails.
//   - warn unconditionally -> "unset is silent" and "a usable directory is
//     silent" fail.
//   - drop the dpapi_fallback arm -> "the key is set but dpapi_fallback is off"
//     fails, and the operator in the DEFAULT configuration is told nothing at
//     all (I-3).
func TestLogDpapiProfileDirVerdict(t *testing.T) {
	forceGOOS := func(t *testing.T, goos string) {
		t.Helper()
		real := runtimeGOOS
		runtimeGOOS = func() string { return goos }
		t.Cleanup(func() { runtimeGOOS = real })
	}
	// DpapiFallback true throughout except in the subtest that is about it:
	// the flag is what decides whether the directory is ever read, so a
	// service built without it would make every other arm unreachable.
	svc := func(log *dpapiTestLogger, dir string) *AutoCookieService {
		return &AutoCookieService{
			logger:          log,
			DpapiProfileDir: func() string { return dir },
			DpapiFallback:   true,
		}
	}

	t.Run("unset is silent", func(t *testing.T) {
		forceGOOS(t, "windows")
		log := &dpapiTestLogger{}
		svc(log, "").LogDpapiProfileDirVerdict()
		if len(log.warns) != 0 || len(log.errors) != 0 {
			t.Errorf("an unset key must say nothing; warns=%v errors=%v", log.warns, log.errors)
		}
	})

	t.Run("off Windows the key is accepted and ignored, out loud", func(t *testing.T) {
		forceGOOS(t, "linux")
		log := &dpapiTestLogger{}
		dir := mkDpapiProfileDir(t)
		svc(log, dir).LogDpapiProfileDirVerdict()
		if !dpapiLinesContain(log.warns, "Windows-only", dir) {
			t.Errorf("expected one Warn saying the key is ignored on this host and naming the directory; warns=%v", log.warns)
		}
		if len(log.errors) != 0 {
			t.Errorf("a Windows-only setting on Linux is not an error; errors=%v", log.errors)
		}
	})

	t.Run("an unusable directory warns and does not fail the boot", func(t *testing.T) {
		forceGOOS(t, "windows")
		log := &dpapiTestLogger{}
		missing := filepath.Join(t.TempDir(), "Default")
		svc(log, missing).LogDpapiProfileDirVerdict()
		if !dpapiAnyContains(log.warns, missing) {
			t.Errorf("expected one Warn naming the unusable directory; warns=%v", log.warns)
		}
		if len(log.errors) != 0 {
			t.Errorf("a missing directory at boot is a Warn, not an error; errors=%v", log.errors)
		}
	})

	t.Run("a usable directory is silent", func(t *testing.T) {
		forceGOOS(t, "windows")
		log := &dpapiTestLogger{}
		svc(log, mkDpapiProfileDir(t)).LogDpapiProfileDirVerdict()
		if len(log.warns) != 0 || len(log.errors) != 0 {
			t.Errorf("a usable directory must say nothing; warns=%v errors=%v", log.warns, log.errors)
		}
	})

	t.Run("the key is set but dpapi_fallback is off", func(t *testing.T) {
		// I-3, and the likeliest misconfiguration there is:
		// cookies.dpapi_fallback DEFAULTS TO FALSE, and the DPAPI branch in
		// refreshCookiesDetailed is gated on it, so an operator who sets only
		// this key gets a clean boot and a directory nothing ever reads. The
		// Warn names BOTH keys, because naming only the broken one leaves the
		// reader to guess which of the two to change.
		forceGOOS(t, "windows")
		log := &dpapiTestLogger{}
		dir := mkDpapiProfileDir(t) // perfectly usable — the flag is the problem
		(&AutoCookieService{
			logger:          log,
			DpapiProfileDir: func() string { return dir },
			DpapiFallback:   false,
		}).LogDpapiProfileDirVerdict()
		if !dpapiLinesContain(log.warns, "dpapi_fallback", dir) {
			t.Errorf("expected one Warn naming cookies.dpapi_fallback and the directory; warns=%v", log.warns)
		}
		if len(log.errors) != 0 {
			t.Errorf("an unused setting is not an error; errors=%v", log.errors)
		}
	})
}

// TestDpapiProfileDirIsReadLivePerConsultation pins the one property that
// keeps cookies.dpapi_profile_dir out of BOTH restart-required lists
// (restartRequiredKeys in internal/tui/settings.go, RESTART_REQUIRED_FIELDS in
// web/public/modules/settings.js, pinned against each other by
// TestRestartRequiredListsAgree): the value is a closure read on EVERY
// consultation, never a string snapshotted when the service was built.
//
// Mutant: snapshot at construction (a `dpapiProfileDir string` field filled in
// by cmd/moombox, or a memoised first answer here) -> the second consultation
// still names the OLD directory, and an operator who fixes the path in either
// UI is told nothing changed until they restart — with no UI saying a restart
// is needed, because the key is deliberately in neither list.
func TestDpapiProfileDirIsReadLivePerConsultation(t *testing.T) {
	realGOOS := runtimeGOOS
	runtimeGOOS = func() string { return "windows" }
	t.Cleanup(func() { runtimeGOOS = realGOOS })

	before := filepath.Join(t.TempDir(), "BeforeTheEdit")
	after := filepath.Join(t.TempDir(), "AfterTheEdit")
	dirs := []string{before, after}
	reads := 0

	log := &dpapiTestLogger{}
	svc := &AutoCookieService{logger: log, DpapiFallback: true, DpapiProfileDir: func() string {
		d := dirs[min(reads, len(dirs)-1)]
		reads++
		return d
	}}

	svc.LogDpapiProfileDirVerdict()
	svc.LogDpapiProfileDirVerdict()

	if reads != 2 {
		t.Errorf("DpapiProfileDir was consulted %d time(s) over two passes, want 2 — a cached answer is a "+
			"restart-required setting with nothing saying so", reads)
	}
	if !dpapiAnyContains(log.warns, before) {
		t.Errorf("the first verdict must name %q; warns=%v", before, log.warns)
	}
	if !dpapiAnyContains(log.warns, after) {
		t.Errorf("the second verdict must name the edited directory %q, not the one read at construction; warns=%v",
			after, log.warns)
	}
}
