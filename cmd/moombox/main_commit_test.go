package main

import (
	"runtime/debug"
	"testing"
)

// TestResolveBuildCommitMarksADirtyTree: `go build` in a working tree with
// uncommitted changes stamps the SAME vcs.revision it would stamp clean, so a
// rebuild serves different app.js bytes under an identical commit string. The
// web server uses that string as its ETag, so a local-dev rebuild answered the
// browser's If-None-Match with 304 and the page kept running the old module
// (sweep R5-4). The "-dirty" suffix is what internal/web's trustedCommit tests
// for before it trusts the commit as a cache validator.
//
// THE MUTANT: drop the vcs.modified lookup (or the suffix) — the dirty case
// returns the bare "abc1234" and this fails.
func TestResolveBuildCommitMarksADirtyTree(t *testing.T) {
	tests := []struct {
		name     string
		settings []debug.BuildSetting
		want     string
	}{
		{
			name: "clean tree keeps the bare 7-char revision",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abc1234567890"},
				{Key: "vcs.modified", Value: "false"},
			},
			want: "abc1234",
		},
		{
			name: "dirty tree is suffixed",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abc1234567890"},
				{Key: "vcs.modified", Value: "true"},
			},
			want: "abc1234-dirty",
		},
		{
			// Settings order is not part of go's contract, and the previous
			// resolver returned on the first vcs.revision it saw — which would
			// miss a vcs.modified that sorts after it.
			// THE MUTANT: return early inside the loop on vcs.revision.
			name: "modified is read even when it follows the revision",
			settings: []debug.BuildSetting{
				{Key: "-ldflags", Value: "-s -w"},
				{Key: "vcs.revision", Value: "deadbeefcafe"},
				{Key: "vcs.modified", Value: "true"},
			},
			want: "deadbee-dirty",
		},
		{
			// Only the literal "true" means dirty; anything else is clean.
			name:     "no vcs stamp at all",
			settings: []debug.BuildSetting{{Key: "-trimpath", Value: "true"}},
			want:     "",
		},
		{
			name:     "short revision is unusable",
			settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc12"}},
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveBuildCommit(tt.settings); got != tt.want {
				t.Errorf("resolveBuildCommit() = %q, want %q", got, tt.want)
			}
		})
	}
}
