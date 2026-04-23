package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// engineHTTPClient is a shared HTTP client for segment and chunk downloads.
//
// Transport config: the Go default caps idle connections per host at 2,
// which causes TCP-handshake churn during high-concurrency catch-up with
// ParallelDownloads (6) workers plus HEAD probes and playlist fetches.
// Bump idle-per-host to ParallelDownloads+2 so the workers reuse sockets,
// and keep them alive for 90 s (matches http.DefaultTransport's value).
//
// Timeout: context-based cancellation at every callsite is the primary
// deadline mechanism (each request has its own ctx with SegmentTimeout /
// ChunkTimeout wired). The client-level Timeout is a safety net for the
// pathological "ctx never fires AND server keeps the socket open forever"
// case; 5 minutes is generous enough for multi-MB segments on slow
// connections without becoming a hang vector.
var engineHTTPClient = &http.Client{
	Timeout: 5 * time.Minute,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   ParallelDownloads + 2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}

// Segment read limits applied to resp.Body. Both are bounded so a
// misbehaving server can't force the downloader to allocate unbounded
// memory for a single response.
const (
	// maxSegmentBodyBytes caps the body of a normal segment/playlist fetch.
	// YouTube live DASH segments are typically 200KB-4MB; 100 MB gives
	// enormous headroom without letting a broken response balloon RAM.
	maxSegmentBodyBytes = 100 << 20
	// maxIgnoredRangeBodyBytes is used when a server returns 200 OK to a
	// Range request instead of 206 Partial. We discard the body at 50 MB
	// so a dumb static server on a multi-GB VOD can't drain the process.
	maxIgnoredRangeBodyBytes = 50 << 20
)

// applyPoTokenQuery appends `?pot=<token>` (or `&pot=<token>` if the URL
// already has a query string) to a segment URL. Returns the URL unchanged
// if the token is empty. Centralized here so segment, head-probe, and
// chunk fetch all inject the token identically.
func applyPoTokenQuery(rawURL, token string) string {
	if token == "" {
		return rawURL
	}
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return rawURL + sep + "pot=" + token
}

// ConnectivityReporter is the interface the engine uses to notify the
// connectivity monitor about HTTP successes and failures. It's stored in an
// atomic.Pointer so SetConnectivityReporter and the many concurrent readers
// in fetchSegment/probe/* don't race.
type ConnectivityReporter interface {
	ReportFailure(tag string)
	ReportSuccess(tag string)
}

var connReporter atomic.Pointer[ConnectivityReporter]

// SetConnectivityReporter sets the global connectivity reporter for the engine package.
// Safe to call before or after downloads start; reads are lock-free via atomic.Pointer.
func SetConnectivityReporter(r ConnectivityReporter) {
	if r == nil {
		connReporter.Store(nil)
		return
	}
	connReporter.Store(&r)
}

// loadConnReporter returns the current reporter or nil if none is set.
// Small helper so callers don't need to double-deref the atomic pointer.
func loadConnReporter() ConnectivityReporter {
	p := connReporter.Load()
	if p == nil {
		return nil
	}
	return *p
}

func reportFailure(tag string) {
	if r := loadConnReporter(); r != nil {
		r.ReportFailure(tag)
	}
}

func reportSuccess(tag string) {
	if r := loadConnReporter(); r != nil {
		r.ReportSuccess(tag)
	}
}

// fetchSegment downloads a single segment (or playlist) by URL.
func (d *SegmentDownloader) fetchSegment(ctx context.Context, segURL string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, SegmentTimeout)
	defer cancel()

	// Apply GVS PO token to segment URL (query mode: ?pot=token)
	segURL = applyPoTokenQuery(segURL, d.opts.PoToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, segURL, nil)
	if err != nil {
		return nil, 0, err
	}
	d.setCommonHeaders(req, uaWeb)

	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		reportFailure("engine/fetch")
		return nil, 0, err
	}
	reportSuccess("engine/fetch")
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSegmentBodyBytes))
	if err != nil {
		return nil, resp.StatusCode, err
	}

	return data, resp.StatusCode, nil
}

// fetchSegmentWithRetry attempts to fetch a segment with retries and exponential backoff.
// Returns the data on success, or nil if all retries failed or the segment is permanently gone.
func (d *SegmentDownloader) fetchSegmentWithRetry(ctx context.Context, segURL string) []byte {
	for attempt := range MaxSegmentRetries {
		if d.isCancelled() || ctx.Err() != nil {
			return nil
		}
		data, status, err := d.fetchSegment(ctx, segURL)
		if err == nil && status < 400 {
			return data
		}
		if status == 403 || status == 410 {
			return nil // Segment gone permanently
		}
		sleepCtx(ctx, time.Duration(5*(attempt+1))*time.Second)
	}
	return nil
}

// probeHeadSequence discovers the current live head segment using a high sequence GET probe.
// YouTube returns the X-Head-Seqnum header on GET requests to a non-existent segment.
func (d *SegmentDownloader) probeHeadSequence(ctx context.Context) (int, error) {
	probeURL := d.buildSegmentURL(999999999)
	probeURL = applyPoTokenQuery(probeURL, d.opts.PoToken)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return -1, err
	}
	d.setCommonHeaders(req, uaWeb)

	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		reportFailure("engine/fetch")
		return -1, err
	}
	reportSuccess("engine/fetch")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	headSeqStr := resp.Header.Get("X-Head-Seqnum")
	if headSeqStr == "" {
		return -1, fmt.Errorf("no X-Head-Seqnum header")
	}

	headSeq, err := strconv.Atoi(headSeqStr)
	if err != nil {
		return -1, fmt.Errorf("parse X-Head-Seqnum: %w", err)
	}

	return headSeq, nil
}

// probeFileSize discovers the total file size using a Range: bytes=0-0 request.
// Returns 0 if the server doesn't support Range requests or the size is unknown.
func (d *SegmentDownloader) probeFileSize(ctx context.Context) int64 {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.opts.BaseURL, nil)
	if err != nil {
		return 0
	}
	d.setCommonHeaders(req, uaAndroid)
	req.Header.Set("Range", "bytes=0-0")

	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		reportFailure("engine/fetch")
		return 0
	}
	reportSuccess("engine/fetch")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		return 0 // Server doesn't support range requests
	}

	// Parse Content-Range: bytes 0-0/TOTAL
	contentRange := resp.Header.Get("Content-Range")
	if idx := strings.LastIndex(contentRange, "/"); idx >= 0 {
		sizeStr := contentRange[idx+1:]
		if sizeStr != "*" {
			size, _ := strconv.ParseInt(sizeStr, 10, 64)
			return size
		}
	}

	return 0
}

// fetchChunkWithRetry downloads a byte range with exponential backoff retry.
func (d *SegmentDownloader) fetchChunkWithRetry(ctx context.Context, start, end int64) ([]byte, int, error) {
	for attempt := range MaxChunkRetries {
		if d.isCancelled() || ctx.Err() != nil {
			return nil, 0, d.cancelErr(ctx)
		}

		data, status, err := d.fetchChunk(ctx, start, end)
		if err == nil {
			return data, status, nil
		}

		if status == http.StatusRequestedRangeNotSatisfiable {
			return nil, status, err
		}

		// Retry on 5xx or network errors with exponential backoff (capped at 60s)
		if status >= 500 || status == 0 {
			delay := time.Duration(1<<uint(attempt)) * time.Second
			delay = min(delay, 60*time.Second)
			sleepCtx(ctx, delay)
			continue
		}

		return nil, status, err
	}

	return nil, 0, fmt.Errorf("chunk download failed after %d retries", MaxChunkRetries)
}

// fetchChunk downloads a single byte range from the direct URL.
func (d *SegmentDownloader) fetchChunk(ctx context.Context, start, end int64) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, SegmentTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.opts.BaseURL, nil)
	if err != nil {
		return nil, 0, err
	}
	d.setCommonHeaders(req, uaAndroid)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		reportFailure("engine/fetch")
		return nil, 0, err
	}
	reportSuccess("engine/fetch")
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return nil, resp.StatusCode, fmt.Errorf("range not satisfiable")
	}
	if resp.StatusCode == http.StatusOK {
		// Server ignored Range header -- cap read to avoid unbounded memory usage
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxIgnoredRangeBodyBytes))
		return data, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusPartialContent {
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}
