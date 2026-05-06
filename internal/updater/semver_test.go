package updater

import "testing"

func TestParseVersion(t *testing.T) {
	tests := []struct {
		input               string
		major, minor, patch int
		wantErr             bool
	}{
		{"2.0.15", 2, 0, 15, false},
		{"v2.0.15", 2, 0, 15, false},
		{"1.0", 1, 0, 0, false},
		{"v0.1.0", 0, 1, 0, false},
		{"10.20.30", 10, 20, 30, false},
		{"", 0, 0, 0, true},
		{"abc", 0, 0, 0, true},
		{"1.2.3.4", 0, 0, 0, true},
		{"v1.x.0", 0, 0, 0, true},
	}
	for _, tt := range tests {
		maj, min, pat, err := ParseVersion(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseVersion(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if err == nil && (maj != tt.major || min != tt.minor || pat != tt.patch) {
			t.Errorf("ParseVersion(%q) = %d.%d.%d, want %d.%d.%d",
				tt.input, maj, min, pat, tt.major, tt.minor, tt.patch)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"2.0.15", "2.0.16", -1},
		{"2.0.16", "2.0.15", 1},
		{"2.0.15", "2.0.15", 0},
		{"v2.0.15", "2.0.15", 0},
		{"2.1.0", "2.0.15", 1},
		{"3.0.0", "2.9.99", 1},
		{"1.0", "1.0.0", 0},
		{"1.0", "1.0.1", -1},
		{"2.0.0", "1.99.99", 1},
	}
	for _, tt := range tests {
		got := CompareVersions(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// TestParseVersionFullPreReleaseAndBuild covers the SemVer-2.0.0
// extension shipped to support Moombox's `v2.6.0-test.N` pre-release
// tags. Build metadata is parsed but does NOT participate in ordering.
func TestParseVersionFullPreReleaseAndBuild(t *testing.T) {
	tests := []struct {
		input        string
		major, minor int
		patch        int
		wantPre      []string
		wantBuild    string
		wantErr      bool
	}{
		{"2.6.0-test.27", 2, 6, 0, []string{"test", "27"}, "", false},
		{"v2.6.0-test.27", 2, 6, 0, []string{"test", "27"}, "", false},
		{"1.0.0-rc.1+build.7", 1, 0, 0, []string{"rc", "1"}, "build.7", false},
		{"1.0.0+meta", 1, 0, 0, nil, "meta", false},
		{"1.0.0-alpha", 1, 0, 0, []string{"alpha"}, "", false},
		{"1.0.0-alpha.beta.1", 1, 0, 0, []string{"alpha", "beta", "1"}, "", false},
		{"2.0.15", 2, 0, 15, nil, "", false},
		// Errors:
		{"v1.x.0", 0, 0, 0, nil, "", true},
		{"1.2.3.4", 0, 0, 0, nil, "", true}, // 4 segments still rejected
	}
	for _, tt := range tests {
		v, err := ParseVersionFull(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseVersionFull(%q) err = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if v.Major != tt.major || v.Minor != tt.minor || v.Patch != tt.patch {
			t.Errorf("ParseVersionFull(%q) = %d.%d.%d, want %d.%d.%d",
				tt.input, v.Major, v.Minor, v.Patch, tt.major, tt.minor, tt.patch)
		}
		if !equalStrSlice(v.PreRelease, tt.wantPre) {
			t.Errorf("ParseVersionFull(%q).PreRelease = %v, want %v", tt.input, v.PreRelease, tt.wantPre)
		}
		if v.Build != tt.wantBuild {
			t.Errorf("ParseVersionFull(%q).Build = %q, want %q", tt.input, v.Build, tt.wantBuild)
		}
	}
}

// TestCompareVersionsPreReleaseOrdering locks the SemVer-2.0.0 ordering
// rules: pre-release < release of same MMP, numeric < alpha, list-prefix
// < longer list, build-metadata ignored.
func TestCompareVersionsPreReleaseOrdering(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		// Pre-release < same-MMP release.
		{"1.0.0-alpha", "1.0.0", -1},
		{"1.0.0", "1.0.0-alpha", 1},

		// Numeric ordering within pre-release.
		{"v2.6.0-test.2", "v2.6.0-test.10", -1},
		{"v2.6.0-test.10", "v2.6.0-test.2", 1},

		// Numeric < alpha (per spec).
		{"1.0.0-1", "1.0.0-alpha", -1},
		{"1.0.0-alpha", "1.0.0-1", 1},

		// Shorter < longer when prefix matches.
		{"1.0.0-rc", "1.0.0-rc.1", -1},
		{"1.0.0-rc.1", "1.0.0-rc", 1},

		// Build metadata ignored.
		{"1.0.0+a", "1.0.0+b", 0},
		{"1.0.0-rc.1+a", "1.0.0-rc.1+b", 0},

		// MMP wins over pre-release entirely.
		{"2.6.0-test.99", "2.6.1", -1},
		{"2.6.1", "2.6.0-test.99", 1},

		// Same identifiers compare equal.
		{"v2.6.0-test.27", "v2.6.0-test.27", 0},
	}
	for _, tt := range tests {
		got := CompareVersions(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
