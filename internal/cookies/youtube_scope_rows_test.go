package cookies

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests pin owner ruling KA-7 (2026-11-14, "Do what yt-dlp does"): a
// row www.youtube.com is sent is kept and sent under any name, at each of the
// four places that used to filter by name. Plain SIDCC is the motivating
// case: YouTube renews it on every page reply and Moombox neither kept nor
// sent it. Every value is synthetic.

// pathRow is cookieRow on a path other than "/".
func pathRow(domain, path, name, value string) string {
	return strings.Join([]string{domain, "TRUE", path, "TRUE", "0", name, value}, "\t")
}

// Place 1, Load. A row is kept under any name exactly when www.youtube.com
// is sent it on every path; the name rule still decides every other row.
//
// Mutants: keepsYouTubeRow back on namedYouTubeRow alone (the kept rows are
// dropped); wwwYouTubeRow on isYouTubeDomain || isGoogleDomain (google.com's
// own SIDCC and music.youtube.com's are kept); without its path clause (the
// /feed row is kept); without cookieNameToken (the split-header name is
// kept).
func TestLoadKeepsEveryRowWWWYouTubeIsSent(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  string
		want bool
	}{
		{"plain SIDCC on .youtube.com", cookieRow(".youtube.com", "0", "SIDCC", "v"), true},
		{"on host-only youtube.com", cookieRow("youtube.com", "0", "SIDCC", "v"), true},
		{"on www.youtube.com", cookieRow("www.youtube.com", "0", "SIDCC", "v"), true},
		{"an HttpOnly row", cookieRow("#HttpOnly_.youtube.com", "0", "SIDCC", "v"), true},
		{"a name no list has ever held", cookieRow(".youtube.com", "0", "__Secure-YEC", "v"), true},
		{"google.com's own SIDCC", cookieRow(".google.com", "0", "SIDCC", "v"), false},
		{"a host www is not under", cookieRow(".music.youtube.com", "0", "SIDCC", "v"), false},
		{"a narrower path", pathRow(".youtube.com", "/feed", "SIDCC", "v"), false},
		{"a name that would split the header", cookieRow(".youtube.com", "0", "a;b", "v"), false},
		{"a look-alike domain", cookieRow(".notyoutube.com", "0", "SIDCC", "v"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jar := loadRows(t, []string{tc.row})
			name := strings.Split(tc.row, "\t")[5]
			_, got := lookupEntry(jar, PlatformYouTube, name)
			if got != tc.want {
				t.Errorf("kept = %v, want %v", got, tc.want)
			}
			if _, inTwitch := lookupEntry(jar, PlatformTwitch, name); inTwitch {
				t.Errorf("the row is in the Twitch jar")
			}
		})
	}
}

// A jar that holds only rows kept for their scope is not signed in, was
// never configured, and has no session or identity: the auth predicates read
// their own names and nothing else (owner ruling KA-7, point 4).
//
// Mutant: any of the four predicates widened to "the jar holds a YouTube
// row".
func TestScopeKeptRowsAreNotAuthCookies(t *testing.T) {
	jar := loadRows(t, []string{
		cookieRow(".youtube.com", "0", "SIDCC", "v"),
		cookieRow(".youtube.com", "0", "__Secure-YEC", "v"),
	})
	if jar.GetCookieHeader() == "" {
		t.Fatal("fixture: the jar holds nothing")
	}
	if jar.HasYouTubeAuthCookies() || jar.HasAnyYouTubeAuthCookie() {
		t.Errorf("a jar with no auth cookie reads as signed in or configured")
	}
	if key := jar.youTubeSessionKey(); key != "" {
		t.Errorf("session key is set for a jar with no SAPISID")
	}
	if id := jar.YouTubeIdentity(); id != "" {
		t.Errorf("identity is set for a jar with no session")
	}
	if n := jar.ExpiredAuthCookiesFor(PlatformYouTube, time.Now().Unix()); n != 0 {
		t.Errorf("ExpiredAuthCookiesFor = %d, want 0", n)
	}
}

// namedRows is a signed-in set the jar keeps by name, with one row expired.
func namedRows() []string {
	return []string{
		cookieRow(".youtube.com", "0", "SAPISID", "sapisid"),
		cookieRow(".youtube.com", "0", "LOGIN_INFO", "login"),
		cookieRow(".youtube.com", "1000", "SID", "sid-expired"),
		cookieRow(".google.com", "0", "__Secure-3PSID", "google-3psid"),
		cookieRow(".youtube.com", "0", "__Secure-1PSIDCC", "1psidcc"),
		cookieRow(".youtube.com", "0", "VISITOR_INFO1_LIVE", "visitor"),
	}
}

// Place 2, the Cookie header. The rows kept for their scope are sent, after
// the rows kept by name: the named block is byte for byte the header the
// same jar sent without them, so no auth cookie moves and none is cut,
// whatever the other rows are called and however many there are.
//
// Mutants: youTubeHeaderLocked sorting all names together ("AAA" leads the
// header and the prefix check fails); dropping the scoped block (SIDCC is not
// sent); sending an expired scoped row (the expired one is found); dropping
// expired named rows too (the expired SID leaves the prefix).
func TestScopeKeptRowsFollowTheNamedRowsInTheHeader(t *testing.T) {
	before := loadRows(t, namedRows()).GetCookieHeader()
	if !strings.Contains(before, "SID=sid-expired") {
		t.Fatal("fixture: an expired auth cookie is no longer sent")
	}

	future := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	rows := append(namedRows(),
		cookieRow(".youtube.com", future, "SIDCC", "plain-sidcc"),
		cookieRow(".youtube.com", "0", "AAA", "sorts-first"),
		cookieRow(".youtube.com", "0", "zzz", "sorts-last"),
		cookieRow(".youtube.com", "1000", "STALE", "expired-scope-row"),
	)
	for i := range 40 {
		rows = append(rows, cookieRow(".youtube.com", "0", "ST-"+strconv.Itoa(i), strings.Repeat("x", 200)))
	}
	jar := loadRows(t, rows)

	got := jar.GetCookieHeader()
	if !strings.HasPrefix(got, before+"; ") {
		t.Fatalf("the header does not begin with the named rows exactly as they were sent before")
	}
	tail := strings.TrimPrefix(got, before+"; ")
	if !strings.HasPrefix(tail, "AAA=sorts-first; SIDCC=plain-sidcc; ST-0=") || !strings.HasSuffix(tail, "; zzz=sorts-last") {
		t.Errorf("the scope-kept rows are not all sent, sorted by name, after the named rows")
	}
	if n := len(headerPairs(got)); n != len(headerPairs(before))+43 {
		t.Errorf("header holds %d pairs, want every named row and the 43 live scope-kept rows", n)
	}
	if strings.Contains(got, "STALE=") {
		t.Errorf("an expired row kept only for its scope was sent")
	}

	// The page header orders and filters them the same way.
	page := jar.YouTubePageCookieHeader()
	named, scoped, ok := strings.Cut(page, "; AAA=sorts-first; ")
	if !ok || !strings.HasPrefix(scoped, "SIDCC=plain-sidcc; ") || !strings.Contains(named, "SAPISID=sapisid") {
		t.Errorf("the page header does not carry the scope-kept rows after the named rows")
	}
	if strings.Contains(page, "STALE=") {
		t.Errorf("the page header sent an expired row kept only for its scope")
	}

	// A media host is sent none of them (owner ruling KA-6).
	if media := jar.GetYouTubeMediaCookieHeader(); media != before {
		t.Errorf("the media header is not the named rows alone")
	}
}

// Place 3, the browser export. A row www.youtube.com is sent is written to
// cookies.txt under any name; google.com's row of the same name is not.
//
// Mutant: deduplicateAndFormat back on isEssentialCookie alone.
func TestBrowserExportKeepsPlainSIDCC(t *testing.T) {
	lines := deduplicateAndFormat([]extractedCookie{
		{domain: ".google.com", path: "/", name: "SIDCC", value: "google-sidcc"},
		{domain: ".youtube.com", path: "/", name: "SIDCC", value: "youtube-sidcc", expiry: 2000000000},
		{domain: ".youtube.com", path: "/feed", name: "PATHED", value: "v"},
	})
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "\tSIDCC\tyoutube-sidcc") || !strings.HasPrefix(lines[0], ".youtube.com\t") {
		t.Errorf("export kept %d row(s), want youtube.com's SIDCC alone", len(lines))
	}
}

// Place 3, the cookie-file import. The import has always written every row
// of the pasted file; the jar loaded from the result now holds and sends the
// youtube.com rows it used to drop.
//
// Mutant: Load back on the name rule.
func TestImportedPlainSIDCCIsLoadedAndSent(t *testing.T) {
	merged, err := prepareCookieImport("", strings.Join([]string{
		cookieRow(".youtube.com", "0", "SAPISID", "sapisid"),
		cookieRow(".youtube.com", "0", "SIDCC", "plain-sidcc"),
	}, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	jar.loadFrom([]byte(merged), "")
	if !strings.HasSuffix(jar.GetCookieHeader(), "; SIDCC=plain-sidcc") {
		t.Errorf("the imported SIDCC is not sent")
	}
}

// Place 4, a reply's Set-Cookie. A signed-in youtube.com reply that sets
// plain SIDCC renews the row the file holds, creates it when the file holds
// none, and is sent on the next request. google.com's own SIDCC row, which
// the file can hold after an import, is not written over: the two are
// different cookies.
//
// Mutants: renewableCookie without its wwwYouTubeRow arm (nothing is kept);
// crossesConsentSides back on SOCS alone (the google.com row takes YouTube's
// value when the file holds no youtube.com row).
func TestPlainSIDCCIsRenewedFromAReply(t *testing.T) {
	const setSIDCC = "SIDCC=sidcc-new; Domain=.youtube.com; Path=/; Max-Age=31536000"
	googleRow := ".google.com\tTRUE\t/\tFALSE\t2000000000\tSIDCC\tgoogle-own\n"

	for _, tc := range []struct {
		name  string
		extra string
		reply string
	}{
		{"the file holds the row", googleRow + ".youtube.com\tTRUE\t/\tFALSE\t2000000000\tSIDCC\tplain-old\n", setSIDCC},
		{"the file holds no youtube.com row", googleRow, setSIDCC},
		{"an unscoped header", googleRow, "SIDCC=sidcc-new; Path=/; Max-Age=31536000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cookies.txt")
			jar := loadRowsInto(t, path, strings.Split(strings.TrimSuffix(strings.TrimPrefix(renewalSeed+tc.extra, "# Netscape HTTP Cookie File\n"), "\n"), "\n"))
			t.Cleanup(jar.FlushRenewals)

			jar.OfferYouTubeReply(replyTo(t, "https://www.youtube.com/feed/x", jar.GetCookieHeader(), 200, tc.reply))

			if !strings.HasSuffix(jar.GetCookieHeader(), "; SIDCC=sidcc-new") {
				t.Errorf("the next request does not send the renewed SIDCC")
			}
			jar.FlushRenewals()
			file := fileText(t, path)
			if n := strings.Count(file, "youtube.com\tTRUE\t/\tFALSE\t"); n != 1 || !strings.Contains(file, "\tSIDCC\tsidcc-new\n") || strings.Contains(file, "plain-old") {
				t.Errorf("cookies.txt does not hold exactly one youtube.com SIDCC row with the renewed value")
			}
			if !strings.Contains(file, googleRow) {
				t.Errorf("google.com's own SIDCC row was changed")
			}
		})
	}
}
