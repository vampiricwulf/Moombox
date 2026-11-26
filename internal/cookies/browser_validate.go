package cookies

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// knownBrowserTypes must match the browser-type identifiers used by
// DetectBrowser / autocookies_chromium / autocookies_firefox so a custom
// path with a known type plugs into the existing extraction machinery.
var knownBrowserTypes = map[string]struct{}{
	"firefox": {}, "waterfox": {}, "librewolf": {}, "zen": {},
	"chrome": {}, "brave": {}, "edge": {}, "vivaldi": {}, "thorium": {}, "opera": {},
}

// ImportWrittenSuffixes are the endings of every file name POST /api/import
// can leave on disk: the video extensions it takes (importVideoExts in
// internal/web/routes/import_routes.go), the ".chat.json" it names a chat
// with, the ".partial" an entry is extracted to and the ".zip" an upload is
// spooled to. A test in internal/web/routes pins this list equal to the
// import's own, so a new import extension cannot slip past the browser rule.
var ImportWrittenSuffixes = []string{".mp4", ".mkv", ".webm", ".ts", ".chat.json", ".partial", ".zip"}

// errImportWrittenName is the refusal of a browser path whose file name an
// import could have written.
var errImportWrittenName = errors.New("an imported file is never a browser")

// importWrittenSuffix returns the entry of ImportWrittenSuffixes the path's
// file name ends in, or "". The name is lower-cased as the import folds an
// extension (strings.ToLower), so ".MP4" and a KELVIN SIGN spelling of
// ".mkv", which the import also takes, match too.
func importWrittenSuffix(path string) string {
	name := strings.ToLower(filepath.Base(path))
	for _, s := range ImportWrittenSuffixes {
		if strings.HasSuffix(name, s) {
			return s
		}
	}
	return ""
}

// ValidateBrowserPath does the full validation including a 10-second
// `--version` smoke test. Suitable for HTTP endpoints (async) where
// blocking is acceptable. Pass req.Context() so subprocess cleanup
// follows request cancellation.
func ValidateBrowserPath(ctx context.Context, path, browserType string) error {
	if err := ValidateBrowserPathQuick(path, browserType); err != nil {
		return err
	}
	// Compose the request context with a 10s ceiling. If the caller
	// cancels (HTTP request aborted), the subprocess is killed within
	// the next syscall by exec.CommandContext.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %q --version failed: %w", path, err)
	}
	return nil
}

// ValidateBrowserPathQuick does the static checks (name, existence, regular
// file, executable bit on Unix, known type) without spawning a
// subprocess. Suitable for synchronous contexts like the TUI's
// settings save where blocking the UI is unacceptable.
func ValidateBrowserPathQuick(path, browserType string) error {
	if path == "" {
		return fmt.Errorf("browser path is empty")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("browser path must be absolute, got %q", path)
	}
	if _, ok := knownBrowserTypes[browserType]; !ok {
		return fmt.Errorf("unknown browser type %q (must be one of: firefox, waterfox, librewolf, zen, chrome, brave, edge, vivaldi, thorium, opera)", browserType)
	}
	if suffix := importWrittenSuffix(path); suffix != "" {
		return fmt.Errorf("%q ends in %q, a name an import writes: %w", path, suffix, errImportWrittenName)
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%q is not a regular file", path)
	}
	// Windows has no executable bit, and CreateProcess runs a PE whatever its
	// extension — so without a check a LAN client could have this run a file
	// it planted (POST /api/import writes uploads as "<title> [<id>].mp4").
	// Every browser on Windows is a .exe; on Unix the executable bit plays
	// that part. The import sets 0644, but a filesystem without POSIX modes
	// (CIFS, exFAT, ntfs-3g, a Docker Desktop bind mount) ignores that and
	// reports the bit on every file, which is why the name rule above runs
	// on every platform first (owner ruling R3-5).
	if runtime.GOOS == "windows" {
		if !strings.EqualFold(filepath.Ext(path), ".exe") {
			return fmt.Errorf("%q is not a .exe", path)
		}
	} else if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%q is not executable (chmod +x)", path)
	}
	return nil
}
