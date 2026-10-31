package youtube

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// G46: doRetryRequest retries what yt-dlp's _extract_response retries
// (_base.py:1322-1356 at b77c9304e) and returns a 429 at once. The servers
// below answer from a script, one entry per request, the last repeated; the
// counts are the assertions, and the ladder runs at TestMain's millisecond
// base, so no test waits on a clock.

const okPlayerBody = `{"videoDetails":{"videoId":"abc12345678","title":"T"}}`

// scriptedResponse is one answer a scripted server gives.
type scriptedResponse struct {
	status int
	header map[string]string
	body   string
}

// serveScript answers request n with script[n], the last entry once the
// script runs out, and counts the requests.
func serveScript(t *testing.T, script ...scriptedResponse) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(hits.Add(1)) - 1
		resp := script[min(n, len(script)-1)]
		for k, v := range resp.header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		w.Write([]byte(resp.body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestPlayerRetryBackoffBaseIsOneSecond pins the production base TestMain
// scales down: 1 s, 2 s, 4 s.
func TestPlayerRetryBackoffBaseIsOneSecond(t *testing.T) {
	if productionPlayerRetryBackoffBase != time.Second {
		t.Errorf("playerRetryBackoffBase = %s, want 1s", productionPlayerRetryBackoffBase)
	}
}

// TestDoRetryRequestReturnsA429AtOnce: a 429 is YouTube saying too many
// requests, and upstream returns it at once and moves on. It was retried
// three more times at 1, 2 and 4 s, on every client of a cascade.
//
// Mutant: put 429 back in the retry arm — the server sees 4 requests and the
// error loses its Retry-After.
func TestDoRetryRequestReturnsA429AtOnce(t *testing.T) {
	srv, hits := serveScript(t, scriptedResponse{status: http.StatusTooManyRequests,
		header: map[string]string{"Retry-After": "120"},
		body:   `{"error":{"message":"Too many requests","status":"RESOURCE_EXHAUSTED"}}`})

	_, err := newRetryTestAPI().doRetryRequest(context.Background(), srv.URL, []byte(`{}`), nil, nil, "TVHTML5", "abc12345678")
	if n := hits.Load(); n != 1 {
		t.Errorf("server saw %d requests, want 1", n)
	}
	rl, ok := errors.AsType[*HTTPRateLimitError](err)
	if !ok {
		t.Fatalf("err = %v, want an HTTPRateLimitError", err)
	}
	if rl.RetryAfter != 2*time.Minute {
		t.Errorf("RetryAfter = %s, want 2m0s", rl.RetryAfter)
	}
	// The head probe_classify reads: "HTTP 429" first, YouTube's text after.
	want := "TVHTML5 API error: HTTP 429 — RESOURCE_EXHAUSTED: Too many requests (Retry-After 2m0s)"
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}

// TestRetryAfterReadsBothForms: delta-seconds and an HTTP-date; nothing for
// a date already past, a negative count or text that is neither.
func TestRetryAfterReadsBothForms(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"", 0},
		{"30", 30 * time.Second},
		{" 3600 ", time.Hour},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"-5", 0},
		{"soon", 0},
	} {
		h := http.Header{}
		if tc.header != "" {
			h.Set("Retry-After", tc.header)
		}
		if got := retryAfter(h, now); got != tc.want {
			t.Errorf("retryAfter(%q) = %s, want %s", tc.header, got, tc.want)
		}
	}
}

// TestDoRetryRequestRetriesA404: YouTube answers the odd 404 (ytdl-org
// #28289), and upstream retries it. Returned at once, it dropped the client
// for the extraction — TV, on the authenticated cascade, the playability
// authority.
//
// Mutant: take 404 out of the retry arm — one request, and the error.
func TestDoRetryRequestRetriesA404(t *testing.T) {
	srv, hits := serveScript(t,
		scriptedResponse{status: http.StatusNotFound, body: `{}`},
		scriptedResponse{status: http.StatusNotFound, body: `{}`},
		scriptedResponse{status: http.StatusOK, body: okPlayerBody})

	info, err := newRetryTestAPI().doRetryRequest(context.Background(), srv.URL, []byte(`{}`), nil, nil, "TVHTML5", "abc12345678")
	if err != nil || info == nil {
		t.Fatalf("doRetryRequest = %v, %v; want the third answer's player response", info, err)
	}
	if n := hits.Load(); n != 3 {
		t.Errorf("server saw %d requests, want 3", n)
	}
}

// unknownErrorAlertBody is a 200 carrying the alert yt-dlp #839 retries.
const unknownErrorAlertBody = `{"alerts":[{"alertRenderer":{"type":"ERROR","text":{"runs":[{"text":"An unknown error occurred."}]}}}]}`

// TestDoRetryRequestRetriesAnUnknownErrorAlert: "YouTube's servers may
// return errors we want to retry on in a 200 OK response" (_base.py:1350).
// It was parsed as the client's answer, a response with no player in it.
//
// Mutant: drop the alert check — one request, and the alert's body comes
// back as the player response.
func TestDoRetryRequestRetriesAnUnknownErrorAlert(t *testing.T) {
	t.Run("then an answer", func(t *testing.T) {
		srv, hits := serveScript(t,
			scriptedResponse{status: http.StatusOK, body: unknownErrorAlertBody},
			scriptedResponse{status: http.StatusOK, body: okPlayerBody})
		info, err := newRetryTestAPI().doRetryRequest(context.Background(), srv.URL, []byte(`{}`), nil, nil, "WEB", "abc12345678")
		if err != nil || info == nil || info.Title != "T" {
			t.Fatalf("doRetryRequest = %+v, %v; want the second answer's player response", info, err)
		}
		if n := hits.Load(); n != 2 {
			t.Errorf("server saw %d requests, want 2", n)
		}
	})
	t.Run("every time", func(t *testing.T) {
		srv, hits := serveScript(t, scriptedResponse{status: http.StatusOK, body: unknownErrorAlertBody})
		_, err := newRetryTestAPI().doRetryRequest(context.Background(), srv.URL, []byte(`{}`), nil, nil, "WEB", "abc12345678")
		if err == nil || !strings.Contains(err.Error(), "WEB API error: YouTube said: An unknown error occurred.") {
			t.Errorf("err = %v, want the alert, as upstream raises it", err)
		}
		if n := hits.Load(); n != 4 {
			t.Errorf("server saw %d requests, want 4", n)
		}
	})
}

// TestDoRetryRequestTakesOtherAlertsAtTheirWord: only the alert upstream
// raises is retried — the last of type ERROR, and only when it says "unknown
// error". A warning that says so, or an earlier error that a later one
// supersedes, leaves the response the client's answer.
//
// Mutants: drop the type test — the warning row is retried; read the first
// ERROR alert rather than the last — the two-error row is retried.
func TestDoRetryRequestTakesOtherAlertsAtTheirWord(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"another error", `{"alerts":[{"alertRenderer":{"type":"ERROR","text":{"simpleText":"This video is unavailable"}}}],` +
			`"videoDetails":{"videoId":"abc12345678","title":"T"}}`},
		{"a warning that says unknown error", `{"alerts":[{"alertWithButtonRenderer":{"type":"WARNING","text":{"simpleText":"An unknown error occurred."}}}],` +
			`"videoDetails":{"videoId":"abc12345678","title":"T"}}`},
		{"a later error supersedes it", `{"alerts":[` +
			`{"alertRenderer":{"type":"ERROR","text":{"simpleText":"An unknown error occurred."}}},` +
			`{"alertRenderer":{"type":"ERROR","text":{"simpleText":"This video is unavailable"}}}],` +
			`"videoDetails":{"videoId":"abc12345678","title":"T"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := serveScript(t, scriptedResponse{status: http.StatusOK, body: tc.body})
			info, err := newRetryTestAPI().doRetryRequest(context.Background(), srv.URL, []byte(`{}`), nil, nil, "WEB", "abc12345678")
			if err != nil || info == nil {
				t.Fatalf("doRetryRequest = %v, %v; want the response parsed", info, err)
			}
			if n := hits.Load(); n != 1 {
				t.Errorf("server saw %d requests, want 1", n)
			}
		})
	}
}
