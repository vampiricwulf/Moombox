package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/httpx"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// engineMaxIdleConnsPerHost bounds the idle-connection pool per host for
// engineHTTPClient below. engineHTTPClient is a package-level singleton
// built once at package init — long before any download's operative worker
// count (DownloaderOptions.SegmentWorkers, ultimately
// config.Downloader.SegmentWorkers) is known — so it cannot be sized per
// download the way the worker-pool channels in downloader_parallel.go and
// downloader_hls.go are. It is instead sized generously up front: 64
// comfortably exceeds config.SegmentWorkersWarnThreshold (16, the point
// past which the config layer already warns operators about resource
// tradeoffs) with headroom for HEAD probes and playlist fetches sharing the
// same host. A downloader configured with more workers than this still
// downloads correctly — connections beyond the idle pool's capacity simply
// aren't kept alive, so those workers degrade to paying a fresh TCP+TLS
// handshake per segment rather than failing outright.
const engineMaxIdleConnsPerHost = 64

// engineHTTPClient is a shared HTTP client for segment and chunk downloads.
//
// Transport config: the Go default caps idle connections per host at 2,
// which causes TCP-handshake churn during high-concurrency catch-up. Bump
// idle-per-host to engineMaxIdleConnsPerHost (see its doc) so workers reuse
// sockets, and keep them alive for 90 s (matches http.DefaultTransport's
// value).
//
// Timeout: none. Every call site carries its own deadline — fetchSegment and
// fetchChunk a read-progress (idle) deadline under a segmentHardCeiling
// ceiling, the probes an explicit context.WithTimeout, the streaming fallback
// the idle deadline alone — and a client-level Timeout covers the whole body, so
// it would re-impose the total deadline sweep-2 ENGINE-4 removed and cap the
// streaming fallback's VOD size at whatever fits in five minutes
// (ENGINE-6). Built via httpx.NewTransport so the keep-alive tuning stays in
// sync with the rest of the codebase.
var engineHTTPClient = httpx.ClientWithTransport(
	0,
	httpx.NewTransport(httpx.TransportOptions{
		MaxIdleConnsPerHost: engineMaxIdleConnsPerHost,
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

// potValueRe matches a pot query value in a URL or an error string. It is the
// fallback for a URL net/url cannot parse, and the scrub for a wrapper's
// precomputed message.
var potValueRe = regexp.MustCompile(`pot=[^&"\s]+`)

// redactedPotValue replaces the PO token wherever an error would print it.
const redactedPotValue = "pot=<redacted>"

// redactPoToken keeps the GVS PO token out of an error's text. A transport
// failure from http.Client.Do is a *url.Error whose Error() embeds the full
// request URL, and applyPoTokenQuery put the token in that URL — so the
// string would reach `job error` and the job's stored error.
//
// Contract: when a *url.Error anywhere in err's chain carries a pot value,
// its URL field is rewritten IN PLACE to pot=<redacted> (Op and Err are
// untouched, so errors.Is / errors.As on the cause still hold). When err is
// that *url.Error itself it is returned as is; when it sits under a wrapper
// whose message was precomputed (fmt.Errorf), the result is a thin wrapper
// with the scrubbed message whose Unwrap is err. Every other error — nil,
// no *url.Error, no pot value — is returned unchanged, and a second call is
// a no-op.
func redactPoToken(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	redacted := redactPotInURL(ue.URL)
	if redacted == ue.URL {
		return err
	}
	msg := err.Error()
	ue.URL = redacted
	if err == error(ue) {
		return err
	}
	return &potRedactedError{msg: potValueRe.ReplaceAllString(msg, redactedPotValue), err: err}
}

// redactPotInURL rewrites every pot query value in rawURL to <redacted>,
// leaving every other parameter and their order byte-identical. A URL
// net/url cannot parse falls back to the regexp.
func redactPotInURL(rawURL string) string {
	if _, err := url.Parse(rawURL); err != nil {
		return potValueRe.ReplaceAllString(rawURL, redactedPotValue)
	}
	// Splice the raw string rather than re-serialise through url.URL, so
	// nothing but the pot value can change.
	head, rest, hasQuery := strings.Cut(rawURL, "?")
	if !hasQuery {
		return rawURL
	}
	query, frag, hasFrag := strings.Cut(rest, "#")
	parts := strings.Split(query, "&")
	changed := false
	for i, p := range parts {
		if key, _, _ := strings.Cut(p, "="); key == "pot" && p != redactedPotValue {
			parts[i] = redactedPotValue
			changed = true
		}
	}
	if !changed {
		return rawURL
	}
	out := head + "?" + strings.Join(parts, "&")
	if hasFrag {
		out += "#" + frag
	}
	return out
}

// potRedactedError carries a wrapper's message with the PO token scrubbed;
// Unwrap keeps the original chain for errors.Is / errors.As.
type potRedactedError struct {
	msg string
	err error
}

func (e *potRedactedError) Error() string { return e.msg }
func (e *potRedactedError) Unwrap() error { return e.err }

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

// reportFetchFailure records a connectivity failure unless the CALLER's
// context is already done. A cancelled download (shutdown, user cancel,
// quality split, superseded refresh) kills its in-flight requests by design,
// and counting those as network failures drags the connectivity oracle
// toward "offline" on every clean stop. Every fetch below derives a
// per-request context from its caller's, so the guard must ask the parent:
// the derived one carries the request deadline, and a request that genuinely
// timed out IS evidence.
func reportFetchFailure(parent context.Context, tag string) {
	if parent.Err() != nil {
		return
	}
	reportFailure(tag)
}

// readBody reads resp.Body, returns at most capBytes, and pre-allocates the
// result when the server declared a usable Content-Length: 0 < ContentLength
// <= capBytes (a declared length equal to the cap — the common 206-chunk
// case, where ContentLength == end-start+1 == the requested range size —
// takes the sized path too, not just a strictly smaller one).
//
// Segment and chunk bodies run 200 KB - 5 MB. io.ReadAll starts at 512 bytes
// and grows by ~1.25x, so an unsized read of a 4 MB segment copies the body
// through ~20 reallocations — about twice the final size in garbage — on
// every one of the thousands of segments a long recording fetches.
//
// The sized path probes one byte past the declared length (clamped so the
// probe itself never exceeds capBytes, which matters when the declaration
// equals the cap) before trusting it outright. Fix round 1 tested the
// hypothesis that a read landing exactly on Content-Length defeats
// connection reuse: it does not (see TestFetchSegmentReusesConnection) —
// net/http's body wrapper already surfaces io.EOF on the same Read call
// that drains a body to its own declared Content-Length. What the extra
// byte buys instead is correctness when Content-Length UNDERSTATES the real
// body: a scenario reachable only through a hand-built *http.Response (in
// production, net/http itself never hands out more bytes than the header
// declared), where it distinguishes "the body ended exactly where declared"
// (the probe hits EOF early) from "there's more" (the probe fills
// completely) — in the latter case the remainder is read on the bounded
// path below, up to capBytes.
//
// A body with no declared length (chunked, or transparently decompressed) or
// one declaring more than capBytes falls back to today's bounded io.ReadAll.
func readBody(resp *http.Response, capBytes int64) ([]byte, error) {
	n := resp.ContentLength
	if n <= 0 || n > capBytes {
		return io.ReadAll(io.LimitReader(resp.Body, capBytes))
	}
	// n == capBytes: the probe must not exceed the ceiling
	probeCap := min(n+1, capBytes)
	buf := make([]byte, 0, probeCap)
	for len(buf) < cap(buf) {
		m, err := resp.Body.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+m]
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return buf, err
		}
	}
	remaining := capBytes - int64(len(buf))
	if remaining <= 0 {
		// n == capBytes and the probe already filled to the ceiling.
		return buf, nil
	}
	// The body outran its declared Content-Length: finish reading on the
	// bounded path so the real data (up to capBytes) is still returned.
	rest, err := io.ReadAll(io.LimitReader(resp.Body, remaining))
	return append(buf, rest...), err
}

// errFetchIdle is the CAUSE a fetch's derived context carries when the
// read-progress deadline cancelled it, as opposed to the caller cancelling.
// The distinction matters twice: reportFetchFailure must still count a stall
// as network evidence, and a clean shutdown must not be reported as one.
var errFetchIdle = errors.New("no data received within the idle deadline")

// errFetchCeiling is the CAUSE the hard ceiling below cancels a fetch with.
// Separate from errFetchIdle because the two describe opposite failures — one
// CDN went silent, the other throttled us to death — and a log that cannot
// tell them apart cannot tell an operator which one they have. Both are
// transient as far as fetchSegmentWithRetry is concerned: neither is evidence
// that the segment is gone, so both take the ordinary retry ladder.
var errFetchCeiling = errors.New("fetch exceeded its hard ceiling")

// segmentHardCeiling is the absolute lifetime of ONE segment or chunk fetch,
// layered UNDER the read-progress deadline: SegmentTimeout ends a fetch that
// stopped delivering, and this ends one that delivers forever. Without it a
// body trickling a byte every few seconds resets the idle timer indefinitely
// and wedges a segment worker for the life of a 24/7 recording, never
// producing the error the retry ladder needs (sweep-2 Task 3 review, Probe 3:
// a 1-byte-per-idle/2 body ran past 25x the idle bound and stopped only on
// parent cancel).
//
// 15 minutes clears the case the read-progress deadline exists for with a
// wide margin — the ENGINE-4 repro is a 7.5 MB Twitch VOD segment at 10 KB/s,
// which is 12.5 minutes — while still capping the pathological trickle. It
// bounds fetchSegment and fetchChunk only: ProbeSegmentAvailable has its own
// total bound, and runDirectDownloadFallback streams a whole multi-GB VOD in
// one response, where any total bound is the wrong shape.
const segmentHardCeiling = 15 * time.Minute

// idleBody wraps a response body so that every Read delivering bytes pushes
// the fetch's deadline out again. SegmentTimeout used to be a TOTAL deadline
// on the derived context (sweep-2 ENGINE-4): a 7.5 MB Twitch VOD segment then
// needed 250 KB/s to survive, so twelve default workers imposed a 24 Mbit/s
// link floor below which every segment timed out, retried five times and left
// a permanent gap. moonarchive uses per-read timeouts of 2x the target
// duration for the same reason. The 30 s value is unchanged — it is now what
// it always read like, an IDLE bound.
type idleBody struct {
	rc    io.ReadCloser
	timer *time.Timer
	idle  time.Duration
	read  int64
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.read += int64(n)
		// Reset on an already-fired AfterFunc timer simply schedules it
		// again; if it has fired the context is already cancelled and this
		// Read's error path takes over, so there is nothing to undo.
		b.timer.Reset(b.idle)
	}
	return n, err
}

// received reports how many bytes this body has delivered, for the ceiling
// error's message — "exceeded its hard ceiling of 15m0s having received 41
// bytes" tells an operator they are being throttled rather than ignored.
// Only the fetch's own goroutine ever touches the counter (the deadline
// timers only cancel a context), and a nil receiver reports 0 so a failure
// that happened before there was a response body needs no special case.
func (b *idleBody) received() int64 {
	if b == nil {
		return 0
	}
	return b.read
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	return b.rc.Close()
}

// withReadProgressDeadline derives a context that is cancelled with
// errFetchIdle once `idle` elapses with no progress. The returned timer is
// handed to idleBody so body reads can push it out; the connect-and-headers
// phase runs under the same single arming, which is exactly the old
// behaviour for a server that never answers.
func withReadProgressDeadline(parent context.Context, idle time.Duration) (context.Context, *time.Timer, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	timer := time.AfterFunc(idle, func() { cancel(errFetchIdle) })
	return ctx, timer, func() { timer.Stop(); cancel(nil) }
}

// withFetchDeadlines derives the pair of bounds a segment or chunk fetch runs
// under: the hard ceiling outermost (a plain WithTimeoutCause, so
// context.Cause reads errFetchCeiling exactly the way the idle path reads
// errFetchIdle) and the read-progress deadline inside it. Returning one
// CancelFunc that releases both keeps the call sites to a single defer.
//
// runDirectDownloadFallback deliberately does NOT use this: it streams a
// whole VOD in one response and calls withReadProgressDeadline directly.
func withFetchDeadlines(parent context.Context, idle, ceiling time.Duration) (context.Context, *time.Timer, context.CancelFunc) {
	hard, hardCancel := context.WithTimeoutCause(parent, ceiling, errFetchCeiling)
	ctx, timer, cancel := withReadProgressDeadline(hard, idle)
	return ctx, timer, func() { cancel(); hardCancel() }
}

// idleFetchError re-labels a context error that the read-progress deadline
// caused, so callers and logs see a stall rather than a bare cancellation.
// Any other error passes through with only its PO token redacted
// (redactPoToken), so no caller can carry the token into a job error.
// Shared with runDirectDownloadFallback, which has no ceiling.
func idleFetchError(ctx context.Context, idle time.Duration, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(context.Cause(ctx), errFetchIdle) {
		return fmt.Errorf("stalled: %w for %s", errFetchIdle, idle)
	}
	return redactPoToken(err)
}

// fetchDeadlineError re-labels a context error that EITHER per-fetch deadline
// caused. The ceiling branch names the bound and how far the transfer got;
// everything else — an idle stall, a caller cancel, a transport failure —
// falls through to idleFetchError, which redacts any PO token in a transport
// error's URL and otherwise passes it through. body is nil when the fetch died
// before there was one, which is why received() tolerates a nil receiver.
func fetchDeadlineError(ctx context.Context, idle, ceiling time.Duration, body *idleBody, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(context.Cause(ctx), errFetchCeiling) {
		return fmt.Errorf("%w of %s having received %d bytes", errFetchCeiling, ceiling, body.received())
	}
	return idleFetchError(ctx, idle, err)
}

// fetchSegment downloads a single segment (or playlist) by URL.
func (d *SegmentDownloader) fetchSegment(parent context.Context, segURL string) ([]byte, int, error) {
	idle, ceiling := SegmentTimeout, d.delays.fetchHardCeiling
	ctx, idleTimer, cancel := withFetchDeadlines(parent, idle, ceiling)
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
		reportFetchFailure(parent, "engine/fetch")
		return nil, 0, fetchDeadlineError(ctx, idle, ceiling, nil, err)
	}
	reportSuccess("engine/fetch")
	// Wrap BEFORE the deferred Close: `defer resp.Body.Close()` binds the
	// receiver at defer time, so wrapping afterwards would close the raw body
	// and leak the timer.
	body := &idleBody{rc: resp.Body, timer: idleTimer, idle: idle}
	resp.Body = body
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

	data, err := readBody(resp, maxSegmentBodyBytes)
	if err != nil {
		return nil, resp.StatusCode, fetchDeadlineError(ctx, idle, ceiling, body, err)
	}

	return data, resp.StatusCode, nil
}

// fetchSegmentWithRetry attempts to fetch a segment with retries and two
// different backoff ramps, neither of them exponential across the whole
// function: the 403-with-refresh path doubles (singleGoneRetry << attempt —
// 500ms/1s/2s/4s, sized to outlive credentialRefreshCooldown), while every
// other transient failure waits a LINEAR 5s x (attempt+1) — 5s, 10s, 15s, 20s
// over the default MaxSegmentRetries=5, with the final attempt's sleep
// skipped because no fetch follows it.
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
//
// rebuildURL, when non-nil, recomputes segURL from the CURRENT base URL
// after a behind-head 403 triggers refreshCredentials(). Without this, a
// refreshed base URL (sig/n-param rotation — exactly what SetBaseURL and
// OnCredentialRefresh are for) never reaches the in-flight retry: segURL
// is otherwise a snapshot taken before this function was called, and only
// the PO token is re-read per attempt (getPoToken() inside fetchSegment).
// Callers whose segURL is derived from d.buildSegmentURL(seq) — the
// parallel DASH catch-up worker — should pass a closure over that seq.
// Callers whose segURL is an opaque absolute URL unrelated to any base
// (HLS, which reads literal URLs out of the playlist) pass nil; a base
// URL refresh has nothing to rebuild there.
func (d *SegmentDownloader) fetchSegmentWithRetry(ctx context.Context, segURL string, rebuildURL func() string) ([]byte, error) {
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
			// Every caller of this function fetches MEDIA payload (catch-up
			// workers, parallel HLS) — playlists and probes use fetchSegment
			// directly — so the arrival signal fires here unconditionally.
			d.noteFetch(len(body))
			return body, nil
		}
		if status == 410 {
			// Genuinely evicted. Never retried, never refreshed.
			return nil, ErrSegmentPermanent
		}
		if status == 403 {
			// 403 is overloaded. Past the head it is how a finished stream
			// signals "no such segment" and MUST stay terminal — VOD and
			// post-live finalization are built on it. BELOW the head the
			// segment demonstrably exists (X-Head-Seqnum told us so), so a
			// 403 there means our credentials went stale, which is exactly
			// what a manual cancel-and-resume fixes. Refresh them and retry
			// rather than declaring live segments permanently gone (the
			// 2026-08-15 mid-stream-join stall).
			if !d.behindHeadTailPending() || d.opts.OnCredentialRefresh == nil {
				// OnCredentialRefresh == nil: a strategy that harvests
				// X-Head-Seqnum but wires no refresh callback (the
				// manifest-based DASH path) has no possible recovery here —
				// refreshCredentials() would just no-op every time (it
				// checks the same nil internally). Without this gate, every
				// 403 below head would still pay the full
				// forbiddenRefreshAttempts retries (3 attempts, 2x
				// singleGoneRetryDelay) per segment across every catch-up
				// worker for a callback that can never fire. Falling through
				// here restores the pre-refresh immediate ErrSegmentPermanent
				// for callers that never opted in.
				return nil, ErrSegmentPermanent
			}
			if attempt >= forbiddenRefreshAttempts-1 {
				// Out of refresh attempts for this segment — report it the
				// way we always did so no caller hangs. The caller re-attempts
				// the segment on its next pass, by which point the cooldown
				// may have allowed another refresh.
				return nil, ErrSegmentPermanent
			}
			// Best-effort: a false return means the cooldown is still open or
			// no callback is installed. Retry anyway — a refresh fired by the
			// previous failing segment may already have installed working
			// credentials that this attempt will pick up.
			d.refreshCredentials()
			// Rebuild unconditionally on refreshCredentials()'s own return:
			// a concurrent catch-up worker's refresh (same cooldown claim)
			// may have installed the new base a moment before this call
			// checked, and rebuildURL() picking up d.getBaseURL() fresh is
			// what actually matters, not who won the cooldown race.
			if rebuildURL != nil {
				segURL = rebuildURL()
			}
			d.emitActivity(ActivityRetrying)
			// Doubling backoff, not a flat delay: the point of waiting is to
			// outlive credentialRefreshCooldown so this segment's LATER
			// attempts can actually claim a refresh. 500ms/1s/2s/4s spans
			// 7.5s against a 5s cooldown. See forbiddenRefreshAttempts.
			utils.Sleep(ctx, d.delays.singleGoneRetry<<attempt)
			continue
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

// errNoHeadSeqUsable marks a probe that got an HTTP RESPONSE the fallback
// probe could plausibly improve on: no X-Head-Seqnum header, or one that
// does not parse. A TRANSPORT error — no response at all (DNS, refused,
// reset, timeout) — is deliberately NOT this: during an outage every probe
// fails the same way, so a fallback only doubles the doomed round-trips.
var errNoHeadSeqUsable = errors.New("no usable X-Head-Seqnum header")

// probeHeadSequence discovers the current live head segment using a high sequence GET probe.
// YouTube returns the X-Head-Seqnum header on GET requests to a non-existent segment.
//
// Two-tier probe strategy (audit reports/engine.md #13):
//
//  1. First attempt: probe at sequence 999,999,999. This is well past any
//     real live segment number and YouTube has historically responded with
//     X-Head-Seqnum so we discover the live edge in one round-trip.
//  2. Fallback: if the first probe ANSWERED but carried no usable
//     X-Head-Seqnum (server changed behavior, rejected the absurdly high
//     number, or returned an opaque error page) AND we already have a usable
//     currentSeq, retry at currentSeq+1000 — close enough to be plausible
//     while still being ahead of the head. A transport error (no response at
//     all) skips the fallback: the second probe would fail the same way.
//
// The fallback only fires when currentSeq > 0 because pre-first-segment
// downloads have no anchor to extrapolate from.
func (d *SegmentDownloader) probeHeadSequence(ctx context.Context) (int, error) {
	seq, err := d.probeHeadAt(ctx, 999999999)
	if err == nil {
		return seq, nil
	}
	// Fallback only when the edge ANSWERED and the answer was unusable. A
	// transport error means the network is down or the host is unreachable,
	// and a second probe to the same host fails identically — during an
	// outage that doubled every probe cycle's round-trips for nothing.
	// Do not propagate the first error past the fallback — surface the
	// fallback's own outcome instead.
	if cur := int(d.currentSeq.Load()); cur > 0 && errors.Is(err, errNoHeadSeqUsable) {
		return d.probeHeadAt(ctx, cur+1000)
	}
	return -1, err
}

// probeHeadAt issues a single head-discovery GET at the given probe sequence
// and parses the X-Head-Seqnum response header.
func (d *SegmentDownloader) probeHeadAt(parent context.Context, probeSeq int) (int, error) {
	probeURL := d.buildSegmentURL(probeSeq)
	probeURL = applyPoTokenQuery(probeURL, d.getPoToken())
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return -1, err
	}
	d.setCommonHeaders(req, uaWeb)

	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		reportFetchFailure(parent, "engine/fetch")
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
		return -1, errNoHeadSeqUsable
	}

	headSeq, err := strconv.Atoi(headSeqStr)
	if err != nil {
		return -1, fmt.Errorf("%w: parse %q: %v", errNoHeadSeqUsable, headSeqStr, err)
	}

	return headSeq, nil
}

// parseContentRangeStart extracts the first-byte position a 206 says its body
// begins at. RFC 9110 §14.4 spells the header `bytes <start>-<end>/<total>`,
// with `*` allowed for the total; `bytes */<total>` is the unsatisfied-range
// form a 416 carries and has no start at all, so it returns ok=false — as do
// an absent, non-`bytes` or unparsable header.
//
// The callers need it because a 206 whose body does NOT start where they asked
// writes the wrong bytes at a plausible length (sweep-2 B-I1/D-R1): on the
// chunked path the read is bounded to end-start+1 so the file keeps its shape
// and corrupts in place, and in the streaming fallback — which runs with no
// known total size — the file simply grows. Neither is visible downstream.
// What an UNVERIFIABLE 206 means is left to each caller: the fallback refuses
// one, fetchChunk keeps trusting it.
func parseContentRangeStart(h http.Header) (int64, bool) {
	unit, spec, found := strings.Cut(strings.TrimSpace(h.Get("Content-Range")), " ")
	if !found || !strings.EqualFold(unit, "bytes") {
		return 0, false
	}
	rangeSpec, _, found := strings.Cut(strings.TrimSpace(spec), "/")
	if !found {
		return 0, false
	}
	startStr, _, found := strings.Cut(rangeSpec, "-")
	if !found {
		return 0, false // the "*" of an unsatisfied range has no dash
	}
	start, err := strconv.ParseInt(strings.TrimSpace(startStr), 10, 64)
	if err != nil || start < 0 {
		return 0, false
	}
	return start, true
}

// probeFileSize discovers the total file size using a Range: bytes=0-0 request.
// Returns 0 if the server doesn't support Range requests or the size is unknown.
//
// Status check happens before body drain: if the server ignores Range and
// returns 200 OK with the full file, we close without reading. The legacy
// behavior unconditionally io.Copy'd the body to io.Discard first, which on
// a non-Range-supporting CDN meant pulling a multi-GB VOD just to throw it
// away (audit reports/engine.md Finding 14).
func (d *SegmentDownloader) probeFileSize(parent context.Context) int64 {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	// The GVS PO token rides every direct-path request, the probe included:
	// a WEB-family format URL answers this 1-byte probe 206 without it and
	// then 403s the first real chunk (VOD 403 fix, 2026-09-29).
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, applyPoTokenQuery(d.getBaseURL(), d.getPoToken()), nil)
	if err != nil {
		return 0
	}
	d.setCommonHeaders(req, uaAndroid)
	req.Header.Set("Range", "bytes=0-0")

	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		reportFetchFailure(parent, "engine/fetch")
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
	// real chunked download that follows — but bounded like every other
	// drain here (sweep-2 ENGINE-14): a server that answers a 1-byte range
	// with megabytes must not be pulled in full just to reclaim a socket.
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))

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

// probeFileSizeWithRetry re-asks for the file size before the caller gives up
// on Range support. probeFileSize returns 0 for BOTH "this server does not do
// Range" and "that one request failed", and the caller's answer to 0 is the
// streaming fallback — so a single transient failure used to cost a resumed
// VOD its staged bytes and its sidecar (sweep-2 ENGINE-6). Three attempts
// with a 2 s/4 s backoff (delays.genericRetry, so tests scale it) cost a
// genuinely non-Range server six seconds of backoff, or up to ~36 s if every
// probe hangs to its own 10 s cap (probeFileSize's context.WithTimeout),
// once per download. It also triples the body sacrificed to a non-Range
// origin — measured at ~1 MB across the three probes, against ~330 KB for
// one — which is nothing beside the multi-GB VOD that follows.
func (d *SegmentDownloader) probeFileSizeWithRetry(ctx context.Context) int64 {
	const attempts = 3
	for i := range attempts {
		if size := d.probeFileSize(ctx); size > 0 {
			return size
		}
		if d.isCancelled() || ctx.Err() != nil {
			return 0
		}
		if i < attempts-1 {
			d.logger.Debug("[Downloader] Range probe returned no size; retrying", "attempt", i+1)
			if err := utils.Sleep(ctx, d.delays.genericRetry<<i); err != nil {
				return 0
			}
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
func (d *SegmentDownloader) fetchChunk(parent context.Context, start, end int64) ([]byte, int, error) {
	idle, ceiling := SegmentTimeout, d.delays.fetchHardCeiling
	ctx, idleTimer, cancel := withFetchDeadlines(parent, idle, ceiling)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, applyPoTokenQuery(d.getBaseURL(), d.getPoToken()), nil)
	if err != nil {
		return nil, 0, err
	}
	d.setCommonHeaders(req, uaAndroid)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		reportFetchFailure(parent, "engine/fetch")
		return nil, 0, fetchDeadlineError(ctx, idle, ceiling, nil, err)
	}
	reportSuccess("engine/fetch")
	// Wrap BEFORE the deferred Close, for the reason fetchSegment states.
	body := &idleBody{rc: resp.Body, timer: idleTimer, idle: idle}
	resp.Body = body
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return nil, resp.StatusCode, fmt.Errorf("range not satisfiable")
	}
	if resp.StatusCode == http.StatusOK {
		// Server ignored Range header -- cap read to avoid unbounded memory usage
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxIgnoredRangeBodyBytes))
		return data, resp.StatusCode, fetchDeadlineError(ctx, idle, ceiling, body, err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// A 206 that begins somewhere other than where we asked puts the WRONG
	// bytes at the right length: the read below is bounded to end-start+1
	// either way, so the file keeps its size and nothing downstream can see
	// the corruption (sweep-2 D-R1). Returning an error hands it to
	// fetchChunkWithRetry, which treats it exactly as it treats a short read
	// that ERRORS — an honest short body is no error at all and is accepted
	// as it stands.
	// An origin that omits Content-Range entirely keeps today's behaviour:
	// the header is mandatory on a 206, but refusing one on that ground alone
	// would break a working origin over a header this path does not need.
	if gotStart, ok := parseContentRangeStart(resp.Header); ok && gotStart != start {
		return nil, resp.StatusCode, fmt.Errorf("origin answered Range %d with Content-Range start %d", start, gotStart)
	}

	// Bound the 206 read to the requested range size — a correct server sends
	// exactly end-start+1 bytes, and a broken one must not be able to balloon
	// memory past it (mirrors the maxIgnoredRangeBodyBytes cap on the 200 path).
	data, err := readBody(resp, end-start+1)
	if err == nil {
		// A correct server declares Content-Length == end-start+1, so
		// readBody takes its sized path and observes the body's own io.EOF
		// itself while reading (see readBody's doc above +
		// TestFetchSegmentReusesConnection) — the connection already
		// returns to the idle pool without further help. This drain is
		// belt-and-braces for the unsized fallback: when Content-Length is
		// absent or declares more than end-start+1, readBody falls through
		// to the bounded io.ReadAll(io.LimitReader(...)) path, which can
		// return before net/http has observed the body's own EOF and would
		// leave the connection non-reusable without this drain.
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
	}
	return data, resp.StatusCode, fetchDeadlineError(ctx, idle, ceiling, body, err)
}
