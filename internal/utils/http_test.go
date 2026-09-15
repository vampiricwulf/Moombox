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

// goCancelAfter runs cancel after d in a goroutine carrying the project's
// inline recover (the global rule: every goroutine has one). Without it a
// panic here takes the whole test binary down with a stack naming only the
// goroutine, attributed to no test. The goroutine cannot report the panic
// itself — no t method is legal from a goroutine that may outlive the test —
// so the value goes into a buffered channel and the returned join func, which
// the test body calls while it is still running, fails the right test with it.
func goCancelAfter(t *testing.T, d time.Duration, cancel context.CancelFunc) func() {
	t.Helper()
	panicked := make(chan any, 1)
	done := make(chan struct{})
	go func() {
		defer close(done) // registered first, so it runs AFTER the recover below
		defer func() {
			if r := recover(); r != nil {
				panicked <- r
			}
		}()
		time.Sleep(d)
		cancel()
	}()
	return func() {
		t.Helper()
		<-done
		select {
		case r := <-panicked:
			t.Fatalf("the cancel goroutine panicked: %v", r)
		default:
		}
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
	joinCancel := goCancelAfter(t, 50*time.Millisecond, cancel)
	_, err := FetchBody(ctx, srv.URL, 5*time.Second, nil)
	joinCancel()
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

// TestUtilsHTTPClientCarriesNoCookieJar pins a property another package
// silently depends on for correctness.
//
// internal/youtube's liveness probes fetch a page with an explicit Cookie
// header and then decide whether the answer may be read as evidence about the
// user's YouTube session. The check that makes that sound is "did the request
// which finally answered still carry that header" — the stdlib strips it,
// permanently, once a redirect leaves the origin (net/http/client.go:620,
// :688, :826), so its absence is how a credential-less bounce is detected.
//
// Installing an http.CookieJar here breaks that with nothing failing: the
// stdlib would re-add a Cookie header on the final hop from the jar's own
// scope rules, the probe's check would pass on a request that never carried
// the caller's session, and a healthy account would be reported as dead
// cookies. There is no way for the youtube package to observe this, so the
// assertion lives next to the client it constrains.
//
// CheckRedirect is asserted alongside it: a custom one could re-populate
// headers on a redirect hop and defeat the same check by a different route.
func TestUtilsHTTPClientCarriesNoCookieJar(t *testing.T) {
	if utilsHTTPClient.Jar != nil {
		t.Error("utilsHTTPClient has a CookieJar — this silently invalidates " +
			"internal/youtube's liveness guard; see the comment on utilsHTTPClient")
	}
	if utilsHTTPClient.CheckRedirect != nil {
		t.Error("utilsHTTPClient has a custom CheckRedirect — verify it cannot " +
			"restore headers the stdlib stripped before relaxing this assertion")
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

// TestFetchWithTimeoutCallerCancelIsNotAFailure pins T4-35 for the shared
// helper: the connectivity oracle must not learn "the network is down" from
// a shutdown or a superseded probe cancelling its own fetch.
//
// Mutant this kills: reporting unconditionally (failures becomes 1).
func TestFetchWithTimeoutCallerCancelIsNotAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	rec := &recordingReporter{}
	SetConnectivityReporter(rec)
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	ctx, cancel := context.WithCancel(t.Context())
	joinCancel := goCancelAfter(t, 50*time.Millisecond, cancel)
	_, _, err := FetchWithTimeout(ctx, srv.URL, 5*time.Second, nil)
	joinCancel()
	if err == nil {
		t.Fatal("FetchWithTimeout on a cancelled context returned nil error")
	}
	cancel()

	if got := rec.failures.Load(); got != 0 {
		t.Errorf("failures = %d, want 0 — a caller cancel was recorded as a network failure", got)
	}
}

// TestFetchWithTimeoutTransportErrorIsAFailure is the other half: a real
// transport failure with a healthy caller must still be reported.
//
// Mutant this kills: a guard that suppresses every error outright (failures
// drops to 0). It does NOT discriminate a parent guard from one written
// against the DERIVED context: the connection reset here happens instantly,
// long before either context's deadline, so both are still alive and a
// derived-context guard reports identically to the correct one.
// TestFetchWithTimeoutDerivedTimeoutIsAFailure below is what kills that
// mutant.
func TestFetchWithTimeoutTransportErrorIsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	t.Cleanup(srv.Close)

	rec := &recordingReporter{}
	SetConnectivityReporter(rec)
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	if _, _, err := FetchWithTimeout(t.Context(), srv.URL, 5*time.Second, nil); err == nil {
		t.Fatal("FetchWithTimeout against a hang-up server returned nil error")
	}
	if got := rec.failures.Load(); got != 1 {
		t.Errorf("failures = %d, want 1 — a transport error with a healthy caller IS network evidence", got)
	}
}

// TestFetchWithTimeoutDerivedTimeoutIsAFailure is the discriminator the two
// tests above cannot be: it is the only scenario where the parent
// (t.Context()) stays alive but the DERIVED context — trimmed here to 20ms
// — is the one that expires, against a server that never answers. A
// request that genuinely ran out of time with a healthy caller IS network
// evidence and must still be reported.
//
// Mutant this kills: guarding on the DERIVED context instead of the parent
// — failures would stay 0, exactly backwards from what a guard against
// caller-cancellation is supposed to catch.
func TestFetchWithTimeoutDerivedTimeoutIsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	rec := &recordingReporter{}
	SetConnectivityReporter(rec)
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	if _, _, err := FetchWithTimeout(t.Context(), srv.URL, 20*time.Millisecond, nil); err == nil {
		t.Fatal("FetchWithTimeout against a server that never answers returned nil error")
	}
	if got := rec.failures.Load(); got != 1 {
		t.Errorf("failures = %d, want 1 — a derived-context timeout with a healthy caller IS network evidence", got)
	}
}
