package monitor

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
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
)

// VideoProbeResult holds the stream status returned by a video probe.
type VideoProbeResult struct {
	StreamStatus string // "live", "upcoming", "vod", "post_live", "not_a_stream"
	Title        string // metadata title (may be better than feed title)
	ChannelName  string // metadata channel name
}

// VideoProbeFunc probes a YouTube video and returns its stream status.
// This is typically wired to youtube.Service.ProbeVideoStatus.
type VideoProbeFunc func(videoID string) (*VideoProbeResult, error)

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

// ProcessYouTubeVideoParams holds the dependencies for ProcessYouTubeVideo.
type ProcessYouTubeVideoParams struct {
	VideoID  string
	Title    string
	Channel  *config.ChannelConfig
	ProbeVideo VideoProbeFunc
	AddToHistory func(videoID string) error
	Tracker  *MetadataFailureTracker
	Logger   interface {
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

	meta, err := p.ProbeVideo(p.VideoID)
	if err != nil {
		count, giveUp := p.Tracker.RecordFailure(p.VideoID)
		if giveUp {
			p.Logger.Warn(fmt.Sprintf("[Monitor] Failed to check metadata for %s %d times, adding to history to stop retrying: %v",
				p.VideoID, count, err))
			if p.AddToHistory != nil {
				p.AddToHistory(p.VideoID)
			}
		} else {
			p.Logger.Warn(fmt.Sprintf("[Monitor] Failed to check metadata for %s (attempt %d/%d): %v",
				p.VideoID, count, maxMetadataFailures, err))
		}
		return ProcessYouTubeVideoResult{ShouldProcess: false, Title: p.Title}
	}

	// Clear failure counter on success
	p.Tracker.ClearFailure(p.VideoID)

	// Classify stream status
	switch meta.StreamStatus {
	case "not_a_stream":
		if !includeNonLive {
			p.Logger.Info(fmt.Sprintf("[Monitor] Skipping non-stream content: %s (%s)", p.Title, p.VideoID))
			if p.AddToHistory != nil {
				p.AddToHistory(p.VideoID)
			}
			return ProcessYouTubeVideoResult{ShouldProcess: false, Title: p.Title}
		}
		p.Logger.Info(fmt.Sprintf("[Monitor] Including non-stream content (include_non_live_content=true): %s (%s)", p.Title, p.VideoID))

	case "upcoming":
		p.Logger.Info(fmt.Sprintf("[Monitor] Found upcoming stream/premiere: %s (%s) - will monitor", p.Title, p.VideoID))

	case "live":
		p.Logger.Info(fmt.Sprintf("[Monitor] Found LIVE stream/premiere: %s (%s) - queuing for download", p.Title, p.VideoID))

	case "post_live", "vod":
		if !includeNonLive {
			p.Logger.Info(fmt.Sprintf("[Monitor] Skipping ended stream (%s): %s (%s)", meta.StreamStatus, p.Title, p.VideoID))
			if p.AddToHistory != nil {
				p.AddToHistory(p.VideoID)
			}
			return ProcessYouTubeVideoResult{ShouldProcess: false, Title: p.Title}
		}
		p.Logger.Info(fmt.Sprintf("[Monitor] Including ended stream (include_non_live_content=true): %s (%s)", p.Title, p.VideoID))
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
func matchTermNormalized(normText, rawText, pattern string) bool {
	// Check for /pattern/flags syntax
	if isRegexPattern(pattern) {
		inner, flags := parseRegexPattern(pattern)
		re, err := getCachedRegex("(?" + flags + ")" + inner)
		if err != nil {
			return fuzzyMatch(rawText, pattern)
		}
		return re.MatchString(normText)
	}

	// Handle (?i) prefix
	finalPattern := pattern
	if strings.HasPrefix(finalPattern, "(?i)") {
		finalPattern = finalPattern[4:]
	}

	// Try as regex first (TS always uses new RegExp(pattern))
	re, err := getCachedRegex("(?i)" + finalPattern)
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

// normalizeText applies NFD normalization, strips diacritics, and lowercases.
func normalizeText(s string) string {
	// NFD decompose
	decomposed := norm.NFD.String(s)

	// Strip combining marks (diacritics)
	var b strings.Builder
	for _, r := range decomposed {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}

	return strings.ToLower(b.String())
}
