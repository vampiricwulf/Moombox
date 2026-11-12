package worker

import (
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestAWaitingMonitorRowFollowsTheOutputSetting is owner ruling R2-X1: a
// monitor-detected row that is still waiting writes to the directory the
// settings name when its download starts (the channel's override, else the
// global one), and the row is rewritten to say so; a row that has started,
// or has an archive or a part, keeps the directory it has, and a manually
// added row is left as it was.
//
// Mutants: buildJobContext not settling (every waiting row keeps the old
// directory); followsOutputSetting ignoring the status, output_file, the
// parts or manually_added (the matching "keeps" row moves); the channel
// override not read, or matched without the Twitch login (the override rows
// take the global directory); settledOutputDirectory stamping a blank row
// (the blank rows stop reading the setting).
func TestAWaitingMonitorRowFollowsTheOutputSetting(t *testing.T) {
	w, db := testWorkerSetup(t)
	base := t.TempDir()
	oldDir, newDir := filepath.Join(base, "old"), filepath.Join(base, "new")
	ytDir, twDir := filepath.Join(base, "yt"), filepath.Join(base, "tw")
	own := filepath.Join(base, "own")
	w.cfg.Paths.OutputDirectory = newDir
	w.cfg.Channels = []config.ChannelConfig{
		{ID: "UC_override", OutputDirectory: ytDir},
		{ID: "UC_plain"},
		{ID: "SomeStreamer", Platform: "twitch", OutputDirectory: twDir},
	}
	plain, override, gone := "UC_plain", "UC_override", "UC_removed"

	cases := []struct {
		name    string
		job     database.Job
		part    bool
		wantDir string // the directory the run writes to
		wantRow string // output_directory afterwards
	}{
		{"upcoming", database.Job{Status: database.StatusUpcoming, ChannelID: &plain, OutputDirectory: oldDir}, false, newDir, newDir},
		{"queued", database.Job{Status: database.StatusQueued, ChannelID: &plain, OutputDirectory: oldDir}, false, newDir, newDir},
		{"live", database.Job{Status: database.StatusLive, ChannelID: &plain, OutputDirectory: oldDir}, false, newDir, newDir},
		{"removed channel", database.Job{Status: database.StatusQueued, ChannelID: &gone, OutputDirectory: oldDir}, false, newDir, newDir},
		{"channel override", database.Job{Status: database.StatusUpcoming, ChannelID: &override, OutputDirectory: oldDir}, false, ytDir, ytDir},
		{"twitch live by login", database.Job{Status: database.StatusLive, Platform: "twitch", URL: "https://twitch.tv/somestreamer", OutputDirectory: oldDir}, false, twDir, twDir},
		{"blank stays blank", database.Job{Status: database.StatusQueued, ChannelID: &plain}, false, newDir, ""},
		{"blank takes an override", database.Job{Status: database.StatusQueued, ChannelID: &override}, false, ytDir, ytDir},

		{"downloading keeps", database.Job{Status: database.StatusDownloading, ChannelID: &plain, OutputDirectory: oldDir}, false, oldDir, oldDir},
		{"error keeps", database.Job{Status: database.StatusError, ChannelID: &plain, OutputDirectory: oldDir}, false, oldDir, oldDir},
		{"finished keeps", database.Job{Status: database.StatusFinished, ChannelID: &plain, OutputDirectory: oldDir}, false, oldDir, oldDir},
		{"archive keeps", database.Job{Status: database.StatusUpcoming, ChannelID: &plain, OutputDirectory: oldDir, OutputFile: filepath.Join(oldDir, "a.mp4")}, false, oldDir, oldDir},
		{"part keeps", database.Job{Status: database.StatusQueued, ChannelID: &plain, OutputDirectory: oldDir}, true, oldDir, oldDir},
		{"manual own keeps", database.Job{Status: database.StatusUpcoming, ChannelID: &override, ManuallyAdded: true, OutputDirectory: own}, false, own, own},
		{"manual blank", database.Job{Status: database.StatusUpcoming, ChannelID: &override, ManuallyAdded: true}, false, newDir, ""},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := tc.job
			job.ID = "odir_" + string(rune('a'+i))
			job.VideoID = job.ID
			job.Title = "t"
			if job.Platform == "" {
				job.Platform = "youtube"
			}
			if _, err := db.AddJob(&job); err != nil {
				t.Fatal(err)
			}
			if tc.part {
				if err := db.AddSegment(&database.Segment{JobID: job.ID, SegmentIndex: 0, Filename: "p1.mp4", FilePath: filepath.Join(oldDir, "p1.mp4")}); err != nil {
					t.Fatal(err)
				}
			}
			row, _ := db.GetJob(job.ID)
			ctx := w.buildJobContext(row)
			if ctx.OutputDir != tc.wantDir || ctx.Config.OutputDirectory != tc.wantDir {
				t.Errorf("run writes to %q (config %q), want %q", ctx.OutputDir, ctx.Config.OutputDirectory, tc.wantDir)
			}
			after, _ := db.GetJob(job.ID)
			if after.OutputDirectory != tc.wantRow {
				t.Errorf("row output_directory = %q, want %q", after.OutputDirectory, tc.wantRow)
			}
			if row.OutputDirectory != after.OutputDirectory {
				t.Errorf("the run's copy of the row says %q, the row %q", row.OutputDirectory, after.OutputDirectory)
			}
		})
	}
}

// TestASettledOutputDirectoryStaysWithTheJob: the directory a waiting row
// took when its download started is the row's own from then on. A later
// change of the setting does not move a Downloading job, whose mux and
// Resume build their context from the row.
//
// Mutant: settleOutputDirectory not writing the row (the second context
// follows the setting again, away from the first one's files).
func TestASettledOutputDirectoryStaysWithTheJob(t *testing.T) {
	w, db := testWorkerSetup(t)
	base := t.TempDir()
	first, second := filepath.Join(base, "first"), filepath.Join(base, "second")
	ch := "UC_plain"
	job := &database.Job{ID: "odir_stay", VideoID: "odir_stay", Title: "t", Platform: "youtube",
		Status: database.StatusUpcoming, ChannelID: &ch, OutputDirectory: filepath.Join(base, "detected")}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	w.cfg.Paths.OutputDirectory = first
	row, _ := db.GetJob(job.ID)
	if got := w.buildJobContext(row).OutputDir; got != first {
		t.Fatalf("first run writes to %q, want %q", got, first)
	}
	db.UpdateJobFields(job.ID, map[string]any{"status": database.StatusDownloading})
	w.cfg.Paths.OutputDirectory = second
	row, _ = db.GetJob(job.ID)
	if got := w.buildJobContext(row).OutputDir; got != first {
		t.Errorf("after the setting changed mid-download the job writes to %q, want %q", got, first)
	}
}

// TestSettleOutputDirectoryLeavesARowThatMovedOn: the settle is decided on
// the row as the run read it, so it is written only while the row is still
// waiting. One that was cancelled in between keeps its directory, and the
// run's copy keeps naming what the row names.
//
// Mutant: an unconditional UpdateJobFields in settleOutputDirectory.
func TestSettleOutputDirectoryLeavesARowThatMovedOn(t *testing.T) {
	w, db := testWorkerSetup(t)
	base := t.TempDir()
	oldDir := filepath.Join(base, "old")
	w.cfg.Paths.OutputDirectory = filepath.Join(base, "new")
	ch := "UC_plain"
	job := &database.Job{ID: "odir_moved", VideoID: "odir_moved", Title: "t", Platform: "youtube",
		Status: database.StatusUpcoming, ChannelID: &ch, OutputDirectory: oldDir}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	row, _ := db.GetJob(job.ID)
	db.UpdateJobFields(job.ID, map[string]any{"status": database.StatusCancelled})
	ctx := w.buildJobContext(row)
	after, _ := db.GetJob(job.ID)
	if after.OutputDirectory != oldDir || ctx.OutputDir != oldDir {
		t.Errorf("row = %q, run = %q, want both %q", after.OutputDirectory, ctx.OutputDir, oldDir)
	}
}
