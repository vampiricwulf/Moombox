// Package utils provides shared utility functions.
package utils

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// utilsHTTPClient is a shared HTTP client with a long safety-net timeout.
// FetchWithTimeout creates its own timeout via context.WithTimeout, so the
// client timeout only guards against truly stuck connections.
var utilsHTTPClient = &http.Client{Timeout: 5 * time.Minute}

var connReporter interface {
	ReportFailure(tag string)
	ReportSuccess(tag string)
}

// SetConnectivityReporter sets the global connectivity reporter for HTTP utilities.
func SetConnectivityReporter(r interface{ ReportFailure(string); ReportSuccess(string) }) {
	connReporter = r
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
		if connReporter != nil {
			connReporter.ReportFailure("utils/http")
		}
		cancel()
		return nil, nil, err
	}
	if connReporter != nil {
		connReporter.ReportSuccess("utils/http")
	}

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

	const maxFetchBodySize = 50 << 20 // 50MB cap
	return io.ReadAll(io.LimitReader(resp.Body, maxFetchBodySize))
}

