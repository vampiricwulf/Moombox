package routes

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/updater"
)

// updateCheckDebounce bounds how often POST /api/update/check actually asks
// GitHub. Each call spends one of the 60/h unauthenticated requests, so a held
// key or a script could exhaust the quota and suppress the daily auto-check
// for an hour — on a box whose YouTube/Twitch extractors rot without updates
// (WEB-12). Same window and same wire answer as /api/monitors/check-now. The
// SCHEDULED check is untouched: checkAndBroadcastUpdate (cmd/moombox/helpers.go)
// calls updater.CheckForUpdate directly and never travels this route.
const updateCheckDebounce = 30 * time.Second

// releaseVersionRe is the only shape /api/update/release-notes accepts.
// updater.FetchReleaseNotes interpolates the value into api.github.com's path
// (".../releases/tags/v"+version) with no escaping, so an unvalidated "?"
// ended the path and turned the rest into a query string. The host is fixed
// and the method is GET, so this is hygiene rather than a hole, and this keeps
// it that way. It admits every tag Moombox has published, with or without the
// leading "v" (the Web sends the bare version, the TUI sends the tag).
var releaseVersionRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// updateApplyOriginAllowed gates POST /api/update/apply to loopback callers.
// Replacing the binary + restarting is a high-impact action; the audit
// (reports/web.md S-8) explicitly wanted this tightened beyond the generic
// CSRFMiddleware so a LAN peer can't trigger it even when network_access
// is "lan" / "external". Returns true when the request originates from
// loopback (no Origin header set, or Origin host is 127.0.0.1 / ::1 /
// localhost).
func updateApplyOriginAllowed(r *http.Request) bool {
	// No Origin header on a mutating request is normally rejected by
	// CSRFMiddleware before this handler runs; if we got here, that's a
	// same-process caller (e.g. TUI presenting InternalToken).
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// SharedUpdateInfo is the package-level atomic pointer storing the latest
// available update info. Set by main.go, read by status route and update routes.
var SharedUpdateInfo atomic.Pointer[updater.ReleaseInfo]

// updateInProgress prevents concurrent apply requests from racing on binary swap.
var updateInProgress atomic.Bool

// UpdateRouteDeps holds dependencies for the update routes.
type UpdateRouteDeps struct {
	Updater   *updater.Updater
	Version   string
	OnRestart func()
	OnFound   func(*updater.ReleaseInfo) // broadcast update to WebSocket + TUI
	// OnDismissed runs after a dismiss is persisted, carrying the tag that
	// was skipped. The Web hides its own indicator from SharedUpdateInfo,
	// but the TUI holds a separate copy of the pending release — this is
	// how it learns to drop the badge. Optional.
	OnDismissed func(tag string)
}

// DismissUpdate records tag as the skipped version and clears the shared
// pending-update pointer if it still names that tag. Shared by the
// POST /api/update/dismiss route and the TUI's S key in the release-notes
// overlay. CompareAndSwap, not Store(nil): a newer release found while the
// dismiss was in flight must survive.
func DismissUpdate(store *config.Store, tag string) error {
	mu := store.RWMutex()
	cfg := store.Config()
	mu.Lock()
	old := cfg.Updates.SkippedVersion
	cfg.Updates.SkippedVersion = tag
	if err := store.SaveLocked(); err != nil {
		cfg.Updates.SkippedVersion = old
		mu.Unlock()
		return err
	}
	mu.Unlock()
	if pending := SharedUpdateInfo.Load(); pending != nil && pending.TagName == tag {
		SharedUpdateInfo.CompareAndSwap(pending, nil)
	}
	return nil
}

// UpdateRoutes registers the update check/apply/dismiss API endpoints. The
// Store carries the cfg + lock + savePath; /api/update/dismiss flips the
// AutoCheckUpdates field and persists via store.SaveLocked, rolling back
// the in-memory mutation if the save fails.
func UpdateRoutes(r chi.Router, deps *UpdateRouteDeps, store *config.Store) {
	updateCheckGate := newCallDebouncer(updateCheckDebounce)

	// GET /api/update/status — current update status
	r.Get("/api/update/status", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"currentVersion": deps.Version,
			"available":      false,
		}
		if ui := SharedUpdateInfo.Load(); ui != nil {
			resp["available"] = true
			resp["version"] = ui.Version
			resp["tagName"] = ui.TagName
			resp["releaseNotes"] = ui.ReleaseNotes
			resp["releaseNotesHtml"] = ui.ReleaseNotesHtml
			resp["publishedAt"] = ui.PublishedAt
		}
		jsonResponse(w, resp)
	})

	// POST /api/update/check — manually trigger an update check
	r.Post("/api/update/check", func(w http.ResponseWriter, r *http.Request) {
		if deps.Updater == nil {
			jsonError(w, "updater not available", http.StatusServiceUnavailable)
			return
		}

		// Stamp on the ATTEMPT, not on the outcome: the quota is spent by the
		// request, whether or not GitHub answers usefully.
		if ok, wait := updateCheckGate.allow(time.Now()); !ok {
			writeDebounced(w, wait)
			return
		}

		release, err := deps.Updater.CheckForUpdate(r.Context())
		if err != nil {
			jsonError(w, "check failed", http.StatusInternalServerError)
			return
		}

		resp := map[string]any{
			"currentVersion": deps.Version,
			"available":      false,
		}
		if release != nil {
			SharedUpdateInfo.Store(release)
			if deps.OnFound != nil {
				deps.OnFound(release)
			}
			resp["available"] = true
			resp["version"] = release.Version
			resp["tagName"] = release.TagName
			resp["releaseNotes"] = release.ReleaseNotes
			resp["releaseNotesHtml"] = release.ReleaseNotesHtml
			resp["publishedAt"] = release.PublishedAt
		}

		jsonResponse(w, resp)
	})

	// POST /api/update/apply — download update and restart
	r.Post("/api/update/apply", func(w http.ResponseWriter, r *http.Request) {
		if deps.Updater == nil {
			jsonError(w, "updater not available", http.StatusServiceUnavailable)
			return
		}

		// Defence-in-depth on top of CSRFMiddleware: replacing the
		// running binary + restarting is high-impact; only loopback
		// callers (browser tab on this machine, TUI in-process) may
		// invoke it, even when network_access is set to lan/external.
		// Audit reports/web.md S-8.
		if !updateApplyOriginAllowed(r) {
			jsonError(w, "update apply restricted to local origins", http.StatusForbidden)
			return
		}

		if !updateInProgress.CompareAndSwap(false, true) {
			jsonError(w, "update already in progress", http.StatusConflict)
			return
		}

		release := SharedUpdateInfo.Load()
		if release == nil {
			updateInProgress.Store(false)
			jsonError(w, "no update available", http.StatusBadRequest)
			return
		}

		if err := deps.Updater.ApplyUpdate(r.Context(), release); err != nil {
			updateInProgress.Store(false)
			jsonError(w, "update failed", http.StatusInternalServerError)
			return
		}

		jsonResponse(w, map[string]any{"success": true})

		// Flush the response before triggering restart to ensure the
		// client receives the success response.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		// Trigger restart after response is flushed
		go func() {
			defer func() {
				if r := recover(); r != nil {
					reportPanic("update restart handler", r)
				}
			}()
			if deps.OnRestart != nil {
				deps.OnRestart()
			}
		}()
	})

	// POST /api/update/verify — verify current binary's signature
	r.Post("/api/update/verify", func(w http.ResponseWriter, r *http.Request) {
		if deps.Updater == nil {
			jsonError(w, "updater not available", http.StatusServiceUnavailable)
			return
		}
		if err := deps.Updater.VerifyCurrentSignature(r.Context()); err != nil {
			jsonError(w, "signature verification failed", http.StatusUnprocessableEntity)
			return
		}
		jsonResponse(w, map[string]any{"verified": true})
	})

	// GET /api/update/release-notes?version=X.Y.Z fetches release notes
	// for a specific version. Defaults to the running version if no query
	// param. Returns { tagName, releaseNotes (raw markdown), releaseNotesHtml }.
	r.Get("/api/update/release-notes", func(w http.ResponseWriter, r *http.Request) {
		if deps.Updater == nil {
			jsonError(w, "updater not configured", http.StatusServiceUnavailable)
			return
		}
		version := r.URL.Query().Get("version")
		if version == "" {
			version = deps.Updater.CurrentVersion()
		}
		// AFTER the default on purpose: a build whose own version string is
		// malformed must fail loudly here rather than quietly query a tag
		// GitHub has never had.
		if !releaseVersionRe.MatchString(version) {
			jsonError(w, "invalid version", http.StatusBadRequest)
			return
		}
		info, err := deps.Updater.FetchReleaseNotes(r.Context(), version)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		jsonResponse(w, map[string]any{
			"tagName":          info.TagName,
			"releaseNotes":     info.ReleaseNotes,
			"releaseNotesHtml": info.ReleaseNotesHtml,
		})
	})

	// POST /api/update/dismiss — skip THIS pending version and clear the
	// update info. Version-scoped by design: the old behavior flipped
	// AutoCheckUpdates=false globally, so the dialog's only non-apply
	// action permanently disabled all update awareness on a box whose
	// YouTube/Twitch extractors rot without updates. Disabling checks
	// entirely remains available as the Settings > Updates toggle.
	// The NEXT release (different tag) notifies normally.
	r.Post("/api/update/dismiss", func(w http.ResponseWriter, r *http.Request) {
		pending := SharedUpdateInfo.Load()
		if pending == nil {
			jsonError(w, "no update pending", http.StatusBadRequest)
			return
		}
		if err := DismissUpdate(store, pending.TagName); err != nil {
			jsonError(w, "failed to save config", http.StatusInternalServerError)
			return
		}
		if deps.OnDismissed != nil {
			deps.OnDismissed(pending.TagName)
		}
		jsonResponse(w, map[string]any{"success": true, "skipped": pending.TagName})
	})
}
