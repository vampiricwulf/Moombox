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

// fourHundredDays is RFC 6265bis §5.5's lifetime cap, restated here rather than
// read from maxCookieLifetime so that changing the constant fails these rows
// instead of silently moving them with it.
const fourHundredDays = int64(400 * 24 * 60 * 60)

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
			// newly-live unscoped path. It is also the control for the clamp two
			// rows down: without it "always 400 days" would pass.
			name:  "unscoped Max-Age sets the expiry it names",
			admit: true, header: "YSC=fixture-ysc; Max-Age=600; Path=/",
			wantKey: cookieUpdateKey{Name: "YSC"}, wantValue: "fixture-ysc", wantExpiryOffset: 600,
		},
		{
			// RFC 6265bis §5.5 caps a cookie's lifetime at 400 days, and Chrome,
			// Edge and Safari all enforce it — so a browser-exported cookies.txt
			// never carries a longer expiry, and the clamp writes the shape the
			// rest of the file already speaks. 63072000 is two years.
			name:  "an over-long Max-Age is clamped to 400 days",
			admit: true, header: "PREF=fixture-pref; Max-Age=63072000; Path=/",
			wantKey: cookieUpdateKey{Name: "PREF"}, wantValue: "fixture-pref",
			wantExpiryOffset: fourHundredDays,
		},
		{
			// The overflow, and it is the reason the clamp is not merely tidy.
			// `now + 9223372036854775807` wraps to a large NEGATIVE expiry, and
			// every expiry guard in this package is `exp > 0 && exp < now`
			// (rowExpired, CookieJar.Load's capture, ExpiredAuthCookiesFor) — so
			// a negative field 5 reads as "not expired" everywhere: an unprunable
			// row, invisible to the freshness accounting, and one yt-dlp's loader
			// rejects outright on its `[0-9]+` expires match. Clamping the addend
			// makes the addition unreachable rather than checked afterwards.
			//
			// Asserted as an expiry WINDOW, which also pins that it is positive.
			name:  "a Max-Age that would overflow int64 is clamped, not wrapped",
			admit: true, header: "SAPISID=fixture-sapisid; Max-Age=9223372036854775807; Path=/",
			wantKey: cookieUpdateKey{Name: "SAPISID"}, wantValue: "fixture-sapisid",
			wantExpiryOffset: fourHundredDays,
		},
		{
			// RFC 6265 §5.2 step 3: WS is trimmed from the name-string and the
			// value-string SEPARATELY. Trimming only the whole `name=value` pair
			// left this parsed as the name "SAPISID " — which no predicate in this
			// package recognises, so the unscoped branch refused it — and the
			// value " fixture-sapisid".
			//
			// This widens admission slightly, in the conformant direction.
			name:  "whitespace around the equals sign is trimmed from both halves",
			admit: true, header: "SAPISID = fixture-sapisid ; Path=/",
			wantKey: cookieUpdateKey{Name: "SAPISID"}, wantValue: "fixture-sapisid",
		},
		{
			// WS in §5.2 is SP *and* HTAB, so a tab that is only padding is
			// TRIMMED rather than refused by step 1. The trim rule and the
			// interior-tab rule do not collide: this row and "a tab in the value
			// is refused" below have to hold at the same time.
			name:  "a padding tab is trimmed, not read as row-breaking",
			admit: true, header: "SAPISID=\tfixture-sapisid\t; Path=/",
			wantKey: cookieUpdateKey{Name: "SAPISID"}, wantValue: "fixture-sapisid",
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
			// Overturned in fix round 1. This row used to assert "admitted, value
			// preserved verbatim", on the reasoning that CookieJar.Load joins
			// fields 6.. so a tab in a value is legal input and refusing it would
			// drop a legitimate rotation. That last clause was false: RFC 6265's
			// cookie-octet excludes HTAB and every control character, browsers
			// reject them, and no real rotation carries one. What admitting it
			// actually bought was a row with EIGHT tab-separated fields — which
			// yt-dlp's own MozillaCookieJar.load refuses (`invalid length 8`,
			// it requires exactly 7), so the cookie silently vanished for anyone
			// sharing the file with yt-dlp.
			//
			// CookieJar.Load's tolerance of tab-carrying rows ALREADY in the file
			// is a separate rule and is unchanged — it must keep reading whatever
			// a browser export or another tool wrote. See
			// TestProcessSetCookiesPreservesTabbedValue, which still passes. This
			// rule governs only what THIS writer may add.
			name:  "a tab in the value is refused",
			admit: false, header: "SAPISID=a\tb; Domain=.google.com",
		},

		// --- malformed shapes ---
		{
			// Pins the pre-existing name == "" check.
			name:  "an empty name is refused",
			admit: false, header: "=v; Domain=.google.com",
		},
		{
			// Regression guard on the same check after fix round 1 moved it BELOW
			// the per-field WS trim: a name that is only whitespace must still be
			// no name at all.
			name:  "an all-whitespace name is refused",
			admit: false, header: " \t =v; Domain=.google.com",
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

// TestUnscopedInsertionLandsOnTheDeclaredOrigin covers the branch this task's
// own fix brought back to life, and the defect that was waiting in it.
//
// updateCookieFile's insertion loop has always had a `domain == ""` fallback for
// a Set-Cookie with no Domain=. It was DEAD CODE for as long as it existed —
// the substring pre-filter dropped every Domain-less header before it could
// become an unscoped key — and while it was dead it guessed the domain from the
// cookie NAME: .google.com whenever isGoogleOnlyAuthName said so. Making the
// unscoped path reachable made that guess live, and it is the exact inverse of
// resolveRowUpdate's rule 2: an ordinary youtube.com reply host-scoping SID to
// www.youtube.com had it written as a `.google.com` row, and every later request
// to accounts.google.com carried it. A host-only youtube.com SID and the
// .google.com SID are DIFFERENT COOKIES; the real one is rotated by
// accounts.google.com with an explicit Domain=, which takes the scoped path and
// never reaches this branch.
//
// Discriminates in both directions. The name heuristic put SID on .google.com
// regardless of caller; the origin rule puts it on the caller's own site, so the
// two disagree for originYouTube (the live case) and agree for originGoogle —
// which is why the originGoogle row is a control and not a proof.
func TestUnscopedInsertionLandsOnTheDeclaredOrigin(t *testing.T) {
	// The live path first: a real Set-Cookie, through processYouTubeSetCookies.
	t.Run("from the wire", func(t *testing.T) {
		rs, jar, path := newSetCookieFixture(t, nopLogger{}, "# Netscape HTTP Cookie File\n")

		// SID is isGoogleOnlyAuthName's headline name, so this is precisely the
		// header the old heuristic sent to .google.com. No substring anywhere, so
		// only the unscoped path can admit it.
		rs.processYouTubeSetCookies(setCookieResponse("SID=unscoped-only; Path=/; Secure"))

		rows := rowsNamed(readCookieRows(t, path), "SID")
		if len(rows) != 1 {
			t.Fatalf("want exactly 1 SID row, got %d: %+v", len(rows), rows)
		}
		if rows[0].domain != ".youtube.com" {
			t.Errorf("an unscoped SID from a youtube.com reply landed on %q, want .youtube.com — "+
				"a cookie the response host-scoped to youtube.com is now sent to accounts.google.com",
				rows[0].domain)
		}
		if rows[0].value != "unscoped-only" {
			t.Errorf("SID value = %q, want unscoped-only", rows[0].value)
		}
		// And it is readable: the jar was reloaded from the file this wrote.
		if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != "unscoped-only" {
			t.Errorf("jar SID = %q, want unscoped-only", got)
		}
	})

	// The rule itself, across origins, through the write path directly.
	cases := []struct {
		name       string
		origin     cookieOrigin
		wantDomain string // "" means: nothing may be written
	}{
		// The discriminator: the name says google, the origin says youtube, and
		// the origin wins.
		{"youtube caller", originYouTube, ".youtube.com"},
		// The control. Agrees with the retired heuristic, so it proves nothing on
		// its own — without it, "never insert" would pass.
		{"google caller", originGoogle, ".google.com"},
		// Now placed correctly rather than refused: the invented domain is the
		// origin's own site, so a Twitch caller's unscoped row lands on
		// .twitch.tv. It used to be refused because the NAME heuristic sent it to
		// .google.com and the platform guard caught it — the right outcome for
		// the wrong reason, and the reason is what mattered.
		{"twitch caller", originTwitch, ".twitch.tv"},
		// An undeclared origin invents ".", which is on no platform, so the
		// insertion guard refuses it. The narrow direction.
		{"no declared origin", cookieOrigin(""), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs, _, path := newSetCookieFixture(t, nopLogger{}, "# Netscape HTTP Cookie File\n")

			updates := map[cookieUpdateKey]cookieUpdate{
				{Name: "SID"}: {Value: "inserted-value", Expiry: 2100000000},
			}
			if err := rs.updateCookieFile(updates, tc.origin); err != nil {
				t.Fatalf("updateCookieFile: %v", err)
			}

			rows := rowsNamed(readCookieRows(t, path), "SID")
			if tc.wantDomain == "" {
				if len(rows) != 0 {
					t.Errorf("origin %q inserted SID under %q; nothing may be written", tc.origin, rows[0].domain)
				}
				return
			}
			if len(rows) != 1 {
				t.Fatalf("want exactly 1 SID row for origin %q, got %d: %+v", tc.origin, len(rows), rows)
			}
			if rows[0].domain != tc.wantDomain {
				t.Errorf("origin %q inserted SID under %q, want %q — the domain must come from the "+
					"declared origin, not from the cookie name", tc.origin, rows[0].domain, tc.wantDomain)
			}
		})
	}
}

// TestUnscopedIsNotInsertedBesideAScopedSibling covers the duplicate row.
//
// One response may legitimately carry both `SID=v1` (no Domain=) and
// `SID=v2; Domain=.google.com`. The scoped header matches and rewrites the
// existing .google.com row; the unscoped one matches nothing left and falls into
// the insertion loop, which appends a SECOND row. CookieJar.Load keeps the LAST
// row it reads for a name, so the unscoped value defeats the scoped value the
// server was more specific about — a silent downgrade with no error anywhere.
//
// The fix is in the insertion loop and is deliberately narrow: an unscoped key
// may not be inserted when a SCOPED key of the same name is in the same batch.
// It cannot live in admitSetCookie, which is per-header and pure and cannot see
// siblings. And it must not be the wider "once any key of this name matched,
// treat them all as handled" — the second subtest is the case that rule breaks.
func TestUnscopedIsNotInsertedBesideAScopedSibling(t *testing.T) {
	t.Run("the unscoped twin is dropped", func(t *testing.T) {
		initial := "# Netscape HTTP Cookie File\n" +
			".google.com\tTRUE\t/\tTRUE\t2000000000\tSID\tfixture-google\n"
		rs, _, path := newSetCookieFixture(t, nopLogger{}, initial)

		rs.processYouTubeSetCookies(setCookieResponse(
			"SID=unscoped-v1; Path=/; Secure",
			"SID=scoped-v2; Domain=.google.com; Path=/",
		))

		rows := rowsNamed(readCookieRows(t, path), "SID")
		if len(rows) != 1 {
			var where []string
			for _, r := range rows {
				where = append(where, r.domain)
			}
			t.Fatalf("the file holds %d SID rows %v, want 1 — an unscoped Set-Cookie was inserted "+
				"beside the scoped one that had already claimed the row", len(rows), where)
		}
		if rows[0].value != "scoped-v2" {
			t.Errorf("SID = %q, want scoped-v2 — the more specific header must win", rows[0].value)
		}
		// The reason the row COUNT matters: Load keeps the last row for a name,
		// so a duplicate is not merely untidy, it decides the value.
		fresh := NewCookieJar()
		if err := fresh.Load(path); err != nil {
			t.Fatal(err)
		}
		if got := fresh.GetCookieFor(PlatformYouTube, "SID"); got != "scoped-v2" {
			t.Errorf("a freshly loaded jar reads SID as %q, want scoped-v2", got)
		}
	})

	t.Run("two scoped siblings still both land", func(t *testing.T) {
		// The case that kills the wider rule. Both keys are scoped, the file
		// holds only one of the two rows, and the missing one must be inserted —
		// this is the ordinary shape of a rotation that covers both domains.
		initial := "# Netscape HTTP Cookie File\n" +
			".google.com\tTRUE\t/\tTRUE\t2000000000\tSID\tfixture-google\n"
		rs, _, path := newSetCookieFixture(t, nopLogger{}, initial)

		rs.processYouTubeSetCookies(setCookieResponse(
			"SID=scoped-google; Domain=.google.com; Path=/",
			"SID=scoped-youtube; Domain=.youtube.com; Path=/",
		))

		rows := readCookieRows(t, path)
		if got := len(rowsNamed(rows, "SID")); got != 2 {
			t.Fatalf("want 2 SID rows, got %d — the sibling rule must not swallow a scoped insertion", got)
		}
		if got := rowFor(t, rows, "SID", ".google.com").value; got != "scoped-google" {
			t.Errorf(".google.com SID = %q, want scoped-google", got)
		}
		if got := rowFor(t, rows, "SID", ".youtube.com").value; got != "scoped-youtube" {
			t.Errorf(".youtube.com SID = %q, want scoped-youtube", got)
		}
	})

	t.Run("a scoped DELETE sibling does not suppress the insert", func(t *testing.T) {
		// The case the guard got wrong when it looked only at the key. A response
		// carrying `SID=; Domain=.google.com; Max-Age=0` beside an unscoped
		// `SID=fresh` is saying REPLACE — retire the cookie on google.com, set it
		// host-scoped here. Counting the deletion as a scoped claim ate the
		// replacement: the delete removed the .google.com row, the unscoped
		// insert was then suppressed as "beside a scoped one", and the fresh
		// value reached nothing at all. A deletion claims no row an insertion
		// could duplicate, because after it runs there is no row.
		//
		// Discriminates: pre-fix the file ends with ZERO SID rows and the jar
		// with no SID — a silent credential loss, not a stale value.
		initial := "# Netscape HTTP Cookie File\n" +
			".google.com\tTRUE\t/\tTRUE\t2000000000\tSID\tfixture-google\n"
		rs, _, path := newSetCookieFixture(t, nopLogger{}, initial)

		rs.processYouTubeSetCookies(setCookieResponse(
			"SID=; Domain=.google.com; Max-Age=0",
			"SID=fresh; Path=/; Secure",
		))

		rows := rowsNamed(readCookieRows(t, path), "SID")
		if len(rows) != 1 {
			var where []string
			for _, r := range rows {
				where = append(where, r.domain)
			}
			t.Fatalf("the file holds %d SID rows %v, want exactly 1 — a scoped DELETION is not a "+
				"scoped sibling, and suppressing the unscoped insert beside it loses the replacement",
				len(rows), where)
		}
		if rows[0].domain != ".youtube.com" {
			t.Errorf("the replacement SID landed on %q, want .youtube.com — the unscoped header is "+
				"host-scoped to the declared origin", rows[0].domain)
		}
		if rows[0].value != "fresh" {
			t.Errorf("SID = %q, want fresh", rows[0].value)
		}
		// The value has to reach the session, not just the file.
		fresh := NewCookieJar()
		if err := fresh.Load(path); err != nil {
			t.Fatal(err)
		}
		if got := fresh.GetCookieFor(PlatformYouTube, "SID"); got != "fresh" {
			t.Errorf("a freshly loaded jar reads SID as %q, want fresh", got)
		}
	})

	t.Run("an unscoped cookie with no sibling still lands", func(t *testing.T) {
		// The other control: the rule keys on the SIBLING, not on being unscoped.
		rs, _, path := newSetCookieFixture(t, nopLogger{}, "# Netscape HTTP Cookie File\n")

		rs.processYouTubeSetCookies(setCookieResponse("SID=unscoped-alone; Path=/; Secure"))

		rows := rowsNamed(readCookieRows(t, path), "SID")
		if len(rows) != 1 || rows[0].value != "unscoped-alone" {
			t.Errorf("want one .youtube.com SID row carrying unscoped-alone, got %+v", rows)
		}
	})
}

// TestScopedInsertionCreatesOnItsDeclaredDomain pins the owner's rule — YOUTUBE
// COOKIES SHOULD ALLOW GOOGLE COOKIES AS WELL — on the one verb that had nothing
// behind it. YouTube and Google are ONE credential platform, so a youtube.com
// reply that sends a cookie explicitly scoped Domain=.google.com is entitled to
// CREATE that row when the file holds none, and the created row has to reach the
// YouTube jar.
//
// WHAT THIS ADDS, stated honestly because the first draft of this comment
// overclaimed and was measured: the behaviour was ALREADY pinned from two
// directions, and neither of the mutants below is uniquely caught here.
//
//   - Admission is pinned by TestAdmitSetCookieOriginDecidesAdmission's "scoped
//     google from a youtube caller" row — the header is admitted.
//   - Creation is pinned by TestInsertionStaysInsideTheDeclaredPlatform's
//     "explicit google domain from a youtube caller" row — a hand-built
//     {Name: "SAPISID", Domain: ".google.com"} against an empty file lands on
//     .google.com.
//
// What no single existing test spans is the COMPOSITION for this case: a real
// Set-Cookie header entering at the production entry point, surviving admission,
// creating a row that did not exist, and that row then being READABLE as a
// YouTube-platform credential. The scoped subtests that do drive
// processYouTubeSetCookies (TestUnscopedIsNotInsertedBesideAScopedSibling) all
// SEED the .google.com row first, so they compose the path for REWRITE and say
// nothing about creation. This test is the one place the owner's rule is stated
// end to end.
//
// Three cases, the two edges giving it shape:
//
//   - .google.com from a youtube.com reply CREATES. The claim itself.
//   - .twitch.tv from a youtube.com reply creates NOTHING. Note where that is
//     decided: admitSetCookie's step 2 refuses it, because cookiePlatformOf(
//     ".twitch.tv") is not the origin's platform, so the header never becomes an
//     update and the insertion loop never sees it. updateCookieFile's insertion
//     guard is the independent SECOND net for the same class, reachable only by a
//     caller that hand-builds an update; TestInsertionStaysInsideTheDeclared-
//     Platform's "explicit twitch domain from a youtube caller" row drives that
//     one directly.
//   - an UNSCOPED SID against the same empty file lands on .youtube.com, NOT
//     .google.com. The misattribution rule, and the row that separates this test
//     from an over-confinement. Fully pinned by
//     TestUnscopedInsertionLandsOnTheDeclaredOrigin; repeated here so the
//     mutant below can be seen to spare it.
//
// MUTANTS RUN AGAINST THIS TEST, both killed here and both also killed
// elsewhere — recorded so the next reader does not have to re-derive them:
//
//   - OVER-CONFINEMENT: make the insertion loop force
//     `domain = "." + string(origin)` unconditionally rather than only when
//     key.Domain is empty. The first subtest writes .youtube.com and fails while
//     the third still passes, which is the asymmetry this test exists to show.
//     Also caught by TestInsertionStaysInsideTheDeclaredPlatform.
//   - SEAM: drop scoped keys outside the origin's own SITE inside
//     processYouTubeSetCookies — the literal implementation of the false claim
//     that a youtube.com reply "cannot invent a .google.com row". Also caught by
//     the three sibling subtests and TestProcessSetCookiesPerDomainValues.
func TestScopedInsertionCreatesOnItsDeclaredDomain(t *testing.T) {
	const empty = "# Netscape HTTP Cookie File\n"

	t.Run("a scoped google cookie from a youtube reply creates its row", func(t *testing.T) {
		rs, _, path := newSetCookieFixture(t, nopLogger{}, empty)

		rs.processYouTubeSetCookies(setCookieResponse("SID=scoped-google; Domain=.google.com; Path=/"))

		rows := rowsNamed(readCookieRows(t, path), "SID")
		if len(rows) != 1 {
			var where []string
			for _, r := range rows {
				where = append(where, r.domain)
			}
			t.Fatalf("the file holds %d SID rows %v, want exactly 1 — a scoped Set-Cookie that matches "+
				"no row must be created, once, on the domain it named", len(rows), where)
		}
		if rows[0].domain != ".google.com" {
			t.Errorf("the created SID row landed on %q, want .google.com — a youtube.com reply may "+
				"create a Google row when the server scoped the cookie there itself; re-scoping it "+
				"onto the origin's own site writes a different cookie under this one's name",
				rows[0].domain)
		}
		if rows[0].value != "scoped-google" {
			t.Errorf("SID = %q, want scoped-google", rows[0].value)
		}
		// The row has to reach the SESSION, not just the file: a row written but
		// unreadable would satisfy every file assertion above and still be inert.
		// The arm that carries it is jar.go's `isYouTubeDomain(domain) ||
		// isGoogleDomain(domain)` case, which sends BOTH domains to the one
		// youtube map — that is what "one credential platform" means downstream.
		// Admission within it is by essentialYouTubeCookies["SID"], NOT by the
		// isGoogleAuth clause beside it (checked: removing SID from isGoogleAuth
		// changes nothing here, because the essential set already covers it).
		fresh := NewCookieJar()
		if err := fresh.Load(path); err != nil {
			t.Fatal(err)
		}
		if got := fresh.GetCookieFor(PlatformYouTube, "SID"); got != "scoped-google" {
			t.Errorf("a freshly loaded jar reads SID as %q, want scoped-google — the created row must "+
				"be the one the YouTube session actually sends", got)
		}
	})

	t.Run("a scoped twitch cookie from a youtube reply creates nothing", func(t *testing.T) {
		rs, _, path := newSetCookieFixture(t, nopLogger{}, empty)

		rs.processYouTubeSetCookies(setCookieResponse("SID=scoped-twitch; Domain=.twitch.tv; Path=/"))

		if rows := rowsNamed(readCookieRows(t, path), "SID"); len(rows) != 0 {
			t.Errorf("a .twitch.tv SID from a youtube.com reply created a row under %q — one "+
				"platform's reply may not place another platform's credential, and the entitlement "+
				"above is to the DECLARED platform, not to any Domain= the header cares to name",
				rows[0].domain)
		}
	})

	t.Run("an unscoped cookie against the same file lands on the origin's site", func(t *testing.T) {
		rs, _, path := newSetCookieFixture(t, nopLogger{}, empty)

		rs.processYouTubeSetCookies(setCookieResponse("SID=unscoped; Path=/; Secure"))

		rows := rowsNamed(readCookieRows(t, path), "SID")
		if len(rows) != 1 {
			t.Fatalf("want exactly 1 SID row, got %d: %+v", len(rows), rows)
		}
		if rows[0].domain != ".youtube.com" {
			t.Errorf("an unscoped SID from a youtube.com reply landed on %q, want .youtube.com — a "+
				"Domain-less cookie belongs to the responding site, and inventing .google.com for it "+
				"mints a different cookie under a real one's name", rows[0].domain)
		}
	})
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
