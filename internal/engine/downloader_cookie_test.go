package engine

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
)

// These tests pin ONE claim: the Cookie header is read AT USE TIME, per
// outbound request, not captured when the downloader is constructed.
//
// A download is the longest-lived HTTP consumer in the program. A multi-hour
// archive runs while the in-process refresh rotates Google's cookies roughly
// every 30 minutes, so a snapshotted header was still being presented to the
// last segment of the recording.
//
// The header itself is never rewritten: a host that gets it gets the jar's own
// string. Which hosts get it is pinned at the end of this file and, on the
// wire, in downloader_media_cookie_test.go: every host but the media hosts
// (googlevideo.com and its subdomains). The servers here are 127.0.0.1, which
// is not one, so these tests see the header on every request.
//
// Every cookie value below is synthetic.

// cookieRecorder is a local HTTP server standing in for a media CDN. It records
// the Cookie header of every request it serves — "" meaning the header was
// absent entirely, which is distinct from an empty one.
type cookieRecorder struct {
	server *httptest.Server
	mu     sync.Mutex
	seen   []string
	absent []bool
}

func startCookieRecorder(t *testing.T) *cookieRecorder {
	t.Helper()
	rec := &cookieRecorder{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		values := r.Header.Values("Cookie")
		rec.absent = append(rec.absent, len(values) == 0)
		rec.seen = append(rec.seen, r.Header.Get("Cookie"))
		rec.mu.Unlock()
		// Enough for probeHeadAt to parse a head and for fetchSegment to
		// accept the body; probeFileSize simply reads 0 from a 200.
		w.Header().Set("X-Head-Seqnum", "5000")
		_, _ = w.Write([]byte("segment-bytes"))
	}))
	t.Cleanup(rec.server.Close)
	return rec
}

func (rec *cookieRecorder) cookies() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.seen...)
}

func (rec *cookieRecorder) headerAbsent() []bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]bool(nil), rec.absent...)
}

// headerSequence yields values in order, repeating the last once exhausted — a
// stand-in for a jar that rotates underneath a running download.
func headerSequence(values ...string) func() string {
	var mu sync.Mutex
	i := 0
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		v := values[min(i, len(values)-1)]
		i++
		return v
	}
}

// TestSegmentFetchReadsCookieHeaderPerRequest is the claim. Two segment fetches
// with a getter whose value changes between them must produce two DIFFERENT
// Cookie headers at the server.
//
// The assertion is on the exact sequence of headers, not on the presence of a
// header: "a Cookie header arrived" is satisfied by a snapshot too, so a test
// that inspects only the first request — or only that some cookie was sent —
// passes in the broken state this exists to catch.
func TestSegmentFetchReadsCookieHeaderPerRequest(t *testing.T) {
	rec := startCookieRecorder(t)
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:      rec.server.URL + "/v?x=1",
		OutputFile:   "x",
		CookieHeader: headerSequence("SID=first-rotation", "SID=second-rotation"),
	})

	for seq := range 2 {
		if _, _, err := d.fetchSegment(t.Context(), d.buildSegmentURL(seq)); err != nil {
			t.Fatalf("fetchSegment(%d): %v", seq, err)
		}
	}

	got := rec.cookies()
	want := []string{"SID=first-rotation", "SID=second-rotation"}
	if !slices.Equal(got, want) {
		t.Errorf("Cookie headers = %q, want %q — the second segment presented the credential the "+
			"download started with rather than the one the jar holds now", got, want)
	}
}

// TestCookieHeaderReachesEveryFetchPath drives three of setCommonHeaders' six
// callers with the same rotating getter. Each is a separate outbound request,
// so each must carry the value current at ITS moment: a shared snapshot shows
// up here as three identical headers.
//
// fetchSegment and probeHeadAt use uaWeb, probeFileSize uaAndroid — the two UA
// variants every caller is one of.
func TestCookieHeaderReachesEveryFetchPath(t *testing.T) {
	rec := startCookieRecorder(t)
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:      rec.server.URL + "/v?x=1",
		OutputFile:   "x",
		CookieHeader: headerSequence("SID=at-segment", "SID=at-head-probe", "SID=at-size-probe"),
	})

	if _, _, err := d.fetchSegment(t.Context(), d.buildSegmentURL(1)); err != nil {
		t.Fatalf("fetchSegment: %v", err)
	}
	if _, err := d.probeHeadAt(t.Context(), 2); err != nil {
		t.Fatalf("probeHeadAt: %v", err)
	}
	d.probeFileSize(t.Context())

	got := rec.cookies()
	want := []string{"SID=at-segment", "SID=at-head-probe", "SID=at-size-probe"}
	if !slices.Equal(got, want) {
		t.Errorf("Cookie headers = %q, want %q", got, want)
	}
}

// TestNilCookieGetterSendsNoCookieHeader: nil must behave exactly as the empty
// string did — no header at all, and no panic on the hot path.
func TestNilCookieGetterSendsNoCookieHeader(t *testing.T) {
	rec := startCookieRecorder(t)
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    rec.server.URL + "/v?x=1",
		OutputFile: "x",
		// CookieHeader intentionally left nil.
	})

	if _, _, err := d.fetchSegment(t.Context(), d.buildSegmentURL(1)); err != nil {
		t.Fatalf("fetchSegment: %v", err)
	}

	if absent := rec.headerAbsent(); !slices.Equal(absent, []bool{true}) {
		t.Errorf("Cookie header presence = %v, want exactly one request with no header at all "+
			"(got %q)", absent, rec.cookies())
	}
}

// TestEmptyCookieGetterSendsNoCookieHeader preserves the `!= ""` guard: an
// unauthenticated jar sends no Cookie header rather than an empty one.
func TestEmptyCookieGetterSendsNoCookieHeader(t *testing.T) {
	rec := startCookieRecorder(t)
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:      rec.server.URL + "/v?x=1",
		OutputFile:   "x",
		CookieHeader: func() string { return "" },
	})

	if _, _, err := d.fetchSegment(t.Context(), d.buildSegmentURL(1)); err != nil {
		t.Fatalf("fetchSegment: %v", err)
	}

	if absent := rec.headerAbsent(); !slices.Equal(absent, []bool{true}) {
		t.Errorf("Cookie header presence = %v, want exactly one request with no header at all "+
			"(got %q)", absent, rec.cookies())
	}
}

// unrotatedJarHeader is what internal/cookies produces for an authenticated
// YouTube jar: every pair the jar holds, sorted by name, joined with "; ".
// Synthetic values.
const unrotatedJarHeader = "LOGIN_INFO=synthetic-login-info; SAPISID=synthetic-sapisid; " +
	"__Secure-1PSID=synthetic-1psid; __Secure-3PSID=synthetic-3psid"

// TestUnrotatedJarSendsByteIdenticalHeader is the no-regression half. A getter
// whose value never changes must put exactly the jar's own string on the wire,
// unaltered, on every request — the same bytes a plain string field sent.
func TestUnrotatedJarSendsByteIdenticalHeader(t *testing.T) {
	rec := startCookieRecorder(t)
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:      rec.server.URL + "/v?x=1",
		OutputFile:   "x",
		CookieHeader: func() string { return unrotatedJarHeader },
	})

	for seq := range 3 {
		if _, _, err := d.fetchSegment(t.Context(), d.buildSegmentURL(seq)); err != nil {
			t.Fatalf("fetchSegment(%d): %v", seq, err)
		}
	}

	got := rec.cookies()
	want := []string{unrotatedJarHeader, unrotatedJarHeader, unrotatedJarHeader}
	if !slices.Equal(got, want) {
		t.Errorf("Cookie headers = %q, want the jar's own string verbatim on every request (%q)",
			got, unrotatedJarHeader)
	}
}

// TestSessionCookiesAreWithheldFromMediaHosts pins which hosts the header is
// kept from: googlevideo.com and its subdomains, however the host is spelled,
// and no other. The media lives there, on a different site from youtube.com,
// and a browser and yt-dlp send those hosts no session cookies. A host that
// only looks like one still gets the jar's header verbatim, as every host did
// before the rule.
//
// This replaces TestCookieHeaderIsNotHostScoped, which pinned the opposite
// (owner ruling KA-6, 2026-11-13). What happens when a media host refuses a
// request sent without the header is in downloader_media_cookie_test.go.
//
// Mutants: isMediaHost matching by substring (the two look-alikes lose the
// header); comparing the host with its port, its case or its trailing dot
// (those rows keep it).
func TestSessionCookiesAreWithheldFromMediaHosts(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:      "https://rr5---sn-4g5ednds.googlevideo.com/videoplayback?expire=1&itag=248",
		OutputFile:   "x",
		CookieHeader: func() string { return unrotatedJarHeader },
	})

	for _, tc := range []struct {
		rawURL   string
		withheld bool
	}{
		{"https://rr5---sn-4g5ednds.googlevideo.com/videoplayback?expire=1&itag=248&sq=42", true},
		{"https://manifest.googlevideo.com/api/manifest/dash/sq/42", true},
		{"https://googlevideo.com/videoplayback", true},
		{"https://RR5---SN-4G5EDNDS.GoogleVideo.COM/videoplayback", true},
		{"https://rr5---sn-4g5ednds.googlevideo.com:443/videoplayback", true},
		{"https://rr5---sn-4g5ednds.googlevideo.com./videoplayback", true},
		{"https://www.youtube.com/watch?v=x", false},
		{"https://notgooglevideo.com/videoplayback", false},
		{"https://googlevideo.com.example.test/videoplayback", false},
	} {
		req, err := http.NewRequest(http.MethodGet, tc.rawURL, nil)
		if err != nil {
			t.Fatalf("build request for %s: %v", tc.rawURL, err)
		}
		d.setCommonHeaders(req, uaWeb)

		values := req.Header.Values("Cookie")
		switch {
		case tc.withheld && len(values) != 0:
			t.Errorf("request to %s carries %d Cookie header(s), want none: it is a media host", req.URL.Host, len(values))
		case !tc.withheld && !slices.Equal(values, []string{unrotatedJarHeader}):
			t.Errorf("Cookie for %s = %q, want exactly one header carrying the jar verbatim (%q)",
				req.URL.Host, values, unrotatedJarHeader)
		}
	}
}
