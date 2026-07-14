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

// MembershipVideo is a members-only video discovered from a channel's
// membership tab. It mirrors youtube.MembershipVideo but is declared here so
// the monitor package stays decoupled from the youtube package (the wiring
// closure in cmd/moombox adapts between the two, exactly like ProbeVideo does).
type MembershipVideo struct {
	VideoID string
	Title   string
}

// MembershipFetchFunc fetches the members-only videos listed on a channel's
// authenticated /membership tab. It returns an empty slice (no error) when
// there are no auth cookies or the account is not a member of the channel.
// Typically wired to youtube.Service.FetchMembershipVideos.
type MembershipFetchFunc func(ctx context.Context, channelID string) ([]MembershipVideo, error)

// FeedMonitor polls YouTube RSS feeds for new videos from monitored channels.
type FeedMonitor struct {
	mu          sync.Mutex
	configStore *config.Store
	db          *database.Database
	checking    bool
	// pendingKick latches a CheckNow that landed while a cycle was in
	// flight — previously silently dropped. Consumed in runCycle's defer.
	pendingKick bool
	// warnedSlow rate-limits the oversubscribed warning; atomic because
	// scheduleNext touches it outside the monitor mutex.
	warnedSlow  atomic.Bool
	timer       *time.Timer
	ctx         context.Context
	cancel      context.CancelFunc
	NextCheckAt int64 // epoch ms; -1 = check in progress, 0 = no channels

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	health *healthTracker

	OnSchedule      func(nextCheckAt int64)
	OnVideoFound    func(videoID, title, url string, channel *config.ChannelConfig)
	ProbeVideo      VideoProbeFunc
	MetadataTracker *MetadataFailureTracker
	ProbeCooldown   *ProbeCooldown // per-monitor; window from config, refreshed each cycle
	IsOnline        func() bool    // nil = always online

	// FetchMembership discovers members-only videos via a channel's
	// authenticated /membership tab. RSS never lists members-only content, so
	// this is the ONLY discovery source for members-only live/upcoming streams
	// (and, when include_non_live_content is set, their VODs/premieres). Nil
	// disables membership discovery. Wired to youtube.Service.FetchMembershipVideos.
	FetchMembership MembershipFetchFunc
	// MembershipEnabled gates membership discovery each cycle — typically
	// "config flag on AND YouTube auth cookies present". Nil means "always
	// enabled whenever FetchMembership is set".
	MembershipEnabled func() bool
}

// Health returns the per-channel health snapshot for /api/status.
func (fm *FeedMonitor) Health() []ChannelHealth { return fm.health.snapshot() }

// PruneHealth drops health entries for channels no longer configured.
func (fm *FeedMonitor) PruneHealth() {
	active := make(map[string]struct{})
	for _, ch := range fm.getYouTubeChannels() {
		active[ch.ID] = struct{}{}
	}
	fm.health.prune(active)
}

// SetOnChannelUnhealthy installs the callback fired when a channel crosses
// the consecutive-failure threshold.
func (fm *FeedMonitor) SetOnChannelUnhealthy(fn func(channelID string, consecutive int, lastErr string)) {
	fm.health.onUnhealthy = fn
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
		health:          newHealthTracker(),
		MetadataTracker: NewMetadataFailureTracker(),
		// Window is set from config at the start of every cycle via
		// refreshProbeCooldown (before any probe runs), so the zero here just
		// means "disabled until the first cycle reads config".
		ProbeCooldown: NewProbeCooldown(0),
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

// scheduleNext arms the next cycle. cycleStart anchors fixed-RATE
// scheduling: the delay is interval minus the elapsed cycle time, so the
// configured interval is a true period — previously it was a GAP after
// each cycle (which can stretch by minutes when inline probes run).
// Zero-value cycleStart behaves as a plain interval.
func (fm *FeedMonitor) scheduleNext(ctx context.Context, cycleStart time.Time) {
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

	delay := interval
	if !cycleStart.IsZero() {
		elapsed := time.Since(cycleStart)
		delay = interval - elapsed
		if delay < time.Second {
			// Warn only when the cycle ran WELL past the interval (>2×) —
			// feed cycles run inline probes that can legitimately take a
			// while; once, via the atomic guard.
			if elapsed >= 2*interval && fm.warnedSlow.CompareAndSwap(false, true) {
				fm.logger.Warn("feed check cycle takes far longer than the configured interval — effective cadence degraded",
					"cycle", elapsed.Round(time.Second), "interval", interval.Round(time.Second))
			}
			delay = time.Second
		}
	}

	fm.mu.Lock()
	// Don't schedule if monitor was stopped; clear the checking sentinel so
	// a stopped monitor never reports -1 forever.
	if fm.cancel == nil {
		fm.NextCheckAt = 0
		fm.mu.Unlock()
		return
	}
	fm.NextCheckAt = time.Now().Add(delay).UnixMilli()
	if fm.timer != nil {
		fm.timer.Stop()
	}
	fm.timer = time.AfterFunc(delay, func() {
		fm.runCycle(ctx)
	})
	next := fm.NextCheckAt
	fm.mu.Unlock()

	if fm.OnSchedule != nil {
		fm.OnSchedule(next)
	}

	fm.logger.Debug("feed check scheduled", "in", delay.Round(time.Second))
}

func (fm *FeedMonitor) runCycle(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			fm.logger.Error("feed monitor runCycle panic", "panic", r)
		}
	}()

	cycleStart := time.Now()
	fm.mu.Lock()
	if fm.checking {
		// A kick landed mid-cycle: latch it so the defer re-runs
		// immediately instead of silently dropping it (the new channel
		// would otherwise wait a full interval).
		fm.pendingKick = true
		fm.mu.Unlock()
		return
	}
	fm.checking = true
	// -1 sentinel = "checking now" for the UI countdowns (0 keeps its
	// existing meaning of "no channels"). Restored by scheduleNext.
	fm.NextCheckAt = -1
	fm.mu.Unlock()
	if fm.OnSchedule != nil {
		fm.OnSchedule(-1)
	}

	defer func() {
		fm.mu.Lock()
		fm.checking = false
		rerun := fm.pendingKick
		fm.pendingKick = false
		fm.mu.Unlock()
		if rerun {
			// Re-enter via goroutine so the stop-check and offline gate
			// run naturally; that cycle does its own scheduleNext.
			go fm.runCycle(ctx)
			return
		}
		fm.scheduleNext(ctx, cycleStart)
	}()

	fm.refreshProbeCooldown()
	fm.doCheck(ctx)
}

// refreshProbeCooldown hot-reloads the per-video probe cooldown window from
// config before each cycle's probes run, so a config change takes effect on
// the next cycle without a restart (mirrors how the check interval is re-read
// in scheduleNext). Read under configStore.Read to avoid racing a reload.
func (fm *FeedMonitor) refreshProbeCooldown() {
	var d time.Duration
	fm.configStore.Read(func(c *config.MoomboxConfig) {
		d = c.Monitors.ProbeCooldown.AsDuration(time.Second)
	})
	fm.ProbeCooldown.SetDuration(d)
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
			fm.health.recordError(ch.ID, err)
			fm.logger.Warn("feed check failed", "channel", ch.Name, "err", err)
		} else {
			fm.health.recordSuccess(ch.ID)
		}

		// Members-only discovery via the channel's /membership tab (independent
		// of RSS health). RSS never lists members-only content, so this is the
		// only path that catches members live/upcoming streams and, when
		// include_non_live_content is set, their VODs/premieres. No-op unless
		// wired + enabled + auth cookies present.
		fm.checkMembership(ctx, ch)

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

// maxFeedItems is the per-channel cap on how many recent items a discovery
// source considers each cycle: the channel's own MaxFeedItems override, else
// the global monitors.max_feed_items, else defaultMaxFeedItems. Shared by the
// RSS path (truncates the feed) and the membership path (bounds re-probing of
// already-handled videos to the recent window).
func (fm *FeedMonitor) maxFeedItems(ch *config.ChannelConfig) int {
	if ch.MaxFeedItems != nil && *ch.MaxFeedItems > 0 {
		return *ch.MaxFeedItems
	}
	var cfgMax int
	fm.configStore.Read(func(c *config.MoomboxConfig) {
		cfgMax = c.Monitors.MaxFeedItems
	})
	if cfgMax > 0 {
		return cfgMax
	}
	return defaultMaxFeedItems
}

func (fm *FeedMonitor) processFeed(ctx context.Context, ch *config.ChannelConfig, data []byte) error {
	var feed atomFeed
	if err := xml.Unmarshal(data, &feed); err != nil {
		return fmt.Errorf("parse feed: %w", err)
	}

	maxItems := fm.maxFeedItems(ch)

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

		// Description dedup: filter lines that appear in older entries
		description := entry.MediaGroup.Description
		if lookbehind > 0 && i+1 < len(entries) {
			end := min(i+1+lookbehind, len(entries))
			description = filterUniqueDescriptionLinesPrecomputed(description, entryLineSets[i+1:end])
		}

		// Get video URL (feed provides an alternate link; else synthesized in
		// processCandidate).
		videoURL := ""
		for _, link := range entry.Links {
			if link.Rel == "alternate" {
				videoURL = link.Href
				break
			}
		}

		fm.processCandidate(ctx, ch, videoID, entry.Title, description, videoURL, "feed")
	}

	return nil
}

// processCandidate runs a single discovered video through the shared feed
// pipeline: active-job dedup, term matching (title/description), stream-status
// classification via ProbeVideo, and OnVideoFound. Both the RSS path
// (processFeed) and the membership path (checkMembership) funnel through here,
// so dedup, probe, and job-creation semantics stay identical across discovery
// sources — a video seen by two sources can never create two jobs.
//
// source is only a log label ("feed" | "membership"). description may be empty
// (membership entries carry no description, so matching is title-only, like
// DECAPI). videoURL may be empty; a canonical watch URL is synthesized.
func (fm *FeedMonitor) processCandidate(ctx context.Context, ch *config.ChannelConfig, videoID, title, description, videoURL, source string) {
	if videoID == "" {
		return
	}

	// Skip if an active job exists (but not if merely finished — a stream may
	// restart on the same URL).
	active, err := fm.db.HasActiveJob(videoID)
	if err != nil {
		// Don't swallow DB errors — proceeding as if no active job could create
		// duplicates if the DB was simply busy. Skip this entry for this cycle.
		fm.logger.Debug("HasActiveJob query failed", "videoID", videoID, "err", err)
		return
	}
	if active {
		return
	}

	if videoURL == "" {
		videoURL = fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID)
	}

	// Term matching: title OR description can match independently.
	titleMatch := MatchesTerms(title, ch)
	descMatch := description != "" && MatchesTerms(description, ch)
	if !titleMatch && !descMatch {
		return
	}

	// Check if this is a re-probe of a previously processed video. Ignore the
	// error — a DB-busy here is best treated as "not yet processed" so we probe
	// and let upstream dedup (INSERT-OR-IGNORE) handle collisions.
	reprobe, hpErr := fm.db.HasProcessed(videoID)
	if hpErr != nil {
		fm.logger.Debug("HasProcessed query failed", "videoID", videoID, "err", hpErr)
	}

	if reprobe {
		fm.logger.Debug("feed match found (re-probe)",
			"videoID", videoID, "title", title, "channel", ch.Name, "source", source)
	} else {
		fm.logger.Info("feed match found",
			"videoID", videoID, "title", title, "channel", ch.Name, "source", source)
	}

	// Probe video metadata to classify stream status before creating a job.
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
		return
	}

	if fm.OnVideoFound != nil {
		fm.OnVideoFound(videoID, result.Title, videoURL, ch)
	}
}

// checkMembership discovers members-only videos via a channel's authenticated
// /membership tab and routes each through the shared processCandidate pipeline.
// RSS and DECAPI cannot see members-only content, so this is the only source
// for members-only live/upcoming streams (and, when include_non_live_content is
// set, their VODs/premieres).
//
// It is a no-op when FetchMembership is unset or MembershipEnabled gates it off
// (feature disabled or no auth cookies). Fetch failures are logged at Debug and
// never touch RSS feed health — the two are independent signals, so a membership
// hiccup must not mark the channel's RSS feed unhealthy.
//
// Re-probe bounding: a members video NOT yet in history is always processed, so
// new live/upcoming streams — and, with include_non_live_content, VOD backfill —
// are never missed regardless of their position in the tab. A video ALREADY in
// history (previously jobbed or classified) is only re-probed while it sits in
// the recent maxFeedItems window; older handled videos are skipped rather than
// re-probed every cycle forever. This is safe because a jobbed upcoming/live
// stream is polled by the worker (and skipped here via HasActiveJob), not by a
// monitor re-probe, while a recent transiently-failing live stream stays in the
// window and keeps being retried. The window mirrors the RSS path's bounded
// recent view, so both sources scale the same way with monitors.max_feed_items.
func (fm *FeedMonitor) checkMembership(ctx context.Context, ch *config.ChannelConfig) {
	if fm.FetchMembership == nil {
		return
	}
	if fm.MembershipEnabled != nil && !fm.MembershipEnabled() {
		return
	}

	// Bound the whole channel's membership scan the way the RSS path bounds
	// processFeed (checkChannel wraps it in feedProcessTimeout). ProbeVideo can
	// be slow (retries + backoff), and a first enable or large members backlog
	// probes every not-yet-seen video with no count cap; without an aggregate
	// deadline one channel could run past the cycle interval and starve later
	// channels' live-stream discovery. The HTTP fetch keeps its own shorter
	// timeout inside FetchMembership; this cap covers fetch + all probes, and a
	// cutoff just defers the remaining (still-unprocessed) videos to next cycle.
	procCtx, cancel := context.WithTimeout(ctx, feedProcessTimeout)
	defer cancel()

	videos, err := fm.FetchMembership(procCtx, ch.ID)
	if err != nil {
		fm.logger.Debug("membership discovery failed", "channel", ch.Name, "err", err)
		return
	}
	if len(videos) == 0 {
		return
	}

	window := fm.maxFeedItems(ch)
	fm.logger.Debug("membership videos discovered", "channel", ch.Name, "count", len(videos), "reprobeWindow", window)
	for i, v := range videos {
		select {
		case <-procCtx.Done():
			return
		default:
		}
		// Beyond the recent window, skip anything we've already handled so old
		// members VODs aren't re-probed every cycle. New (not-yet-processed)
		// videos still fall through to be probed even when far down the list.
		if i >= window {
			processed, hpErr := fm.db.HasProcessed(v.VideoID)
			if hpErr != nil {
				fm.logger.Debug("HasProcessed query failed (membership window)", "videoID", v.VideoID, "err", hpErr)
			} else if processed {
				continue
			}
		}
		fm.processCandidate(procCtx, ch, v.VideoID, v.Title, "", "", "membership")
	}
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
