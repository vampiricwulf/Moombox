package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// potRequest is one request the direct-path test server saw: the query the
// URL carried and the Range header, which is what names the builder that
// sent it (bytes=0-0 = probeFileSize, bytes=N-M = fetchChunk, none or an
// open-ended bytes=N- = runDirectDownloadFallback).
type potRequest struct {
	rawQuery string
	hasPot   bool
	pot      string
	rng      string
}

func (r potRequest) kind() string {
	switch {
	case r.rng == "bytes=0-0":
		return "probeFileSize (Range bytes=0-0)"
	case r.rng == "" || strings.HasSuffix(r.rng, "-"):
		return fmt.Sprintf("runDirectDownloadFallback (Range %q)", r.rng)
	default:
		return fmt.Sprintf("fetchChunk (Range %s)", r.rng)
	}
}

// potServer serves body as a direct googlevideo-style file and records every
// request. honourRange=false answers every request 200 with the whole body,
// so probeFileSize returns 0 and runDirectDownload takes the streaming
// fallback.
func potServer(t *testing.T, body []byte, honourRange bool) (*httptest.Server, func() []potRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []potRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		_, has := q["pot"]
		mu.Lock()
		seen = append(seen, potRequest{rawQuery: r.URL.RawQuery, hasPot: has, pot: q.Get("pot"), rng: r.Header.Get("Range")})
		mu.Unlock()

		rng := r.Header.Get("Range")
		if !honourRange || rng == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			w.Write(body)
			return
		}
		spec := strings.TrimPrefix(rng, "bytes=")
		startStr, endStr, _ := strings.Cut(spec, "-")
		start, _ := strconv.ParseInt(startStr, 10, 64)
		end := int64(len(body)) - 1
		if endStr != "" {
			end, _ = strconv.ParseInt(endStr, 10, 64)
		}
		if end >= int64(len(body)) {
			end = int64(len(body)) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[start : end+1])
	}))
	t.Cleanup(srv.Close)
	return srv, func() []potRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]potRequest(nil), seen...)
	}
}

// runDirectWithToken drives runDirectDownload against baseURL with the given
// PO token and returns the bytes written.
func runDirectWithToken(t *testing.T, baseURL, token string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "video.mp4")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: baseURL, OutputFile: path, IsDirectURL: true, PoToken: token})
	d.delays = fastDelays()
	d.outputFile = f
	t.Cleanup(func() { d.outputFile.Close() })

	if err := d.runDirectDownload(context.Background()); err != nil {
		t.Fatalf("runDirectDownload = %v, want nil", err)
	}
	d.outputFile.Sync()
	got, _ := os.ReadFile(path)
	return got
}

// potTestBody spans three 5 MB chunks so the chunked loop sends more than one
// fetchChunk request.
func potTestBody() []byte {
	body := make([]byte, 2*DownloadChunkSize+100)
	for i := range body {
		body[i] = byte(i)
	}
	return body
}

// TestDirectPathAttachesPoToken pins the VOD 403 fix (2026-09-29): a WEB-family
// format URL 403s its first 5 MB chunk without the GVS PO token, so the three
// direct-path request builders inject it through applyPoTokenQuery exactly as
// fetchSegment and probeHeadAt already do.
//
// Mutants (each run, each named by the failure below): reverting
// probeFileSize's builder to the bare d.getBaseURL() fails the "probeFileSize"
// request; reverting fetchChunk's fails every "fetchChunk" request; reverting
// runDirectDownloadFallback's fails the "runDirectDownloadFallback" request of
// the non-Range subtest.
func TestDirectPathAttachesPoToken(t *testing.T) {
	body := potTestBody()

	t.Run("chunked", func(t *testing.T) {
		srv, seen := potServer(t, body, true)
		got := runDirectWithToken(t, srv.URL+"/videoplayback", "tok123")
		if len(got) != len(body) {
			t.Fatalf("wrote %d bytes, want %d", len(got), len(body))
		}
		reqs := seen()
		var probes, chunks int
		for _, r := range reqs {
			if r.pot != "tok123" {
				t.Errorf("%s carried pot=%q (query %q), want pot=tok123", r.kind(), r.pot, r.rawQuery)
			}
			switch {
			case strings.HasPrefix(r.kind(), "probeFileSize"):
				probes++
			case strings.HasPrefix(r.kind(), "fetchChunk"):
				chunks++
			}
		}
		if probes != 1 || chunks != 3 {
			t.Fatalf("saw %d probes and %d chunks, want 1 and 3: %+v", probes, chunks, reqs)
		}
	})

	t.Run("streaming fallback", func(t *testing.T) {
		srv, seen := potServer(t, body, false)
		got := runDirectWithToken(t, srv.URL+"/videoplayback", "tok123")
		if len(got) != len(body) {
			t.Fatalf("wrote %d bytes, want %d", len(got), len(body))
		}
		reqs := seen()
		var fallbacks int
		for _, r := range reqs {
			if r.pot != "tok123" {
				t.Errorf("%s carried pot=%q (query %q), want pot=tok123", r.kind(), r.pot, r.rawQuery)
			}
			if strings.HasPrefix(r.kind(), "runDirectDownloadFallback") {
				fallbacks++
			}
		}
		if fallbacks != 1 {
			t.Fatalf("saw %d fallback requests, want 1 — the non-Range server must force the streaming fallback: %+v", fallbacks, reqs)
		}
	})
}

// TestDirectPathNoPoTokenLeavesURLBare pins the other half: a downloader built
// without a token (tv/visionos/android_vr formats) sends the URL untouched —
// no pot key at all, not an empty one.
func TestDirectPathNoPoTokenLeavesURLBare(t *testing.T) {
	body := potTestBody()
	for _, honour := range []bool{true, false} {
		t.Run(fmt.Sprintf("honourRange=%v", honour), func(t *testing.T) {
			srv, seen := potServer(t, body, honour)
			runDirectWithToken(t, srv.URL+"/videoplayback", "")
			reqs := seen()
			if len(reqs) == 0 {
				t.Fatal("server saw no requests")
			}
			for _, r := range reqs {
				if r.hasPot {
					t.Errorf("%s carried a pot key (query %q), want none", r.kind(), r.rawQuery)
				}
			}
		})
	}
}

// TestDirectPathPoTokenJoinsExistingQuery pins the separator: a googlevideo
// URL always has a query, so the token must go on with '&', never a second
// '?' that would fold it into the previous parameter's value.
func TestDirectPathPoTokenJoinsExistingQuery(t *testing.T) {
	body := potTestBody()
	for _, honour := range []bool{true, false} {
		t.Run(fmt.Sprintf("honourRange=%v", honour), func(t *testing.T) {
			srv, seen := potServer(t, body, honour)
			runDirectWithToken(t, srv.URL+"/videoplayback?itag=302&expire=1", "tok123")
			for _, r := range seen() {
				if r.rawQuery != "itag=302&expire=1&pot=tok123" {
					t.Errorf("%s query = %q, want itag=302&expire=1&pot=tok123", r.kind(), r.rawQuery)
				}
			}
		})
	}
}

// fallbackTransportError drives runDirectDownloadFallback at rawURL with the
// given token and returns the error the transport failure produced.
func fallbackTransportError(t *testing.T, parent context.Context, rawURL, token string) error {
	t.Helper()
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: rawURL, OutputFile: filepath.Join(t.TempDir(), "video.mp4"), IsDirectURL: true, PoToken: token})
	d.delays = fastDelays()
	err := d.runDirectDownloadFallback(parent)
	if err == nil {
		t.Fatalf("runDirectDownloadFallback(%q) = nil, want a transport error", rawURL)
	}
	return err
}

// refusedURL returns a URL on a loopback port nothing listens on, so the
// fallback's Do fails at dial.
func refusedURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr + "/videoplayback?itag=302&expire=1"
}

// TestFallbackTransportErrorRedactsPoToken pins the close-wave fix: the
// streaming fallback's transport failure is a *url.Error whose Error() embeds
// the full request URL — pot= included — and that string used to reach `job
// error` and the job's stored error. The token is redacted in place while the
// error chain stays intact for errors.Is / errors.As.
//
// Mutant (run): returning err unchanged from redact.MediaError fails every
// "contains SECRETTOKEN" row below.
func TestFallbackTransportErrorRedactsPoToken(t *testing.T) {
	t.Run("dial refused", func(t *testing.T) {
		err := fallbackTransportError(t, context.Background(), refusedURL(t), "SECRETTOKEN")
		msg := err.Error()
		if strings.Contains(msg, "SECRETTOKEN") {
			t.Fatalf("error carries the PO token: %q", msg)
		}
		if !strings.Contains(msg, "pot=<redacted>") {
			t.Fatalf("error = %q, want it to show pot=<redacted>", msg)
		}
		if !strings.Contains(msg, "itag=302&expire=1&") {
			t.Fatalf("error = %q, want the rest of the query kept", msg)
		}
		var ue *url.Error
		if !errors.As(err, &ue) {
			t.Fatalf("errors.As(*url.Error) = false on %q", msg)
		}
		if ue.Op != "Get" {
			t.Fatalf("url.Error.Op = %q, want Get", ue.Op)
		}
		var oe *net.OpError
		if !errors.As(err, &oe) || oe.Op != "dial" {
			t.Fatalf("errors.As(*net.OpError) = %v (%+v), want the dial error kept as the cause", oe != nil, oe)
		}
	})

	t.Run("cancelled parent", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := fallbackTransportError(t, ctx, refusedURL(t), "SECRETTOKEN")
		if strings.Contains(err.Error(), "SECRETTOKEN") {
			t.Fatalf("error carries the PO token: %q", err.Error())
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("errors.Is(context.Canceled) = false on %q", err.Error())
		}
	})

	t.Run("no token leaves the URL as it was", func(t *testing.T) {
		raw := refusedURL(t)
		err := fallbackTransportError(t, context.Background(), raw, "")
		if !strings.Contains(err.Error(), strconv.Quote(raw)) || strings.Contains(err.Error(), "redacted") {
			t.Fatalf("error = %q, want the untouched URL %q", err.Error(), raw)
		}
	})
}
