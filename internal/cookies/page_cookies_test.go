package cookies

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Rows' expiry columns: one long past (2001) and one far ahead (2100), so a
// row's expired-or-not never depends on when the test runs.
const (
	expiredAt   = "1000000000"
	unexpiredAt = "4102444800"
	// googleSOCS is a consent google.com sets on its own domain.
	googleSOCS = "CAISHAgCEhJnd3NfMjAyNDA"
)

// TestYouTubePageCookieHeader pins the consent and language cookies a YouTube
// HTML page is fetched with: yt-dlp's _initialize_consent (SOCS=CAI for a
// session that is not signed in and holds no consented SOCS, _base.py:675-681)
// and _initialize_pref (hl=en and tz=UTC merged into PREF, _base.py:683-692),
// over the jar's own YouTube cookies.
//
// The jar's SOCS and PREF are read as yt-dlp's www.youtube.com-scoped jar
// read sees them (youtubeSees): a google.com copy, one on another YouTube
// host, or an expired one, is not there — CAI and a fresh PREF in its place,
// and no SOCS for a signed-in jar.
//
// Mutants: SOCS never added (every "CAI" row fails); SOCS added for a
// signed-in jar too (the two signed-in rows); a CAA SOCS kept (its row); a
// consented SOCS replaced (its row); an empty SOCS kept (its row);
// HasAnyYouTubeAuthCookie as the signed-in test (the half-signed-in row);
// PREF forwarded unchanged (every PREF row); PREF rebuilt from hl and tz
// alone (the f6 rows); the domain read dropped (the google.com rows and the
// other-host row); isYouTubeDomain in its place (the other-host row); the
// expiry read dropped (the expired rows); an unexpired expiry read as past
// (the not-expired row).
func TestYouTubePageCookieHeader(t *testing.T) {
	sapisid := cookieRow(".youtube.com", "0", "SAPISID", "sap")
	loginInfo := cookieRow(".youtube.com", "0", "LOGIN_INFO", "li")
	for _, tc := range []struct {
		name string
		rows []string
		want string
	}{
		{name: "an empty jar: CAI and an English PREF",
			want: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "no auth, no SOCS: CAI added beside the jar's cookies",
			rows: []string{cookieRow(".youtube.com", "0", "VISITOR_INFO1_LIVE", "vis"), cookieRow(".youtube.com", "0", "YSC", "ysc")},
			want: "PREF=hl=en&tz=UTC; SOCS=CAI; VISITOR_INFO1_LIVE=vis; YSC=ysc"},
		{name: "a CAA SOCS (no consent) is replaced",
			rows: []string{cookieRow(".youtube.com", "0", "SOCS", "CAAaBgiAgICBBg")},
			want: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "a consented SOCS is kept",
			rows: []string{cookieRow(".youtube.com", "0", "SOCS", "CAESEwgDEgk2NjI4NDQ2OTIaAmVuIAEaBgiA_LyuBg")},
			want: "PREF=hl=en&tz=UTC; SOCS=CAESEwgDEgk2NjI4NDQ2OTIaAmVuIAEaBgiA_LyuBg"},
		{name: "an empty SOCS counts as none",
			rows: []string{cookieRow(".youtube.com", "0", "SOCS", "")},
			want: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "a signed-in jar gets no SOCS",
			rows: []string{sapisid, loginInfo},
			want: "LOGIN_INFO=li; PREF=hl=en&tz=UTC; SAPISID=sap"},
		{name: "a signed-in jar's own SOCS goes out as it is, CAA included",
			rows: []string{sapisid, loginInfo, cookieRow(".youtube.com", "0", "SOCS", "CAAaBgiAgICBBg")},
			want: "LOGIN_INFO=li; PREF=hl=en&tz=UTC; SAPISID=sap; SOCS=CAAaBgiAgICBBg"},
		{name: "half signed in (SAPISID, no LOGIN_INFO) is not signed in",
			rows: []string{sapisid},
			want: "PREF=hl=en&tz=UTC; SAPISID=sap; SOCS=CAI"},
		{name: "a localized PREF keeps its other keys and their order",
			rows: []string{sapisid, loginInfo, cookieRow(".youtube.com", "0", "PREF", "f6=40000000&tz=Asia.Tokyo&f7=100&hl=ja")},
			want: "LOGIN_INFO=li; PREF=f6=40000000&tz=UTC&f7=100&hl=en; SAPISID=sap"},
		{name: "a PREF with neither hl nor tz has both appended",
			rows: []string{cookieRow(".youtube.com", "0", "PREF", "f6=40000000&f5=30000")},
			want: "PREF=f6=40000000&f5=30000&hl=en&tz=UTC; SOCS=CAI"},
		{name: "an unparsable PREF is replaced by hl and tz alone",
			rows: []string{cookieRow(".youtube.com", "0", "PREF", "f6=4%zz&hl=ja")},
			want: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "a google.com SOCS is not YouTube's: CAI in its place",
			rows: []string{cookieRow(".google.com", "0", "SOCS", googleSOCS)},
			want: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "a google.com PREF is not YouTube's: a fresh one",
			rows: []string{cookieRow(".google.com", "0", "PREF", "hl=de&f6=8")},
			want: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "a signed-in jar sends no google.com SOCS",
			rows: []string{sapisid, loginInfo, cookieRow(".google.com", "0", "SOCS", googleSOCS)},
			want: "LOGIN_INFO=li; PREF=hl=en&tz=UTC; SAPISID=sap"},
		{name: "a youtube.com SOCS beside a google.com one is the one read",
			rows: []string{cookieRow(".google.com", "0", "SOCS", googleSOCS), cookieRow(".youtube.com", "0", "SOCS", "CAESxyz")},
			want: "PREF=hl=en&tz=UTC; SOCS=CAESxyz"},
		{name: "a SOCS scoped to another YouTube host is not www's",
			rows: []string{cookieRow("music.youtube.com", "0", "SOCS", "CAESxyz")},
			want: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "a SOCS on www.youtube.com itself is read",
			rows: []string{cookieRow("www.youtube.com", "0", "SOCS", "CAESxyz")},
			want: "PREF=hl=en&tz=UTC; SOCS=CAESxyz"},
		{name: "an expired consented SOCS counts as none",
			rows: []string{cookieRow(".youtube.com", expiredAt, "SOCS", "CAESEwgDEgk2NjI4NDQ2OTIaAmVuIAEaBgiA_LyuBg")},
			want: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "a signed-in jar sends no expired SOCS",
			rows: []string{sapisid, loginInfo, cookieRow(".youtube.com", expiredAt, "SOCS", "CAESxyz")},
			want: "LOGIN_INFO=li; PREF=hl=en&tz=UTC; SAPISID=sap"},
		{name: "an expired PREF counts as none",
			rows: []string{cookieRow(".youtube.com", expiredAt, "PREF", "f6=40000000&hl=ja")},
			want: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "a SOCS that has not expired is kept",
			rows: []string{cookieRow(".youtube.com", unexpiredAt, "SOCS", "CAESxyz")},
			want: "PREF=hl=en&tz=UTC; SOCS=CAESxyz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jar := loadRows(t, tc.rows)
			if got := jar.YouTubePageCookieHeader(); got != tc.want {
				t.Errorf("YouTubePageCookieHeader() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestYouTubeConsentCookieHeader: the public cascade's watch page goes out
// without the jar's session, so it gets the consent and language cookies
// alone — and, carrying no auth cookie, is signed out whatever the jar
// holds, so SOCS is always sent.
//
// Mutants: an auth cookie or another jar cookie let through (the signed-in
// row); the jar's signed-in verdict deciding SOCS (the same row); the jar's
// consented SOCS or PREF ignored (their rows); the domain or the expiry read
// dropped (the google.com row, the expired row).
func TestYouTubeConsentCookieHeader(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []string
		want string
	}{
		{name: "an empty jar", want: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "a signed-in jar: no credential, and CAI",
			rows: []string{
				cookieRow(".youtube.com", "0", "SAPISID", "sap"), cookieRow(".youtube.com", "0", "LOGIN_INFO", "li"),
				cookieRow(".youtube.com", "0", "VISITOR_INFO1_LIVE", "vis"),
			},
			want: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "the jar's consented SOCS and its PREF",
			rows: []string{cookieRow(".youtube.com", "0", "SOCS", "CAESxyz"), cookieRow(".youtube.com", "0", "PREF", "f6=40000000&hl=de")},
			want: "PREF=f6=40000000&hl=en&tz=UTC; SOCS=CAESxyz"},
		{name: "google.com's SOCS and PREF are not YouTube's",
			rows: []string{cookieRow(".google.com", "0", "SOCS", googleSOCS), cookieRow(".google.com", "0", "PREF", "hl=de&f6=8")},
			want: "PREF=hl=en&tz=UTC; SOCS=CAI"},
		{name: "an expired SOCS and PREF count as none",
			rows: []string{cookieRow(".youtube.com", expiredAt, "SOCS", "CAESxyz"), cookieRow(".youtube.com", expiredAt, "PREF", "f6=40000000&hl=de")},
			want: "PREF=hl=en&tz=UTC; SOCS=CAI"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jar := loadRows(t, tc.rows)
			if got := jar.YouTubeConsentCookieHeader(); got != tc.want {
				t.Errorf("YouTubeConsentCookieHeader() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEnglishPrefMatchesYtDlp holds englishPref to yt-dlp's
// urlencode(dict(parse_qsl(value)) | {hl: en, tz: UTC}), each want computed
// by Python 3 from that expression: blank and '='-less fields dropped, a
// repeated key's first place and last value, '+' and %20 as spaces, and
// escapes re-encoded as quote_plus encodes them.
//
// Mutants: url.Values (sorted keys, so the order rows fail); a blank value
// kept (the blank-field row); a repeated key appended twice (its row); the
// value written back unescaped (the escape rows).
func TestEnglishPrefMatchesYtDlp(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "hl=en&tz=UTC"},
		{"f6=40000000&tz=Asia.Tokyo&f7=100&hl=ja", "f6=40000000&tz=UTC&f7=100&hl=en"},
		{"tz=Asia%2FTokyo&f5=30000", "tz=UTC&f5=30000&hl=en"},
		{"a=1&b=2&a=3", "a=3&b=2&hl=en&tz=UTC"},
		{"flag&x=&y=1&&", "y=1&hl=en&tz=UTC"},
		{"v=a%20b+c", "v=a+b+c&hl=en&tz=UTC"},
		{"=b&k=%E3%81%82", "=b&k=%E3%81%82&hl=en&tz=UTC"},
		{"f6=4%zz&hl=ja", "hl=en&tz=UTC"},
	} {
		if got := englishPref(tc.in); got != tc.want {
			t.Errorf("englishPref(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPageCookiesAreHeaderOnly: building the page header writes nothing back
// — the jar's own header and the cookie file are exactly what they were, so
// a refresh, an export or the Innertube calls see the user's cookies (owner
// ruling D1). And a SOCS in cookies.txt survives Load, now that it is in
// essentialYouTubeCookies, to be the one the page carries.
//
// Mutants: SOCS dropped from essentialYouTubeCookies (the loaded SOCS is
// gone, so the page sends CAI); the helper storing its SOCS or PREF in the
// jar (GetCookieHeader changes).
func TestPageCookiesAreHeaderOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	jar := loadRowsInto(t, path, []string{
		cookieRow(".youtube.com", "0", "SOCS", "CAESxyz"),
		cookieRow(".youtube.com", "0", "PREF", "f6=40000000&hl=ja"),
	})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const jarHeader = "PREF=f6=40000000&hl=ja; SOCS=CAESxyz"
	if got := jar.GetCookieHeader(); got != jarHeader {
		t.Fatalf("GetCookieHeader() = %q, want %q — SOCS must survive Load", got, jarHeader)
	}
	if got, want := jar.YouTubePageCookieHeader(), "PREF=f6=40000000&hl=en&tz=UTC; SOCS=CAESxyz"; got != want {
		t.Errorf("YouTubePageCookieHeader() = %q, want %q", got, want)
	}
	_ = jar.YouTubeConsentCookieHeader()
	if got := jar.GetCookieHeader(); got != jarHeader {
		t.Errorf("GetCookieHeader() after the page headers = %q, want the jar's own %q", got, jarHeader)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("cookies.txt changed:\nbefore %q\nafter  %q", before, after)
	}
	if strings.Contains(string(after), "CAI") || strings.Contains(string(after), "tz=UTC") {
		t.Errorf("cookies.txt carries the page cookies: %q", after)
	}
}

// TestPageCookieHeadersAreSanitisedAsTheJarIs: the page headers render the
// jar's values as GetCookieHeaderFor does, CR, LF, ';' and ',' stripped, so a
// hand-edited cookies.txt cannot end a pair early on one renderer and not
// the other. Pins the jar's own renderer too, which no test held before.
//
// Mutants: sanitizeCookieValue dropped from joinCookiePairs (the page
// rows); from GetCookieHeaderFor (the jar row); sanitizeCookieValue
// returning its value unchanged (every row).
func TestPageCookieHeadersAreSanitisedAsTheJarIs(t *testing.T) {
	jar := loadRows(t, []string{
		cookieRow(".youtube.com", "0", "VISITOR_INFO1_LIVE", "a;b,c"),
		cookieRow(".youtube.com", "0", "SOCS", "CAE;SID=x,y"),
	})
	for _, tc := range []struct {
		name, got, want string
	}{
		{"GetCookieHeader", jar.GetCookieHeader(), "SOCS=CAESID=xy; VISITOR_INFO1_LIVE=abc"},
		{"YouTubePageCookieHeader", jar.YouTubePageCookieHeader(), "PREF=hl=en&tz=UTC; SOCS=CAESID=xy; VISITOR_INFO1_LIVE=abc"},
		{"YouTubeConsentCookieHeader", jar.YouTubeConsentCookieHeader(), "PREF=hl=en&tz=UTC; SOCS=CAESID=xy"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s() = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}
