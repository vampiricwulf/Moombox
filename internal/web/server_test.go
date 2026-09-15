package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// staticFixture mounts three assets on a real server + middleware chain.
// RemoteAddr matters: IPGateMiddleware is in the chain and the default
// network_access is "localhost", so a request must arrive from loopback.
func staticFixture(t *testing.T, commit string) (*Server, fstest.MapFS) {
	t.Helper()
	s := NewServer(config.NewStore(config.Defaults(), ""), testWSLogger{})
	if commit != "" {
		s.SetCommit(commit)
	}
	fsys := fstest.MapFS{
		"index.html": {Data: []byte(`<html><head><link rel="stylesheet" href="/moombox.css" /></head>` +
			`<body><script type="module" src="/app.js"></script></body></html>`)},
		"app.js":      {Data: []byte("export const app = 1;\n")},
		"favicon.svg": {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
	}
	s.MountStaticFiles(fsys)
	return s, fsys
}

func getStatic(t *testing.T, s *Server, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)
	return rr
}

// TestStaticAssetsAnswerConditionalGET: embed.FS has a zero ModTime, so
// net/http emits no Last-Modified and every page load re-downloaded app.js
// (144 KB) plus ~20 modules in full. An ETag gives net/http something to
// compare (sweep T2-18).
//
// THE MUTANT: stop setting the ETag header — the 304 assertion sees 200 and
// the whole body comes back on every load.
func TestStaticAssetsAnswerConditionalGET(t *testing.T) {
	s, _ := staticFixture(t, "abc1234")

	first := getStatic(t, s, "/app.js", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first GET: status %d, want 200", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag != `"abc1234"` {
		t.Fatalf("ETag = %q, want the quoted build commit %q", etag, `"abc1234"`)
	}

	second := getStatic(t, s, "/app.js", map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Fatalf("conditional GET: status %d, want 304 (body was %d bytes)", second.Code, second.Body.Len())
	}
	if second.Body.Len() != 0 {
		t.Errorf("304 carried %d body bytes, want none", second.Body.Len())
	}
}

// TestStaticAssetETagFallsBackToContentHash: a build with no VCS stamp reports
// commit "unknown", and a build from a dirty working tree reports
// "<rev>-dirty". Using either as the validator would give two different builds
// the same ETag, so an in-place update — or a local-dev rebuild — would keep
// serving the old app.js.
//
// THE MUTANT: treat "unknown" (or the "-dirty" suffix) as a known commit —
// both files come back with the same ETag and the assertion fails.
func TestStaticAssetETagFallsBackToContentHash(t *testing.T) {
	for _, commit := range []string{"", "unknown", "abc1234-dirty"} {
		s, _ := staticFixture(t, commit)

		js := getStatic(t, s, "/app.js", nil).Header().Get("ETag")
		svg := getStatic(t, s, "/favicon.svg", nil).Header().Get("ETag")
		if js == "" || svg == "" {
			t.Fatalf("commit=%q: ETag missing (js=%q svg=%q)", commit, js, svg)
		}
		if js == svg {
			t.Errorf("commit=%q: app.js and favicon.svg share the ETag %q — the fallback must be "+
				"per-FILE content, or one changed file invalidates nothing", commit, js)
		}
		if len(js) != 66 { // 64 hex digits inside two quotes
			t.Errorf("commit=%q: ETag %q is not a quoted SHA-256", commit, js)
		}

		again := getStatic(t, s, "/app.js", map[string]string{"If-None-Match": js})
		if again.Code != http.StatusNotModified {
			t.Errorf("commit=%q: conditional GET on the hash ETag: status %d, want 304", commit, again.Code)
		}
	}
}

// TestStaticCachePolicyFollowsTheCacheBuster: `immutable, max-age=1y` was
// applied by EXTENSION, so unversioned /favicon.svg was pinned for a year while
// /app.js?v=<commit> — the one URL that genuinely cannot go stale — revalidated
// every load (sweep T2-18).
//
// THE MUTANT: restore the extension switch — favicon.svg comes back immutable
// and a rebranded install keeps the old icon for a year.
func TestStaticCachePolicyFollowsTheCacheBuster(t *testing.T) {
	s, _ := staticFixture(t, "abc1234")

	if cc := getStatic(t, s, "/favicon.svg", nil).Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("/favicon.svg Cache-Control = %q; an unversioned URL must revalidate", cc)
	}
	cc := getStatic(t, s, "/app.js?v=abc1234", nil).Header().Get("Cache-Control")
	if !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
		t.Errorf("/app.js?v= Cache-Control = %q, want the year-long immutable policy", cc)
	}
}

// TestStaticCachePolicyNeedsATrustedCommit: the immutable branch keyed off the
// ?v= QUERY alone, so an untrusted commit — "unknown", or a dirty-tree
// "<rev>-dirty" — still pinned /app.js?v=<that> for a year even though the
// ETag beside it had already been downgraded to a content hash for exactly the
// same reason. A year-long immutable entry cannot be revalidated at all, so a
// dev rebuild (or an in-place update of a stamp-less build) was unreachable
// until the user hard-reloaded (sweep R5-4/F2).
//
// THE MUTANT: let the immutable branch check only r.URL.Query().Get("v") —
// both untrusted commits come back immutable and this fails.
func TestStaticCachePolicyNeedsATrustedCommit(t *testing.T) {
	for _, commit := range []string{"unknown", "abc1234-dirty"} {
		s, _ := staticFixture(t, commit)

		rr := getStatic(t, s, "/app.js?v="+commit, nil)
		cc := rr.Header().Get("Cache-Control")
		if strings.Contains(cc, "immutable") {
			t.Errorf("commit=%q: /app.js?v= Cache-Control = %q; an untrusted commit names no fixed "+
				"body, so the URL must stay revalidatable", commit, cc)
		}
		if cc != "no-cache" {
			t.Errorf("commit=%q: Cache-Control = %q, want no-cache", commit, cc)
		}
		// The ETag is still there: revalidation must cost a 304, not the file.
		if tag := rr.Header().Get("ETag"); len(tag) != 66 {
			t.Errorf("commit=%q: ETag %q is not a quoted SHA-256", commit, tag)
		}
	}

	// The trusted case is unchanged: a real commit + ?v= is still immutable.
	s, _ := staticFixture(t, "abc1234")
	if cc := getStatic(t, s, "/app.js?v=abc1234", nil).Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("trusted commit: Cache-Control = %q, want the year-long immutable policy", cc)
	}
}

// TestCompressionSkipsAlreadyCompressedMedia: a dashboard full of job cards
// gzipped one JPEG per card for nothing (sweep T2-18).
//
// THE MUTANT: delete the skipCompression call in Write — Content-Encoding
// comes back "gzip" on an image/jpeg body.
func TestCompressionSkipsAlreadyCompressedMedia(t *testing.T) {
	body := strings.Repeat("jpeg-ish-bytes-", 200) // > gzipMinSize
	for _, ct := range []string{"image/jpeg", "image/png", "video/mp4"} {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(body))
		})
		req := httptest.NewRequest(http.MethodGet, "/api/jobs/abc/thumbnail", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		CompressionMiddleware(handler).ServeHTTP(rec, req)

		if enc := rec.Header().Get("Content-Encoding"); enc != "" {
			t.Errorf("Content-Type %s: Content-Encoding = %q, want none", ct, enc)
		}
		if rec.Body.String() != body {
			t.Errorf("Content-Type %s: body changed (%d bytes, want %d)", ct, rec.Body.Len(), len(body))
		}
	}

	// A handler that encoded its own body must not be encoded twice.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body))
	})
	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	CompressionMiddleware(handler).ServeHTTP(rec, req)
	if enc := rec.Header().Get("Content-Encoding"); enc != "br" {
		t.Errorf("Content-Encoding = %q on a pre-encoded body, want the handler's own %q", enc, "br")
	}
}

// TestActualPortIsReadThroughAnAccessor: Start writes the bound port from its
// own goroutine while main.go reads it across a 500 ms bind window
// (cmd/moombox/main.go:373 and :452, routes_wiring.go:216). As a plain int
// field that is an unsynchronised read of a concurrently-written word.
//
// THE MUTANT: make actualPort a plain int again — cmd/moombox stops compiling,
// and `go test -race -run TestActualPort ./internal/web/` reports a DATA RACE
// on this test's two goroutines.
func TestActualPortIsReadThroughAnAccessor(t *testing.T) {
	s := NewServer(config.NewStore(config.Defaults(), ""), testWSLogger{})

	if got := s.ActualPort(); got != 0 {
		t.Errorf("ActualPort() = %d before any bind, want 0 — main.go treats 0 as 'not bound yet' "+
			"and would otherwise report a healthy dashboard as FAILED", got)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.setActualPort(8123)
	}()
	for range 1000 { // read concurrently with the write, the way main.go does
		_ = s.ActualPort()
	}
	<-done

	if got := s.ActualPort(); got != 8123 {
		t.Errorf("ActualPort() = %d after the bind, want 8123", got)
	}
}

// TestRootServesTheCacheBustedIndex: /, /index.html and every SPA route must
// serve the SUBSTITUTED copy. Before this, only the SPA-fallback branch did —
// the root normalised to "index.html", found it in the FS and served the RAW
// embedded bytes through the FileServer, so the dashboard's own load never got
// a cache-busted app.js (Arc 5 arc-close F8).
//
// THE MUTANT: restore the file-exists branch ahead of the index branch — the
// ?v= assertions see the raw body.
func TestRootServesTheCacheBustedIndex(t *testing.T) {
	s, _ := staticFixture(t, "abc1234")

	for _, target := range []string{"/", "/index.html", "/jobs/42"} {
		rr := getStatic(t, s, target, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d, want 200", target, rr.Code)
		}
		body := rr.Body.String()
		for _, want := range []string{`"/app.js?v=abc1234"`, `"/moombox.css?v=abc1234"`} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s: body does not carry %s", target, want)
			}
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s: Cache-Control = %q, want no-cache", target, got)
		}
		if got := rr.Header().Get("ETag"); got != `"abc1234"` {
			t.Errorf("GET %s: ETag = %q, want the quoted build commit", target, got)
		}
		if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Errorf("GET %s: Content-Type = %q, want text/html", target, got)
		}
	}
}

// THE MUTANT: keep the substitution gate at `s.commit != ""` — the body comes
// back carrying "?v=unknown" / "?v=abc-dirty", which names no build, and the
// ETag then describes bytes that are not the ones served.
func TestRootOmitsTheCacheBusterOnAnUntrustedCommit(t *testing.T) {
	for _, commit := range []string{"unknown", "abc1234-dirty"} {
		t.Run(commit, func(t *testing.T) {
			s, fsys := staticFixture(t, commit)
			rr := getStatic(t, s, "/", nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("status %d, want 200", rr.Code)
			}
			if strings.Contains(rr.Body.String(), "?v=") {
				t.Errorf("body carries a ?v= for untrusted commit %q: %s", commit, rr.Body.String())
			}
			// With no substitution the served bytes ARE the embedded file, so
			// the content-hash ETag describes them exactly.
			sum := sha256.Sum256(fsys["index.html"].Data)
			want := `"` + hex.EncodeToString(sum[:]) + `"`
			if got := rr.Header().Get("ETag"); got != want {
				t.Errorf("ETag = %q, want the content hash %q", got, want)
			}
			if got := rr.Body.String(); got != string(fsys["index.html"].Data) {
				t.Errorf("body = %q, want the embedded file verbatim", got)
			}
		})
	}
}

// THE MUTANT: write the body with w.Write instead of http.ServeContent — the
// conditional request is answered with a full 200.
func TestRootAnswersConditionalGET(t *testing.T) {
	s, _ := staticFixture(t, "abc1234")

	first := getStatic(t, s, "/", nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the first GET")
	}

	second := getStatic(t, s, "/", map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Fatalf("status %d, want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 carried %d body bytes, want 0", second.Body.Len())
	}
}
