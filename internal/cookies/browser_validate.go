package cookies

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// knownBrowserTypes must match the browser-type identifiers used by
// DetectBrowser / autocookies_chromium / autocookies_firefox so a custom
// path with a known type plugs into the existing extraction machinery.
var knownBrowserTypes = map[string]struct{}{
	"firefox": {}, "waterfox": {}, "librewolf": {}, "zen": {},
	"chrome": {}, "brave": {}, "edge": {}, "vivaldi": {}, "thorium": {}, "opera": {},
}

// ValidateBrowserPath does the full validation including a 10-second
// `--version` smoke test. Suitable for HTTP endpoints (async) where
// blocking is acceptable.
//
// Used by the /api/auto-cookies/validate-browser-path endpoint before
// saving the user's config selection. Returns nil when the path is good.
func ValidateBrowserPath(path, browserType string) error {
	if err := ValidateBrowserPathQuick(path, browserType); err != nil {
		return err
	}
	// Run --version with a short timeout. Any non-error exit means the
	// binary at least starts up; we don't parse the output because each
	// browser formats it differently.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %q --version failed: %w", path, err)
	}
	return nil
}

// ValidateBrowserPathQuick does the static checks (existence, regular
// file, executable bit on Unix, known type) without spawning a
// subprocess. Suitable for synchronous contexts like the TUI's
// settings save where blocking the UI is unacceptable.
func ValidateBrowserPathQuick(path, browserType string) error {
	if path == "" {
		return fmt.Errorf("browser path is empty")
	}
	if _, ok := knownBrowserTypes[browserType]; !ok {
		return fmt.Errorf("unknown browser type %q (must be one of: firefox, waterfox, librewolf, zen, chrome, brave, edge, vivaldi, thorium, opera)", browserType)
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%q is not a regular file", path)
	}
	// Executable-bit check skipped on Windows where mode bits don't work
	// the same way (Windows uses the .exe extension and ACLs).
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%q is not executable (chmod +x)", path)
	}
	return nil
}
