package updater

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
)

// Version is the parsed form of a SemVer-2.0.0-style tag: MAJOR.MINOR.PATCH
// with an optional pre-release suffix and build metadata. Build metadata
// (after `+`) is captured but does NOT participate in ordering, per spec.
type Version struct {
	Major      int
	Minor      int
	Patch      int
	PreRelease []string // dot-separated identifiers, empty for full releases
	Build      string   // raw build-metadata segment, ignored for ordering
}

// ParseVersionFull parses a SemVer-2.0.0-style tag including pre-release
// and build-metadata segments. Examples:
//
//	"v2.0.15"            → 2.0.15
//	"2.6.0-test.27"      → 2.6.0 with PreRelease=["test","27"]
//	"1.0.0-rc.1+build.7" → 1.0.0 with PreRelease=["rc","1"], Build="build.7"
//	"1.0"                → 1.0.0 (patch defaults to 0)
//
// Returns an error if MAJOR.MINOR cannot be parsed as integers, or if the
// segment count outside the suffixes isn't 2 or 3.
func ParseVersionFull(s string) (Version, error) {
	s = strings.TrimPrefix(s, "v")

	// Split off build-metadata (after the first '+'). Build does not affect
	// ordering and may contain segments that look like pre-release.
	var build string
	if i := strings.Index(s, "+"); i >= 0 {
		build = s[i+1:]
		s = s[:i]
	}

	// Split off pre-release (after the first '-'). Anything after is the
	// PreRelease segment, which is dot-separated.
	var preRelease []string
	if i := strings.Index(s, "-"); i >= 0 {
		preRelease = strings.Split(s[i+1:], ".")
		s = s[:i]
	}

	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return Version{}, fmt.Errorf("invalid version format: %q", s)
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return Version{}, fmt.Errorf("invalid major version: %w", err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return Version{}, fmt.Errorf("invalid minor version: %w", err)
	}
	patch := 0
	if len(parts) == 3 {
		patch, err = strconv.Atoi(parts[2])
		if err != nil {
			return Version{}, fmt.Errorf("invalid patch version: %w", err)
		}
	}

	return Version{
		Major:      major,
		Minor:      minor,
		Patch:      patch,
		PreRelease: preRelease,
		Build:      build,
	}, nil
}

// ParseVersion parses a version string into major/minor/patch ints. Kept
// for backwards compatibility; new callers should prefer ParseVersionFull
// since pre-release suffixes are now common in Moombox tags
// (`v2.6.0-test.N`). Audit reports/small-packages.md.
func ParseVersion(s string) (major, minor, patch int, err error) {
	v, err := ParseVersionFull(s)
	if err != nil {
		return 0, 0, 0, err
	}
	return v.Major, v.Minor, v.Patch, nil
}

// CompareVersions compares two version strings per SemVer 2.0.0 ordering
// rules. Returns -1 if a < b, 0 if a == b, 1 if a > b.
//
// Pre-release ordering (per spec):
//
//   - A version WITH a pre-release suffix is LESS than the same version
//     without one (1.0.0-rc.1 < 1.0.0).
//   - Numeric pre-release identifiers compare numerically; alphanumeric
//     compare lexically; numeric < alphanumeric.
//   - Shorter pre-release identifier list is less than longer one when
//     all preceding identifiers are equal (1.0.0-rc < 1.0.0-rc.1).
//
// Build metadata is ignored for ordering. If either input fails to parse,
// CompareVersions falls back to plain lexical strings.Compare on the
// originals so the function never panics.
func CompareVersions(a, b string) int {
	va, aErr := ParseVersionFull(a)
	vb, bErr := ParseVersionFull(b)
	if aErr != nil || bErr != nil {
		return strings.Compare(a, b)
	}
	if va.Major != vb.Major {
		return cmp.Compare(va.Major, vb.Major)
	}
	if va.Minor != vb.Minor {
		return cmp.Compare(va.Minor, vb.Minor)
	}
	if va.Patch != vb.Patch {
		return cmp.Compare(va.Patch, vb.Patch)
	}
	return comparePreRelease(va.PreRelease, vb.PreRelease)
}

// comparePreRelease implements the SemVer 2.0.0 pre-release ordering rules
// for two parsed identifier lists.
func comparePreRelease(a, b []string) int {
	// Empty pre-release > non-empty: 1.0.0 > 1.0.0-rc.
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	if len(a) == 0 {
		return 1
	}
	if len(b) == 0 {
		return -1
	}

	// Compare identifiers pairwise.
	for i := 0; i < len(a) && i < len(b); i++ {
		ai, aIsNum := parsePreReleaseIdent(a[i])
		bi, bIsNum := parsePreReleaseIdent(b[i])
		switch {
		case aIsNum && bIsNum:
			if c := cmp.Compare(ai, bi); c != 0 {
				return c
			}
		case aIsNum && !bIsNum:
			// Numeric identifiers always have lower precedence than alpha.
			return -1
		case !aIsNum && bIsNum:
			return 1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	// Larger identifier list wins when all preceding identifiers match.
	return cmp.Compare(len(a), len(b))
}

// parsePreReleaseIdent returns (numeric-value, true) for purely-numeric
// identifiers, or (0, false) for alphanumeric ones.
func parsePreReleaseIdent(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}
