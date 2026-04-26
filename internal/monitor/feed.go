package monitor

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/httpx"
)

const (
	feedFetchTimeout    = 15 * time.Second
	feedProcessTimeout  = 60 * time.Second
	defaultMaxFeedItems = 15
	// feedStagger spaces consecutive channel feed fetches. Decapi and Twitch
	// already stagger; a tight loop of YouTube RSS fetches on a big channel
	// list looks like scraping behavior from a single source IP.
	feedStagger = 500 * time.Millisecond
)

// monitorHTTPClient is a shared HTTP client for monitor HTTP requests.
// Backed by the shared httpx transport so keep-alive amortises across
// monitor / cookies / youtube fetches against the same hosts.
var monitorHTTPClient = httpx.Client(30 * time.Second)

// ConnectivityReporter is the subset of connectivity.Monitor we invoke from
// monitor HTTP paths. Wiring this into the FeedMonitor and DecapiMonitor lets
// their fetches contribute to the passive-outage tracker (see
// internal/connectivity/passive.go) so a DNS outage that hits only YouTube
// RSS or DECAPI can still flip the global online/offline state.
type ConnectivityReporter interface {
	ReportFailure(tag string)
	ReportSuccess(tag string)
}

// connReporter is an atomic.Pointer so SetConnectivityReporter can be called
// without racing against concurrent fetches. In practice main.go installs
// the reporter once at startup, but making the read lock-free removes a
// happens-before foot-gun for future callers or tests.
var connReporter atomic.Pointer[ConnectivityReporter]

// SetConnectivityReporter wires the package-wide connectivity reporter for
// monitor HTTP paths. Safe to call concurrently with in-flight fetches.
func SetConnectivityReporter(r ConnectivityReporter) {
	if r == nil {
		connReporter.Store(nil)
		return
	}
	connReporter.Store(&r)
}

// reportMonitorResult forwards a fetch outcome to the installed reporter, if
// any. tag identifies the subsystem (e.g. "monitor/feed", "monitor/decapi")
// so the passive tracker can count distinct-subsystem failures toward the
// offline-trigger threshold.
func reportMonitorResult(tag string, failed bool) {
	rp := connReporter.Load()
	if rp == nil {
		return
	}
	if failed {
		(*rp).ReportFailure(tag)
	} else {
		(*rp).ReportSuccess(tag)
	}
}

// FeedMonitor polls YouTube RSS feeds for new videos from monitored channels.
type FeedMonitor struct {
	mu          sync.Mutex
	configStore *config.Store
	db          *database.Database
	checking    bool
	timer       *time.Timer
	ctx         context.Context
	cancel      context.CancelFunc
	NextCheckAt int64 // epoch ms

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	OnSchedule      func(nextCheckAt int64)
	OnVideoFound    func(videoID, title, url string, channel *config.ChannelConfig)
	ProbeVideo      VideoProbeFunc
	MetadataTracker *MetadataFailureTracker
	ProbeCooldown   *ProbeCooldown // optional: shared with DecapiMonitor to dedup re-probes
	IsOnline        func() bool    // nil = always online
}

// NewFeedMonitor creates a new RSS feed monitor. The Store carries the
// cfg+lock used to read channel list and interval settings; all reads
// happen under configStore.Read so a config-reload doesn't race against
// an in-flight cycle (audit reports/monitor.md Critical Issue #1).
func NewFeedMonitor(store *config.Store, db *database.Database, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *FeedMonitor {
	return &FeedMonitor{
		configStore:     store,
		db:              db,
		logger:          logger,
		MetadataTracker: NewMetadataFailureTracker(),
	}
}

// Start begins the feed monitoring loop.
func (fm *FeedMonitor) Start(ctx context.Context) {
	fm.mu.Lock()
	if fm.cancel != nil {
		fm.mu.Unlock()
		return // Already running
	}
	ctx, cancel := context.WithCancel(ctx)
	fm.ctx = ctx
	fm.cancel = cancel
	fm.mu.Unlock()

	fm.logger.Info("feed monitor started")

	// Immediate first check on startup (runCycle schedules next in its defer)
	go fm.runCycle(ctx)
}

// Stop stops the feed monitor.
func (fm *FeedMonitor) Stop() {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	if fm.cancel != nil {
		fm.cancel()
		fm.ctx = nil
		fm.cancel = nil
	}
	if fm.timer != nil {
		fm.timer.Stop()
		fm.timer = nil
	}
	fm.NextCheckAt = 0
	fm.logger.Info("feed monitor stopped")
}

// GetNextCheckAt returns the next scheduled check time in epoch ms.
func (fm *FeedMonitor) GetNextCheckAt() int64 {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	return fm.NextCheckAt
}

// CheckNow triggers an immediate feed check if the monitor is running.
func (fm *FeedMonitor) CheckNow() {
	fm.mu.Lock()
	if fm.cancel == nil {
		fm.mu.Unlock()
		return // Not running
	}
	ctx := fm.ctx
	fm.mu.Unlock()
	go fm.runCycle(ctx)
}

func (fm *FeedMonitor) scheduleNext(ctx context.Context) {
	channels := fm.getYouTubeChannels()
	if len(channels) == 0 {
		fm.mu.Lock()
		fm.NextCheckAt = 0
		fm.mu.Unlock()
		if fm.OnSchedule != nil {
			fm.OnSchedule(0)
		}
		return
	}

	var interval time.Duration
	fm.configStore.Read(func(c *config.MoomboxConfig) {
		interval = c.Monitors.FeedCheckInterval.AsDuration(time.Minute)
	})
	if interval < time.Minute {
		interval = 10 * time.Minute
	}

	// Add jitter (±10% of interval)
	tenPercent := int64(interval) / 10
	if tenPercent > 0 {
		interval = interval - time.Duration(tenPercent) + time.Duration(rand.Int63n(2*tenPercent))
	}

	fm.mu.Lock()
	// Don't schedule if monitor was stopped (cancel set to nil)
	if fm.cancel == nil {
		fm.mu.Unlock()
		return
	}
	fm.NextCheckAt = time.Now().Add(interval).UnixMilli()
	if fm.timer != nil {
		fm.timer.Stop()
	}
	fm.timer = time.AfterFunc(interval, func() {
		fm.runCycle(ctx)
	})
	next := fm.NextCheckAt
	fm.mu.Unlock()

	if fm.OnSchedule != nil {
		fm.OnSchedule(next)
	}

	fm.logger.Debug("feed check scheduled", "in", interval.Round(time.Second))
}

func (fm *FeedMonitor) runCycle(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			fm.logger.Error("feed monitor runCycle panic", "panic", r)
		}
	}()

	fm.mu.Lock()
	if fm.checking {
		fm.mu.Unlock()
		return
	}
	fm.checking = true
	fm.mu.Unlock()

	defer func() {
		fm.mu.Lock()
		fm.checking = false
		fm.mu.Unlock()
		fm.scheduleNext(ctx)
	}()

	fm.doCheck(ctx)
}

func (fm *FeedMonitor) doCheck(ctx context.Context) {
	if fm.IsOnline != nil && !fm.IsOnline() {
		fm.logger.Debug("skipping feed poll — offline")
		return
	}
	channels := fm.getYouTubeChannels()
	if len(channels) == 0 {
		return
	}

	fm.logger.Info("checking feeds", "channels", len(channels))

	for i := range channels {
		select {
		case <-ctx.Done():
			return
		default:
		}

		ch := &channels[i]
		if err := fm.checkChannel(ctx, ch); err != nil {
			fm.logger.Warn("feed check failed", "channel", ch.Name, "err", err)
		}

		// Stagger between requests to avoid looking like a scraper and to
		// match the pacing of the other two monitors.
		if i < len(channels)-1 {
			staggerTimer := time.NewTimer(feedStagger)
			select {
			case <-ctx.Done():
				staggerTimer.Stop()
				return
			case <-staggerTimer.C:
			}
		}
	}
}

func (fm *FeedMonitor) checkChannel(ctx context.Context, ch *config.ChannelConfig) error {
	feedURL := fmt.Sprintf("https://www.youtube.com/feeds/videos.xml?channel_id=%s", ch.ID)

	// Use a short timeout for the feed HTTP fetch itself
	fetchCtx, fetchCancel := context.WithTimeout(ctx, feedFetchTimeout)
	defer fetchCancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, feedURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := monitorHTTPClient.Do(req)
	if err != nil {
		// Transport-level failure — most likely a DNS error, TCP reset, or
		// context deadline. Contributes toward the passive offline trigger.
		reportMonitorResult("monitor/feed", true)
		return fmt.Errorf("fetch feed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 4xx/5xx is not necessarily a connectivity problem (YouTube could be
		// rate-limiting or a channel ID is dead), but we don't want to flag
		// these as success either. Leave the tracker alone.
		io.Copy(io.Discard, resp.Body) // drain for connection reuse
		return fmt.Errorf("feed http %d", resp.StatusCode)
	}
	reportMonitorResult("monitor/feed", false)

	data, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB limit
	if err != nil {
		return err
	}

	// Use a longer timeout for video processing (ProbeVideo can be slow)
	processCtx, processCancel := context.WithTimeout(ctx, feedProcessTimeout)
	defer processCancel()

	return fm.processFeed(processCtx, ch, data)
}

// atomFeed represents the Atom XML feed structure.
type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	VideoID    string         `xml:"http://www.youtube.com/xml/schemas/2015 videoId"`
	Title      string         `xml:"title"`
	Links      []atomLink     `xml:"link"`
	MediaGroup atomMediaGroup `xml:"http://search.yahoo.com/mrss/ group"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
}

type atomMediaGroup struct {
	Description string `xml:"http://search.yahoo.com/mrss/ description"`
}

func (fm *FeedMonitor) processFeed(ctx context.Context, ch *config.ChannelConfig, data []byte) error {
	var feed atomFeed
	if err := xml.Unmarshal(data, &feed); err != nil {
		return fmt.Errorf("parse feed: %w", err)
	}

	maxItems := defaultMaxFeedItems
	if ch.MaxFeedItems != nil && *ch.MaxFeedItems > 0 {
		maxItems = *ch.MaxFeedItems
	} else {
		var cfgMax int
		fm.configStore.Read(func(c *config.MoomboxConfig) {
			cfgMax = c.Monitors.MaxFeedItems
		})
		if cfgMax > 0 {
			maxItems = cfgMax
		}
	}

	entries := feed.Entries
	if len(entries) > maxItems {
		entries = entries[:maxItems]
	}

	if len(entries) == 0 {
		return nil
	}

	// Build lookbehind set for description dedup
	lookbehind := 0
	if ch.NumDescLookbehind != nil {
		lookbehind = *ch.NumDescLookbehind
	}

	// Precompute per-entry line sets once; filterUniqueDescriptionLines
	// previously rebuilt these from scratch for every i, producing O(N*M*K)
	// trim work (N entries × M older entries × K lines) per channel.
	var entryLineSets []map[string]struct{}
	if lookbehind > 0 {
		entryLineSets = make([]map[string]struct{}, len(entries))
		for i := range entries {
			entryLineSets[i] = descriptionLineSet(entries[i].MediaGroup.Description)
		}
	}

	for i, entry := range entries {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		videoID := entry.VideoID
		if videoID == "" {
			continue
		}

		// Skip if active job exists (but not if merely finished — stream may restart on same URL)
		active, err := fm.db.HasActiveJob(videoID)
		if err != nil {
			// Don't swallow DB errors — proceeding as if no active job could
			// create duplicates if the DB was simply busy. Log and skip this
			// entry for the current cycle.
			fm.logger.Debug("HasActiveJob query failed", "videoID", videoID, "err", err)
			continue
		}
		if active {
			continue
		}

		// Description dedup: filter lines that appear in older entries
		description := entry.MediaGroup.Description
		if lookbehind > 0 && i+1 < len(entries) {
			end := min(i+1+lookbehind, len(entries))
			description = filterUniqueDescriptionLinesPrecomputed(description, entryLineSets[i+1:end])
		}

		// Get video URL
		videoURL := ""
		for _, link := range entry.Links {
			if link.Rel == "alternate" {
				videoURL = link.Href
				break
			}
		}
		if videoURL == "" {
			videoURL = fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID)
		}

		// Term matching: title OR description can match independently
		title := entry.Title
		titleMatch := MatchesTerms(title, ch)
		descMatch := description != "" && MatchesTerms(description, ch)

		if !titleMatch && !descMatch {
			continue
		}

		// Check if this is a re-probe of a previously processed video.
		// Ignore the error — a DB-busy here is best treated as "not yet
		// processed" so we probe and let upstream dedup (INSERT-OR-IGNORE)
		// handle collisions. We still log for visibility.
		reprobe, hpErr := fm.db.HasProcessed(videoID)
		if hpErr != nil {
			fm.logger.Debug("HasProcessed query failed", "videoID", videoID, "err", hpErr)
		}

		if reprobe {
			fm.logger.Debug("feed match found (re-probe)",
				"videoID", videoID,
				"title", title,
				"channel", ch.Name)
		} else {
			fm.logger.Info("feed match found",
				"videoID", videoID,
				"title", title,
				"channel", ch.Name)
		}

		// Probe video metadata to classify stream status before creating job
		result := ProcessYouTubeVideo(ProcessYouTubeVideoParams{
			Ctx:          ctx,
			VideoID:      videoID,
			Title:        title,
			Channel:      ch,
			ProbeVideo:   fm.ProbeVideo,
			AddToHistory: func(id string) error { return fm.db.AddToHistory(id) },
			Tracker:      fm.MetadataTracker,
			Cooldown:     fm.ProbeCooldown,
			IsReprobe:    reprobe,
			Logger:       fm.logger,
		})
		if !result.ShouldProcess {
			continue
		}

		if fm.OnVideoFound != nil {
			fm.OnVideoFound(videoID, result.Title, videoURL, ch)
		}
	}

	return nil
}

// descriptionLineSet builds the trimmed-line lookup set for a description.
// Sharing one set per entry across the outer loop keeps dedup work linear in
// total lines rather than quadratic in entries.
func descriptionLineSet(description string) map[string]struct{} {
	set := make(map[string]struct{})
	for line := range strings.SplitSeq(description, "\n") {
		set[strings.TrimSpace(line)] = struct{}{}
	}
	return set
}

// filterUniqueDescriptionLinesPrecomputed removes lines that appear in any of
// the precomputed older-entry line sets.
func filterUniqueDescriptionLinesPrecomputed(description string, olderLineSets []map[string]struct{}) string {
	var unique []string
	for line := range strings.SplitSeq(description, "\n") {
		trimmed := strings.TrimSpace(line)
		found := false
		for _, set := range olderLineSets {
			if _, ok := set[trimmed]; ok {
				found = true
				break
			}
		}
		if !found {
			unique = append(unique, line)
		}
	}
	return strings.Join(unique, "\n")
}

// filterUniqueDescriptionLines is retained for backwards-compat (and tests)
// but now routes through the precomputed-set implementation.
func filterUniqueDescriptionLines(description string, olderEntries []atomEntry) string {
	sets := make([]map[string]struct{}, len(olderEntries))
	for i := range olderEntries {
		sets[i] = descriptionLineSet(olderEntries[i].MediaGroup.Description)
	}
	return filterUniqueDescriptionLinesPrecomputed(description, sets)
}

// getYouTubeChannels returns a copy of the YouTube channel list under
// configStore.Read so doCheck can iterate freely without holding the lock
// across network calls. Closes the cfgMu race flagged in
// reports/monitor.md Critical Issue #1.
func (fm *FeedMonitor) getYouTubeChannels() []config.ChannelConfig {
	var channels []config.ChannelConfig
	fm.configStore.Read(func(c *config.MoomboxConfig) {
		for _, ch := range c.Channels {
			if ch.Enabled != nil && !*ch.Enabled {
				continue
			}
			if ch.Platform == "twitch" {
				continue
			}
			channels = append(channels, ch)
		}
	})
	return channels
}
