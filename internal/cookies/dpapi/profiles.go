package dpapi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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

// ValidateProfileDir reports whether dir looks like a Chromium-family PROFILE
// directory the DPAPI reader can work with. It exists for
// cookies.dpapi_profile_dir, the one place an operator names a profile
// directory by hand: pointing the reader at the wrong directory produces "no
// cookies came out", a sentence that has several causes needing different
// responses, so the wrong directory is caught here and named.
//
// Every check below is a RUNTIME check — it stats the filesystem. config.Validate
// deliberately runs none of them: a container writes its config.toml before the
// volume holding the profile is mounted, so a boot-time existence check would
// fail the save that configures it. config.Validate checks the path's SHAPE
// (no ".." traversal) and nothing else; this is what the pass re-checks, and
// what AutoCookieService.LogDpapiProfileDirVerdict reports at boot as a Warn.
//
// Three facts:
//
//   - dir EXISTS, is a DIRECTORY, and is NOT A SYMLINK. The symlink case is
//     refused rather than followed, which is why this uses Lstat and not Stat:
//     every other judgement made about this setting — the caller's
//     browser-profile-tree deny-list, and the `Local State` lookup below — is
//     made on the path AS WRITTEN, and a link pointing out of that path would
//     leave both of them describing a different directory than the one the
//     reader opens.
//   - `Local State` ONE LEVEL UP. That is where Chromium keeps the
//     DPAPI-protected master key; a profile dir without it has no key to
//     decrypt the cookie values with, and the read would fail late with
//     "nothing came out".
//   - a cookie store INSIDE: `Cookies` on older Chromium, `Network/Cookies`
//     since the network-service move. Either satisfies it.
//
// The error names the directory and which fact was missing, because the whole
// point of the setting is an operator pointing at a directory by hand and
// needing to know when they pointed at the wrong one — the User Data root
// instead of the profile inside it being the likely mistake.
func ValidateProfileDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("cookies.dpapi_profile_dir %q: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("cookies.dpapi_profile_dir %q is a symlink; name the real profile directory, "+
			"because every check made here is made on the path as written", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("cookies.dpapi_profile_dir %q is not a directory", dir)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "Local State")); err != nil {
		return fmt.Errorf("cookies.dpapi_profile_dir %q has no \"Local State\" beside its parent — "+
			"name the PROFILE directory (…/User Data/Default), not the User Data root", dir)
	}
	for _, rel := range []string{"Cookies", filepath.Join("Network", "Cookies")} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			return nil
		}
	}
	return fmt.Errorf("cookies.dpapi_profile_dir %q holds neither \"Cookies\" nor \"Network/Cookies\"", dir)
}
