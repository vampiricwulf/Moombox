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
// the error says so; the retries stay inside their window — a third of a
// caller's remaining deadline, and one attempt's timeout from the start —
// returning the page's own error rather than the deadline; and a cancel
// ends the wait with the cancel.
//
// The deadline rows are the window's arithmetic for a page answering 503
// throughout. 10 s, the 403 refresh at the MaximumTimeout floor: the window
// closes at 3.33 s, so the 1 s retry is taken and the 2 s one (which would
// leave 0.33 s, under the base reserved for its attempt) is not. 4.5 s: the
// window closes at 1.5 s, which the 1 s wait would leave only 0.5 s of. 45 s:
// the window closes at 15 s, room for the whole ladder.
//
// Mutants: the attempt count past 4 (the first row's count); the window
// widened to the whole deadline, the guard as it stood (the 10 s row asks at
// 3 s and 7 s too, holding the player calls back 7 s of 10) or to half of it
// (a third request at 3 s); the base reserved for the attempt after a wait
// dropped (the 4.5 s row retries at 1 s); retryExitErr bypassed (the cancel
// row reports the 503).
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

	for _, tc := range []struct {
		name     string
		deadline time.Duration
		wantAt   []time.Duration
	}{
		{name: "a 10 s deadline: one retry", deadline: 10 * time.Second, wantAt: []time.Duration{0, time.Second}},
		{name: "a 4.5 s deadline: no room for a retry's attempt", deadline: 4500 * time.Millisecond, wantAt: []time.Duration{0}},
		{name: "a 45 s deadline: the whole ladder", deadline: 45 * time.Second, wantAt: []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				arrivals := &pageArrivals{start: time.Now()}
				servePagesInBubble(t, failing(arrivals), 0)
				ctx, cancel := context.WithTimeout(context.Background(), tc.deadline)
				defer cancel()
				_, err := FetchWatchPage(ctx, "test1234567", "")
				if err == nil || !strings.Contains(err.Error(), "HTTP 503") || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("err = %v, want the page's 503, not the deadline", err)
				}
				if got := arrivals.of("/watch"); !reflect.DeepEqual(got, tc.wantAt) {
					t.Errorf("requests arrived at %v, want %v", got, tc.wantAt)
				}
				if last := tc.wantAt[len(tc.wantAt)-1]; time.Since(arrivals.start) != last {
					t.Errorf("returned after %v, want at once after the last answer (%v)", time.Since(arrivals.start), last)
				}
			})
		})
	}

	// A page that never answers. hangs makes every attempt that reaches the
	// handler wait for its request to end; dialFails refuses that many dials
	// first. Without a deadline the window is the first attempt's 30 s.
	for _, tc := range []struct {
		name      string
		dialFails int
		wantAt    []time.Duration
		wantErr   string
		notErr    error
	}{
		// The first attempt runs its 30 s and is not retried: four would
		// have held the extraction back 127 s.
		{name: "a hung page is not retried past its timeout", wantAt: []time.Duration{0}, wantErr: "failed to fetch watch page"},
		// A refused dial is retried at 1 s; that retry hangs, and the window
		// ends it at 30 s, not its own timeout at 31 s. The error is the
		// page's own answer, the refused dial — not the window's deadline.
		{name: "a retry that hangs ends with the window", dialFails: 1, wantAt: []time.Duration{time.Second},
			wantErr: "connection refused", notErr: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				arrivals := &pageArrivals{start: time.Now()}
				servePagesInBubble(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					arrivals.note(r.URL.Path)
					<-r.Context().Done()
				}), tc.dialFails)
				_, err := FetchWatchPage(context.Background(), "test1234567", "")
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || (tc.notErr != nil && errors.Is(err, tc.notErr)) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if got := arrivals.of("/watch"); !reflect.DeepEqual(got, tc.wantAt) {
					t.Errorf("requests arrived at %v, want %v", got, tc.wantAt)
				}
				if took := time.Since(arrivals.start); took != 30*time.Second {
					t.Errorf("returned after %v, want 30s — one attempt's timeout, the window", took)
				}
			})
		})
	}

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

// timedPlayerCalls records when each player call went out, on the bubble's
// clock, and answers it through inner.
type timedPlayerCalls struct {
	mu    sync.Mutex
	start time.Time
	at    []time.Duration
	inner http.RoundTripper
}

func (tr *timedPlayerCalls) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.mu.Lock()
	tr.at = append(tr.at, time.Since(tr.start))
	tr.mu.Unlock()
	return tr.inner.RoundTrip(req)
}

func (tr *timedPlayerCalls) calls() []time.Duration {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]time.Duration(nil), tr.at...)
}

// TestAFailingWatchPageLeavesThePlayerCallsTheirBudget is the retry window
// seen from the extraction: the page leads into the player calls, and its
// retries may not eat the budget those calls run on. Under the 403
// refresh's 10 s floor a page answering 503 throughout gets one retry and
// the first player call goes out at 1 s — it went out at 7 s with the
// retries bounded by the deadline alone, and with the iframe API down as
// well at 8 s, 2 s of 10 left. A hung page costs one attempt's 30 s, deadline
// or none, as the single attempt before the retries did: bounded by the
// deadline alone it spent the whole 45 s ceiling and no player call was
// made, and with no deadline it held the first one back 127 s.
//
// Mutants: the window widened to the whole deadline (the 10 s rows' first
// call at 7 s or 8 s; the 45 s row's never made); the no-deadline window
// dropped (the last row's first call at 127 s).
func TestAFailingWatchPageLeavesThePlayerCallsTheirBudget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		deadline     time.Duration // 0: none
		hang         bool
		knownPlayer  bool // a fresh homepage player is remembered
		wantPage     []time.Duration
		wantIframe   []time.Duration
		wantFirstAPI time.Duration
	}{
		{name: "a 503 page under a 10 s refresh", deadline: 10 * time.Second, knownPlayer: true,
			wantPage: []time.Duration{0, time.Second}, wantFirstAPI: time.Second},
		{name: "a 503 page and iframe API under a 10 s refresh", deadline: 10 * time.Second,
			wantPage: []time.Duration{0, time.Second}, wantIframe: []time.Duration{time.Second, 2 * time.Second}, wantFirstAPI: 2 * time.Second},
		{name: "a hung page under the 45 s ceiling", deadline: 45 * time.Second, hang: true, knownPlayer: true,
			wantPage: []time.Duration{0}, wantFirstAPI: 30 * time.Second},
		{name: "a hung page with no deadline", hang: true, knownPlayer: true,
			wantPage: []time.Duration{0}, wantFirstAPI: 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				arrivals := &pageArrivals{start: time.Now()}
				servePagesInBubble(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					arrivals.note(r.URL.Path)
					if tc.hang {
						<-r.Context().Done()
						return
					}
					http.Error(w, "down", http.StatusServiceUnavailable)
				}), 0)
				orig := fetchIframePlayerURL
				fetchIframePlayerURL = fetchPlayerURLFromIframeAPI
				t.Cleanup(func() { fetchIframePlayerURL = orig })
				calls := &timedPlayerCalls{start: arrivals.start, inner: &clientBodyTransport{}}
				origClient := apiClient
				apiClient = &http.Client{Transport: calls}
				t.Cleanup(func() { apiClient = origClient })

				p := newRetryTestAPI()
				if tc.knownPlayer {
					p.rememberPlayerURL(homepagePlayerURL, "homepage")
				}
				ctx := context.Background()
				if tc.deadline > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.deadline)
					defer cancel()
				}
				info, err := p.GetVideoInfoPublic(ctx, "test1234567")
				if err != nil || len(info.Formats) == 0 {
					t.Fatalf("GetVideoInfoPublic = %v — the player calls must still have the budget to answer", err)
				}
				if got := arrivals.of("/watch"); !reflect.DeepEqual(got, tc.wantPage) {
					t.Errorf("watch page asked at %v, want %v", got, tc.wantPage)
				}
				if got := arrivals.of("/iframe_api"); !reflect.DeepEqual(got, tc.wantIframe) {
					t.Errorf("iframe API asked at %v, want %v", got, tc.wantIframe)
				}
				if got := calls.calls(); len(got) == 0 || got[0] != tc.wantFirstAPI {
					t.Errorf("player calls at %v, want the first at %v", got, tc.wantFirstAPI)
				}
			})
		})
	}
}
