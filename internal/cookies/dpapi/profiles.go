package dpapi

import "strings"

// BrowserProfile identifies one Chromium-family profile directory on disk.
// The Path is the absolute path to the profile dir (e.g.
// "C:\Users\Wulf\AppData\Local\Google\Chrome\User Data\Default"), which
// is what ReadChromeCookies takes.
//
// Browser is the family identifier ("chrome", "edge", "brave", etc.) so
// callers can route the result based on which browser the user actually
// signed in to. Name is the profile name from Chrome's Local State —
// "Default" / "Profile 1" / "Profile 2", etc. — for display.
//
// IsDefault flags the "Default" subdir; multi-profile users have one
// Default and zero or more "Profile N" siblings. Most installations
// have only "Default".
type BrowserProfile struct {
	Browser   string // "chrome" | "edge" | "edge-beta" | "edge-dev" | "edge-canary" | "brave" | "chromium" | "vivaldi" | "chrome-beta" | "chrome-dev" | "chrome-canary"
	Name      string // "Default" | "Profile 1" | ...
	Path      string // absolute path to the profile dir
	IsDefault bool
}

// chromiumBrowserLayout describes one Chromium-family browser's
// User Data root location relative to %LOCALAPPDATA%. All entries
// share the standard Chromium profile layout: User Data is the parent
// and Default / Profile N are direct children. Opera's layout is
// different (no User Data root, single profile dir per channel) and
// is intentionally excluded — Opera users are rare and the layout
// difference would muddy the type.
type chromiumBrowserLayout struct {
	id           string // BrowserProfile.Browser value
	userDataPath string // path under LOCALAPPDATA to the User Data dir
}

// isChromiumProfileDirName returns true for directory names that match
// Chromium's profile-dir convention: the literal "Default" or names
// starting with "Profile " followed by something (typically a digit
// but Chrome accepts other forms in rare migration scenarios).
//
// System dirs that Chrome creates alongside profiles (e.g. "System
// Profile", "Guest Profile", "Crashpad", "ShaderCache") are
// intentionally excluded — they don't have user-meaningful cookies.
//
// Lives in the shared file (no build tag) because the test exercises
// it cross-platform; the actual Windows filesystem walk is in
// profiles_windows.go.
func isChromiumProfileDirName(name string) bool {
	if name == "Default" {
		return true
	}
	return strings.HasPrefix(name, "Profile ")
}

// chromiumBrowsers is the search list for FindBrowserProfiles. Order
// is informational only — every entry that exists on disk is returned;
// no "first match wins" priority. Stable installs come ahead of
// pre-release channels so a typical machine produces a tidy list.
var chromiumBrowsers = []chromiumBrowserLayout{
	{"chrome", `Google\Chrome\User Data`},
	{"edge", `Microsoft\Edge\User Data`},
	{"brave", `BraveSoftware\Brave-Browser\User Data`},
	{"vivaldi", `Vivaldi\User Data`},
	{"chromium", `Chromium\User Data`},
	{"chrome-beta", `Google\Chrome Beta\User Data`},
	{"chrome-dev", `Google\Chrome Dev\User Data`},
	{"chrome-canary", `Google\Chrome SxS\User Data`},
	{"edge-beta", `Microsoft\Edge Beta\User Data`},
	{"edge-dev", `Microsoft\Edge Dev\User Data`},
	{"edge-canary", `Microsoft\Edge SxS\User Data`},
}

// KnownBrowserFamilies returns the distinct base browser families
// FindBrowserProfiles knows a layout for — channel variants collapsed to
// their base id, so "chrome-beta" contributes "chrome", not a second
// entry. Order matches chromiumBrowsers' first occurrence of each base id.
//
// Exists so a caller outside this package can tell "this configured
// browser_type has NO possible dpapi layout at all" (e.g. Opera — excluded
// above for a real layout-shape reason — or Thorium, which
// browser_validate.go's knownBrowserTypes accepts as a launchable browser
// but this package has never had a layout for) apart from "this type DOES
// have a layout, but this machine happens to have no profile for it".
// Those two cases need different responses from a caller filtering
// profiles by configured type: the first must not filter at all (there is
// nothing to filter TO), the second is a legitimate "not found" the
// caller should still be able to report. Without this, that caller would
// otherwise have to hardcode a second copy of this id list to tell them
// apart.
func KnownBrowserFamilies() []string {
	seen := make(map[string]bool, len(chromiumBrowsers))
	out := make([]string, 0, len(chromiumBrowsers))
	for _, b := range chromiumBrowsers {
		base, _, _ := strings.Cut(b.id, "-")
		if seen[base] {
			continue
		}
		seen[base] = true
		out = append(out, base)
	}
	return out
}
