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

// TestDirectChunkRefusedRefreshesTheURL pins the whole-file path's URL
// refresh. A googlevideo URL lives about six hours, so a long transfer meets a
// 403 (or 410) partway through; the chunk loop used to return it after one
// attempt and the job errored. It now asks OnCredentialRefresh, installs the
// fresh URL AND token, and retries the chunk — here on a fresh URL the origin
// serves only with the fresh token.
//
// Mutant: `status == http.StatusForbidden || status == http.StatusGone` →
// `status == http.StatusForbidden` in fetchChunkWithRetry — the 410 row fails
// on its first refusal. Mutant: dropping `d.SetBaseURL(freshURL)` from
// refreshDirectURL — both rows keep fetching the expired URL. Mutant:
// dropping `d.SetPoToken(freshToken)` — the fresh URL is refused without its
// token.
func TestDirectChunkRefusedRefreshesTheURL(t *testing.T) {
	body := headedBody(2*DownloadChunkSize+100, 'V')
	for _, status := range []int{http.StatusForbidden, http.StatusGone} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			full := serveRangeFile(body)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var start int64
				fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
				switch {
				case r.URL.Path == "/expired.mp4" && start >= DownloadChunkSize:
					w.WriteHeader(status) // the URL's expire= has passed
				case r.URL.Path == "/fresh.mp4" && r.URL.Query().Get("pot") != "fresh-token":
					w.WriteHeader(http.StatusForbidden)
				default:
					full(w, r)
				}
			}))
			defer srv.Close()

			var refreshes atomic.Int32
			out := filepath.Join(t.TempDir(), "video.mp4")
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL: srv.URL + "/expired.mp4", OutputFile: out, IsDirectURL: true, PoToken: "stale-token",
				OnCredentialRefresh: func() (string, string) {
					refreshes.Add(1)
					return srv.URL + "/fresh.mp4", "fresh-token"
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
		})
	}
}

// TestDirectRefreshRefusesAnotherFile pins the identity half: the partial on
// disk is a prefix of ONE rendition, and a refresh that came back with a URL
// for another (a re-extraction whose pool moved) must not be appended. The
// error keeps the sidecar, so a Resume re-selects from scratch.
//
// Mutant: `was != now` → `false` in refreshDirectURL — itag 136's bytes are
// appended to itag 137's first chunk and Start returns nil.
func TestDirectRefreshRefusesAnotherFile(t *testing.T) {
	bodyA := headedBody(2*DownloadChunkSize+100, 'A')
	bodyB := headedBody(2*DownloadChunkSize+100, 'B')
	clen := strconv.Itoa(len(bodyA))
	serveA, serveB := serveRangeFile(bodyA), serveRangeFile(bodyB)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
		if r.URL.Query().Get("itag") == "136" {
			serveB(w, r)
			return
		}
		if start >= DownloadChunkSize {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		serveA(w, r)
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "video.mp4")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL: srv.URL + "/videoplayback?expire=1&id=o-AAAA&itag=137&clen=" + clen, OutputFile: out, IsDirectURL: true,
		OnCredentialRefresh: func() (string, string) {
			return srv.URL + "/videoplayback?expire=2&id=o-BBBB&itag=136&clen=" + clen, ""
		},
	})
	d.delays = fastDelays()
	d.directResumeIntervalOverride = DownloadChunkSize
	err := d.Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), "different stream") {
		t.Fatalf("Start = %v, want the refreshed URL refused as a different stream", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, bodyA[:DownloadChunkSize]) {
		t.Errorf("output is %d bytes with %d of itag 136 — want itag 137's first chunk alone", len(got), bytes.Count(got, []byte("B")))
	}
	if _, statErr := os.Stat(out + resumeFileSuffix); statErr != nil {
		t.Errorf("resume sidecar gone (stat err = %v); a refused refresh must leave it for a Resume", statErr)
	}
}

// TestDirectRefreshIsBounded pins that a refused chunk asks for a fresh URL a
// bounded number of times: a refresh whose URL the origin still refuses is
// retried directRefreshAttempts times in all, and one that returns nothing
// ends it at once — retrying the URL that was just refused only spends the
// attempt.
//
// Mutant: `refreshes >= directRefreshAttempts` → `false` in
// fetchChunkWithRetry — the still-refused row refreshes until the test's
// deadline. Mutant: dropping refreshDirectURL's nothing-returned check — the
// empty row refreshes twice.
func TestDirectRefreshIsBounded(t *testing.T) {
	body := headedBody(2*DownloadChunkSize+100, 'V')
	for _, tc := range []struct {
		name      string
		fresh     string
		wantCalls int32
		wantErr   string
	}{
		{"a fresh URL the origin still refuses", "/still-expired.mp4", directRefreshAttempts, "HTTP 403"},
		{"a refresh that returns nothing", "", 1, "returned nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			full := serveRangeFile(body)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var start int64
				fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
				if start >= DownloadChunkSize {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				full(w, r)
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

// outageServer serves body by Range, except that from the first request past
// the first chunk it drops every connection mid-body (206 headers, a few
// bytes, then EOF) for down. online answers like a connectivity monitor that
// calls the outage offline only over [offlineFrom, offlineUntil) of it.
type outageServer struct {
	*httptest.Server
	mu    sync.Mutex
	began time.Time
	drops atomic.Int32
}

func newOutageServer(t *testing.T, body []byte, down time.Duration) *outageServer {
	t.Helper()
	s := &outageServer{}
	full := serveRangeFile(body)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if start >= DownloadChunkSize {
			s.mu.Lock()
			if s.began.IsZero() {
				s.began = time.Now()
			}
			dropped := time.Since(s.began) < down
			s.mu.Unlock()
			if dropped {
				s.drops.Add(1)
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
				w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(body[start : start+1000]) // short: the server drops the connection
				return
			}
		}
		full(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *outageServer) online(offlineFrom, offlineUntil time.Duration) func() bool {
	return func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.began.IsZero() {
			return true
		}
		el := time.Since(s.began)
		return el < offlineFrom || el >= offlineUntil
	}
}

// TestDirectChunkWaitsOutAnOutage pins the whole-file path's connectivity
// wait. The chunk ladder is three attempts over three seconds, so an outage
// longer than that used to fail the job — and a connection dropped mid-chunk
// (a 206 whose body broke off) failed it on the first attempt, the shape a
// dead link actually takes during a transfer. Now:
//
//   - a failure while IsOnline says offline is waited out and not charged:
//     one dropped attempt, not the whole ladder;
//   - a monitor that has not yet noticed when the ladder runs out is given
//     the time it needs, and the outage it then reports is waited out;
//   - a link the monitor keeps calling up still gives up after the ladder.
//
// Mutant: `offline := !d.opts.IsOnline()` → `offline := false` — the
// already-offline row drops three attempts. Mutant: dropping
// `offline = d.awaitOutageVerdict(ctx)` — the late-monitor row fails after
// three attempts. Mutant: awaitOutageVerdict's deadline `return false` →
// `return true` — the online row never gives up. Mutant: `incomplete :=
// status < 300` → `status == 0` — the mid-body drop fails at once. Mutant:
// dropping `d.emitActivity(ActivityReconnecting)` — no Reconnecting
// activity.
func TestDirectChunkWaitsOutAnOutage(t *testing.T) {
	body := headedBody(2*DownloadChunkSize+100, 'V')
	for _, tc := range []struct {
		name                      string
		down                      time.Duration
		offlineFrom, offlineUntil time.Duration
		wantErr                   string
		maxDrops                  int32
	}{
		{"the monitor already calls it offline", 600 * time.Millisecond, 0, 600 * time.Millisecond, "", 1},
		{"the monitor notices after the ladder ran out", 900 * time.Millisecond, 300 * time.Millisecond, 900 * time.Millisecond, "", MaxChunkRetries},
		{"a link the monitor calls up gives up", time.Hour, time.Hour, time.Hour, fmt.Sprintf("after %d attempts", MaxChunkRetries), MaxChunkRetries},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newOutageServer(t, body, tc.down)
			out := filepath.Join(t.TempDir(), "video.mp4")
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL: srv.URL + "/video.mp4", OutputFile: out, IsDirectURL: true,
				IsOnline: srv.online(tc.offlineFrom, tc.offlineUntil),
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
			err := d.Start(ctx)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Start = %v, want the outage waited out", err)
				}
				if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
					t.Errorf("output is %d bytes, want the whole %d-byte file", len(got), len(body))
				}
				if !reconnecting.Load() {
					t.Error("no ActivityReconnecting while the outage was waited out")
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Start = %v, want an error naming %q", err, tc.wantErr)
			}
			if n := srv.drops.Load(); n > tc.maxDrops {
				t.Errorf("%d attempts dropped during the outage, want at most %d", n, tc.maxDrops)
			}
		})
	}
}

// TestDirectOutageVerdictHonoursCancel pins that a cancel landing while the
// chunk waits for the monitor's verdict is reported as the cancel it is. The
// wait holds the loop past its last attempt, and returning the last fetch's
// network error instead would read as a failed download — an Error row for a
// job the operator stopped.
//
// Mutant: dropping the cancelErr check after fetchChunkWithRetry's loop —
// Start returns "chunk download failed after 3 attempts: unexpected EOF".
func TestDirectOutageVerdictHonoursCancel(t *testing.T) {
	body := headedBody(2*DownloadChunkSize+100, 'V')
	srv := newOutageServer(t, body, time.Hour)
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
		// Past the three attempts (~150 ms here), inside the verdict window.
		for srv.drops.Load() < MaxChunkRetries {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	if err := d.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Start = %v, want context.Canceled", err)
	}
}
