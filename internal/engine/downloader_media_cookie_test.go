package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// These tests pin owner ruling K6 (2026-11-13) on the wire: the session
// cookies are not sent to a media host, a media request refused with 403
// while they are withheld is asked once more with them, and when that does
// not help the 403 handling that was there before runs exactly as it did.
//
// They swap the package-level engineHTTPClient (serveEngineInMemory), so none
// of them may call t.Parallel(). Every cookie value and signature is
// synthetic.

// mediaOrigin is a googlevideo host, served in memory: nothing resolves it.
const mediaOrigin = "https://rr5---sn-k6test.googlevideo.com"

// mediaBase is a manifest-free format URL on mediaOrigin.
const mediaBase = mediaOrigin + "/videoplayback?id=k6.1&itag=299&sig=synthetic-signature"

// sessionCookieSentinel is the Cookie header the tests' jar holds. No log
// line may carry any part of it.
const sessionCookieSentinel = "SID=sentinel-session-value"

// mediaRequest is what a media host saw of one request.
type mediaRequest struct {
	cookie string // "" = no Cookie header
	pot    string
	query  string
}

// mediaHost stands in for a googlevideo edge. refuse decides which requests
// it answers 403; every other request is served: a byte range of "abcd" for
// a Range request, the whole of it otherwise.
type mediaHost struct {
	head   string // X-Head-Seqnum on every answer; "" = none
	refuse func(r *http.Request) bool

	mu   sync.Mutex
	seen []mediaRequest
}

func (m *mediaHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.seen = append(m.seen, mediaRequest{cookie: r.Header.Get("Cookie"), pot: r.URL.Query().Get("pot"), query: r.URL.RawQuery})
	m.mu.Unlock()
	if m.head != "" {
		w.Header().Set("X-Head-Seqnum", m.head)
	}
	if m.refuse != nil && m.refuse(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	const file = "abcd"
	if rng := r.Header.Get("Range"); rng != "" {
		start, end := 0, len(file)-1
		if n, _ := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); n == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(file)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(file[start : end+1]))
		return
	}
	_, _ = w.Write([]byte(file))
}

func (m *mediaHost) requests() []mediaRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.seen)
}

// cookiesSeen is the Cookie header of each request, in order.
func (m *mediaHost) cookiesSeen() []string {
	var out []string
	for _, r := range m.requests() {
		out = append(out, r.cookie)
	}
	return out
}

func withoutCookies(r *http.Request) bool { return r.Header.Get("Cookie") == "" }

// mediaPath is one of the six places the downloader sends a request from.
// run reports whether the request was answered with what it asked for.
type mediaPath struct {
	name   string
	direct bool
	run    func(t *testing.T, d *SegmentDownloader) bool
}

var mediaPaths = []mediaPath{
	{"fetchSegment", false, func(t *testing.T, d *SegmentDownloader) bool {
		body, status, err := d.fetchSegment(t.Context(), d.buildSegmentURL(1))
		return err == nil && status == http.StatusOK && string(body) == "abcd"
	}},
	{"probeHeadAt", false, func(t *testing.T, d *SegmentDownloader) bool {
		head, err := d.probeHeadAt(t.Context(), 2)
		return err == nil && head == 5000
	}},
	{"probeSegment", false, func(t *testing.T, d *SegmentDownloader) bool {
		status, body, err := d.probeSegment(t.Context(), 3)
		return err == nil && status == http.StatusOK && string(body) == "abcd"
	}},
	{"probeFileSize", true, func(t *testing.T, d *SegmentDownloader) bool {
		size, status := d.probeFileSize(t.Context())
		return size == 4 && status == http.StatusPartialContent
	}},
	{"fetchChunk", true, func(t *testing.T, d *SegmentDownloader) bool {
		data, status, err := d.fetchChunk(t.Context(), 0, 3)
		return err == nil && status == http.StatusPartialContent && string(data) == "abcd"
	}},
	{"streamDirectOnce", true, func(t *testing.T, d *SegmentDownloader) bool {
		status, _, err := d.streamDirectOnce(t.Context())
		return err == nil && status == http.StatusOK
	}},
}

// newMediaDownloader builds a downloader on mediaBase whose jar holds
// sessionCookieSentinel, with an open output file for the one path that
// writes (streamDirectOnce).
func newMediaDownloader(t *testing.T, direct bool, logger DownloaderLogger) *SegmentDownloader {
	t.Helper()
	path := filepath.Join(t.TempDir(), "media.mp4")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:      mediaBase,
		OutputFile:   path,
		IsDirectURL:  direct,
		Logger:       logger,
		CookieHeader: func() string { return sessionCookieSentinel },
	})
	d.outputFile = f
	t.Cleanup(func() { d.outputFile.Close() })
	return d
}

// warnings is the number of Warn lines the logger holds.
func warnings(l *secretLogger) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.HasPrefix(line, "WARN ") {
			n++
		}
	}
	return n
}

// TestMediaRequestsCarryNoSessionCookies: each of the six request paths, on a
// media host that serves it, sends one request and no Cookie header, and
// logs no warning.
//
// Mutant: setCommonHeaders without the isMediaHost check (every row shows the
// header).
func TestMediaRequestsCarryNoSessionCookies(t *testing.T) {
	for _, p := range mediaPaths {
		t.Run(p.name, func(t *testing.T) {
			host := &mediaHost{head: "5000"}
			serveEngineInMemory(t, host)
			logger := &secretLogger{}
			d := newMediaDownloader(t, p.direct, logger)

			if !p.run(t, d) {
				t.Fatalf("%s was not served", p.name)
			}
			if got := host.cookiesSeen(); !slices.Equal(got, []string{""}) {
				t.Errorf("Cookie headers = %q, want one request with none", got)
			}
			if n := warnings(logger); n != 0 {
				t.Errorf("%d Warn line(s) for a media host that served the request, want none", n)
			}
		})
	}
}

// TestRefusedMediaRequestIsAskedOnceWithCookies is the safety net. A media
// host that answers 403 without the session cookies and serves the request
// with them must cost the download nothing, on every one of the six paths:
// the refused request is sent again with the header, its answer is the one
// the caller gets, one Warn says so, and the next request carries the header
// at once (no second refusal, no second Warn).
//
// The Warn names the host and nothing else of the request: not the cookie's
// value, not the URL's signature.
//
// Mutants: a path left on engineHTTPClient.Do (its row fails with the 403);
// doRequest not setting mediaHostNeedsCookies (the follow-up request is
// refused first: four requests, not three); the Warn outside the
// CompareAndSwap (two Warn lines); the Warn logging req.URL (the signature is
// found).
func TestRefusedMediaRequestIsAskedOnceWithCookies(t *testing.T) {
	for _, p := range mediaPaths {
		t.Run(p.name, func(t *testing.T) {
			host := &mediaHost{head: "5000", refuse: withoutCookies}
			serveEngineInMemory(t, host)
			logger := &secretLogger{}
			d := newMediaDownloader(t, p.direct, logger)

			if !p.run(t, d) {
				t.Fatalf("%s failed on a media host that serves it with cookies: the refusal was not retried with them", p.name)
			}
			if got, want := host.cookiesSeen(), []string{"", sessionCookieSentinel}; !slices.Equal(got, want) {
				t.Fatalf("Cookie headers = %q, want %q: one request without, the same one again with", got, want)
			}
			reqs := host.requests()
			if reqs[0].query != reqs[1].query {
				t.Errorf("the retry asked for %q, the refused request for %q: it must be the same request", reqs[1].query, reqs[0].query)
			}

			// The rest of the download sends the header up front.
			if _, _, err := d.fetchSegment(t.Context(), d.buildSegmentURL(7)); err != nil {
				t.Fatalf("follow-up fetchSegment: %v", err)
			}
			if got, want := host.cookiesSeen(), []string{"", sessionCookieSentinel, sessionCookieSentinel}; !slices.Equal(got, want) {
				t.Errorf("Cookie headers after a follow-up request = %q, want %q", got, want)
			}

			if n := warnings(logger); n != 1 {
				t.Errorf("%d Warn line(s), want exactly one for the download", n)
			}
			if hits := logger.containing("media host needed session cookies"); len(hits) != 1 {
				t.Errorf("lines saying the media host needed session cookies = %q, want one", hits)
			}
			for _, secret := range []string{"sentinel-session-value", "synthetic-signature"} {
				if hits := logger.containing(secret); len(hits) != 0 {
					t.Errorf("log carries %q: %q", secret, hits)
				}
			}
		})
	}
}

// TestCookieRetryThatFailsLeavesThe403HandlingUnchanged proves the other half
// of the safety net: when the cookies do not help, what happens next is what
// happened before they were withheld. Each scenario is one of
// fetchSegmentWithRetry's 403 rules, run twice against a media host that
// ignores cookies: once by a downloader with no jar (which has nothing to
// retry with, so it is the old behaviour by construction) and once by one
// whose jar holds a session.
//
// The two runs must end the same way (same error, same body, same number of
// credential refreshes) and must send the same requests without cookies, in
// the same order. The run with a session adds exactly one request per 403,
// the same one with the Cookie header, right after the refusal; it logs no
// Warn and goes on withholding the header (the request after the refresh
// carries none).
//
// Mutants: doRequest retrying twice (the request count is off); setting
// mediaHostNeedsCookies on a failed retry (later requests carry the header,
// so the cookie-less list is short, and a Warn appears); a failed cookie
// retry's answer ending the fetch without the refresh (the first scenario
// ends in an error with no refresh).
func TestCookieRetryThatFailsLeavesThe403HandlingUnchanged(t *testing.T) {
	stalePot := func(r *http.Request) bool { return r.URL.Query().Get("pot") != "fresh" }
	always := func(*http.Request) bool { return true }

	scenarios := []struct {
		name        string
		head        int // -1 = no head known
		seq         int
		refuse      func(*http.Request) bool
		withRefresh bool
		wantErr     error
		wantRefresh int32
		wantPlain   []string // pot of each request sent without cookies
	}{
		{"behind the head: refresh and retry", 5000, 10, stalePot, true, nil, 1, []string{"stale", "fresh"}},
		{"behind the head, no refresh callback: permanent at once", 5000, 10, always, false, ErrSegmentPermanent, 0, []string{"stale"}},
		{"past the head: the end of the stream", 100, 101, always, true, ErrSegmentPermanent, 0, []string{"stale"}},
		{"no head known: three asks, no refresh", -1, 10, always, true, ErrSegmentPermanent, 0, []string{"stale", "stale", "stale"}},
	}

	type outcome struct {
		err       error
		body      string
		refreshes int32
		reqs      []mediaRequest
		warns     int
	}
	run := func(t *testing.T, sc int, cookieHeader func() string) outcome {
		s := scenarios[sc]
		host := &mediaHost{refuse: s.refuse}
		if s.head >= 0 {
			host.head = fmt.Sprint(s.head)
		}
		serveEngineInMemory(t, host)
		logger := &secretLogger{}
		var refreshes atomic.Int32
		opts := DownloaderOptions{
			BaseURL:      mediaBase,
			OutputFile:   "x",
			PoToken:      "stale",
			Logger:       logger,
			CookieHeader: cookieHeader,
		}
		if s.withRefresh {
			opts.OnCredentialRefresh = func() (string, string) {
				refreshes.Add(1)
				return "", "fresh"
			}
		}
		d := NewSegmentDownloader(opts)
		if s.head >= 0 {
			d.noteHeadSeq(s.head)
		}
		d.currentSeq.Store(int64(s.seq))
		d.lastSegTime.StoreNow() // behindHeadTailPending's budget clock, as runDashLoop seeds it
		d.delays = fastDelays()

		body, err := d.fetchSegmentWithRetry(context.Background(), d.buildSegmentURL(s.seq), nil)
		return outcome{err: err, body: string(body), refreshes: refreshes.Load(), reqs: host.requests(), warns: warnings(logger)}
	}

	for i, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			before := run(t, i, nil)
			after := run(t, i, func() string { return sessionCookieSentinel })

			if !errors.Is(before.err, s.wantErr) || !errors.Is(after.err, s.wantErr) {
				t.Fatalf("errors = %v (no jar), %v (session); want %v from both", before.err, after.err, s.wantErr)
			}
			if before.body != after.body {
				t.Errorf("body = %q (no jar), %q (session)", before.body, after.body)
			}
			if before.refreshes != s.wantRefresh || after.refreshes != s.wantRefresh {
				t.Errorf("credential refreshes = %d (no jar), %d (session); want %d from both", before.refreshes, after.refreshes, s.wantRefresh)
			}

			var plainBefore []string
			for _, r := range before.reqs {
				if r.cookie != "" {
					t.Errorf("a downloader with no jar sent a Cookie header")
				}
				plainBefore = append(plainBefore, r.pot)
			}
			if !slices.Equal(plainBefore, s.wantPlain) {
				t.Errorf("requests with no jar = %q, want %q", plainBefore, s.wantPlain)
			}

			var plain []string
			refused := 0
			for n, r := range after.reqs {
				if r.cookie != "" {
					continue
				}
				plain = append(plain, r.pot)
				if s.wantErr == nil && r.pot == "fresh" {
					continue // the one request these hosts serve
				}
				refused++
				if n+1 >= len(after.reqs) || after.reqs[n+1].cookie != sessionCookieSentinel || after.reqs[n+1].query != r.query {
					t.Errorf("request %d was refused without cookies and was not followed by the same request with them", n)
				}
			}
			if !slices.Equal(plain, s.wantPlain) {
				t.Errorf("requests without cookies, with a session = %q, want %q: the ones a downloader with no jar sends", plain, s.wantPlain)
			}
			if got, want := len(after.reqs), len(s.wantPlain)+refused; got != want {
				t.Errorf("requests with a session = %d, want %d: one cookie retry per refusal and no more", got, want)
			}
			if before.warns != 0 || after.warns != 0 {
				t.Errorf("Warn lines = %d (no jar), %d (session); want none: the cookies did not help", before.warns, after.warns)
			}
		})
	}
}

// TestRefusalIsNotRepeatedWhenCookiesWereSent: the second ask exists only for
// a request the cookies were withheld from. A 403 to a request that already
// carried them is the caller's at once, with no second request: on a host
// that is not a media host, and on a media host once the download has gone
// over to sending them.
//
// Mutant: doRequest without its "carried a Cookie header, or not a media
// host" return (each row sends the refused request twice).
func TestRefusalIsNotRepeatedWhenCookiesWereSent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		base   string
		sticky bool
	}{
		{"not a media host", inMemoryOrigin + "/videoplayback?id=k6.1&itag=299", false},
		{"a media host this download already sends cookies to", mediaBase, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &mediaHost{refuse: func(*http.Request) bool { return true }}
			serveEngineInMemory(t, host)
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL:      tc.base,
				OutputFile:   "x",
				CookieHeader: func() string { return sessionCookieSentinel },
			})
			d.mediaHostNeedsCookies.Store(tc.sticky)

			if _, status, _ := d.fetchSegment(t.Context(), d.buildSegmentURL(1)); status != http.StatusForbidden {
				t.Fatalf("status = %d, want the 403", status)
			}
			if got, want := host.cookiesSeen(), []string{sessionCookieSentinel}; !slices.Equal(got, want) {
				t.Errorf("Cookie headers = %q, want %q: one request, with the cookies", got, want)
			}
		})
	}
}
