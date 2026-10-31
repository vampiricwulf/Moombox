package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// initForbiddenAttempts bounds how many times fetchInitSegment asks for an
// init segment that fetchSegmentWithRetry reported gone (403/410): the first
// try and two more, each after a credential refresh. fetchSegmentWithRetry's
// own 403 refresh only runs for a segment it can prove exists below the known
// head, and at Start the head is not known yet, so without these a single 403
// on the init — a stale n/sig or PO token at the moment a part begins, the
// case refreshCredentials exists for — ended the part. Kept small for the
// same reason forbiddenRefreshAttempts is: each refresh is a player-response
// round trip plus a mint, and a URL still refused after two of them is not
// one a third will fix; the orchestrator's refresh builds the part again.
const initForbiddenAttempts = 3

// fetchInitSegment fetches an init segment — a manifest Initialization, or the
// sq=0 a manifest-free stream carries its ftyp+moov in — the way yt-dlp
// fetches a DASH download's fragment 0: retried, never skipped
// (downloader/fragment.py retries it fragment_retries times and then fails
// the download; downloader/dash.py marks index 0 fatal). The 5xx, timeout
// and outage ladder is fetchSegmentWithRetry's; on top of it an
// answer of 403/410 (ErrSegmentPermanent) is tried initForbiddenAttempts
// times in all, each retry after refreshCredentials and, when rebuildURL is
// non-nil, on a URL rebuilt from the base that refresh installed — never on
// the construction-time string, which is exactly the credential that was
// refused. Any other error, and the last 403/410, is returned as is.
func (d *SegmentDownloader) fetchInitSegment(ctx context.Context, segURL string, rebuildURL func() string) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		data, err := d.fetchSegmentWithRetry(ctx, segURL, rebuildURL)
		if err == nil || !errors.Is(err, ErrSegmentPermanent) || attempt >= initForbiddenAttempts {
			return data, err
		}
		d.emitActivity(ActivityRetrying)
		if serr := utils.Sleep(ctx, d.initRefreshWait()); serr != nil {
			return nil, serr
		}
		d.refreshCredentials()
		if rebuildURL != nil {
			segURL = rebuildURL()
		}
	}
}

// initRefreshWait is the pause before fetchInitSegment's next refresh: long
// enough that refreshCredentials can claim a fresh credentialRefreshCooldown
// window, so every retry really is on new credentials — a retry inside a
// closed cooldown re-sends the refused URL, the trap forbiddenRefreshAttempts'
// INVARIANT describes. The first refresh of a downloader's life never waits
// on it (no claim yet); one that fetchSegmentWithRetry's behind-head refresh
// just claimed does. Without a refresh callback there is nothing to wait for,
// and the pause is the ordinary singleGoneRetry.
func (d *SegmentDownloader) initRefreshWait() time.Duration {
	wait := d.delays.singleGoneRetry
	if d.opts.OnCredentialRefresh == nil {
		return wait
	}
	if last := d.lastCredentialRefresh.Load(); !last.IsZero() {
		wait = max(wait, credentialRefreshCooldown-time.Since(last))
	}
	return wait
}

// writeInitFromSq0 fetches a manifest-free stream's sq=0 — at firstURL, and
// after a refresh at buildSegmentURL(0) on the current base — and writes only
// the ftyp+moov at its head (extractMP4InitBoxes), so segment 0's media does
// not prefix a file whose media starts later.
func (d *SegmentDownloader) writeInitFromSq0(ctx context.Context, firstURL string) error {
	data, err := d.fetchInitSegment(ctx, firstURL, func() string { return d.buildSegmentURL(0) })
	if err != nil {
		return fmt.Errorf("sq=0 init: %w", err)
	}
	init := extractMP4InitBoxes(data)
	if init == nil {
		return fmt.Errorf("sq=0 init: no ftyp/moov init found in sq=0 (%d bytes)", len(data))
	}
	return d.writeInitBytes(init)
}

// writeInitBytes appends an init to the output file. A failed write is the
// staging's, not the stream's: ErrLocalWrite, which the orchestrator stops
// on rather than reading as a stream that ended.
func (d *SegmentDownloader) writeInitBytes(data []byte) error {
	n, err := d.outputFile.Write(data)
	if err != nil {
		return fmt.Errorf("%w: write init segment: %w", ErrLocalWrite, err)
	}
	d.bytesWritten.Add(int64(n))
	return nil
}

// ensureInlineInit runs before every media write of the DASH loop and its
// catch-up flush. For an InitInlineAtStart downloader whose file is still
// EMPTY and whose next write is a sequence above 0 — media that would open
// the file without the init only sq=0 carries — it first writes sq=0's
// ftyp+moov, fetched from the CURRENT base (an OnCipherFailure swap or a
// credential refresh may have replaced the one the downloader started with);
// when that is impossible it fails the start with ErrInitSegmentUnavailable
// rather than record a file FFmpeg rejects. Otherwise it does nothing.
//
// yt-dlp needs no such guard — its live-from-start generator begins at sq=0
// and fragment 0 is fatal, so nothing is ever appended without it — and here
// huntMayAdvance keeps the first-segment hunt on sq=0 for exactly this
// downloader, which is what used to open the file above 0: the hunt stepped
// past a refused sq=0 (even after OnCipherFailure had repaired the URL) and
// the recording began at sq=1 with no init, hours of capture that could not
// be muxed — the "moov atom not found" Moombox 1c2840104 fixed for the
// forced-start parts only. The guard covers the routes that can still open
// such a file above 0: a resume sidecar naming a position past 0 with no
// bytes (an older build wrote them for a hunt that had written nothing;
// saveResume no longer does, and loadResume still accepts one), and any later
// path that moves currentSeq before the first byte.
func (d *SegmentDownloader) ensureInlineInit(ctx context.Context, seq int) error {
	if !d.opts.InitInlineAtStart || seq <= 0 || d.bytesWritten.Load() > 0 {
		return nil
	}
	d.logger.Warn("[Downloader] first media segment is past sq=0 — writing sq=0's init ahead of it",
		"seq", seq)
	err := d.writeInitFromSq0(ctx, d.buildSegmentURL(0))
	if err == nil {
		return nil
	}
	if cerr := d.cancelErr(ctx); cerr != nil {
		return cerr
	}
	if errors.Is(err, ErrLocalWrite) {
		return err
	}
	d.logger.Error("[Downloader] sq=0 init unavailable — not writing media FFmpeg could not mux",
		"seq", seq, "error", err)
	return fmt.Errorf("%w: ahead of segment %d: %w", ErrInitSegmentUnavailable, seq, err)
}

// extractMP4InitBoxes returns the leading ftyp+moov initialization boxes from a
// fragmented-MP4 segment — everything before the first media box (moof / mdat /
// styp / sidx). Returns nil when the data is too short, malformed, or has no
// ftyp box (i.e. it is a bare media fragment, not an init-carrying segment).
//
// Manifest-free DASH parts carry their init only inline at sq=0 (no separate
// init URL), so a part that force-starts at sq>0 must derive its ftyp+moov from
// a one-shot sq=0 fetch. Extracting just the init — rather than prepending the
// whole sq=0 segment — keeps segment 0's media from leaking into a later part.
func extractMP4InitBoxes(data []byte) []byte {
	off := 0
	sawFtyp := false
	for off+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[off : off+4]))
		boxType := string(data[off+4 : off+8])
		switch boxType {
		case "moof", "mdat", "styp", "sidx":
			// First media box — the init is everything before it.
			if sawFtyp && off > 0 {
				return data[:off]
			}
			return nil
		case "ftyp":
			sawFtyp = true
		}
		switch {
		case size == 1:
			// 64-bit extended size in the 8 bytes following the box header.
			if off+16 > len(data) {
				return nil
			}
			size64 := binary.BigEndian.Uint64(data[off+8 : off+16])
			// Reject a garbage/oversized size on two grounds: the absolute
			// cap (size64 > len(data)) also guards int(size64) against
			// overflowing into a negative offset, and — with that cap making
			// off+int(size64) safe from wraparound — the off-relative check
			// rejects a box that claims to extend past the fetched buffer
			// rather than jumping over a real media box that follows.
			if size64 < 16 || size64 > uint64(len(data)) || off+int(size64) > len(data) {
				return nil
			}
			off += int(size64)
		case size < 8:
			// size 0 means "to EOF"; anything < 8 is a malformed header.
			return nil
		case off+size > len(data):
			// Box overshoots the fetched buffer. Advancing off past it would
			// skip a real moof/mdat and make the loop fall through to
			// "return whole buffer", emitting a truncated/media-contaminated
			// init. Treat as malformed so the caller's missing-init path fires.
			return nil
		default:
			off += size
		}
	}
	// No media box encountered — if an ftyp was seen the whole buffer is init.
	if sawFtyp {
		return data
	}
	return nil
}
