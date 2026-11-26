package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/sqliteuri"
)

// A finalize's Finished write is the one that licenses deleting staging, the
// only other copy of the recording. It was made unconditionally and its
// result ignored: a statement the database refused (another process holding
// its write lock past busy_timeout, an I/O error, a full volume) still
// returned nil to the caller, which sent Download Finished and deleted the
// staging of a row that never named the archive. Owner ruling R3-1: the write
// is retried a few times; still refused, the finalize fails with the file
// named, ahead of the notification, and staging is kept.

// rawExec runs one statement against the database file over a connection of
// its own, closed at once.
func rawExec(t *testing.T, path, stmt string) {
	t.Helper()
	raw, err := sql.Open("sqlite", sqliteuri.FileURI(path)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// onWarnLogger runs hook once, on the first Warn line whose message is msg.
type onWarnLogger struct {
	discardLogger
	msg  string
	once sync.Once
	hook func()
}

func (l *onWarnLogger) Warn(msg string, _ ...any) {
	if msg == l.msg {
		l.once.Do(l.hook)
	}
}

// finishedWriteFixture is an Error row with one staged recording, or a split
// capture's two parts, on a worker whose notifications are recorded and whose
// Finished-write retries do not sleep. It returns the database file's path
// with the rest.
func finishedWriteFixture(t *testing.T, split bool) (w *DownloadWorker, db *database.Database, rec *notificationtest.Recorder, staging, dbPath string) {
	t.Helper()
	old := finishedWriteRetries
	finishedWriteRetries = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { finishedWriteRetries = old })

	w, db = testWorkerSetup(t)
	t.Cleanup(w.Stop)
	rec = notificationtest.New()
	w.notifier = rec
	w.orchestrator.notifier = rec
	staging, _ = muxFixtureJob(t, w, db, "j-fw")
	if split {
		stageSplitParts(t, staging, "video.mp4")
	} else {
		ffmpegPath, _ := requireFFmpegTools(t)
		writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 3)
	}
	db.UpdateJobFields("j-fw", map[string]any{"status": database.StatusError})
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	return w, db, rec, staging, filepath.Join(filepath.Dir(stagingBase), "test.db")
}

// Mutants: writeFinished's result ignored at a finalize site (the row stays
// Muxing with no error, Download Finished is sent and staging is deleted);
// the error returned after the notification (the embed is sent).
func TestAFinishedWriteTheDatabaseRefusesKeepsStaging(t *testing.T) {
	for _, split := range []bool{false, true} {
		name := "single file"
		if split {
			name = "split parts"
		}
		t.Run(name, func(t *testing.T) {
			w, db, rec, staging, dbPath := finishedWriteFixture(t, split)
			failJobWritesWhere(t, dbPath, `NEW.status = 'Finished'`)

			if err := w.MuxJob("j-fw"); err != nil {
				t.Fatal(err)
			}
			w.wg.Wait()

			row, _ := db.GetJob("j-fw")
			if row == nil || row.Status != database.StatusError {
				t.Fatalf("row = %+v, want Error", row)
			}
			if !strings.Contains(row.Error, "use Mux") || !strings.Contains(row.Error, ".mp4") {
				t.Errorf("row error = %q, want it to name the file and point at Mux", row.Error)
			}
			if _, err := os.Stat(filepath.Join(staging, "video.mp4")); err != nil {
				t.Errorf("the staged recording is gone: %v", err)
			}
			if sent := rec.ByEvent("finished"); len(sent) != 0 {
				t.Errorf("%d Download Finished notifications sent for a job that is not Finished", len(sent))
			}
		})
	}
}

// A write refused once and accepted on the retry is a finished job like any
// other: recorded, announced once, its staging reclaimed.
//
// Mutant: no retry (the row ends in Error on the first refusal).
func TestAFinishedWriteThatLandsOnARetryFinishesTheJob(t *testing.T) {
	w, db, rec, staging, dbPath := finishedWriteFixture(t, false)
	failJobWritesWhere(t, dbPath, `NEW.status = 'Finished'`)
	w.logger = discardLogger{}
	w.orchestrator.logger = &onWarnLogger{
		msg:  "the Finished write did not land; trying it again",
		hook: func() { rawExec(t, dbPath, `DROP TRIGGER fail_job_writes`) },
	}

	if err := w.MuxJob("j-fw"); err != nil {
		t.Fatal(err)
	}
	w.wg.Wait()

	row, _ := db.GetJob("j-fw")
	if row == nil || row.Status != database.StatusFinished || row.OutputFile == "" {
		t.Fatalf("row = %+v, want Finished with its archive", row)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging was kept under the Finished row (stat: %v)", err)
	}
	if sent := rec.ByEvent("finished"); len(sent) != 1 {
		t.Errorf("%d Download Finished notifications, want 1", len(sent))
	}
}

// A row deleted while its finalize ran is the operator's delete, not a
// failed write: ErrCancelled, as writeUnlessOutcome answers a row that is
// gone, so the run ends as a cancelled one instead of in an Error nobody can
// see.
//
// Mutant: the `cur == nil` arm dropped (the retries run and a "database
// refused" error comes back).
func TestAFinishedWriteOnADeletedRowIsACancel(t *testing.T) {
	w, db, _, _, _ := finishedWriteFixture(t, false)
	if err := db.DeleteJob("j-fw"); err != nil {
		t.Fatal(err)
	}
	_, err := w.orchestrator.writeFinished(context.Background(), "j-fw",
		map[string]any{"status": database.StatusFinished}, "out.mp4")
	if !errors.Is(err, ErrCancelled) {
		t.Errorf("writeFinished on a deleted row = %v, want ErrCancelled", err)
	}
}

// The wait between attempts ends with the job's context: a shutdown or a
// delete does not sit out the retries.
//
// Mutant: the ctx arm dropped from the wait (the call takes the full hour).
func TestAFinishedWriteRetryStopsWithItsContext(t *testing.T) {
	w, _, _, _, dbPath := finishedWriteFixture(t, false)
	finishedWriteRetries = []time.Duration{time.Hour}
	failJobWritesWhere(t, dbPath, `NEW.status = 'Finished'`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := w.orchestrator.writeFinished(ctx, "j-fw",
			map[string]any{"status": database.StatusFinished}, "out.mp4")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, ErrCancelled) {
			t.Errorf("writeFinished = %v, want a failure naming the file", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("writeFinished waited out its retry delay on a cancelled context")
	}
}

// cleanupStagingAfterMux read a row it could not read as a row that is gone,
// and a gone row has no incomplete tail or chat verdict to shield its
// staging. A failed read keeps the directory.
//
// Mutant: the read's error ignored again (the directory is removed).
func TestStagingIsKeptWhenTheRowCannotBeRead(t *testing.T) {
	w, _, _, staging, dbPath := finishedWriteFixture(t, false)
	// The prepared read names a column that is no longer there, so it fails
	// while every other table still reads.
	rawExec(t, dbPath, `ALTER TABLE jobs RENAME COLUMN title TO title_gone`)
	if _, err := w.db.GetJob("j-fw"); err == nil {
		t.Skip("the row read did not fail on this driver; the case cannot be staged")
	}

	w.cleanupStagingAfterMux("j-fw", staging)

	if _, err := os.Stat(filepath.Join(staging, "video.mp4")); err != nil {
		t.Errorf("the staged recording was deleted on a row read that failed: %v", err)
	}
}
