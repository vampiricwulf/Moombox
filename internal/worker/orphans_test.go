package worker

import (
	"runtime"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	// Basic test: relative paths resolve to absolute
	result := normalizePath("test.txt")
	if result == "" {
		t.Error("normalizePath should return non-empty result")
	}

	// Case normalization on Windows
	if runtime.GOOS == "windows" {
		a := normalizePath("C:\\Users\\Test\\file.txt")
		b := normalizePath("c:\\users\\test\\file.txt")
		if a != b {
			t.Errorf("normalizePath should be case-insensitive on Windows: %q != %q", a, b)
		}
	}
}

func TestIsUnderDirectory(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		dir      string
		expected bool
	}{
		{
			name:     "path under directory",
			path:     "./output/subdir/file.mp4",
			dir:      "./output",
			expected: true,
		},
		{
			name:     "path is the directory itself",
			path:     "./output",
			dir:      "./output",
			expected: false, // Correctly rejects deleting the root directory
		},
		{
			name:     "path escapes via ..",
			path:     "./output/../secret/file.txt",
			dir:      "./output",
			expected: false,
		},
		{
			name:     "completely unrelated path",
			path:     "./other/file.txt",
			dir:      "./output",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isUnderDirectory(tt.path, tt.dir)
			if result != tt.expected {
				t.Errorf("isUnderDirectory(%q, %q) = %v, want %v",
					tt.path, tt.dir, result, tt.expected)
			}
		})
	}
}
