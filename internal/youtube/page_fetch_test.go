package youtube

import (
	"context"
	"errors"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/httpx/httpxtest"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// watchPageFixture is the smallest watch page the extractors read something
// off: a ytcfg naming a player and its STS, and visitor data.
const watchPageFixture = `<html><script>ytcfg.set({"STS":20381,"VISITOR_DATA":"x","visitorData":"vd-page",` +
	`"PLAYER_JS_URL":"\/s\/player\/0a1b2c3d\/player_es6.vflset\/en_US\/base.js"});</script></html>`

// pageArrivals records when each page request reached the handler, on the
// bubble's clock, by path.
type pageArrivals struct {
	mu    sync.Mutex
	start time.Time
	at    map[string][]time.Duration
}

func (a *pageArrivals) note(path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.at == nil {
		a.at = map[string][]time.Duration{}
	}
	a.at[path] = append(a.at[path], time.Since(a.start))
	return len(a.at[path])
}

func (a *pageArrivals) of(path string) []time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]time.Duration(nil), a.at[path]...)
}

// servePagesInBubble points fetchYouTubePage (pageGet) at h, in-process, so
// the page fetch and its backoff run on the bubble's fake clock. dialFails
// makes the first n attempts fail as a refused dial would, before reaching h.
func servePagesInBubble(t *testing.T, h http.Handler, dialFails int) {
	t.Helper()
	client := httpxtest.NewClient(h)
	var mu sync.Mutex
	orig := pageGet
	pageGet = func(parent context.Context, url string, timeout time.Duration, headers map[string]string) (*http.Response, context.CancelFunc, error) {
		mu.Lock()
		fail := dialFails > 0
		dialFails--
		mu.Unlock()
		if fail {
			return nil, nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
		}
		ctx, cancel := context.WithTimeout(parent, timeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			return nil, nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			return nil, nil, err
		}
		return resp, cancel, nil
	}
	t.Cleanup(func() { pageGet = orig })
}

// TestWatchPageFetchRetriesTransientFailures is G13's port of yt-dlp's
// _download_webpage_with_retries (_base.py:1001-1011) for the watch page: a
// transport error, a 5xx or any status but 403/429 gets another attempt, up
// to three retries, 1 s, 2 s, 4 s apart. The page that finally answers is
// parsed as any other.
//
// Mutants: fetchYouTubePage making one attempt (every row but "answers at
// once" fails); pageRetryable refusing every error (same); the backoff
// dropped or reordered (the arrival times differ); a status error treated as
// a transport one or the reverse (unchanged here — see the refusal test).
func TestWatchPageFetchRetriesTransientFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statuses  []int // answered in turn, then 200
		dialFails int
		wantAt    []time.Duration
	}{
		{name: "answers at once", wantAt: []time.Duration{0}},
		{name: "500, 500, 200", statuses: []int{500, 500}, wantAt: []time.Duration{0, time.Second, 3 * time.Second}},
		{name: "503, 404, 502, 200", statuses: []int{503, 404, 502}, wantAt: []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second}},
		// The refused dials never reach the handler; only the attempt that
		// does is recorded, after both waits.
		{name: "two refused dials, then 200", dialFails: 2, wantAt: []time.Duration{3 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				arrivals := &pageArrivals{start: time.Now()}
				servePagesInBubble(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n := arrivals.note(r.URL.Path)
					if n <= len(tc.statuses) {
						http.Error(w, "blip", tc.statuses[n-1])
						return
					}
					_, _ = w.Write([]byte(watchPageFixture))
				}), tc.dialFails)

				wp, err := FetchWatchPage(context.Background(), "test1234567", "")
				if err != nil {
					t.Fatalf("FetchWatchPage: %v — a transient failure must be retried", err)
				}
				if got := arrivals.of("/watch"); !reflect.DeepEqual(got, tc.wantAt) {
					t.Errorf("watch page requests arrived at %v, want %v", got, tc.wantAt)
				}
				if wp.Ytcfg.VisitorData != "vd-page" {
					t.Errorf("ytcfg = %+v, want the page's visitor data", wp.Ytcfg)
				}
			})
		})
	}
}

// TestWatchPageFetchDoesNotRetryARefusal: a 403 or a 429 is YouTube refusing
// this session, and upstream gives up on it at once (retry_on_status
// defaults to (403, 429)); so does the consent interstitial, which the next
// attempt would reach with the same cookies. One request each, and the
// error the caller has always seen.
//
// Mutants: pageRetryable retrying 403, 429 or the consent wall (two or more
// requests); the consent check dropped (the wall reads as a page).
func TestWatchPageFetchDoesNotRetryARefusal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		consent bool
		wantErr string
	}{
		{name: "403", status: http.StatusForbidden, wantErr: "failed to fetch watch page: HTTP 403 from "},
		{name: "429", status: http.StatusTooManyRequests, wantErr: "failed to fetch watch page: HTTP 429 from "},
		{name: "consent wall", consent: true, wantErr: "watch page redirected to consent wall (consent.youtube.com)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				arrivals := &pageArrivals{start: time.Now()}
				servePagesInBubble(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					arrivals.note(r.URL.Path)
					switch {
					case r.Host == "consent.youtube.com":
						_, _ = w.Write([]byte(`<html>Before you continue to YouTube</html>`))
					case tc.consent:
						http.Redirect(w, r, "https://consent.youtube.com/m?continue=x", http.StatusFound)
					default:
						http.Error(w, "refused", tc.status)
					}
				}), 0)

				_, err := FetchWatchPage(context.Background(), "test1234567", "")
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("FetchWatchPage err = %v, want %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), "attempts)") {
					t.Errorf("err = %v — a refusal is not retried, so it names no attempt count", err)
				}
				if got := arrivals.of("/watch"); len(got) != 1 {
					t.Errorf("the watch page was asked %d times (%v), want once", len(got), got)
				}
			})
		})
	}
}

// TestWatchPageFetchGivesUpWithinItsBudget: four failures are the end, and
// the error says so; a caller's deadline cuts the ladder short of a wait it
// cannot afford, returning the page's own error rather than the deadline;
// and a cancel ends the wait with the cancel.
//
// Mutants: the attempt count past 4 (the first row's count); the deadline
// guard dropped (the second row sleeps to its deadline — a third request at
// 3 s, or context.DeadlineExceeded); retryExitErr bypassed (the third row
// reports the 503, or the second the deadline).
func TestWatchPageFetchGivesUpWithinItsBudget(t *testing.T) {
	failing := func(arrivals *pageArrivals) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			arrivals.note(r.URL.Path)
			http.Error(w, "down", http.StatusServiceUnavailable)
		})
	}

	t.Run("four attempts", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			arrivals := &pageArrivals{start: time.Now()}
			servePagesInBubble(t, failing(arrivals), 0)
			_, err := FetchWatchPage(context.Background(), "test1234567", "")
			if err == nil || !strings.Contains(err.Error(), "HTTP 503") || !strings.HasSuffix(err.Error(), "(4 attempts)") {
				t.Fatalf("err = %v, want the last 503 and the attempt count", err)
			}
			if want := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second}; !reflect.DeepEqual(arrivals.of("/watch"), want) {
				t.Errorf("requests arrived at %v, want %v", arrivals.of("/watch"), want)
			}
		})
	})

	t.Run("a deadline the next wait would outlast", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			arrivals := &pageArrivals{start: time.Now()}
			servePagesInBubble(t, failing(arrivals), 0)
			// 2.5 s: the 1 s wait leaves 1.5 s, more than a base for the
			// attempt after it; the 2 s wait would not leave one.
			ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
			defer cancel()
			_, err := FetchWatchPage(ctx, "test1234567", "")
			if err == nil || !strings.Contains(err.Error(), "HTTP 503") || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want the page's 503, not the deadline", err)
			}
			if want := []time.Duration{0, time.Second}; !reflect.DeepEqual(arrivals.of("/watch"), want) {
				t.Errorf("requests arrived at %v, want %v", arrivals.of("/watch"), want)
			}
			if time.Since(arrivals.start) != time.Second {
				t.Errorf("returned after %v, want at once after the second answer (1s), not at the deadline", time.Since(arrivals.start))
			}
		})
	})

	t.Run("a cancel during the wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			arrivals := &pageArrivals{start: time.Now()}
			servePagesInBubble(t, failing(arrivals), 0)
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(500*time.Millisecond, cancel)
			_, err := FetchWatchPage(ctx, "test1234567", "")
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled — a cancel is the caller's verdict, not a 503", err)
			}
			if got := arrivals.of("/watch"); len(got) != 1 {
				t.Errorf("requests %v, want the one before the cancel", got)
			}
		})
	})
}

// TestPageGetIsTheSharedFetch pins the seam the retry tests replace: in
// production the page fetch is utils.FetchWithTimeout, the shared transport
// with its connectivity reporting and its no-cookie-jar rule.
//
// Mutant: pageGet initialised to anything else.
func TestPageGetIsTheSharedFetch(t *testing.T) {
	if reflect.ValueOf(pageGet).Pointer() != reflect.ValueOf(utils.FetchWithTimeout).Pointer() {
		t.Error("pageGet is not utils.FetchWithTimeout")
	}
}

// TestFailedWatchPageKeepsTheCachedIdentity is G13's other half: a watch
// page that could not be had no longer sends the extraction out as nobody.
// Every player call carries the Service's cached visitor data, and on the
// authenticated path the session the last page under the jar's account
// stated — X-Goog-AuthUser and X-Goog-PageId, without which a brand
// account's members-only stream is judged as the primary account and
// refused. The session half goes beyond upstream (yt-dlp loses it too). The
// fallback is not captured back as if a page had stated it.
//
// Mutants: fallbackYtcfg returning DefaultYtcfg (no visitor data, no
// session); the public path seeding the session; the authenticated path not
// seeding it; captureVisitorData run on the fallback (OnVisitorData fires).
func TestFailedWatchPageKeepsTheCachedIdentity(t *testing.T) {
	for _, path := range []string{"authenticated", "public"} {
		t.Run(path, func(t *testing.T) {
			orig := fetchWatchPage
			fetchWatchPage = func(context.Context, string, string) (*WatchPageResult, error) {
				return nil, errors.New("failed to fetch watch page: HTTP 503 from https://www.youtube.com/watch (4 attempts)")
			}
			t.Cleanup(func() { fetchWatchPage = orig })
			tr := recordPlayerCalls(t)
			jar, _ := loadAccountJar(t, accountCookies("a"))
			svc := NewService(jar, noopLogger{})
			svc.SetVisitorData("vd-cached")
			idx := 2
			svc.PlayerAPI.captureVisitorData(&YtcfgData{SessionIndex: &idx, DelegatedSessionID: "brand-123"}, jar.YouTubeIdentity())
			var captured atomic.Int32
			svc.PlayerAPI.OnVisitorData = func(string) { captured.Add(1) }

			get := svc.PlayerAPI.GetVideoInfoPublic
			if path == "authenticated" {
				get = svc.PlayerAPI.GetVideoInfoAuthenticated
			}
			if _, err := get(context.Background(), "test1234567"); err != nil {
				t.Fatalf("extraction: %v", err)
			}
			if len(tr.headers) == 0 {
				t.Fatal("no player call was made")
			}
			for i, h := range tr.headers {
				client := h.Get("X-YouTube-Client-Name")
				if client == "28" || client == "101" {
					continue // the cookieless chain carries visitor data only
				}
				if got := h.Get("X-Goog-Visitor-Id"); got != "vd-cached" {
					t.Errorf("call %d (client %s): X-Goog-Visitor-Id = %q, want the cached vd-cached", i, client, got)
				}
				wantPage, wantUser := "", ""
				if path == "authenticated" {
					wantPage, wantUser = "brand-123", "2"
				}
				if got := h.Get("X-Goog-PageId"); got != wantPage {
					t.Errorf("call %d (client %s): X-Goog-PageId = %q, want %q", i, client, got, wantPage)
				}
				if got := h.Get("X-Goog-AuthUser"); got != wantUser {
					t.Errorf("call %d (client %s): X-Goog-AuthUser = %q, want %q", i, client, got, wantUser)
				}
			}
			if n := captured.Load(); n != 0 {
				t.Errorf("OnVisitorData fired %d times for a ytcfg no page stated", n)
			}
		})
	}
}
