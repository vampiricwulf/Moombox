package updater

import "testing"

func TestParseVersion(t *testing.T) {
	tests := []struct {
		input                  string
		major, minor, patch    int
		wantErr                bool
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
