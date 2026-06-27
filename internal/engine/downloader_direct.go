package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// validateDownloadedMP4 guards the whole-file VOD direct-download path against
// two failure shapes: a bare fragmented-MP4 media segment with no ftyp/moov
// init (the post-live "manifestless" case, where the bare format URL serves a
// single &sq segment) and an HTML/JSON error body saved as media. It does NOT
// require a specific container: a complete file may be MP4/M4A (leading 'ftyp'
// box) OR WebM (VP9/Opus, leading EBML magic 0x1A45DFA3) — both are valid and
// must pass. Only the known-bad shapes are rejected, so a corrupt download
// fails cleanly before FFmpeg instead of producing "moov atom not found".
func validateDownloadedMP4(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("validate download: %w", err)
	}
	defer f.Close()
	// Need at least a full box header: 4-byte size + 4-byte type.
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return fmt.Errorf("validate download: file too small or unreadable: %w", err)
	}
	// An HTML/JSON error body (e.g. a 403 page) saved as media.
	switch hdr[0] {
	case '<', '{', '[':
		return fmt.Errorf("downloaded file looks like a text/error response (leading byte %q), not media", string(hdr[0:1]))
	}
	// A bare fragmented-MP4 media segment lacking the ftyp/moov init — the
	// post-live single-segment failure. A complete MP4/M4A leads with 'ftyp'
	// and a complete WebM with the EBML magic; only a fragment leads with one
	// of these boxes.
	switch string(hdr[4:8]) {
	case "styp", "moof", "sidx", "mdat":
		return fmt.Errorf("downloaded file is a bare media fragment (leading box %q, no ftyp/moov init) — likely a post-live segment", string(hdr[4:8]))
	}
	return nil
}

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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.getBaseURL(), nil)
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
			"url_prefix", truncateURL(d.getBaseURL(), 120),
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
