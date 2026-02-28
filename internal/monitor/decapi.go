package monitor

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

const (
	decapiRequestTimeout  = 15 * time.Second
	decapiStagger         = 1 * time.Second
	decapiMinInterval     = 15 * time.Second
	decapiDefaultRateLimit = 60
)

var decapiVideoIDRe = regexp.MustCompile(`(?:youtu\.be/|youtube\.com/watch\?v=)([a-zA-Z0-9_-]{11})`)

// rateLimitState tracks DECAPI rate limiting.
type rateLimitState struct {
	limit     int
	remaining int
	resetAt   time.Time
}

// DecapiMonitor polls DECAPI for latest videos from YouTube channels.
type DecapiMonitor struct {
	mu          sync.Mutex
	cfg         *config.MoomboxConfig
	db          *database.Database
	checking    bool
	timer       *time.Timer
	cancel      context.CancelFunc
	rateLimit   rateLimitState
	NextCheckAt int64

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

// NewDecapiMonitor creates a new DECAPI monitor.
func NewDecapiMonitor(cfg *config.MoomboxConfig, db *database.Database, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *DecapiMonitor {
	return &DecapiMonitor{
		cfg: cfg,
		db:  db,
		rateLimit: rateLimitState{
			limit:     decapiDefaultRateLimit,
			remaining: decapiDefaultRateLimit,
		},
		logger:          logger,
		MetadataTracker: NewMetadataFailureTracker(),
	}
}

// Start begins the DECAPI monitoring loop.
func (dm *DecapiMonitor) Start(ctx context.Context) {
	dm.mu.Lock()
	if dm.cancel != nil {
		dm.mu.Unlock()
		return // Already running
	}
	ctx, cancel := context.WithCancel(ctx)
	dm.cancel = cancel
	dm.mu.Unlock()

	dm.logger.Info("decapi monitor started")

	// Immediate first check on startup (runCycle schedules next in its defer)
	go dm.runCycle(ctx)
}

// Stop stops the DECAPI monitor.
func (dm *DecapiMonitor) Stop() {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	if dm.cancel != nil {
		dm.cancel()
		dm.cancel = nil
	}
	if dm.timer != nil {
		dm.timer.Stop()
		dm.timer = nil
	}
	dm.NextCheckAt = 0
	dm.logger.Info("decapi monitor stopped")
}

// GetNextCheckAt returns the next scheduled check time in epoch ms.
func (dm *DecapiMonitor) GetNextCheckAt() int64 {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	return dm.NextCheckAt
}

func (dm *DecapiMonitor) scheduleNext(ctx context.Context) {
	channels := dm.getYouTubeChannels()
	if len(channels) == 0 {
		dm.mu.Lock()
		dm.NextCheckAt = 0
		dm.mu.Unlock()
		if dm.OnSchedule != nil {
			dm.OnSchedule(0)
		}
		return
	}

	interval := dm.calculateInterval(len(channels))

	dm.mu.Lock()
	// Don't schedule if monitor was stopped (cancel set to nil)
	if dm.cancel == nil {
		dm.mu.Unlock()
		return
	}
	dm.NextCheckAt = time.Now().Add(interval).UnixMilli()
	if dm.timer != nil {
		dm.timer.Stop()
	}
	dm.timer = time.AfterFunc(interval, func() {
		dm.runCycle(ctx)
	})
	dm.mu.Unlock()

	if dm.OnSchedule != nil {
		dm.OnSchedule(dm.NextCheckAt)
	}

	dm.logger.Debug("decapi check scheduled", "in", interval.Round(time.Second))
}

func (dm *DecapiMonitor) calculateInterval(channelCount int) time.Duration {
	// Dynamic: ceil(channels / ratePerMinute * 60) seconds
	dm.mu.Lock()
	ratePerMin := dm.rateLimit.limit
	dm.mu.Unlock()

	if ratePerMin <= 0 {
		ratePerMin = decapiDefaultRateLimit
	}

	dynamicSec := math.Ceil(float64(channelCount) / float64(ratePerMin) * 60)
	interval := time.Duration(dynamicSec) * time.Second

	// Config override
	if dm.cfg.Monitors.DecapiCheckInterval != nil && *dm.cfg.Monitors.DecapiCheckInterval >= 15 {
		interval = time.Duration(*dm.cfg.Monitors.DecapiCheckInterval) * time.Second
	}

	// Floor
	if interval < decapiMinInterval {
		interval = decapiMinInterval
	}

	return interval
}

func (dm *DecapiMonitor) runCycle(ctx context.Context) {
	dm.mu.Lock()
	if dm.checking {
		dm.mu.Unlock()
		return
	}
	dm.checking = true
	dm.mu.Unlock()

	defer func() {
		dm.mu.Lock()
		dm.checking = false
		dm.mu.Unlock()
		dm.scheduleNext(ctx)
	}()

	dm.doCheck(ctx)
}

func (dm *DecapiMonitor) doCheck(ctx context.Context) {
	channels := dm.getYouTubeChannels()
	if len(channels) == 0 {
		return
	}

	dm.logger.Debug("decapi checking", "channels", len(channels))

	for i := range channels {
		select {
		case <-ctx.Done():
			return
		default:
		}

		ch := &channels[i]
		dm.waitForRateLimit(ctx)

		if err := dm.checkChannel(ctx, ch); err != nil {
			dm.logger.Debug("decapi check failed", "channel", ch.Name, "err", err)
		}

		// Stagger between requests
		if i < len(channels)-1 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(decapiStagger):
			}
		}
	}
}

func (dm *DecapiMonitor) waitForRateLimit(ctx context.Context) {
	dm.mu.Lock()
	rl := &dm.rateLimit

	// Proactive reset if window expired
	if !rl.resetAt.IsZero() && time.Now().After(rl.resetAt) {
		rl.remaining = rl.limit
		rl.resetAt = time.Time{}
	}

	if rl.remaining > 0 {
		dm.mu.Unlock()
		return
	}

	// Need to wait
	waitDur := time.Until(rl.resetAt)
	dm.mu.Unlock()

	if waitDur > 0 {
		dm.logger.Debug("waiting for rate limit", "wait", waitDur.Round(time.Second))
		select {
		case <-ctx.Done():
			return
		case <-time.After(waitDur):
		}

		dm.mu.Lock()
		rl.remaining = rl.limit
		rl.resetAt = time.Time{}
		dm.mu.Unlock()
	}
}

func (dm *DecapiMonitor) checkChannel(ctx context.Context, ch *config.ChannelConfig) error {
	url := fmt.Sprintf("https://decapi.me/youtube/latest_video?id=%s", ch.ID)

	ctx, cancel := context.WithTimeout(ctx, decapiRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Moombox/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("decapi request: %w", err)
	}
	defer resp.Body.Close()

	// Start a 1-minute rate limit window if none is active, and decrement
	// remaining count after response (matches TS fetchDecapi post-fetch).
	dm.mu.Lock()
	if dm.rateLimit.resetAt.IsZero() {
		dm.rateLimit.resetAt = time.Now().Add(60 * time.Second)
	}
	if dm.rateLimit.remaining > 0 {
		dm.rateLimit.remaining--
	}
	dm.mu.Unlock()

	// Always update rate limit from headers (server headers override)
	dm.updateRateLimit(resp)

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := resp.Header.Get("Retry-After")
		if secs, err := strconv.Atoi(retryAfter); err == nil {
			dm.mu.Lock()
			dm.rateLimit.resetAt = time.Now().Add(time.Duration(secs) * time.Second)
			dm.rateLimit.remaining = 0
			dm.mu.Unlock()
		}
		return fmt.Errorf("rate limited (429)")
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("decapi http %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	return dm.processResponse(string(body), ch)
}

func (dm *DecapiMonitor) updateRateLimit(resp *http.Response) {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	if v := resp.Header.Get("X-RateLimit-Limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			dm.rateLimit.limit = n
		}
	}
	if v := resp.Header.Get("X-RateLimit-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			dm.rateLimit.remaining = n
		}
	}
	if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			// Could be epoch seconds or relative seconds
			if n > 1_000_000_000 {
				dm.rateLimit.resetAt = time.Unix(n, 0)
			} else {
				dm.rateLimit.resetAt = time.Now().Add(time.Duration(n) * time.Second)
			}
		}
	}
}

func (dm *DecapiMonitor) processResponse(body string, ch *config.ChannelConfig) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}

	// Parse video ID
	matches := decapiVideoIDRe.FindStringSubmatch(body)
	if matches == nil {
		return nil // No video ID found
	}
	videoID := matches[1]

	// Parse title (everything before the URL)
	title := body
	if idx := strings.LastIndex(body, " - https://"); idx >= 0 {
		title = body[:idx]
	}

	// Compare with last known video
	lastVideo, _ := dm.db.GetLastVideo(ch.ID)
	if videoID == lastVideo {
		return nil // No change
	}

	// Update last video
	dm.db.SetLastVideo(ch.ID, videoID)

	// Check if already processed
	if processed, _ := dm.db.HasProcessed(videoID); processed {
		return nil
	}

	// Check if active job exists
	if active, _ := dm.db.HasActiveJob(videoID); active {
		return nil
	}

	// Term matching (title only — no description from DECAPI)
	if !MatchesTerms(title, ch) {
		return nil
	}

	videoURL := fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID)

	dm.logger.Info("decapi match found",
		"videoID", videoID,
		"title", title,
		"channel", ch.Name)

	// Probe video metadata to classify stream status before creating job
	result := ProcessYouTubeVideo(ProcessYouTubeVideoParams{
		VideoID:      videoID,
		Title:        title,
		Channel:      ch,
		ProbeVideo:   dm.ProbeVideo,
		AddToHistory: func(id string) error { return dm.db.AddToHistory(id) },
		Tracker:      dm.MetadataTracker,
		Logger:       dm.logger,
	})
	if !result.ShouldProcess {
		return nil
	}

	if dm.OnVideoFound != nil {
		dm.OnVideoFound(videoID, result.Title, videoURL, ch)
	}

	return nil
}

func (dm *DecapiMonitor) getYouTubeChannels() []config.ChannelConfig {
	var channels []config.ChannelConfig
	for _, ch := range dm.cfg.Channels {
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
