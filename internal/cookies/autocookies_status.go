package cookies

// autocookies_status.go — the auto-cookie status snapshot: the wire type and
// the reads (GetStatus, ReloginStatus, LogProfileDirVerdict) that publish it.

import (
	"maps"
	"time"
)

// AutoCookieStatus holds the current status of the auto-cookie service.
//
// There is deliberately no `configured` flag. One existed, computed as
// `profileDir != ""`, and it could not be false: cmd/moombox seeds the profile
// dir with a "./browser-profile" default before the service is constructed, so
// every install reported true from the first run. Nothing read it — not the
// dashboard, not the settings dialog, not the TUI — and a value that is always
// true and read by nobody is worse than an absent one, because the next reader
// believes it. Whether auto-cookies were ever set up is answered honestly by
// lastRefresh and needsManualRelogin, which describe what actually happened.
type AutoCookieStatus struct {
	SetupInProgress       bool                      `json:"setupInProgress"`
	Browser               *DetectedBrowser          `json:"browser"`
	AvailableBrowsers     []DetectedBrowser         `json:"availableBrowsers"`
	ConfiguredBrowserPath string                    `json:"configuredBrowserPath,omitempty"`
	ConfiguredBrowserType string                    `json:"configuredBrowserType,omitempty"`
	LastRefresh           *string                   `json:"lastRefresh"`
	LastError             *string                   `json:"lastError"`
	NeedsManualRelogin    AutoCookieReloginRequired `json:"needsManualRelogin"`
}

// GetStatus returns the current auto-cookie status.
func (s *AutoCookieService) GetStatus() AutoCookieStatus {
	// DetectBrowser/DetectBrowsers do filesystem I/O and registry queries —
	// call outside the lock to avoid holding it while doing slow I/O.
	browser := DetectBrowser()
	available := DetectBrowsers()
	var cfgPath, cfgType string
	if s.ConfiguredBrowserOverride != nil {
		cfgPath, cfgType = s.ConfiguredBrowserOverride()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Status polling is the most frequent visitor to this lock, which makes it
	// the reap that actually fires in practice — both UIs poll it while their
	// cookie dialog is open, and the TUI polls it with no dialog at all.
	s.reapAbandonedSetupLocked()

	var lastRefreshStr *string
	if s.lastRefresh != nil {
		v := s.lastRefresh.UTC().Format(time.RFC3339)
		lastRefreshStr = &v
	}

	return AutoCookieStatus{
		SetupInProgress:       s.setupInProgressLocked(),
		Browser:               browser,
		AvailableBrowsers:     available,
		ConfiguredBrowserPath: cfgPath,
		ConfiguredBrowserType: cfgType,
		LastRefresh:           lastRefreshStr,
		LastError:             s.lastError,
		// Must be a COPY: the HTTP handlers JSON-marshal this after the lock
		// is released, while refresh/flag paths mutate the live map under
		// s.mu — a concurrent map read+write is a fatal runtime throw that
		// RecoveryMiddleware cannot catch.
		NeedsManualRelogin: maps.Clone(s.needsRelogin),
	}
}

// ReloginStatus returns which platforms need the user to sign in again,
// without GetStatus's browser/registry detection scan (~155ms measured
// 2026-08-25: DetectBrowser + DetectBrowsers, filesystem I/O and a reg.exe
// spawn on Windows). Four of GetStatus's five production callers read
// nothing else from the returned AutoCookieStatus, so they pay that cost on
// every poll for a field this method computes identically — same lock,
// same clone-under-lock copy of s.needsRelogin GetStatus returns.
//
// Status polling is the most frequent visitor to this lock, which makes it
// the reap that actually fires in practice — both UIs poll it while their
// cookie dialog is open, and the TUI polls it with no dialog at all (see
// GetStatus's doc comment, which this one must keep agreeing with). Moving
// GetStatus's four highest-frequency callers here means THIS method is now
// that most-frequent visitor, so it MUST call reapAbandonedSetupLocked
// exactly as GetStatus does — a future "simplification" that drops the call
// because ReloginStatus "only reads needsRelogin" would silently stop Arc
// 3's abandoned-setup reap from firing in production, with no existing test
// noticing because the reap still exists, it would just never run.
func (s *AutoCookieService) ReloginStatus() AutoCookieReloginRequired {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapAbandonedSetupLocked()
	return maps.Clone(s.needsRelogin)
}

// LogProfileDirVerdict says ONCE what the launch guard decided about the
// configured browser profile directory, at the level the acquisition mode
// earns. Silent when the directory is fine, which is nearly every install.
//
// A method rather than a line in NewAutoCookieService because the constructor
// runs BEFORE AcquisitionMode is wired (cmd/moombox/services.go builds the
// service, then assigns the callbacks), so it cannot tell the two
// configurations apart and logged the same red line for both.
//
// Under "auto" that line is right, and its wording is kept verbatim: a browser
// refresh will be refused this directory, so an operator has a launch they
// believe is happening and is not, and ERROR is the level that gets read.
//
// Under "profile" nothing was going to launch anyway. The refusal describes an
// event that never occurs, and the pass that DOES run — the read-only import,
// which copies cookies.sqlite and its -wal into a 0700 temp dir and opens the
// COPY mode=ro — is not affected by the guard at all. So that mode gets one
// INFO saying both halves out loud, because an operator who followed the README
// recipe is owed an acknowledgement rather than a rejection.
//
// The verdict itself is untouched: validateBrowserProfileDirForLaunch is still
// called exactly once, at construction, and the four subprocess sites still
// read s.profileDirErr directly, in every mode (see the field's comment and
// TestLaunchGuardHoldsEveryLaunchSiteInEveryMode). This changes a sentence, not
// a decision.
//
// NO LOCK, and it must not be called with s.mu held: resolvedAcquisition
// reaches the config store's own read lock through AcquisitionMode, which is
// the same rule the launch sites and readOnlyProfileDirErr already follow.
// Everything it reads — profileDirErr, profileDir, logger, AcquisitionMode —
// is written once, before the service is handed to any goroutine. Called once,
// from the wiring sequence.
func (s *AutoCookieService) LogProfileDirVerdict() {
	if s.profileDirErr == nil || s.logger == nil {
		return
	}
	if s.resolvedAcquisition() == AcquisitionProfile {
		s.logger.Info("browser profile dir sits inside a real installed browser's profile tree — "+
			"no headless browser will be launched against it; cookies.acquisition is \"profile\", "+
			"so the read-only import is what runs",
			"profile_dir", s.profileDir)
		return
	}
	s.logger.Error("auto-cookie profile dir rejected at construction", "err", s.profileDirErr)
}
