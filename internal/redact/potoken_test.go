package redact

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestPoTokenURL pins the URL rule for both places a googlevideo URL carries
// the GVS PO token, and that nothing else in the URL changes.
//
// Mutants (run):
//   - PoTokenURL without its redactPotPath call: every path-form row keeps
//     its token (the W24-08 defect — the live DASH/HLS manifest URL).
//   - redactPotPath without the authority skip: "host named pot" rewrites
//     the first path segment.
//   - the query loop matching on strings.HasPrefix(p, "pot") instead of the
//     key: "potato" is rewritten.
//   - PoTokenText (the unparseable fallback) without potPathRe: the
//     "unparseable, path form" row keeps its token.
func TestPoTokenURL(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"no token", "https://h/videoplayback?itag=1&sq=5", "https://h/videoplayback?itag=1&sq=5"},
		{"query, last", "https://h/v?itag=1&pot=SECRET", "https://h/v?itag=1&pot=<redacted>"},
		{"query, first", "https://h/v?pot=SECRET&itag=1", "https://h/v?pot=<redacted>&itag=1"},
		{"query with fragment", "https://h/v?pot=SECRET#t=1", "https://h/v?pot=<redacted>#t=1"},
		{"query, other key kept", "https://h/v?potato=1&pot=SECRET", "https://h/v?potato=1&pot=<redacted>"},
		{
			"path, live DASH manifest",
			"https://manifest.googlevideo.com/api/manifest/dash/id/abc/source/yt_live_broadcast/pot/SECRET",
			"https://manifest.googlevideo.com/api/manifest/dash/id/abc/source/yt_live_broadcast/pot/<redacted>",
		},
		{
			"path, HLS variant playlist",
			"https://manifest.googlevideo.com/api/manifest/hls_playlist/id/x/itag/95/playlist/index.m3u8/pot/SECRET",
			"https://manifest.googlevideo.com/api/manifest/hls_playlist/id/x/itag/95/playlist/index.m3u8/pot/<redacted>",
		},
		{"path, mid-URL", "https://h/a/pot/SECRET/b/c", "https://h/a/pot/<redacted>/b/c"},
		{"path and query", "https://h/a/pot/SECRET?pot=SECRET&x=1", "https://h/a/pot/<redacted>?pot=<redacted>&x=1"},
		{"path, trailing slash", "https://h/a/pot/", "https://h/a/pot/"},
		{"host named pot", "https://pot/SECRETLESS/x", "https://pot/SECRETLESS/x"},
		{"no scheme", "/api/pot/SECRET", "/api/pot/<redacted>"},
		{"already redacted", "https://h/a/pot/<redacted>?pot=<redacted>", "https://h/a/pot/<redacted>?pot=<redacted>"},
		{"unparseable, query form", "http://[::1%zz/v?pot=SECRET&a=b", "http://[::1%zz/v?pot=<redacted>&a=b"},
		{"unparseable, path form", "http://[::1%zz/v/pot/SECRET?a=b", "http://[::1%zz/v/pot/<redacted>?a=b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PoTokenURL(tc.in); got != tc.want {
				t.Fatalf("PoTokenURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestPoTokenText pins the free-text rule: both forms go wherever they sit in
// an already flattened message, and text without a token is left alone.
//
// Mutants (run): dropping either regexp's ReplaceAllString fails the row of
// its form; a fast path that returns s when it lacks "pot=" fails the path
// row.
func TestPoTokenText(t *testing.T) {
	cases := []struct{ in, want string }{
		{`setup download: fetch DASH manifest: Get "http://127.0.0.1:1/api/manifest/dash/id/abc/pot/SECRET": dial tcp: connection refused`,
			`setup download: fetch DASH manifest: Get "http://127.0.0.1:1/api/manifest/dash/id/abc/pot/<redacted>": dial tcp: connection refused`},
		{`download: Get "https://h/v?itag=1&pot=SECRET": EOF`, `download: Get "https://h/v?itag=1&pot=<redacted>": EOF`},
		{"no token at all", "no token at all"},
		{"spotless", "spotless"},
	}
	for _, tc := range cases {
		if got := PoTokenText(tc.in); got != tc.want {
			t.Errorf("PoTokenText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPoToken pins the error contract: a *url.Error found anywhere in the
// tree loses its token, in either form; the chain and every other error pass
// through untouched.
//
// Mutants (run):
//   - PoToken returning err unchanged: every redacting row keeps SECRET.
//   - rewritePotURLs not descending into Unwrap() []error: the "joined" row
//     keeps the token of the second url.Error.
//   - building the wrapper from err.Error() read AFTER the rewrite but without
//     PoTokenText, for a wrapper whose message was precomputed: the
//     "wrapped, path form" row keeps the token.
func TestPoToken(t *testing.T) {
	cause := errors.New("boom")
	cases := []struct {
		name    string
		err     error
		want    string
		samePtr bool
	}{
		{"nil", nil, "", true},
		{"not a url.Error", cause, "boom", true},
		{"no pot", &url.Error{Op: "Get", URL: "https://h/v?itag=1", Err: cause}, `Get "https://h/v?itag=1": boom`, true},
		{"bare, query form", &url.Error{Op: "Get", URL: "https://h/v?itag=1&pot=SECRET&x=2", Err: cause}, `Get "https://h/v?itag=1&pot=<redacted>&x=2": boom`, false},
		{"bare, path form", &url.Error{Op: "Get", URL: "https://h/index.m3u8/pot/SECRET", Err: cause}, `Get "https://h/index.m3u8/pot/<redacted>": boom`, false},
		{"wrapped, query form", fmt.Errorf("download: %w", &url.Error{Op: "Get", URL: "https://h/v?pot=SECRET", Err: cause}), `download: Get "https://h/v?pot=<redacted>": boom`, false},
		{"wrapped, path form", fmt.Errorf("fetch HLS manifest: %w", &url.Error{Op: "Get", URL: "https://h/index.m3u8/pot/SECRET", Err: cause}), `fetch HLS manifest: Get "https://h/index.m3u8/pot/<redacted>": boom`, false},
		{"joined", errors.Join(&url.Error{Op: "Get", URL: "https://h/a", Err: cause}, &url.Error{Op: "Get", URL: "https://h/b/pot/SECRET", Err: cause}),
			"Get \"https://h/a\": boom\nGet \"https://h/b/pot/<redacted>\": boom", false},
		{"unparseable URL", &url.Error{Op: "parse", URL: "http://[::1%zz/v?pot=SECRET&a=b", Err: cause}, `parse "http://[::1%zz/v?pot=<redacted>&a=b": boom`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PoToken(tc.err)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("PoToken(nil) = %v", got)
				}
				return
			}
			if got.Error() != tc.want {
				t.Fatalf("Error() = %q, want %q", got.Error(), tc.want)
			}
			if tc.samePtr && got != tc.err {
				t.Fatalf("PoToken returned a new error for %q, want it untouched", tc.name)
			}
			if errors.Is(tc.err, cause) && !errors.Is(got, cause) {
				t.Fatalf("errors.Is(cause) lost")
			}
			if again := PoToken(got); again.Error() != got.Error() {
				t.Fatalf("second PoToken changed the text: %q -> %q", got.Error(), again.Error())
			}
		})
	}
}

// TestPoTokenKeepsTransportCause drives a real transport failure: the
// *url.Error and the *net.OpError under it must survive the redaction, since
// callers classify transport errors with errors.As.
func TestPoTokenKeepsTransportCause(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	resp, err := (&http.Client{Transport: &http.Transport{}}).Get("http://" + addr + "/api/manifest/dash/id/abc/pot/SECRETPOT")
	if err == nil {
		resp.Body.Close()
		t.Fatal("want a transport error from a refused port")
	}
	got := PoToken(fmt.Errorf("fetch DASH manifest: %w", err))
	if strings.Contains(got.Error(), "SECRETPOT") {
		t.Fatalf("error carries the PO token: %q", got.Error())
	}
	var ue *url.Error
	if !errors.As(got, &ue) || ue.Op != "Get" {
		t.Fatalf("errors.As(*url.Error) lost on %q", got.Error())
	}
	var oe *net.OpError
	if !errors.As(got, &oe) || oe.Op != "dial" {
		t.Fatalf("errors.As(*net.OpError) lost on %q", got.Error())
	}
}
