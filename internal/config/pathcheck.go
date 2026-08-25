package config

import "strings"

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
		// Windows drops trailing spaces from a path component, so ".. "
		// resolves to "..". Trim before comparing or the segment form would
		// be weaker than the substring check it replaces.
		if strings.TrimRight(seg, " ") == ".." {
			return true
		}
	}
	return false
}

func isPathSeparator(r rune) bool { return r == '/' || r == '\\' }

func isDriveLetter(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}
