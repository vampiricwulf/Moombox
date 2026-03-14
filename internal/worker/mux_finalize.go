package worker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// workerHTTPClient is a shared HTTP client for file downloads in the worker package.
// Uses a generous timeout since files can be up to 2GB (thumbnails, VODs, assets).
var workerHTTPClient = &http.Client{Timeout: 10 * time.Minute}

// DownloadFile downloads a file from a URL to the output path.
// Used for VOD direct downloads and thumbnail/asset fetching.
func DownloadFile(ctx context.Context, url, outputPath string) error {
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

	// Limit body to 2GB to prevent unbounded memory/disk usage
	_, err = io.Copy(f, io.LimitReader(resp.Body, 2<<30))
	f.Close()
	if err != nil {
		os.Remove(tmpPath)
		return err
	}

	return os.Rename(tmpPath, outputPath)
}

// DownloadFileMinSize downloads a file but discards it if smaller than minSize bytes.
func DownloadFileMinSize(ctx context.Context, url, outputPath string, minSize int64) error {
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
		return fmt.Errorf("file too small: %d bytes (min %d)", n, minSize)
	}

	return os.Rename(tmpPath, outputPath)
}

// DownloadThumbnail downloads a thumbnail to the staging directory.
func DownloadThumbnail(ctx context.Context, thumbnailURL, stagingDir string) (string, error) {
	if thumbnailURL == "" {
		return "", nil
	}

	outputPath := filepath.Join(stagingDir, "thumbnail.jpg")
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := DownloadFile(ctx, thumbnailURL, outputPath); err != nil {
		return "", err
	}
	return outputPath, nil
}

type httpError struct {
	StatusCode int
}

func (e *httpError) Error() string {
	return http.StatusText(e.StatusCode)
}
