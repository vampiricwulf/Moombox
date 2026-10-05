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

// directResumeInterval is how many bytes a whole-file download writes between
// resume checkpoints — ten 5 MB chunks. An interrupted multi-GB VOD then
// resumes from its last checkpoint instead of re-downloading from byte 0, and
// the cadence keeps the save (which now fsyncs the media first, owner decision
// O-G) off the hot path: once per 50 MB is nothing beside the transfer itself.
//
// BOTH whole-file paths save on it — the chunked loop and the streaming
// fallback — deliberately through the one constant rather than a second
// cadence of the fallback's own.
const directResumeInterval = 10 * DownloadChunkSize

// directResumeIntervalBytes is directResumeInterval, or the test override when
// one is set (see SegmentDownloader.directResumeIntervalOverride).
func (d *SegmentDownloader) directResumeIntervalBytes() int64 {
	if d.directResumeIntervalOverride > 0 {
		return d.directResumeIntervalOverride
	}
	return directResumeInterval
}

// runDirectDownload downloads a complete file from a direct URL (for VODs).
// Uses 5MB chunked Range requests with per-chunk retry and percentage progress.
// Falls back to streaming download if the server doesn't support Range requests.
func (d *SegmentDownloader) runDirectDownload(ctx context.Context) error {
	// Probe total file size via Range: bytes=0-0, retried so one transient
	// failure cannot route a resumable download into the streaming fallback.
	totalSize := d.probeFileSizeWithRetry(ctx)

	if totalSize <= 0 {
		// The server really does not support Range requests — stream it.
		// No reset here: the fallback resumes from d.bytesWritten with its
		// own Range header and discards only if the server ignores it.
		return d.runDirectDownloadFallback(ctx)
	}

	// Chunked download with 5MB Range requests. Resume from the byte position
	// Start() restored: on a resumed run it loaded a valid resume sidecar,
	// validated identity (itag-bearing googlevideo URL) and file size, and
	// truncated the output to the fsync'd offset + opened O_APPEND — so
	// continuing from d.bytesWritten appends cleanly. A hard crash that lost
	// the file's tail fails Start's size check and restarts fresh, so this
	// can never splice a torn tail. Fresh runs start at 0 (bytesWritten==0).
	offset := d.bytesWritten.Load()
	lastSavedOffset := offset
	resumeInterval := d.directResumeIntervalBytes()
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
			// Already phrased by fetchChunkWithRetry ("chunk download failed
			// after N attempts: <cause>"); a second prefix here used to
			// double it.
			return err
		}

		// A 200 (not 206) means the server IGNORED the Range and sent the whole
		// file from byte 0. Writing that at the current offset would splice the
		// file's leading bytes into the middle of the output (doubled/corrupt),
		// and it's capped at maxIgnoredRangeBodyBytes so it's also truncated.
		// The probe returned a size, so this is an inconsistent/interleaved
		// backend — abandon the chunked approach and hand over to the
		// streaming fallback rather than write byte-0 data at offset>0. No
		// reset here either: the fallback re-asks with its own Range from
		// this same offset, and only a second 200 forces the discard.
		if statusCode == http.StatusOK {
			d.logger.Warn("[Downloader] direct chunk got 200 (Range ignored) mid-download; restarting via streaming",
				"offset", offset)
			return d.runDirectDownloadFallback(ctx)
		}

		if len(data) == 0 {
			break
		}
		d.noteFetch(len(data))

		n, writeErr := d.outputFile.Write(data)
		if writeErr != nil {
			return fmt.Errorf("%w: write chunk: %w", ErrLocalWrite, writeErr)
		}
		offset += int64(n)
		d.bytesWritten.Store(offset)

		// Persist resume progress periodically (see directResumeInterval).
		if offset-lastSavedOffset >= resumeInterval {
			d.saveResume()
			lastSavedOffset = offset
		}

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

	// Fully downloaded — clear the resume sidecar so a later run doesn't try
	// to append to a complete file.
	d.ClearResume()

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

// discardStagedMedia is the ONLY place staged media is destroyed on purpose.
// It reopens OutputFile O_TRUNC — not d.outputFile.Truncate, because Windows
// refuses ftruncate on an O_APPEND handle ("Access is denied") and reopening
// also drops the append flag so writes land from byte 0 — zeroes the byte
// counter and clears the resume sidecar. Start's deferred Close reads
// d.outputFile at exit, so reassigning it is safe. No-op when nothing has
// been written yet.
//
// reason is logged: every discard must be attributable, because the guard in
// Start (ErrStagedMediaPresent) exists precisely so that nothing else can do
// this silently.
func (d *SegmentDownloader) discardStagedMedia(reason string) error {
	if d.bytesWritten.Load() == 0 {
		return nil
	}
	d.logger.Warn("[Downloader] Discarding staged media", "file", d.opts.OutputFile,
		"bytes", d.bytesWritten.Load(), "reason", reason)
	d.outputFile.Close()
	f, err := os.OpenFile(d.opts.OutputFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("discard staged media: %w", err)
	}
	d.outputFile = f
	d.bytesWritten.Store(0)
	d.ClearResume()
	return nil
}

// runDirectDownloadFallback streams the file when Range chunking is not
// available. It still SENDS a Range from the resume offset: the fallback used
// to open at byte 0 unconditionally, so a transient probe failure on a
// resumed VOD threw the staged bytes away (sweep-2 ENGINE-6). Only a server
// that answers 200 to that Range — i.e. one that is sending from byte 0 —
// forces a discard, and that discard is explicit.
//
// The whole transfer runs under the same read-progress (idle) deadline the
// segment and chunk fetches use. It is the only bound this GET has: the
// client-level Timeout that used to cap it went away with ENGINE-4, and a
// total deadline is the wrong shape anyway for a multi-GB VOD streamed in one
// response.
func (d *SegmentDownloader) runDirectDownloadFallback(parent context.Context) error {
	idle := SegmentTimeout
	ctx, idleTimer, cancel := withReadProgressDeadline(parent, idle)
	defer cancel()

	offset := d.bytesWritten.Load()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, applyPoTokenQuery(d.getBaseURL(), d.getPoToken()), nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	d.setCommonHeaders(req, uaAndroid)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		return idleFetchError(ctx, idle, fmt.Errorf("download: %w", redactPoToken(err)))
	}
	resp.Body = &idleBody{rc: resp.Body, timer: idleTimer, idle: idle}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		// Range honoured — but ONLY if the body really starts where we asked.
		// This is the one path that meets a 206 with no known total size, so
		// an origin answering from a different offset just makes the file
		// grow, and neither validateDownloadedMP4 nor the mux notices: the
		// job reports Finished over a spliced archive (sweep-2 B-I1).
		start, ok := parseContentRangeStart(resp.Header)
		switch {
		case ok && start == offset:
			// The body continues where the file stops.
		case ok && start == 0 && offset > 0:
			// Same shape as the 200 below — the origin restarted from the
			// top and labelled it honestly, so the staged bytes must go.
			if derr := d.discardStagedMedia("206 Content-Range starts at byte 0, not the resume offset"); derr != nil {
				return derr
			}
		default:
			// Nothing written yet, so the staged bytes and the sidecar both
			// survive for the next attempt.
			return fmt.Errorf("origin answered Range %d with Content-Range start %d (header %q)",
				offset, start, resp.Header.Get("Content-Range"))
		}
	case http.StatusOK:
		if offset > 0 {
			if derr := d.discardStagedMedia("server answered 200 to the resume Range — the body starts at byte 0"); derr != nil {
				return derr
			}
		}
	default:
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && offset > 0 {
			// The resume offset is at or past EOF: the staged file already
			// holds everything the origin has. The chunked loop reads 416 the
			// same way (past end of file) and so did the pre-arc fallback,
			// which sent no Range and simply re-fetched the whole file.
			d.logger.Info("[Downloader] Resume offset is at or past EOF — staged file is already complete",
				"offset", offset)
			d.ClearResume()
			return nil
		}
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
	// Read AFTER the switch above: a discard there reset the counter to zero,
	// and the checkpoint cadence measures from wherever this transfer starts.
	// Without these saves the fallback streamed gigabytes with nothing on disk
	// describing them, so an interruption cost the whole partial — the chunked
	// loop's 50 MB cadence, applied to the path that has no chunks.
	lastSavedOffset := d.bytesWritten.Load()
	resumeInterval := d.directResumeIntervalBytes()
	for {
		// The CALLER's context, not the derived one: an idle stall is a
		// network failure the read below surfaces as such, while a cancel
		// from above is a shutdown and must stay one.
		if d.isCancelled() || parent.Err() != nil {
			return d.cancelErr(parent)
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			d.noteFetch(n)
			written, writeErr := d.outputFile.Write(buf[:n])
			if writeErr != nil {
				return fmt.Errorf("%w: write: %w", ErrLocalWrite, writeErr)
			}
			stagedBytes := d.bytesWritten.Add(int64(written))

			// Same cadence as the chunked loop (directResumeInterval).
			if stagedBytes-lastSavedOffset >= resumeInterval {
				d.saveResume()
				lastSavedOffset = stagedBytes
			}

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
			return idleFetchError(ctx, idle, fmt.Errorf("read: %w", readErr))
		}
	}

	// Fully downloaded — clear the resume sidecar, exactly as the chunked
	// path does on its own completion. Both hand-offs into this function
	// return straight to Start, so the chunked path's ClearResume is never
	// reached from here; without this a crash between "download complete" and
	// "mux" would leave a stale sidecar that truncates the COMPLETE file back
	// to its offset on the next run (sweep-2 B-M1).
	d.ClearResume()
	return nil
}
