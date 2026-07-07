package monitor

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

const (
	decapiRequestTimeout   = 15 * time.Second
	decapiStagger          = 1 * time.Second
	decapiMinInterval      = 15 * time.Second
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
	rateLimit   rateLimitState
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
}

// Health returns the per-channel health snapshot for /api/status.
func (dm *DecapiMonitor) Health() []ChannelHealth { return dm.health.snapshot() }

// PruneHealth drops health entries for channels no longer configured.
func (dm *DecapiMonitor) PruneHealth() {
	active := make(map[string]struct{})
	for _, ch := range dm.getYouTubeChannels() {
		active[ch.ID] = struct{}{}
	}
	dm.health.prune(active)
}

// SetOnChannelUnhealthy installs the callback fired when a channel crosses
// the consecutive-failure threshold.
func (dm *DecapiMonitor) SetOnChannelUnhealthy(fn func(channelID string, consecutive int, lastErr string)) {
	dm.health.onUnhealthy = fn
}

// NewDecapiMonitor creates a new DECAPI monitor. The Store carries the
// cfg+lock used to read channel list and interval settings; all reads
// happen under configStore.Read so a config-reload doesn't race against
// an in-flight cycle (audit reports/monitor.md Critical Issue #1).
func NewDecapiMonitor(store *config.Store, db *database.Database, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *DecapiMonitor {
	return &DecapiMonitor{
		configStore: store,
		db:          db,
		rateLimit: rateLimitState{
			limit:     decapiDefaultRateLimit,
			remaining: decapiDefaultRateLimit,
		},
		logger:          logger,
		health:          newHealthTracker(),
		MetadataTracker: NewMetadataFailureTracker(),
		// Window is set from config at the start of every cycle via
		// refreshProbeCooldown (before any probe runs), so the zero here just
		// means "disabled until the first cycle reads config".
		ProbeCooldown: NewProbeCooldown(0),
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
	dm.ctx = ctx
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
		dm.ctx = nil
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

// CheckNow triggers an immediate check cycle if the monitor is running.
// Used to wake the monitor when channels are added while it was idle.
func (dm *DecapiMonitor) CheckNow() {
	dm.mu.Lock()
	if dm.cancel == nil {
		dm.mu.Unlock()
		return // Not running
	}
	ctx := dm.ctx
	dm.mu.Unlock()
	go dm.runCycle(ctx)
}

// scheduleNext arms the next cycle. cycleStart anchors fixed-RATE
// scheduling: the delay is interval minus the elapsed cycle time, so the
// rate-derived interval is a true period. The per-request stagger and
// waitForRateLimit inside doCheck still bound instantaneous request rate,
// so this cannot exceed the DECAPI budget — calculateInterval already
// targets it and the previous gap-scheduling ran BELOW the budget.
// Zero-value cycleStart behaves as a plain interval.
func (dm *DecapiMonitor) scheduleNext(ctx context.Context, cycleStart time.Time) {
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

	delay := interval
	if !cycleStart.IsZero() {
		elapsed := time.Since(cycleStart)
		delay = interval - elapsed
		if delay < time.Second {
			// DECAPI's rate-derived interval is structurally smaller than
			// the mandatory per-request stagger floor once channel count
			// approaches the per-minute limit, so a normal full-channel
			// cycle exceeds `interval` by design — waitForRateLimit still
			// bounds the actual request rate. Warn only on a >2× overshoot
			// (something genuinely slow), once, via the atomic guard.
			if elapsed >= 2*interval && dm.warnedSlow.CompareAndSwap(false, true) {
				dm.logger.Warn("decapi check cycle takes far longer than the rate-derived interval — effective cadence degraded",
					"cycle", elapsed.Round(time.Second), "interval", interval.Round(time.Second))
			}
			delay = time.Second
		}
	}

	dm.mu.Lock()
	// Don't schedule if monitor was stopped; clear the checking sentinel so
	// a stopped monitor never reports -1 forever.
	if dm.cancel == nil {
		dm.NextCheckAt = 0
		dm.mu.Unlock()
		return
	}
	dm.NextCheckAt = time.Now().Add(delay).UnixMilli()
	if dm.timer != nil {
		dm.timer.Stop()
	}
	dm.timer = time.AfterFunc(delay, func() {
		dm.runCycle(ctx)
	})
	next := dm.NextCheckAt
	dm.mu.Unlock()

	if dm.OnSchedule != nil {
		dm.OnSchedule(next)
	}

	dm.logger.Debug("decapi check scheduled", "in", delay.Round(time.Second))
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
	var cfgInterval *int
	dm.configStore.Read(func(c *config.MoomboxConfig) {
		cfgInterval = c.Monitors.DecapiCheckInterval
	})
	if cfgInterval != nil && *cfgInterval >= 15 {
		interval = time.Duration(*cfgInterval) * time.Second
	}

	// Floor
	interval = max(interval, decapiMinInterval)

	// Add ±10% jitter so multiple Moombox instances don't synchronize polls
	// against decapi.me. Feed and Twitch monitors already jitter.
	tenPercent := int64(interval) / 10
	if tenPercent > 0 {
		interval = interval - time.Duration(tenPercent) + time.Duration(rand.Int63n(2*tenPercent))
	}

	return interval
}

func (dm *DecapiMonitor) runCycle(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			dm.logger.Error("decapi monitor runCycle panic", "panic", r)
		}
	}()

	cycleStart := time.Now()
	dm.mu.Lock()
	if dm.checking {
		// A kick landed mid-cycle: latch it so the defer re-runs
		// immediately instead of silently dropping it (the new channel
		// would otherwise wait a full interval).
		dm.pendingKick = true
		dm.mu.Unlock()
		return
	}
	dm.checking = true
	// -1 sentinel = "checking now" for the UI countdowns (0 keeps its
	// existing meaning of "no channels"). Restored by scheduleNext.
	dm.NextCheckAt = -1
	dm.mu.Unlock()
	if dm.OnSchedule != nil {
		dm.OnSchedule(-1)
	}

	defer func() {
		dm.mu.Lock()
		dm.checking = false
		rerun := dm.pendingKick
		dm.pendingKick = false
		dm.mu.Unlock()
		if rerun {
			// Re-enter via goroutine so the stop-check and offline gate
			// run naturally; that cycle does its own scheduleNext.
			go dm.runCycle(ctx)
			return
		}
		dm.scheduleNext(ctx, cycleStart)
	}()

	dm.refreshProbeCooldown()
	dm.doCheck(ctx)
}

// refreshProbeCooldown hot-reloads the per-video probe cooldown window from
// config before each cycle's probes run, so a config change takes effect on
// the next cycle without a restart (mirrors how the check interval is re-read
// in calculateInterval). Read under configStore.Read to avoid racing a reload.
func (dm *DecapiMonitor) refreshProbeCooldown() {
	var d time.Duration
	dm.configStore.Read(func(c *config.MoomboxConfig) {
		d = c.Monitors.ProbeCooldown.AsDuration(time.Second)
	})
	dm.ProbeCooldown.SetDuration(d)
}

func (dm *DecapiMonitor) doCheck(ctx context.Context) {
	if dm.IsOnline != nil && !dm.IsOnline() {
		dm.logger.Debug("skipping DECAPI poll — offline")
		return
	}
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
			dm.health.recordError(ch.ID, err)
			dm.logger.Debug("decapi check failed", "channel", ch.Name, "err", err)
		} else {
			dm.health.recordSuccess(ch.ID)
		}

		// Stagger between requests
		if i < len(channels)-1 {
			staggerTimer := time.NewTimer(decapiStagger)
			select {
			case <-ctx.Done():
				staggerTimer.Stop()
				return
			case <-staggerTimer.C:
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

	// Defensive: if remaining==0 but resetAt is zero, we lack a server-provided
	// window end (e.g. headers included X-RateLimit-Remaining: 0 without a
	// matching X-RateLimit-Reset). Without this, time.Until(zero) returns a
	// large negative duration, the waitDur>0 guard skips the wait, and we
	// busy-loop. Treat missing resetAt as a fresh 60s window matching the
	// default rate-limit cadence established in checkChannel.
	if rl.resetAt.IsZero() {
		rl.resetAt = time.Now().Add(60 * time.Second)
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

	resp, err := monitorHTTPClient.Do(req)
	if err != nil {
		// Transport-level failure — feeds the passive offline tracker.
		reportMonitorResult("monitor/decapi", true)
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
		// 429 reached the server — explicit throttle, not a connectivity
		// problem. Don't report as failure or success.
		retryAfter := resp.Header.Get("Retry-After")
		if secs, err := strconv.Atoi(retryAfter); err == nil {
			dm.mu.Lock()
			dm.rateLimit.resetAt = time.Now().Add(time.Duration(secs) * time.Second)
			dm.rateLimit.remaining = 0
			dm.mu.Unlock()
		}
		// Drain so the connection can be reused (closing an unread body
		// discards the TCP connection — costly during a sustained 429 storm).
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("rate limited (429)")
	}

	if resp.StatusCode != http.StatusOK {
		// Non-2xx — server reachable but unhappy; leave tracker alone.
		// Drain a bounded amount before close to keep the connection reusable.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("decapi http %d", resp.StatusCode)
	}
	reportMonitorResult("monitor/decapi", false)

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB limit
	if err != nil {
		return err
	}

	return dm.processResponse(ctx, string(body), ch)
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
			var newReset time.Time
			if n > 1_000_000_000 {
				newReset = time.Unix(n, 0)
			} else {
				newReset = time.Now().Add(time.Duration(n) * time.Second)
			}

			// Sanity: reject resetAt values already in the past (server clock
			// skew or a stale cached header). A past resetAt causes
			// waitForRateLimit's proactive-reset branch to fire immediately,
			// handing out a fresh burst of requests and defeating backoff.
			// Also reject values that would shorten an existing future
			// resetAt — back-off should never retreat.
			if !newReset.After(time.Now()) {
				// past or now — ignore
			} else if !dm.rateLimit.resetAt.IsZero() && newReset.Before(dm.rateLimit.resetAt) {
				// would shorten existing backoff — keep the longer window
			} else {
				dm.rateLimit.resetAt = newReset
			}
		}
	}
}

func (dm *DecapiMonitor) processResponse(ctx context.Context, body string, ch *config.ChannelConfig) error {
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

	// Skip if active job exists (but not if merely finished — stream may restart on same URL)
	active, err := dm.db.HasActiveJob(videoID)
	if err != nil {
		// Don't swallow DB errors — proceeding as if no active job could
		// create duplicates if the DB was simply busy. Log and abort this
		// entry for the current cycle.
		dm.logger.Debug("HasActiveJob query failed", "videoID", videoID, "err", err)
		return nil
	}
	if active {
		return nil
	}

	// Term matching (title only — no description from DECAPI)
	if !MatchesTerms(title, ch) {
		return nil
	}

	videoURL := fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID)

	// Check if this is a re-probe of a previously processed video. A DB-busy
	// here is best treated as "not yet processed" so we probe and let upstream
	// dedup (INSERT-OR-IGNORE) handle collisions. We still log for visibility.
	reprobe, hpErr := dm.db.HasProcessed(videoID)
	if hpErr != nil {
		dm.logger.Debug("HasProcessed query failed", "videoID", videoID, "err", hpErr)
	}

	if reprobe {
		dm.logger.Debug("decapi match found (re-probe)",
			"videoID", videoID,
			"title", title,
			"channel", ch.Name)
	} else {
		dm.logger.Info("decapi match found",
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
		ProbeVideo:   dm.ProbeVideo,
		AddToHistory: func(id string) error { return dm.db.AddToHistory(id) },
		Tracker:      dm.MetadataTracker,
		Cooldown:     dm.ProbeCooldown,
		IsReprobe:    reprobe,
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

// getYouTubeChannels returns a copy of the YouTube channel list under
// configStore.Read so the polling loop iterates freely without holding
// the lock across network calls. Audit reports/monitor.md Critical Issue #1.
func (dm *DecapiMonitor) getYouTubeChannels() []config.ChannelConfig {
	var channels []config.ChannelConfig
	dm.configStore.Read(func(c *config.MoomboxConfig) {
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
