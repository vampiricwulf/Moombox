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

	"github.com/vampiricwulf/Moombox/internal/httpx"
	"github.com/vampiricwulf/Moombox/internal/utils"
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
// MaxIdleConnsPerHost is bumped beyond httpx's default of 8 because
// engine fans out parallel segment fetches at ParallelDownloads + 2.
// Built via httpx.NewTransport so the keep-alive tuning stays in sync
// with the rest of the codebase.
var engineHTTPClient = httpx.ClientWithTransport(
	5*time.Minute,
	httpx.NewTransport(httpx.TransportOptions{
		MaxIdleConnsPerHost: ParallelDownloads + 2,
	}),
)

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
	// errorBodySnippetBytes caps how much of an HTTP-error response body
	// we include in the returned error message. Enough to capture
	// YouTube's JSON errors or a one-line HTML title, but small enough
	// that a chatty error page doesn't explode the log line.
	errorBodySnippetBytes = 512
	// maxDrainBytes caps a best-effort body drain done purely to return a
	// keep-alive connection to the idle pool. The bodies we drain (head
	// probes, the trailing bytes after a bounded ReadAll) are expected to be
	// tiny; the cap ensures a misbehaving edge can't make us pull megabytes
	// just to reclaim one socket.
	maxDrainBytes = 64 << 10
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
	segURL = applyPoTokenQuery(segURL, d.getPoToken())

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

	// Harvest the live-head sequence YouTube attaches to GVS segment
	// responses — ordinary fetches then keep headSeq fresh for free and
	// the dedicated probe only fires when segment traffic has gone quiet
	// (yt-dlp 8c1f07d81 reads the same header via dedicated HEAD requests).
	d.noteHeadSeqFromResponse(resp)

	if resp.StatusCode >= 400 {
		// Read a short body snippet for diagnostic error messages. YouTube
		// returns useful JSON on 403 (cipher issues, pot-token rejection)
		// and HTML on 4xx from the CDN. We cap at errorBodySnippetBytes so
		// a chatty error page can't blow the log line up.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodySnippetBytes))
		if len(snippet) > 0 {
			return nil, resp.StatusCode, fmt.Errorf("HTTP %d: %s",
				resp.StatusCode, strings.TrimSpace(string(snippet)))
		}
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSegmentBodyBytes))
	if err != nil {
		return nil, resp.StatusCode, err
	}

	return data, resp.StatusCode, nil
}

// fetchSegmentWithRetry attempts to fetch a segment with retries and exponential backoff.
// Returns:
//   - (data, nil): success.
//   - (nil, ErrSegmentPermanent): segment is gone for good (403/410). Don't retry.
//   - (nil, ErrSegmentRetriesExhausted): transient failure (timeout, 5xx, retries exhausted).
//   - (nil, ctx.Err()): caller's context was cancelled or downloader was cancelled.
//
// Consumers use `errors.Is(err, ErrSegmentPermanent)` to distinguish the
// permanent vs transient cases. Audit reports/engine.md #17: previously
// the function returned `(body, permanent bool)` which forced every
// caller to handle nil-vs-flag explicitly; the sentinel form is more
// composable and lets callers route through `errors.Is`.
func (d *SegmentDownloader) fetchSegmentWithRetry(ctx context.Context, segURL string) ([]byte, error) {
	// d.opts.MaxRetries (defaulted to MaxSegmentRetries in the constructor) —
	// previously this loop used the constant directly, silently ignoring the
	// documented DownloaderOptions.MaxRetries knob.
	for attempt := range d.opts.MaxRetries {
		if d.isCancelled() {
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			return nil, context.Canceled
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		body, status, err := d.fetchSegment(ctx, segURL)
		if err == nil && status < 400 {
			return body, nil
		}
		if status == 403 || status == 410 {
			return nil, ErrSegmentPermanent
		}
		// Surface the backoff in the progress line — the tracker's grace
		// window suppresses this while other segments are still landing, so
		// only a real stall shows it.
		d.emitActivity(ActivityRetrying)
		// Skip the backoff after the final attempt: no fetch follows it, so
		// the sleep only delays the already-decided ErrSegmentRetriesExhausted
		// (up to ~25s at the default MaxRetries=5), keeping a catch-up worker
		// and its un-flushable buffer entry alive that much longer.
		if attempt < d.opts.MaxRetries-1 {
			utils.Sleep(ctx, time.Duration(5*(attempt+1))*time.Second)
		}
	}
	return nil, ErrSegmentRetriesExhausted
}

// noteHeadSeqFromResponse harvests the X-Head-Seqnum header from a segment
// response. YouTube's GVS attaches the current head sequence to segment
// responses for live/post-live content, so every ordinary fetch doubles as
// a head probe. Bumping lastHeadProbeTime here makes the dedicated probes
// in runDashLoop / handleHTTPError fire only when no segment has refreshed
// head within HeadProbeInterval (cold start, stall) — saving one probe
// round-trip per interval per downloader on a healthy stream. Non-GVS
// responses (Twitch HLS, error pages) simply lack the header — no-op.
func (d *SegmentDownloader) noteHeadSeqFromResponse(resp *http.Response) {
	v := resp.Header.Get("X-Head-Seqnum")
	if v == "" {
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return
	}
	if d.noteHeadSeq(n) {
		// Refresh the probe-pacing clock only when the harvest ADVANCED
		// head: a healthy live edge keeps advancing (dedicated probes stay
		// suppressed), while a stall — or a stuck edge echoing the same
		// stale value — lets the dedicated probe re-arm after
		// HeadProbeInterval as the correctness backstop.
		d.lastHeadProbeTime.StoreNow()
	}
}

// noteHeadSeq advances headSeq to n when greater (monotonic max) and
// reports whether it advanced. Harvested headers route through here —
// concurrent catch-up workers can deliver them out-of-order, and an
// unguarded Store from a stale CDN edge could drop head below currentSeq
// and silently disarm behindHeadTailPending / warnIfFinalizingBehindHead
// at the exact moment they matter. Within one SegmentDownloader's
// lifetime head never genuinely decreases (URL/quality refreshes
// construct a NEW downloader); the dedicated probes use
// noteHeadSeqFromProbe, which can additionally correct a poisoned value.
func (d *SegmentDownloader) noteHeadSeq(n int) bool {
	if n < 0 {
		return false
	}
	for {
		cur := d.headSeq.Load()
		if int64(n) <= cur {
			return false
		}
		if d.headSeq.CompareAndSwap(cur, int64(n)) {
			return true
		}
	}
}

// headProbeCorrectionSlack is how far below the tracked head an
// authoritative probe reading may sit before the tracked value is treated
// as poisoned (a bogus-high header that the monotonic ratchet captured)
// rather than the probe as merely stale. Edge staleness spans seconds — a
// handful of segments — so a discrepancy this large means the ratchet is
// wrong and must correct downward: a permanently inflated head would turn
// every stall into an ErrQualityLost refresh and every finalize into a
// full MaxTimeout defer.
const headProbeCorrectionSlack = 100

// noteHeadSeqFromProbe records a dedicated-probe head reading. Probes ask
// GVS for the head directly, so they may both advance the ratchet and —
// unlike harvested headers — correct it DOWNWARD when the discrepancy
// exceeds headProbeCorrectionSlack. Small dips inside the slack are
// ignored as ordinary edge staleness. Self-healing is automatic: a
// poisoned-high head stops harvests from advancing it, which stops
// lastHeadProbeTime refreshes, which un-suppresses the dedicated probe
// within HeadProbeInterval.
func (d *SegmentDownloader) noteHeadSeqFromProbe(n int) {
	if n < 0 {
		return
	}
	for {
		cur := d.headSeq.Load()
		delta := cur - int64(n)
		if delta >= 0 && delta < headProbeCorrectionSlack {
			return // within staleness slack — keep the ratchet
		}
		if d.headSeq.CompareAndSwap(cur, int64(n)) {
			if delta >= headProbeCorrectionSlack {
				d.logger.Warn("[Downloader] head corrected downward — previous value looks bogus",
					"from", cur, "to", n)
			}
			return
		}
	}
}

// probeHeadSequence discovers the current live head segment using a high sequence GET probe.
// YouTube returns the X-Head-Seqnum header on GET requests to a non-existent segment.
//
// Two-tier probe strategy (audit reports/engine.md #13):
//
//  1. First attempt: probe at sequence 999,999,999. This is well past any
//     real live segment number and YouTube has historically responded with
//     X-Head-Seqnum so we discover the live edge in one round-trip.
//  2. Fallback: if the first probe returns no X-Head-Seqnum (server changed
//     behavior, rejected the absurdly high number, or returned an opaque
//     error page) AND we already have a usable currentSeq, retry at
//     currentSeq+1000 — close enough to be plausible while still being
//     ahead of the head.
//
// The fallback only fires when currentSeq > 0 because pre-first-segment
// downloads have no anchor to extrapolate from.
func (d *SegmentDownloader) probeHeadSequence(ctx context.Context) (int, error) {
	if seq, err := d.probeHeadAt(ctx, 999999999); err == nil {
		return seq, nil
	} else if cur := int(d.currentSeq.Load()); cur > 0 {
		// Fallback to a sane near-future probe. Do not propagate the first
		// error — we'll surface the fallback's outcome instead.
		return d.probeHeadAt(ctx, cur+1000)
	} else {
		return -1, err
	}
}

// probeHeadAt issues a single head-discovery GET at the given probe sequence
// and parses the X-Head-Seqnum response header.
func (d *SegmentDownloader) probeHeadAt(ctx context.Context, probeSeq int) (int, error) {
	probeURL := d.buildSegmentURL(probeSeq)
	probeURL = applyPoTokenQuery(probeURL, d.getPoToken())
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
	// Bounded drain to allow keep-alive reuse. The expected response to this
	// probe (a GET at a non-existent segment sequence) is tiny, so a modest
	// cap drains it fully in the normal case; a pathological large body from
	// a misbehaving edge is capped rather than pulled in full just to reclaim
	// one socket (mirrors probeFileSize's no-unbounded-drain policy).
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
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
//
// Status check happens before body drain: if the server ignores Range and
// returns 200 OK with the full file, we close without reading. The legacy
// behavior unconditionally io.Copy'd the body to io.Discard first, which on
// a non-Range-supporting CDN meant pulling a multi-GB VOD just to throw it
// away (audit reports/engine.md Finding 14).
func (d *SegmentDownloader) probeFileSize(ctx context.Context) int64 {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.getBaseURL(), nil)
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		// Server doesn't honor Range. Don't drain the body — it could be
		// multiple GB and we have no use for it. The connection is sacrificed
		// (no keep-alive reuse) but that's cheaper than the bandwidth.
		return 0
	}

	// 1-byte body; safe to drain so the connection can be reused for the
	// real chunked download that follows.
	io.Copy(io.Discard, resp.Body)

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

		// Retry on 5xx or network errors with exponential backoff (capped at
		// 60s). Skip the backoff after the final attempt — no fetch follows,
		// so it only delays the already-decided failure.
		if status >= 500 || status == 0 {
			if attempt < MaxChunkRetries-1 {
				delay := time.Duration(1<<uint(attempt)) * time.Second
				delay = min(delay, 60*time.Second)
				utils.Sleep(ctx, delay)
			}
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.getBaseURL(), nil)
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

	// Bound the 206 read to the requested range size — a correct server sends
	// exactly end-start+1 bytes, and a broken one must not be able to balloon
	// memory past it (mirrors the maxIgnoredRangeBodyBytes cap on the 200 path).
	data, err := io.ReadAll(io.LimitReader(resp.Body, end-start+1))
	if err == nil {
		// LimitReader returns EOF the instant its counter hits 0, WITHOUT the
		// trailing Read that lets net/http observe the body's own io.EOF — so
		// the connection is marked non-reusable and a fresh TCP+TLS handshake
		// is paid per 5MB chunk (thousands over a large VOD) under HTTP/1.1.
		// A bounded drain triggers that EOF-observing read (a correct server
		// has 0 bytes left) so the socket returns to the idle pool.
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
	}
	return data, resp.StatusCode, err
}
