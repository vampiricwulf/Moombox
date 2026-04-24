package updater

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
)

// ParseVersion parses a version string like "v2.0.15" or "2.0.15" into
// major, minor, patch components. Supports 2-segment ("1.0") and
// 3-segment ("1.0.0") versions.
//
// **Pre-release / build-metadata not supported.** Tags such as
// "2.5.0-rc1" or "2.5.0+build.42" return an error from this function;
// CompareVersions falls back to lexical strings.Compare on the original
// inputs in that case, which is *not* SemVer-correct. Moombox tags are
// plain "vX.Y.Z" today, so this is acceptable — but if pre-release
// suffixes ever ship, extend this parser before relying on ordering.
// Audit reports/small-packages.md.
func ParseVersion(s string) (major, minor, patch int, err error) {
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, 0, 0, fmt.Errorf("invalid version format: %q", s)
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid major version: %w", err)
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid minor version: %w", err)
	}
	if len(parts) == 3 {
		patch, err = strconv.Atoi(parts[2])
		if err != nil {
			return 0, 0, 0, fmt.Errorf("invalid patch version: %w", err)
		}
	}
	return major, minor, patch, nil
}

// CompareVersions compares two version strings.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
func CompareVersions(a, b string) int {
	aMaj, aMin, aPat, aErr := ParseVersion(a)
	bMaj, bMin, bPat, bErr := ParseVersion(b)
	if aErr != nil || bErr != nil {
		return strings.Compare(a, b)
	}
	if aMaj != bMaj {
		return cmp.Compare(aMaj, bMaj)
	}
	if aMin != bMin {
		return cmp.Compare(aMin, bMin)
	}
	return cmp.Compare(aPat, bPat)
}
