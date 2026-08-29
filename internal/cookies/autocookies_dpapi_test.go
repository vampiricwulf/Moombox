package cookies

import (
	"fmt"
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
func stubDpapiProfiles(t *testing.T, profiles []dpapi.BrowserProfile, byPath map[string][]dpapi.ChromeCookie) {
	t.Helper()
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

	out, err := dpapiExtractAsNetscape(log, "")
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

	out, err := dpapiExtractAsNetscape(log, "")
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
	out, err := dpapiExtractAsNetscape(log, "edge")
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

	out, err := dpapiExtractAsNetscape(log, "edge")
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

	out, err := dpapiExtractAsNetscape(log, "chrome")
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

	out, err := dpapiExtractAsNetscape(log, "chrome")
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

	out, err := dpapiExtractAsNetscape(log, "brave")
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

	out, err := dpapiExtractAsNetscape(log, "opera")
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

	out, err := dpapiExtractAsNetscape(log, "")
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

	_, err := dpapiExtractAsNetscape(log, "brave")
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
