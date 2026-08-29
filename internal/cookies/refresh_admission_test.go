package cookies

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// This file covers B2: which Set-Cookie headers from the network may become a
// cookie-file update at all. That decision lives in admitSetCookie, and every
// case below asserts a VERDICT — admitted with these exact key fields and these
// exact Delete/Value/Expiry values, or refused. "Did not panic" is not an
// outcome any row here accepts.
//
// The defect: admitSetCookie's loop used to open with a substring pre-filter
// that ran before Domain= was parsed, so a header had to literally contain
// "youtube.com" or "google.com" to be looked at. A genuinely unscoped
// Set-Cookie — no Domain= at all, which RFC 6265 §4.1.2.3 host-scopes to the
// responding host, and which is how youtube.com rotates its own first-party
// cookies — contains neither string and was dropped without ever being parsed.

// b2UnscopedHeader is THE header this task exists for, and its exact text is
// load-bearing twice over.
//
// "The cookie ended up in the file" is a JUNCTION: the scoped path and the
// unscoped path both write there, so a proof about the unscoped path has to use
// a header that ONLY the unscoped path can admit. This one carries no Domain=,
// and no "youtube.com" or "google.com" substring anywhere — not in the name, not
// in the value, not in an attribute. Against the pre-filter it is invisible;
// with the pre-filter gone it is admitted by the tracked-name rule and by
// nothing else. TestB2HeaderCannotPassTheOldPreFilter pins that property so a
// later edit cannot quietly make this fixture prove something weaker.
const b2UnscopedHeader = "SAPISID=rotated-without-a-domain; Path=/; Secure"

// oneYear is admitSetCookie's default expiry when the server named none.
const oneYear = int64(365 * 24 * 60 * 60)

// TestB2HeaderCannotPassTheOldPreFilter guards the fixture, not the code. If
// somebody edits b2UnscopedHeader into something containing either substring,
// every B2 assertion in this file silently degrades into a test of the scoped
// path, which was never broken.
func TestB2HeaderCannotPassTheOldPreFilter(t *testing.T) {
	lower := strings.ToLower(b2UnscopedHeader)
	if strings.Contains(lower, "youtube.com") || strings.Contains(lower, "google.com") {
		t.Fatalf("b2UnscopedHeader %q would have passed the removed substring pre-filter — "+
			"it proves nothing about the unscoped path", b2UnscopedHeader)
	}
	if strings.Contains(lower, "domain=") {
		t.Fatalf("b2UnscopedHeader %q carries a Domain= — it is not the unscoped case", b2UnscopedHeader)
	}
}

// admitCase is one row of the hostile-input table. origin is originYouTube for
// every row here; TestAdmitSetCookieOriginDecidesAdmission varies it separately.
type admitCase struct {
	name   string
	header string
	admit  bool

	// Asserted only when admit is true.
	wantKey      cookieUpdateKey
	wantValue    string
	wantHTTPOnly bool
	wantDelete   bool
	// wantExpiryOffset is seconds from now. 0 means the one-year default.
	wantExpiryOffset int64
}

// TestAdmitSetCookieTable is the deliverable: hostile and legitimate Set-Cookie
// shapes, each with an asserted verdict.
//
// Which rows discriminate the fix from the pre-fix code is noted per row. Rows
// that pass either way are regression guards and are labelled as such, so nobody
// later mistakes one for a proof.
func TestAdmitSetCookieTable(t *testing.T) {
	cases := []admitCase{
		// --- the unscoped branch: everything the pre-filter made unreachable ---
		{
			// B2. Discriminates: pre-fix this header never reached the parser.
			name: "unscoped tracked name is admitted", header: b2UnscopedHeader,
			admit: true, wantKey: cookieUpdateKey{Name: "SAPISID"}, wantValue: "rotated-without-a-domain",
		},
		{
			// Discriminates, and carries the HttpOnly attribute through the branch
			// that could not previously be entered.
			name:  "unscoped HttpOnly is admitted and keeps the flag",
			admit: true, header: "LOGIN_INFO=fixture-login; Path=/; Secure; HttpOnly",
			wantKey: cookieUpdateKey{Name: "LOGIN_INFO"}, wantValue: "fixture-login", wantHTTPOnly: true,
		},
		{
			// Discriminates. The deletion form of the same branch: Max-Age=0 with
			// no Domain=. TestUnscopedDeletionFromTheWireStaysInsideTheOrigin
			// carries this one the rest of the way through updateCookieFile.
			name:  "unscoped deletion is admitted as a deletion",
			admit: true, header: "SAPISID=; Max-Age=0",
			wantKey: cookieUpdateKey{Name: "SAPISID"}, wantValue: "", wantDelete: true,
		},
		{
			// Pins the UNION in trackedCookieName. __Secure-3PSIDCC-extra is not in
			// essentialYouTubeCookies but is an isGoogleOnlyAuthName prefix match,
			// and CookieJar.Load's own isGoogleAuth clause admits it on .google.com
			// — so dropping either half of the union strands a cookie the jar can
			// actually store. Discriminates against the pre-filter too.
			name:  "unscoped google-only auth prefix is admitted",
			admit: true, header: "__Secure-3PSIDCC-extra=fixture-3psidcc; Path=/",
			wantKey: cookieUpdateKey{Name: "__Secure-3PSIDCC-extra"}, wantValue: "fixture-3psidcc",
		},
		{
			// The narrowing that keeps the new surface narrow. Delete the
			// trackedCookieName check and this row starts passing.
			name:  "unscoped untracked name is refused",
			admit: false, header: "foo=bar",
		},
		{
			// RFC 6265 §5.2.3: a Domain= with an empty value is ignored, leaving
			// the cookie host-only. So this takes the unscoped branch, not a
			// scoped branch with a "." domain.
			name:  "bare Domain= is host-only, not a scoped domain",
			admit: true, header: "SAPISID=fixture-sapisid; Domain=; Path=/",
			wantKey: cookieUpdateKey{Name: "SAPISID"}, wantValue: "fixture-sapisid",
		},
		{
			// Regression guard on Max-Age's positive branch, reached through the
			// newly-live unscoped path.
			name:  "unscoped Max-Age sets the expiry it names",
			admit: true, header: "YSC=fixture-ysc; Max-Age=600; Path=/",
			wantKey: cookieUpdateKey{Name: "YSC"}, wantValue: "fixture-ysc", wantExpiryOffset: 600,
		},

		// --- the scoped branch: unchanged, and now the only domain check ---
		{
			name:  "scoped .google.com is admitted",
			admit: true, header: "SAPISID=fixture-sapisid; Domain=.google.com",
			wantKey: cookieUpdateKey{Name: "SAPISID", Domain: ".google.com"}, wantValue: "fixture-sapisid",
		},
		{
			// The leading dot is added, because it is what the Netscape row's
			// include-subdomains flag is derived from downstream.
			name:  "scoped google.com is normalised to a leading dot",
			admit: true, header: "SAPISID=fixture-sapisid; Domain=google.com",
			wantKey: cookieUpdateKey{Name: "SAPISID", Domain: ".google.com"}, wantValue: "fixture-sapisid",
		},
		{
			name:  "scoped .YouTube.com is normalised to lower case",
			admit: true, header: "LOGIN_INFO=fixture-login; Domain=.YouTube.com",
			wantKey: cookieUpdateKey{Name: "LOGIN_INFO", Domain: ".youtube.com"}, wantValue: "fixture-login",
		},
		{
			// Regression guard: suffix-anchored matching, not containment.
			name:  "a host that merely ends with the target is refused",
			admit: false, header: "SAPISID=v; Domain=accounts.google.com.evil.tld",
		},
		{
			name:  "an unrelated host is refused",
			admit: false, header: "SAPISID=v; Domain=evil.tld",
		},
		{
			// THE proof that the substring filter was never the real guard: the
			// header contains "youtube.com" — in its VALUE — and is refused on its
			// parsed Domain=. It passed the pre-filter and was refused below it,
			// exactly as it is refused now.
			name:  "a substring in the value buys nothing",
			admit: false, header: "x=youtube.com; Domain=evil.tld",
		},
		{
			// Another platform's domain is not this origin's. Refused on the same
			// rule as evil.tld — cookiePlatformOf(".twitch.tv") is not the declared
			// origin's platform.
			name:  "a cross-platform domain is refused",
			admit: false, header: "SAPISID=v; Domain=.twitch.tv",
		},
		{
			// The brief's "make sure it is". A bare dot names no host: the domain
			// normalisation leaves it alone (it already has the prefix) and
			// cookiePlatformOf places it nowhere.
			name:  "a bare dot domain is refused",
			admit: false, header: "SAPISID=v; Domain=.",
		},
		{
			// Pre-existing behaviour, pinned so that narrowing it later is a
			// deliberate change and not a silent one. Scoped headers are admitted
			// under any name; only unscoped ones are name-gated.
			name:  "scoped untracked name is admitted",
			admit: true, header: "foo=bar; Domain=.youtube.com",
			wantKey: cookieUpdateKey{Name: "foo", Domain: ".youtube.com"}, wantValue: "bar",
		},

		// --- row-breaking characters, refused before either branch ---
		{
			// The discriminating one. The DOMAIN is perfectly good, so step 2 has
			// no objection; only the name check stands between a tab and a row
			// that splits into the wrong fields on the next CookieJar.Load.
			name:  "a tab in the name is refused",
			admit: false, header: "SA\tPISID=v; Domain=.google.com",
		},
		{
			name:  "a CR in the name is refused",
			admit: false, header: "SAPI\rSID=v; Domain=.google.com",
		},
		{
			name:  "a LF in the name is refused",
			admit: false, header: "SAPI\nSID=v; Domain=.google.com",
		},
		{
			name:  "a NUL in the name is refused",
			admit: false, header: "SAPISID\x00X=v; Domain=.google.com",
		},
		{
			// Regression guard rather than a discriminator, and worth saying why:
			// a domain carrying an interior row-breaker can never match
			// google.com anyway, so step 2 would refuse it too. Step 1 makes the
			// refusal explicit instead of incidental — the day the domain rule
			// loosens, this stays refused.
			name:  "a LF in the domain is refused",
			admit: false, header: "SAPISID=v; Domain=.goo\ngle.com",
		},
		{
			// The exemption, and it is deliberate: CookieJar.Load joins fields
			// 6.. back into one value and updateCookieFile rebuilds from exactly
			// seven fields, so a tab inside a VALUE is legal input. Refusing it
			// would drop a legitimate rotation.
			name:  "a tab in the value is admitted and preserved verbatim",
			admit: true, header: "SAPISID=a\tb; Domain=.google.com",
			wantKey: cookieUpdateKey{Name: "SAPISID", Domain: ".google.com"}, wantValue: "a\tb",
		},

		// --- malformed shapes ---
		{
			// Pins the pre-existing name == "" check.
			name:  "an empty name is refused",
			admit: false, header: "=v; Domain=.google.com",
		},
		{
			name:  "a header with no equals sign is refused",
			admit: false, header: "SAPISID",
		},
		{
			name:  "an empty header is refused",
			admit: false, header: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := time.Now().Unix()
			key, cu, ok := admitSetCookie(tc.header, originYouTube)
			after := time.Now().Unix()

			if ok != tc.admit {
				t.Fatalf("admitted = %v, want %v for %q", ok, tc.admit, tc.header)
			}
			if !ok {
				// A refusal must be total: no key, no update, nothing a caller
				// could accidentally read past the boolean.
				if key != (cookieUpdateKey{}) || cu != (cookieUpdate{}) {
					t.Errorf("a refused header handed back key %+v update %+v, want both zero", key, cu)
				}
				return
			}
			if key != tc.wantKey {
				t.Errorf("key = %+v, want %+v", key, tc.wantKey)
			}
			if cu.Value != tc.wantValue {
				t.Errorf("value = %q, want %q", cu.Value, tc.wantValue)
			}
			if cu.HTTPOnly != tc.wantHTTPOnly {
				t.Errorf("HTTPOnly = %v, want %v", cu.HTTPOnly, tc.wantHTTPOnly)
			}
			if cu.Delete != tc.wantDelete {
				t.Errorf("Delete = %v, want %v", cu.Delete, tc.wantDelete)
			}
			offset := tc.wantExpiryOffset
			if offset == 0 {
				offset = oneYear
			}
			// A window, because the expiry is derived from time.Now() inside the
			// function and a run can straddle a second boundary.
			if cu.Expiry < before+offset || cu.Expiry > after+offset {
				t.Errorf("expiry = %d, want now+%d (window %d..%d)", cu.Expiry, offset, before+offset, after+offset)
			}
		})
	}
}

// TestAdmitSetCookieOriginDecidesAdmission covers the other half of the rule:
// the declared origin, not the function, decides where a header may land.
//
// The unscoped branch has no domain of its own, so the origin is the ONLY thing
// that can place it — and the scoped branch is judged against the same declared
// platform, so a header cannot slip past by naming its own destination.
//
// The originGoogle rows are the controls: youtube.com and google.com are one
// credential platform, so a google.com response's cookies are admitted exactly
// as a youtube.com response's are. Without them "refuse everything" would pass.
func TestAdmitSetCookieOriginDecidesAdmission(t *testing.T) {
	cases := []struct {
		name   string
		header string
		origin cookieOrigin
		admit  bool
	}{
		{"unscoped from the declaring site", b2UnscopedHeader, originYouTube, true},
		{"unscoped from its sibling site", b2UnscopedHeader, originGoogle, true},
		{"unscoped from another platform", b2UnscopedHeader, originTwitch, false},
		{"unscoped from no declared origin", b2UnscopedHeader, cookieOrigin(""), false},

		{"scoped google from a youtube caller", "SAPISID=v; Domain=.google.com", originYouTube, true},
		{"scoped google from a google caller", "SAPISID=v; Domain=.google.com", originGoogle, true},
		{"scoped google from a twitch caller", "SAPISID=v; Domain=.google.com", originTwitch, false},
		{"scoped google from no declared origin", "SAPISID=v; Domain=.google.com", cookieOrigin(""), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, ok := admitSetCookie(tc.header, tc.origin)
			if ok != tc.admit {
				t.Errorf("admitted = %v, want %v — origin %q on %q", ok, tc.admit, tc.origin, tc.header)
			}
		})
	}
}

// --- the round trips: header on the wire -> cookies.txt -> jar ---

// TestB2UnscopedRotationReachesTheJar is the end-to-end proof, and it is the one
// the owner's symptom maps onto: an authenticated guide reply rotates SAPISID
// with no Domain=, and cookies.txt must move.
//
// The assertion is the Cookie header the jar produces, not a log line: the point
// is that the next authenticated request carries the new value.
//
// Discriminates: restore the substring pre-filter and the file is byte-identical
// afterwards and the jar still emits the stale value — which is exactly the
// "cookies.txt untouched for a day" report.
func TestB2UnscopedRotationReachesTheJar(t *testing.T) {
	pointYouTubeGuideAt(t, bodyServer(t, loggedInGuideBody, b2UnscopedHeader))

	jar := jarWithAuth(t)
	path := jar.GetFilePath()
	if got := jar.GetCookieFor(PlatformYouTube, "SAPISID"); got == "" || got == "rotated-without-a-domain" {
		t.Fatalf("premise broken: the fixture jar's SAPISID is %q — it must hold a different, non-empty value", got)
	}

	rs := NewRefreshService(jar, 0, nopLogger{})
	auth, err := rs.checkAndRefreshYouTube(context.Background())
	if err != nil || !auth {
		t.Fatalf("premise broken: auth=%v err=%v, want true/nil — the reply says logged_in=1", auth, err)
	}

	// The row, on the domain the declared origin placed it on.
	row := rowFor(t, readCookieRows(t, path), "SAPISID", ".youtube.com")
	if row.value != "rotated-without-a-domain" {
		t.Errorf(".youtube.com SAPISID = %q, want the rotated value — an unscoped Set-Cookie "+
			"from youtube.com was dropped instead of applied", row.value)
	}
	if len(row.fields) != 7 {
		t.Errorf("rewritten row has %d tab-separated fields, want 7: %q", len(row.fields), row.raw)
	}

	// And the jar, which is what the next request actually reads. processYouTube-
	// SetCookies reloads it; assert through the header rather than the map.
	if got := jar.GetCookieHeader(); !strings.Contains(got, "SAPISID=rotated-without-a-domain") {
		t.Errorf("the jar's Cookie header does not carry the rotated SAPISID — "+
			"the file moved but the session did not (header names only: %q)", cookieNamesIn(got))
	}

	// Nothing else was collateral damage.
	if got := jar.GetCookieFor(PlatformYouTube, "LOGIN_INFO"); got != "login-info-value" {
		t.Errorf("LOGIN_INFO = %q, want the fixture value untouched", got)
	}
}

// cookieNamesIn reduces a Cookie header to its names, so a failure message can
// describe the jar's contents without printing a single cookie value.
func cookieNamesIn(header string) []string {
	var names []string
	for _, pair := range strings.Split(header, ";") {
		if name, _, ok := strings.Cut(strings.TrimSpace(pair), "="); ok && name != "" {
			names = append(names, name)
		}
	}
	return names
}

// TestUnscopedDeletionFromTheWireStaysInsideTheOrigin carries the unscoped
// deletion row of the table through the write path.
//
// resolveRowUpdate's rule 2 has always confined a Domain-less deletion to the
// declared origin's site — but with the pre-filter in place no Domain-less
// header ever reached it, so the rule was dead code. This is that rule from the
// wire: `SAPISID=; Max-Age=0` from a youtube.com reply removes the .youtube.com
// row and must not touch the .google.com one, which is where the Google session
// auth actually lives.
//
// Discriminates: pre-fix neither row moved, because the header was never parsed.
func TestUnscopedDeletionFromTheWireStaysInsideTheOrigin(t *testing.T) {
	initial := "# Netscape HTTP Cookie File\n" +
		".google.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tfixture-google\n" +
		".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tfixture-youtube\n"
	rs, _, path := newSetCookieFixture(t, nopLogger{}, initial)

	rs.processYouTubeSetCookies(setCookieResponse("SAPISID=; Max-Age=0"))

	rows := readCookieRows(t, path)
	for _, r := range rowsNamed(rows, "SAPISID") {
		if r.domain == ".youtube.com" {
			t.Errorf("the .youtube.com row survived an unscoped deletion from youtube.com: %q", r.raw)
		}
	}
	google := rowFor(t, rows, "SAPISID", ".google.com")
	if google.value != "fixture-google" {
		t.Errorf(".google.com SAPISID = %q — an unscoped deletion reached outside the declared origin", google.value)
	}
	if google.expiry != "2000000000" {
		t.Errorf(".google.com SAPISID expiry = %q, want unchanged", google.expiry)
	}
}

// TestUnscopedUntrackedNameIsNotWritten is the narrow half of the new surface,
// end to end. Opening the unscoped path must not turn every stray first-party
// cookie youtube.com sets into a row in a credential file.
//
// Discriminates against a fix that merely deletes the pre-filter: without the
// tracked-name check, a .youtube.com "sessionid" row appears.
func TestUnscopedUntrackedNameIsNotWritten(t *testing.T) {
	initial := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t2000000000\tLOGIN_INFO\tfixture-login\n"
	rs, _, path := newSetCookieFixture(t, nopLogger{}, initial)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	rs.processYouTubeSetCookies(setCookieResponse(
		"sessionid=fixture-session; Path=/; Secure; HttpOnly",
		"_ga=fixture-analytics; Path=/",
	))

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("untracked unscoped cookies rewrote the file:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestRowBreakingNameNeverReachesTheFile is step 1 end to end, on the shape that
// actually gets past everything else: a tab in the NAME of a header whose
// Domain= is a real Google host.
//
// A tab is the Netscape field separator. Written out, "SA<tab>PISID" turns a
// seven-field row into an eight-field one, and the next CookieJar.Load reads the
// fields off by one — the domain column stays put but name and value slide, so
// the row either loads as a different cookie or is skipped as malformed. Either
// way a live credential file has been corrupted by a header.
//
// Discriminates: delete the row-breaking-character check and the file grows a
// row whose reload no longer produces the cookie it was supposed to.
func TestRowBreakingNameNeverReachesTheFile(t *testing.T) {
	for _, header := range []string{
		"SA\tPISID=fixture-tab; Domain=.google.com; Path=/",
		"SA\rPISID=fixture-cr; Domain=.google.com; Path=/",
		"SA\nPISID=fixture-lf; Domain=.google.com; Path=/",
		"SA\x00PISID=fixture-nul; Domain=.google.com; Path=/",
	} {
		t.Run(strings.Map(func(r rune) rune {
			if r < 0x20 {
				return '_'
			}
			return r
		}, header), func(t *testing.T) {
			initial := "# Netscape HTTP Cookie File\n" +
				".youtube.com\tTRUE\t/\tTRUE\t2000000000\tLOGIN_INFO\tfixture-login\n"
			rs, _, path := newSetCookieFixture(t, nopLogger{}, initial)

			rs.processYouTubeSetCookies(setCookieResponse(header))

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != initial {
				t.Errorf("a row-breaking character reached cookies.txt:\n%q", string(data))
			}
			// The file must still read back as the one cookie it started with.
			fresh := NewCookieJar()
			if err := fresh.Load(path); err != nil {
				t.Fatal(err)
			}
			if got := fresh.GetCookieFor(PlatformYouTube, "LOGIN_INFO"); got != "fixture-login" {
				t.Errorf("a freshly loaded jar reads LOGIN_INFO as %q — the file was corrupted", got)
			}
		})
	}
}
