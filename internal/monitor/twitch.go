package monitor

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

const (
	twitchDefaultInterval = 15 * time.Second
)

// twitchStagger spaces consecutive GQL batch requests. Package var so tests
// can shrink it (the pattern discordRetryBackoff uses in internal/notifications).
var twitchStagger = 500 * time.Millisecond

// StreamInfoBatchFunc fetches live-stream info for one chunk of Twitch logins.
// Typically wired to twitch.Service.GetStreamInfoBatch; tests inject a fake via
// TwitchMonitor.FetchBatch — the seam FeedMonitor.FetchRSS is modelled on.
type StreamInfoBatchFunc func(ctx context.Context, logins []string) (infos []*twitch.TwitchStreamInfo, errs []error, wholeErr error)

// TwitchMonitor polls Twitch GQL for live streams from monitored channels.
type TwitchMonitor struct {
	mu          sync.Mutex
	configStore *config.Store
	db          *database.Database
	tw          *twitch.Service
	checking    bool
	// pendingKick latches a CheckNow that landed while a cycle was in
	// flight — previously silently dropped, leaving a just-added channel
	// unpolled for a full interval. Consumed in runCycle's defer.
	pendingKick bool
	// warnedSlow rate-limits the oversubscribed warning; atomic because
	// scheduleNext touches it outside the monitor mutex.
	warnedSlow atomic.Bool
	// batchFailStreak counts consecutive whole-batch GQL failures. They are
	// deliberately kept off every channel's health streak (see checkChunk),
	// which left a persistent one — Twitch refusing the client — visible only
	// at Debug while no Twitch channel was being checked at all.
	batchFailStreak atomic.Int32
	timer           *time.Timer
	ctx             context.Context
	cancel          context.CancelFunc
	NextCheckAt     int64

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	health *healthTracker

	OnSchedule      func(nextCheckAt int64)
	OnStreamFound   func(info *twitch.TwitchStreamInfo, channel *config.ChannelConfig)
	OnStreamRecover func(info *twitch.TwitchStreamInfo, channel *config.ChannelConfig, jobID string)
	IsOnline        func() bool // nil = always online

	// FetchBatch overrides the GQL batch call (tm.tw.GetStreamInfoBatch) for
	// tests. Nil uses the real client — see streamInfoBatch.
	FetchBatch StreamInfoBatchFunc
}

// Health returns the per-channel health snapshot for /api/status.
func (tm *TwitchMonitor) Health() []ChannelHealth { return tm.health.snapshot() }

// PruneHealth drops health entries for channels no longer configured.
func (tm *TwitchMonitor) PruneHealth() {
	active := make(map[string]struct{})
	for _, ch := range tm.getTwitchChannels() {
		active[ch.ID] = struct{}{}
	}
	tm.health.prune(active)
}

// SetOnChannelUnhealthy installs the callback fired when a channel crosses
// the consecutive-failure threshold.
func (tm *TwitchMonitor) SetOnChannelUnhealthy(fn func(channelID string, consecutive int, lastErr string)) {
	tm.health.onUnhealthy = fn
}

// SetOnChannelHealthy installs the callback fired once when a channel that
// crossed the threshold answers a check again.
func (tm *TwitchMonitor) SetOnChannelHealthy(fn func(channelID string)) {
	tm.health.onHealthy = fn
}

// NewTwitchMonitor creates a new Twitch monitor. The Store carries the
// cfg+lock used to read channel list and interval settings; all reads
// happen under configStore.Read so a config-reload doesn't race against
// an in-flight cycle (audit reports/monitor.md Critical Issue #1).
func NewTwitchMonitor(store *config.Store, db *database.Database, tw *twitch.Service, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *TwitchMonitor {
	return &TwitchMonitor{
		configStore: store,
		db:          db,
		tw:          tw,
		logger:      logger,
		health:      newHealthTracker(),
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

// scheduleNext arms the next cycle. cycleStart anchors fixed-RATE
// scheduling: the delay is interval minus the elapsed cycle time, so the
// configured interval is a true period — previously it was a GAP after
// each cycle, silently inflating detection latency by the cycle duration
// (~0.5s×N channels every cycle, permanently). A zero-value cycleStart
// behaves as a plain interval.
func (tm *TwitchMonitor) scheduleNext(ctx context.Context, cycleStart time.Time) {
	// Same rule as runCycle's guard, for the path that arms the timer: a
	// cancelled context means this chain was retired by Stop(), and a retired
	// chain must arm nothing and publish nothing. Checked FIRST, above the
	// channel-count read, because the "no channels" arm below writes
	// NextCheckAt and publishes OnSchedule too — the two things this guard
	// exists to prevent — so a guard placed after it covers only half the
	// function. Deliberately WITHOUT touching NextCheckAt: the countdown
	// belongs to whichever chain is live now, and a dead chain zeroing it
	// would blank the UI's next-check time for no reason. A twin of this check
	// sits inside the locked section below and covers what this one cannot —
	// a Stop()+Start() landing after it; both are wanted.
	if ctx.Err() != nil {
		return
	}

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
	delay := interval
	if !cycleStart.IsZero() {
		elapsed := time.Since(cycleStart)
		delay = interval - elapsed
		if delay < time.Second {
			// Keep a 1s idle floor — the per-request stagger already bounds
			// instantaneous rate. Warn only when the cycle ran WELL past the
			// interval (>2×), so an inherently-staggered full-channel cycle
			// (whose stagger floor structurally exceeds the interval) doesn't
			// read as a degradation. Once, via the atomic guard.
			if elapsed >= 2*interval && tm.warnedSlow.CompareAndSwap(false, true) {
				tm.logger.Warn("twitch check cycle takes far longer than the configured interval — effective cadence degraded",
					"cycle", elapsed.Round(time.Second), "interval", interval.Round(time.Second))
			}
			delay = time.Second
		}
	}

	tm.mu.Lock()
	// Unreachable since the leading ctx.Err() guard — Stop() cancels the ctx
	// and nils cancel together, and Stop() itself writes NextCheckAt = 0 — so
	// this no longer clears the -1 sentinel for a late cycle racing Stop().
	// Kept as defence for a future cancel-without-cancel path: a monitor that
	// nils cancel without cancelling its context would otherwise arm a timer
	// and publish a countdown for a chain nothing owns.
	if tm.cancel == nil {
		tm.NextCheckAt = 0
		tm.mu.Unlock()
		return
	}
	// Kept beside the leading guard, not redundant with it: this closes the
	// window between that check and this lock, in which a Stop()+Start() would
	// install a NEW non-nil cancel — so `cancel == nil` above passes and this
	// dead chain would write the LIVE chain's NextCheckAt and publish its
	// OnSchedule. Re-reading ctx.Err() costs nothing; do not "simplify" it away.
	if ctx.Err() != nil {
		tm.mu.Unlock()
		return
	}
	tm.NextCheckAt = time.Now().Add(delay).UnixMilli()
	if tm.timer != nil {
		tm.timer.Stop()
	}
	tm.timer = time.AfterFunc(delay, func() {
		tm.runCycle(ctx)
	})
	next := tm.NextCheckAt
	tm.mu.Unlock()

	if tm.OnSchedule != nil {
		tm.OnSchedule(next)
	}

	tm.logger.Debug("twitch check scheduled", "in", delay.Round(time.Second))
}

func (tm *TwitchMonitor) calculateInterval() time.Duration {
	base := twitchDefaultInterval

	var cfgInterval *int
	tm.configStore.Read(func(c *config.MoomboxConfig) {
		cfgInterval = c.Monitors.TwitchCheckInterval
	})
	if cfgInterval != nil && *cfgInterval > 0 {
		base = time.Duration(*cfgInterval) * time.Second
	}

	// Apply ±10% jitter (scales with interval)
	tenPercent := int64(base) / 10
	if tenPercent > 0 {
		base = base - time.Duration(tenPercent) + time.Duration(rand.Int63n(2*tenPercent))
	}

	return base
}

func (tm *TwitchMonitor) runCycle(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			tm.logger.Error("twitch monitor runCycle panic", "panic", r)
		}
	}()

	// A cancelled context means this cycle belongs to a STOPPED chain. Stop()
	// cancels the context but leaves the AfterFunc armed, and a later Start()
	// installs a new cancel — so the dead chain's cycle used to pass every
	// guard, run a full doCheck, and then re-arm the SHARED timer field,
	// cancelling the live chain's pending cycle every interval. Returning here,
	// before the `checking` latch and before the scheduleNext defer is
	// installed, is what stops that. Latent today (Stop runs only at shutdown).
	if ctx.Err() != nil {
		return
	}

	cycleStart := time.Now()
	tm.mu.Lock()
	if tm.checking {
		// A kick (channel add, reconnect, operator force-check) landed
		// mid-cycle: latch it so the defer re-runs immediately instead of
		// silently dropping it (the new channel would otherwise wait a
		// full interval).
		tm.pendingKick = true
		tm.mu.Unlock()
		return
	}
	tm.checking = true
	// -1 sentinel = "checking now" for the UI countdowns (0 keeps its
	// existing meaning of "no channels"). Restored by scheduleNext.
	tm.NextCheckAt = -1
	tm.mu.Unlock()
	if tm.OnSchedule != nil {
		tm.OnSchedule(-1)
	}

	defer func() {
		tm.mu.Lock()
		tm.checking = false
		rerun := tm.pendingKick
		tm.pendingKick = false
		tm.mu.Unlock()
		if rerun {
			// Re-enter via goroutine so the stop-check and offline gate
			// run naturally; that cycle does its own scheduleNext.
			go tm.runCycle(ctx)
			return
		}
		tm.scheduleNext(ctx, cycleStart)
	}()

	tm.doCheck(ctx)
}

// twitchBatchChunk bounds how many channels ride in one GQL request (2 ops
// each). Generous — Twitch GQL has no per-request op limit documented, but
// chunking caps payload size and keeps one giant channel list from a single
// oversized request; chunks are staggered like the old per-channel loop.
const twitchBatchChunk = 30

func (tm *TwitchMonitor) doCheck(ctx context.Context) {
	if tm.IsOnline != nil && !tm.IsOnline() {
		tm.logger.Debug("skipping Twitch poll — offline")
		return
	}
	channels := tm.getTwitchChannels()
	if len(channels) == 0 {
		return
	}

	tm.logger.Debug("twitch checking", "channels", len(channels))

	// Batch: one GQL request per chunk instead of one per channel. An
	// N-channel cycle collapses from N serialized round-trips (~0.8s each)
	// to ceil(N/chunk) — every second shaved is video caught at stream
	// start (Twitch HLS has no backfill).
	for start := 0; start < len(channels); start += twitchBatchChunk {
		select {
		case <-ctx.Done():
			return
		default:
		}
		end := min(start+twitchBatchChunk, len(channels))
		tm.checkChunk(ctx, channels[start:end])

		// Stagger between chunks (not between every channel any more). This
		// runs after EVERY chunk, including one whose whole-batch request
		// failed: a GQL 429 or 5xx is exactly when pacing matters, and the old
		// `continue` skipped it — with >30 channels every remaining chunk fired
		// back to back into a throttled API.
		if end < len(channels) {
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

// streamInfoBatch is the injectable GQL batch seam: FetchBatch when a test has
// wired one, else the real client. Mirrors FeedMonitor.rssFetch.
func (tm *TwitchMonitor) streamInfoBatch(ctx context.Context, logins []string) ([]*twitch.TwitchStreamInfo, []error, error) {
	if tm.FetchBatch != nil {
		return tm.FetchBatch(ctx, logins)
	}
	return tm.tw.GetStreamInfoBatch(ctx, logins)
}

// checkChunk runs one batched GQL request and dispatches its per-channel
// results. A whole-request failure (transport/auth/malformed batch) is NOT any
// channel's fault — log once and leave every channel's health streak untouched
// (recording it would falsely mark all channels unhealthy on one shared
// outage). Retried next cycle; the caller still staggers before the next chunk.
func (tm *TwitchMonitor) checkChunk(ctx context.Context, chunk []config.ChannelConfig) {
	logins := make([]string, len(chunk))
	for i := range chunk {
		logins[i] = chunk[i].ID
	}

	infos, errs, wholeErr := tm.streamInfoBatch(ctx, logins)
	if wholeErr != nil {
		// Warn once per streak, at its start: every cycle repeats it, and a
		// line every 15 s would bury the log.
		if tm.batchFailStreak.Add(1) == 1 {
			tm.logger.Warn("Twitch batch check failed; Twitch channels are not being checked until it recovers",
				"channels", len(chunk), "err", wholeErr)
		} else {
			tm.logger.Debug("twitch batch check failed", "channels", len(chunk), "err", wholeErr)
		}
		return
	}
	if n := tm.batchFailStreak.Swap(0); n > 0 {
		tm.logger.Info("Twitch batch checks recovered", "failedBatches", n)
	}

	for i := range chunk {
		ch := &chunk[i]
		if errs[i] != nil {
			tm.health.recordError(ch.ID, errs[i])
			tm.logger.Debug("twitch check failed", "channel", ch.Name, "err", errs[i])
			continue
		}
		tm.health.recordSuccess(ch.ID)
		if infos[i] == nil {
			continue // offline
		}
		if err := tm.processStreamInfo(ctx, ch, infos[i]); err != nil {
			tm.logger.Debug("twitch process failed", "channel", ch.Name, "err", err)
		}
	}
}

// processStreamInfo handles a channel that GetStreamInfoBatch reported LIVE:
// dedup, recovery, term matching, and OnStreamFound dispatch. (The fetch
// itself moved to the batch call in doCheck.)
func (tm *TwitchMonitor) processStreamInfo(ctx context.Context, ch *config.ChannelConfig, info *twitch.TwitchStreamInfo) error {
	// Dedup by stream ID. HasProcessed is keyed by job ID (see main.go's
	// AddToHistory(jobID) call), while HasActiveJob queries the video_id
	// column which for monitor-created Twitch jobs stores the unprefixed
	// stream ID (manual Twitch adds in the web routes store jobID in both
	// columns, which is why their HasActiveJob(jobID) call works). Prior
	// code passed jobID here — that check was effectively dead, surviving
	// only because SQLite's INSERT-OR-IGNORE caught the collision at
	// insert time.
	jobID := twitch.BuildJobID(info.StreamID, false)
	processed, hpErr := tm.db.HasProcessed(jobID)
	if hpErr != nil {
		tm.logger.Debug("HasProcessed query failed", "jobID", jobID, "err", hpErr)
	}
	if processed {
		// Check whether the existing job is in a recoverable error state —
		// i.e. the SAME broadcast is still live (we got a hit on the same
		// streamID that produced this jobID) and the prior error matches
		// a transient Twitch GQL flap. If so, dispatch to OnStreamRecover.
		if tm.OnStreamRecover != nil {
			existing, getErr := tm.db.GetJob(jobID)
			if getErr != nil {
				tm.logger.Debug("recover check: GetJob failed", "jobID", jobID, "err", getErr)
			} else if isRecoverableTwitchError(existing, worker.MaxTwitchAutoRetries) {
				tm.logger.Info("twitch recoverable error — re-enqueueing job",
					"jobID", jobID,
					"channel", info.ChannelDisplayName,
					"streamID", info.StreamID,
					"prevRetries", existing.AutoRetryCount)
				tm.OnStreamRecover(info, ch, jobID)
				return nil
			}
		}
		return nil
	}
	active, haErr := tm.db.HasActiveJob(info.StreamID)
	if haErr != nil {
		// Don't swallow DB errors — proceeding could create duplicates if
		// the DB was simply busy. Log and abort for this cycle.
		tm.logger.Debug("HasActiveJob query failed", "streamID", info.StreamID, "err", haErr)
		return nil
	}
	if active {
		return nil
	}
	// A manually added job parked on this channel (waitForTwitchLive) is
	// waiting for exactly this broadcast and records it when its own poll
	// sees it live. It is the operator's explicit request, so it stands
	// whatever the channel's terms say.
	waiting, mErr := tm.db.HasActiveManualTwitchJob(info.ChannelLogin)
	if mErr != nil {
		tm.logger.Debug("HasActiveManualTwitchJob query failed", "channel", info.ChannelLogin, "err", mErr)
		return nil
	}
	if waiting {
		tm.logger.Debug("twitch stream already claimed by a manually added job",
			"channel", info.ChannelLogin, "streamID", info.StreamID)
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

// getTwitchChannels returns a copy of the Twitch channel list under
// configStore.Read so the polling loop iterates freely without holding
// the lock across network calls. Audit reports/monitor.md Critical Issue #1.
func (tm *TwitchMonitor) getTwitchChannels() []config.ChannelConfig {
	var channels []config.ChannelConfig
	tm.configStore.Read(func(c *config.MoomboxConfig) {
		for _, ch := range c.Channels {
			if ch.Enabled != nil && !*ch.Enabled {
				continue
			}
			if ch.Platform != "twitch" {
				continue
			}
			channels = append(channels, ch)
		}
	})
	return channels
}
