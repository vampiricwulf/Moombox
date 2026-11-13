package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// TestRecordDisplayAspect: a yt:stretch ratio is recorded beside the job's
// staged media as upstream hands it to FFmpeg (`%f`), and a ratio of 0 (no
// keyword) or 1 — the two values FFmpegFixupStretchedPP skips — clears an
// earlier record, so a re-run of a video whose keyword was dropped muxes
// square again; a record that does not read as a positive number is none.
//
// Mutants: the ratio-1 arm writing a record; the clear arm leaving the old
// record; stagedDisplayAspect accepting a non-numeric record.
func TestRecordDisplayAspect(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "video.mp4")
	if got := stagedDisplayAspect(video); got != "" {
		t.Fatalf("fresh staging reads %q", got)
	}
	if err := recordDisplayAspect(dir, 16.0/9); err != nil {
		t.Fatal(err)
	}
	if got := stagedDisplayAspect(video); got != "1.777778" {
		t.Errorf("recorded %q, want 1.777778", got)
	}
	for _, ratio := range []float64{1, 0} {
		if err := recordDisplayAspect(dir, 4.0/3); err != nil {
			t.Fatal(err)
		}
		if err := recordDisplayAspect(dir, ratio); err != nil {
			t.Fatal(err)
		}
		if got := stagedDisplayAspect(video); got != "" {
			t.Errorf("ratio %v left %q recorded", ratio, got)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, displayAspectFile), []byte("wide\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := stagedDisplayAspect(video); got != "" {
		t.Errorf("a garbled record reads %q, want none", got)
	}
	if got := stagedDisplayAspect(""); got != "" {
		t.Errorf("no video reads %q", got)
	}
}

// TestTheArchiveCopyAppliesTheRecordedAspect is G44 at the mux: the
// archive's stream copy of a staged video with a display-aspect record
// beside it (copyStaged, every archive, part and aside copy's one path)
// comes out at that aspect, its frames untouched; without one it is the
// copy it always was. Skipped where FFmpeg is not installed.
//
// Mutants: copyStaged passing "" for the aspect (the archive stays 1:1);
// stagedDisplayAspect reading the record from the output's directory
// instead of the staged video's (none found).
func TestTheArchiveCopyAppliesTheRecordedAspect(t *testing.T) {
	ffmpegPath, ffprobePath := requireFFmpegTools(t)
	w, _ := testWorkerSetup(t)
	staging, out := t.TempDir(), t.TempDir()
	video := filepath.Join(staging, "video.mp4")
	writeMuxFixture(t, ffmpegPath, video, 2) // 64x64: square pixels, 1:1
	dar := func(path string) string {
		t.Helper()
		b, err := exec.Command(ffprobePath, "-v", "error", "-select_streams", "v", "-show_entries", "stream=display_aspect_ratio", "-of", "csv=p=0", path).Output()
		if err != nil {
			t.Fatalf("ffprobe: %v", err)
		}
		return strings.TrimSpace(string(b))
	}

	plain := filepath.Join(out, "plain.mp4")
	if _, err := w.orchestrator.copyStaged(context.Background(), "j", video, "", plain, "", nil); err != nil {
		t.Fatalf("copyStaged: %v", err)
	}
	if got := dar(plain); got != "1:1" {
		t.Errorf("without a record the copy is %q, want 1:1", got)
	}
	if err := recordDisplayAspect(staging, 16.0/9); err != nil {
		t.Fatal(err)
	}
	stretched := filepath.Join(out, "stretched.mp4")
	if _, err := w.orchestrator.copyStaged(context.Background(), "j", video, "", stretched, "", nil); err != nil {
		t.Fatalf("copyStaged: %v", err)
	}
	if got := dar(stretched); got != "16:9" {
		t.Errorf("with the record the copy is %q, want 16:9", got)
	}
}

// TestExecuteWithChatRecordsTheStretchBeforeDownloading pins the one
// production writer of the display-aspect record: ExecuteWithChat, every
// YouTube download's setup, records the extraction's
// VideoInfo.StretchedRatio in the staging root before the download starts —
// the whole-file server finds it there at its first request — and a video
// without a stretch clears one an earlier run left, so a re-run after the
// keyword was dropped muxes square. The run itself is cut at that first
// request (the operator's Cancel), so nothing here is muxed.
//
// Mutants: the recordDisplayAspect call removed (no record is found, the
// stale one stays); handed 0 instead of videoInfo.StretchedRatio (no record
// for the stretched run); moved after the download (the server sees none).
func TestExecuteWithChatRecordsTheStretchBeforeDownloading(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ratio float64
		stale bool // an earlier run's record is in staging
		want  string
	}{
		{"stretched", 16.0 / 9, false, "1.777778"},
		{"square clears a stale record", 0, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			job := &database.Job{ID: "yt_stretch", VideoID: "stretch1234", URL: "u", Platform: "youtube",
				Title: "T", ChannelName: "C", Status: database.StatusUpcoming, OutputDirectory: t.TempDir()}
			if _, err := db.AddJob(job); err != nil {
				t.Fatal(err)
			}
			stored, _ := db.GetJob(job.ID)
			jobCtx := w.buildJobContext(stored)
			if tc.stale {
				if err := recordDisplayAspect(jobCtx.StagingDir, 4.0/3); err != nil {
					t.Fatal(err)
				}
			}
			video := filepath.Join(jobCtx.StagingDir, "video.mp4")
			var seen atomic.Pointer[string]
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				got := stagedDisplayAspect(video)
				if seen.CompareAndSwap(nil, &got) {
					db.UpdateJobFieldsUnlessTerminal(job.ID, map[string]any{"status": database.StatusCancelled})
				}
				http.Error(rw, "cut", http.StatusServiceUnavailable)
			}))
			defer srv.Close()
			info := wholeFileVodInfo(srv, 1024)
			info.StretchedRatio = tc.ratio
			o := NewDownloadOrchestrator(db, nil, "", discardLogger{}, nil, stubCipherSolver{}, nil, notificationtest.New(), nil)
			_ = o.ExecuteWithChat(context.Background(), jobCtx, info, true, nil)
			got := seen.Load()
			if got == nil {
				t.Fatal("the download never reached the server")
			}
			if *got != tc.want {
				t.Errorf("the record at the first request = %q, want %q", *got, tc.want)
			}
		})
	}
}
