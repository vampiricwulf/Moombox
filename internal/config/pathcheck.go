package config

import (
	"fmt"
	"os"
	"strings"
)

// PathHasTraversal reports whether p contains a ".." path segment.
//
// With PathIsQuoted, this is the ONLY constraint Moombox places on a
// user-supplied path value (paths.*, cookies.cookie_file,
// cookies.browser_profile_dir, network.tls_*_path). Absolute paths — POSIX
// "/data/output", Windows "C:\Moombox\data", UNC "\\server\share" — are
// legitimate and must be accepted: the Docker image seeds every path field
// as "/data/...", the TUI has always accepted absolute values, and
// config.toml has always accepted them by hand. A Web UI that rejected them
// made every settings save from a container fail with an unrecoverable 400.
//
// Because absolute paths are allowed, this check is a typo/sanity guard, not
// a containment boundary — an operator who can PUT /api/config can already
// name any location on the host directly. It is kept because "../.." in a
// path field is almost always a mistake, and because a value one UI accepts
// and the other rejects is the exact defect this function was rewritten to
// fix. Both UIs call this; keep them calling the same function.
//
// Deliberately NOT enforced by Validate/Normalize: config.Save runs Validate,
// so rejecting ".." there would make an existing hand-edited config
// unsavable — the same trap in a different costume.
//
// A ".." SEGMENT is matched, not the substring "..": "my..file.txt" and
// "..hidden" are ordinary names, and the old substring check rejected them
// for no benefit.
func PathHasTraversal(p string) bool {
	if p == "" {
		return false
	}
	// Strip a Windows drive prefix so "C:..\escape" — drive-relative
	// traversal — is examined as "..\escape" rather than as a segment
	// literally named "C:..".
	if len(p) >= 2 && p[1] == ':' && isDriveLetter(p[0]) {
		p = p[2:]
	}
	// Both separators are treated as separators on every platform. The value
	// may be authored on one OS and consumed on another, and over-rejecting a
	// Linux file literally named `a\..\b` is a far cheaper mistake than
	// letting `..\` through on Windows.
	for _, seg := range strings.FieldsFunc(p, isPathSeparator) {
		// Windows strips trailing dots and spaces from a path component, so
		// ".. " is not stored verbatim. Measured: it resolves to the CURRENT
		// directory, not the parent — as do "...", "....", and ".. ." — so
		// none of them traverse and none of them has to be caught here.
		// Trimming trailing spaces is a deliberate over-rejection that keeps
		// ".. " on the conservative side; it is not required for correctness.
		// Do NOT extend this to trailing dots on the assumption that they
		// traverse: they don't, and trimming them would start rejecting
		// legitimate names like "..." for no gain.
		if strings.TrimRight(seg, " ") == ".." {
			return true
		}
	}
	return false
}

func isPathSeparator(r rune) bool { return r == '/' || r == '\\' }

// UnquotePath takes one matching pair of surrounding quotes off a pasted path.
// Explorer's "Copy as path" (Ctrl+Shift+C) always wraps the path in double
// quotes, Windows Terminal quotes a dropped file whose path has a space, and
// some Linux terminals single-quote a dropped one — so the quotes are the
// paste's, not the name's, and using them as part of it answered "does not
// exist" about a file that does. Neither quote is a legal character in a
// Windows file name. Only a matching pair at both ends comes off, and only
// one: a quote inside the path, or at one end alone, is left as typed.
//
// The prompts that take a path to use at once call it: the TUI's Import
// Cookie File (E I) and its FFmpeg overlay's custom path, and POST
// /api/ffmpeg/check. A path saved as a setting is refused quoted instead
// (PathIsQuoted).
func UnquotePath(p string) string {
	if len(p) >= 2 {
		if q := p[0]; (q == '"' || q == '\'') && p[len(p)-1] == q {
			return p[1 : len(p)-1]
		}
	}
	return p
}

// PathIsQuoted reports whether p, its surrounding spaces aside, is wrapped in
// a matching pair of quotes — what UnquotePath takes off. A settings path
// field (paths.*, cookies.cookie_file, cookies.browser_profile_dir,
// network.tls_*_path) refuses such a value in both UIs and both setup
// wizards: nothing downstream takes the quotes off, so `"E:\Archive"` has no
// volume on Windows — it is a relative directory whose name holds `"` and
// `:`, and every mux fails to create it. Refused rather than silently
// unquoted, so the value saved is the one the operator sees — and, unlike a
// ".." segment, only when it is a new value (newQuotedPathErrors in
// internal/web/routes, quotedSettingsPath in internal/tui), so one stored
// before the rule does not make every unrelated save fail.
func PathIsQuoted(p string) bool {
	p = strings.TrimSpace(p)
	return UnquotePath(p) != p
}

// SetupDirError is MakeSetupDirs' refusal: the directory that could not be
// created, by its config key (paths.output_directory or
// paths.staging_directory, the key the Web wizard's 400 is detailed under).
type SetupDirError struct {
	Key  string
	Path string
	Err  error
}

func (e *SetupDirError) Error() string {
	return fmt.Sprintf("%s: could not create %q: %v", e.Key, e.Path, e.Err)
}

func (e *SetupDirError) Unwrap() error { return e.Err }

// MakeSetupDirs creates the output and staging directories a first-run setup
// names, before either wizard saves anything — the Web one's POST
// /api/setup/complete and the TUI one's save command. A directory that
// cannot be created (a parent that is a file, a missing drive, no
// permission) refuses the setup with a *SetupDirError, so the operator
// corrects the path in the wizard. Both wizards discarded the error and
// reported "Setup complete": nothing at boot creates the output directory,
// so the first recording downloaded in full and only then failed at mux.
// An empty path is skipped; Validate requires both anyway.
func MakeSetupDirs(outputDir, stagingDir string) error {
	for _, d := range []struct{ key, path string }{
		{"paths.output_directory", outputDir},
		{"paths.staging_directory", stagingDir},
	} {
		if d.path == "" {
			continue
		}
		if err := os.MkdirAll(d.path, 0o755); err != nil {
			return &SetupDirError{Key: d.key, Path: d.path, Err: err}
		}
	}
	return nil
}

func isDriveLetter(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}
