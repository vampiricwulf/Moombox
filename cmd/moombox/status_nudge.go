package main

import "github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"

// statusChangedFrame is the WebSocket frame that tells an open dashboard its
// status has changed: cookie authentication, a re-login flag or the BotGuard
// sidecar's health. It carries no payload (owner ruling R2-D1). The tab
// answers by reading GET /api/status again, so the cookie badge stays a
// per-request surface: every word it shows comes from a fetch the tab made,
// never from a pushed copy (docs/spec/user-interfaces.md, the cookie status
// surfaces).
const statusChangedFrame = "status_changed"

// nudgeDashboardStatus broadcasts statusChangedFrame. It is called from the
// goroutine that made the change (a cookie check, a refresh, the sidecar
// supervisor) and does not block: the hub queues per client. A burst is
// coalesced by the tab, which keeps one status read in flight.
func (s *runState) nudgeDashboardStatus() {
	if s.wsHub == nil {
		return
	}
	s.wsHub.Broadcast(statusChangedFrame, nil)
}

// reloginChanged tells both UIs that a re-login flag was raised or lowered
// (AutoCookieService.OnReloginChange). The flag is laid over the cookie badge
// in each, and a change to it need not come with an auth change: a refresh
// that fails verification on a platform that was already signed out raises it
// and the re-check that follows finds nothing different. The dashboard is
// nudged to read its status again; the TUI's badges are sent again, which
// does not block and is a no-op without a TUI.
func (s *runState) reloginChanged() {
	s.nudgeDashboardStatus()
	s.resendTUICookieStatus()
}

// wireSidecarStatusNudge nudges the dashboards on every sidecar health
// publish and returns the unsubscribe. /api/status serves the health's
// healthy, reason and restarts, and each publish changes one of them.
// SubscribeHealth calls back at once with the current snapshot; that first
// nudge reaches whoever is connected and costs one status read.
func (s *runState) wireSidecarStatusNudge() func() {
	return sidecar.SubscribeHealth(func(sidecar.Health) { s.nudgeDashboardStatus() })
}
