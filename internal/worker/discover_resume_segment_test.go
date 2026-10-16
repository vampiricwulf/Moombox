package worker

import (
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestDiscoverResumeSegment pins the restart mapping between staging dirs,
// recorded segment rows, and the part a resumed Twitch job continues in.
// The "already muxed" cases are the load-bearing ones: before this mapping
// existed, a restart after a quality split appended new data into the root
// video_stream that was already muxed as part 1 — and finalize silently
// dropped everything captured after the restart.
func TestDiscoverResumeSegment(t *testing.T) {
	cases := []struct {
		name       string
		stagedDirs []string // dirs holding media, relative to staging ("" is the root)
		segRows    []int    // recorded segment indices
		tombstone  string   // a staged dir a merge tombstoned
		wantIdx    int
		wantDir    string // relative to staging ("" is the root)
	}{
		{name: "fresh job uses root", wantIdx: 0, wantDir: ""},
		{name: "unmuxed root data resumes part 1", stagedDirs: []string{""}, wantIdx: 0, wantDir: ""},
		// root was muxed as part 1
		{name: "unmuxed seg dir resumes that part", stagedDirs: []string{"", "seg_1"}, segRows: []int{0}, wantIdx: 1, wantDir: "seg_1"},
		// root already muxed (e.g. outage muxed part 1): a fresh seg_1
		{name: "muxed root must not be appended into", stagedDirs: []string{""}, segRows: []int{0}, wantIdx: 1, wantDir: "seg_1"},
		// all staged parts recorded: a fresh seg_3
		{name: "muxed seg dir advances to next index", stagedDirs: []string{"", "seg_1", "seg_2"}, segRows: []int{0, 1, 2}, wantIdx: 3, wantDir: "seg_3"},
		// root was short-skipped; seg_0 is part index 0
		{name: "short-skipped root with seg_0", stagedDirs: []string{"", "seg_0"}, wantIdx: 0, wantDir: "seg_0"},
		// A tombstoned seg_N dir must never be handed back as a resume target
		// -- appending fresh download data into a dir whose content the merge
		// already consumed would corrupt or duplicate that content. seg_1 (the
		// highest staged part, tombstoned) must be invisible entirely, which
		// leaves the root's unmuxed data as the part to resume. With part 0
		// recorded this case could not fail: the next fresh index is 1 either
		// way, and seg_1 is the tombstoned dir's own path.
		//
		// Mutant: stagedSegDirs listing tombstoned dirs (segDirsOf(dir, true))
		// -- the tombstoned seg_1 comes back as (1, seg_1).
		{name: "skips tombstoned seg dir", stagedDirs: []string{"", "seg_1"}, tombstone: "seg_1", wantIdx: 0, wantDir: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, db := newRenameTestOrch(t)
			if _, err := db.AddJob(&database.Job{ID: "j1", VideoID: "j1", URL: "https://twitch.tv/x", Platform: "twitch"}); err != nil {
				t.Fatalf("AddJob: %v", err)
			}
			staging := t.TempDir()
			for _, d := range tc.stagedDirs {
				stageRawMedia(t, filepath.Join(staging, d))
			}
			if tc.tombstone != "" {
				writeTombstone(t, filepath.Join(staging, tc.tombstone))
			}
			for _, idx := range tc.segRows {
				if err := db.AddSegment(&database.Segment{JobID: "j1", SegmentIndex: idx, Quality: "1080p60", Filename: "x.mp4"}); err != nil {
					t.Fatalf("AddSegment: %v", err)
				}
			}
			jobCtx := &JobContext{Job: &database.Job{ID: "j1"}, StagingDir: staging}
			idx, dir := o.discoverResumeSegment(jobCtx)
			if want := filepath.Join(staging, tc.wantDir); idx != tc.wantIdx || dir != want {
				t.Errorf("got (%d, %q), want (%d, %q)", idx, dir, tc.wantIdx, want)
			}
		})
	}
}
