package config

import "testing"

func TestPathHasTraversal(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		// Empty and ordinary relative values.
		{"", false},
		{"output", false},
		{"output/sub", false},
		{"output/sub/file.txt", false},
		{"./output", false},
		{".\\output", false},

		// Absolute paths are legitimate — the Docker image seeds these.
		{"/data/output", false},
		{"/data/cookies.txt", false},
		{`C:\Moombox\data`, false},
		{`c:\moombox\data`, false},
		{`\\server\share\moombox`, false},
		{"\\abs\\path", false},

		// Two dots inside a NAME are not traversal. The old substring check
		// rejected all of these.
		{"my..file.txt", false},
		{"..hidden", false},
		{"..hidden/file", false},
		{"a/b..c/d", false},
		{"/data/my..dir/out", false},

		// Real ".." segments.
		{"..", true},
		{"../escape", true},
		{"output/../escape", true},
		{`..\escape`, true},
		{`C:\data\..\escape`, true},
		{"/data/../etc", true},
		{"a/b/..", true},
		{"a//../b", true},

		// Drive-relative traversal: "C:..\x" means "parent of the CWD on C:".
		{`C:..\escape`, true},
		{"C:../escape", true},

		// Windows drops trailing spaces from a component, so ".. " resolves
		// to "..".
		{"a/.. /b", true},
	}
	for _, tt := range tests {
		if got := PathHasTraversal(tt.in); got != tt.want {
			t.Errorf("PathHasTraversal(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
