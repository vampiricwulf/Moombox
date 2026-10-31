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
