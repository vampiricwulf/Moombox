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
	feedFetchTimeout         = 15 * time.Second
	defaultArchiveWindowDays = 3
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
// Age is a coarse recency estimate (0 = live/upcoming) the STORE step turns
// into the row's skew-new 'coarse' date (or 'assumed' when zero and not live).
type MembershipVideo struct {
	VideoID string
	Title   string
	Age     time.Duration
}

// MembershipFetchFunc fetches the members-only videos listed on a channel's
// authenticated /membership tab. It returns an empty slice (no error) when
// there are no auth cookies or the account is not a member of the channel.
// Typically wired to youtube.Service.FetchMembershipVideos.
type MembershipFetchFunc func(ctx context.Context, channelID string) ([]MembershipVideo, error)

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
	// create the job (spec §10's creator table); Plan 4 implements those
	// semantics — until then the host maps every disposition to today's
	// Upcoming+enqueue behavior (see the PLAN4 marker in monitor_callbacks.go).
	OnVideoFound func(videoID, title, url string, channel *config.ChannelConfig, d JobDisposition)
	ProbeVideo   VideoProbeFunc
	// ProbeVideoAuth is the AUTHENTICATED probe used only for members-only
	// videos discovered via the /membership tab. An anonymous probe (ProbeVideo)
	// can't access members-only content, gets no formats, and the classifier
	// then misfires it as "upcoming" (which bypasses include_non_live_content).
	// The authenticated probe sees the real formats and classifies correctly
	// (vod/live/upcoming). Nil falls back to ProbeVideo.
	ProbeVideoAuth  VideoProbeFunc
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
	if fm.membershipActive() {
		// defer cancel() inside the closure so a panic in FetchMembership can't
		// leak the timeout timer, while still releasing it the moment the fetch
		// returns (not held for the rest of checkChannel).
		vids, mErr := func() ([]MembershipVideo, error) {
			mctx, cancel := context.WithTimeout(ctx, feedFetchTimeout)
			defer cancel()
			return fm.FetchMembership(mctx, chID)
		}()
		if mErr != nil {
			fm.logger.Debug("membership discovery failed", "channel", ch.Name, "err", mErr)
		} else {
			membVideos = vids
		}
	}

	// 2. STORE — upsert every item seen; collect NEW ids for the ARCHIVE step.
	newIDs := map[string]bool{}
	first := cycleNow.Format(time.RFC3339)
	for i, c := range rssCandidates {
		ins, err := fm.db.UpsertFeedItem(database.FeedItem{
			ChannelID: chID, VideoID: c.videoID, Title: c.title,
			Published: c.published.UTC().Format(time.RFC3339), DatePrecision: "exact",
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
		io.Copy(io.Discard, resp.Body) // drain for connection reuse
		return nil, fmt.Errorf("feed http %d", resp.StatusCode)
	}
	reportMonitorResult("monitor/feed", false)

	return io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB limit
}

// membershipActive reports whether members-only discovery should run this cycle
// (a fetcher is wired and the config flag + cookies gate, if set, allows it).
func (fm *FeedMonitor) membershipActive() bool {
	if fm.FetchMembership == nil {
		return false
	}
	return fm.MembershipEnabled == nil || fm.MembershipEnabled()
}

// atomFeed represents the Atom XML feed structure.
type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	VideoID    string         `xml:"http://www.youtube.com/xml/schemas/2015 videoId"`
	Title      string         `xml:"title"`
	Published  string         `xml:"published"` // RFC3339, e.g. 2026-07-13T04:18:12+00:00
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
// (spec §7): videoID/title/published feed the upsert. desc and url are parse
// outputs the store does not persist — the store-driven passes term-match on
// title only (§8) and synthesize canonical watch URLs (archive.go).
type discoveredVideo struct {
	videoID   string
	title     string
	desc      string    // RSS description (lookbehind-deduped); not stored
	url       string    // RSS alternate link; not stored
	published time.Time // RSS <published> — 'exact' precision in the store
	source    string    // always "rss" (feed_items.source)
}

// parseFeedCandidates parses an Atom feed into discovery candidates. It returns
// ALL entries; the STORE step upserts every one, carrying its <published> date
// as the row's 'exact'-precision published. Description dedup
// (NumDescLookbehind) is applied here because it depends on feed entry order. A
// parse failure is returned so the caller can record it as channel-health.
func (fm *FeedMonitor) parseFeedCandidates(ch *config.ChannelConfig, data []byte) ([]discoveredVideo, error) {
	var feed atomFeed
	if err := xml.Unmarshal(data, &feed); err != nil {
		return nil, fmt.Errorf("parse feed: %w", err)
	}
	entries := feed.Entries
	if len(entries) == 0 {
		return nil, nil
	}

	lookbehind := 0
	if ch.NumDescLookbehind != nil {
		lookbehind = *ch.NumDescLookbehind
	}
	// Precompute per-entry line sets once (avoids O(N*M*K) re-trimming).
	var entryLineSets []map[string]struct{}
	if lookbehind > 0 {
		entryLineSets = make([]map[string]struct{}, len(entries))
		for i := range entries {
			entryLineSets[i] = descriptionLineSet(entries[i].MediaGroup.Description)
		}
	}

	out := make([]discoveredVideo, 0, len(entries))
	for i, entry := range entries {
		if entry.VideoID == "" {
			continue
		}

		// Description dedup: filter lines that appear in older entries.
		description := entry.MediaGroup.Description
		if lookbehind > 0 && i+1 < len(entries) {
			end := min(i+1+lookbehind, len(entries))
			description = filterUniqueDescriptionLinesPrecomputed(description, entryLineSets[i+1:end])
		}

		videoURL := ""
		for _, link := range entry.Links {
			if link.Rel == "alternate" {
				videoURL = link.Href
				break
			}
		}

		// A missing/invalid <published> parses to the zero time, sinking the
		// entry below dated ones in the recency sort — acceptable, and YouTube
		// RSS always supplies it.
		published, _ := time.Parse(time.RFC3339, entry.Published)

		out = append(out, discoveredVideo{
			videoID:   entry.VideoID,
			title:     entry.Title,
			desc:      description,
			url:       videoURL,
			published: published,
			source:    "rss",
		})
	}
	return out, nil
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
