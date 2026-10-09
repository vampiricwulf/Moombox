package engine

import (
	"context"
	"errors"
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
	totalSize, err := d.probeFileSizeWithRetry(ctx)
	if err != nil {
		return err
	}

	if totalSize <= 0 {
		// The server really does not support Range requests — stream it.
		// No reset here: the fallback resumes from d.bytesWritten with its
		// own Range header, and discards only if the server ignores it or
		// its answer states a total the partial is not a prefix of.
		return d.runDirectDownloadFallback(ctx)
	}

	// A resumed partial is a prefix of ONE file, and the probe has just said
	// how long the file behind this URL is (differentFileReason). Start a
	// different file over, the way an identity mismatch in Start does for
	// this path.
	if reason := d.differentFileReason(d.bytesWritten.Load(), totalSize, "the probe"); reason != "" {
		if err := d.discardStagedMedia(reason); err != nil {
			return err
		}
	}
	d.directTotalSize = totalSize

	// Chunked download with 5MB Range requests. Resume from the byte position
	// Start() restored: on a resumed run it loaded a valid resume sidecar,
	// validated identity (the caller's StreamID, then the URL's itag/clen
	// fingerprint) and file size, and truncated the output to the fsync'd
	// offset + opened O_APPEND, and the block above has held the partial to
	// the probed total — so continuing from d.bytesWritten appends cleanly
	// to the same file. A hard crash that lost
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
				// The loop runs only while offset < totalSize, so a 416 here
				// is never "past end of file": the probe said there is more.
				// It used to break as if it were, clear the sidecar and pass
				// a truncated file to validation, which reads the header
				// alone — the job finished over a short archive. An error
				// keeps the sidecar for a Resume, whose fresh probe settles
				// whether the origin's file really changed.
				return directShortFileError("416 Range Not Satisfiable", offset, totalSize)
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
			// Same reading as the 416 above: below the probed total an empty
			// 206 is a short origin, not the end of the file.
			return directShortFileError("an empty 206", offset, totalSize)
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

// directRefreshAttempts bounds the URL refreshes one chunk may ask for after a
// 403 or 410: one for the URL that expired, and one more for a refresh whose
// URL the origin still refused (a token the refresh could not re-mint, say).
// A third refusal of the same chunk is not an expiry, and the error stands.
const directRefreshAttempts = 2

// refreshDirectURL answers a 403 or 410 on a whole-file chunk — the
// whole-file twin of the segmented paths' refreshCredentials. It asks
// OnCredentialRefresh for a fresh URL and token and installs whatever comes
// back for the retry.
//
// A fresh URL must name the same file. The partial on disk is a prefix of ONE
// rendition, and a URL for another — a re-extraction whose pool moved —
// would append it mid-file: the splice resumeIdentityMismatch refuses at
// Start. Its fingerprint (streamIdentity) must match the current URL's, and a
// mismatch is an error, which keeps the sidecar for a Resume that selects
// from scratch. Nothing returned at all is an error too: retrying the URL
// that was just refused only spends the attempt. No cooldown, unlike
// refreshCredentials — this path fetches one chunk at a time, and
// directRefreshAttempts bounds it per chunk.
func (d *SegmentDownloader) refreshDirectURL(status int) error {
	freshURL, freshToken := d.opts.OnCredentialRefresh()
	if freshURL == "" && freshToken == "" {
		return errors.New("the URL refresh returned nothing")
	}
	if freshURL != "" {
		if was, now := streamIdentity(d.getBaseURL()), streamIdentity(freshURL); was != now {
			d.logger.Warn("[Downloader] Refreshed whole-file URL names a different stream — refusing it",
				"status", status, "currentIdentity", was, "freshIdentity", now)
			return fmt.Errorf("the refreshed URL names a different stream (%q, not %q) — refusing to append it", now, was)
		}
		d.SetBaseURL(freshURL)
	}
	d.SetPoToken(freshToken)
	d.logger.Info("[Downloader] Whole-file URL refreshed after a refused chunk",
		"status", status, "newURL", freshURL != "", "newToken", freshToken != "")
	return nil
}

// directShortFileError reports an origin that stopped serving bytes before the
// total its size probe declared. It is an error, never a completion: the
// caller returns it without clearing the resume sidecar, so the staged bytes
// stay resumable and the job does not finish over a truncated file.
func directShortFileError(answer string, offset, totalSize int64) error {
	return fmt.Errorf("origin answered %s at byte %d of %d — the file ends short of its probed size",
		answer, offset, totalSize)
}

// differentFileReason says why staged bytes cannot be a prefix of a file whose
// origin, asked by source, states it is total bytes long — or "" when they can
// be, or when nothing is staged. A total that differs from the one the sidecar
// was saved against, or that the partial already overruns, is a different
// file: a different rendition the selection picked this time, or a re-encode
// under the same itag. Appending to it was the splice the identity check
// alone could not catch on a URL without `clen`.
//
// Both whole-file paths ask it of the first total they are told: the chunked
// loop of its size probe, and the streaming fallback — which a failed probe
// routes a resume into — of the Content-Range its resume Range is answered
// with.
func (d *SegmentDownloader) differentFileReason(staged, total int64, source string) string {
	switch {
	case staged <= 0:
		return ""
	case d.directTotalSize > 0 && total != d.directTotalSize:
		return fmt.Sprintf("%s states a total of %d, not the %d the resume state was saved against — a different file",
			source, total, d.directTotalSize)
	case staged > total:
		return fmt.Sprintf("%d bytes staged but %s states a total of %d — a different file", staged, source, total)
	}
	return ""
}

// discardStagedMedia is the ONLY place staged media is destroyed on purpose.
// It reopens OutputFile O_TRUNC — not d.outputFile.Truncate, because Windows
// refuses ftruncate on an O_APPEND handle ("Access is denied") and reopening
// also drops the append flag so writes land from byte 0 — zeroes the byte
// counter and clears the resume sidecar. Start's deferred Close reads
// d.outputFile at exit, so reassigning it is safe. No-op when nothing has
// been written yet.
//
// It forgets the recorded total too: that was the length of the file just
// discarded, and a checkpoint of whatever is fetched next must not hold it to
// the old one's. The chunked loop records its probe's total straight after;
// the streaming fallback, whose 200 may state none, records nothing.
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
	d.directTotalSize = 0
	d.ClearResume()
	return nil
}

// runDirectDownloadFallback streams the file when Range chunking is not
// available. It still SENDS a Range from the resume offset: the fallback used
// to open at byte 0 unconditionally, so a transient probe failure on a
// resumed VOD threw the staged bytes away (sweep-2 ENGINE-6). Only a server
// that answers 200 to that Range — i.e. one that is sending from byte 0 — or
// whose answer names another file (below) forces a discard, and that discard
// is explicit.
//
// A resume reaches this path when the size probe failed, so the check the
// chunked loop makes of the probe's total — is the partial a prefix of this
// file? — is made here of the total the answer itself states
// (differentFileReason): a 206 or 416 naming another file discards the
// partial and streams the file again from byte 0 (restartDirectFallback).
// Without it an outage that failed the probe let a resume append one file's
// tail to another's checkpoint, or finish a partial longer than the file.
//
// A refused or broken request is answered as the chunked loop answers one
// (fetchChunkWithRetry), and asked again from wherever the file stands —
// the resume Range above makes that free:
//
//   - a 403 or 410 asks OnCredentialRefresh for a fresh URL
//     (refreshDirectURL), at most directRefreshAttempts times without a byte
//     written between them. A probe that failed sends a download here, and a
//     mid-download 200 hands one here; a URL that expired on the way ended
//     the job on its 403;
//   - a request that got no complete answer — none at all, or a body that
//     broke off — while IsOnline reports the device offline, or once the
//     monitor, given the time it needs (awaitOutageVerdict), calls an
//     outage, waits it out. One dropped connection ended the job, however
//     much of the file had streamed.
//
// Anything else returns as it did, a failure the monitor does not call an
// outage among them.
func (d *SegmentDownloader) runDirectDownloadFallback(parent context.Context) error {
	refreshes := 0
	for {
		before := d.bytesWritten.Load()
		status, linkFailed, err := d.streamDirectOnce(parent)
		if err == nil {
			return nil
		}
		if d.bytesWritten.Load() != before {
			refreshes = 0 // a refusal after progress is a new expiry
		}
		switch {
		case status == http.StatusForbidden || status == http.StatusGone:
			if d.opts.OnCredentialRefresh == nil || refreshes >= directRefreshAttempts {
				return err
			}
			refreshes++
			if rerr := d.refreshDirectURL(status); rerr != nil {
				return fmt.Errorf("%w; %w", err, rerr)
			}
		case linkFailed && d.opts.IsOnline != nil:
			if d.opts.IsOnline() && !d.awaitOutageVerdict(parent) {
				// A cancel that ended the verdict wait is still a cancel.
				if cerr := d.cancelErr(parent); cerr != nil {
					return cerr
				}
				return err
			}
			d.emitActivity(ActivityReconnecting)
			if werr := waitForConnectivity(parent, d.opts.IsOnline, d.delays.connectivityPoll); werr != nil {
				return d.cancelErr(parent)
			}
		default:
			return err
		}
	}
}

// streamDirectOnce is one request of runDirectDownloadFallback: it asks for
// the file from the resume offset and streams the answer to disk. It returns
// the status the request was answered with (0 when it got no answer) and
// whether it failed for want of a complete answer — no response at all, or a
// body that broke off mid-read — which is the link failing, not the origin
// refusing.
//
// The whole transfer runs under the same read-progress (idle) deadline the
// segment and chunk fetches use. It is the only bound this GET has: the
// client-level Timeout that used to cap it went away with ENGINE-4, and a
// total deadline is the wrong shape anyway for a multi-GB VOD streamed in one
// response.
func (d *SegmentDownloader) streamDirectOnce(parent context.Context) (int, bool, error) {
	idle := SegmentTimeout
	ctx, idleTimer, cancel := withReadProgressDeadline(parent, idle)
	defer cancel()

	offset := d.bytesWritten.Load()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, applyPoTokenQuery(d.getBaseURL(), d.getPoToken()), nil)
	if err != nil {
		return 0, false, fmt.Errorf("create request: %w", err)
	}
	d.setCommonHeaders(req, uaAndroid)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		reportFetchFailure(parent, "engine/fetch")
		return 0, true, idleFetchError(ctx, idle, fmt.Errorf("download: %w", redactPoToken(err)))
	}
	reportSuccess("engine/fetch")
	resp.Body = &idleBody{rc: resp.Body, timer: idleTimer, idle: idle}
	defer resp.Body.Close()
	status := resp.StatusCode

	switch status {
	case http.StatusPartialContent:
		// Range honoured — but ONLY if the body really starts where we asked.
		// This is the one path that meets a 206 with no known total size, so
		// an origin answering from a different offset just makes the file
		// grow, and neither validateDownloadedMP4 nor the mux notices: the
		// job reports Finished over a spliced archive (sweep-2 B-I1).
		start, ok := parseContentRangeStart(resp.Header)
		switch {
		case ok && start == offset:
			// The body continues where the file stops — when it is the same
			// file, which the total this answer states settles.
			if total, known := parseContentRangeTotal(resp.Header); known {
				if reason := d.differentFileReason(offset, total, "the resume Range's 206"); reason != "" {
					return d.restartDirectFallback(parent, resp, reason)
				}
			}
		case ok && start == 0 && offset > 0:
			// Same shape as the 200 below — the origin restarted from the
			// top and labelled it honestly, so the staged bytes must go.
			if derr := d.discardStagedMedia("206 Content-Range starts at byte 0, not the resume offset"); derr != nil {
				return status, false, derr
			}
		default:
			// Nothing written yet, so the staged bytes and the sidecar both
			// survive for the next attempt.
			return status, false, fmt.Errorf("origin answered Range %d with Content-Range start %d (header %q)",
				offset, start, resp.Header.Get("Content-Range"))
		}
	case http.StatusOK:
		if offset > 0 {
			if derr := d.discardStagedMedia("server answered 200 to the resume Range — the body starts at byte 0"); derr != nil {
				return status, false, derr
			}
		}
	default:
		if status == http.StatusRequestedRangeNotSatisfiable && offset > 0 {
			// The resume offset is at or past EOF — complete only if the
			// staged bytes are the whole file. The total a 416 may state
			// (`bytes */<total>`) is held to the partial first, as a 206's
			// is; without one, the total the sidecar was saved against is
			// the length the file has. A known total other than the offset
			// is the short origin the chunked loop reads a 416 below its
			// total as (directShortFileError), and the sidecar is kept for a
			// Resume whose probe settles it. Knowing neither — a legacy
			// sidecar — the origin's word is all there is, as for the
			// pre-arc fallback, which sent no Range and re-fetched the file.
			total, known := parseContentRangeTotal(resp.Header)
			if known {
				if reason := d.differentFileReason(offset, total, "the resume Range's 416"); reason != "" {
					return d.restartDirectFallback(parent, resp, reason)
				}
			} else {
				total = d.directTotalSize
			}
			if total > 0 && total != offset {
				return status, false, directShortFileError("416 Range Not Satisfiable", offset, total)
			}
			d.logger.Info("[Downloader] Resume offset is at or past EOF — staged file is already complete",
				"offset", offset)
			d.ClearResume()
			return status, false, nil
		}
		// Read partial body for diagnostics
		bodySnippet := make([]byte, 1024)
		n, _ := resp.Body.Read(bodySnippet)
		d.logger.Debug("[Downloader] direct URL failed",
			"status", status,
			"url_prefix", truncateURL(d.getBaseURL(), 120),
			"body_snippet", string(bodySnippet[:n]),
		)
		return status, false, fmt.Errorf("HTTP %d downloading direct URL", status)
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
			return status, false, d.cancelErr(parent)
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			d.noteFetch(n)
			written, writeErr := d.outputFile.Write(buf[:n])
			if writeErr != nil {
				return status, false, fmt.Errorf("%w: write: %w", ErrLocalWrite, writeErr)
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
			return status, true, idleFetchError(ctx, idle, fmt.Errorf("read: %w", readErr))
		}
	}

	// Fully downloaded — clear the resume sidecar, exactly as the chunked
	// path does on its own completion. Both hand-offs into this function
	// return straight to Start, so the chunked path's ClearResume is never
	// reached from here; without this a crash between "download complete" and
	// "mux" would leave a stale sidecar that truncates the COMPLETE file back
	// to its offset on the next run (sweep-2 B-M1).
	d.ClearResume()
	return status, false, nil
}

// restartDirectFallback answers a resume Range whose answer names a different
// file (differentFileReason): the body it carries starts at the resume offset
// of the wrong file, so none of it is usable. It closes that response,
// discards the partial and streams the file again. The discard zeroes the
// byte counter, so the second request sends no Range and nothing that leads
// here can fire on it — one level deep at most.
func (d *SegmentDownloader) restartDirectFallback(parent context.Context, resp *http.Response, reason string) (int, bool, error) {
	resp.Body.Close()
	if err := d.discardStagedMedia(reason); err != nil {
		return resp.StatusCode, false, err
	}
	return d.streamDirectOnce(parent)
}
