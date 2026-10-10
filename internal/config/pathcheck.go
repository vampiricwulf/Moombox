package config

import (
	"fmt"
	"os"
	"strings"
)

// PathHasTraversal reports whether p contains a ".." path segment.
//
// This is the ONLY constraint Moombox places on a user-supplied path value
// (paths.*, cookies.cookie_file, cookies.browser_profile_dir,
// network.tls_*_path). Absolute paths — POSIX "/data/output", Windows
// "C:\Moombox\data", UNC "\\server\share" — are legitimate and must be
// accepted: the Docker image seeds every path field as "/data/...", the TUI
// has always accepted absolute values, and config.toml has always accepted
// them by hand. A Web UI that rejected them made every settings save from a
// container fail with an unrecoverable 400.
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
