package cookies

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nopLogger implements the 4-method logger interface used by RefreshService.
type nopLogger struct{}

func (nopLogger) Debug(msg string, args ...any) {}
func (nopLogger) Info(msg string, args ...any)  {}
func (nopLogger) Warn(msg string, args ...any)  {}
func (nopLogger) Error(msg string, args ...any) {}

// TestUpdateCookieFileReplacesDuplicateRows verifies that when a cookie name
// exists multiple times in the file (e.g. both .youtube.com and .google.com),
// every matching row gets the new value — not just the first occurrence
// (finding #4 in reports/cookies.md).
func TestUpdateCookieFileReplacesDuplicateRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	initial := `# Netscape HTTP Cookie File
.google.com	TRUE	/	TRUE	1000	SAPISID	stale_google_value
.youtube.com	TRUE	/	TRUE	1000	SAPISID	stale_youtube_value
.youtube.com	TRUE	/	TRUE	0	LOGIN_INFO	logininfo
`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	rs := NewRefreshService(jar, 0, nopLogger{})

	updates := map[cookieUpdateKey]cookieUpdate{
		{Name: "SAPISID", Domain: ".youtube.com"}: {Value: "fresh_value", Expiry: 2000},
	}
	// originYouTube is what processYouTubeSetCookies declares. It does not
	// enter this case at all — the update carries its own Domain=, so
	// resolveRowUpdate reaches the .google.com row through rule 3's
	// within-one-platform refresh — but the parameter is required and stating
	// the real caller's value keeps the test faithful to the production path.
	if err := rs.updateCookieFile(updates, originYouTube); err != nil {
		t.Fatalf("updateCookieFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "stale_google_value") {
		t.Errorf("expected stale .google.com SAPISID to be refreshed, file still contains it:\n%s", string(got))
	}
	if strings.Contains(string(got), "stale_youtube_value") {
		t.Errorf("expected stale .youtube.com SAPISID to be refreshed, file still contains it:\n%s", string(got))
	}
	if strings.Count(string(got), "fresh_value") != 2 {
		t.Errorf("expected both SAPISID rows to carry fresh_value, got:\n%s", string(got))
	}
}

// TestUpdateCookieFileSubdomainFlag verifies that newly added rows set the
// Netscape include-subdomains flag based on whether Domain= starts with "."
// (finding #5 in reports/cookies.md).
func TestUpdateCookieFileSubdomainFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	initial := "# Netscape HTTP Cookie File\n"
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	rs := NewRefreshService(jar, 0, nopLogger{})

	updates := map[cookieUpdateKey]cookieUpdate{
		{Name: "LOGIN_INFO", Domain: ".youtube.com"}: {Value: "v1", Expiry: 1000},
		{Name: "login", Domain: "host.youtube.com"}:  {Value: "v2", Expiry: 1000},
	}
	// Both rows take the INSERTION path (the file is empty), and the no-dot
	// domain is the case that matters: the flag must come from the leading dot,
	// not be hardcoded TRUE.
	//
	// The no-dot fixture used to be "host.twitch.tv". It is a YouTube host now
	// because insertion refuses to write outside the declared origin's platform,
	// so a Twitch row under originYouTube is no longer reachable through this
	// function at all — see TestUnscopedRefreshCrossesDomainsOnlyInsideThe
	// DeclaredPlatform for that rule. The claim under test is unchanged: a domain
	// with no leading dot still gets FALSE, and still would have failed against
	// the hardcoded-TRUE behaviour this test was written for.
	if err := rs.updateCookieFile(updates, originYouTube); err != nil {
		t.Fatalf("updateCookieFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(got), "\n")
	var dottedLine, noDotLine string
	for _, l := range lines {
		if strings.Contains(l, "LOGIN_INFO") {
			dottedLine = l
		}
		if strings.Contains(l, "login") && !strings.Contains(l, "LOGIN_INFO") {
			noDotLine = l
		}
	}
	if dottedLine == "" {
		t.Fatalf("expected LOGIN_INFO row in %s", string(got))
	}
	if noDotLine == "" {
		t.Fatalf("expected login row in %s", string(got))
	}
	dottedParts := strings.Split(dottedLine, "\t")
	noDotParts := strings.Split(noDotLine, "\t")
	if len(dottedParts) < 2 || dottedParts[1] != "TRUE" {
		t.Errorf(".youtube.com row should have include_subdomains=TRUE, got %q", dottedLine)
	}
	// "host.youtube.com" lacks a leading dot, so the subdomain flag must be FALSE.
	if len(noDotParts) < 2 || noDotParts[1] != "FALSE" {
		t.Errorf("no-dot domain row should have include_subdomains=FALSE, got %q", noDotLine)
	}
}

// TestUpdateCookieFileUnscopedInsertUsesTheDeclaredOrigin replaces
// TestUpdateCookieFileFallsBackToDomainHeuristic, which pinned the rule this
// test overturns.
//
// The old rule (finding #40): with no Domain=, route SID/HSID/SSID/APISID/
// SAPISID/__Secure-[13]P* to .google.com and everything else to .youtube.com.
// That branch was unreachable for its whole life — processYouTubeSetCookies
// dropped every Domain-less header before it could become an unscoped key — so
// the heuristic was never exercised against a real response. When B2 made the
// unscoped path live it turned out to be backwards: a Set-Cookie with no Domain=
// is host-scoped to the RESPONSE, not classified by its name, so an ordinary
// youtube.com reply rotating SID had it written onto .google.com and sent to
// accounts.google.com from then on. The two are different cookies.
//
// The domain now comes from the declared origin and nothing else. This test is
// the same two names under two origins, which separates the rules cleanly: the
// heuristic says "SAPISID→google, LOGIN_INFO→youtube" regardless of caller, and
// the origin rule says "both go wherever the caller came from".
func TestUpdateCookieFileUnscopedInsertUsesTheDeclaredOrigin(t *testing.T) {
	cases := []struct {
		name   string
		origin cookieOrigin
		want   string
	}{
		// Discriminates: the heuristic would have split these two across
		// .google.com and .youtube.com.
		{"youtube caller", originYouTube, ".youtube.com"},
		// Discriminates the other way: the heuristic would have put LOGIN_INFO on
		// .youtube.com even though the response came from google.com.
		{"google caller", originGoogle, ".google.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "cookies.txt")
			if err := os.WriteFile(path, []byte("# Netscape HTTP Cookie File\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			jar := NewCookieJar()
			if err := jar.Load(path); err != nil {
				t.Fatal(err)
			}
			rs := NewRefreshService(jar, 0, nopLogger{})

			updates := map[cookieUpdateKey]cookieUpdate{
				{Name: "SAPISID"}:    {Value: "s", Expiry: 1},
				{Name: "LOGIN_INFO"}: {Value: "l", Expiry: 1},
			}
			if err := rs.updateCookieFile(updates, tc.origin); err != nil {
				t.Fatalf("updateCookieFile: %v", err)
			}

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for name, value := range map[string]string{"SAPISID": "s", "LOGIN_INFO": "l"} {
				want := tc.want + "\tTRUE\t/\t"
				if !strings.Contains(string(got), want) || !strings.Contains(string(got), name+"\t"+value) {
					t.Errorf("expected %s row under %s, got:\n%s", name, tc.want, string(got))
				}
			}
			// Nothing may land on the OTHER domain of the same platform. Without
			// this the assertion above passes on a file that has both.
			other := ".google.com"
			if tc.want == ".google.com" {
				other = ".youtube.com"
			}
			if strings.Contains(string(got), other) {
				t.Errorf("a %s caller wrote a %s row — the domain must come from the origin, "+
					"not from the cookie name:\n%s", tc.origin, other, string(got))
			}
		})
	}
}
