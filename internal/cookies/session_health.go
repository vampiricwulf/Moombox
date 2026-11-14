package cookies

// session_health.go — the derived session health (ok | flickering | relogin),
// the page facts it is derived from, and what a recovery is told about why it
// was asked for (owner ruling K4, 2026-11-13).

import "time"

// SessionHealth is what the API check and the page verdicts say together
// about one platform's stored session. Derived on every read by
// SessionHealthOf and never persisted (CLAUDE.md State machines rules 1, 2
// and 5).
type SessionHealth string

const (
	// SessionOK: nothing to report.
	SessionOK SessionHealth = "ok"
	// SessionFlickering: a page rendered signed out within the last
	// livenessQuietReset while the API check has not failed. Moombox keeps
	// working and retries on the recovery back-off; the operator has nothing
	// to do.
	SessionFlickering SessionHealth = "flickering"
	// SessionRelogin: the API check concluded the stored cookies are refused.
	SessionRelogin SessionHealth = "relogin"
)

// PageHealth is what the signed-out page verdicts of the current back-off
// episode add up to for one platform. Timestamps and a count only. It lives in
// memory: after a restart it is empty until the next signed-out page.
type PageHealth struct {
	// LastSignedOut is when a page last rendered signed out. Zero when none
	// has since the episode began (process start, the 24 h reset, or an
	// operator's setup, import or manual refresh).
	LastSignedOut time.Time
	// Attempts counts the recoveries page verdicts have fired this episode.
	Attempts int
	// NextAttempt is the earliest a signed-out page can fire the next
	// recovery. Zero when no recovery has been decided. In the past means
	// the next signed-out page fires one.
	NextAttempt time.Time
}

// SessionHealthOf is the ONE predicate for a platform's session health. The
// TUI reads it through AuthStatus.SessionHealth; the dashboard's sessionHealth
// (web/public/modules/utils.js) mirrors it over the wire keys `found`,
// `verification` and `pageSignedOutAt`, and the two are compared over
// web/tests/fixtures/session-health-parity.json.
//
// found and verification are the platform's API check (AuthStatus);
// lastSignedOutPage is PageHealth.LastSignedOut.
func SessionHealthOf(found bool, verification RefreshVerdict, lastSignedOutPage, now time.Time) SessionHealth {
	if found && verification == RefreshFailed {
		return SessionRelogin
	}
	if !lastSignedOutPage.IsZero() && now.Sub(lastSignedOutPage) < livenessQuietReset {
		return SessionFlickering
	}
	return SessionOK
}

// SessionHealth is SessionHealthOf for one platform ("youtube" or "twitch")
// of this status; any other name reads ok.
func (s AuthStatus) SessionHealth(platform string, now time.Time) SessionHealth {
	switch platform {
	case "youtube":
		return SessionHealthOf(s.HasYouTubeCookies, s.YouTubeVerification, s.YouTubePages.LastSignedOut, now)
	case "twitch":
		return SessionHealthOf(s.HasTwitchCookies, s.TwitchVerification, s.TwitchPages.LastSignedOut, now)
	}
	return SessionOK
}

// RecoveryTrigger names what asked for a recovery.
type RecoveryTrigger string

const (
	// RecoveryTriggerAuthCheck: the API check failed, or the credentials were
	// refused where they were used (tier 1).
	RecoveryTriggerAuthCheck RecoveryTrigger = "auth_check"
	// RecoveryTriggerPage: a page rendered signed out (tier 2, the liveness
	// observations). The API check may well still pass.
	RecoveryTriggerPage RecoveryTrigger = "page"
)

// RecoveryCause is what OnRecoveryNeeded is told beside the platform.
type RecoveryCause struct {
	Trigger RecoveryTrigger
	// Attempt is the number of this page-triggered recovery within its
	// back-off episode, from 1. Zero for RecoveryTriggerAuthCheck.
	Attempt int
	// NextWindow is how long must pass before a signed-out page may fire the
	// next one. Zero for RecoveryTriggerAuthCheck.
	NextWindow time.Duration
}

// pageEpisode is one platform's entry in RefreshService.pageEpisodes.
type pageEpisode struct {
	// lastSignedOut is the last signed-out page verdict.
	lastSignedOut time.Time
	// run counts the signed-out verdicts, at least livenessRunSpacing apart,
	// since the later of the last signed-in verdict and the last recovery a
	// page verdict fired; runLast is the last one counted. deadFired is set
	// once the dead-session rule has fired, or tier 1 has decided a recovery,
	// during the current stretch of signed-out verdicts, and cleared by the
	// signed-in verdict that ends it.
	run       int
	runLast   time.Time
	deadFired bool
	// attempts counts the recoveries page verdicts fired this episode.
	attempts int
	// passAt is when the last page-triggered recovery pass verified, and
	// passWarned whether a signed-out page after it has been reported.
	passAt     time.Time
	passWarned bool
}

// pageHealthLocked projects one platform's episode. Callers hold rs.mu.
func (rs *RefreshService) pageHealthLocked(platform string) PageHealth {
	ep, ok := rs.pageEpisodes[platform]
	if !ok || ep.lastSignedOut.IsZero() {
		return PageHealth{}
	}
	h := PageHealth{LastSignedOut: ep.lastSignedOut, Attempts: ep.attempts}
	if last, decided := rs.lastRecoveryDecided[platform]; decided {
		h.NextAttempt = last.Add(rs.livenessRefireWindowFor(platform))
	}
	return h
}

// statusLocked is the status every reader gets: rs.status with both
// platforms' page facts laid in. The facts are not stored in rs.status
// because a refresh pass rebuilds that struct whole. Callers hold rs.mu.
func (rs *RefreshService) statusLocked() AuthStatus {
	s := rs.status
	s.YouTubePages = rs.pageHealthLocked("youtube")
	s.TwitchPages = rs.pageHealthLocked("twitch")
	return s
}

// pageHealthMoved reports whether a change of page facts is one the UIs draw:
// the flickering state at `now`, the attempt count, or the next attempt.
// LastSignedOut alone moves on every signed-out verdict (one per channel per
// feed cycle) and draws nothing new.
func pageHealthMoved(before, after PageHealth, now time.Time) bool {
	flickering := func(h PageHealth) bool {
		return SessionHealthOf(false, RefreshUnknown, h.LastSignedOut, now) == SessionFlickering
	}
	return flickering(before) != flickering(after) ||
		before.Attempts != after.Attempts ||
		!before.NextAttempt.Equal(after.NextAttempt)
}

// ClearRecoveryEscalation puts both platforms back on the base back-off and
// forgets their signed-out pages. It is for the operator's own gestures (a
// setup, an import, a manual refresh) and for nothing automatic: an
// automatic pass that verifies proves only that the API check passes, which
// it does throughout a flicker, so clearing on one would bring the 30-minute
// relaunch storm back (owner ruling K4).
//
// The dedupe stamp (lastRecoveryDecided) is kept, as resetLivenessRefire
// keeps it. Safe on a nil service.
func (rs *RefreshService) ClearRecoveryEscalation() {
	if rs == nil {
		return
	}
	now := time.Now()
	var (
		moved  bool
		status AuthStatus
	)
	func() {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		for _, platform := range []string{"youtube", "twitch"} {
			before := rs.pageHealthLocked(platform)
			rs.resetLivenessRefire(platform)
			moved = moved || pageHealthMoved(before, PageHealth{}, now)
		}
		status = rs.statusLocked()
	}()
	if moved && rs.OnAuthChange != nil {
		rs.OnAuthChange(status)
	}
}

// NotePageRecoveryPass records that a page-triggered recovery pass for
// `platform` ran and verified at `now`. The next signed-out page verdict
// after it is reported once, at Warn (ObserveLiveness), however many
// verdicts follow. Safe on a nil service.
func (rs *RefreshService) NotePageRecoveryPass(platform string, now time.Time) {
	if rs == nil {
		return
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.pageEpisodes == nil {
		rs.pageEpisodes = make(map[string]pageEpisode)
	}
	ep := rs.pageEpisodes[platform]
	ep.passAt, ep.passWarned = now, false
	rs.pageEpisodes[platform] = ep
}
