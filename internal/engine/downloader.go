package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// ErrQualityLost signals that the stream is still live but the selected
// quality variant/format has become unavailable (e.g. transcode removed).
var ErrQualityLost = errors.New("stream quality became unavailable")

// ErrSegmentPermanent signals that a segment has been permanently
// evicted from the CDN (HTTP 403/410). Distinct from a transient
// retry-exhausted state: the caller should not bother retrying.
// Returned by fetchSegmentWithRetry when the underlying fetcher gets
// a definitive "gone forever" response. Audit reports/engine.md #17.
var ErrSegmentPermanent = errors.New("segment permanently unavailable")

// ErrSegmentRetriesExhausted signals that a segment fetch ran through
// all MaxSegmentRetries attempts without success. Caller may treat as
// a gap and continue. Audit reports/engine.md #17.
var ErrSegmentRetriesExhausted = errors.New("segment retries exhausted")

// ErrGapDetected signals that the live playlist has moved past the next
// sequence this downloader needed — the missing segments have expired from
// the CDN and the data is unrecoverable. Only returned when
// DownloaderOptions.StopOnGap is set and the output file already holds
// data: the caller is expected to finalize the current file as a complete,
// internally-gapless part and start a new downloader at the live edge.
// Without StopOnGap the loop skips past the gap and keeps appending
// (YouTube-style behavior, where the file knowingly contains a jump).
var ErrGapDetected = errors.New("unrecoverable gap in live stream")

const (
	CatchupThreshold     = 10
	MaxSegmentRetries    = 5
	ParallelDownloads    = 6  // Bounded parallel downloads during catch-up
	DefaultRetryDelayCap = 60 // seconds
	HeadProbeInterval    = 5 * time.Second
	SegmentTimeout       = 30 * time.Second
	// DefaultMaxTimeout is the fallback for DownloaderOptions.MaxTimeout (the
	// operator-configurable config.MaximumTimeout): how long the DASH loop keeps
	// waiting/verifying for the next segment before force-finalizing, even when
	// YouTube still reports the stream live.
	DefaultMaxTimeout = 10 * time.Minute
	// streamStatusCheckInterval bounds how often the DASH loop re-checks the
	// stream's status while waiting for the next segment. A live segment is ~1s
	// of media arriving about once a second, so a gap this long is the signal to
	// verify the stream ended; we then re-check at most once per interval so an
	// ended stream finalizes within ~30s (vs. waiting out MaxTimeout) without
	// hammering the API on brief hiccups.
	streamStatusCheckInterval = 30 * time.Second
	ResumeSeqInterval         = 50 // Save resume state every N sequential segments
	ResumeCatchupInterval     = 10 // Save resume state every N catch-up segments
	// DownloadChunkSize is sourced from the central constants catalog (5 MB).
	DownloadChunkSize = constants.DownloadChunkSize
	MaxChunkRetries   = 3                      // Per-chunk retry limit
	ProgressThrottle  = 500 * time.Millisecond // Throttle VOD progress emission

	// stayBehindSegments is the number of segments to stay behind the live edge
	// during parallel catch-up, avoiding download of in-flight segments.
	stayBehindSegments = 30

	// maxCatchupBatch caps how many segments one runParallelCatchUp call
	// fetches. Without a cap, a resume far behind the live edge sizes the
	// batch to the whole gap (thousands of segments); the out-of-order
	// reorder buffer then holds the entire window in RAM if the
	// head-of-window segment is stuck or CDN-evicted (oldest-first eviction
	// makes that the *likely* alignment on a long resume) — a multi-GB spike
	// that can OOM a low-RAM arm64 host. Bounding the batch makes peak memory
	// O(batch); runDashLoop re-enters catch-up back-to-back to drain a larger
	// gap without losing throughput.
	maxCatchupBatch = 8 * ParallelDownloads
)

// uaWeb and uaAndroid are the User-Agents for download requests, sourced
// from the central UA constants so version bumps stay in lockstep with the
// rest of the codebase.
var (
	uaWeb     = constants.UserAgents.Web
	uaAndroid = constants.UserAgents.Android
)

// DownloaderOptions configures a SegmentDownloader.
type DownloaderOptions struct {
	BaseURL      string
	OutputFile   string
	StartSeq     int
	EndSeq       int // -1 for unlimited
	PoToken      string
	CookieHeader string // Cookie header for authenticated downloads
	IsHls        bool
	IsDirectURL  bool // Direct URL download (not segmented)
	MaxRetries   int
	InitURL      string
	// InitFromSegment marks InitURL as a full media segment (a manifest-free
	// DASH sq=0) rather than a standalone init segment: downloadInitSegment
	// then writes only its extracted ftyp+moov init. Needed for manifestless
	// parts that force-start at sq>0, whose init lives inline at sq=0.
	InitFromSegment bool
	ForceStartSeq   bool // When true, StartSeq is exact (orchestrator-provided), skip DB-fallback +1 logic
	ResumeFile      string
	// StreamID is an optional orchestrator-provided stable identity for the
	// broadcast, persisted in the resume state. When both the saved state
	// and the current options carry one and they differ, the resume state
	// belongs to a different broadcast and is discarded. Essential for
	// platforms whose media URLs carry no extractable identity (Twitch
	// weaver URLs) — without it, a job resumed after the channel started a
	// NEW broadcast would splice the new stream into the old recording.
	StreamID string
	// StopOnGap makes the HLS live loop return ErrGapDetected instead of
	// skipping forward when the playlist has moved past the next needed
	// sequence and the output file already has data. Used for Twitch live,
	// where expired segments are unrecoverable (no DVR): the orchestrator
	// muxes the current file as a finished part and starts a new one, so
	// every output file stays internally gapless. Leave false for platforms
	// with seekable/backfillable streams (YouTube) and for VODs.
	StopOnGap bool
	// MaxTimeout bounds how long the DASH loop keeps retrying/verifying while
	// waiting for the next segment before it force-finalizes the recording —
	// even if YouTube still reports the stream live (its status can lag or
	// stick). The clock resets whenever a segment lands. Zero uses
	// DefaultMaxTimeout. Sourced from config.MaximumTimeout (YouTube only).
	MaxTimeout time.Duration
	// EnforceMaxTimeout opts the HLS loop into the same MaxTimeout backstop.
	// The DASH loop always enforces MaxTimeout because only YouTube ever runs
	// it, but runHlsLoop is shared with Twitch — whose GQL end-detection is
	// reliable and which never sets MaxTimeout (so the constructor default
	// would otherwise apply). Only the YouTube HLS strategy sets this true, so
	// Twitch recordings are never force-finalized by the timeout.
	EnforceMaxTimeout bool
	CheckStreamStatus func(ctx context.Context) (bool, error) // Returns true if stream ended
	IsOnline          func() bool                             // Returns false if device has no internet
	Logger            DownloaderLogger
}

// DownloadActivity describes what the downloader is currently WAITING ON when
// it is not actively pulling segments. The worker maps it to a human-readable
// progress-line message so a verifying/waiting download doesn't read as frozen.
type DownloadActivity int

const (
	ActivityNone                DownloadActivity = iota // actively downloading
	ActivityVerifyingEnd                                // segments stopped; confirming the stream ended
	ActivityReconnecting                                // connectivity lost; waiting for the network
	ActivityRateLimited                                 // 429 backoff
	ActivityFindingFirstSegment                         // pre-first-byte hunt for the first valid segment
	ActivityRetrying                                    // segment/playlist fetch failing; retrying
	ActivityWaitingForSegment                           // caught up at the live edge; the next segment isn't published yet
)

// DownloadProgress holds progress information for event callbacks.
type DownloadProgress struct {
	Seq        int
	Bytes      int64
	HeadSeq    int
	Total      int
	Percent    float64
	TotalBytes int64 // Total file size for VOD chunked downloads (0 if unknown)
	CatchingUp bool
}

// DownloadGap represents a detected gap in segments.
type DownloadGap struct {
	From   int
	To     int
	Stream string
}

// HealthUpdate is a periodic snapshot of downloader-level metrics. Audit
// reports/engine.md #31 — surfaces aggregate health to the UI / TUI for
// the long-running live-stream case where per-segment OnProgress lines
// don't show momentum at a glance. Emitted from the same call sites as
// OnProgress (no separate ticker), so the cadence is "every segment
// or progress flush".
type HealthUpdate struct {
	// ThroughputBps is the average bytes-per-second since the
	// downloader started accumulating bytes. Zero when no bytes
	// have flowed yet.
	ThroughputBps int64
	// ETA is the projected remaining duration to the configured EndSeq
	// (DASH/HLS) or TotalBytes (direct VOD). Zero when the endpoint is
	// unbounded (live-without-segment-cap).
	ETA time.Duration
	// RetryCount is the cumulative non-terminal retry count (4xx/5xx
	// transient errors that the segment fetcher backed off through).
	RetryCount int
	// LastError is the most recent non-terminal error message, or
	// empty when no retries have happened recently. Provided for at-a-
	// glance diagnostics — a stable RetryCount plus a populated
	// LastError signals "transient errors but recovering".
	LastError string
}

// DownloaderLogger is the interface for downloader logging.
type DownloaderLogger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Debug(string, ...any) {}
func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

// atomicTime wraps atomic.Int64 to store a time.Time as UnixNano for
// lock-free access across the download loop and parallel-worker goroutines.
// The zero value represents a zero time (IsZero() returns true from Load()).
type atomicTime struct{ v atomic.Int64 }

func (a *atomicTime) Store(t time.Time) { a.v.Store(t.UnixNano()) }
func (a *atomicTime) Load() time.Time {
	n := a.v.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}
func (a *atomicTime) StoreNow()            { a.v.Store(time.Now().UnixNano()) }
func (a *atomicTime) Since() time.Duration { return time.Since(a.Load()) }

// SegmentDownloader downloads DASH or HLS segments sequentially/in parallel.
type SegmentDownloader struct {
	opts                  DownloaderOptions
	mu                    sync.Mutex
	running               bool
	cancelled             atomic.Bool
	streamEnded           atomic.Bool
	outputFile            *os.File
	bytesWritten          atomic.Int64
	currentSeq            atomic.Int64
	headSeq               atomic.Int64
	lastSegTime           atomicTime
	lastHeadProbeTime     atomicTime
	lastStreamStatusCheck atomicTime
	logger                DownloaderLogger
	cipherFailureFired    atomic.Bool

	// streamEndVerified latches an "ended" verdict from CheckStreamStatus
	// within one continuous gone-burst so the behind-head retry loop in
	// handleGoneError doesn't re-probe the API every iteration. Reset when
	// a segment lands (only the download-loop goroutine touches it, so a
	// plain bool suffices). An ended verdict is sticky in reality —
	// post-live streams don't come back — but re-arming on recovery keeps
	// a spurious verdict from suppressing a later ErrQualityLost refresh.
	streamEndVerified bool

	// finalizedBehindHead latches when a finalize fired the
	// unfetched-tail warning (currentSeq < headSeq at errStreamDone) —
	// the precise "known-incomplete recording" signal the worker
	// persists as the job's incomplete_tail flag. streamEnded alone is
	// too broad: cancels and live-edge MaxTimeout stalls also leave it
	// unset without implying a missing tail.
	finalizedBehindHead atomic.Bool

	// baseURLOverride is set by SetBaseURL when a cipher rotation
	// requires swapping the stream URL mid-download. nil = use
	// opts.BaseURL (the construction-time URL); non-nil = use the
	// override. atomic.Pointer keeps reads lock-free on the hot
	// segment-fetch path. DECISIONS #7.
	baseURLOverride atomic.Pointer[string]

	// startedAt + transientRetries + lastTransientErr feed HealthUpdate
	// snapshots. transientRetries / lastTransientErrMu are read-mostly
	// in the segment fetcher (each non-terminal error is one increment
	// + one store), so a plain mutex is fine.
	startedAt        atomicTime
	transientRetries atomic.Int64
	lastTransientErr atomic.Pointer[string]

	// Callbacks
	OnStart    func(seq int, resuming bool)
	OnProgress func(p DownloadProgress)
	OnGap      func(g DownloadGap)
	OnFinish   func()
	// OnHealthUpdate is called with an aggregate-metrics snapshot
	// alongside each OnProgress emission. Audit reports/engine.md #31.
	// Optional; nil to opt out.
	OnHealthUpdate func(h HealthUpdate)
	// OnCipherFailure is called once on first 403 before any bytes
	// written (likely cipher rotation). The callback should
	// invalidate any cached cipher solver and OPTIONALLY return a
	// freshly-resolved BaseURL. If the return is non-empty, the
	// engine atomically swaps to the new URL via SetBaseURL and
	// continues fetching segments. Returning "" preserves the
	// legacy fall-through to ErrQualityLost.
	OnCipherFailure func() string
	// OnActivity reports the downloader's current wait reason (or
	// ActivityNone when it resumes downloading). Optional; nil to opt out.
	OnActivity func(a DownloadActivity)
}

// SetBaseURL atomically replaces the URL used for subsequent segment
// fetches. Safe to call from any goroutine AFTER Start has returned
// from its resume-validation setup (a SetBaseURL racing with Start's
// initial getBaseURL() read for resume validation is technically
// allowed by atomic semantics but loses the validation's intent --
// callers should wait for Start to finish before invoking SetBaseURL).
// Reads inside the download loop pick up the new value on the next
// getBaseURL() call. Nothing in flight is interrupted — the swap is
// observed at the start of each segment fetch / probe / resume save.
// DECISIONS #7.
func (d *SegmentDownloader) SetBaseURL(url string) {
	d.baseURLOverride.Store(&url)
}

// emitHealthUpdate fires OnHealthUpdate with a snapshot of the current
// rolling metrics. Called from every site that fires OnProgress so the
// UI gets aggregate health on the same cadence. Audit reports/engine.md
// #31. Nil-callback safe.
func (d *SegmentDownloader) emitHealthUpdate(p DownloadProgress) {
	if d.OnHealthUpdate == nil {
		return
	}
	var h HealthUpdate
	bytes := d.bytesWritten.Load()
	if started := d.startedAt.Load(); !started.IsZero() && bytes > 0 {
		elapsed := time.Since(started).Seconds()
		if elapsed > 0 {
			h.ThroughputBps = int64(float64(bytes) / elapsed)
		}
		// ETA: use Total/Percent for segment-based downloads, or
		// TotalBytes/Bytes for direct VOD chunked downloads.
		if p.Total > 0 && p.Seq > 0 && p.Total > p.Seq {
			perSeq := elapsed / float64(p.Seq)
			h.ETA = time.Duration(perSeq*float64(p.Total-p.Seq)) * time.Second
		} else if p.TotalBytes > 0 && bytes > 0 && p.TotalBytes > bytes {
			perByte := elapsed / float64(bytes)
			h.ETA = time.Duration(perByte*float64(p.TotalBytes-bytes)) * time.Second
		}
	}
	h.RetryCount = int(d.transientRetries.Load())
	if msg := d.lastTransientErr.Load(); msg != nil {
		h.LastError = *msg
	}
	d.OnHealthUpdate(h)
}

// emitActivity reports the current wait reason to OnActivity. Nil-callback safe.
func (d *SegmentDownloader) emitActivity(a DownloadActivity) {
	if d.OnActivity != nil {
		d.OnActivity(a)
	}
}

// getBaseURL returns the override URL if SetBaseURL has been called,
// otherwise the construction-time opts.BaseURL.
func (d *SegmentDownloader) getBaseURL() string {
	if p := d.baseURLOverride.Load(); p != nil {
		return *p
	}
	return d.opts.BaseURL
}

// NewSegmentDownloader creates a new segment downloader.
func NewSegmentDownloader(opts DownloaderOptions) *SegmentDownloader {
	if opts.MaxRetries == 0 {
		opts.MaxRetries = MaxSegmentRetries
	}
	if opts.MaxTimeout <= 0 {
		opts.MaxTimeout = DefaultMaxTimeout
	}
	if opts.EndSeq == 0 {
		opts.EndSeq = -1
	}
	if opts.ResumeFile == "" {
		opts.ResumeFile = opts.OutputFile + ".resume.json"
	}

	logger := opts.Logger
	if logger == nil {
		logger = nopLogger{}
	}

	d := &SegmentDownloader{
		opts:   opts,
		logger: logger,
	}
	d.currentSeq.Store(int64(opts.StartSeq))
	d.headSeq.Store(-1)
	return d
}

// Start begins the download process.
func (d *SegmentDownloader) Start(ctx context.Context) error {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return fmt.Errorf("already running")
	}
	d.running = true
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		d.running = false
		d.mu.Unlock()
		if d.OnFinish != nil {
			d.OnFinish()
		}
	}()

	// Check for resume state
	resuming := false
	state, err := d.loadResume()
	if err == nil && state != nil {
		// Validate resume identity — see resumeIdentityMismatch for the
		// full decision rules (explicit StreamID first, then URL
		// fingerprinting, with no-identity URLs deliberately trusted).
		if mismatch, reason := resumeIdentityMismatch(state, d.opts.StreamID, d.getBaseURL()); mismatch {
			d.logger.Warn("[Downloader] Resume state belongs to a different stream, starting fresh",
				"reason", reason,
				"savedStreamID", state.StreamID, "currentStreamID", d.opts.StreamID,
				"savedURLIdentity", streamIdentity(state.BaseURL), "currentURLIdentity", streamIdentity(d.getBaseURL()))
			state = nil
		}
		// Validate resume state: file must exist and be at least as large as saved position
		if state != nil && state.BytesWritten > 0 {
			if info, statErr := os.Stat(d.opts.OutputFile); statErr != nil || info.Size() < state.BytesWritten {
				d.logger.Warn("[Downloader] Resume state invalid, starting fresh",
					"savedBytes", state.BytesWritten,
					"fileExists", statErr == nil)
				state = nil
			}
		}
		if state != nil {
			d.currentSeq.Store(int64(state.LastSeq + 1))
			d.bytesWritten.Store(state.BytesWritten)
			resuming = true
		}
	}

	// Orchestrator-provided StartSeq: exact starting position for quality recovery/split.
	// Takes priority over DB-fallback — the orchestrator captured this from the old downloader.
	if !resuming && d.opts.ForceStartSeq && d.currentSeq.Load() > 0 {
		if info, statErr := os.Stat(d.opts.OutputFile); statErr == nil && info.Size() > 0 {
			// Same-quality recovery: append to existing file
			d.bytesWritten.Store(info.Size())
			resuming = true
			d.logger.Info("[Downloader] Continuing from orchestrator-provided seq (append)",
				"seq", d.currentSeq.Load(), "fileSize", info.Size())
		} else {
			// Different-quality split or new file: start from provided seq
			d.logger.Info("[Downloader] Starting from orchestrator-provided seq (fresh file)",
				"seq", d.currentSeq.Load())
		}
	} else if !resuming && d.currentSeq.Load() > 0 && !d.opts.IsHls {
		if info, statErr := os.Stat(d.opts.OutputFile); statErr == nil && info.Size() > 0 {
			d.bytesWritten.Store(info.Size())
			d.currentSeq.Add(1) // StartSeq is the last downloaded; advance to the next
			resuming = true
			d.logger.Info("[Downloader] Resuming from database state (no resume file)",
				"seq", d.currentSeq.Load(), "fileSize", info.Size())
		} else {
			// Output file missing -- can't resume, start fresh from segment 0
			d.logger.Info("[Downloader] No output file for resume, starting fresh")
			d.currentSeq.Store(0)
		}
	}

	// StopOnGap no-truncate guard: staged data with no usable resume state
	// (corrupt/stale/identity-rejected sidecar — e.g. power loss corrupted
	// the write, or the state aged past maxResumeStateAge during a long
	// outage on a continuing broadcast). Truncating would destroy a
	// recording that finalize-time recovery can still mux as a part — hand
	// the decision to the caller instead: the gap-split path closes this
	// file as a finished part and continues in a fresh one. Deliberate
	// discards (the quality-split short-segment rule) remove the staged
	// media before constructing the downloader, so they don't trip this.
	if !resuming && d.opts.StopOnGap {
		if info, statErr := os.Stat(d.opts.OutputFile); statErr == nil && info.Size() > 0 {
			d.logger.Warn("[Downloader] Staged data present but resume state unusable — splitting instead of truncating",
				"file", d.opts.OutputFile, "size", info.Size())
			return ErrGapDetected
		}
	}

	// Open output file
	flags := os.O_CREATE | os.O_WRONLY
	if resuming {
		flags |= os.O_APPEND
		// Truncate to known good size (only when we have a precise byte position from resume file)
		if state != nil && state.BytesWritten > 0 {
			if info, statErr := os.Stat(d.opts.OutputFile); statErr == nil && info.Size() > state.BytesWritten {
				d.logger.Info("[Downloader] Truncating file for resume",
					"from", info.Size(), "to", state.BytesWritten)
			}
			if truncErr := os.Truncate(d.opts.OutputFile, state.BytesWritten); truncErr != nil {
				if d.opts.StopOnGap {
					// Same contract as the no-truncate guard above: a failed
					// truncate must not fall back to O_TRUNC and destroy the
					// staged recording (transient sharing violations from AV
					// scans hit exactly this window on Windows). Split
					// instead — the caller muxes the file as a finished part.
					d.logger.Warn("[Downloader] Truncate-for-resume failed — splitting instead of starting fresh",
						"file", d.opts.OutputFile, "err", truncErr)
					return ErrGapDetected
				}
				d.logger.Warn("[Downloader] Failed to truncate for resume, starting fresh", "err", truncErr)
				resuming = false
				flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
				state = nil
			}
		}
	} else {
		flags |= os.O_TRUNC
	}

	d.outputFile, err = os.OpenFile(d.opts.OutputFile, flags, 0o644)
	if err != nil {
		return fmt.Errorf("open output file: %w", err)
	}
	// Closure (not `defer d.outputFile.Close()`): the direct-download
	// streaming-fallback reset reopens d.outputFile, and a method-value defer
	// would close the stale handle and leak the new one.
	defer func() { d.outputFile.Close() }()

	// Download init segment first (only if not resuming and not HLS).
	// Non-fatal: a missing init segment usually still produces a playable
	// file (FFmpeg can demux the segment data alone for many codecs), and
	// failing the entire download here would discard hours of recording.
	if !resuming && d.opts.InitURL != "" && !d.opts.IsHls {
		if initErr := d.downloadInitSegment(ctx); initErr != nil {
			if d.opts.InitFromSegment {
				// A manifest-free part that force-starts at sq>0 has no inline
				// init; without this fetched ftyp+moov the part file is a bare
				// moof+mdat that won't mux. Surface it loudly rather than the
				// usual best-effort Warn (which assumes FFmpeg can demux without).
				d.logger.Error("[Downloader] Failed to fetch sq=0 init for manifestless part — part may not mux", "error", initErr)
			} else {
				d.logger.Warn("[Downloader] Failed to download init segment", "error", initErr)
			}
		}
	}

	if d.OnStart != nil {
		d.OnStart(int(d.currentSeq.Load()), resuming)
	}

	// Mark the start time so HealthUpdate can derive throughput / ETA.
	// Audit reports/engine.md #31.
	d.startedAt.StoreNow()

	// Run download loop
	if d.opts.IsDirectURL {
		if err := d.runDirectDownload(ctx); err != nil {
			return err
		}
		// Don't validate a partial file from a user/shutdown cancel — the
		// caller preserves staging for resume, and a truncated head would
		// false-positive. Only gate a download that ran to completion.
		if d.isCancelled() || ctx.Err() != nil {
			return d.cancelErr(ctx)
		}
		return validateDownloadedMP4(d.opts.OutputFile)
	}
	if d.opts.IsHls {
		return d.runHlsLoop(ctx)
	}
	return d.runDashLoop(ctx)
}

// Cancel cancels the download.
func (d *SegmentDownloader) Cancel() {
	d.cancelled.Store(true)
}

// LastSeq returns the last successfully downloaded sequence number.
func (d *SegmentDownloader) LastSeq() int {
	return int(d.currentSeq.Load()) - 1
}

// CurrentSeq returns the next sequence number to be downloaded.
// Use this to capture the exact download position for replacement downloaders.
func (d *SegmentDownloader) CurrentSeq() int {
	return int(d.currentSeq.Load())
}

// FinalizedBehindHead reports whether the downloader finalized knowing
// segments below head were left unfetched. Valid after Start returns nil.
func (d *SegmentDownloader) FinalizedBehindHead() bool { return d.finalizedBehindHead.Load() }

// HeadSeq returns the last known head sequence (-1 if never learned).
func (d *SegmentDownloader) HeadSeq() int { return int(d.headSeq.Load()) }

// BytesWritten returns total bytes written (lock-free).
func (d *SegmentDownloader) BytesWritten() int64 {
	return d.bytesWritten.Load()
}

func (d *SegmentDownloader) isCancelled() bool {
	return d.cancelled.Load()
}

// cancelErr returns context.Canceled when the user-initiated cancel flag is set
// but the context hasn't been cancelled yet. This ensures callers always get a
// non-nil error when the download was cancelled.
func (d *SegmentDownloader) cancelErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.isCancelled() {
		return context.Canceled
	}
	return nil
}

// setCommonHeaders applies User-Agent and Cookie headers to a request.
func (d *SegmentDownloader) setCommonHeaders(req *http.Request, ua string) {
	req.Header.Set("User-Agent", ua)
	if d.opts.CookieHeader != "" {
		req.Header.Set("Cookie", d.opts.CookieHeader)
	}
}

// youtubeSegPathFormat is YouTube's per-segment path convention for
// path-style videoplayback URLs (typical DASH manifest output):
// `/sq/{seq}` appended to the base manifest URL.
const youtubeSegPathFormat = "%s/sq/%d"

// youtubeSegQueryFormat is YouTube's per-segment QUERY convention for
// adaptiveFormat URLs (manifest-free DASH path): `&sq={seq}` appended
// to the base URL that already carries query parameters from the
// player API. Both styles coexist because DASH manifest URLs come back
// path-style while watch-page `streamingData.adaptiveFormats[].url`
// values come back query-style.
const youtubeSegQueryFormat = "%s&sq=%d"

func (d *SegmentDownloader) buildSegmentURL(seq int) string {
	base := d.getBaseURL()
	if strings.Contains(base, "$Number$") {
		return SegmentURL(base, seq)
	}
	// Auto-detect URL shape: query-style URLs (manifest-free DASH from
	// `streamingData.adaptiveFormats[].url`) carry their parameters in
	// the query string, so we append `&sq=N`. Path-style URLs (DASH
	// manifest output) get the conventional `/sq/N`. The separator is
	// the discriminator: `?` present means we're looking at a query-
	// style URL.
	if strings.Contains(base, "?") {
		return fmt.Sprintf(youtubeSegQueryFormat, base, seq)
	}
	base = strings.TrimRight(base, "/")
	return fmt.Sprintf(youtubeSegPathFormat, base, seq)
}

func (d *SegmentDownloader) downloadInitSegment(ctx context.Context) error {
	data, status, err := d.fetchSegment(ctx, d.opts.InitURL)
	if err != nil || status >= 400 {
		return fmt.Errorf("init segment: status=%d: %w", status, err)
	}
	if d.opts.InitFromSegment {
		// InitURL is a full sq=0 media segment (manifest-free DASH); keep only
		// its ftyp+moov init so segment 0's media doesn't prefix this part.
		init := extractMP4InitBoxes(data)
		if init == nil {
			return fmt.Errorf("init segment: no ftyp/moov init found in sq=0 (%d bytes)", len(data))
		}
		data = init
	}
	n, err := d.outputFile.Write(data)
	if err != nil {
		return err
	}
	d.bytesWritten.Add(int64(n))
	return nil
}

// truncateURL returns the first maxLen characters of a URL for logging.
func truncateURL(u string, maxLen int) string {
	if len(u) <= maxLen {
		return u
	}
	return u[:maxLen] + "..."
}
