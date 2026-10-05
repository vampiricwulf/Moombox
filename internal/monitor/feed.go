package monitor

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/httpx"
)

const (
	feedFetchTimeout         = 15 * time.Second
	defaultArchiveWindowDays = 3
	// membershipMemoTTL is how long a "not a member of this channel" answer
	// suppresses that channel's authenticated /membership fetch. RSS never
	// lists members-only content, so the fetch is the only discovery source
	// for it — but for a channel the operator is not a member of, it is a ~1 MB
	// authenticated page load per cycle that can only ever say the same thing.
	// Six hours is short enough that joining a channel's membership starts
	// working the same day without a restart.
	membershipMemoTTL = 6 * time.Hour
	// membershipLivenessMaxTries bounds how many NOMINATION-DRIVEN membership
	// fetches one cycle will spend chasing the liveness floor when they keep
	// failing. The floor wants a fetch that RETURNS, so an errored one hands
	// the attempt to another channel — but without a ceiling, a cycle in which
	// every fetch fails would walk the entire channel list, which is the
	// per-cycle load the memo exists to remove. Two: the nominee, plus one
	// stand-in.
	//
	// Only fetches the nomination bought are charged. A channel outside its
	// non-member horizon is fetched regardless, so letting its failure spend
	// the budget would let broken channels starve the nominee — see
	// recordMembershipFetchError.
	membershipLivenessMaxTries = 2
	// monitorDrainLimit bounds how much of a non-200 response body is read
	// before Close. Draining returns the connection to the idle pool instead
	// of discarding it (costly during a sustained outage), but an unbounded
	// drain reads an arbitrarily large error page — a 5xx HTML page, a
	// captive-portal interstitial — purely to throw it away.
	monitorDrainLimit = 4096
)

// feedStagger spaces consecutive channel feed fetches. Decapi and Twitch
// already stagger; a tight loop of YouTube RSS fetches on a big channel
// list looks like scraping behavior from a single source IP. Package var so
// tests driving several full cycles can shrink it.
var feedStagger = 500 * time.Millisecond

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
// Age is a coarse recency estimate (0 = live/upcoming) the STORE step turns
// into the row's skew-new 'coarse' date (or 'assumed' when zero and not live).
type MembershipVideo struct {
	VideoID string
	Title   string
	Age     time.Duration
}

// MembershipFetchFunc fetches the members-only videos listed on a channel's
// authenticated /membership tab.
//
// confirmedNonMember is the ONLY thing the non-member memo (membershipMemoTTL)
// may act on, and it is deliberately narrow: true means YouTube, on a session
// it RECOGNISED, showed this channel no membership tab. Everything else is
// false, because everything else is a question that was not answered:
//
//   - a member, whether or not their tab currently lists any videos (an empty
//     members tab and a non-member's page have identical video lists, so the
//     list can never be the source of this bit);
//   - a session YouTube did not recognise, or one it answered ambiguously —
//     a logged-out session is served the same "you are not a member" page for
//     EVERY channel, and memoizing that would suppress members-only discovery
//     everywhere for membershipMemoTTL on nothing but an expired cookie;
//   - any fetch that returned an error, which answers neither question.
//
// So false means "do not memoize", and false is the zero value: a caller that
// forgets this bit degrades to today's fetch-every-cycle behaviour rather than
// to a six-hour blackout. An error must still leave the memo untouched — the
// monitor enforces that itself rather than trusting the fetcher.
//
// Typically wired to youtube.Service.FetchMembershipVideos through an adapter
// that folds in the session verdict (see cmd/moombox's
// membershipConfirmedNonMember).
type MembershipFetchFunc func(ctx context.Context, channelID string) (videos []MembershipVideo, confirmedNonMember bool, err error)

// RSSFetchFunc fetches a channel's raw YouTube RSS feed body. Mirrors
// MembershipFetchFunc: a named type so FeedMonitor.FetchRSS and test fixtures
// (feed_test.go) share one signature. Typically wired to fm.fetchFeed (the
// real HTTP GET); tests inject fixtures instead via rssFetch's nil check.
type RSSFetchFunc func(ctx context.Context, ch *config.ChannelConfig) ([]byte, error)

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

	OnSchedule func(nextCheckAt int64)
	// OnVideoFound is fired by the ARCHIVE step (archive.go) for every item
	// the §10 decision table admits. The disposition tells the host HOW to
	// create the job (spec §10's creator table).
	OnVideoFound func(videoID, title, url string, channel *config.ChannelConfig, d JobDisposition)
	ProbeVideo   VideoProbeFunc
	// ProbeVideoAuth is the AUTHENTICATED probe used only for members-only
	// videos discovered via the /membership tab. An anonymous probe (ProbeVideo)
	// can't access members-only content, gets no formats, and the classifier
	// then misfires it as "upcoming" (which bypasses include_non_live_content).
	// The authenticated probe sees the real formats and classifies correctly
	// (vod/live/upcoming). Nil falls back to ProbeVideo.
	ProbeVideoAuth VideoProbeFunc
	// ProbeDate is the date-completing half of the two-phase probe (§9).
	// The status probes (ANDROID_VR/TV) carry no microformat and therefore
	// no publish dates; when a vod-family probe returns dateless on a row
	// whose own date is only an estimate (coarse/assumed), the walk calls
	// this once — a WEB player fetch carrying the jar's credentials — to
	// date the row. The ladder makes the upgrade one-time per video. Nil
	// disables the second phase (rows with rankable dates still classify;
	// see applyProbe).
	ProbeDate       func(ctx context.Context, videoID string) (publishedAt, precision string, err error)
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

	// nonMemberUntil memoizes the channels YouTube CONFIRMED the account is
	// not a member of: the horizon (cycle now + membershipMemoTTL) before
	// which the authenticated fetch is skipped. Keyed by channel ID and pruned
	// to the configured list every cycle by armMembershipLiveness, so it is
	// bounded by the channel count. Guarded by fm.mu.
	nonMemberUntil map[string]time.Time
	// membershipFetchErrored holds the channels whose LAST membership fetch
	// returned an error, cleared the moment one returns. Such a channel is
	// still fetched every cycle (an error writes no memo), but that fetch
	// answers nothing, so armMembershipLiveness must not count it as this
	// cycle's liveness observation — otherwise one permanently broken channel
	// suppresses every other channel's fetch forever. Guarded by fm.mu.
	membershipFetchErrored map[string]struct{}
	// membershipLivenessNeeded says this cycle still owes the session a
	// membership fetch that RETURNS, because every channel it would otherwise
	// have fetched is inside its non-member horizon. Set by
	// armMembershipLiveness, cleared by the first fetch that comes back.
	// Guarded by fm.mu.
	membershipLivenessNeeded bool
	// membershipLivenessID is the channel PREFERRED to carry that fetch — the
	// memoized one with the earliest horizon, which is what makes the duty
	// rotate. Empty means "any memoized channel will do", which is the state
	// after the preferred one's fetch failed. Guarded by fm.mu.
	membershipLivenessID string
	// membershipLivenessTries is how many more NOMINATION-DRIVEN membership
	// fetches this cycle may spend satisfying the floor
	// (membershipLivenessMaxTries at the top of each cycle). A channel fetched
	// on its own account never draws on it. Guarded by fm.mu.
	membershipLivenessTries int

	// FetchRSS overrides the RSS feed fetch (fm.fetchFeed's real HTTP GET)
	// for tests. Nil uses the real fetch — see rssFetch.
	FetchRSS RSSFetchFunc

	// BackfillSweep, when non-nil, is invoked ONCE per monitor cycle (spec
	// §11: the sweep condition is evaluated every cycle plus startup and
	// kickMonitors — and both of those run a cycle, via Start's immediate
	// first check and CheckNow, so this single site covers every trigger).
	// The host wires it to the backfill worker's Sweep with freshly-resolved
	// ChannelRefs. Only the TRIGGER runs in the cycle: scans run on the
	// worker's own throttled serial queue, never through the monitor's
	// per-video retry/backoff loop.
	BackfillSweep func()

	// now returns the current time. checkChannel reads it exactly ONCE per
	// cycle (spec §7's one-`now` rule) so every timestamp a cycle writes —
	// last_rss_ok_at, the STORE step's coarse/assumed dates, the ARCHIVE
	// cutoff — derives from a single instant. Defaults to time.Now; tests pin
	// it via withNow (feed_test.go) for deterministic date math.
	now func() time.Time
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

// SetOnChannelHealthy installs the callback fired once when a channel that
// crossed the threshold answers a check again.
func (fm *FeedMonitor) SetOnChannelHealthy(fn func(channelID string)) {
	fm.health.onHealthy = fn
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
		now:           time.Now,
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
	// Unreachable since the leading ctx.Err() guard — Stop() cancels the ctx
	// and nils cancel together, and Stop() itself writes NextCheckAt = 0 — so
	// this no longer clears the -1 sentinel for a cycle racing Stop(). Kept as
	// defence for a future cancel-without-cancel path: a monitor that nils
	// cancel without cancelling its context would otherwise arm a timer and
	// publish a countdown for a chain nothing owns.
	if fm.cancel == nil {
		fm.NextCheckAt = 0
		fm.mu.Unlock()
		return
	}
	// Kept beside the leading guard, not redundant with it: this closes the
	// window between that check and this lock, in which a Stop()+Start() would
	// install a NEW non-nil cancel — so `cancel == nil` above passes and this
	// dead chain would write the LIVE chain's NextCheckAt and publish its
	// OnSchedule. Re-reading ctx.Err() costs nothing; do not "simplify" it away.
	if ctx.Err() != nil {
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
	// Backfill sweep trigger (spec §11) — after the offline gate (scans
	// would only fail offline) but BEFORE the no-channels early return: a
	// sweep over an emptied channel list is how the worker learns every
	// channel departed and prunes their data.
	if fm.BackfillSweep != nil {
		fm.BackfillSweep()
	}
	channels := fm.getYouTubeChannels()
	if len(channels) == 0 {
		return
	}
	fm.armMembershipLiveness(channels, fm.now().UTC())

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

// checkChannel runs one channel's monitor cycle (spec §7):
//
//  1. FETCH   RSS and — when active — the authenticated /membership tab,
//     independently; either may fail, neither is fatal to the other.
//     An RSS transport SUCCESS writes channel_state.last_rss_ok_at
//     immediately, here, not at cycle end — the ARCHIVE step reads the
//     established gate later THIS SAME cycle, so a fresh install's first
//     successful fetch opens the gate without waiting a poll interval.
//  2. STORE   Upsert every item seen (db.UpsertFeedItem) with its
//     listing-derived date/precision and collect the video IDs
//     inserted (not merely re-sighted) THIS cycle into newIDs, for
//     the ARCHIVE step to disposition as new-vs-backlog.
//  3. WALK    the serial probe pass over the store's scope (walk.go, spec §8),
//     returning the FRESH map of this cycle's successful probes.
//  4. ARCHIVE re-read scope — the walk corrected dates and statuses, so rows
//     may have entered or left it — and decide jobs per item against
//     the §10 decision table (archive.go), reusing FRESH results so
//     nothing is probed twice in one cycle.
//
// WALK and ARCHIVE run under separate budgets scaled to the scope they read
// (passBudget) — a truncated walk must not also kill archival (§7).
//
// Returns the RSS fetch/parse error for channel-health accounting. A
// membership failure is logged but never marks the RSS feed unhealthy — they
// are independent signals.
func (fm *FeedMonitor) checkChannel(ctx context.Context, ch *config.ChannelConfig) error {
	chID := ch.ID
	cycleNow := fm.now().UTC()
	cutoff := cycleNow.Add(-time.Duration(fm.archiveWindowDays(ch)) * 24 * time.Hour).Format(time.RFC3339)

	// 1. FETCH — independent; neither failure is fatal to the other.
	data, rssErr := fm.rssFetch(ctx, ch)
	if rssErr == nil {
		// Transport success establishes the channel even on zero entries or an
		// unparseable body (spec §11 residual) — a FETCH, not a parse, is the
		// gate.
		if err := fm.db.SetChannelRSSOK(chID, cycleNow.Format(time.RFC3339)); err != nil {
			fm.logger.Warn("last_rss_ok_at write failed", "channel", ch.Name, "err", err)
		}
	}

	var rssCandidates []discoveredVideo
	if rssErr == nil {
		// A parse failure (malformed 200 response) becomes the health error, so
		// a persistently broken feed still surfaces as unhealthy rather than a
		// silent success. It does not retract the last_rss_ok_at write above.
		rssCandidates, rssErr = fm.parseFeedCandidates(ch, data)
	}

	// Members-only discovery: RSS never lists members-only content, so this is
	// the only source for members live/upcoming streams (and, with
	// include_non_live_content, their VODs).
	var membVideos []MembershipVideo
	if fm.membershipActive() && fm.membershipFetchAllowed(chID, cycleNow) {
		// defer cancel() inside the closure so a panic in FetchMembership can't
		// leak the timeout timer, while still releasing it the moment the fetch
		// returns (not held for the rest of checkChannel).
		vids, nonMember, mErr := func() ([]MembershipVideo, bool, error) {
			mctx, cancel := context.WithTimeout(ctx, feedFetchTimeout)
			defer cancel()
			return fm.FetchMembership(mctx, chID)
		}()
		if mErr != nil {
			// A failed fetch answers neither question, so it writes no memo —
			// the same rule cmd/moombox's routeLivenessVerdict applies to a
			// verdict we never got. nonMember is ignored here on purpose:
			// MembershipFetchFunc says an error leaves it unanswered, and the
			// memo is too costly to be wrong about on a fetcher's good
			// behaviour.
			fm.recordMembershipFetchError(chID, cycleNow)
			fm.logger.Debug("membership discovery failed", "channel", ch.Name, "err", mErr)
		} else {
			membVideos = vids
			fm.recordMembershipSuccess(chID, nonMember, cycleNow)
		}
	}

	// 2. STORE — upsert every item seen; collect NEW ids for the ARCHIVE step.
	newIDs := map[string]bool{}
	first := cycleNow.Format(time.RFC3339)
	for i, c := range rssCandidates {
		// A zero published (missing/unparseable <published> — see
		// parseFeedCandidates) stores as 'assumed'/cycle-now: a claim of
		// ignorance Q2's unresolved arm keeps in scope until a probe dates it.
		// 'exact'/0001-01-01T00:00:00Z would be a permanent sink — outside Q1
		// forever, in neither Q2 arm, and rank-4 'exact' blocks any later
		// correction (§12: nothing is excluded on a date we have not verified).
		pub, prec := c.published.UTC().Format(time.RFC3339), "exact"
		if c.published.IsZero() {
			pub, prec = first, "assumed"
		}
		ins, err := fm.db.UpsertFeedItem(database.FeedItem{
			ChannelID: chID, VideoID: c.videoID, Title: c.title,
			Published: pub, DatePrecision: prec,
			CatalogPos: i, Source: "rss", FirstSeen: first,
		})
		if err != nil {
			fm.logger.Warn("upsert failed; skipping item this cycle", "id", c.videoID, "err", err)
			continue
		}
		if ins {
			newIDs[c.videoID] = true
		}
	}
	for i, v := range membVideos {
		pub, prec := first, "assumed"
		if v.Age > 0 {
			pub, prec = cycleNow.Add(-v.Age).Format(time.RFC3339), "coarse"
		}
		ins, err := fm.db.UpsertFeedItem(database.FeedItem{
			ChannelID: chID, VideoID: v.VideoID, Title: v.Title,
			Published: pub, DatePrecision: prec,
			CatalogPos: i, Source: "membership", FirstSeen: first,
		})
		if err != nil {
			fm.logger.Warn("upsert failed; skipping item this cycle", "id", v.VideoID, "err", err)
			continue
		}
		if ins {
			newIDs[v.VideoID] = true
		}
	}

	// 3. WALK — the serial probe pass over the store's scope (spec §8).
	scope, scopeErr := fm.db.FeedScope(chID, cutoff, fm.membershipDiscoveryEnabled())
	if scopeErr != nil {
		fm.logger.Warn("FeedScope query failed; walk+archive skipped this cycle", "channel", ch.Name, "err", scopeErr)
		return rssErr
	}
	walkCtx, walkCancel := context.WithTimeout(ctx, passBudget(len(scope)))
	fresh := fm.walk(walkCtx, ch, chID, cutoff, scope)
	walkCancel()

	// 4. ARCHIVE — re-read scope (the walk corrected dates and statuses, so
	// rows may have entered or left it) and decide jobs per item (spec §10).
	scope, scopeErr = fm.db.FeedScope(chID, cutoff, fm.membershipDiscoveryEnabled())
	if scopeErr != nil {
		fm.logger.Warn("FeedScope re-read failed; archive skipped this cycle", "channel", ch.Name, "err", scopeErr)
		return rssErr
	}
	archiveCtx, archiveCancel := context.WithTimeout(ctx, passBudget(len(scope)))
	fm.archive(archiveCtx, ch, chID, cutoff, scope, newIDs, fresh)
	archiveCancel()

	return rssErr
}

// rssFetch is the injectable RSS-fetch seam: FetchRSS when a test has wired
// one, else the real HTTP GET. Mirrors membershipActive's FetchMembership
// indirection so checkChannel never calls fetchFeed directly.
func (fm *FeedMonitor) rssFetch(ctx context.Context, ch *config.ChannelConfig) ([]byte, error) {
	if fm.FetchRSS != nil {
		return fm.FetchRSS(ctx, ch)
	}
	return fm.fetchFeed(ctx, ch)
}

// fetchFeed GETs the channel's RSS feed, reporting the transport outcome to the
// passive connectivity tracker. Returns the body on success.
func (fm *FeedMonitor) fetchFeed(ctx context.Context, ch *config.ChannelConfig) ([]byte, error) {
	feedURL := fmt.Sprintf("https://www.youtube.com/feeds/videos.xml?channel_id=%s", ch.ID)

	fetchCtx, fetchCancel := context.WithTimeout(ctx, feedFetchTimeout)
	defer fetchCancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := monitorHTTPClient.Do(req)
	if err != nil {
		// Transport-level failure — DNS error, TCP reset, or context deadline.
		// Contributes toward the passive offline trigger.
		reportMonitorResult("monitor/feed", true)
		return nil, fmt.Errorf("fetch feed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 4xx/5xx isn't necessarily a connectivity problem (rate-limiting or a
		// dead channel ID), but isn't a success either — leave the tracker alone.
		drainBounded(resp.Body) // bounded drain for connection reuse
		return nil, fmt.Errorf("feed http %d", resp.StatusCode)
	}
	reportMonitorResult("monitor/feed", false)

	return io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB limit
}

// drainBounded reads and discards at most monitorDrainLimit bytes of r so the
// underlying connection returns to the idle pool. Shared by the feed and
// DECAPI fetchers, which are the two monitor paths that close on a non-200.
func drainBounded(r io.Reader) {
	io.Copy(io.Discard, io.LimitReader(r, monitorDrainLimit))
}

// membershipActive reports whether members-only discovery should run this cycle
// (a fetcher is wired and the config flag + cookies gate, if set, allows it).
func (fm *FeedMonitor) membershipActive() bool {
	if fm.FetchMembership == nil {
		return false
	}
	return fm.MembershipEnabled == nil || fm.MembershipEnabled()
}

// armMembershipLiveness prepares this cycle's membership decisions: it prunes
// the per-channel bookkeeping to the configured channels and, when every
// channel it would otherwise fetch is inside its non-member horizon, nominates
// the one with the earliest horizon to be fetched anyway.
//
// WHAT THE NOMINATION GUARANTEES, exactly: every cycle makes at least one
// membership fetch that RETURNS, provided some memoized channel has not
// recently errored. When they ALL have, the errored ones are tried again
// rather than skipped — a fetch is the only thing that can clear the flag —
// and that retry is bounded by membershipLivenessMaxTries, so a cycle in
// which everything fails still costs at most two attempts and may end with
// nothing returned. It does NOT guarantee a liveness VERDICT even when a
// fetch does return; tier-2 FallbackLiveness is the backstop for both gaps.
// The fetch feeds a verdict — cmd/moombox's FetchMembership adapter hands the
// SessionAuthState to (*cookies.RefreshService).ObserveLiveness — but the
// routeLivenessVerdict that gets it there forwards only LoggedIn/LoggedOut, so
// a page carrying no login marker observes nothing even though the fetch
// succeeded. That residue is the tier-2 FallbackLiveness probe's job: it runs
// precisely when no conclusive observation has landed recently
// (livenessObservedRecently in internal/cookies/refresh_liveness.go; wired to
// ProbeAccountLiveness in cmd/moombox/services.go). This floor is the cheap
// first tier, not the whole guarantee.
//
// What it does rule out is the silent failure the memo would otherwise create:
// skipping EVERY channel, cycle after cycle, so the tier-1 signal disappears
// while both dashboards stay clean.
//
// A channel whose last fetch errored is discounted TWICE here, for the same
// reason: an error answers nothing.
//
//   - It is not counted as "fetched on its own account". Its fetch still
//     happens (an error writes no memo, so it never becomes memoized) but it
//     answers nothing, and letting it stand in would let one permanently
//     broken channel suppress every other channel's fetch forever.
//   - It is passed over when NOMINATING. A failed fetch writes no memo, so a
//     broken nominee's horizon never advances and it would win the
//     earliest-horizon pick every cycle — and recordMembershipFetchError's
//     failover can only reach channels later in the walk, so a broken nominee
//     at the end of the list would take the whole cycle down with it.
//
// The second only yields when EVERY memoized channel has errored: a fetch is
// the only thing that clears the flag, so nominating nobody would be a state
// with no exit.
//
// Called once per cycle from doCheck. This is a SECOND fm.now() read per
// cycle, distinct from checkChannel's one-`now` rule (spec §7): it dates no
// stored row, only the 6 h memo horizons.
func (fm *FeedMonitor) armMembershipLiveness(channels []config.ChannelConfig, now time.Time) {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	fm.membershipLivenessID = ""
	fm.membershipLivenessNeeded = false
	fm.membershipLivenessTries = membershipLivenessMaxTries
	if len(fm.nonMemberUntil) == 0 && len(fm.membershipFetchErrored) == 0 {
		return
	}

	configured := make(map[string]struct{}, len(channels))
	for i := range channels {
		configured[channels[i].ID] = struct{}{}
	}
	for id := range fm.nonMemberUntil {
		if _, ok := configured[id]; !ok {
			delete(fm.nonMemberUntil, id)
		}
	}
	for id := range fm.membershipFetchErrored {
		if _, ok := configured[id]; !ok {
			delete(fm.membershipFetchErrored, id)
		}
	}
	if len(fm.nonMemberUntil) == 0 {
		return // nothing is being skipped, so there is nothing to stand in for
	}

	// Two candidate slots, because "earliest horizon" alone re-nominates a
	// channel that cannot answer. A failed fetch writes no memo, so a nominee
	// that keeps erroring keeps the earliest horizon and keeps winning the
	// pick — and the in-cycle failover in recordMembershipFetchError can only
	// hand the duty to a memoized channel LATER in the walk, so with the
	// broken nominee last, every cycle ends with nothing returned until the
	// memo expires. The errored ones are therefore passed over here.
	//
	// They are not passed over unconditionally: only a fetch can clear a
	// channel's errored flag, so a cycle that nominates nobody is a dead end
	// once every memoized channel has failed. When the preferred slot comes
	// up empty the earliest errored one is tried again — costing at most
	// membershipLivenessMaxTries attempts, exactly as a failing nominee
	// always has.
	var earliestID, erroredID string
	var earliest, erroredEarliest time.Time
	for i := range channels {
		id := channels[i].ID
		until, memoized := fm.nonMemberUntil[id]
		_, errored := fm.membershipFetchErrored[id]
		if !memoized || !now.Before(until) {
			if errored {
				continue // it will be fetched, but it will not answer
			}
			return // fetched on its own account; no nomination needed
		}
		if errored {
			if erroredID == "" || until.Before(erroredEarliest) {
				erroredID, erroredEarliest = id, until
			}
			continue
		}
		if earliestID == "" || until.Before(earliest) {
			earliestID, earliest = id, until
		}
	}
	if earliestID == "" {
		earliestID = erroredID
	}
	if earliestID == "" {
		return
	}
	fm.membershipLivenessID = earliestID
	fm.membershipLivenessNeeded = true
}

// membershipFetchAllowed reports whether this cycle fetches chID's
// authenticated /membership tab: yes when the channel is not memoized as a
// non-member (or its horizon has passed), and yes while this cycle still owes
// the session a fetch that returns and chID is the channel nominated to carry
// it (or any memoized channel, once the nominee's own fetch has failed).
//
// It does NOT consume the nomination. Only a fetch that comes back satisfies
// the floor, and this runs before the fetch — consuming here would let one
// transient error cost the whole cycle's liveness signal.
func (fm *FeedMonitor) membershipFetchAllowed(chID string, now time.Time) bool {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	until, memoized := fm.nonMemberUntil[chID]
	if !memoized || !now.Before(until) {
		return true
	}
	if !fm.membershipLivenessNeeded {
		return false
	}
	return fm.membershipLivenessID == "" || fm.membershipLivenessID == chID
}

// recordMembershipSuccess stores the verdict of a membership fetch that came
// back for chID, and retires this cycle's liveness obligation: the fetch
// returned, which is all the floor promises.
//
// A member is never memoized — their tab is the only place a members-only live
// stream is ever listed, and it can go from empty to live between two cycles —
// and neither is a channel whose "not a member" page came back on a session
// YouTube did not recognise (see MembershipFetchFunc).
func (fm *FeedMonitor) recordMembershipSuccess(chID string, confirmedNonMember bool, now time.Time) {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	delete(fm.membershipFetchErrored, chID)
	fm.membershipLivenessNeeded = false
	fm.membershipLivenessID = ""

	if !confirmedNonMember {
		delete(fm.nonMemberUntil, chID)
		return
	}
	if fm.nonMemberUntil == nil {
		fm.nonMemberUntil = make(map[string]time.Time)
	}
	fm.nonMemberUntil[chID] = now.Add(membershipMemoTTL)
}

// recordMembershipFetchError notes that chID's membership fetch did not come
// back. It writes NO memo — an error answers neither "is this account a
// member" nor "is this session alive".
//
// Whether it touches the cycle's liveness obligation depends on WHY the fetch
// happened, which `now` against the memo answers: a channel inside its
// non-member horizon was fetched because the nomination asked for it, so its
// failure is the nomination's failure — the preference is dropped, another
// memoized channel can carry it, and the attempt is charged against
// membershipLivenessMaxTries so a cycle in which everything fails cannot walk
// the whole channel list.
//
// A channel OUTSIDE its horizon (or never memoized at all) was going to be
// fetched whatever the memo said. Its failure costs the floor nothing and must
// leave the budget alone in both directions: charging it lets a couple of
// permanently broken channels at the head of the list exhaust the budget
// before the nominee is reached — the cycle then observes nothing at all,
// which is the very outcome the floor exists to prevent — and re-arming on it
// buys a second authenticated ~1 MB fetch of a channel the memo says to skip,
// after the nominee has already answered.
func (fm *FeedMonitor) recordMembershipFetchError(chID string, now time.Time) {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	if fm.membershipFetchErrored == nil {
		fm.membershipFetchErrored = make(map[string]struct{})
	}
	fm.membershipFetchErrored[chID] = struct{}{}

	// The same test membershipFetchAllowed made before the fetch. A failed
	// fetch writes no memo and a cycle walks its channels one at a time, so
	// the only thing that can have moved the answer in between is
	// ResetMembershipMemo, called from the cookie-refresh goroutine. It only
	// ever DELETES entries — never adds or extends one — so the disagreement
	// is one-directional: a channel that was inside its horizon before the
	// fetch can read as un-memoized here, and this returns early instead of
	// charging the try. That leaves a try UNSPENT, which is the harmless
	// direction; the repair it rode in on re-fetches every channel next cycle
	// anyway.
	until, memoized := fm.nonMemberUntil[chID]
	if !memoized || !now.Before(until) {
		return // fetched on its own account; not the nomination's to spend
	}

	if chID == fm.membershipLivenessID {
		// The preferred channel failed; any memoized one will do now. Leaving
		// the preference pinned would strand the floor on a channel this cycle
		// has already passed.
		fm.membershipLivenessID = ""
	}
	fm.membershipLivenessTries--
	fm.membershipLivenessNeeded = fm.membershipLivenessTries > 0
}

// ResetMembershipMemo drops every non-member memo and reports how many it
// dropped, so the next cycle re-fetches every channel's /membership tab.
//
// Wired to a YouTube auth repair (cmd/moombox's OnAuthRecovered). The memo
// suppresses the ONLY discovery path there is for members-only content, so an
// operator who has just fixed their credentials must not have to wait out
// membershipMemoTTL to see members-only streams again — belt to the
// confirmedNonMember braces, which already keep an unrecognised session from
// writing a memo at all.
//
// The fetch-errored set is deliberately left alone: re-authenticating repairs
// no transport failure, and that set only ever relaxes the nomination.
func (fm *FeedMonitor) ResetMembershipMemo() int {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	n := len(fm.nonMemberUntil)
	clear(fm.nonMemberUntil)
	return n
}

// atomFeed represents the Atom XML feed structure.
type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	VideoID   string     `xml:"http://www.youtube.com/xml/schemas/2015 videoId"`
	Title     string     `xml:"title"`
	Published string     `xml:"published"` // RFC3339, e.g. 2026-07-13T04:18:12+00:00
	Links     []atomLink `xml:"link"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
}

// resolveArchiveWindowDays is THE per-channel resolver for how many days back
// to archive — stated once, shared by both monitors' archiveWindowDays
// methods so feed and DECAPI can never disagree about the window: the
// channel's own ArchiveWindowDays override, else the global
// monitors.archive_window_days, else defaultArchiveWindowDays. Upcoming/live
// content is always covered regardless of this window.
func resolveArchiveWindowDays(store *config.Store, ch *config.ChannelConfig) int {
	if ch.ArchiveWindowDays != nil && *ch.ArchiveWindowDays > 0 {
		return *ch.ArchiveWindowDays
	}
	var g int
	store.Read(func(c *config.MoomboxConfig) { g = c.Monitors.ArchiveWindowDays })
	if g > 0 {
		return g
	}
	return defaultArchiveWindowDays
}

// archiveWindowDays resolves the channel's archive window
// (resolveArchiveWindowDays). Read by checkChannel to compute the cycle's
// cutoff, which both the walk's early exit and the ARCHIVE step's window
// re-check (archive.go) test against.
func (fm *FeedMonitor) archiveWindowDays(ch *config.ChannelConfig) int {
	return resolveArchiveWindowDays(fm.configStore, ch)
}

// membershipDiscoveryEnabled reports the operator's membership_discovery
// config TOGGLE only — never membershipActive(), which also folds in
// cookie state. FeedScope's includeMembership parameter must reflect only
// the toggle: cookie state moving scope would let a cookie lapse silently
// drop members rows instead of leaving them in scope, probe-gated (spec
// §7). Mirrors monitor_callbacks.go's MembershipEnabled config half.
func (fm *FeedMonitor) membershipDiscoveryEnabled() bool {
	var enabled bool
	fm.configStore.Read(func(c *config.MoomboxConfig) { enabled = c.Monitors.MembershipDiscoveryEnabled() })
	return enabled
}

// discoveredVideo is one parsed RSS feed entry, as consumed by the STORE step
// (spec §7): videoID/title/published feed the upsert. url is a parse output
// the store does not persist — the store-driven passes synthesize canonical
// watch URLs (archive.go). The description is not parsed at all: the passes
// term-match on title only (§8), since a store row carries no description.
type discoveredVideo struct {
	videoID   string
	title     string
	url       string    // RSS alternate link; not stored
	published time.Time // RSS <published> — 'exact' in the store; zero ⇒ 'assumed'/cycle-now
	source    string    // always "rss" (feed_items.source)
}

// parseFeedCandidates parses an Atom feed into discovery candidates. It returns
// ALL entries; the STORE step upserts every one, carrying its <published> date
// as the row's 'exact'-precision published (zero time ⇒ 'assumed'/cycle-now —
// see the STORE step). A parse failure is returned so the caller can record it
// as channel-health.
func (fm *FeedMonitor) parseFeedCandidates(ch *config.ChannelConfig, data []byte) ([]discoveredVideo, error) {
	var feed atomFeed
	if err := xml.Unmarshal(data, &feed); err != nil {
		return nil, fmt.Errorf("parse feed: %w", err)
	}
	entries := feed.Entries
	if len(entries) == 0 {
		return nil, nil
	}

	out := make([]discoveredVideo, 0, len(entries))
	for _, entry := range entries {
		if entry.VideoID == "" {
			continue
		}

		videoURL := ""
		for _, link := range entry.Links {
			if link.Rel == "alternate" {
				videoURL = link.Href
				break
			}
		}

		// A missing/invalid <published> parses to the zero time. The zero
		// value is a SIGNAL, not a date: the STORE step (checkChannel) maps
		// it to precision 'assumed' with published = the cycle's now, never
		// 'exact'/zero — which would sink the row outside every window with
		// no path to correction (§12). YouTube RSS always supplies the field;
		// this is the defensive arm.
		published, _ := time.Parse(time.RFC3339, entry.Published)

		out = append(out, discoveredVideo{
			videoID:   entry.VideoID,
			title:     entry.Title,
			url:       videoURL,
			published: published,
			source:    "rss",
		})
	}
	return out, nil
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
