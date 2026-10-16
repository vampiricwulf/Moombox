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

// TestUnquotePath pins the one-matching-pair rule.
//
// Mutants it kills (each compiles):
//   - the `p[len(p)-1] == q` match dropped (any quote closes): `"a b'` comes
//     out unquoted;
//   - a loop taking every pair off: `""a b""` comes out as `a b`;
//   - `len(p) >= 2` relaxed to `>= 1`: `"` slices p[1:0] and panics;
//   - single quotes not recognised: `'a b'` comes out quoted.
func TestUnquotePath(t *testing.T) {
	for in, want := range map[string]string{
		`"a b"`:    `a b`,
		`'a b'`:    `a b`,
		`""`:       ``,
		`"`:        `"`,
		`"a b'`:    `"a b'`,
		`""a b""`:  `"a b"`,
		`a "b" c`:  `a "b" c`,
		`a b`:      `a b`,
		`'it's'`:   `it's`,
		`C:\x y\z`: `C:\x y\z`,
	} {
		if got := UnquotePath(in); got != want {
			t.Errorf("UnquotePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPathIsQuoted: a value wrapped in a matching pair of quotes — spaces
// around the pair aside, as a paste leaves them — is quoted; one quote, a
// quote inside the path or none is not.
//
// Mutants it kills: PathIsQuoted without its TrimSpace (the padded row is
// not quoted); comparing against the untrimmed value (every padded unquoted
// row is quoted).
func TestPathIsQuoted(t *testing.T) {
	for in, want := range map[string]bool{
		`"E:\Archive"`:     true,
		`'/data/output'`:   true,
		` "E:\Archive" `:   true,
		`E:\Archive`:       false,
		` E:\Archive `:     false,
		`"E:\Archive`:      false,
		`E:\My "x" Folder`: false,
		``:                 false,
	} {
		if got := PathIsQuoted(in); got != want {
			t.Errorf("PathIsQuoted(%q) = %v, want %v", in, got, want)
		}
	}
}
