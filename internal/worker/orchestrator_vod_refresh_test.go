package worker

import (
	"path/filepath"
	"regexp"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

func TestComputeIncompleteTail(t *testing.T) {
	if !computeIncompleteTail(true, false) || !computeIncompleteTail(false, true) {
		t.Error("either downloader behind head must flag the job")
	}
	if computeIncompleteTail(false, false) {
		t.Error("clean finish must not flag")
	}
}

// TestFinalizeIncompleteTailWritesAndSelfClears locks in the shared helper
// FIX 3 introduces for both the VOD and live branches of ExecuteWithChat: a
// clean finish (both downloaders nil, i.e. neither ever behind head) must
// write incomplete_tail=false unconditionally — the same write path a
// previously-flagged job's successful retry/resume relies on to self-clear
// the flag.
func TestFinalizeIncompleteTailWritesAndSelfClears(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	jobID := "yt_finalizeIncompleteTailTest"
	job := &database.Job{
		ID:      jobID,
		VideoID: "finalizeIncompleteTailTest",
		URL:     "https://youtube.com/watch?v=finalizeIncompleteTailTest",
		Status:  database.StatusDownloading,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	// Seed the job as previously flagged, to prove the write below is
	// unconditional and self-clears it on a clean finish.
	if updated := db.UpdateJobFields(jobID, map[string]any{"incomplete_tail": true}); updated == nil {
		t.Fatal("UpdateJobFields(incomplete_tail) returned nil")
	}

	o := &DownloadOrchestrator{db: db, logger: &discardLogger{}}

	incomplete, vSeq, vHead, aSeq, aHead := o.finalizeIncompleteTail(jobID, &DownloadResult{})
	if incomplete {
		t.Error("both downloaders nil must never be reported incomplete")
	}
	if vSeq != 0 || aSeq != 0 || vHead != -1 || aHead != -1 {
		t.Errorf("nil-downloader seq/head values = (%d,%d,%d,%d), want (0,-1,0,-1)", vSeq, vHead, aSeq, aHead)
	}

	got, err := db.GetJob(jobID)
	if err != nil || got == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.IncompleteTail {
		t.Error("finalizeIncompleteTail must self-clear a previously-flagged job on a clean finish")
	}
}

// TestIncompleteProgressString pins FIX 4's honest-progress format: the same
// DASH-style "(A: x/y V: x/y[ C: n])" shape ProgressTracker.buildProgressString
// (progress.go) uses, omitting the "/head" part when head <= 0 (downloaderHead
// returns -1 for a nil downloader), and — per the fix's explicit compatibility
// note — still matching the exact regex app.js's dashMatch uses to parse it.
func TestIncompleteProgressString(t *testing.T) {
	dashMatch := regexp.MustCompile(`\(A:\s*(\S+)\s+V:\s*(\S+)(?:\s+C:\s*(\d+))?\)`)

	cases := []struct {
		name                                string
		vSeq, vHead, aSeq, aHead, chatCount int
		wantStr                             string
		wantPercent                         float64
	}{
		{"both heads known", 500, 1200, 500, 1200, 0, "(A: 500/1200 V: 500/1200)", float64(500) / 1200 * 100},
		{"unknown head (-1) omits slash, percent 0", 500, -1, 500, -1, 0, "(A: 500 V: 500)", 0},
		{"video head 0 omits slash, percent stays 0", 10, 0, 10, 20, 0, "(A: 10/20 V: 10)", 0},
		{"chat count appended", 500, 1200, 480, 1200, 42, "(A: 480/1200 V: 500/1200 C: 42)", float64(500) / 1200 * 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotStr, gotPercent := incompleteProgressString(c.vSeq, c.vHead, c.aSeq, c.aHead, c.chatCount)
			if gotStr != c.wantStr {
				t.Errorf("string = %q, want %q", gotStr, c.wantStr)
			}
			if gotPercent != c.wantPercent {
				t.Errorf("percent = %v, want %v", gotPercent, c.wantPercent)
			}
			if !dashMatch.MatchString(gotStr) {
				t.Errorf("%q does not match app.js's dashMatch regex", gotStr)
			}
		})
	}
}

func TestVodRefreshDecision(t *testing.T) {
	cases := []struct {
		name                       string
		behindHead, progressed     bool
		attempt                    int
		manifestlessStillAvailable bool
		want                       bool
	}{
		{"incomplete with progress refreshes", true, true, 1, true, true},
		{"complete finalize stops", false, true, 1, true, false},
		{"no progress stops (avoid API spin)", true, false, 1, true, false},
		{"attempts exhausted stops", true, true, maxVodRefreshAttempts, true, false},
		{"stream became true VOD stops", true, true, 1, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldRefreshVodDownload(c.behindHead, c.progressed, c.attempt, c.manifestlessStillAvailable)
			if got != c.want {
				t.Errorf("shouldRefreshVodDownload = %v, want %v", got, c.want)
			}
		})
	}
}

// TestRefreshFormatMatches guards the VOD refresh loop's mixed-codec-append
// prevention: refreshDownload re-runs format selection against a freshly
// re-extracted pool every attempt, and SelectBestDashStream silently falls
// back to a different itag when the previously pinned one has vanished. If
// the fresh selection picked a different video identity than the one
// already on disk, appending fresh's segments under the old init produces a
// silently corrupt mixed-codec file — refreshFormatMatches is the guard
// that must catch that before the caller commits to `fresh`.
func TestRefreshFormatMatches(t *testing.T) {
	itag137 := &youtube.Format{Itag: 137}
	itag248 := &youtube.Format{Itag: 248}
	itag140 := &youtube.Format{Itag: 140}
	itag251 := &youtube.Format{Itag: 251}

	base := func() *DownloadResult {
		return &DownloadResult{
			HasVideo: true, HasAudio: true,
			VideoWidth: 1920, VideoHeight: 1080, VideoFps: 30,
		}
	}

	tests := []struct {
		name string
		old  *DownloadResult
		new  *DownloadResult
		want bool
	}{
		{
			name: "identical format matches",
			old:  base(),
			new:  base(),
			want: true,
		},
		{
			name: "resolution drift does not match",
			old:  base(),
			new: func() *DownloadResult {
				r := base()
				r.VideoWidth, r.VideoHeight = 1280, 720
				return r
			}(),
			want: false,
		},
		{
			name: "fps drift does not match",
			old:  base(),
			new: func() *DownloadResult {
				r := base()
				r.VideoFps = 60
				return r
			}(),
			want: false,
		},
		{
			name: "video disappearing is a shape change, not a match",
			old:  base(),
			new: func() *DownloadResult {
				r := base()
				r.HasVideo = false
				return r
			}(),
			want: false,
		},
		{
			name: "audio disappearing is a shape change, not a match",
			old:  base(),
			new: func() *DownloadResult {
				r := base()
				r.HasAudio = false
				return r
			}(),
			want: false,
		},
		{
			name: "audio-only jobs match on the audio-only shape alone",
			old:  &DownloadResult{HasAudio: true},
			new:  &DownloadResult{HasAudio: true},
			want: true,
		},
		{
			name: "identical resolution but different video itag does not match",
			old:  func() *DownloadResult { r := base(); r.VideoFormat = itag137; return r }(),
			new:  func() *DownloadResult { r := base(); r.VideoFormat = itag248; return r }(),
			want: false,
		},
		{
			name: "same video itag matches",
			old:  func() *DownloadResult { r := base(); r.VideoFormat = itag137; return r }(),
			new:  func() *DownloadResult { r := base(); r.VideoFormat = itag137; return r }(),
			want: true,
		},
		{
			name: "different audio itag does not match even with identical video",
			old:  func() *DownloadResult { r := base(); r.AudioFormat = itag140; return r }(),
			new:  func() *DownloadResult { r := base(); r.AudioFormat = itag251; return r }(),
			want: false,
		},
		{
			name: "one side missing VideoFormat skips itag check (falls back to dims)",
			old:  func() *DownloadResult { r := base(); r.VideoFormat = itag137; return r }(),
			new:  base(),
			want: true,
		},
		{
			name: "nil old is never a match",
			old:  nil,
			new:  base(),
			want: false,
		},
		{
			name: "nil fresh is never a match",
			old:  base(),
			new:  nil,
			want: false,
		},
		{
			name: "both nil is never a match",
			old:  nil,
			new:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := refreshFormatMatches(tt.old, tt.new); got != tt.want {
				t.Errorf("refreshFormatMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestKeepIncompleteTailProgressPreservesHonestValues locks in the finalize
// half of FIX 4: the honest progress/percent written when a download gave up
// behind head must survive muxAndFinalize/finalizeMultiSegmentJob, which
// otherwise hard-code percent=100 for every job that muxes successfully. A
// clean job's update map must pass through untouched.
func TestKeepIncompleteTailProgressPreservesHonestValues(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	o := &DownloadOrchestrator{db: db, logger: &discardLogger{}}

	newJob := func(id string, incomplete bool) string {
		job := &database.Job{
			ID:      id,
			VideoID: id,
			URL:     "https://youtube.com/watch?v=" + id,
			Status:  database.StatusMuxing,
		}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		if incomplete {
			if updated := db.UpdateJobFields(id, map[string]any{"incomplete_tail": true}); updated == nil {
				t.Fatal("UpdateJobFields(incomplete_tail) returned nil")
			}
		}
		return id
	}

	// Flagged job: the finalize overwrites must be stripped so the honest
	// values already in the row survive.
	flagged := map[string]any{"status": "Finished", "progress": "", "percent": 100.0}
	o.keepIncompleteTailProgress(newJob("flaggedTailJob", true), flagged)
	if _, ok := flagged["percent"]; ok {
		t.Error("percent overwrite not stripped for a flagged job — bar would read 100% on a truncated recording")
	}
	if _, ok := flagged["progress"]; ok {
		t.Error("progress overwrite not stripped for a flagged job")
	}
	if flagged["status"] != "Finished" {
		t.Error("unrelated keys must be left alone")
	}

	// Clean job: untouched.
	clean := map[string]any{"status": "Finished", "progress": "", "percent": 100.0}
	o.keepIncompleteTailProgress(newJob("cleanTailJob", false), clean)
	if clean["percent"] != 100.0 || clean["progress"] != "" {
		t.Errorf("clean job's finalize map was modified: %+v", clean)
	}

	// Unknown job (DB read fails): fall back to pre-existing behavior.
	unknown := map[string]any{"percent": 100.0}
	o.keepIncompleteTailProgress("nonexistent-job", unknown)
	if unknown["percent"] != 100.0 {
		t.Error("a failed job lookup must leave the map untouched (pre-existing 100% behavior)")
	}
}
