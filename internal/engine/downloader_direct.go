package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// runDirectDownload downloads a complete file from a direct URL (for VODs).
// Uses 5MB chunked Range requests with per-chunk retry and percentage progress.
// Falls back to streaming download if the server doesn't support Range requests.
func (d *SegmentDownloader) runDirectDownload(ctx context.Context) error {
	// Probe total file size via Range: bytes=0-0
	totalSize := d.probeFileSize(ctx)

	if totalSize <= 0 {
		// Server doesn't support Range requests -- fall back to streaming download
		return d.runDirectDownloadFallback(ctx)
	}

	// Chunked download with 5MB Range requests
	var offset int64
	lastProgressTime := time.Time{}

	for offset < totalSize {
		if d.isCancelled() || ctx.Err() != nil {
			return d.cancelErr(ctx)
		}

		end := offset + DownloadChunkSize - 1
		if end >= totalSize {
			end = totalSize - 1
		}

		data, statusCode, err := d.fetchChunkWithRetry(ctx, offset, end)
		if err != nil {
			if statusCode == http.StatusRequestedRangeNotSatisfiable {
				break // Past end of file
			}
			return fmt.Errorf("chunk download failed: %w", err)
		}

		if len(data) == 0 {
			break
		}

		n, writeErr := d.outputFile.Write(data)
		if writeErr != nil {
			return fmt.Errorf("write chunk: %w", writeErr)
		}
		offset += int64(n)
		d.bytesWritten.Store(offset)

		// Throttled progress emission
		now := time.Now()
		if d.OnProgress != nil && (now.Sub(lastProgressTime) >= ProgressThrottle || offset >= totalSize) {
			lastProgressTime = now
			pct := float64(offset) / float64(totalSize) * 100
			d.OnProgress(DownloadProgress{
				Bytes:      offset,
				TotalBytes: totalSize,
				Percent:    pct,
			})
		}
	}

	// Final 100% progress callback
	if d.OnProgress != nil {
		d.OnProgress(DownloadProgress{
			Bytes:      d.bytesWritten.Load(),
			TotalBytes: totalSize,
			Percent:    100,
		})
	}

	return nil
}

// runDirectDownloadFallback is the streaming fallback when Range requests are not supported.
func (d *SegmentDownloader) runDirectDownloadFallback(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.opts.BaseURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	d.setCommonHeaders(req, uaAndroid)

	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Read partial body for diagnostics
		bodySnippet := make([]byte, 1024)
		n, _ := resp.Body.Read(bodySnippet)
		d.logger.Debug("[Downloader] direct URL failed",
			"status", resp.StatusCode,
			"url_prefix", truncateURL(d.opts.BaseURL, 120),
			"body_snippet", string(bodySnippet[:n]),
		)
		return fmt.Errorf("HTTP %d downloading direct URL", resp.StatusCode)
	}

	buf := make([]byte, 64*1024) // 64KB buffer
	var lastProgressTime time.Time
	for {
		if d.isCancelled() || ctx.Err() != nil {
			return d.cancelErr(ctx)
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			written, writeErr := d.outputFile.Write(buf[:n])
			if writeErr != nil {
				return fmt.Errorf("write: %w", writeErr)
			}
			d.bytesWritten.Add(int64(written))

			if d.OnProgress != nil && time.Since(lastProgressTime) >= ProgressThrottle {
				lastProgressTime = time.Now()
				d.OnProgress(DownloadProgress{
					Bytes: d.bytesWritten.Load(),
				})
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read: %w", readErr)
		}
	}

	return nil
}
