package engine

import (
	"context"
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
// They also pin what this change deliberately does NOT do: nothing filters the
// header, re-scopes it by host, or otherwise changes which cookies go where.
// For an unrotated jar the bytes on the wire are identical to before.
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
		if _, _, err := d.fetchSegment(context.Background(), d.buildSegmentURL(seq)); err != nil {
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

	if _, _, err := d.fetchSegment(context.Background(), d.buildSegmentURL(1)); err != nil {
		t.Fatalf("fetchSegment: %v", err)
	}
	if _, err := d.probeHeadAt(context.Background(), 2); err != nil {
		t.Fatalf("probeHeadAt: %v", err)
	}
	d.probeFileSize(context.Background())

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

	if _, _, err := d.fetchSegment(context.Background(), d.buildSegmentURL(1)); err != nil {
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

	if _, _, err := d.fetchSegment(context.Background(), d.buildSegmentURL(1)); err != nil {
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
		if _, _, err := d.fetchSegment(context.Background(), d.buildSegmentURL(seq)); err != nil {
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

// TestCookieHeaderIsNotHostScoped pins the rejected alternative out of the
// code, at the only place it could be reintroduced.
//
// Dropping cookies for *.googlevideo.com rests on the unmeasured premise that
// entitlement rides the signed URL rather than the session; if that premise is
// wrong the failure lands on members-only and age-gated captures — the exact
// content the cookie subsystem exists to serve. A wire test against a local
// server cannot see such a gate, because the gate would key on the media host
// and the test server is 127.0.0.1. So this asserts on setCommonHeaders
// directly, with the real host in the request.
func TestCookieHeaderIsNotHostScoped(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:      "https://rr5---sn-4g5ednds.googlevideo.com/videoplayback?expire=1&itag=248",
		OutputFile:   "x",
		CookieHeader: func() string { return unrotatedJarHeader },
	})

	for _, rawURL := range []string{
		"https://rr5---sn-4g5ednds.googlevideo.com/videoplayback?expire=1&itag=248&sq=42",
		"https://manifest.googlevideo.com/api/manifest/dash/sq/42",
		"https://www.youtube.com/watch?v=x",
	} {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("build request for %s: %v", rawURL, err)
		}
		d.setCommonHeaders(req, uaWeb)

		values := req.Header.Values("Cookie")
		if !slices.Equal(values, []string{unrotatedJarHeader}) {
			t.Errorf("Cookie for %s = %q, want exactly one header carrying the jar verbatim (%q) — "+
				"which cookies reach which host is the jar's decision, not this function's",
				req.URL.Host, values, unrotatedJarHeader)
		}
	}
}
