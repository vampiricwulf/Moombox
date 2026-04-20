package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// engineHTTPClient is a shared HTTP client for segment and chunk downloads.
// Uses a long timeout as a safety net — context-based cancellation handles
// normal timeouts. Segments on slow connections can take several minutes.
var engineHTTPClient = &http.Client{Timeout: 5 * time.Minute}

var connReporter interface {
	ReportFailure(tag string)
	ReportSuccess(tag string)
}

// SetConnectivityReporter sets the global connectivity reporter for the engine package.
func SetConnectivityReporter(r interface{ ReportFailure(string); ReportSuccess(string) }) {
	connReporter = r
}

// fetchSegment downloads a single segment (or playlist) by URL.
func (d *SegmentDownloader) fetchSegment(ctx context.Context, segURL string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, SegmentTimeout)
	defer cancel()

	// Apply GVS PO token to segment URL (query mode: ?pot=token)
	if d.opts.PoToken != "" {
		sep := "?"
		if strings.Contains(segURL, "?") {
			sep = "&"
		}
		segURL = segURL + sep + "pot=" + d.opts.PoToken
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, segURL, nil)
	if err != nil {
		return nil, 0, err
	}
	d.setCommonHeaders(req, uaWeb)

	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		if connReporter != nil {
			connReporter.ReportFailure("engine/fetch")
		}
		return nil, 0, err
	}
	if connReporter != nil {
		connReporter.ReportSuccess("engine/fetch")
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20)) // 100MB max segment
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
	// Apply GVS PO token
	if d.opts.PoToken != "" {
		sep := "?"
		if strings.Contains(probeURL, "?") {
			sep = "&"
		}
		probeURL = probeURL + sep + "pot=" + d.opts.PoToken
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return -1, err
	}
	d.setCommonHeaders(req, uaWeb)

	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		if connReporter != nil {
			connReporter.ReportFailure("engine/fetch")
		}
		return -1, err
	}
	if connReporter != nil {
		connReporter.ReportSuccess("engine/fetch")
	}
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
		if connReporter != nil {
			connReporter.ReportFailure("engine/fetch")
		}
		return 0
	}
	if connReporter != nil {
		connReporter.ReportSuccess("engine/fetch")
	}
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
		if connReporter != nil {
			connReporter.ReportFailure("engine/fetch")
		}
		return nil, 0, err
	}
	if connReporter != nil {
		connReporter.ReportSuccess("engine/fetch")
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return nil, resp.StatusCode, fmt.Errorf("range not satisfiable")
	}
	if resp.StatusCode == http.StatusOK {
		// Server ignored Range header -- cap read to avoid unbounded memory usage
		data, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20)) // 50MB cap for ignored-range fallback
		return data, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusPartialContent {
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}
