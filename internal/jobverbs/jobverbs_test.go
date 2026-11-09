package jobverbs

import (
	"os"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/treeguard"
)

// TestMain runs the package's tests through treeguard.Run, which fails the
// run when they leave a file in this directory.
func TestMain(m *testing.M) { os.Exit(treeguard.Run(m)) }

// TestVerbStatusTable pins every verb's status set, status by status: the
// table the REST routes, the TUI and the worker verbs all answer from. A
// change here is a change in what a verb accepts, and the dashboard's sets
// (cmd/moombox TestVerbStatusSetsMatchTheDashboard) must move with it.
//
// Columns: cancel, resume (no incomplete tail), resume (incomplete tail),
// reinitialize, mux, mux needs unmuxed parts, delete, watched, trim, chapter
// import (complete), chapter import (incomplete tail).
func TestVerbStatusTable(t *testing.T) {
	type row struct {
		cancel, resume, resumeTail, reinit, mux, muxParts, del, watched, trim, chapters, chaptersTail bool
	}
	table := map[database.JobStatus]row{
		database.StatusQueued:      {cancel: true},
		database.StatusUpcoming:    {cancel: true},
		database.StatusLive:        {cancel: true},
		database.StatusDownloading: {cancel: true},
		database.StatusMuxing:      {cancel: true},
		database.StatusCookies:     {cancel: true, resume: true, resumeTail: true, reinit: true, del: true},
		database.StatusError:       {resume: true, resumeTail: true, reinit: true, mux: true, del: true},
		database.StatusCancelled:   {resume: true, resumeTail: true, reinit: true, mux: true, del: true},
		database.StatusFinished:    {resumeTail: true, mux: true, muxParts: true, del: true, watched: true, trim: true, chapters: true},
		"":                         {},
		"Bogus":                    {},
	}
	for s, want := range table {
		got := row{
			cancel:       Cancellable(s),
			resume:       ResumableStatus(s, false),
			resumeTail:   ResumableStatus(s, true),
			reinit:       ReinitializableStatus(s),
			mux:          MuxableStatus(s),
			muxParts:     MuxNeedsUnmuxedParts(s),
			del:          DeletableStatus(s),
			watched:      WatchableStatus(s),
			trim:         TrimmableStatus(s),
			chapters:     ChapterImportableStatus(s, false),
			chaptersTail: ChapterImportableStatus(s, true),
		}
		if got != want {
			t.Errorf("%q:\n got  %+v\n want %+v", s, got, want)
		}
	}
	if !ResumeSupported("youtube") || ResumeSupported("twitch") || ResumeSupported("") {
		t.Error("ResumeSupported: want youtube only")
	}
}
