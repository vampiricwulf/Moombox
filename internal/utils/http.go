// Package utils provides shared utility functions.
package utils

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"time"
)

// DrainBody reads and discards the remaining response body, then closes it.
// This ensures the underlying TCP connection can be reused by the HTTP client pool.
func DrainBody(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}

	return resp, cancel, nil
}

// FetchWithRetry performs an HTTP GET with automatic retry on 5xx/429 errors.
// Matches TypeScript's fetchWithTimeout behavior: retries 5xx and 429 responses,
// returns 4xx responses immediately without retry.
func FetchWithRetry(ctx context.Context, url string, timeout time.Duration, headers map[string]string) (*http.Response, error) {
	var lastResp *http.Response
	var lastCancel context.CancelFunc
	cfg := RetryConfig{
		MaxRetries:    3,
		InitialDelay:  time.Second,
		MaxDelay:      10 * time.Second,
		BackoffFactor: 2.0,
	}

	err := Retry(ctx, cfg, func() error {
		resp, cancel, err := FetchWithTimeout(ctx, url, timeout, headers)
		if err != nil {
			return err
		}
		// Retry on 5xx and 429 (rate limit)
		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			cancel()
			return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
		}
		// Cancel previous attempt's context if any
		if lastCancel != nil {
			lastCancel()
		}
		lastResp = resp
		lastCancel = cancel
		return nil
	})
	if err != nil && lastResp == nil {
		if lastCancel != nil {
			lastCancel()
		}
		return nil, err
	}
	// Note: caller must close resp.Body; lastCancel will fire when the timeout expires
	// or when the response body is fully consumed and closed.
	// We can't defer lastCancel here because the caller needs to read the body.
	// The timeout context will self-cancel after the timeout duration.
	return lastResp, nil
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

	return io.ReadAll(resp.Body)
}

// RetryConfig holds configuration for retry logic.
type RetryConfig struct {
	MaxRetries     int
	InitialDelay   time.Duration
	MaxDelay       time.Duration
	BackoffFactor  float64
}

// DefaultRetryConfig returns sensible default retry configuration.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:    3,
		InitialDelay:  time.Second,
		MaxDelay:      30 * time.Second,
		BackoffFactor: 2.0,
	}
}

// Retry executes fn with exponential backoff retries.
func Retry(ctx context.Context, cfg RetryConfig, fn func() error) error {
	var lastErr error
	delay := cfg.InitialDelay

	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		lastErr = fn()
		if lastErr == nil {
			return nil
		}

		if attempt < cfg.MaxRetries {
			// Add jitter: delay * (0.5 to 1.5)
			jittered := time.Duration(float64(delay) * (0.5 + rand.Float64()))
			select {
			case <-time.After(jittered):
			case <-ctx.Done():
				return ctx.Err()
			}

			delay = time.Duration(math.Min(float64(delay)*cfg.BackoffFactor, float64(cfg.MaxDelay)))
		}
	}

	return fmt.Errorf("max retries (%d) exceeded: %w", cfg.MaxRetries, lastErr)
}
