package worker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/disk"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// The chapter import's claim honoured by every verb that would race its
// re-mux (owner decision PR-M5). The import replaces the archive in place
// with a copy of the archive as it read it; a run's finalize that wrote a new
// archive at that name meanwhile was renamed over by the old, shorter one.

// importClaimWorker is a worker over a real database with one Error row and
// no FFmpeg, for the claim's waits.
func importClaimWorker(t *testing.T) (*DownloadWorker, *database.Database, string) {
	t.Helper()
	db := openTestDB(t)
	cfg := &config.MoomboxConfig{}
	cfg.Paths.StagingDirectory = filepath.Join(t.TempDir(), "staging")
	w := NewDownloadWorker(db, nil, cfg, &discardLogger{}, &DownloadWorkerDeps{})
	id := "yt_claim"
	if _, err := db.AddJob(&database.Job{ID: id, VideoID: "claim", Platform: "youtube", URL: "u",
		Status: database.StatusError, OutputDirectory: t.TempDir(), OutputFile: filepath.Join(t.TempDir(), "archive.mp4")}); err != nil {
		t.Fatal(err)
	}
	return w, db, id
}

// TestResumeAndReinitializeWaitForAChapterImport: a Resume or a Reinitialize
// that reaches the worker while an import holds the job's claim — the TUI's,
// the monitor's auto-resume, one the REST layer let through just before the
// claim was taken — waits for the import, and then runs. It ran at once,
// starting a run whose finalize the re-mux's rename then wrote over.
//
// Mutant: afterJobExit not asking ChapterImportRunning — the row reads
// Downloading (or Upcoming) while the import still holds the claim.
func TestResumeAndReinitializeWaitForAChapterImport(t *testing.T) {
	for _, tc := range []struct {
		name string
		verb func(w *DownloadWorker, id string)
		want database.JobStatus
	}{
		{"resume", (*DownloadWorker).ResumeJob, database.StatusDownloading},
		{"reinitialize", (*DownloadWorker).ReinitializeJob, database.StatusUpcoming},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w, db, id := importClaimWorker(t)
				release, err := w.ClaimChapterImport(id)
				if err != nil {
					t.Fatalf("claim: %v", err)
				}
				tc.verb(w, id)
				synctest.Wait()
				if row, _ := db.GetJob(id); row.Status != database.StatusError {
					t.Fatalf("status = %s while the import holds the claim, want the row untouched (Error)", row.Status)
				}
				release()
				w.wg.Wait()
				if row, _ := db.GetJob(id); row.Status != tc.want {
					t.Errorf("status = %s once the import is done, want %s", row.Status, tc.want)
				}
			})
		})
	}
}

// TestFinalizeWaitsForAChapterImport: a run's finalize holds while an import
// holds the job's claim, writing nothing — not even its Muxing status —
// and gives up with the run's context. The wiring is the worker's
// (waitArchiveRewrite = waitChapterImport).
//
// Mutants: muxAndFinalize not calling waitArchiveRewrite — the row goes
// Muxing under the import; NewDownloadWorker not installing it — the same.
func TestFinalizeWaitsForAChapterImport(t *testing.T) {
	w, db, id := importClaimWorker(t)
	db.UpdateJobFields(id, map[string]any{"status": database.StatusDownloading})
	if w.orchestrator.waitArchiveRewrite == nil {
		t.Fatal("NewDownloadWorker left the orchestrator's waitArchiveRewrite nil")
	}
	// The claim, as ClaimChapterImport takes it (that one refuses an active
	// row: this is a run already going when the import began).
	release, err := w.claimJobOperation(id, opChapterImport)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the run gives up while it waits
	job, _ := db.GetJob(id)
	err = w.orchestrator.muxAndFinalize(ctx, w.buildJobContext(job), &DownloadResult{HasVideo: true})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("muxAndFinalize = %v, want the run's cancellation while it waited", err)
	}
	if row, _ := db.GetJob(id); row.Status != database.StatusDownloading {
		t.Errorf("status = %s, want nothing written while the import holds the claim", row.Status)
	}
}

// TestWaitChapterImportReturnsOnRelease: the wait ends at the claim's
// release, not on a poll, and never for a claim that is not an import's.
func TestWaitChapterImportReturnsOnRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, _, id := importClaimWorker(t)
		muxRelease, err := w.claimJobOperation(id, opMux)
		if err != nil {
			t.Fatal(err)
		}
		if !w.waitChapterImport(context.Background(), id, "a test") {
			t.Fatal("waited on the Mux action's claim")
		}
		muxRelease()

		release, err := w.claimJobOperation(id, opChapterImport)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan bool, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic: %v", r)
				}
			}()
			done <- w.waitChapterImport(context.Background(), id, "a test")
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("the wait returned while the import held the claim")
		default:
		}
		release()
		if !<-done {
			t.Error("the wait reported its context ended")
		}
	})
}

// TestRemuxArchiveChaptersRefusesARowThatMovedOn: the re-mux re-reads the row
// immediately before its rename, and a row no longer in the status the import
// read, or with a run in flight, keeps its archive as it is: a Resume that
// landed while FFmpeg wrote may have a new archive at that name by now. The
// check only asked whether the row was deleted.
//
// Mutant: the check not calling archiveStillAsRead — the copy of the old
// archive replaces the file in both rows.
func TestRemuxArchiveChaptersRefusesARowThatMovedOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(r *importRig)
	}{
		{"status moved", func(r *importRig) {
			r.db.UpdateJobFields(r.job.ID, map[string]any{"status": database.StatusDownloading})
		}},
		{"a run in flight", func(r *importRig) {
			r.w.queue.Enqueue(r.job.ID, database.StatusLive)
			if got, _, ok := r.w.queue.Dequeue(context.Background()); !ok || got != r.job.ID {
				t.Fatalf("Dequeue = %q %v", got, ok)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newImportRig(t)
			prev := remuxCheck
			remuxCheck = func(orig, out *archiveShape, want int) error {
				tc.move(r) // lands while FFmpeg wrote, as a Resume would
				return verifyArchiveRemux(orig, out, want)
			}
			t.Cleanup(func() { remuxCheck = prev })
			before, err := os.ReadFile(r.archive)
			if err != nil {
				t.Fatal(err)
			}
			list := utils.SanitizeChapters([]utils.Chapter{{StartMs: 0, Title: "one"}, {StartMs: 2000, Title: "two"}}, 4000)
			_, err = r.w.RemuxArchiveChapters(r.job.ID, database.StatusFinished, r.archive, list)
			if err == nil || !strings.Contains(err.Error(), "left as it was") {
				t.Fatalf("RemuxArchiveChapters = %v, want the archive left as it was", err)
			}
			if after, _ := os.ReadFile(r.archive); !bytes.Equal(before, after) {
				t.Error("the archive was replaced under a row that moved on")
			}
		})
	}
}

// TestGapFillSkipsWhileAChapterImportRuns: a due plan whose job's archive a
// chapter import is rewriting is left as it is, due, for the next sweep: the
// fill would add parts beside the archive and refit its chapter list while
// the import replaces both.
//
// Mutant: step not asking archiveRewriting — the VOD is looked up and the
// fill runs under the import.
func TestGapFillSkipsWhileAChapterImportRuns(t *testing.T) {
	f := newGapFillFixture(t)
	f.plan(t, []stagedPartCoverage{covPart(0, 25*time.Second, time.Minute, false)},
		[]gapSpan{{FromMs: ms(0), ToMs: ms(25 * time.Second), Kind: gapKindLateStart, AfterPart: -1}},
		gapStart.Add(25*time.Hour))
	looked := false
	findVod := f.filler.findVod
	f.filler.findVod = func(ctx context.Context, login string, a, b time.Time) (string, error) {
		looked = true
		return findVod(ctx, login, a, b)
	}
	f.filler.archiveRewriting = func(jobID string) bool { return jobID == "tw_1" }
	row, _ := f.db.GetGapFill("tw_1")
	f.filler.step(context.Background(), *row, gapStart.Add(70*time.Minute))
	if looked {
		t.Error("the fill ran while a chapter import held the archive")
	}
	after, _ := f.db.GetGapFill("tw_1")
	if after == nil || after.NextCheckAt != row.NextCheckAt {
		t.Errorf("plan after the skip = %+v, want it kept as it was (%s)", after, row.NextCheckAt)
	}
	if job, _ := f.db.GetJob("tw_1"); len(job.Segments) != 0 {
		t.Errorf("parts were added under the import: %+v", job.Segments)
	}
}

// The worker's filler asks the worker's claim (newTwitchGapFiller).
func TestWorkerGapFillerAsksTheImportClaim(t *testing.T) {
	w, _, id := importClaimWorker(t)
	f := w.newTwitchGapFiller()
	if f.archiveRewriting == nil {
		t.Fatal("the worker's filler has no archiveRewriting")
	}
	release, err := w.ClaimChapterImport(id)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !f.archiveRewriting(id) {
		t.Error("the filler does not see the import's claim")
	}
}

// TestRemuxRoom pins the chapter re-mux's disk check (owner decision PR-I4):
// the archive's volume needs the archive's size plus remuxHeadroom free, and
// the copy must not take it to disk_critical. A volume that cannot be read
// is not refused.
//
// Mutants: the free-space comparison dropped (the 1 GiB-short row passes);
// the projected-usage check dropped, or judged on the reading before the
// copy (the 93%-plus-the-copy row passes); an unreadable volume refused.
func TestRemuxRoom(t *testing.T) {
	db := openTestDB(t)
	cfg := &config.MoomboxConfig{}
	cfg.Paths.StagingDirectory = filepath.Join(t.TempDir(), "staging")
	cfg.Disk.CriticalPercent = 95
	w := NewDownloadWorker(db, nil, cfg, &discardLogger{}, &DownloadWorkerDeps{})
	archive := filepath.Join(t.TempDir(), "archive.mp4")
	const size = 4 << 30 // a 4 GiB archive (sparse)
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	const total = 100 << 30
	for _, tc := range []struct {
		name    string
		free    uint64
		usedPct float64
		err     error
		ok      bool
	}{
		{"room to spare", 50 << 30, 50, nil, true},
		{"1 GiB short of the copy (a quota, the usage low)", 3 << 30, 10, nil, false},
		{"room, but the copy crosses disk_critical", 7 << 30, 93, nil, false},
		{"room, and the copy stays under it", 20 << 30, 80, nil, true},
		{"the volume cannot be read", 0, 0, errors.New("statfs: no such device"), true},
	} {
		w.diskSpace = func(string) (*disk.DiskSpace, error) {
			if tc.err != nil {
				return nil, tc.err
			}
			return &disk.DiskSpace{Free: tc.free, Total: total, UsedPct: tc.usedPct}, nil
		}
		err := w.remuxRoom(archive)
		if (err == nil) != tc.ok {
			t.Errorf("%s: remuxRoom = %v, want ok=%v", tc.name, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrArchiveRemuxNoSpace) {
			t.Errorf("%s: %v is not ErrArchiveRemuxNoSpace", tc.name, err)
		}
	}
}

// TestRemuxArchiveChaptersChecksTheVolumeFirst: a volume with no room for the
// copy leaves the archive untouched and writes nothing beside it — the check
// comes before the probe and FFmpeg (owner decision PR-I4).
//
// Mutant: RemuxArchiveChapters not calling remuxRoom — the archive is
// re-muxed into a volume the reading says is full.
func TestRemuxArchiveChaptersChecksTheVolumeFirst(t *testing.T) {
	r := newImportRig(t)
	r.w.diskSpace = func(string) (*disk.DiskSpace, error) {
		return &disk.DiskSpace{Free: 1 << 20, Total: 100 << 30, UsedPct: 99}, nil
	}
	before, err := os.ReadFile(r.archive)
	if err != nil {
		t.Fatal(err)
	}
	list := utils.SanitizeChapters([]utils.Chapter{{StartMs: 0, Title: "one"}}, 4000)
	if _, err := r.w.RemuxArchiveChapters(r.job.ID, database.StatusFinished, r.archive, list); !errors.Is(err, ErrArchiveRemuxNoSpace) {
		t.Fatalf("RemuxArchiveChapters = %v, want ErrArchiveRemuxNoSpace", err)
	}
	if after, _ := os.ReadFile(r.archive); !bytes.Equal(before, after) {
		t.Error("the archive changed on a full volume")
	}
	entries, _ := os.ReadDir(filepath.Dir(r.archive))
	for _, e := range entries {
		if e.Name() != "archive.mp4" {
			t.Errorf("left beside the archive: %s", e.Name())
		}
	}
}
