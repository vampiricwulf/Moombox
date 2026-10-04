package worker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/vampiricwulf/Moombox/internal/httpx"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// workerHTTPClient downloads assets (DownloadFileMinSize), backed by the
// shared httpx transport. VOD downloads go through internal/engine, not
// here.
var workerHTTPClient = httpx.Client(10 * time.Minute)

// DownloadFileMinSize downloads a file but discards it if smaller than minSize bytes.
// If lg is non-nil and the file is rejected for being too small, the rejection
// is also logged at Debug level (per audit reports/worker.md Finding 48 — the
// silent error was hard to diagnose in production).
func DownloadFileMinSize(ctx context.Context, url, outputPath string, minSize int64, lg logger) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := workerHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return &httpError{StatusCode: resp.StatusCode}
	}

	tmpPath := outputPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	n, err := io.Copy(f, io.LimitReader(resp.Body, 50<<20)) // 50MB limit
	f.Close()
	if err != nil {
		os.Remove(tmpPath)
		return err
	}
	if n < minSize {
		os.Remove(tmpPath)
		if lg != nil {
			lg.Debug("DownloadFileMinSize: rejected file (too small)", "url", url, "bytes", n, "min", minSize)
		}
		return fmt.Errorf("file too small: %d bytes (min %d)", n, minSize)
	}

	// utils.ReplaceFile, not os.Rename: the file was written a moment ago, and
	// on Windows a scanner or indexer still holding it turns the last step of
	// this atomic write into a spurious failure (sweep-2 TOOL-2).
	return utils.ReplaceFile(tmpPath, outputPath)
}

type httpError struct {
	StatusCode int
}

func (e *httpError) Error() string {
	return http.StatusText(e.StatusCode)
}
