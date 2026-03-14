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
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

const (
	feedFetchTimeout    = 15 * time.Second
	feedProcessTimeout  = 60 * time.Second
	defaultMaxFeedItems = 15
)

// monitorHTTPClient is a shared HTTP client with a timeout for monitor HTTP requests.
var monitorHTTPClient = &http.Client{Timeout: 30 * time.Second}

// FeedMonitor polls YouTube RSS feeds for new videos from monitored channels.
type FeedMonitor struct {
	mu          sync.Mutex
	cfg         *config.MoomboxConfig
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
}

// NewFeedMonitor creates a new RSS feed monitor.
func NewFeedMonitor(cfg *config.MoomboxConfig, db *database.Database, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *FeedMonitor {
	return &FeedMonitor{
		cfg:             cfg,
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

	intervalMin := fm.cfg.Monitors.FeedCheckInterval.Value
	if intervalMin <= 0 {
		intervalMin = 10 // default 10 minutes
	}
	interval := time.Duration(intervalMin) * time.Minute
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
		return fmt.Errorf("fetch feed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) // drain for connection reuse
		return fmt.Errorf("feed http %d", resp.StatusCode)
	}

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
	} else if fm.cfg.Monitors.MaxFeedItems > 0 {
		maxItems = fm.cfg.Monitors.MaxFeedItems
	}

	entries := feed.Entries
	if len(entries) > maxItems {
		entries = entries[:maxItems]
	}

	if len(entries) == 0 {
		return nil
	}

	// Track latest video for DECAPI baseline
	fm.db.SetLastVideo(ch.ID, entries[0].VideoID)

	// Build lookbehind set for description dedup
	lookbehind := 0
	if ch.NumDescLookbehind != nil {
		lookbehind = *ch.NumDescLookbehind
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

		// Skip if already processed
		if processed, _ := fm.db.HasProcessed(videoID); processed {
			continue
		}

		// Skip if active job exists
		if active, _ := fm.db.HasActiveJob(videoID); active {
			continue
		}

		// Description dedup: filter lines that appear in older entries
		description := entry.MediaGroup.Description
		if lookbehind > 0 && i+1 < len(entries) {
			end := i + 1 + lookbehind
			if end > len(entries) {
				end = len(entries)
			}
			description = filterUniqueDescriptionLines(description, entries[i+1:end])
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

		fm.logger.Info("feed match found",
			"videoID", videoID,
			"title", title,
			"channel", ch.Name)

		// Probe video metadata to classify stream status before creating job
		result := ProcessYouTubeVideo(ProcessYouTubeVideoParams{
			VideoID:      videoID,
			Title:        title,
			Channel:      ch,
			ProbeVideo:   fm.ProbeVideo,
			AddToHistory: func(id string) error { return fm.db.AddToHistory(id) },
			Tracker:      fm.MetadataTracker,
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

// filterUniqueDescriptionLines removes lines that appear in older entries' descriptions.
func filterUniqueDescriptionLines(description string, olderEntries []atomEntry) string {
	olderLines := make(map[string]struct{})
	for _, older := range olderEntries {
		for _, line := range strings.Split(older.MediaGroup.Description, "\n") {
			olderLines[strings.TrimSpace(line)] = struct{}{}
		}
	}

	var unique []string
	for _, line := range strings.Split(description, "\n") {
		if _, found := olderLines[strings.TrimSpace(line)]; !found {
			unique = append(unique, line)
		}
	}

	return strings.Join(unique, "\n")
}

func (fm *FeedMonitor) getYouTubeChannels() []config.ChannelConfig {
	var channels []config.ChannelConfig
	for _, ch := range fm.cfg.Channels {
		if ch.Enabled != nil && !*ch.Enabled {
			continue
		}
		if ch.Platform == "twitch" {
			continue
		}
		channels = append(channels, ch)
	}
	return channels
}
