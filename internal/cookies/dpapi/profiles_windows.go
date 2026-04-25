//go:build windows

package dpapi

import (
	"os"
	"path/filepath"
)

// FindBrowserProfiles enumerates every Chromium-family profile directory
// the package knows how to read. Each profile is one BrowserProfile entry
// pointing at a Default / Profile N subdir under a known User Data root.
//
// The function is filesystem-driven: it walks each known User Data root
// for which the env-var-rooted path exists and lists subdirectories that
// look like profile dirs ("Default" or "Profile " prefix). Missing
// browsers / channels / profiles are silently skipped. Returns an empty
// slice (not nil) when no profiles are found so callers can range over
// the result without nil-checks.
//
// The discovery is cheap (a handful of os.Stat / os.ReadDir calls under
// %LOCALAPPDATA%) so it's safe to call on every refresh tick.
func FindBrowserProfiles() []BrowserProfile {
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		return []BrowserProfile{}
	}
	return findBrowserProfilesIn(localAppData)
}

// findBrowserProfilesIn is the testable seam for FindBrowserProfiles —
// callers pass the LOCALAPPDATA root explicitly so unit tests can run
// against a synthetic filesystem.
func findBrowserProfilesIn(localAppData string) []BrowserProfile {
	out := make([]BrowserProfile, 0, 4)
	for _, b := range chromiumBrowsers {
		userDataDir := filepath.Join(localAppData, b.userDataPath)
		entries, err := os.ReadDir(userDataDir)
		if err != nil {
			// User Data dir doesn't exist — browser not installed (or
			// installed but never run; the dir is created on first run).
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if !isChromiumProfileDirName(name) {
				continue
			}
			out = append(out, BrowserProfile{
				Browser:   b.id,
				Name:      name,
				Path:      filepath.Join(userDataDir, name),
				IsDefault: name == "Default",
			})
		}
	}
	return out
}

