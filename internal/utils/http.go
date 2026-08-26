// Package utils provides shared utility functions.
package utils

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/connectivity"
	"github.com/vampiricwulf/Moombox/internal/httpx"
)

// MaxFetchBodySize caps response bodies read by FetchBody. Public so other
// packages can validate against the same ceiling instead of guessing.
const MaxFetchBodySize = 50 << 20

// utilsHTTPClient backs FetchBody / FetchWithTimeout via the shared
// httpx transport. The 5-minute client-level timeout is a safety net;
// per-call timeouts come from FetchWithTimeout's ctx.WithTimeout.
// httpx's transport already carries the keep-alive tuning the audit
// recommended (MaxIdleConnsPerHost=8, IdleConnTimeout=90s, HTTP/2).
//
// DO NOT give this client an http.CookieJar. internal/youtube's liveness
// probes decide whether a fetched page may be read as evidence about the
// user's session by checking that the request which finally answered still
// carried the Cookie header they set — the stdlib strips it, permanently, once
// a redirect leaves the origin. A jar would re-add a Cookie header on that
// final hop from its own scope rules, so the check would pass on a request
// that never carried the caller's session and the probe would report healthy
// credentials as dead. Nothing in that package can observe this from here,
// which is why TestUtilsHTTPClientCarriesNoCookieJar pins it.
var utilsHTTPClient = httpx.Client(5 * time.Minute)

// ConnectivityReporter is a type alias to connectivity.Reporter so the HTTP
// helpers don't carry a separate-but-identical interface that drifts on
// future renames. Tagging via "utils/http" lets the passive tracker
// distinguish this subsystem from other fetch paths (see
// internal/connectivity/passive.go). Audit reports/small-packages.md.
type ConnectivityReporter = connectivity.Reporter

// connReporter is an atomic.Pointer so that SetConnectivityReporter can be
// called without racing with concurrent fetches. In practice main.go
// installs the reporter once at startup, but making the read lock-free
// removes any happens-before foot-gun for future callers or tests that
// might reinstall the reporter mid-run.
var connReporter atomic.Pointer[ConnectivityReporter]

// SetConnectivityReporter sets the global connectivity reporter for HTTP utilities.
// Safe to call concurrently with in-flight fetches.
func SetConnectivityReporter(r ConnectivityReporter) {
	if r == nil {
		connReporter.Store(nil)
		return
	}
	connReporter.Store(&r)
}

// reportConnResult is an internal helper that loads the reporter atomically
// and forwards the success/failure callback if one is installed.
func reportConnResult(failed bool) {
	rp := connReporter.Load()
	if rp == nil {
		return
	}
	if failed {
		(*rp).ReportFailure("utils/http")
	} else {
		(*rp).ReportSuccess("utils/http")
	}
}

// FetchWithTimeout performs an HTTP GET with a timeout.
// IMPORTANT: The caller receives a cancel function that MUST be called after
// the response body has been fully read. The timeout context is kept alive
// so the caller can read resp.Body without "context canceled" errors.
func FetchWithTimeout(ctx context.Context, url string, timeout time.Duration, headers map[string]string) (*http.Response, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("failed to create request: %w", err)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := utilsHTTPClient.Do(req)
	if err != nil {
		reportConnResult(true)
		cancel()
		return nil, nil, err
	}
	reportConnResult(false)

	return resp, cancel, nil
}

// FetchBody performs an HTTP GET and returns the response body as bytes.
// Uses a single timeout context that spans both the HTTP request and body read.
func FetchBody(ctx context.Context, url string, timeout time.Duration, headers map[string]string) ([]byte, error) {
	resp, cancel, err := FetchWithTimeout(ctx, url, timeout, headers)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	return io.ReadAll(io.LimitReader(resp.Body, MaxFetchBodySize))
}
