package monitor

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/vampiricwulf/Moombox/internal/config"
)

const (
	// maxMetadataFailures is the maximum number of probe failures per video
	// before adding it to history and giving up.
	maxMetadataFailures = 3

	// maxMetadataFailuresMapSize caps the failure tracker map to prevent
	// unbounded growth from orphaned entries.
	maxMetadataFailuresMapSize = 500

	// probeCooldownMaxSize caps the cooldown map to bound memory.
	probeCooldownMaxSize = 2000
)

// VideoProbeResult holds the stream status returned by a video probe.
type VideoProbeResult struct {
	StreamStatus       string // "live", "upcoming", "vod", "post_live", "not_a_stream"
	Title              string // metadata title (may be better than feed title)
	PublishedAt        string // probe's authoritative publish date (RFC3339, may be empty)
	PublishedPrecision string // "started" or "day", empty when PublishedAt is empty
	PlayabilityError   string // playability status from YouTube API (may be empty for OK videos)
}

// VideoProbeFunc probes a YouTube video and returns its stream status.
// This is typically wired to youtube.Service.ProbeVideoStatus. Takes a
// context so probes inherit the monitor's lifecycle — without it, in-flight
// probes finish out their full retry budget after the monitor is stopped.
type VideoProbeFunc func(ctx context.Context, videoID string) (*VideoProbeResult, error)

// MetadataFailureTracker tracks per-video probe failure counts.
// It is safe for concurrent use.
type MetadataFailureTracker struct {
	mu       sync.Mutex
	failures map[string]int
}

// NewMetadataFailureTracker creates a new failure tracker.
func NewMetadataFailureTracker() *MetadataFailureTracker {
	return &MetadataFailureTracker{
		failures: make(map[string]int),
	}
}

// RecordFailure increments the failure counter for a video and returns the
// new count. If the count reaches maxMetadataFailures, the entry is removed
// and shouldGiveUp is true.
func (t *MetadataFailureTracker) RecordFailure(videoID string) (count int, shouldGiveUp bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.failures[videoID]++
	count = t.failures[videoID]

	if count >= maxMetadataFailures {
		delete(t.failures, videoID)
		t.evictExcess()
		return count, true
	}

	t.evictExcess()
	return count, false
}

// ClearFailure removes the failure counter for a video (called on success).
func (t *MetadataFailureTracker) ClearFailure(videoID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failures, videoID)
}

// evictExcess caps the map size. Must be called with mu held.
func (t *MetadataFailureTracker) evictExcess() {
	excess := len(t.failures) - maxMetadataFailuresMapSize
	if excess <= 0 {
		return
	}
	for k := range t.failures {
		if excess <= 0 {
			break
		}
		delete(t.failures, k)
		excess--
	}
}

// ProbeCooldown caches "last probe time" per video ID to prevent every
// feed/DECAPI cycle from re-probing the same videos through the YouTube
// player API. Each monitor owns its own instance (created in its constructor,
// like MetadataTracker); the window is the operator-configurable
// config.Monitors.ProbeCooldown, hot-reloaded each cycle via SetDuration.
//
// A window of zero DISABLES the cooldown: ShouldProbe always returns true and
// Record is a no-op, so every cycle re-probes every listed video and the poll
// interval becomes the only throttle. This is the default — the operator opts
// into rate-limiting per-video re-probes by setting a non-zero window.
type ProbeCooldown struct {
	mu        sync.Mutex
	lastProbe map[string]time.Time
	duration  time.Duration
}

// NewProbeCooldown creates a cooldown cache with the given window. A window of
// zero (or negative) disables the cooldown. Monitors construct with the value
// from config and refresh it each cycle via SetDuration.
func NewProbeCooldown(duration time.Duration) *ProbeCooldown {
	return &ProbeCooldown{
		lastProbe: make(map[string]time.Time),
		duration:  duration,
	}
}

// SetDuration updates the cooldown window, letting a monitor hot-reload the
// configured value each cycle without recreating the cache. Existing recorded
// timestamps are honored against the new window on the next ShouldProbe.
func (cd *ProbeCooldown) SetDuration(duration time.Duration) {
	if cd == nil {
		return
	}
	cd.mu.Lock()
	cd.duration = duration
	cd.mu.Unlock()
}

// ShouldProbe returns true if the video has not been probed within the
// cooldown window. A nil receiver, or a disabled (<= 0) window, always
// returns true so call sites can be guarded uniformly without nil checks.
func (cd *ProbeCooldown) ShouldProbe(videoID string) bool {
	if cd == nil {
		return true
	}
	cd.mu.Lock()
	defer cd.mu.Unlock()
	if cd.duration <= 0 {
		return true
	}
	last, ok := cd.lastProbe[videoID]
	if !ok {
		return true
	}
	return time.Since(last) >= cd.duration
}

// enabled reports whether the cooldown window is active (non-nil, > 0).
func (cd *ProbeCooldown) enabled() bool {
	if cd == nil {
		return false
	}
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return cd.duration > 0
}

// Record marks videoID as just-probed with the current window. Called after a
// SUCCESSFUL probe classification, and after giving up on a persistently
// failing video (so it isn't re-probed every cycle). A no-op when the cooldown
// is disabled (<= 0), which also keeps the map empty in that mode.
func (cd *ProbeCooldown) Record(videoID string) {
	if cd == nil {
		return
	}
	cd.mu.Lock()
	defer cd.mu.Unlock()
	if cd.duration <= 0 {
		return
	}
	cd.lastProbe[videoID] = time.Now()
	cd.evictExcess()
}

// evictExcess caps the cooldown map by dropping the oldest entries. Must
// be called with mu held.
func (cd *ProbeCooldown) evictExcess() {
	excess := len(cd.lastProbe) - probeCooldownMaxSize
	if excess <= 0 {
		return
	}
	type entry struct {
		id string
		t  time.Time
	}
	entries := make([]entry, 0, len(cd.lastProbe))
	for id, t := range cd.lastProbe {
		entries = append(entries, entry{id, t})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].t.Before(entries[j].t) })
	for i := 0; i < excess && i < len(entries); i++ {
		delete(cd.lastProbe, entries[i].id)
	}
}

// ProcessYouTubeVideoParams holds the dependencies for ProcessYouTubeVideo.
type ProcessYouTubeVideoParams struct {
	Ctx        context.Context // forwarded to ProbeVideo so shutdown cancels in-flight probes
	VideoID    string
	Title      string
	Channel    *config.ChannelConfig
	ProbeVideo VideoProbeFunc
	Tracker    *MetadataFailureTracker
	Cooldown   *ProbeCooldown // optional: skips re-probes within the cooldown window
	IsReprobe  bool           // true if re-checking a previously processed video (logs demoted to Debug)
	// Repeat marks a video the caller classified last time as well: the
	// classification logs are demoted to Debug, as for IsReprobe, but nothing
	// is skipped as already processed — the video was never jobbed.
	Repeat bool
	Logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// ProcessYouTubeVideoResult holds the outcome of ProcessYouTubeVideo.
type ProcessYouTubeVideoResult struct {
	ShouldProcess bool   // true if the video should be queued as a job
	Title         string // possibly updated title from metadata
	// StreamStatus is the probe's classification ("live", "upcoming", "vod",
	// "post_live", "not_a_stream"). It is populated whenever a probe COMPLETED
	// — including the two arms that then decline to process the video — and is
	// empty only when no probe ran (the nil-probe passthrough) or the probe
	// errored or was suppressed by the cooldown. DECAPI reads it to pick a
	// JobDisposition (spec §10's creator table) and, since T2-12, to memoize a
	// terminal classification it must not pay for again next cycle.
	StreamStatus string
	// PublishedAt is the probe's authoritative publish date (RFC3339); empty
	// whenever StreamStatus is — and for broadcast probes, which supply no
	// date at all (§12). DECAPI reads it for the archive-window check on
	// vod-family results (§13).
	PublishedAt string
	// Denied is true when the probe COMPLETED, isDenied flagged it, AND the
	// refusal is one an anonymous prober can never see change —
	// members_only, see deniedIsSettled. It is the LATCH flag, not the
	// refusal flag: DECAPI writes it into its terminal memo and then stops
	// probing that video, because the classification rides on "upcoming",
	// which decapiTerminalStatus can never treat as terminal.
	//
	// ShouldProcess is false for EVERY refusal, latched or not. A
	// login_required refusal therefore returns ShouldProcess=false with
	// Denied=false: no job, no notification, and still re-probed next cycle
	// (bounded by DECAPI's 429 limiter and the poll interval), because that
	// pushback clears on its own and the latch would not.
	Denied bool
}

// nonLiveSkipReason decides whether a non-live classification (a VOD, post-live
// recording, or non-stream upload) should be skipped, and returns a distinct
// reason for the log so the two very different causes aren't conflated:
//   - "VODs not archived for this channel" — include_non_live_content is off.
//   - "already processed (in history)" — the channel DOES archive these, but the
//     video is already in the processing history (its job may have been deleted;
//     clear it from the Orphaned history list to let it be picked up again).
func nonLiveSkipReason(includeNonLive, isReprobe bool) (skip bool, reason string) {
	if !includeNonLive {
		return true, "VODs not archived for this channel"
	}
	if isReprobe {
		return true, "already processed (in history)"
	}
	return false, ""
}

// isDenied is THE denied predicate (spec §9) — stated once; every caller
// defers here. Distrust a probe result only when YouTube said it refused us
// AND the classifier was guessing:
//
//	denied ⇔ StreamStatus == 'upcoming' AND PlayabilityError ∈ {members_only, login_required}
//
// Broader rules are rejected in the spec: "trust only ok" kills genuine
// premieres; "any non-ok" refuses downloadable age-restricted VODs; "unknown"
// is not a refusal (it is 'we could not read the answer').
func isDenied(streamStatus, playabilityError string) bool {
	return streamStatus == "upcoming" &&
		(playabilityError == "members_only" || playabilityError == "login_required")
}

// deniedIsSettled splits the two refusals isDenied recognises by whether
// asking again could ever produce a different answer for a prober that has no
// credentials to retry with. Callers reach it only for a refusal (it does not
// re-test isDenied's status half); it decides whether the verdict may be
// REMEMBERED, never whether a job is created — no refusal creates a job.
//
//   - members_only: settled. The video IS members-only; an anonymous player
//     call says so every time. DECAPI has no authenticated probe at all (the
//     feed path owns that escalation, walk.go's probeRow), so its re-asking
//     buys nothing but ~240 player calls an hour.
//   - login_required: NOT settled. That is transient anti-bot pushback on a
//     PUBLIC video and it clears on its own — but the memo that would hold it
//     is released only by a new newest video, PruneHealth or a restart, so
//     remembering it would disable the RSS redundancy for that video long
//     after YouTube started answering again.
//
// age_restricted never reaches here: isDenied excludes it deliberately (the
// spec rejects "any non-ok" because it would refuse downloadable
// age-restricted VODs), so it stays an ordinary classification.
func deniedIsSettled(playabilityError string) bool {
	return playabilityError == "members_only"
}

// ProbeOutcome classifies the result of a single probeAndClassify call.
type ProbeOutcome int

const (
	OutcomeProbed   ProbeOutcome = iota // metadata returned, NOT denied — fresh classification
	OutcomeDenied                       // YouTube refused AND isDenied guessed why
	OutcomeErrored                      // the probe itself failed
	OutcomeCooldown                     // ProbeCooldown suppressed the probe this cycle
)

// ProbeClassifyParams holds the dependencies for probeAndClassify — the
// probe+classify core that ProcessYouTubeVideo recomposes. Deliberately a
// subset of ProcessYouTubeVideoParams: no IsReprobe (log-level demotion is a
// composed-function concern).
type ProbeClassifyParams struct {
	Ctx        context.Context // forwarded to ProbeVideo so shutdown cancels in-flight probes
	VideoID    string
	Channel    *config.ChannelConfig
	ProbeVideo VideoProbeFunc // REQUIRED — nil panics; the feed path has no passthrough mode, production always wires it (feed.go, decapi.go)
	Tracker    *MetadataFailureTracker
	Cooldown   *ProbeCooldown // optional: skips re-probes within the cooldown window
	Logger     interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// ProbeClassifyResult holds the outcome of probeAndClassify.
type ProbeClassifyResult struct {
	Outcome ProbeOutcome

	// StreamStatus, Title, PublishedAt, and PublishedPrecision
	// are populated on OutcomeProbed and OutcomeDenied (a successful probe);
	// zero-valued otherwise. PlayabilityError is always carried on those two
	// outcomes — the escalation reads it even when Outcome == OutcomeDenied.
	StreamStatus       string
	Title              string
	PublishedAt        string
	PublishedPrecision string
	PlayabilityError   string
}

// probeAndClassify runs one probe of a YouTube video and classifies it into
// one of four outcomes (OutcomeCooldown, OutcomeErrored, OutcomeDenied,
// OutcomeProbed). It owns the cooldown gate, the probe call, and the
// failure-tracker bookkeeping (RecordFailure/ClearFailure, cooldown Record on
// success or give-up) — exactly the middle third of what ProcessYouTubeVideo
// used to do inline. It performs NO AddToHistory and consults NO IsReprobe;
// those remain composed-function concerns in ProcessYouTubeVideo, which
// recomposes this.
//
// ProbeVideo is REQUIRED. A nil ProbeVideo is a programming error, not a
// runtime condition to handle gracefully — production always wires it, and
// the feed path's "no probe configured" fallback lives in
// ProcessYouTubeVideo's passthrough, checked before delegating here.
func probeAndClassify(p ProbeClassifyParams) ProbeClassifyResult {
	if p.ProbeVideo == nil {
		panic("probeAndClassify: ProbeVideo not wired")
	}

	// Cooldown gate: a probe within the configured window is recent enough
	// that re-probing now contributes nothing. Disabled by default
	// (config.Monitors.ProbeCooldown = 0 → ShouldProbe always true), so this
	// gate is a no-op unless the operator opts into a window. Audit
	// reports/monitor.md #5.
	if !p.Cooldown.ShouldProbe(p.VideoID) {
		p.Logger.Debug("[Monitor] Skipping re-probe within cooldown",
			"videoID", p.VideoID, "channel", p.Channel.Name)
		return ProbeClassifyResult{Outcome: OutcomeCooldown}
	}

	ctx := p.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	meta, err := p.ProbeVideo(ctx, p.VideoID)
	if err != nil {
		count, giveUp := p.Tracker.RecordFailure(p.VideoID)
		if giveUp {
			// Give up on this video. Nothing stops re-probing it — DECAPI
			// still calls ProcessYouTubeVideo and the feed walk still calls
			// probeAndClassify — so the cooldown is the only rate limiter.
			// (DECAPI used to write a history row here as well. History is
			// what the feed's archive gates VODs on, so three transient
			// failures on a channel's newest VOD kept it from ever being
			// archived.) Record the window (giveUp also resets the tracker's escalation
			// to 0), otherwise a broken-but-still-matching video re-probes
			// every cycle. When the cooldown is disabled the operator has
			// accepted that per-cycle re-probe (Record is a no-op) — the
			// poll interval is the throttle, and the log says so rather than
			// claiming a back-off that does not happen.
			p.Cooldown.Record(p.VideoID)
			next := "retrying next cycle"
			if p.Cooldown.enabled() {
				next = "backing off"
			}
			p.Logger.Warn(fmt.Sprintf("[Monitor] Failed to check metadata for %s %d times, %s: %v",
				p.VideoID, count, next, err))
		} else {
			// Transient failure: leave the cooldown UNRECORDED so the next
			// cycle re-probes promptly — a freshly-live video must not be
			// blinded by one flaky probe. The failure tracker bounds this to
			// maxMetadataFailures attempts before the give-up branch above.
			p.Logger.Warn(fmt.Sprintf("[Monitor] Failed to check metadata for %s (attempt %d/%d): %v",
				p.VideoID, count, maxMetadataFailures, err))
		}
		return ProbeClassifyResult{Outcome: OutcomeErrored}
	}

	// Successful probe: record the (configured) cooldown to limit total
	// re-probe rate, and clear the failure counter.
	p.Cooldown.Record(p.VideoID)
	p.Tracker.ClearFailure(p.VideoID)

	result := ProbeClassifyResult{
		Outcome:            OutcomeProbed,
		StreamStatus:       meta.StreamStatus,
		Title:              meta.Title,
		PublishedAt:        meta.PublishedAt,
		PublishedPrecision: meta.PublishedPrecision,
		PlayabilityError:   meta.PlayabilityError,
	}
	if isDenied(meta.StreamStatus, meta.PlayabilityError) {
		result.Outcome = OutcomeDenied
	}
	return result
}

// ProcessYouTubeVideo probes a YouTube video's metadata to classify its stream
// status before creating a job. This mirrors the TypeScript processYouTubeVideo
// from monitorUtils.ts.
//
// Returns ShouldProcess=true if the video should proceed to job creation,
// or false if it was skipped (non-stream, ended stream, or probe failure).
//
// Recomposed on top of probeAndClassify: this function owns the passthrough
// (no ProbeVideo configured) and the IsReprobe log-level demotion —
// probeAndClassify owns neither. See utils_test.go for the pinning tests.
//
// It writes NO history. History means "a job was created" (the host writes
// it at creation, monitor_callbacks.go), and both monitors' archive steps
// gate VODs on it, so a row for a video we only skipped or failed to probe
// kept that VOD from ever being archived — by the feed after three
// transient probe failures here, and by both monitors after a skip while
// include_non_live_content was off, even once it was turned on.
func ProcessYouTubeVideo(p ProcessYouTubeVideoParams) ProcessYouTubeVideoResult {
	includeNonLive := p.Channel.IncludeNonLiveContent

	// If no probe function is configured, pass through (backwards compatible)
	if p.ProbeVideo == nil {
		return ProcessYouTubeVideoResult{ShouldProcess: true, Title: p.Title}
	}

	cr := probeAndClassify(ProbeClassifyParams{
		Ctx:        p.Ctx,
		VideoID:    p.VideoID,
		Channel:    p.Channel,
		ProbeVideo: p.ProbeVideo,
		Tracker:    p.Tracker,
		Cooldown:   p.Cooldown,
		Logger:     p.Logger,
	})

	switch cr.Outcome {
	case OutcomeCooldown:
		return ProcessYouTubeVideoResult{ShouldProcess: false, Title: p.Title}

	case OutcomeErrored:
		return ProcessYouTubeVideoResult{ShouldProcess: false, Title: p.Title}

	case OutcomeDenied:
		// DENIED FIRST, and for the same reason archive.go puts it first: a
		// refusal carries StreamStatus "upcoming" BY DEFINITION (see
		// isDenied), so a table that reaches the classification switch below
		// launders the 2.7.2 misfire into a broadcast job — an Upcoming row,
		// a "Stream Found" notification, and then a COOKIES? park the config
		// never asked for.
		//
		// The cost of routing login_required away is that a channel under
		// sustained anti-bot pushback gets its upcoming streams from the feed
		// path only — which already lives with that, and only while the
		// pushback lasts, because Denied is deliberately NOT set for it
		// (deniedIsSettled): DECAPI keeps probing it, at the pre-fix cost,
		// bounded by the 429 limiter. Only members_only is remembered, and
		// what DECAPI remembers it stops asking about until the channel's
		// newest video changes, PruneHealth drops the channel, or the process
		// restarts — never because the refusal ended.
		deniedLog := p.Logger.Info
		if p.IsReprobe || p.Repeat {
			deniedLog = p.Logger.Debug
		}
		deniedLog(fmt.Sprintf("[Monitor] YouTube refused this video (%s); not creating a job: %s (%s)",
			cr.PlayabilityError, p.Title, p.VideoID))
		return ProcessYouTubeVideoResult{
			ShouldProcess: false,
			Denied:        deniedIsSettled(cr.PlayabilityError),
			Title:         p.Title,
			StreamStatus:  cr.StreamStatus,
		}
	}

	// cr.Outcome is OutcomeProbed here — the only outcome that reaches a
	// classification. OutcomeDenied used to fall through to this switch and be
	// treated as a plain "upcoming"; it now returns in the arm above.

	// Classify stream status (demote to Debug for re-probes of finished videos)
	logInfo := p.Logger.Info
	if p.IsReprobe || p.Repeat {
		logInfo = p.Logger.Debug
	}

	switch cr.StreamStatus {
	case "not_a_stream":
		if skip, reason := nonLiveSkipReason(includeNonLive, p.IsReprobe); skip {
			logInfo(fmt.Sprintf("[Monitor] Skipping non-stream content (%s): %s (%s)", reason, p.Title, p.VideoID))
			return ProcessYouTubeVideoResult{ShouldProcess: false, Title: p.Title, StreamStatus: cr.StreamStatus}
		}
		logInfo(fmt.Sprintf("[Monitor] Including non-stream content (include_non_live_content=true): %s (%s)", p.Title, p.VideoID))

	case "upcoming":
		logInfo(fmt.Sprintf("[Monitor] Found upcoming stream/premiere: %s (%s) - will monitor", p.Title, p.VideoID))

	case "live":
		// Always INFO for live — this is actionable even on re-probe
		p.Logger.Info(fmt.Sprintf("[Monitor] Found LIVE stream/premiere: %s (%s) - queuing for download", p.Title, p.VideoID))

	case "post_live", "vod":
		if skip, reason := nonLiveSkipReason(includeNonLive, p.IsReprobe); skip {
			logInfo(fmt.Sprintf("[Monitor] Skipping ended stream (%s, %s): %s (%s)", cr.StreamStatus, reason, p.Title, p.VideoID))
			return ProcessYouTubeVideoResult{ShouldProcess: false, Title: p.Title, StreamStatus: cr.StreamStatus}
		}
		logInfo(fmt.Sprintf("[Monitor] Including ended stream (include_non_live_content=true): %s (%s)", p.Title, p.VideoID))
	}

	p.Logger.Debug(fmt.Sprintf("[Monitor] Video %s classified as: %s", p.VideoID, cr.StreamStatus))

	// Use the metadata title if available, but don't let API fallback
	// placeholders ("Unknown Title") overwrite the real feed title.
	title := p.Title
	if cr.Title != "" && cr.Title != "Unknown Title" {
		title = cr.Title
	}

	return ProcessYouTubeVideoResult{
		ShouldProcess: true,
		Title:         title,
		StreamStatus:  cr.StreamStatus,
		PublishedAt:   cr.PublishedAt,
	}
}

// MatchesTerms checks if the given text matches any of the channel's configured terms.
// Returns true if no terms are configured (match everything).
func MatchesTerms(text string, channel *config.ChannelConfig) bool {
	terms := channel.Terms.Patterns()
	if len(terms) == 0 {
		return true
	}

	normText := normalizeText(text)
	for _, pattern := range terms {
		if matchTermNormalized(normText, text, pattern) {
			return true
		}
	}
	return false
}

// Compiled regex cache for matchTerm (bounded to prevent unbounded growth).
var (
	regexCacheMu sync.RWMutex
	regexCache   = make(map[string]*regexp.Regexp)
)

const maxRegexCacheSize = 200

func getCachedRegex(pattern string) (*regexp.Regexp, error) {
	regexCacheMu.RLock()
	re, ok := regexCache[pattern]
	regexCacheMu.RUnlock()
	if ok {
		return re, nil
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}

	regexCacheMu.Lock()
	if len(regexCache) >= maxRegexCacheSize {
		// Evict a random entry
		for k := range regexCache {
			delete(regexCache, k)
			break
		}
	}
	regexCache[pattern] = re
	regexCacheMu.Unlock()
	return re, nil
}

// matchTermNormalized checks whether text matches a single term pattern,
// taking pre-normalized text to avoid redundant work. All patterns are
// treated as regex; /pattern/flags syntax and a (?i) prefix are supported.
//
// The pattern must be transformed to correspond to the normalized text it
// runs against: normText is diacritic-stripped AND lowercased, so patterns
// get their literals diacritic-stripped (via normalizePattern) and case is
// handled by always compiling with the `i` flag. Lowercasing the pattern
// itself would be wrong — it corrupts escape classes (`\W` → `\w` inverts
// the match). Without this, `/Minecraft/` (uppercase, no flags) and any
// term containing a diacritic (`café`) could never match.
func matchTermNormalized(normText, rawText, pattern string) bool {
	// Check for /pattern/flags syntax
	if isRegexPattern(pattern) {
		inner, flags := parseRegexPattern(pattern)
		flags = goRegexFlags(flags)
		if !strings.Contains(flags, "i") {
			flags += "i"
		}
		re, err := getCachedRegex("(?" + flags + ")" + normalizePattern(inner))
		if err != nil {
			// Substring-match the BODY of the term, not the raw "/.../flags"
			// text: the delimiters and flags are syntax, not characters a
			// title could contain, so the raw term could never match.
			return fuzzyMatch(rawText, inner)
		}
		return re.MatchString(normText)
	}

	// Try as regex first (TS always uses new RegExp(pattern)).
	// Always case-insensitive — strip redundant (?i) prefix if present.
	finalPattern := strings.TrimPrefix(pattern, "(?i)")
	re, err := getCachedRegex("(?i)" + normalizePattern(finalPattern))
	if err != nil {
		// Invalid regex — fall back to fuzzy substring match
		return fuzzyMatch(rawText, pattern)
	}
	return re.MatchString(normText)
}

// goRegexFlags maps a /pattern/flags suffix onto what Go's (?flags) group
// accepts. The terms syntax is JavaScript's (inherited from the TypeScript
// archiver), and four of its flags have no Go spelling but also no bearing on
// a boolean "does the text match": g (every match) and y (sticky) only shape
// iteration, d only adds match indices, and u (Unicode) is the only mode a Go
// regexp has. Left in, "(?gi)karaoke" failed to compile and the term silently
// never matched anything. Everything else — Go's own i/m/s, or a letter
// neither engine knows — passes through, so a flag Go rejects still fails the
// compile and takes the substring fallback rather than being reinterpreted.
func goRegexFlags(flags string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case 'g', 'y', 'u', 'd':
			return -1
		}
		return r
	}, flags)
}

// isRegexPattern checks if a string looks like a regex pattern (/pattern/ or /pattern/flags).
func isRegexPattern(s string) bool {
	if !strings.HasPrefix(s, "/") {
		return false
	}
	lastSlash := strings.LastIndex(s[1:], "/")
	return lastSlash >= 0
}

// parseRegexPattern extracts the pattern and flags from a /pattern/flags string.
func parseRegexPattern(s string) (pattern, flags string) {
	inner := s[1:] // strip leading /
	lastSlash := strings.LastIndex(inner, "/")
	if lastSlash < 0 {
		return s, ""
	}
	return inner[:lastSlash], inner[lastSlash+1:]
}

// fuzzyMatch performs case-insensitive substring matching with Unicode normalization.
func fuzzyMatch(text, needle string) bool {
	normText := normalizeText(text)
	normNeedle := normalizeText(needle)
	return strings.Contains(normText, normNeedle)
}

// stripDiacritics NFD-decomposes s and removes combining marks. ASCII-only
// strings take a fast path with no decomposition or allocation. This is the
// shared core of normalizeText and normalizePattern — the two MUST transform
// characters identically, or patterns stop corresponding to the text they
// are matched against.
func stripDiacritics(s string) string {
	isASCII := true
	for i := range len(s) {
		if s[i] >= 0x80 {
			isASCII = false
			break
		}
	}
	if isASCII {
		return s
	}

	decomposed := norm.NFD.String(s)
	var b strings.Builder
	for _, r := range decomposed {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizePattern strips diacritics from a regex pattern's literal
// characters so they correspond to the diacritic-stripped text the compiled
// regex runs against. Deliberately does NOT lowercase — see
// matchTermNormalized (ToLower would corrupt escape classes like \W → \w).
// Regex metacharacters are ASCII and unaffected by the transform.
func normalizePattern(s string) string {
	return stripDiacritics(s)
}

// normalizeText applies NFD normalization, strips diacritics, and lowercases.
func normalizeText(s string) string {
	return strings.ToLower(stripDiacritics(s))
}
