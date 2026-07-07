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
	StreamStatus string // "live", "upcoming", "vod", "post_live", "not_a_stream"
	Title        string // metadata title (may be better than feed title)
	ChannelName  string // metadata channel name
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
	Ctx          context.Context // forwarded to ProbeVideo so shutdown cancels in-flight probes
	VideoID      string
	Title        string
	Channel      *config.ChannelConfig
	ProbeVideo   VideoProbeFunc
	AddToHistory func(videoID string) error
	Tracker      *MetadataFailureTracker
	Cooldown     *ProbeCooldown // optional: skips re-probes within the cooldown window
	IsReprobe    bool           // true if re-checking a previously processed video (logs demoted to Debug)
	Logger       interface {
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
	ChannelName   string // possibly updated channel name from metadata
}

// ProcessYouTubeVideo probes a YouTube video's metadata to classify its stream
// status before creating a job. This mirrors the TypeScript processYouTubeVideo
// from monitorUtils.ts.
//
// Returns ShouldProcess=true if the video should proceed to job creation,
// or false if it was skipped (non-stream, ended stream, or probe failure).
func ProcessYouTubeVideo(p ProcessYouTubeVideoParams) ProcessYouTubeVideoResult {
	includeNonLive := p.Channel.IncludeNonLiveContent

	// If no probe function is configured, pass through (backwards compatible)
	if p.ProbeVideo == nil {
		return ProcessYouTubeVideoResult{ShouldProcess: true, Title: p.Title}
	}

	// Cooldown gate: a probe within the configured window is recent enough
	// that re-probing now contributes nothing. Returning ShouldProcess=false
	// here means upstream skips OnVideoFound for this cycle and waits for the
	// cooldown to expire. Disabled by default (config.Monitors.ProbeCooldown
	// = 0 → ShouldProbe always true), so this gate is a no-op unless the
	// operator opts into a window. Audit reports/monitor.md #5.
	if !p.Cooldown.ShouldProbe(p.VideoID) {
		p.Logger.Debug("[Monitor] Skipping re-probe within cooldown",
			"videoID", p.VideoID, "channel", p.Channel.Name)
		return ProcessYouTubeVideoResult{ShouldProcess: false, Title: p.Title}
	}

	ctx := p.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	meta, err := p.ProbeVideo(ctx, p.VideoID)
	if err != nil {
		count, giveUp := p.Tracker.RecordFailure(p.VideoID)
		if giveUp {
			// Give up on this video. AddToHistory does NOT actually stop
			// re-probing — HasProcessed only flips the reprobe/log-level
			// flag; feed/DECAPI still call ProcessYouTubeVideo — so the
			// cooldown is the only rate limiter. Record the window (giveUp
			// also resets the tracker's escalation to 0), otherwise a
			// broken-but-still-matching video re-probes every cycle. When the
			// cooldown is disabled the operator has accepted that per-cycle
			// re-probe (Record is a no-op) — the poll interval is the throttle.
			p.Cooldown.Record(p.VideoID)
			p.Logger.Warn(fmt.Sprintf("[Monitor] Failed to check metadata for %s %d times, backing off: %v",
				p.VideoID, count, err))
			if p.AddToHistory != nil {
				p.AddToHistory(p.VideoID)
			}
		} else {
			// Transient failure: leave the cooldown UNRECORDED so the next
			// cycle re-probes promptly — a freshly-live video must not be
			// blinded by one flaky probe. The failure tracker bounds this to
			// maxMetadataFailures attempts before the give-up branch above.
			p.Logger.Warn(fmt.Sprintf("[Monitor] Failed to check metadata for %s (attempt %d/%d): %v",
				p.VideoID, count, maxMetadataFailures, err))
		}
		return ProcessYouTubeVideoResult{ShouldProcess: false, Title: p.Title}
	}

	// Successful probe: record the (configured) cooldown to limit total
	// re-probe rate, and clear the failure counter.
	p.Cooldown.Record(p.VideoID)
	p.Tracker.ClearFailure(p.VideoID)

	// Classify stream status (demote to Debug for re-probes of finished videos)
	logInfo := p.Logger.Info
	if p.IsReprobe {
		logInfo = p.Logger.Debug
	}

	switch meta.StreamStatus {
	case "not_a_stream":
		if !includeNonLive || p.IsReprobe {
			logInfo(fmt.Sprintf("[Monitor] Skipping non-stream content: %s (%s)", p.Title, p.VideoID))
			if p.AddToHistory != nil {
				p.AddToHistory(p.VideoID)
			}
			return ProcessYouTubeVideoResult{ShouldProcess: false, Title: p.Title}
		}
		logInfo(fmt.Sprintf("[Monitor] Including non-stream content (include_non_live_content=true): %s (%s)", p.Title, p.VideoID))

	case "upcoming":
		logInfo(fmt.Sprintf("[Monitor] Found upcoming stream/premiere: %s (%s) - will monitor", p.Title, p.VideoID))

	case "live":
		// Always INFO for live — this is actionable even on re-probe
		p.Logger.Info(fmt.Sprintf("[Monitor] Found LIVE stream/premiere: %s (%s) - queuing for download", p.Title, p.VideoID))

	case "post_live", "vod":
		if !includeNonLive || p.IsReprobe {
			logInfo(fmt.Sprintf("[Monitor] Skipping ended stream (%s): %s (%s)", meta.StreamStatus, p.Title, p.VideoID))
			if p.AddToHistory != nil {
				p.AddToHistory(p.VideoID)
			}
			return ProcessYouTubeVideoResult{ShouldProcess: false, Title: p.Title}
		}
		logInfo(fmt.Sprintf("[Monitor] Including ended stream (include_non_live_content=true): %s (%s)", p.Title, p.VideoID))
	}

	p.Logger.Debug(fmt.Sprintf("[Monitor] Video %s classified as: %s", p.VideoID, meta.StreamStatus))

	// Use metadata title/channel if available, but don't let API fallback
	// placeholders ("Unknown Title") overwrite the real feed title.
	title := p.Title
	if meta.Title != "" && meta.Title != "Unknown Title" {
		title = meta.Title
	}
	channelName := ""
	if meta.ChannelName != "" {
		channelName = meta.ChannelName
	}

	return ProcessYouTubeVideoResult{
		ShouldProcess: true,
		Title:         title,
		ChannelName:   channelName,
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

// matchTerm checks if text matches a single term pattern.
// All patterns are treated as regex (matching TypeScript behavior).
// Supports /pattern/flags syntax and (?i) prefix for case-insensitive.
func matchTerm(text, pattern string) bool {
	return matchTermNormalized(normalizeText(text), text, pattern)
}

// matchTermNormalized is like matchTerm but takes pre-normalized text to avoid redundant work.
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
		if !strings.Contains(flags, "i") {
			flags += "i"
		}
		re, err := getCachedRegex("(?" + flags + ")" + normalizePattern(inner))
		if err != nil {
			return fuzzyMatch(rawText, pattern)
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
