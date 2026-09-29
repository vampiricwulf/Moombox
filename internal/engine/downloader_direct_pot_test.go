package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
