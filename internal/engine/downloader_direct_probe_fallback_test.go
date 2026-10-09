package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// dropConnection answers a request with no response at all: the connection
// is closed before a status line, the way a dead link fails a request.
func dropConnection(t *testing.T, w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Error("response writer cannot hijack")
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	conn.Close()
}

// TestDirectSizeProbeRefusedRefreshesTheURL pins the size probe's URL
// refresh. A URL that expired before the first request answers the 1-byte
// probe 403 (or 410); the probe spent its three attempts on it and handed the
// download to the streaming fallback. It now asks OnCredentialRefresh and
// probes the fresh URL, so the download runs chunked from the start.
//
// Mutant: dropping the probe's 403/410 branch from probeFileSizeWithRetry —
// the expired URL is asked three times, and the fallback streams the fresh
// one with no Range. Mutant: `status == http.StatusForbidden || status ==
// http.StatusGone` → `status == http.StatusForbidden` in that branch — the
// 410 row does the same.
func TestDirectSizeProbeRefusedRefreshesTheURL(t *testing.T) {
	body := headedBody(2*DownloadChunkSize+100, 'V')
	for _, status := range []int{http.StatusForbidden, http.StatusGone} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			full := serveRangeFile(body)
			var expiredHits, freshUnranged atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/expired.mp4":
					expiredHits.Add(1)
					w.WriteHeader(status) // the URL's expire= passed before the first request
				default:
					if r.Header.Get("Range") == "" {
						freshUnranged.Add(1)
					}
					full(w, r)
				}
			}))
			defer srv.Close()

			var refreshes atomic.Int32
			out := filepath.Join(t.TempDir(), "video.mp4")
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL: srv.URL + "/expired.mp4", OutputFile: out, IsDirectURL: true,
				OnCredentialRefresh: func() (string, string) {
					refreshes.Add(1)
					return srv.URL + "/fresh.mp4", ""
				},
			})
			d.delays = fastDelays()
			if err := d.Start(t.Context()); err != nil {
				t.Fatalf("Start = %v, want the refreshed URL to finish the file", err)
			}
			if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
				t.Errorf("output is %d bytes, want the whole %d-byte file", len(got), len(body))
			}
			if n := refreshes.Load(); n != 1 {
				t.Errorf("OnCredentialRefresh calls = %d, want 1", n)
			}
			if n := expiredHits.Load(); n != 1 {
				t.Errorf("the expired URL was asked %d times, want once", n)
			}
			if n := freshUnranged.Load(); n != 0 {
				t.Errorf("%d requests streamed the fresh URL with no Range — the download went to the fallback", n)
			}
		})
	}
}

// TestDirectSizeProbeRefreshIsBounded pins that the probe asks for a fresh
// URL a bounded number of times: one the origin still refuses ends the
// download after directRefreshAttempts refreshes, and one that returns
// nothing ends it at once — each an error that keeps the sidecar, as the
// chunked loop's own refusal is.
//
// Mutant: `refreshes >= directRefreshAttempts` → `false` in
// probeFileSizeWithRetry — the still-refused row refreshes until the test's
// deadline. Mutant: returning 0, nil instead of the refresh's error — the
// empty row goes on to the fallback, which asks for a second refresh.
func TestDirectSizeProbeRefreshIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fresh     string
		wantCalls int32
		wantErr   string
	}{
		{"a fresh URL the origin still refuses", "/still-expired.mp4", directRefreshAttempts, "size probe refused: HTTP 403"},
		{"a refresh that returns nothing", "", 1, "returned nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			}))
			defer srv.Close()

			var calls atomic.Int32
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL: srv.URL + "/expired.mp4", OutputFile: filepath.Join(t.TempDir(), "video.mp4"), IsDirectURL: true,
				OnCredentialRefresh: func() (string, string) {
					calls.Add(1)
					if tc.fresh == "" {
						return "", ""
					}
					return srv.URL + tc.fresh, ""
				},
			})
			d.delays = fastDelays()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := d.Start(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Start = %v, want an error naming %q", err, tc.wantErr)
			}
			if n := calls.Load(); n != tc.wantCalls {
				t.Errorf("OnCredentialRefresh calls = %d, want %d", n, tc.wantCalls)
			}
		})
	}
}

// TestDirectSizeProbeWaitsOutAnOutage pins the probe's connectivity wait. An
// outage as the download starts failed the three probes inside the outage
// and sent the download to the streaming fallback. A probe that gets no
// answer, or a 5xx, while IsOnline says offline — or one with no answer on
// its last attempt, once the monitor has been given the time to say so — now
// waits the outage out and probes again, so the download runs chunked. The
// 5xx is a gateway answering for an origin it cannot reach.
//
// Mutant: dropping `offline = d.awaitOutageVerdict(ctx)` from
// probeFileSizeWithRetry — the late-monitor row goes to the fallback.
// Mutant: dropping `d.emitActivity(ActivityReconnecting)` — no Reconnecting
// activity. Mutant: `status == 0 || status >= 500` → `status == 0` — the
// gateway row spends its three probes on the 503s and goes to the fallback.
// (`offline := !d.opts.IsOnline()` → `false` is caught here only by the
// verdict wait taking over on the last attempt;
// TestDirectSizeProbeChargesNeitherARefreshNorAnOutage pins it.)
func TestDirectSizeProbeWaitsOutAnOutage(t *testing.T) {
	body := headedBody(2*DownloadChunkSize+100, 'V')
	for _, tc := range []struct {
		name                      string
		gateway                   bool // the outage answers 503, not nothing
		down                      time.Duration
		offlineFrom, offlineUntil time.Duration
	}{
		{"the monitor already calls it offline", false, time.Second, 0, time.Second},
		{"the monitor notices on the last attempt", false, 1200 * time.Millisecond, 500 * time.Millisecond, 1200 * time.Millisecond},
		{"a gateway's 503s while the monitor calls it offline", true, time.Second, 0, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			full := serveRangeFile(body)
			var (
				mu        sync.Mutex
				began     time.Time
				unranged  atomic.Int32
				elapsedAt = func() time.Duration {
					mu.Lock()
					defer mu.Unlock()
					if began.IsZero() {
						began = time.Now()
					}
					return time.Since(began)
				}
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if elapsedAt() < tc.down {
					if tc.gateway {
						w.WriteHeader(http.StatusServiceUnavailable)
					} else {
						dropConnection(t, w)
					}
					return
				}
				if r.Header.Get("Range") == "" {
					unranged.Add(1)
				}
				full(w, r)
			}))
			defer srv.Close()

			out := filepath.Join(t.TempDir(), "video.mp4")
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL: srv.URL + "/video.mp4", OutputFile: out, IsDirectURL: true,
				IsOnline: func() bool {
					el := elapsedAt()
					return el < tc.offlineFrom || el >= tc.offlineUntil
				},
			})
			d.delays = fastDelays()
			var reconnecting atomic.Bool
			d.OnActivity = func(a DownloadActivity) {
				if a == ActivityReconnecting {
					reconnecting.Store(true)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err := d.Start(ctx); err != nil {
				t.Fatalf("Start = %v, want the outage waited out", err)
			}
			if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
				t.Errorf("output is %d bytes, want the whole %d-byte file", len(got), len(body))
			}
			if n := unranged.Load(); n != 0 {
				t.Errorf("%d requests streamed the file with no Range — the download went to the fallback", n)
			}
			if !reconnecting.Load() {
				t.Error("no ActivityReconnecting while the outage was waited out")
			}
		})
	}
}

// fallbackOrigin answers every size probe 500, so a download goes to the
// streaming fallback, and serves body by Range otherwise — except as next
// says, request by request (1-based, size probes not counted).
type fallbackOrigin struct {
	*httptest.Server
	mu       sync.Mutex
	requests int
	broke    time.Time // when the first answer that failed the link was given
}

func newFallbackOrigin(t *testing.T, body []byte, next func(n int, path string, w http.ResponseWriter, r *http.Request) bool) *fallbackOrigin {
	t.Helper()
	o := &fallbackOrigin{}
	full := serveRangeFile(body)
	o.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		o.mu.Lock()
		o.requests++
		n := o.requests
		o.mu.Unlock()
		if next(n, r.URL.Path, w, r) {
			return
		}
		full(w, r)
	}))
	t.Cleanup(o.Close)
	return o
}

// markBroken records that the link failed now, for online's window.
func (o *fallbackOrigin) markBroken() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.broke.IsZero() {
		o.broke = time.Now()
	}
}

// online answers like a connectivity monitor that calls the outage offline
// only over [from, until) of it, measured from the first broken answer.
func (o *fallbackOrigin) online(from, until time.Duration) func() bool {
	return func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.broke.IsZero() {
			return true
		}
		el := time.Since(o.broke)
		return el < from || el >= until
	}
}

// brokenBody answers 200 with the whole file's length and sends only its
// first cut bytes: the body breaks off mid-read.
func brokenBody(w http.ResponseWriter, body []byte, cut int) {
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body[:cut])
}

// TestDirectFallbackRefusedRefreshesTheURL pins the streaming fallback's URL
// refresh. A probe that failed — or a mid-download 200 — hands the download
// to the fallback, whose one request ended the job on a 403 when the URL had
// expired on the way. It now asks OnCredentialRefresh and streams the fresh
// URL.
//
// Mutant: dropping the fallback's 403/410 case — "HTTP 403 downloading
// direct URL". Mutant: `status == http.StatusForbidden || status ==
// http.StatusGone` → `status == http.StatusForbidden` — the 410 row fails.
func TestDirectFallbackRefusedRefreshesTheURL(t *testing.T) {
	body := headedBody(DownloadChunkSize+100, 'V')
	for _, status := range []int{http.StatusForbidden, http.StatusGone} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			o := newFallbackOrigin(t, body, func(_ int, path string, w http.ResponseWriter, _ *http.Request) bool {
				if path == "/expired.mp4" {
					w.WriteHeader(status)
					return true
				}
				return false
			})
			var refreshes atomic.Int32
			out := filepath.Join(t.TempDir(), "video.mp4")
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL: o.URL + "/expired.mp4", OutputFile: out, IsDirectURL: true,
				OnCredentialRefresh: func() (string, string) {
					refreshes.Add(1)
					return o.URL + "/fresh.mp4", ""
				},
			})
			d.delays = fastDelays()
			if err := d.Start(t.Context()); err != nil {
				t.Fatalf("Start = %v, want the refreshed URL streamed", err)
			}
			if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
				t.Errorf("output is %d bytes, want the whole %d-byte file", len(got), len(body))
			}
			if n := refreshes.Load(); n != 1 {
				t.Errorf("OnCredentialRefresh calls = %d, want 1", n)
			}
		})
	}
}

// TestDirectFallbackRefreshIsBounded pins the fallback's refresh bound: at
// most directRefreshAttempts refreshes without a byte written between them.
// A fresh URL the origin still refuses ends the download after
// that many; a refusal after the transfer moved is a new expiry and starts
// the count again, so a stream long enough to outlive two URLs is not cut
// off — whether it moved through a body that broke off or through 206s that
// ended short of the file and were asked for the rest (errDirectRestToCome).
//
// Mutant: `refreshes >= directRefreshAttempts` → `false` — the still-refused
// row refreshes until the test's deadline. Mutant: dropping `refreshes = 0`
// — the progress rows stop on the refusal after the transfer moved. Mutant:
// moving the reset below the errDirectRestToCome `continue`, where it was —
// both short-round rows stop on a 403 the budget should have covered.
func TestDirectFallbackRefreshIsBounded(t *testing.T) {
	body := headedBody(DownloadChunkSize+100, 'V')

	t.Run("a fresh URL the origin still refuses", func(t *testing.T) {
		o := newFallbackOrigin(t, body, func(_ int, _ string, w http.ResponseWriter, _ *http.Request) bool {
			w.WriteHeader(http.StatusForbidden)
			return true
		})
		var calls atomic.Int32
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL: o.URL + "/expired.mp4", OutputFile: filepath.Join(t.TempDir(), "video.mp4"), IsDirectURL: true,
			OnCredentialRefresh: func() (string, string) {
				calls.Add(1)
				return o.URL + "/still-expired.mp4", ""
			},
		})
		d.delays = fastDelays()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := d.Start(ctx); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
			t.Errorf("Start = %v, want the 403 the refreshed URL still got", err)
		}
		if n := calls.Load(); n != directRefreshAttempts {
			t.Errorf("OnCredentialRefresh calls = %d, want %d", n, directRefreshAttempts)
		}
	})

	t.Run("a refusal after progress counts again", func(t *testing.T) {
		// /u0 is refused; /u1 breaks off after 1000 bytes (an outage the
		// monitor calls), then is refused; /u2 is refused; /u3 serves.
		var o *fallbackOrigin
		o = newFallbackOrigin(t, body, func(n int, path string, w http.ResponseWriter, _ *http.Request) bool {
			switch {
			case path == "/u1" && n == 2:
				o.markBroken()
				brokenBody(w, body, 1000)
				return true
			case path == "/u3":
				return false
			}
			w.WriteHeader(http.StatusForbidden)
			return true
		})
		var calls atomic.Int32
		out := filepath.Join(t.TempDir(), "video.mp4")
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL: o.URL + "/u0", OutputFile: out, IsDirectURL: true,
			IsOnline: o.online(0, 300*time.Millisecond),
			OnCredentialRefresh: func() (string, string) {
				return o.URL + "/u" + strconv.Itoa(int(calls.Add(1))), ""
			},
		})
		d.delays = fastDelays()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if err := d.Start(ctx); err != nil {
			t.Fatalf("Start = %v, want the third URL's refusal refreshed past", err)
		}
		if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
			t.Errorf("output is %d bytes, want the whole %d-byte file", len(got), len(body))
		}
		if n := calls.Load(); n != 3 {
			t.Errorf("OnCredentialRefresh calls = %d, want 3", n)
		}
	})

	// shortRound answers a Range with a 206 of the next 1000 bytes that
	// states the whole file's total: a round that ends short of the file.
	shortRound := func(w http.ResponseWriter, r *http.Request) {
		var start int
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+999, len(body)))
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[start : start+1000])
	}

	for _, tc := range []struct {
		name string
		// serve answers a request for path; false lets the origin serve the
		// rest of the file. hits counts the requests path has had before.
		serve     func(path string, hits int, w http.ResponseWriter, r *http.Request) bool
		wantCalls int32
	}{
		{
			// /u0 is refused; /u1 serves one short round, then is refused;
			// /u2 is refused; /u3 serves the rest: two refreshes after the
			// round, as many as directRefreshAttempts allows.
			"a refusal after a short round gets the whole budget",
			func(path string, hits int, w http.ResponseWriter, r *http.Request) bool {
				switch {
				case path == "/u3":
					return false
				case path == "/u1" && hits == 0:
					shortRound(w, r)
					return true
				}
				w.WriteHeader(http.StatusForbidden)
				return true
			},
			3,
		},
		{
			// Every URL serves one short round and then expires: three
			// expiries, each after the file moved on.
			"three expiries with short rounds between",
			func(path string, hits int, w http.ResponseWriter, r *http.Request) bool {
				switch {
				case path == "/u3":
					return false
				case hits == 0:
					shortRound(w, r)
					return true
				}
				w.WriteHeader(http.StatusForbidden)
				return true
			},
			3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			hits := map[string]int{}
			o := newFallbackOrigin(t, body, func(_ int, path string, w http.ResponseWriter, r *http.Request) bool {
				mu.Lock()
				n := hits[path]
				hits[path]++
				mu.Unlock()
				return tc.serve(path, n, w, r)
			})
			var calls atomic.Int32
			out := filepath.Join(t.TempDir(), "video.mp4")
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL: o.URL + "/u0", OutputFile: out, IsDirectURL: true,
				OnCredentialRefresh: func() (string, string) {
					return o.URL + "/u" + strconv.Itoa(int(calls.Add(1))), ""
				},
			})
			d.delays = fastDelays()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err := d.Start(ctx); err != nil {
				t.Fatalf("Start = %v, want every refusal after a short round refreshed past", err)
			}
			if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
				t.Errorf("output is %d bytes, want the whole %d-byte file", len(got), len(body))
			}
			if n := calls.Load(); n != tc.wantCalls {
				t.Errorf("OnCredentialRefresh calls = %d, want %d", n, tc.wantCalls)
			}
		})
	}
}

// TestDirectFallbackShortHeadFromByteZeroEnds pins that a 206 the fallback
// asks for the rest of must have taken the file past the byte it was asked
// from. An origin that answers every Range with a short head of the file
// labelled from byte 0 discards the partial (streamDirectOnce) and writes
// the head again; that moved the byte counter, and was read as a round that
// moved the file on, so the fallback asked again without end — thousands of
// requests a second. It is now the short origin, an error.
//
// Mutant: `staged <= offset` → `staged == offset` — the heads alternate in
// length, so no request lands exactly on its offset and the fallback asks
// until the test's deadline. Mutant: dropping the arm — the same.
func TestDirectFallbackShortHeadFromByteZeroEnds(t *testing.T) {
	body := headedBody(DownloadChunkSize+100, 'V')
	o := newFallbackOrigin(t, body, func(n int, _ string, w http.ResponseWriter, _ *http.Request) bool {
		head := 1000 // odd requests; even ones get half as much
		if n%2 == 0 {
			head = 500
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", head-1, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(head))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[:head])
		return true
	})
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL: o.URL + "/video.mp4", OutputFile: filepath.Join(t.TempDir(), "video.mp4"), IsDirectURL: true,
	})
	d.delays = fastDelays()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := d.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "ends short of its probed size") {
		t.Errorf("Start = %v, want the short origin's error", err)
	}
	o.mu.Lock()
	n := o.requests
	o.mu.Unlock()
	if n != 2 {
		t.Errorf("the fallback made %d requests, want 2: the head, and the answer from byte 0 that got no further", n)
	}
}

// brokenRest answers a request's Range with a 206 for the rest of the file
// whose body breaks off before its first byte: a request that gets no
// complete answer and moves nothing.
func brokenRest(w http.ResponseWriter, r *http.Request, body []byte) {
	var start int
	fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)-start))
	w.WriteHeader(http.StatusPartialContent)
}

// TestDirectFallbackWaitsOutAnOutage pins the streaming fallback's
// connectivity wait. Its one request ended the job on any failure, so a
// connection that dropped partway through a stream — or an outage as it
// started — lost the job however much had streamed. A request that gets no
// complete answer while IsOnline says offline, or once the monitor has been
// given the time to say so, now waits the outage out and asks again from
// where the file stands. The first request breaks off after 1000 bytes (or,
// in the no-answer row, gets none); every request after it fails with
// nothing for as long as each row's link is down.
//
// Mutant: dropping the fallback's offline wait — the rows the monitor calls
// offline spend their attempts inside the outage and fail. Mutant: returning
// false for linkFailed from streamDirectOnce's broken read — the broken-body
// rows fail on their first break. Mutant: returning false for it from the
// failed request — the no-answer row fails at once. Mutant: dropping the
// awaitOutageVerdict — the late-monitor row's third failure, inside the
// outage, ends it. Mutant: dropping the emitActivity(ActivityReconnecting) —
// no Reconnecting activity.
func TestDirectFallbackWaitsOutAnOutage(t *testing.T) {
	body := headedBody(DownloadChunkSize+100, 'V')
	for _, tc := range []struct {
		name        string
		noAnswer    bool
		down        time.Duration // how long every request fails, from the first
		from, until time.Duration // when the monitor calls it offline
	}{
		{"no answer, the monitor already calls it offline", true, 300 * time.Millisecond, 0, 300 * time.Millisecond},
		{"a body that breaks off, the monitor already calls it offline", false, 300 * time.Millisecond, 0, 300 * time.Millisecond},
		// Three failures by ~150 ms; the verdict window after the third
		// covers the monitor's call at 400 ms.
		{"a body that breaks off, the monitor notices late", false, 700 * time.Millisecond, 400 * time.Millisecond, 700 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var o *fallbackOrigin
			var resumedAt atomic.Value
			o = newFallbackOrigin(t, body, func(n int, _ string, w http.ResponseWriter, r *http.Request) bool {
				switch {
				case n > 1 && o.online(0, tc.down)():
					resumedAt.Store(r.Header.Get("Range"))
					return false
				case tc.noAnswer:
					// Every request for the length of the outage: the
					// transport retries one dropped on a reused connection.
					o.markBroken()
					dropConnection(t, w)
				case n == 1:
					o.markBroken()
					brokenBody(w, body, 1000)
				default:
					brokenRest(w, r, body)
				}
				return true
			})
			out := filepath.Join(t.TempDir(), "video.mp4")
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL: o.URL + "/video.mp4", OutputFile: out, IsDirectURL: true,
				IsOnline: o.online(tc.from, tc.until),
			})
			d.delays = fastDelays()
			var reconnecting atomic.Bool
			d.OnActivity = func(a DownloadActivity) {
				if a == ActivityReconnecting {
					reconnecting.Store(true)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err := d.Start(ctx); err != nil {
				t.Fatalf("Start = %v, want the outage waited out", err)
			}
			if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
				t.Errorf("output is %d bytes, want the whole %d-byte file", len(got), len(body))
			}
			if !reconnecting.Load() {
				t.Error("no ActivityReconnecting while the outage was waited out")
			}
			if !tc.noAnswer {
				if got, _ := resumedAt.Load().(string); got != fmt.Sprintf("bytes=%d-", 1000) {
					t.Errorf("the request after the outage sent Range %q, want it to resume at byte 1000", got)
				}
			}
		})
	}
}

// TestDirectFallbackRetriesWhatTheMonitorDoesNotCall pins the fallback's own
// attempts, the chunked loop's MaxChunkRetries ladder: a 5xx, or a request
// with no complete answer that the monitor does not call an outage, is asked
// again, and only MaxChunkRetries of them without the file getting further
// between them end the download. One connection reset ended a multi-GB
// stream on the spot while the monitor called the link up, the resume Range
// that makes a retry free notwithstanding; a 5xx ended it at once, and so
// did a 5xx a gateway answered for an unreachable origin while the device
// was offline, which is now waited out instead.
//
// Mutant: `linkFailed || status >= 500` → `linkFailed` — the 503 rows fail
// at once. Mutant: `failures >= MaxChunkRetries` → `failures >= 1`, the
// answer at once it used to give — every retried row fails. Mutant: `failures >= MaxChunkRetries` → `false`
// — the rows that must end ask until the test's deadline. Mutant: dropping
// `failures = 0` — the row that moves on each break stops at its third.
// Mutant: `staged > furthest` measured from the request's own start (a
// `furthest = d.bytesWritten.Load()` before it) — the restarting origin's
// row, whose 200s rewrite the file from byte 0 to alternating lengths, asks
// until the deadline. Mutant: guarding the retry on IsOnline != nil — the
// no-monitor row fails on its break.
func TestDirectFallbackRetriesWhatTheMonitorDoesNotCall(t *testing.T) {
	body := headedBody(DownloadChunkSize+100, 'V')
	up := func() bool { return true }
	for _, tc := range []struct {
		name     string
		isOnline func(o *fallbackOrigin) func() bool
		// fail answers request n (1-based) in its place, or reports false to
		// let the origin serve it.
		fail      func(o *fallbackOrigin, n int, w http.ResponseWriter, r *http.Request) bool
		wantErr   string // "" — the download finishes
		wantAsked int    // requests made, 0 — not checked
	}{
		{
			"one reset, the monitor calls the link up",
			func(*fallbackOrigin) func() bool { return up },
			func(_ *fallbackOrigin, n int, w http.ResponseWriter, _ *http.Request) bool {
				if n == 1 {
					brokenBody(w, body, 1000)
				}
				return n == 1
			},
			"", 2,
		},
		{
			"one reset, no monitor",
			func(*fallbackOrigin) func() bool { return nil },
			func(_ *fallbackOrigin, n int, w http.ResponseWriter, _ *http.Request) bool {
				if n == 1 {
					brokenBody(w, body, 1000)
				}
				return n == 1
			},
			"", 2,
		},
		{
			"two 503s, the monitor calls the link up",
			func(*fallbackOrigin) func() bool { return up },
			func(_ *fallbackOrigin, n int, w http.ResponseWriter, _ *http.Request) bool {
				if n <= 2 {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
				return n <= 2
			},
			"", 3,
		},
		{
			"503s while the monitor calls it offline",
			func(o *fallbackOrigin) func() bool { return o.online(0, 300*time.Millisecond) },
			func(o *fallbackOrigin, n int, w http.ResponseWriter, _ *http.Request) bool {
				if n == 1 {
					o.markBroken()
				}
				if o.online(0, 300*time.Millisecond)() {
					return false
				}
				w.WriteHeader(http.StatusServiceUnavailable) // the gateway, its upstream unreachable
				return true
			},
			"", 0,
		},
		{
			"a break after each 1000 bytes, five times",
			func(*fallbackOrigin) func() bool { return up },
			func(_ *fallbackOrigin, n int, w http.ResponseWriter, r *http.Request) bool {
				if n > 5 {
					return false
				}
				var start int
				fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
				w.Header().Set("Content-Length", strconv.Itoa(len(body)-start))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(body[start : start+1000])
				return true
			},
			"", 6,
		},
		{
			"503 throughout",
			func(*fallbackOrigin) func() bool { return up },
			func(_ *fallbackOrigin, _ int, w http.ResponseWriter, _ *http.Request) bool {
				w.WriteHeader(http.StatusServiceUnavailable)
				return true
			},
			"HTTP 503", MaxChunkRetries,
		},
		{
			"a body that breaks off with nothing, the monitor calls the link up",
			func(*fallbackOrigin) func() bool { return up },
			func(_ *fallbackOrigin, n int, w http.ResponseWriter, r *http.Request) bool {
				if n == 1 {
					brokenBody(w, body, 1000)
				} else {
					brokenRest(w, r, body)
				}
				return true
			},
			"unexpected EOF", MaxChunkRetries,
		},
		{
			// A 200 to every Range: each answer discards the partial and
			// breaks off again, at 3000 bytes and 2000 by turns — never
			// further than the file has stood.
			"an origin that restarts from byte 0 and breaks off",
			func(*fallbackOrigin) func() bool { return up },
			func(_ *fallbackOrigin, n int, w http.ResponseWriter, _ *http.Request) bool {
				brokenBody(w, body, 2000+1000*(n%2))
				return true
			},
			"unexpected EOF", MaxChunkRetries,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var o *fallbackOrigin
			o = newFallbackOrigin(t, body, func(n int, _ string, w http.ResponseWriter, r *http.Request) bool {
				return tc.fail(o, n, w, r)
			})
			out := filepath.Join(t.TempDir(), "video.mp4")
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL: o.URL + "/video.mp4", OutputFile: out, IsDirectURL: true,
				IsOnline: tc.isOnline(o),
			})
			d.delays = fastDelays()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			err := d.Start(ctx)
			o.mu.Lock()
			asked := o.requests
			o.mu.Unlock()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("Start = %v, want an error naming %q", err, tc.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("Start = %v, want the failure asked past", err)
				}
				if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
					t.Errorf("output is %d bytes, want the whole %d-byte file", len(got), len(body))
				}
			}
			if tc.wantAsked != 0 && asked != tc.wantAsked {
				t.Errorf("the fallback made %d requests, want %d", asked, tc.wantAsked)
			}
		})
	}
}

// TestDirectFallbackOutageVerdictHonoursCancel pins that a cancel landing
// while the fallback waits for the monitor's verdict is reported as the
// cancel it is, not as the broken stream's error — an Error row for a job
// the operator stopped. Every request breaks off with nothing after the
// first, so the third is the last attempt and asks for the verdict.
//
// Mutant: dropping the cancelErr check before a failure is counted — Start
// returns the broken read's error.
func TestDirectFallbackOutageVerdictHonoursCancel(t *testing.T) {
	body := headedBody(DownloadChunkSize+100, 'V')
	broke := make(chan struct{})
	o := newFallbackOrigin(t, body, func(n int, _ string, w http.ResponseWriter, r *http.Request) bool {
		if n == 1 {
			brokenBody(w, body, 1000)
		} else {
			brokenRest(w, r, body)
		}
		if n == MaxChunkRetries {
			close(broke)
		}
		return true
	})
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL: o.URL + "/video.mp4", OutputFile: filepath.Join(t.TempDir(), "video.mp4"), IsDirectURL: true,
		IsOnline: func() bool { return true },
	})
	d.delays = fastDelays()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("cancel goroutine panicked: %v", r)
			}
		}()
		<-broke
		time.Sleep(200 * time.Millisecond) // inside the ~750 ms verdict window
		cancel()
	}()
	if err := d.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Start = %v, want context.Canceled", err)
	}
}

// TestDirectSizeProbeChargesNeitherARefreshNorAnOutage pins that the probe's
// three attempts are spent on answers about Range support alone: a refusal
// the refresh answers, and a failure waited out as an outage, each leave the
// count where it was, as on the chunked loop. Each row's origin then fails
// two probes more before it answers — the attempts left only when neither
// was charged.
//
// Mutant: dropping the `i--` after the refresh — the refused row runs out of
// attempts and streams the file with no Range. Mutant: dropping it after the
// outage wait — the outage row does. Mutant: `offline := !d.opts.IsOnline()`
// → `offline := false` — the outage row's first failure, not its last
// attempt, is charged, and the row runs out the same way.
func TestDirectSizeProbeChargesNeitherARefreshNorAnOutage(t *testing.T) {
	body := headedBody(2*DownloadChunkSize+100, 'V')
	for _, tc := range []struct {
		name  string
		first func(t *testing.T, w http.ResponseWriter)
		down  time.Duration // how long the monitor calls it offline after the first probe
	}{
		{"a refusal the refresh answers", func(_ *testing.T, w http.ResponseWriter) { w.WriteHeader(http.StatusForbidden) }, 0},
		{"an outage waited out", dropConnection, 300 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			full := serveRangeFile(body)
			var probes, unranged atomic.Int32
			var firstAt atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") == "bytes=0-0" {
					switch n := probes.Add(1); {
					case n == 1:
						firstAt.Store(time.Now().UnixNano())
						tc.first(t, w)
						return
					case n <= 3:
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
				}
				if r.Header.Get("Range") == "" {
					unranged.Add(1)
				}
				full(w, r)
			}))
			defer srv.Close()

			out := filepath.Join(t.TempDir(), "video.mp4")
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL: srv.URL + "/video.mp4", OutputFile: out, IsDirectURL: true,
				IsOnline: func() bool {
					at := firstAt.Load()
					return at == 0 || time.Since(time.Unix(0, at)) >= tc.down
				},
				OnCredentialRefresh: func() (string, string) { return srv.URL + "/fresh.mp4", "" },
			})
			d.delays = fastDelays()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err := d.Start(ctx); err != nil {
				t.Fatalf("Start = %v", err)
			}
			if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
				t.Errorf("output is %d bytes, want the whole %d-byte file", len(got), len(body))
			}
			if n := unranged.Load(); n != 0 {
				t.Errorf("%d requests streamed the file with no Range — the probe ran out of attempts", n)
			}
		})
	}
}

// TestDirectSizeProbeOutageVerdictHonoursCancel pins that a cancel landing
// while the probe's last attempt waits for the monitor's verdict comes back
// as the cancel, not as "no size" — which would send a stopped download on to
// the fallback.
//
// Mutant: `return 0, d.cancelErr(ctx)` after the loop → `return 0, nil`.
func TestDirectSizeProbeOutageVerdictHonoursCancel(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes.Add(1)
		dropConnection(t, w)
	}))
	defer srv.Close()
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL: srv.URL + "/video.mp4", OutputFile: filepath.Join(t.TempDir(), "video.mp4"), IsDirectURL: true,
		IsOnline: func() bool { return true },
	})
	d.delays = fastDelays()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("cancel goroutine panicked: %v", r)
			}
		}()
		// Past the three probes (~300 ms here), inside the ~750 ms verdict
		// window that follows the last.
		for probes.Load() < 3 {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	if size, err := d.probeFileSizeWithRetry(ctx); size != 0 || !errors.Is(err, context.Canceled) {
		t.Errorf("probeFileSizeWithRetry = %d, %v; want 0 and context.Canceled", size, err)
	}
}
