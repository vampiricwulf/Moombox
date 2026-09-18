package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
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

// ErrStagedMediaPresent signals that Start found non-empty staged media at
// OutputFile that it could neither resume from (no usable sidecar, no DB
// position) nor was told to discard. Destroying it was the previous
// behaviour: an implicit `O_TRUNC` over a complete multi-hour recording
// (sweep-2 ENGINE-1/ENGINE-5). The rule is now the one the StopOnGap path
// always had — never truncate non-empty staged media unless the CALLER
// explicitly asked to discard it — with the decision handed back to the
// orchestrator, which can mux what is staged instead.
//
// StopOnGap callers get ErrGapDetected instead: they have a richer recovery
// (close this file as a finished part, continue in a fresh one).
var ErrStagedMediaPresent = errors.New("staged media present with no usable resume state")

// ErrTruncateBlocked signals that a staged recording could not be shrunk to
// its fsync'd resume offset: an antivirus scanner or the search indexer still
// holds the file open, or the volume went read-only. NOTHING WAS LOST — the
// media and its sidecar are exactly as they were, so the job stays resumable
// and the run should end now rather than be re-verified for half an hour.
//
// It never travels alone. A plain resume returns it wrapping the refusal; a
// StopOnGap caller gets it joined with ErrGapDetected, which still splits to a
// fresh part but must NOT raise the "segments expired from the CDN"
// notification, because no segment expired.
var ErrTruncateBlocked = errors.New("truncate for resume blocked")

// ErrInitSegmentChanged signals that the HLS playlist's #EXT-X-MAP init
// segment changed CONTENT mid-part (e.g. a Twitch transcode restart on the
// fMP4/CMAF delivery path). Appending fragments that reference a different
// moov onto the current file would corrupt it, so — like ErrGapDetected —
// the caller muxes the current file as a finished part and starts a fresh
// downloader, whose empty file begins with the new init segment. Only
// returned when StopOnGap is set; other configurations (VODs, YouTube HLS)
// write the new init inline instead. A changed map URI whose content is
// byte-identical to the written init (Twitch token rotation) never
// triggers this — see ensureHlsInit.
var ErrInitSegmentChanged = errors.New("HLS init segment changed")

const (
	CatchupThreshold     = 10
	MaxSegmentRetries    = 5
	ParallelDownloads    = 6  // Bounded parallel downloads during catch-up
	DefaultRetryDelayCap = 60 // seconds
	HeadProbeInterval    = 5 * time.Second
	// DefaultMaxTimeout is the fallback for DownloaderOptions.MaxTimeout (the
	// operator-configurable config.MaximumTimeout): how long the DASH loop keeps
	// waiting/verifying for the next segment before force-finalizing, even when
	// YouTube still reports the stream live.
	DefaultMaxTimeout = 10 * time.Minute
	// InterruptionNoStall is a sentinel DownloaderOptions.InterruptionTimeout
	// value distinct from both a positive ceiling and the zero "unbounded"
	// default: stallForPossibleResume consults MayResume exactly once per
	// call, latches finalizedDuringInterruption when it reports true, and
	// always returns false — finalize proceeds immediately, with no stall
	// and no clock started. Lets a caller latch Tier-2 evidence
	// (incomplete_tail preservation) WITHOUT ever blocking finalize, which
	// InterruptionTimeout's own zero value can't express — zero already
	// means "no ceiling" (unbounded). The worker maps its
	// interruption_timeout=0 config value ("stall disabled" per the config
	// contract) onto this sentinel rather than passing 0 straight through,
	// since a wired live downloader's 0 would otherwise collide with the
	// engine's own unrelated "unbounded" meaning for 0.
	InterruptionNoStall time.Duration = -1
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

	// catchUpRegrowInterval is how much elapsed time restores one segment of
	// catch-up batch width after a failure episode.
	catchUpRegrowInterval = 1 * time.Second

	// catchUpBufferBytes caps the RAM held by catch-up's out-of-order reorder
	// buffer. The buffer was previously bounded only by segment COUNT
	// (segmentWorkers*3), so memory scaled with a throughput setting: at the
	// 3.7-6.2 MB segments of a 1080p60 live stream, sixteen workers would
	// hold ~250 MB. Bounding by bytes means a wider pool costs connections,
	// not memory — workers simply wait for the head segment to land.
	catchUpBufferBytes = 256 << 20
)

// SegmentTimeout is the READ-PROGRESS (idle) deadline on a single segment or
// chunk fetch: the fetch is cancelled only after this long with no bytes
// arriving, so a slow-but-moving transfer runs as long as it keeps
// progressing (sweep-2 ENGINE-4). Consumers: fetchSegment and fetchChunk in
// downloader_fetch.go, via withReadProgressDeadline + idleBody, and
// runDirectDownloadFallback (downloader_direct.go), which streams a whole VOD
// in ONE response through the same pair — it is that transfer's only bound
// now that the client-level Timeout is gone (sweep-2 ENGINE-6).
// ProbeSegmentAvailable (eviction_probe.go) reuses the same value as a plain
// TOTAL context.WithTimeout — its body is capped at
// probeSegmentMaxBodyBytes, so there is no slow-transfer case to protect.
// fetchSegment and fetchChunk — and only those two — additionally run under
// segmentHardCeiling (downloader_fetch.go), the 15-minute absolute lifetime
// that ends a body trickling just fast enough to keep resetting this bound.
// A package var rather than a const purely so a test can shrink it under
// t.Cleanup-restored assignment and exercise a genuine deadline without an
// actual 30s wait; production code never mutates it. The rule that assignment
// imposes is narrow: a test that MUTATES this var (or any other package-level
// seam, e.g. syncMediaFile or ffmpegPathOS) must stay serial, because a
// parallel test reading it while another writes it is a data race. It is NOT a
// ban on t.Parallel() in this package — many tests here are parallel (the DASH
// integration, interruption, catch-up and eviction-probe suites) and must stay
// so; no count is quoted here because it would go stale. The
// canonical statement of the rule sits on the mutating test itself, at
// downloader_fetch_cancel_test.go's
// TestFetchSegmentDerivedTimeoutIsAConnectivityFailure ("Do not add
// t.Parallel(): shrinks the package-global SegmentTimeout").
var SegmentTimeout = 30 * time.Second

// uaWeb and uaAndroid are the User-Agents for download requests, sourced
// from the central UA constants so version bumps stay in lockstep with the
// rest of the codebase.
var (
	uaWeb     = constants.UserAgents.Web
	uaAndroid = constants.UserAgents.Android
)

// DownloaderOptions configures a SegmentDownloader.
type DownloaderOptions struct {
	BaseURL    string
	OutputFile string
	StartSeq   int
	EndSeq     int // -1 for unlimited
	PoToken    string
	// CookieHeader returns the CURRENT Cookie header, re-read on every
	// request. A getter and not a captured string because a download is the
	// longest-lived HTTP consumer in the program: a multi-hour archive runs
	// while the in-process refresh rotates Google's cookies roughly every 30
	// minutes, and a header snapshotted at construction was still being sent
	// to the last segment of the recording.
	//
	// What it returns is deliberately NOT filtered or host-scoped here. Making
	// the header live keeps the bytes on the wire identical to what an
	// unrotated jar sent before; deciding that entitlement rides the signed URL
	// rather than the session — and dropping cookies for *.googlevideo.com —
	// is an unmeasured premise whose failure would land on exactly the
	// members-only and age-gated captures cookies exist to serve.
	//
	// nil is safe and means "send no Cookie header", as an empty string did.
	CookieHeader func() string
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
	// DiscardStaged tells Start that this caller needs a file that begins at
	// the start of the stream, so the no-truncate guard (ErrStagedMediaPresent)
	// stands down and the file is opened O_TRUNC. It never means "destroy
	// whatever is there": the full rule is
	//
	//   - a USABLE resume sidecar always wins. Start resumes, appends the
	//     missing tail, and this flag is irrelevant — the guard block is not
	//     even reached;
	//   - otherwise, if staging holds a HEADED recording (an ftyp box or the
	//     EBML magic, i.e. something a muxer can open), it is preserved as
	//     <OutputFile>.restart-<unix ts> — with its sidecar — and the fresh
	//     file starts beside it;
	//   - otherwise (staging empty, or non-empty but unrecognisable) it is
	//     discarded, which is the ordinary fresh start.
	//
	// The one production setter is the manifest-free DASH strategy's
	// post-live restart: those segments carry their ftyp+moov init inline at
	// sq=0 only, so a finished stream that cannot resume genuinely must begin
	// again at 0 and the partial file cannot be appended to. Deliberate
	// discards that REMOVE the media before constructing the downloader (the
	// quality-split short-segment rule) never need this flag at all: the
	// guard only looks at bytes that are still there.
	DiscardStaged bool
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
	// InterruptionTimeout bounds how long stallForPossibleResume defers a
	// budget-expired finalize while MayResume keeps returning true. Zero
	// means no engine-side ceiling — the stall is unbounded here, gated only
	// by the worker's own resume-eligibility window. InterruptionNoStall (a
	// negative sentinel — see its doc comment) means the opposite of
	// unbounded: MayResume is still consulted once per finalize decision to
	// latch Tier-2 evidence, but the helper never actually stalls at all.
	// Sourced from config.InterruptionTimeout; the worker maps its own 0
	// ("stall disabled" per the config contract) onto InterruptionNoStall
	// rather than passing 0 straight through, since 0 here means something
	// different (unbounded, not disabled).
	InterruptionTimeout time.Duration
	CheckStreamStatus   func(ctx context.Context) (bool, error) // Returns true if stream ended
	IsOnline            func() bool                             // Returns false if device has no internet
	// OnCredentialRefresh is called when segments 403 while the downloader is
	// still behind the live head — i.e. the segments demonstrably exist and
	// our credentials, not the stream, are the problem. The callback should
	// re-fetch the player response and return a freshly-resolved BaseURL and
	// a freshly-minted GVS PO token; either may be "" to leave that half
	// unchanged. Both upstreams recover this way rather than treating the
	// 403 as terminal (see docs/superpowers/plans/2026-08-15-live-403-
	// credential-recovery.md — removed after implementation; read it from
	// git history, e.g. `git show aedc162^:docs/superpowers/plans/2026-08-15-live-403-credential-recovery.md`).
	// Optional; nil disables recovery, restoring
	// the previous behaviour exactly.
	OnCredentialRefresh func() (baseURL string, poToken string)
	Logger              DownloaderLogger
	// SegmentWorkers is how many segments this download fetches
	// concurrently. Zero means ParallelDownloads, preserving the historical
	// behaviour for callers that do not set it. No upper limit is enforced —
	// see config.SegmentWorkersWarnThreshold for why that is deliberate.
	SegmentWorkers int
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
	ActivityWaitingResume                               // broadcast interrupted; deferring finalize while resume is plausible
)

// String names the activity for logs and test failures.
func (a DownloadActivity) String() string {
	switch a {
	case ActivityNone:
		return "ActivityNone"
	case ActivityVerifyingEnd:
		return "ActivityVerifyingEnd"
	case ActivityReconnecting:
		return "ActivityReconnecting"
	case ActivityRateLimited:
		return "ActivityRateLimited"
	case ActivityFindingFirstSegment:
		return "ActivityFindingFirstSegment"
	case ActivityRetrying:
		return "ActivityRetrying"
	case ActivityWaitingForSegment:
		return "ActivityWaitingForSegment"
	case ActivityWaitingResume:
		return "ActivityWaitingResume"
	}
	return fmt.Sprintf("DownloadActivity(%d)", int(a))
}

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

// Clear resets the atomicTime to the zero value (Load().IsZero() becomes
// true). NOT the same as Store(time.Time{}): time.Time{}.UnixNano() is a
// large negative number, not 0, so that Store call would NOT produce a zero
// value here — only a direct v.Store(0) does.
func (a *atomicTime) Clear() { a.v.Store(0) }

// TryClaim atomically claims the next slot when at least cooldown has
// elapsed since the last claim, returning true to exactly ONE caller per
// window. A plain Since()+StoreNow() pair is check-then-act: concurrent
// callers can all pass the check before any of them stores.
func (a *atomicTime) TryClaim(cooldown time.Duration) bool {
	for {
		old := a.v.Load()
		now := time.Now()
		if old != 0 && now.Sub(time.Unix(0, old)) < cooldown {
			return false
		}
		if a.v.CompareAndSwap(old, now.UnixNano()) {
			return true
		}
	}
}

// SegmentDownloader downloads DASH or HLS segments sequentially/in parallel.
type SegmentDownloader struct {
	opts                  DownloaderOptions
	mu                    sync.Mutex
	running               bool
	cancelled             atomic.Bool
	streamEnded           atomic.Bool
	outputFile            *os.File
	bytesWritten          atomic.Int64
	bytesFetched          atomic.Int64 // media bytes ARRIVED off the network — see noteFetch
	currentSeq            atomic.Int64
	headSeq               atomic.Int64
	lastSegTime           atomicTime
	lastHeadProbeTime     atomicTime
	lastStreamStatusCheck atomicTime
	logger                DownloaderLogger
	cipherFailureFired    atomic.Bool

	// delays is every wait the loops sleep on; defaultDelays() in production,
	// fastDelays() in tests (see delays.go).
	delays delays

	// hlsInitWritten / hlsInitURI / hlsInitHash track the #EXT-X-MAP init
	// segment at the head of the output file (fMP4/CMAF HLS — see
	// ensureHlsInit). URI is the map URI the init was adopted under (updated
	// in place on a token-rotation rename whose content matched), hash the
	// SHA-256 of the written bytes — the identity that decides "same init"
	// when the URI rotates. Persisted in the resume sidecar so a successor
	// downloader appending to this file neither re-writes the init nor
	// mistakes the rotated URI for a transcode restart. Only touched from
	// the download-loop goroutine (Start restores them before the loop), so
	// plain fields suffice.
	hlsInitWritten bool
	hlsInitURI     string
	hlsInitHash    string

	// mediaSyncWarned latches the one Warn for a failed media fsync (owner
	// decision O-G, saveResume) so a volume that has gone read-only mid-
	// recording does not write a log line every cadence tick for hours.
	// saveResume and every media write run on the download-loop goroutine —
	// the same ownership hlsInitWritten above relies on — so a plain bool
	// needs no atomic.
	mediaSyncWarned bool

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

	// finalizedDuringInterruption latches when a budget-expired finalize was
	// deferred at least once by stallForPossibleResume (Tier 1: MayResume
	// reported true) before ultimately finalizing — either MayResume flipped
	// false or the InterruptionTimeout ceiling expired (Tier 2). The worker
	// consumes this to preserve resume data instead of the usual cleanup.
	finalizedDuringInterruption atomic.Bool

	// interruptionStallStart latches the moment stallForPossibleResume first
	// observes MayResume()==true for the current gone/timeout burst — the
	// clock InterruptionTimeout measures against. Zero (IsZero()) means no
	// stall is in progress. Only the download-loop goroutine touches it.
	interruptionStallStart atomicTime

	// baseURLOverride is set by SetBaseURL when a cipher rotation
	// requires swapping the stream URL mid-download. nil = use
	// opts.BaseURL (the construction-time URL); non-nil = use the
	// override. atomic.Pointer keeps reads lock-free on the hot
	// segment-fetch path. DECISIONS #7.
	baseURLOverride atomic.Pointer[string]

	// poTokenOverride carries a re-minted GVS PO token. opts.PoToken is the
	// value the downloader STARTED with; credential recovery replaces it in
	// place rather than tearing the downloader down (both upstreams refresh
	// credentials inside the segment loop — see the plan's background).
	// Mirrors baseURLOverride exactly.
	poTokenOverride atomic.Pointer[string]

	// lastCredentialRefresh gates OnCredentialRefresh to one call per
	// credentialRefreshCooldown.
	lastCredentialRefresh atomicTime

	// lastCatchUpFailure timestamps the most recent catch-up failure episode.
	// The batch ceiling is throttled relative to it so a 403 burst stops
	// being answered with 48 simultaneous requests (moonarchive damps its
	// batch the same way and regrows it over time).
	lastCatchUpFailure atomicTime

	// catchUpBufferBytesOverride lets tests shrink the reorder buffer's byte
	// ceiling below production's 256 MB catchUpBufferBytes, so a test can
	// saturate it with a handful of small fake segments instead of waiting
	// on real production-scale transfers. Zero (the default) means "use
	// catchUpBufferBytes" — production code never sets this.
	catchUpBufferBytesOverride int

	// hlsVodBufferBytesOverride is the same seam for the HLS VOD reorder
	// buffer (runHlsVodParallel) — deliberately the same shape as
	// catchUpBufferBytesOverride above rather than a package var, so the two
	// twins read alike and tests that shrink either one stay parallelisable.
	// Zero (the default) means "use catchUpBufferBytes"; production code
	// never sets this.
	//
	// The ceiling it shrinks is sweep-2 ENGINE-2: that reorder buffer was a
	// plain map with no bound at all, so while fetchSegmentWithRetry worked
	// through its 5+10+15+20 s ladder (plus up to five idle deadlines) on the
	// head-of-order segment, the other workers spent that window racing the
	// rest of the playlist into RAM — 0.6-2.5 GB on a 100 Mbit/s link at the
	// default 12 workers and ~7.5 MB Twitch VOD segments, capped only by the
	// size of the VOD. Same ceiling as the DASH catch-up twin, which has had
	// one since Arc 3.
	hlsVodBufferBytesOverride int

	// directResumeIntervalOverride is the same seam again for the whole-file
	// download's sidecar cadence (directResumeInterval, 50 MB — see both call
	// sites in downloader_direct.go). Zero (the default) means "use
	// directResumeInterval"; production code never sets this. A test that had
	// to move 100 MB through an httptest server to observe two checkpoints
	// would cost seconds and hundreds of megabytes of RAM to pin a rule that
	// is about the cadence, not the constant — which is itself pinned, by
	// TestDirectResumeIntervalIsFiftyMegabytes.
	directResumeIntervalOverride int64

	// onResumeSaved is a TEST SEAM, like delays and
	// catchUpBufferBytesOverride: production code never sets it. When
	// non-nil, saveResume calls it with the LastSeq it just persisted, after
	// the sidecar rename succeeded. It is the only way a test can count
	// sidecar WRITES — a clean end sets streamEnded and the loop's defer
	// then ClearResume()s the file out from under any on-disk assertion.
	onResumeSaved func(lastSeq int)

	// Callbacks
	OnStart    func(seq int, resuming bool)
	OnProgress func(p DownloadProgress)
	OnGap      func(g DownloadGap)
	OnFinish   func()
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
	// MayResume reports whether the broadcast may resume — stall evidence.
	// Called only from the download-loop goroutine. nil = feature off.
	MayResume func() bool
	// OnFetch reports n bytes of media payload ARRIVING off the network —
	// fired at every successful segment/chunk body read, including catch-up
	// workers whose data sits in the reorder buffer long before it flushes
	// (OnProgress only fires on the ordered flush, which goes quiet for the
	// whole clump while a wide worker pool is saturating the connection).
	// May be invoked concurrently from multiple worker goroutines; the
	// consumer must do its own locking. Assign before Start, like every
	// other callback. Optional; nil to opt out.
	OnFetch func(n int64)
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

// SetPoToken atomically replaces the PO token used for subsequent segment
// fetches. An empty token is ignored: a failed re-mint must not blank a
// credential that is still working. Nothing in flight is interrupted — the
// swap is visible to the next getPoToken() call.
func (d *SegmentDownloader) SetPoToken(token string) {
	if token == "" {
		return
	}
	d.poTokenOverride.Store(&token)
}

// getPoToken returns the current PO token: the refreshed override when one
// has been installed, otherwise the token the downloader was constructed
// with.
func (d *SegmentDownloader) getPoToken() string {
	if p := d.poTokenOverride.Load(); p != nil {
		return *p
	}
	return d.opts.PoToken
}

// emitActivity reports the current wait reason to OnActivity. Nil-callback safe.
func (d *SegmentDownloader) emitActivity(a DownloadActivity) {
	if d.OnActivity != nil {
		d.OnActivity(a)
	}
}

// noteOfflineRecovery re-arms the interruption stall clock after a
// connectivity outage, alongside the caller's lastSegTime reset (MaxTimeout
// pauses for offline; owner ruling 2026-08-21 gives the interruption
// ceiling the same treatment): an ACTIVE stall episode is re-latched to
// now, so the ceiling measures time actually spent waiting on YouTube —
// time with no internet is nobody's resume-plausibility evidence. A zero
// clock (no episode in progress) stays zero: recovery must never START an
// episode.
func (d *SegmentDownloader) noteOfflineRecovery() {
	if !d.interruptionStallStart.Load().IsZero() {
		d.interruptionStallStart.StoreNow()
	}
}

// noteFetch records n bytes of media payload arriving off the network and
// fires OnFetch. Called at every successful segment/chunk body read — the
// answer to "is data arriving?", which is a different question from the
// flush-driven counters: bytesWritten, lastSegTime, resume, and the
// finalize budget deliberately stay keyed on what is durably on disk (a
// fetch that lands in the reorder buffer above a permanent gap is
// discarded, and crediting it to the finalize clock would keep a stuck-gap
// catch-up cycle alive forever). Playlist and head-probe fetches are
// excluded: they are not stream payload, and counting them would keep
// "data arriving" true through an outage in which every real segment 403s.
func (d *SegmentDownloader) noteFetch(n int) {
	d.bytesFetched.Add(int64(n))
	if d.OnFetch != nil {
		d.OnFetch(int64(n))
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

// refreshCredentials asks the owner for fresh download credentials and
// installs whatever it returns. Returns true when a refresh actually ran and
// installed at least one value. Cooldown-gated: a 403 burst can call this on
// every failed segment, but only one player-response round trip per
// credentialRefreshCooldown is allowed through.
func (d *SegmentDownloader) refreshCredentials() bool {
	if d.opts.OnCredentialRefresh == nil {
		return false
	}
	if !d.lastCredentialRefresh.TryClaim(credentialRefreshCooldown) {
		return false
	}

	freshURL, freshToken := d.opts.OnCredentialRefresh()
	installed := false
	if freshURL != "" {
		d.SetBaseURL(freshURL)
		installed = true
	}
	if freshToken != "" {
		d.SetPoToken(freshToken)
		installed = true
	}
	if installed && d.logger != nil {
		d.logger.Info("[Downloader] credentials refreshed after 403",
			"newURL", freshURL != "", "newToken", freshToken != "")
	}
	return installed
}

// stallForPossibleResume reports whether a budget-expired finalize should
// defer because the broadcast may resume (interruption spec Tier 1). The
// FIRST true observation latches the stall clock; the configured ceiling
// (opts.InterruptionTimeout, 0 = no engine ceiling) bounds the total stall.
// A false MayResume (or expired ceiling) that follows a true observation
// latches finalizedDuringInterruption so the worker preserves resume data
// (Tier 2). Confirmed-ended callers must not consult this at all.
//
// InterruptionTimeout == InterruptionNoStall is a distinct third mode (I1
// fix): consult MayResume exactly once, latch finalizedDuringInterruption
// when it's true, and always return false — no stall clock is ever
// started, so finalize proceeds on this same call. Lets a caller latch
// Tier-2 evidence without ever blocking finalize's latency.
func (d *SegmentDownloader) stallForPossibleResume() bool {
	if d.opts.InterruptionTimeout == InterruptionNoStall {
		if d.MayResume != nil && d.MayResume() {
			d.finalizedDuringInterruption.Store(true)
		}
		return false
	}
	if d.MayResume == nil || !d.MayResume() {
		if !d.interruptionStallStart.Load().IsZero() {
			d.finalizedDuringInterruption.Store(true) // stalled earlier, evidence gone
		}
		return false
	}
	if d.interruptionStallStart.Load().IsZero() {
		d.interruptionStallStart.StoreNow()
		d.logger.Info("[Downloader] stream interrupted — deferring finalize while resume is plausible")
	}
	if d.opts.InterruptionTimeout > 0 && d.interruptionStallStart.Since() >= d.opts.InterruptionTimeout {
		d.finalizedDuringInterruption.Store(true)
		d.logger.Warn("[Downloader] interruption ceiling expired; finalizing with resume data preserved",
			"ceiling", d.opts.InterruptionTimeout)
		return false
	}
	return true
}

// segmentWorkers is the operative concurrency for this download: the
// configured DownloaderOptions.SegmentWorkers when the caller opted in, or
// ParallelDownloads otherwise. No upper clamp — a caller asking for an
// extreme value gets it; config.SegmentWorkersWarnThreshold is where that
// tradeoff is surfaced to the operator, not here.
func (d *SegmentDownloader) segmentWorkers() int {
	if d.opts.SegmentWorkers > 0 {
		return d.opts.SegmentWorkers
	}
	return ParallelDownloads
}

// maxCatchupBatch is the full (undamped) width of catch-up's rolling claim
// window — how far runParallelCatchUp's workers may fetch ahead of the
// flush position — derived from the operative worker count so a wider pool
// gets a proportionally deeper pipeline instead of starving against a
// fixed ceiling. Without the window, a resume far behind the live edge
// lets fetches run arbitrarily ahead of a stuck head-of-window segment
// (CDN oldest-first eviction makes that the *likely* alignment on a long
// resume); the byte ceiling caps what those fetches hold resident, and
// this caps how much work is dispatched past a potential failure point
// only to be discarded. See catchUpBatchLimit for the post-failure damped
// value.
func (d *SegmentDownloader) maxCatchupBatch() int {
	return 8 * d.segmentWorkers()
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
		opts.ResumeFile = opts.OutputFile + resumeFileSuffix
	}

	logger := opts.Logger
	if logger == nil {
		logger = nopLogger{}
	}

	d := &SegmentDownloader{
		opts:   opts,
		logger: logger,
		delays: defaultDelays(),
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
			d.hlsInitWritten = state.InitWritten
			d.hlsInitURI = state.InitURI
			d.hlsInitHash = state.InitHash
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

	// Shared no-truncate guard (sweep-2 ENGINE-1/5/6, verifier merge M2).
	// Reached ONLY when the engine could not resume — a corrupt/stale/
	// identity-rejected sidecar, a restart that re-probed the stream as
	// post-live and re-seeded seq 0, a sidecar the natural end already
	// cleared. A usable sidecar never lands here: `resuming` is already true
	// above, the file is truncated to the fsync'd offset and the missing tail
	// is appended, which is the documented incomplete-tail recovery.
	//
	// With no resume available, the bytes on disk must still not be destroyed
	// silently — finalize-time recovery can mux them. What happens next is
	// the caller's declared intent:
	//
	//   - no DiscardStaged: refuse. StopOnGap callers have the richer answer
	//     (close this file as a finished part and continue in a fresh one),
	//     so they keep ErrGapDetected; everyone else gets
	//     ErrStagedMediaPresent and the orchestrator decides.
	//   - DiscardStaged: this caller REQUIRES a file that begins at the start
	//     of the stream (the manifest-free sq=0 restart), so it cannot refuse
	//     — but it can preserve. A headed recording is set aside and the
	//     fresh file starts beside it; unrecognisable bytes are discarded.
	//
	// IsDirectURL is out of scope for both: a whole-file VOD download is not
	// segmented staged media and its partial is always re-fetchable from the
	// same static URL, so restarting it costs bandwidth, not footage. What it
	// costs is now genuinely bounded by the 50 MB sidecar cadence, which the
	// direct paths did not actually write until Task 10 — saveResume's
	// `currentSeq > 0` guard returned early on every whole-file download, so
	// the clause this comment leaned on was aspirational and an interrupted
	// VOD restarted from byte 0 however far it had got (directResumeInterval,
	// downloader_direct.go, now saves on both the chunked and the streaming
	// path).
	if !resuming && !d.opts.IsDirectURL {
		if info, statErr := os.Stat(d.opts.OutputFile); statErr == nil && info.Size() > 0 {
			if !d.opts.DiscardStaged {
				if d.opts.StopOnGap {
					d.logger.Warn("[Downloader] Staged data present but resume state unusable — splitting instead of truncating",
						"file", d.opts.OutputFile, "size", info.Size())
					return ErrGapDetected
				}
				d.logger.Error("[Downloader] Staged data present but resume state unusable — refusing to truncate",
					"file", d.opts.OutputFile, "size", info.Size())
				return fmt.Errorf("%w: %s holds %d bytes", ErrStagedMediaPresent, d.opts.OutputFile, info.Size())
			}
			if preserveErr := d.preserveStagedRecording(ctx, info.Size()); preserveErr != nil {
				return preserveErr
			}
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
			if truncErr := truncateForResume(ctx, d.opts.OutputFile, state.BytesWritten); truncErr != nil {
				if d.opts.StopOnGap {
					// Same contract as the no-truncate guard above: a failed
					// truncate must not fall back to O_TRUNC and destroy the
					// staged recording (transient sharing violations from AV
					// scans hit exactly this window on Windows). Split
					// instead — the caller muxes the file as a finished part.
					// That part keeps whatever bytes lie past the fsync'd
					// offset, so its last fragment may be torn; FFmpeg drops a
					// partial trailing fragment, and a torn tail beats a
					// destroyed recording. ErrTruncateBlocked rides along so
					// the orchestrator splits WITHOUT telling the operator
					// that segments were lost to the CDN — none were.
					d.logger.Warn("[Downloader] Truncate-for-resume failed — splitting instead of starting fresh",
						"file", d.opts.OutputFile, "err", truncErr)
					return fmt.Errorf("%w: %w: %v", ErrGapDetected, ErrTruncateBlocked, truncErr)
				}
				// ENGINE-5: the old branch here logged a Warn, cleared the
				// resume state and opened the file O_TRUNC — losing hours of
				// footage to a transient sharing violation. The retry ladder
				// above has already ridden out that window; anything left is
				// a real filesystem failure, and returning it keeps staging
				// and the sidecar intact for a later Resume.
				d.logger.Error("[Downloader] Truncate-for-resume failed after retries",
					"file", d.opts.OutputFile, "err", truncErr)
				return fmt.Errorf("%w: %w", ErrTruncateBlocked, truncErr)
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
	// discard (discardStagedMedia) reopens d.outputFile, and a method-value
	// defer would close the stale handle and leak the new one.
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

// StagedRestartSuffix marks a recording Start set aside instead of truncating
// it: <OutputFile>.restart-<unix ts>, with its sidecar alongside as
// <OutputFile>.restart-<unix ts>.resume.json.
//
// Exported because it is a cross-package contract, not an implementation
// detail: package worker keys on it in two places — the orchestrator registers
// an aside as an unmuxed part, and the orphan sweep recognises one so it is
// neither deleted nor mistaken for a live staging file. Both must match on
// this const rather than a hardcoded literal, or the two halves drift.
//
// A path carrying this suffix is the RECORDING only when it does not also end
// in resumeFileSuffix — the sidecar twin shares the timestamped stem.
const StagedRestartSuffix = ".restart-"

// resumeFileSuffix is the sidecar's name relative to its media file, used both
// by NewSegmentDownloader's default and by the aside rename above so the two
// can never drift apart.
const resumeFileSuffix = ".resume.json"

// StagedRestartSidecar returns the resume sidecar that travels with an aside.
//
// Exported for the same reason IsStagedRestartPath is: package worker deletes
// an aside once it has been recovered into its own output file, and the twin
// has to go with it — a sidecar left beside nothing is a stale offset map. The
// suffix itself stays unexported so the pair can only ever be spelled here.
func StagedRestartSidecar(aside string) string { return aside + resumeFileSuffix }

// IsStagedRestartPath reports whether name is a recording set aside by the
// no-truncate guard — <file>.restart-<unix ts> — and not its sidecar twin,
// which shares that timestamped stem as <file>.restart-<unix ts>.resume.json.
//
// Exported because resumeFileSuffix is not: package worker's staging scan
// (internal/worker/orchestrator_mux.go, stagedRecordingParts) has to tell the
// recording from its twin, and hardcoding either literal there is exactly the
// drift StagedRestartSuffix's doc comment exists to prevent. The timestamp is
// required to be digits so an ordinary file that merely contains ".restart-"
// is never mistaken for one of ours.
func IsStagedRestartPath(name string) bool {
	if strings.HasSuffix(name, resumeFileSuffix) {
		return false
	}
	i := strings.LastIndex(name, StagedRestartSuffix)
	if i < 0 {
		return false
	}
	return allDigits(name[i+len(StagedRestartSuffix):])
}

// RestartSiblingStem reports whether base is a RECOVERED set-aside recording —
// the sibling output file the worker muxes beside a job's archive, named
// <stem>.restart-<unix ts>[-N].<ext> — and returns the <stem> whose archive it
// belongs to.
//
// The twin predicate IsStagedRestartPath matches the RAW recording in staging,
// whose name ENDS at the timestamp; this one matches the muxed output, where
// the timestamp is an infix before the container extension and may carry the
// worker's collision counter. Both live here, beside the const they share, for
// the reason StagedRestartSuffix's doc comment gives: package worker's orphan
// sweep has to tell a recovered sibling from an ordinary output file so it is
// never offered for deletion, and hardcoding the literal there is exactly the
// drift this pair exists to prevent.
//
// base is a file's base name, not a path.
func RestartSiblingStem(base string) (string, bool) {
	body := base
	if dot := strings.LastIndex(base, "."); dot >= 0 {
		body = base[:dot] // strip the container extension
	}
	i := strings.LastIndex(body, StagedRestartSuffix)
	if i < 0 {
		return "", false
	}
	stamp := body[i+len(StagedRestartSuffix):]
	// <ts> or <ts>-<counter>; both halves are digits, so an ordinary file that
	// merely contains ".restart-" is never mistaken for one of ours.
	if ts, counter, split := strings.Cut(stamp, "-"); split {
		if !allDigits(ts) || !allDigits(counter) {
			return "", false
		}
	} else if !allDigits(stamp) {
		return "", false
	}
	return body[:i], true
}

// allDigits reports whether s is a non-empty run of ASCII digits — the stamp
// rule both restart predicates above hang on.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// preserveStagedRecording is the DiscardStaged half of the no-truncate guard.
// The caller has declared it needs a file that begins at the start of the
// stream, and the engine has already established it cannot resume, so the
// bytes on disk cannot simply be appended to. They are still not destroyed:
// anything that might be muxable is renamed to <OutputFile>.restart-<unix ts>
// (its sidecar too) and the fresh file is opened beside it. Only bytes the
// engine has positively READ and found unrecognisable — a bare moof+mdat run
// from a part that force-started mid-stream — fall through to the O_TRUNC
// below, because there is genuinely nothing there to preserve.
//
// Both renames ride the same transient ladder as truncateForResume: the
// scanner/indexer hold that refuses a truncate refuses a rename the same way
// (a holder without FILE_SHARE_DELETE), and failing the run at the first
// refusal while the truncate twenty lines below rides it out for 1270 ms would
// be an arbitrary difference (fix round 3, Concern 2).
//
// A rename that fails permanently is NOT downgraded to a truncate: the guard's
// whole promise is that nothing is destroyed implicitly, so the error surfaces
// and the job stays resumable with everything where it was.
func (d *SegmentDownloader) preserveStagedRecording(ctx context.Context, size int64) error {
	if !stagedRecordingWorthPreserving(d.opts.OutputFile) {
		d.logger.Warn("[Downloader] Staged bytes carry no container header — discarding for a fresh start",
			"file", d.opts.OutputFile, "size", size)
		return nil
	}
	aside := fmt.Sprintf("%s%s%d", d.opts.OutputFile, StagedRestartSuffix, time.Now().Unix())
	if err := renameStagedFile(ctx, d.opts.OutputFile, aside); err != nil {
		d.logger.Error("[Downloader] Could not set the staged recording aside — refusing to truncate it",
			"file", d.opts.OutputFile, "aside", aside, "err", err)
		return fmt.Errorf("%w: %s holds %d bytes and could not be set aside: %w",
			ErrStagedMediaPresent, d.opts.OutputFile, size, err)
	}
	if err := d.moveResumeStateAside(ctx, aside); err != nil {
		return err
	}
	d.logger.Warn("[Downloader] Staged recording set aside for a fresh start — mux it from this path if the restart falls short; a first segment that was only partly written may not be muxable on its own",
		"from", d.opts.OutputFile, "to", aside, "bytes", size)
	return nil
}

// moveResumeStateAside sends the sidecar after the recording it describes.
// When the recording moved but the sidecar cannot follow, leaving it beside
// the fresh file would be a TRAP rather than a nuisance: this Start already
// rejected it, but a LATER Start could match the same stale offsets against
// the regrown file and resume-append at the old recording's sequence. The
// sidecar is unusable by definition here — that is why this branch was
// reached — so deleting it loses nothing and is the cheapest way to make the
// fresh start unambiguous. Only when it can be neither moved nor deleted does
// the run fail, rather than proceed with a live trap on disk.
func (d *SegmentDownloader) moveResumeStateAside(ctx context.Context, aside string) error {
	err := renameStagedFile(ctx, d.opts.ResumeFile, aside+resumeFileSuffix)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	if rmErr := retryTransientFileOp(ctx, func() error { return os.Remove(d.opts.ResumeFile) }); rmErr != nil && !os.IsNotExist(rmErr) {
		d.logger.Error("[Downloader] Staged recording set aside but its stale resume state could not be cleared — refusing to start over it",
			"file", d.opts.ResumeFile, "renameErr", err, "removeErr", rmErr)
		return fmt.Errorf("%w: stale resume state %s could not be moved or removed: %w",
			ErrStagedMediaPresent, d.opts.ResumeFile, rmErr)
	}
	d.logger.Warn("[Downloader] Staged recording set aside without its resume state; the stale sidecar was removed so the fresh start cannot resume against it",
		"file", d.opts.ResumeFile, "err", err)
	return nil
}

// stagedRecordingWorthPreserving reports whether the bytes at path must be
// preserved rather than truncated away. It answers "yes" in two cases:
//
//   - the file begins with a container header a muxer can open — an MP4/M4A
//     'ftyp' box, or the Matroska/WebM EBML magic. A capture that began at the
//     start of the stream has one, because a manifest-free DASH sq=0 segment
//     carries its ftyp+moov init inline;
//   - the header could not be read AT ALL. A share-mode-0 holder, an EIO on a
//     network staging dir or a file shorter than the eight bytes the check
//     needs all land here, and "cannot tell" must not read as "worthless": the
//     alternative is destroying a multi-hour recording the engine merely
//     failed to peek at (fix round 3, Note 1). The cost of guessing wrong is a
//     small aside file; the cost of the other guess is the recording.
//
// It answers "no" only for bytes it positively read and did not recognise — a
// bare moof+mdat run from a part that force-started mid-stream, which FFmpeg
// cannot demux on its own.
func stagedRecordingWorthPreserving(path string) bool {
	f, err := openStagedFile(path)
	if err != nil {
		return true
	}
	defer f.Close()
	var hdr [8]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return true
	}
	if string(hdr[4:8]) == "ftyp" {
		return true
	}
	return hdr[0] == 0x1A && hdr[1] == 0x45 && hdr[2] == 0xDF && hdr[3] == 0xA3
}

// truncateForResume shrinks a staged recording to its fsync'd resume offset.
// On Windows an antivirus scanner or the search indexer briefly holds a
// freshly written recording open and the truncate is refused with
// ERROR_ACCESS_DENIED or ERROR_SHARING_VIOLATION although nothing is wrong
// with the file; ONLY those refusals are retried. Every other error (a missing
// file, a directory in its place, a read-only volume, a POSIX EACCES) is
// permanent and is returned on the FIRST attempt.
//
// Same shape and same constants as utils.ReplaceFile's rename retry, spelled
// out here rather than reused because that helper replaces a target and these
// callers truncate or rename to a fresh path. Either way the caller is left
// with the staged media and its sidecar untouched, so the job stays resumable.
func truncateForResume(ctx context.Context, path string, size int64) error {
	return retryTransientFileOp(ctx, func() error { return truncateFile(path, size) })
}

// renameStagedFile moves a staged recording (or its sidecar) aside through the
// same ladder. The refusal it rides out is the same one truncateForResume
// rides out — a scanner or indexer holding the file without FILE_SHARE_DELETE
// makes os.Rename fail with ERROR_SHARING_VIOLATION while nothing is wrong
// with either path.
func renameStagedFile(ctx context.Context, from, to string) error {
	return retryTransientFileOp(ctx, func() error { return renameFile(from, to) })
}

// retryTransientFileOp is the ladder itself: run op, and on a refusal the
// platform classifier calls transient, pause and try again — 10, 20, 40, 80,
// 160, 320 and 640 ms, 1270 ms across eight attempts. A permanent error and a
// dead ctx both return immediately with the last error, so a caller never
// waits out a shutdown or a read-only volume.
func retryTransientFileOp(ctx context.Context, op func() error) error {
	delay := truncateResumeFirstDelay
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil || attempt >= truncateResumeAttempts || !isTransientTruncateError(err) {
			return err
		}
		if truncateRetrySleep(ctx, delay) != nil {
			// Shutting down mid-ladder: surface the refusal now. Nothing is
			// lost by stopping early — the file is exactly as it was.
			return err
		}
		if delay < truncateResumeMaxDelay {
			delay *= 2
		}
	}
}

const (
	// truncateResumeAttempts bounds the ladder; with the pauses below the
	// worst case waits 1270 ms — long enough to outlast a scanner's hold on a
	// just-written file, short enough not to stall a restart.
	truncateResumeAttempts   = 8
	truncateResumeFirstDelay = 10 * time.Millisecond
	// truncateResumeMaxDelay caps the doubling. The last delay below it still
	// doubles, so the final pause is 640 ms — the same overshoot
	// utils.ReplaceFile's identical `< replaceFileMaxDelay` test produces.
	truncateResumeMaxDelay = 400 * time.Millisecond
)

// Seams for the tests: the two file operations the ladder runs, the platform
// classifier, the pause, and the open behind the header check. Production
// never reassigns them (mirrors utils.ReplaceFile's renameFile /
// isTransientReplaceError / replaceFileSleep).
var (
	truncateFile             = os.Truncate
	renameFile               = os.Rename
	isTransientTruncateError = transientTruncateError
	truncateRetrySleep       = utils.Sleep
	// openStagedFile exists so the "cannot read the staged file at all" branch
	// — a share-mode-0 holder, an EIO on a network staging dir — is reachable
	// in a test without a platform-specific fixture.
	openStagedFile = os.Open
)

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

// FinalizedDuringInterruption reports whether the downloader deferred a
// budget-expired finalize at least once because MayResume reported the
// broadcast could resume, before ultimately finalizing anyway (either
// MayResume flipped false or the InterruptionTimeout ceiling expired).
// Valid after Start returns nil.
func (d *SegmentDownloader) FinalizedDuringInterruption() bool {
	return d.finalizedDuringInterruption.Load()
}

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
//
// The Cookie header is READ HERE, per request, rather than snapshotted at
// construction — see DownloaderOptions.CookieHeader. All six callers reach
// this once per outbound HTTP request (never inside a loop within one
// request), so the getter costs one cookie-jar RLock per request; no caller
// holds a downloader lock across it.
//
// The `!= ""` guard is unchanged, so an empty value still sends no header at
// all rather than an empty one. Nothing here inspects req.URL: which cookies
// go to which host is the jar's business and is deliberately not re-decided
// per request.
func (d *SegmentDownloader) setCommonHeaders(req *http.Request, ua string) {
	req.Header.Set("User-Agent", ua)
	if d.opts.CookieHeader != nil {
		if ch := d.opts.CookieHeader(); ch != "" {
			req.Header.Set("Cookie", ch)
		}
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
	d.noteFetch(len(data))
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
