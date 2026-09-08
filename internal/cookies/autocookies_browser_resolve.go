package cookies

// autocookies_browser_resolve.go — resolving which browser (or none) a setup
// or refresh pass should use, and validating the configured profile directory
// it would be launched against.

import (
	"fmt"
	"path/filepath"
	"strings"
)

// dangerousProfilePathSubstrings flags absolute profile directories that
// belong to a real installed browser. Allowing the auto-cookie service
// to launch headless against one of these would let a malicious config
// (or, in the future, a compromised /api/config write) launch Chrome
// against the user's actual logged-in profile and exfiltrate session
// cookies via the cookies.txt export. Patterns are matched
// case-insensitively against the path's absolute form with every separator
// normalised to "/", so one list covers the Windows %AppData% trees, the
// Linux dotfile, snap and flatpak trees, and the macOS Library trees; a Linux
// path that carries literal backslashes is normalised the same way. Before
// 2026-09-08 the list knew only the Windows shapes, so on Linux (Docker) a
// real ~/.mozilla/firefox profile was never recognised and the guard was
// inert there. Audit reports/cookies.md #26.
var dangerousProfilePathSubstrings = []string{
	// Windows: %LocalAppData% / %AppData%
	`/google/chrome/user data`,
	`/google/chrome beta/user data`,
	`/google/chrome dev/user data`,
	`/google/chrome canary/user data`,
	`/microsoft/edge/user data`,
	`/microsoft/edge beta/user data`,
	`/microsoft/edge dev/user data`,
	`/microsoft/edge canary/user data`,
	`/bravesoftware/brave-browser/user data`,
	`/chromium/user data`,
	`/vivaldi/user data`,
	`/opera software/opera stable`,
	`/opera software/opera gx stable`,
	`/mozilla/firefox/profiles`,
	`/mozilla/firefox developer edition/profiles`,
	`/waterfox/profiles`,
	`/thunderbird/profiles`,
	`/librewolf/profiles`,
	// Linux: ~/.mozilla, ~/.config and the dotfile trees; snap keeps the same
	// ~/.mozilla tree under ~/snap/firefox/common, which these still match.
	`/.mozilla/firefox/`,
	`/.mozilla/firefox-esr/`,
	`/.config/google-chrome/`,
	`/.config/google-chrome-beta/`,
	`/.config/google-chrome-unstable/`,
	`/.config/chromium/`,
	`/.config/bravesoftware/brave-browser/`,
	`/.config/microsoft-edge/`,
	`/.config/microsoft-edge-beta/`,
	`/.config/microsoft-edge-dev/`,
	`/.config/vivaldi/`,
	`/.config/opera/`,
	`/.thunderbird/`,
	`/.librewolf/`,
	`/.waterfox/`,
	// Linux flatpak sandboxes
	`/.var/app/org.mozilla.firefox/`,
	`/.var/app/com.google.chrome/`,
	`/.var/app/org.chromium.chromium/`,
	`/.var/app/com.brave.browser/`,
	`/.var/app/com.microsoft.edge/`,
	// macOS: ~/Library/Application Support
	`/library/application support/firefox/profiles`,
	`/library/application support/google/chrome`,
	`/library/application support/chromium`,
	`/library/application support/bravesoftware/brave-browser`,
	`/library/application support/microsoft edge`,
	`/library/application support/vivaldi`,
	`/library/application support/thunderbird/profiles`,
}

// validateBrowserProfileDirForLaunch refuses configured profile directories
// that sit inside any user-installed browser's real profile tree, for the
// purpose of LAUNCHING a browser against them. Empty input is allowed — it just
// leaves the service inert, since every entry point that needs a profile dir
// returns ErrProfileNotFound without one. Otherwise the path is resolved to
// absolute, lowercased, and checked against the dangerous-substring list above.
// Audit reports/cookies.md #26.
//
// The name carries the scope because the scope was the defect (audit G3). The
// cached verdict also fast-failed the two READ-ONLY sites, which launch
// nothing: the import copies cookies.sqlite and its -wal sidecar into a 0700
// temp dir and opens the COPY mode=ro, the line dpapi/dpapi.go already draws.
// Those two now consult readOnlyProfileDirErr instead. This function stays on
// every subprocess site, in every mode, unconditionally.
func validateBrowserProfileDirForLaunch(profileDir string) error {
	if profileDir == "" {
		return nil
	}
	abs, err := filepath.Abs(profileDir)
	if err != nil {
		return fmt.Errorf("resolve profile dir: %w", err)
	}
	// Separators are normalised to "/" before matching: filepath.Abs keeps
	// backslashes on Windows, and a Linux path may carry them as ordinary
	// filename bytes; one form lets a single list serve every OS.
	lower := strings.ToLower(strings.ReplaceAll(abs, `\`, "/"))
	for _, pat := range dangerousProfilePathSubstrings {
		if strings.Contains(lower, pat) {
			return fmt.Errorf("profile dir %q points at a known browser profile path; refusing to launch a headless session against it (audit cookies.md #26)", abs)
		}
	}
	return nil
}

// browserOverrideConfigured reports whether a (path, browserType) pair
// read from ConfiguredBrowserOverride is a REAL override, as opposed to a
// partially-filled setting. Both must be non-empty: the TUI's browser_type
// field is free text with no matching path field required
// (tui/settings.go's Save writes BrowserType independently of
// BrowserPath), so a type typed in without a path is reachable in
// practice, not just in theory. resolvedBrowser and
// dpapiExtractAsNetscape's caller (autocookies.go's DPAPI fallback branch)
// both gate on this SAME predicate — Arc 8 fix round 1, Finding 3: before
// this existed, the DPAPI call site keyed off browserType alone, so a
// type-only setting was "no override" everywhere else but a hard filter
// there.
func browserOverrideConfigured(path, browserType string) bool {
	return path != "" && browserType != ""
}

// resolvedBrowser returns the user's configured browser when set, else
// the auto-detected best match. Used by StartSetup and RefreshCookies
// so the UI's browser_path/browser_type setting actually drives
// extraction (not just cosmetic display in the dropdown).
func (s *AutoCookieService) resolvedBrowser() *DetectedBrowser {
	if s.ConfiguredBrowserOverride != nil {
		path, btype := s.ConfiguredBrowserOverride()
		if browserOverrideConfigured(path, btype) {
			// Try to find the matching DetectedBrowser entry from
			// DetectBrowsers so Name is human-readable; fall back to
			// path-as-name if the configured path isn't in the
			// detected set (legitimate case for a user-supplied
			// custom binary).
			for _, b := range DetectBrowsers() {
				if b.Path == path {
					return &b
				}
			}
			return &DetectedBrowser{Type: btype, Path: path, Name: path}
		}
	}
	if s.detectBrowser != nil {
		return s.detectBrowser()
	}
	return DetectBrowser()
}

// browserLaunchBlocked reports whether this pass is forbidden a headless
// browser. Both consultations of the gate go through here so they cannot drift
// apart: one decides whether a browser is used, the other decides which of two
// sentences explains its absence, and a reader given the wrong sentence is sent
// to install a browser they already have.
//
// nil predicate = allowed, so a service built without it behaves exactly as
// before.
func (s *AutoCookieService) browserLaunchBlocked(policy browserGatePolicy) bool {
	return policy == gateApplies && s.BrowserLaunchAllowed != nil && !s.BrowserLaunchAllowed()
}

// resolvedAcquisition is cookies.acquisition as this package uses it: always
// one of the two constants, never anything else.
//
// The normalisation is not redundant with config.validateOrNormalize. This
// package is reachable from tests and from a service built by struct literal
// that never went through config at all, and an unhandled third value would
// mean an undefined refresh path rather than a loud failure. Same rule as
// browserLaunchBlocked's nil predicate: the safe answer, always.
func (s *AutoCookieService) resolvedAcquisition() string {
	if s.AcquisitionMode == nil {
		return AcquisitionAuto
	}
	if strings.ToLower(strings.TrimSpace(s.AcquisitionMode())) == AcquisitionProfile {
		return AcquisitionProfile
	}
	return AcquisitionAuto
}

// readOnlyProfileDirErr is the launch guard's verdict as a READ-ONLY caller
// must see it: honoured by default, lifted by the operator's explicit opt-in,
// and worded for a path that launches nothing. Two changes from reading
// profileDirErr directly, and both are the point of audit G3.
//
// First, cookies.acquisition = "profile" is consent. The read copies
// cookies.sqlite and its -wal sidecar into a 0700 temp dir and opens the COPY
// mode=ro, so nothing writes into the user's profile; the residual concern is
// exfiltration through cookies.txt, which is a decision for the operator to
// make once, in a setting, exactly as dpapi_fallback is. Second, the sentence:
// the cached error says "refusing to launch a headless session against it",
// which on this path describes an event that never happens.
//
// Consulted per call rather than cached, so a mode changed at runtime reaches
// the next R F.
func (s *AutoCookieService) readOnlyProfileDirErr() error {
	if s.profileDirErr == nil {
		return nil
	}
	if s.resolvedAcquisition() == AcquisitionProfile {
		return nil
	}
	return fmt.Errorf("browser profile dir %q sits inside a real installed browser's profile tree, "+
		"so nothing was launched and nothing was read — set cookies.acquisition = %q to allow a "+
		"read-only import from it (audit cookies.md #26): %w",
		s.profileDir, AcquisitionProfile, ErrProfileDirNotOptedIn)
}
