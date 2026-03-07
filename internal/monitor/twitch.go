package monitor

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

const (
	twitchDefaultInterval = 15 * time.Second
	twitchStagger         = 500 * time.Millisecond
)

// TwitchMonitor polls Twitch GQL for live streams from monitored channels.
type TwitchMonitor struct {
	mu          sync.Mutex
	cfg         *config.MoomboxConfig
	db          *database.Database
	tw          *twitch.Service
	checking    bool
	timer       *time.Timer
	ctx         context.Context
	cancel      context.CancelFunc
	NextCheckAt int64

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	OnSchedule    func(nextCheckAt int64)
	OnStreamFound func(info *twitch.TwitchStreamInfo, channel *config.ChannelConfig)
}

// NewTwitchMonitor creates a new Twitch monitor.
func NewTwitchMonitor(cfg *config.MoomboxConfig, db *database.Database, tw *twitch.Service, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *TwitchMonitor {
	return &TwitchMonitor{
		cfg:    cfg,
		db:     db,
		tw:     tw,
		logger: logger,
	}
}

// Start begins the Twitch monitoring loop.
func (tm *TwitchMonitor) Start(ctx context.Context) {
	tm.mu.Lock()
	if tm.cancel != nil {
		tm.mu.Unlock()
		return // Already running
	}
	ctx, cancel := context.WithCancel(ctx)
	tm.ctx = ctx
	tm.cancel = cancel
	tm.mu.Unlock()

	tm.logger.Info("twitch monitor started")

	// Immediate first check on startup (runCycle schedules next in its defer)
	go tm.runCycle(ctx)
}

// Stop stops the Twitch monitor.
func (tm *TwitchMonitor) Stop() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.cancel != nil {
		tm.cancel()
		tm.ctx = nil
		tm.cancel = nil
	}
	if tm.timer != nil {
		tm.timer.Stop()
		tm.timer = nil
	}
	tm.NextCheckAt = 0
	tm.logger.Info("twitch monitor stopped")
}

// GetNextCheckAt returns the next scheduled check time in epoch ms.
func (tm *TwitchMonitor) GetNextCheckAt() int64 {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.NextCheckAt
}

// CheckNow triggers an immediate check cycle if the monitor is running.
// Used to wake the monitor when channels are added while it was idle.
func (tm *TwitchMonitor) CheckNow() {
	tm.mu.Lock()
	if tm.cancel == nil {
		tm.mu.Unlock()
		return // Not running
	}
	ctx := tm.ctx
	tm.mu.Unlock()
	go tm.runCycle(ctx)
}

func (tm *TwitchMonitor) scheduleNext(ctx context.Context) {
	channels := tm.getTwitchChannels()
	if len(channels) == 0 {
		tm.mu.Lock()
		tm.NextCheckAt = 0
		tm.mu.Unlock()
		if tm.OnSchedule != nil {
			tm.OnSchedule(0)
		}
		return
	}

	interval := tm.calculateInterval()

	tm.mu.Lock()
	// Don't schedule if monitor was stopped (cancel set to nil)
	if tm.cancel == nil {
		tm.mu.Unlock()
		return
	}
	tm.NextCheckAt = time.Now().Add(interval).UnixMilli()
	if tm.timer != nil {
		tm.timer.Stop()
	}
	tm.timer = time.AfterFunc(interval, func() {
		tm.runCycle(ctx)
	})
	next := tm.NextCheckAt
	tm.mu.Unlock()

	if tm.OnSchedule != nil {
		tm.OnSchedule(next)
	}

	tm.logger.Debug("twitch check scheduled", "in", interval.Round(time.Second))
}

func (tm *TwitchMonitor) calculateInterval() time.Duration {
	base := twitchDefaultInterval

	if tm.cfg.Monitors.TwitchCheckInterval != nil && *tm.cfg.Monitors.TwitchCheckInterval > 0 {
		base = time.Duration(*tm.cfg.Monitors.TwitchCheckInterval) * time.Second
	}

	// Apply ±10% jitter (scales with interval)
	tenPercent := int64(base) / 10
	if tenPercent > 0 {
		base = base - time.Duration(tenPercent) + time.Duration(rand.Int63n(2*tenPercent))
	}

	return base
}

func (tm *TwitchMonitor) runCycle(ctx context.Context) {
	tm.mu.Lock()
	if tm.checking {
		tm.mu.Unlock()
		return
	}
	tm.checking = true
	tm.mu.Unlock()

	defer func() {
		tm.mu.Lock()
		tm.checking = false
		tm.mu.Unlock()
		tm.scheduleNext(ctx)
	}()

	tm.doCheck(ctx)
}

func (tm *TwitchMonitor) doCheck(ctx context.Context) {
	channels := tm.getTwitchChannels()
	if len(channels) == 0 {
		return
	}

	tm.logger.Debug("twitch checking", "channels", len(channels))

	for i := range channels {
		select {
		case <-ctx.Done():
			return
		default:
		}

		ch := &channels[i]
		if err := tm.checkChannel(ctx, ch); err != nil {
			tm.logger.Debug("twitch check failed", "channel", ch.Name, "err", err)
		}

		// Stagger between requests
		if i < len(channels)-1 {
			staggerTimer := time.NewTimer(twitchStagger)
			select {
			case <-ctx.Done():
				staggerTimer.Stop()
				return
			case <-staggerTimer.C:
			}
		}
	}
}

func (tm *TwitchMonitor) checkChannel(ctx context.Context, ch *config.ChannelConfig) error {
	info, err := tm.tw.GetStreamInfo(ctx, ch.ID)
	if err != nil {
		return fmt.Errorf("get stream info: %w", err)
	}

	if info == nil {
		return nil // Channel offline
	}

	// Dedup by stream ID
	jobID := twitch.BuildJobID(info.StreamID, false)
	if processed, _ := tm.db.HasProcessed(jobID); processed {
		return nil
	}
	if active, _ := tm.db.HasActiveJob(jobID); active {
		return nil
	}

	// Term matching: title OR category can match (checked independently)
	titleMatch := MatchesTerms(info.Title, ch)
	categoryMatch := false
	if info.GameCategory != "" {
		categoryMatch = MatchesTerms(info.GameCategory, ch)
	}
	if !titleMatch && !categoryMatch {
		return nil
	}

	tm.logger.Info("twitch stream found",
		"channel", info.ChannelDisplayName,
		"title", info.Title,
		"streamID", info.StreamID)

	if tm.OnStreamFound != nil {
		tm.OnStreamFound(info, ch)
	}

	return nil
}

func (tm *TwitchMonitor) getTwitchChannels() []config.ChannelConfig {
	var channels []config.ChannelConfig
	for _, ch := range tm.cfg.Channels {
		if ch.Enabled != nil && !*ch.Enabled {
			continue
		}
		if ch.Platform != "twitch" {
			continue
		}
		channels = append(channels, ch)
	}
	return channels
}
