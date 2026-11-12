package cookies

import "testing"

// TestSOCSIsAdmittedFromYouTubeDomainsAlone pins what the jar holds for SOCS,
// and so what GetCookieHeader — the header every Innertube call sends —
// carries to www.youtube.com: a SOCS www.youtube.com is sent, never one
// scoped to google.com (admitsEssentialYouTubeName). yt-dlp's YouTube
// requests carry _get_cookies('https://www.youtube.com'), http.cookiejar's
// domain-matched read for that host (_base.py:777-779 at b77c9304e), which a
// google.com consent does not domain-match. Load used to admit SOCS on either
// domain, as it admits every other essential name, so an EU browser's
// google.com consent rode the Innertube calls as YouTube's.
//
// The google.com PREF row is the control: every other essential name is
// still admitted on a Google domain, so the rule is SOCS's, not a Google
// domain's. The two-domain rows are loaded in both orders, so the youtube.com
// copy wins on admission, not on compareCookieDomains' preference alone.
//
// Mutants: admitsEssentialYouTubeName without its youtube-only arm (the
// google.com rows carry SOCS); sentToWWWYouTube as isYouTubeDomain (the
// music.youtube.com row carries it); Load back on essentialYouTubeCookies
// alone (the google.com rows again).
func TestSOCSIsAdmittedFromYouTubeDomainsAlone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rows       []string
		wantSOCS   string
		wantHeader string
	}{
		{name: "a google.com SOCS is not admitted, and not sent",
			rows:       []string{cookieRow(".google.com", unexpiredAt, "SOCS", googleSOCS)},
			wantHeader: ""},
		{name: "a youtube.com SOCS is admitted and sent",
			rows:       []string{cookieRow(".youtube.com", unexpiredAt, "SOCS", "CAESxyz")},
			wantSOCS:   "CAESxyz",
			wantHeader: "SOCS=CAESxyz"},
		{name: "a SOCS on www.youtube.com itself is admitted",
			rows:       []string{cookieRow("www.youtube.com", unexpiredAt, "SOCS", "CAESxyz")},
			wantSOCS:   "CAESxyz",
			wantHeader: "SOCS=CAESxyz"},
		{name: "a SOCS on a YouTube host www is not under is not admitted",
			rows:       []string{cookieRow("music.youtube.com", unexpiredAt, "SOCS", "CAESxyz")},
			wantHeader: ""},
		{name: "a google.com SOCS after a youtube.com one leaves YouTube's",
			rows: []string{cookieRow(".youtube.com", unexpiredAt, "SOCS", "CAESxyz"),
				cookieRow(".google.com", unexpiredAt, "SOCS", googleSOCS)},
			wantSOCS:   "CAESxyz",
			wantHeader: "SOCS=CAESxyz"},
		{name: "a google.com SOCS before a youtube.com one leaves YouTube's",
			rows: []string{cookieRow(".google.com", unexpiredAt, "SOCS", googleSOCS),
				cookieRow(".youtube.com", unexpiredAt, "SOCS", "CAESxyz")},
			wantSOCS:   "CAESxyz",
			wantHeader: "SOCS=CAESxyz"},
		{name: "a google.com PREF beside it is still admitted (the control)",
			rows: []string{cookieRow(".google.com", unexpiredAt, "SOCS", googleSOCS),
				cookieRow(".google.com", unexpiredAt, "PREF", "f6=8")},
			wantHeader: "PREF=f6=8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jar := loadRows(t, tc.rows)
			if got := jar.GetCookieFor(PlatformYouTube, "SOCS"); got != tc.wantSOCS {
				t.Errorf("the jar's SOCS = %q, want %q", got, tc.wantSOCS)
			}
			if got := jar.GetCookieHeader(); got != tc.wantHeader {
				t.Errorf("GetCookieHeader() = %q, want %q: the Innertube calls' header", got, tc.wantHeader)
			}
		})
	}
}

// TestAnExpiredSOCSIsNotSentOnInnertube: the jar keeps an expired SOCS (Load
// keeps every row whatever its expiry — see Load), but GetCookieHeader, the
// header every Innertube call sends, leaves it out, as the page header does
// (youtubeSees) and as yt-dlp does: YoutubeDLCookieJar.get_cookie_header
// (yt_dlp/cookies.py:1405-1409 at b77c9304e) goes through http.cookiejar's
// add_cookie_header, whose policy refuses an expired cookie
// (return_ok_expires), though load(ignore_expires=True) keeps it. Before, a
// cookies.txt exported a year ago sent the consent it held then on every
// Innertube call while the page header sent CAI in its place.
//
// The expired SAPISID beside it is the control: Load's contract still holds
// for every other name — an expired credential is still sent, and is what
// ExpiredAuthCookiesFor reports — so the rule is SOCS's alone. A session
// SOCS (expiry 0) is sent; the unexpired one is a row of
// TestSOCSIsAdmittedFromYouTubeDomainsAlone.
//
// Mutant: GetCookieHeaderFor without its expired-SOCS skip (the expired SOCS
// rides the header); the skip applied to every name (the expired SAPISID is
// left out).
func TestAnExpiredSOCSIsNotSentOnInnertube(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rows       []string
		wantSOCS   string
		wantHeader string
	}{
		{name: "an expired youtube.com SOCS is held but not sent",
			rows: []string{cookieRow(".youtube.com", expiredAt, "SOCS", "CAESold"),
				cookieRow(".youtube.com", expiredAt, "SAPISID", "sapisid-a")},
			wantSOCS:   "CAESold",
			wantHeader: "SAPISID=sapisid-a"},
		{name: "a session SOCS is sent",
			rows:       []string{cookieRow(".youtube.com", "0", "SOCS", "CAESxyz")},
			wantSOCS:   "CAESxyz",
			wantHeader: "SOCS=CAESxyz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jar := loadRows(t, tc.rows)
			if got := jar.GetCookieFor(PlatformYouTube, "SOCS"); got != tc.wantSOCS {
				t.Errorf("the jar's SOCS = %q, want %q: Load keeps it", got, tc.wantSOCS)
			}
			if got := jar.GetCookieHeader(); got != tc.wantHeader {
				t.Errorf("GetCookieHeader() = %q, want %q: the Innertube calls' header", got, tc.wantHeader)
			}
		})
	}
}

// TestASOCSRefreshStaysOnItsSideOfWWWYouTube: resolveRowUpdate's rule 3 lets
// a single value refresh cross YouTube and Google rows, one credential
// platform — but not for SOCS, a consent each site keeps for itself. A
// google.com SOCS refreshing YouTube's row would be admitted by Load as
// YouTube's consent, the leak admitsEssentialYouTubeName closes; and an
// unscoped youtube.com SOCS refreshing google.com's row would write YouTube's
// consent over Google's. Unreachable today (originYouTube is the only
// caller, and YouTube sets no google.com SOCS); pinned for the caller that
// arrives. A SOCS refresh on the same side still crosses hosts, and every
// other name still crosses the platform (the controls).
//
// Mutant: rule 3 without crossesConsentSides (the two cross-side rows are
// refreshed).
func TestASOCSRefreshStaysOnItsSideOfWWWYouTube(t *testing.T) {
	for _, tc := range []struct {
		name      string
		key       cookieUpdateKey
		rowDomain string
		origin    cookieOrigin
		want      bool
	}{
		{"google.com's SOCS does not refresh YouTube's row",
			cookieUpdateKey{Name: "SOCS", Domain: ".google.com"}, ".youtube.com", originGoogle, false},
		{"an unscoped youtube.com SOCS does not refresh google.com's row",
			cookieUpdateKey{Name: "SOCS"}, ".google.com", originYouTube, false},
		{"a youtube.com SOCS refreshes a www.youtube.com row",
			cookieUpdateKey{Name: "SOCS", Domain: ".youtube.com"}, "www.youtube.com", originYouTube, true},
		{"SID still refreshes across the platform",
			cookieUpdateKey{Name: "SID", Domain: ".google.com"}, ".youtube.com", originGoogle, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updates := map[cookieUpdateKey]cookieUpdate{tc.key: {Value: "fresh"}}
			_, _, ok := resolveRowUpdate(updates, []cookieUpdateKey{tc.key}, tc.rowDomain, tc.origin)
			if ok != tc.want {
				t.Errorf("resolveRowUpdate(%+v onto a %s row) applies: %v, want %v", tc.key, tc.rowDomain, ok, tc.want)
			}
		})
	}
}

// TestADeletedGoogleSOCSIsNotAnEssentialCookie: updateCookieFile logs a
// server's deletion of an essential YouTube cookie at Info ("the signed-in
// session may have ended") and any other deletion at Debug, judged by the
// admission rule Load applies (admitsEssentialYouTubeName). A .google.com
// SOCS is not one Load keeps, so its deletion is a Debug line; a
// .youtube.com SOCS's is the Info line (the control). Log severity only:
// both rows are deleted.
//
// Mutant: rowHasEssential back on the name-and-platform form (the
// google.com SOCS's deletion logs at Info).
func TestADeletedGoogleSOCSIsNotAnEssentialCookie(t *testing.T) {
	for _, tc := range []struct {
		name, domain string
		origin       cookieOrigin
		wantInfo     int
	}{
		{"a google.com SOCS", ".google.com", originGoogle, 0},
		{"a youtube.com SOCS", ".youtube.com", originYouTube, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &levelLogger{}
			rs, _, path := newSetCookieFixture(t, log, "# Netscape HTTP Cookie File\n"+cookieRow(tc.domain, unexpiredAt, "SOCS", "CAESxyz")+"\n")
			updates := map[cookieUpdateKey]cookieUpdate{{Name: "SOCS", Domain: tc.domain}: {Delete: true}}
			if err := rs.updateCookieFile(updates, tc.origin, ""); err != nil {
				t.Fatalf("updateCookieFile: %v", err)
			}
			if got := rowsNamed(readCookieRows(t, path), "SOCS"); len(got) != 0 {
				t.Fatalf("premise broken: the row was not deleted: %q", got[0].raw)
			}
			if got := log.infoCount("deleted an essential cookie"); got != tc.wantInfo {
				t.Errorf("Info lines naming an essential deletion = %d, want %d (info=%v)", got, tc.wantInfo, log.info)
			}
		})
	}
}
