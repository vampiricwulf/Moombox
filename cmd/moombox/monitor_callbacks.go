package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// crossMonitorVouchWindow bounds how recently a sibling monitor must have
// succeeded for its success to suppress another monitor's "unhealthy" alert.
// Comfortably longer than DECAPI's seconds-scale cadence and a typical feed
// interval, but short enough that a silently-stalled sibling (frozen at a stale
// success) stops vouching so a genuine outage still surfaces.
const crossMonitorVouchWindow = 20 * time.Minute

// sweepShouldResume reports whether one job should be bounced out of
// StatusCookies back to Upcoming by a credential sweep for `platform`.
//
// currentIdentity is the opaque fingerprint of the account the platform's
// cookies belong to right now, or "" when the caller has none to offer. It is
// what separates the two sweeps:
//
//   - The auth-recovered sweep passes "". Dead cookies came back to life,
//     which fixes an auth park and cannot fix a membership one.
//   - The credential sweep passes the observed identity, letting a membership
//     park move if — and only if — the account is not the one that refused it.
//
// The status+platform gate is the pre-existing behavior. What the park reason
// adds is the membership case: a job parks at ParkReasonMembership only when
// the platform answered a session it had already confirmed was SIGNED IN, so
// the auth transition cannot be the event that fixes it — that session was
// authenticated when it failed. Resuming there bought a guaranteed-identical
// failure and a full extraction attempt every auth cycle, forever.
//
// The membership comparison is deliberately against the job's OWN recorded
// identity rather than any process-level "did it change since last time" edge.
// A durable per-job comparison cannot be missed: it survives restarts, it
// cannot be consumed by an intermediate observation, and re-evaluating it is
// free and idempotent. A resumed job that fails again re-parks under the
// current identity, so it settles at exactly one retry per real account
// change.
//
// Two permissive defaults, both chosen so an unknown resolves to one wasted
// retry rather than a permanent strand:
//
//   - ParkReasonNone (every COOKIES? row predating the park_reason column, and
//     any park recorded by a path that does not classify) is resumable —
//     nothing on such a row says retroactively whether it was a membership
//     problem, and stranding a genuinely dead-cookie job is the worse error.
//   - A membership park with no recorded identity ("" — a pre-v19 row, or a
//     park where the fingerprint could not be computed) is treated as parked
//     under an unknown account and resumes on the next observation.
func sweepShouldResume(job *database.Job, platform, currentIdentity string) bool {
	if job == nil || job.Status != database.StatusCookies || job.Platform != platform {
		return false
	}
	if job.ParkReason != database.ParkReasonMembership {
		return true
	}
	// No identity on offer means this caller cannot speak to the membership
	// question at all (the auth-recovered sweep), so it must not move these.
	return currentIdentity != "" && currentIdentity != job.ParkIdentity
}

// resumeCookieParkedJobs applies sweepShouldResume to every job and returns
// how many were resumed. Split out of the callback closures so the decision
// and the database loop it actually drives can both be tested directly.
func resumeCookieParkedJobs(db *database.Database, log interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}, platform, currentIdentity string) int {
	jobs, err := db.GetAllJobs()
	if err != nil {
		log.Warn("cookie-parked sweep: GetAllJobs failed", "platform", platform, "err", err)
		return 0
	}
	resumed := 0
	for _, job := range jobs {
		if !sweepShouldResume(job, platform, currentIdentity) {
			continue
		}
		db.UpdateJobFields(job.ID, map[string]any{
			"status":        database.StatusUpcoming,
			"error":         "",
			"park_reason":   database.ParkReasonNone,
			"park_identity": "",
		})
		resumed++
	}
	return resumed
}

// channelHealthReporter is the slice of a monitor's surface needed to
// cross-confirm a channel's reachability across sibling monitors.
type channelHealthReporter interface {
	Health() []monitor.ChannelHealth
}

// siblingReachable reports whether any sibling monitor RECENTLY reached the
// channel successfully. When true the channel is not "not responding" — another
// monitor is still seeing it, so its streams aren't being missed — and the
// failing monitor's unhealthy alert is a false positive to suppress. This is
// the guard against YouTube serving RSS 404/5xx during peak hours while the
// independent DECAPI monitor stays healthy. A sibling vouches only on a FRESH
// success (last check succeeded within crossMonitorVouchWindow); one that is
// itself failing, has never checked the channel, or has gone stale does not.
func siblingReachable(siblings []channelHealthReporter, channelID string, now time.Time) bool {
	for _, sib := range siblings {
		if sib == nil {
			continue
		}
		for _, h := range sib.Health() {
			if h.ChannelID != channelID {
				continue
			}
			if h.ConsecutiveErrors == 0 && h.LastCheckedAt != 0 &&
				now.Sub(time.UnixMilli(h.LastCheckedAt)) <= crossMonitorVouchWindow {
				return true
			}
		}
	}
	return false
}

// resumeOnRedetect decides what a live re-detection of an EXISTING job does.
// Only a Finished job with preserved resume data (incomplete_tail) AND
// staging files still on disk resumes; Cancelled is a human decision;
// everything else keeps today's silent drop. stagingExists mirrors the
// human-initiated /resume route's own gate (internal/web/routes/jobs.go,
// worker.HasStagingFiles against config.Paths.EffectiveStagingDir()) —
// staging can vanish between the Finished write and this re-detection
// (manual deletion, a reconfigured staging_dir), and resuming into an
// empty/missing staging dir would silently masquerade a fresh restart as
// an actual resume.
func resumeOnRedetect(existing *database.Job, disposition monitor.JobDisposition, stagingExists bool, lastAutoResume time.Time, now time.Time) bool {
	if existing == nil || disposition != monitor.DispositionBroadcast {
		return false
	}
	if existing.Status != database.StatusFinished || !existing.IncompleteTail {
		return false
	}
	if !stagingExists {
		return false
	}
	return now.Sub(lastAutoResume) >= 5*time.Minute
}

// jobCreationForDisposition maps a monitor.JobDisposition to the created
// job's initial state — spec §10's creator table:
//
//	Broadcast (live/upcoming) → Upcoming, queue_priority 0, enqueue now
//	NewVOD                    → Upcoming, queue_priority 0, enqueue now
//	BacklogVOD                → Queued,   queue_priority 1, NO enqueue (scheduler wake)
//
// Every creator writes queue_priority explicitly — the schema DEFAULT 1
// exists only for pre-v16 legacy rows and must never be relied on.
func jobCreationForDisposition(d monitor.JobDisposition) (status database.JobStatus, priority int, enqueueNow bool) {
	switch d {
	case monitor.DispositionBroadcast:
		return database.StatusUpcoming, 0, true
	case monitor.DispositionNewVOD:
		return database.StatusUpcoming, 0, true
	case monitor.DispositionBacklogVOD:
		return database.StatusQueued, 1, false
	default:
		// Unknown disposition: fail open to immediate admission — a wrongly
		// Queued job would rest until the scheduler noticed it; a wrongly
		// admitted job merely downloads early (today's behavior).
		return database.StatusUpcoming, 0, true
	}
}

// wireMonitorCallbacks installs every post-service-startup callback that
// connects the construction graph: cookie recovery / auth-recovered sweep,
// monitor ProbeVideo + OnVideoFound / OnStreamFound job-creation closures,
// monitor OnSchedule (broadcast-all-timers + TUI atomic dispatch), initial
// per-job log tracking, database OnJobUpdate / OnJobsChange subscribers
// (persisted on runState so shutdown can unsubscribe), the log-forwarder
// goroutine, and the connectivity OnStateChange wiring.
//
// Called once between wireRoutes() and the "start services" phase in run().
func (s *runState) wireMonitorCallbacks() {
	// Cooldown for auth-recovery failure notifications: OnRecoveryNeeded can
	// re-fire on every periodic auth check while cookies stay dead, and a
	// broken refresh should page the operator once per window, not per poll.
	// Guarded by a mutex — the recovery attempt runs on its own goroutine.
	var authNotifyMu sync.Mutex
	lastAuthFailNotify := make(map[string]time.Time)
	notifyAuthFailure := func(platform, title, desc string, ntype notifications.NotificationType) {
		authNotifyMu.Lock()
		defer authNotifyMu.Unlock()
		if time.Since(lastAuthFailNotify[platform]) < 30*time.Minute {
			return
		}
		lastAuthFailNotify[platform] = time.Now()
		s.notifyMgr.Send(title, desc, ntype,
			[]notifications.Field{{Name: "Platform", Value: platform, Inline: true}},
			notifications.SendOptions{Event: "auth"},
		)
	}

	// Cooldown for auto-resume on broadcast re-detection: a restarted
	// broadcast can be re-detected on every monitor cycle (as often as
	// every 15s per the field case that motivated this) while a previous
	// resume is still spinning up — coalesce to at most one auto-resume
	// attempt per window per job. Guarded by a mutex — createYouTubeJob
	// runs on the feed/DECAPI monitor goroutines. Same pattern as
	// lastAuthFailNotify above.
	var resumeMu sync.Mutex
	lastAutoResume := make(map[string]time.Time)

	s.cookieRefresh.OnRecoveryNeeded = func(platform string) {
		var autoEnabled bool
		s.configStore.Read(func(c *config.MoomboxConfig) {
			autoEnabled = c.Cookies.AutoEnabled
		})
		if !autoEnabled {
			s.log.Debug("Auth lost but auto-cookies disabled, skipping recovery", "platform", platform)
			return
		}
		// See notifyAuthFailure bodies below: the guidance leads with the
		// cookie FILE rather than the Settings wizard, because the wizard
		// drives a local browser and its endpoints are loopback-gated — it
		// is unreachable from a container and from a remote dashboard, which
		// is exactly where this notification is most likely to be read.
		s.log.Warn("Auth lost, attempting auto-cookie recovery", "platform", platform)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("auto-cookie recovery panic", "panic", r)
				}
			}()
			refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer refreshCancel()
			ok, err := s.autoCookieSvc.RefreshCookies(refreshCtx)
			if err != nil {
				s.log.Error("auto-cookie recovery failed", "platform", platform, "err", err)
				// Previously log-only: the operator learned cookies were dead
				// only when a recording actually failed. 30-min per-platform
				// cooldown via notifyAuthFailure.
				notifyAuthFailure(platform, "Cookie Auto-Refresh Failed",
					fmt.Sprintf("Automatic cookie refresh for %s failed — recordings will fail until the cookies are replaced. Export a fresh Netscape cookies.txt from a browser signed in to the account and overwrite the file at %s. (Export from a private window and close it: browsing on in the source profile rotates the session and invalidates the export.) The interactive browser login in Settings is an alternative only on the machine hosting Moombox.", platform, s.cookieFilePath()),
					notifications.TypeError)
			} else if ok {
				s.log.Info("auto-cookie recovery succeeded", "platform", platform)
				// Re-check auth status immediately so the UI updates
				s.cookieRefresh.CheckNow(context.Background())
			} else {
				s.log.Warn("auto-cookie recovery did not restore auth", "platform", platform)
				// States no cause, for the same reason the equivalent log line
				// in services.go states none: this fires from every
				// (false, nil) return of RefreshCookies, and most of those
				// mean it DECLINED to run (setup in progress, a refresh
				// already running, no platforms configured) with the session
				// possibly perfectly healthy. A notification is more visible
				// than a log line, so an assertion here is worse, not better.
				notifyAuthFailure(platform, "Cookie Auto-Refresh Ineffective",
					fmt.Sprintf("Automatic cookie refresh did not restore %s authentication — it either declined to run or found nothing usable (the log at debug level says which). If the cookies have in fact expired, replace %s with a fresh Netscape export from a browser signed in to the account; the interactive browser login in Settings is an alternative only on the machine hosting Moombox.", platform, s.cookieFilePath()),
					notifications.TypeWarning)
			}
		}()
	}

	// When a platform transitions from not-authenticated to authenticated,
	// sweep the jobs parked in StatusCookies on that platform back to Upcoming
	// so they get re-probed without manual intervention. Closes audit
	// decision #23 (worker.md Q3).
	//
	// "the jobs", not "every job": sweepShouldResume holds back the
	// membership-parked ones, whose session was already authenticated when
	// they failed and which this transition therefore cannot fix.
	s.cookieRefresh.OnAuthRecovered = func(platform string) {
		resumed := resumeCookieParkedJobs(s.db, s.log, platform, "")
		if resumed > 0 {
			s.log.Info("auth recovered — resumed COOKIES? jobs", "platform", platform, "count", resumed)
			// Event "auth" pairs with the worker's "Authentication Required"
			// emit — an empty Event would bypass every target's allowlist
			// (unfilterable) since the filter only applies when Event != "".
			s.notifyMgr.Send("Authentication Recovered",
				fmt.Sprintf("Resumed %d job(s) waiting on %s cookies", resumed, platform),
				notifications.TypeInfo,
				[]notifications.Field{
					{Name: "Platform", Value: platform, Inline: true},
					{Name: "Jobs", Value: fmt.Sprintf("%d", resumed), Inline: true},
				},
				notifications.SendOptions{Event: "auth"},
			)
		}
	}

	// When the cookie file starts holding a DIFFERENT, working account, sweep
	// the membership-parked jobs too — this is the one event that can fix
	// them, and it is invisible to OnAuthRecovered above because those jobs
	// parked while auth was perfectly healthy.
	//
	// Dead-cookie parks are eligible here as well. In the common case
	// OnAuthRecovered already took them (a swap that also restores auth fires
	// both), and resumeCookieParkedJobs is idempotent, so whichever runs
	// second simply finds nothing left. Being permissive costs nothing and
	// covers the swap-while-healthy case for them too.
	s.cookieRefresh.OnCredentialsChanged = func(platform, identity string) {
		resumed := resumeCookieParkedJobs(s.db, s.log, platform, identity)
		if resumed > 0 {
			s.log.Info("credentials changed — resumed COOKIES? jobs", "platform", platform, "count", resumed)
			// Same "auth" event as the recovery notification above, for the
			// same reason: an empty Event bypasses every target's allowlist.
			s.notifyMgr.Send("Credentials Changed",
				fmt.Sprintf("A different %s account was supplied — resumed %d parked job(s), including any waiting on a channel membership", platform, resumed),
				notifications.TypeInfo,
				[]notifications.Field{
					{Name: "Platform", Value: platform, Inline: true},
					{Name: "Jobs", Value: fmt.Sprintf("%d", resumed), Inline: true},
				},
				notifications.SendOptions{Event: "auth"},
			)
		}
	}

	// ProbeVideo callback for monitors (metadata check before job creation).
	// Uses the caller-supplied ctx so monitor shutdown cancels in-flight
	// probes (per audit reports/cross-cutting.md C4).
	probeVideoFunc := func(ctx context.Context, videoID string) (*monitor.VideoProbeResult, error) {
		meta, err := s.ytService.ProbeVideoStatus(ctx, videoID)
		if err != nil {
			return nil, err
		}
		return &monitor.VideoProbeResult{
			StreamStatus:       string(meta.StreamStatus),
			Title:              meta.Title,
			ChannelName:        meta.ChannelName,
			PublishedAt:        meta.PublishedAt,
			PublishedPrecision: meta.PublishedPrecision,
			PlayabilityError:   string(meta.PlayabilityError),
		}, nil
	}
	s.feedMon.ProbeVideo = probeVideoFunc
	s.decapiMon.ProbeVideo = probeVideoFunc

	// Date-completing fetch for the two-phase probe (§9): the ANDROID_VR/TV
	// status probes carry no microformat, so vod-family results arrive
	// dateless; both monitors call this (one anonymous WEB player fetch)
	// when a date is actually needed for a window decision.
	probeDateFunc := func(ctx context.Context, videoID string) (string, string, error) {
		return s.ytService.ProbeVideoDate(ctx, videoID)
	}
	s.feedMon.ProbeDate = probeDateFunc
	s.decapiMon.ProbeDate = probeDateFunc

	// Authenticated probe for members-only videos: an anonymous probe can't see
	// members-only content, gets no formats, and misclassifies it as "upcoming"
	// (which bypasses include_non_live_content). The TV_DOWNGRADED+cookies probe
	// classifies it correctly (vod/live/upcoming). Only the feed monitor's
	// membership path uses it; RSS/DECAPI stay on the anonymous probe.
	s.feedMon.ProbeVideoAuth = func(ctx context.Context, videoID string) (*monitor.VideoProbeResult, error) {
		meta, err := s.ytService.ProbeVideoStatusAuthenticated(ctx, videoID)
		if err != nil {
			return nil, err
		}
		return &monitor.VideoProbeResult{
			StreamStatus:       string(meta.StreamStatus),
			Title:              meta.Title,
			ChannelName:        meta.ChannelName,
			PublishedAt:        meta.PublishedAt,
			PublishedPrecision: meta.PublishedPrecision,
			PlayabilityError:   string(meta.PlayabilityError),
		}, nil
	}

	// Membership discovery: authenticated /membership tab scan for members-only
	// videos the RSS feed never lists. Wired on the feed monitor only. The
	// closure adapts youtube.MembershipVideo -> monitor.MembershipVideo (keeping
	// the monitor package decoupled from youtube, like probeVideoFunc does for
	// VideoInfo). MembershipEnabled re-reads the config flag AND cookie state
	// live each cycle, so toggling the setting or acquiring cookies takes effect
	// on the next cycle with no restart.
	s.feedMon.FetchMembership = func(ctx context.Context, channelID string) ([]monitor.MembershipVideo, error) {
		vids, err := s.ytService.FetchMembershipVideos(ctx, channelID)
		if err != nil {
			return nil, err
		}
		out := make([]monitor.MembershipVideo, len(vids))
		for i, v := range vids {
			out[i] = monitor.MembershipVideo{VideoID: v.VideoID, Title: v.Title, Age: v.Age}
		}
		return out, nil
	}
	s.feedMon.MembershipEnabled = func() bool {
		enabled := true
		s.configStore.Read(func(c *config.MoomboxConfig) {
			enabled = c.Monitors.MembershipDiscoveryEnabled()
		})
		return enabled && s.ytService.HasAuthCookies()
	}

	// createYouTubeJob creates a YouTube job per the disposition's creation
	// semantics (spec §10's creator table, via jobCreationForDisposition).
	// Stream-status classification is handled by the monitors via
	// ProcessYouTubeVideo.
	createYouTubeJob := func(videoID, title, videoURL string, ch *config.ChannelConfig, source string, d monitor.JobDisposition) {
		s.log.Info("Video found", slog.String("source", source), slog.String("videoID", videoID),
			slog.String("title", title), slog.String("disposition", d.String()))

		includeNonLive := ch.IncludeNonLiveContent
		outputDir := resolveOutputDir(ch, s.configStore)
		thumbnailURL := youtubeThumbnailURL(videoID)

		status, priority, enqueueNow := jobCreationForDisposition(d)
		// The feed affiliation the scheduler groups by. Copied so the row
		// never aliases live config memory; an empty ID (defensive — feed
		// channels always carry one) stays nil and therefore stores NULL.
		var channelID *string
		if ch.ID != "" {
			id := ch.ID
			channelID = &id
		}

		now := time.Now().UTC().Format(time.RFC3339)
		job := &database.Job{
			ID:                videoID,
			VideoID:           videoID,
			URL:               videoURL,
			Title:             title,
			ChannelName:       ch.Name,
			Platform:          "youtube",
			Status:            status,
			ThumbnailURL:      thumbnailURL,
			OutputDirectory:   outputDir,
			AllowNonStream:    includeNonLive,
			QualityPreference: ch.QualityPreference,
			ChannelID:         channelID,
			QueuePriority:     priority,
			CreatedAt:         now,
			UpdatedAt:         now,
		}

		added, err := s.db.AddJob(job)
		if err != nil {
			s.log.Error("Failed to add YouTube job", slog.String("error", err.Error()))
			return
		}
		if !added {
			// Duplicate job — but a broadcast that dropped and came back
			// under the SAME video ID (interrupted stream, re-detected
			// live) may be sitting Finished with preserved staging
			// (incomplete_tail). Re-arm exactly that case; everything
			// else (still active, Cancelled, no preserved tail, VOD
			// re-detection, or within the cooldown of a prior auto-resume)
			// stays a silent drop, unchanged from today.
			existing, _ := s.db.GetJob(videoID)

			// The cheap in-memory half of resumeOnRedetect's guard,
			// duplicated only to decide whether the staging-existence
			// disk check below is worth paying for. This branch runs on
			// EVERY re-detection of an already-known job — including a
			// still-live job re-polled every monitor cycle — so gating
			// the stat avoids hitting disk on that hot path. This can't
			// loosen the real decision: resumeOnRedetect independently
			// re-checks every one of these conditions itself.
			eligible := existing != nil && d == monitor.DispositionBroadcast &&
				existing.Status == database.StatusFinished && existing.IncompleteTail

			var stagingExists bool
			if eligible {
				// Same gate + same stagingBase resolution as the human
				// /resume route (internal/web/routes/jobs.go) — a job
				// whose preserved staging vanished (manual deletion, a
				// reconfigured staging_dir) must not masquerade a fresh
				// empty-staging restart as a resume.
				var stagingBase string
				s.configStore.Read(func(c *config.MoomboxConfig) {
					stagingBase = c.Paths.EffectiveStagingDir()
				})
				stagingExists = worker.HasStagingFiles(stagingBase, videoID)
			}

			resumeMu.Lock()
			shouldResume := resumeOnRedetect(existing, d, stagingExists, lastAutoResume[videoID], time.Now())
			if shouldResume {
				lastAutoResume[videoID] = time.Now()
			}
			resumeMu.Unlock()
			if !shouldResume {
				if eligible && !stagingExists {
					s.log.Debug("broadcast re-detected live but preserved staging is gone — skipping auto-resume, use Reinitialize",
						slog.String("videoID", videoID))
				}
				return
			}
			if title != "" && title != existing.Title {
				s.db.UpdateJobFields(videoID, map[string]any{"title": title})
			}
			s.dlWorker.ResumeJob(videoID)
			// No notification here: orchestrator.go sends "YouTube Download
			// Starting" unconditionally on every ExecuteWithChat entry —
			// including this resume — so a second send here would just be
			// a duplicate back-to-back message for the same event. This
			// INFO log is the auto-resume-specific record for the operator.
			s.log.Info("broadcast re-detected live — auto-resuming preserved job",
				slog.String("source", source), slog.String("videoID", videoID), slog.String("title", title))
			return
		}
		// History fires for EVERY disposition — it is what makes
		// HasProcessed mean "a job was created" (spec §10/§15).
		s.db.AddToHistory(videoID)
		if enqueueNow {
			s.dlWorker.EnqueueJob(videoID)
		} else {
			// Backlog VOD: rests in Queued until the archive-slots
			// scheduler admits it (M per channel). Wake so a running
			// scheduler sweeps now instead of on its next heartbeat.
			s.dlWorker.Scheduler().Wake()
		}
		// AddJob's OnJobAdded handler (wired below) handles the WS
		// broadcast for the new job; no explicit BroadcastJobsUpdate
		// needed here. DECISIONS #21 consumer migration.
		if s.notifyMgr.HasTargets() {
			s.notifyMgr.Send("Stream Found",
				fmt.Sprintf("Found matching stream: %s", title),
				notifications.TypeInfo,
				[]notifications.Field{
					{Name: "Channel", Value: ch.Name, Inline: true},
					{Name: "Video ID", Value: videoID, Inline: true},
				},
				notifications.SendOptions{
					Event:     "found",
					URL:       videoURL,
					Thumbnail: youtubeThumbnailURL(videoID),
				})
		}
	}

	// Monitor -> Worker: create jobs for found videos. Panic recovery
	// prevents a single bad callback from killing the monitor goroutine.
	// The JobDisposition drives the creation semantics — see
	// jobCreationForDisposition for spec §10's creator table.
	s.feedMon.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig, d monitor.JobDisposition) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("Panic in OnVideoFound (feed)", slog.Any("panic", r))
			}
		}()
		createYouTubeJob(videoID, title, url, ch, "feed", d)
	}
	s.decapiMon.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig, d monitor.JobDisposition) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("Panic in OnVideoFound (decapi)", slog.Any("panic", r))
			}
		}()
		createYouTubeJob(videoID, title, url, ch, "decapi", d)
	}
	s.twitchMon.OnStreamFound = func(info *twitch.TwitchStreamInfo, ch *config.ChannelConfig) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("Panic in OnStreamFound (twitch)", slog.Any("panic", r))
			}
		}()
		jobID := twitch.BuildJobID(info.StreamID, false)
		s.log.Info("Stream found by Twitch monitor", slog.String("jobID", jobID), slog.String("title", info.Title))

		outputDir := resolveOutputDir(ch, s.configStore)

		now := time.Now().UTC().Format(time.RFC3339)
		title := info.ChannelDisplayName + " — " + info.Title
		if info.Title == "" {
			title = info.ChannelDisplayName + " — " + time.Now().UTC().Format(time.RFC3339)
		}

		job := &database.Job{
			ID:                jobID,
			VideoID:           info.StreamID,
			URL:               "https://twitch.tv/" + info.ChannelLogin,
			Title:             title,
			ChannelName:       info.ChannelDisplayName,
			Platform:          "twitch",
			Status:            database.StatusLive, // Twitch: immediately Live (confirmed by GQL)
			ThumbnailURL:      info.ThumbnailURL,
			ChannelAvatarURL:  info.ProfileImageURL,
			TwitchCategory:    info.GameCategory,
			TwitchQuality:     ch.QualityPreference,
			QualityPreference: ch.QualityPreference,
			StreamStartTime:   info.StartedAt,
			OutputDirectory:   outputDir,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		added, err := s.db.AddJob(job)
		if err != nil {
			s.log.Error("Failed to add Twitch job", slog.String("error", err.Error()))
			return
		}
		if !added {
			return // Duplicate job
		}
		// Stash the monitor's fresh streamInfo for processTwitchLive to consume,
		// so it doesn't immediately re-query Twitch GQL (which has been observed
		// to return Stream=nil for ~1s after StreamMetadata reports a stream as
		// live, manifesting as a false "twitch channel is offline" error).
		s.dlWorker.StashTwitchStreamInfo(jobID, info)
		s.db.AddToHistory(jobID)
		s.dlWorker.EnqueueJob(jobID)
		// Same as the YouTube path — AddJob's OnJobAdded handler
		// broadcasts the new job; no explicit BroadcastJobsUpdate
		// needed. DECISIONS #21 consumer migration.
		if s.notifyMgr.HasTargets() {
			twitchFields := []notifications.Field{
				{Name: "Channel", Value: info.ChannelDisplayName, Inline: true},
				{Name: "Stream ID", Value: info.StreamID, Inline: true},
			}
			if info.GameCategory != "" {
				twitchFields = append(twitchFields, notifications.Field{
					Name: "Category", Value: info.GameCategory, Inline: true,
				})
			}
			s.notifyMgr.Send("Twitch Stream Found",
				fmt.Sprintf("Live: %s", title),
				notifications.TypeInfo,
				twitchFields,
				notifications.SendOptions{
					Event:     "found",
					URL:       "https://twitch.tv/" + info.ChannelLogin,
					Thumbnail: info.ThumbnailURL,
				})
		}
	}

	s.twitchMon.OnStreamRecover = func(info *twitch.TwitchStreamInfo, ch *config.ChannelConfig, jobID string) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("Panic in OnStreamRecover (twitch)", slog.Any("panic", r))
			}
		}()

		// Stash the fresh streamInfo so processTwitchLive consumes it instead
		// of re-querying GQL — same flap-prevention as the OnStreamFound path.
		s.dlWorker.StashTwitchStreamInfo(jobID, info)

		// AutoReinitializeJob increments auto_retry_count, clears state, and
		// re-enqueues. The cap (worker.MaxTwitchAutoRetries) is enforced by
		// the monitor's predicate before we even get here.
		s.dlWorker.AutoReinitializeJob(jobID)

		s.log.Info("auto-recovered twitch job",
			slog.String("jobID", jobID),
			slog.String("channel", info.ChannelDisplayName),
			slog.String("streamID", info.StreamID))
	}

	// Backfill worker -> UIs: progress surfacing (spec §11), modeled on the
	// disk_status pipeline in main.go — generic hub.Broadcast for web
	// clients, non-blocking channel push for the TUI — plus the snapshot
	// write InitialState and the TUI seed read (disk_status keeps its
	// snapshot in routes.SharedDiskStatus; backfill's lives on runState).
	// The worker never imports web/tui; this closure is the seam.
	s.backfillWorker.OnProgress = func(chID, tab string, pages int, state string) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("Panic in backfill OnProgress", slog.Any("panic", r))
			}
		}()
		// Snapshot first, broadcast second, so a client connecting between
		// the two sees this state in InitialState rather than missing it.
		// Active states are stored; "done" and "idle" clear the entry (see
		// the backfillProgress field doc).
		s.backfillMu.Lock()
		switch state {
		case "scanning", "error":
			s.backfillProgress[chID] = backfillProgressState{Tab: tab, Pages: pages, State: state}
		default: // "done", "idle"
			delete(s.backfillProgress, chID)
		}
		s.backfillMu.Unlock()

		// Broadcast to web clients — the exact Task 5 payload shape.
		s.wsHub.Broadcast("backfill_status", map[string]any{
			"channel": chID,
			"tab":     tab,
			"pages":   pages,
			"state":   state,
		})

		// Push to TUI (drop-on-full like tuiDiskStatusCh — a dropped page
		// tick is superseded by the next one within a second).
		select {
		case s.tuiBackfillCh <- tui.BackfillStatusMsg{Channel: chID, Tab: tab, Pages: pages, State: state}:
		default:
		}
	}

	// Monitor -> WebSocket: broadcast timer updates. TypeScript broadcasts
	// ALL three monitor times on each schedule event so we do the same —
	// read all three monitors' next check times.
	broadcastAllTimers := func() {
		s.wsHub.BroadcastCheckTimers(map[string]any{
			"nextFeedCheck":   s.feedMon.GetNextCheckAt(),
			"nextDecapiCheck": s.decapiMon.GetNextCheckAt(),
			"nextTwitchCheck": s.twitchMon.GetNextCheckAt(),
		})
	}

	// OnSchedule subscriber slots live on runState (atomic.Pointer fields).
	// Set the dispatchers once before monitor.Start(); the TUI wiring later
	// Store()s concrete funcs into the atomic slots. Reassigning the
	// monitor's plain func field while its goroutine is running would race
	// with scheduleNext() reading it; atomic.Pointer keeps the read-side
	// lock-free and the write-side safe.
	s.feedMon.OnSchedule = func(next int64) {
		broadcastAllTimers()
		if fn := s.feedTUISchedule.Load(); fn != nil {
			(*fn)(next)
		}
	}
	s.decapiMon.OnSchedule = func(next int64) {
		broadcastAllTimers()
		if fn := s.decapiTUISchedule.Load(); fn != nil {
			(*fn)(next)
		}
	}
	s.twitchMon.OnSchedule = func(next int64) {
		broadcastAllTimers()
		if fn := s.twitchTUISchedule.Load(); fn != nil {
			(*fn)(next)
		}
	}

	// Channel-health notifications: a channel that fails every check for a
	// sustained streak (renamed/banned Twitch login, dead YouTube channel,
	// 404 RSS) previously rotted at Debug level until a stream was missed.
	// One notification per streak, per monitor; the /api/status
	// channelHealth surface shows the live state. platform label is set per
	// monitor so the operator knows which source flagged it.
	unhealthyNotify := func(platform string, siblings ...channelHealthReporter) func(channelID string, consecutive int, lastErr string) {
		return func(channelID string, consecutive int, lastErr string) {
			// Cross-monitor confirmation: a channel is only "not responding" if
			// EVERY monitor covering it has lost it. YouTube serves RSS 404/5xx
			// during peak hours while the independent DECAPI monitor keeps
			// working, so a lone feed-monitor failure is a false positive — its
			// streams are still being seen. Suppress unless no sibling vouches.
			if siblingReachable(siblings, channelID, time.Now()) {
				s.log.Info("channel unhealthy on one monitor but still reachable via another — suppressing alert",
					"platform", platform, "channel", channelID, "consecutive", consecutive, "err", lastErr)
				return
			}
			s.log.Warn("channel failing monitor checks — verify it still exists",
				"platform", platform, "channel", channelID, "consecutive", consecutive, "err", lastErr)
			s.notifyMgr.Send("Channel Not Responding",
				fmt.Sprintf("A %s channel has failed %d consecutive monitor checks — it may be renamed, banned, or misconfigured, and its streams are being missed", platform, consecutive),
				notifications.TypeWarning,
				[]notifications.Field{
					{Name: "Channel", Value: channelID, Inline: true},
					{Name: "Platform", Value: platform, Inline: true},
					{Name: "Last Error", Value: lastErr},
				},
				notifications.SendOptions{Event: "channel_unhealthy"},
			)
		}
	}
	// YouTube channels are covered by both the RSS feed and DECAPI monitors, so
	// each cross-confirms against the other before alerting. Twitch has a single
	// (reliable GQL) monitor with no sibling to confirm against.
	s.feedMon.SetOnChannelUnhealthy(unhealthyNotify("youtube", s.decapiMon))
	s.decapiMon.SetOnChannelUnhealthy(unhealthyNotify("youtube", s.feedMon))
	s.twitchMon.SetOnChannelUnhealthy(unhealthyNotify("twitch"))

	// Initialize per-job log tracking with existing jobs (matches TS knownJobIds)
	if existingJobs, err := s.db.GetAllJobs(); err == nil {
		for _, j := range existingJobs {
			s.db.TrackJobForLogs(j.ID)
		}
	}

	// Database -> WebSocket: broadcast job updates. Uses the
	// fine-grained OnJobChange API. silentColumns (resume_position,
	// chat_offset) bypass OnJobChange entirely at the writer side,
	// so player-state scrubs don't reach this subscriber.
	//
	// No per-job throttle here: progress writes are already capped to
	// ~60Hz/job upstream by ProgressTracker.maybeUpdate (16ms gate in
	// internal/worker/progress.go), and every other UpdateJobFields
	// caller is event-driven (state transitions, not loops).
	s.unsubWSJobUpdate = s.db.OnJobChange(func(ev *database.JobChange) {
		job := ev.Job
		// Skip broadcasting updates for archived (old finished) jobs — same
		// classification as the list filter, via the shared jobArchivedAt
		// predicate so the two can never disagree about which jobs are
		// archived.
		if job.Status == database.StatusFinished && job.UpdatedAt != "" {
			var hideAgeDays float64
			s.configStore.Read(func(c *config.MoomboxConfig) {
				hideAgeDays = c.Monitors.HideFinishedAgeDays.Value
			})
			if hideAgeDays >= 0 {
				cutoff := time.Now().Add(-time.Duration(hideAgeDays*24) * time.Hour)
				if jobArchivedAt(job, cutoff) {
					return
				}
			}
		}
		s.wsHub.BroadcastJobUpdate(job)
	})

	// OnJobAdded subscriber: AddJob no longer fires OnJobsChange (the
	// writer-side dispatch was dropped as part of DECISIONS #21
	// consumer migration). The new-job broadcast goes through
	// BroadcastJobUpdate — frontend's "job_update" handler already
	// has a "job not in array yet — add it and re-render" branch
	// (web/public/app.js around line 1021), so a singular update
	// for an unknown ID is the right wire shape. We also do the
	// per-job log tracking here that the OnJobsChange handler used
	// to do for ALL jobs on every fan-out.
	s.unsubWSJobAdded = s.db.OnJobAdded(func(ev *database.JobAdded) {
		job := ev.Job
		s.db.TrackJobForLogs(job.ID)
		s.wsHub.BroadcastJobUpdate(job)
	})

	// OnTrimsChanged subscriber: AddTrim/DeleteTrim no longer fire
	// OnJobsChange (writer-side dispatch dropped per DECISIONS #21).
	// Re-fetch the affected job (so its Trims field reflects the
	// current SQLite state) and broadcast it through BroadcastJobUpdate
	// — frontend's existing job_update handler replaces the in-memory
	// job and re-renders, picking up the new trim list naturally.
	s.unsubWSTrimsChanged = s.db.OnTrimsChanged(func(ev *database.TrimsChanged) {
		job, err := s.db.GetJob(ev.JobID)
		if err != nil || job == nil {
			return
		}
		s.wsHub.BroadcastJobUpdate(job)
	})

	// OnJobDeleted subscriber: send a targeted job_deleted WS event so the
	// frontend drops the row immediately. This replaces the prior full-list
	// rebroadcast (jobs_update) which raced against the preceding
	// status=Cancelled job_update and left stale rows visible in the UI.
	// Per-job log buffers are pruned via the active-IDs set derived from the
	// post-delete snapshot (the deleted ID drops out naturally).
	s.unsubWSJobDeleted = s.db.OnJobDeleted(func(ev *database.JobDeleted) {
		jobs := getAllJobsSafe(s.db)
		activeIDs := make(map[string]struct{}, len(jobs))
		for _, j := range jobs {
			activeIDs[j.ID] = struct{}{}
		}
		// Only the DATABASE per-job log pipeline is live (RouteLogToJobs);
		// the logger's parallel buffers are unwired and permanently empty.
		s.db.PruneJobLogs(activeIDs)
		s.wsHub.BroadcastJobDeleted(ev.JobID)
	})

	s.unsubWSJobsChange = s.db.OnJobsChange(func(jobs []*database.Job) {
		// Keep per-job log tracking in sync (matches TS knownJobIds update)
		activeIDs := make(map[string]struct{}, len(jobs))
		for _, j := range jobs {
			activeIDs[j.ID] = struct{}{}
			s.db.TrackJobForLogs(j.ID)
		}
		s.db.PruneJobLogs(activeIDs)
		s.wsHub.BroadcastJobsUpdate(filterJobsByAge(jobs, s.configStore))
	})

	// Logger -> WebSocket: broadcast log lines + route to per-job buffers
	s.logSub = s.log.Subscribe()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("log forwarder panic", "panic", r)
			}
		}()
		for line := range s.logSub {
			s.wsHub.BroadcastLog(line)
			s.db.RouteLogToJobs(line) // Route to per-job buffer (matches TS knownJobIds log routing)
		}
	}()

	// Connectivity -> monitors + WebSocket: kick monitors on reconnect,
	// broadcast state — and notify. The web/TUI broadcasts are ephemeral;
	// an operator who was away during an outage previously had NO trace
	// that monitoring was down (streams silently missed). Transitions are
	// already debounced by the connectivity monitor, so this can't flap-spam.
	//
	// ONE notification, on restore only: a "lost" webhook has, by
	// definition, no connectivity to ride — it either dies in the sender's
	// bounded retries or lands late and out of order, so the offline
	// transition just stamps outageStart and the restore sends the whole
	// story (start, end, duration) as a single Outage Alert.
	// outageStart is guarded by a mutex — OnStateChange serializes today,
	// but this closure must not silently depend on that.
	var connNotifyMu sync.Mutex
	var outageStart time.Time
	s.connMon.OnStateChange(func(online bool) {
		if online {
			s.feedMon.CheckNow()
			s.decapiMon.CheckNow()
			s.twitchMon.CheckNow()
		}
		s.wsHub.BroadcastConnectivity(online)

		connNotifyMu.Lock()
		defer connNotifyMu.Unlock()
		if !online {
			outageStart = time.Now()
			return
		}
		if outageStart.IsZero() {
			return // startup/initial online state — nothing to report
		}
		title, desc, fields := outageAlert(outageStart, time.Now())
		outageStart = time.Time{}
		// Event key stays "connectivity_restored": the alert still fires on
		// the restore transition, and existing target allowlists must keep
		// working (the UIs relabel the toggle "Outage alert").
		s.notifyMgr.Send(title, desc, notifications.TypeWarning, fields,
			notifications.SendOptions{Event: "connectivity_restored"},
		)
	})
}

// outageAlert builds the Outage Alert notification for a connectivity
// outage spanning [start, end]: title, description, and the three embed
// fields. Start/end render as Discord dynamic timestamps (<t:unix:f> —
// Discord shows each viewer's local timezone; Discord webhooks are the only
// notification target type, so the markup never reaches a renderer that
// can't display it). Duration is a plain second-rounded string.
func outageAlert(start, end time.Time) (title, description string, fields []notifications.Field) {
	return "Outage Alert",
		"Internet connectivity was lost — channel monitoring paused and downloads waited; monitors are re-checking now",
		[]notifications.Field{
			{Name: "Started", Value: fmt.Sprintf("<t:%d:f>", start.Unix()), Inline: true},
			{Name: "Ended", Value: fmt.Sprintf("<t:%d:f>", end.Unix()), Inline: true},
			{Name: "Duration", Value: end.Sub(start).Round(time.Second).String(), Inline: true},
		}
}
