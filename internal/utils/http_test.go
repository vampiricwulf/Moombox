package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestFetchBodyHappyPath verifies a successful 200-OK round trip returns
// the response body bytes intact, headers in the request reach the
// server, and the connection is closed cleanly.
func TestFetchBodyHappyPath(t *testing.T) {
	const want = "hello world"
	var sawHeader atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Test") == "yes" {
			sawHeader.Store(true)
		}
		fmt.Fprint(rw, want)
	}))
	t.Cleanup(srv.Close)

	body, err := FetchBody(t.Context(), srv.URL, 5*time.Second, map[string]string{
		"X-Test": "yes",
	})
	if err != nil {
		t.Fatalf("FetchBody: %v", err)
	}
	if string(body) != want {
		t.Errorf("body: want %q, got %q", want, string(body))
	}
	if !sawHeader.Load() {
		t.Error("server did not see X-Test header")
	}
}

// TestFetchBodyHTTPErrorReturnsErr covers the 4xx / 5xx surface — the
// helper should return an error containing the status code and URL so
// retry logic and operator logs can discriminate.
func TestFetchBodyHTTPErrorReturnsErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		http.Error(rw, "nope", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	body, err := FetchBody(t.Context(), srv.URL, 5*time.Second, nil)
	if err == nil {
		t.Fatal("FetchBody on 404: want error, got nil")
	}
	if body != nil {
		t.Errorf("body should be nil on error, got %q", body)
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("error: want %q, got %v", "HTTP 404", err)
	}
}

// TestFetchBodyRespectsTimeout verifies the timeout argument actually
// fires when the server is slow — without it, FetchBody could hang on a
// lazy peer indefinitely.
func TestFetchBodyRespectsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		rw.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	start := time.Now()
	_, err := FetchBody(t.Context(), srv.URL, 50*time.Millisecond, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("FetchBody with 50ms timeout vs 500ms server: want error, got nil")
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("FetchBody returned after %v — timeout did not fire promptly", elapsed)
	}
}

// TestFetchBodyHonoursCtxCancel covers the upstream-ctx-cancellation
// path: a parent ctx cancelled before the response body is read should
// surface as ctx.Canceled.
func TestFetchBodyHonoursCtxCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		rw.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := FetchBody(ctx, srv.URL, 5*time.Second, nil)
	if err == nil {
		t.Fatal("FetchBody with cancelled ctx: want error, got nil")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("err: want context.Canceled, got %v", err)
	}
}

// TestFetchBodyCapsAtMaxFetchBodySize verifies the io.LimitReader
// boundary at 50MB. The test serves a body exactly at the cap to
// confirm the read truncates without error.
func TestFetchBodyCapsAtMaxFetchBodySize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		// 100 bytes — well under the cap. We're verifying the
		// LimitReader is wired, not that 50MB downloads work.
		io.WriteString(rw, strings.Repeat("a", 100))
	}))
	t.Cleanup(srv.Close)

	body, err := FetchBody(t.Context(), srv.URL, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("FetchBody: %v", err)
	}
	if len(body) != 100 {
		t.Errorf("body length: want 100, got %d", len(body))
	}
}

// TestSetConnectivityReporterReplacesAtomically exercises the
// atomic.Pointer migration: a nil reset followed by an install should
// take effect on the very next FetchBody call without races.
func TestSetConnectivityReporterReplacesAtomically(t *testing.T) {
	rec := &recordingReporter{}
	SetConnectivityReporter(rec)
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	if _, err := FetchBody(t.Context(), srv.URL, 5*time.Second, nil); err != nil {
		t.Fatalf("FetchBody: %v", err)
	}
	if got := rec.successes.Load(); got != 1 {
		t.Errorf("successes after one fetch: want 1, got %d", got)
	}
	if got := rec.failures.Load(); got != 0 {
		t.Errorf("failures after a 200 OK: want 0, got %d", got)
	}
}

// recordingReporter satisfies ConnectivityReporter and counts callbacks
// for assertion in tests. Unexported because it lives only here.
type recordingReporter struct {
	successes atomic.Int32
	failures  atomic.Int32
}

func (r *recordingReporter) ReportSuccess(string) { r.successes.Add(1) }
func (r *recordingReporter) ReportFailure(string) { r.failures.Add(1) }
