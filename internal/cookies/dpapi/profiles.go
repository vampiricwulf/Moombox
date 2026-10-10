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
// is what ReadChromeCookiesStats takes.
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
// A relative value resolves against Moombox's WORKING directory — the same as
// paths.* and cookies.browser_profile_dir, and nothing here rewrites it.
//
// Three facts:
//
//   - dir EXISTS and is a real DIRECTORY — not a symlink, and not a Windows
//     junction (see linkLike). THE SYMLINK RULE, stated once: it applies to
//     the profile directory ITSELF, which is refused rather than followed,
//     because that directory is the path every other judgement here is made
//     about and a link out of it would describe somewhere else entirely.
//     Files beside or inside it are resolved exactly as the reader resolves
//     them — following links — since the reader is what opens them.
//   - a `Local State` that ChromeLocalStatePath can find: beside the directory
//     (Chromium's `User Data` root) or inside it (Opera's self-contained
//     layout). That is where Chromium keeps the DPAPI-protected master key; a
//     profile dir with neither has no key to decrypt the cookie values with,
//     and the read would fail late with "nothing came out".
//   - a cookie store INSIDE: `Cookies` on older Chromium, `Network/Cookies`
//     since the network-service move. Either satisfies it.
//
// The error names the directory and which fact was missing, because the whole
// point of the setting is an operator pointing at a directory by hand and
// needing to know when they pointed at the wrong one — the User Data root
// instead of the profile inside it being the likely mistake. That mistake is
// named on the COOKIE-STORE arm, which is the one it lands on: a User Data root
// holds its own `Local State`, and since Opera's layout was accepted
// ChromeLocalStatePath takes an inside one, so the root passes the key check
// and fails for want of a cookie store.
func ValidateProfileDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("cookies.dpapi_profile_dir %q: %w", dir, err)
	}
	if linkLike(info.Mode()) {
		return fmt.Errorf("cookies.dpapi_profile_dir %q is a symlink or junction; name the real profile "+
			"directory, because the directory itself is judged as written", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("cookies.dpapi_profile_dir %q is not a directory", dir)
	}
	if _, err := ChromeLocalStatePath(dir); err != nil {
		return fmt.Errorf("cookies.dpapi_profile_dir %q: %w — that file holds the DPAPI master key "+
			"the cookie values are encrypted with", dir, err)
	}
	for _, rel := range []string{"Cookies", filepath.Join("Network", "Cookies")} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			return nil
		}
	}
	// The User Data ROOT is the likely mistake, and this is the arm it lands
	// on — NOT the Local State arm above, which the root sails through: Opera's
	// layout put `Local State` INSIDE the profile dir, so ChromeLocalStatePath
	// now accepts a root holding its own. What a root does not hold is a cookie
	// store; what it does hold is `Default` or `Profile N`. Naming that turns
	// "holds neither" into an instruction.
	if child := chromiumProfileChild(dir); child != "" {
		return fmt.Errorf("cookies.dpapi_profile_dir %q holds neither \"Cookies\" nor \"Network/Cookies\", "+
			"but it does hold %q — name the PROFILE directory (…/User Data/Default), not the User Data root",
			dir, child)
	}
	return fmt.Errorf("cookies.dpapi_profile_dir %q holds neither \"Cookies\" nor \"Network/Cookies\"", dir)
}

// chromiumProfileChild returns the name of a Chromium PROFILE subdirectory of
// dir — `Default` or `Profile N` — or "" if there is none.
//
// Used only to word ValidateProfileDir's cookie-store refusal: a directory with
// those children and no cookie store of its own is a User Data root, which is
// the one mistake the setting invites. Case-insensitively, because the
// filesystems this runs on are, and best-effort — an unreadable directory
// simply produces the shorter sentence.
func chromiumProfileChild(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.EqualFold(name, "Default") {
			return name
		}
		rest, ok := strings.CutPrefix(strings.ToLower(name), "profile ")
		if !ok || rest == "" {
			continue
		}
		if strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
			return name
		}
	}
	return ""
}

// linkLike reports whether an Lstat mode describes a reparse point rather than
// the directory the path names.
//
// os.ModeSymlink alone is not enough on Windows: Go reports a directory
// JUNCTION — `mklink /J`, which needs no Developer Mode and is therefore the
// likelier shape of this mistake — as os.ModeIrregular. Without the second bit
// a junction fell through to ValidateProfileDir's IsDir() arm and the operator
// was told their directory was not a directory, which it visibly is.
func linkLike(mode os.FileMode) bool {
	return mode&(os.ModeSymlink|os.ModeIrregular) != 0
}

// ChromeLocalStatePath returns the `Local State` file that holds the
// DPAPI-protected master key for profileDir, and is the SINGLE rule for that
// question: ValidateProfileDir decides whether a directory is usable and
// loadChromeMasterKey (dpapi_windows.go) decides where the key is, and while
// those were two separate judgements they could disagree — they both hard-coded
// "one level up", so Opera, which keeps `Local State` INSIDE the profile
// directory with no User Data root above it, was refused by one and would have
// failed in the other.
//
// Two layouts, and the PARENT is tried first:
//
//   - `<profileDir>/../Local State` — Chromium, Edge, Brave, Vivaldi: one
//     `User Data` root with `Default` / `Profile N` children.
//   - `<profileDir>/Local State` — Opera, which collapses the root into the
//     profile directory itself.
//
// The order is not cosmetic. A normal Chromium profile directory that happens
// to contain a stray `Local State` (a copied file, a half-finished profile
// move) must still decrypt against the real root key beside it; preferring the
// inner file would silently pick the wrong key and every row would fail to
// decrypt.
//
// Untagged on purpose: the reader is Windows-only, but this rule is the half
// that can be pinned on both CI legs.
func ChromeLocalStatePath(profileDir string) (string, error) {
	beside := filepath.Join(filepath.Dir(profileDir), "Local State")
	if _, err := os.Stat(beside); err == nil {
		return beside, nil
	}
	inside := filepath.Join(profileDir, "Local State")
	if _, err := os.Stat(inside); err == nil {
		return inside, nil
	}
	return "", fmt.Errorf("no \"Local State\" beside the profile directory (%q) or inside it (%q)", beside, inside)
}
