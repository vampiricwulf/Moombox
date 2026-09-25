package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// untrackOnTerminal is cmd/moombox's syncJobLogRoutingOnChange, reduced to the
// half that matters here: the OnJobChange subscriber runs INLINE inside
// UpdateJobFields (database_subscribers.go's notifyJobUpdate is synchronous for
// OnJobUpdate/OnJobChange; only the jobs-changed fan-out is a goroutine), so
// the moment a finalize writes status=Finished the job is out of db.logRouted
// and RouteLogToJobs stops matching its lines.
func untrackOnTerminal(t *testing.T, db *database.Database) {
	t.Helper()
	unsub := db.OnJobChange(func(ev *database.JobChange) {
		if ev == nil || ev.Job == nil {
			return
		}
		if ev.Job.IsTerminal() {
			db.UntrackJobForLogs(ev.Job.ID)
			return
		}
		db.TrackJobForLogs(ev.Job.ID)
	})
	t.Cleanup(unsub)
}

// TestCleanupStagingAfterMuxKeepsItsLinesInTheJobLog pins the closing half of
// every archive's own log.
//
// cleanupStagingAfterMux runs AFTER the finalize wrote status=Finished, and
// that write untracks the job's per-job log routing synchronously (CORE-12).
// So the last thing the operator is told about a job — which of the five
// outcomes its staging directory got, and where the footage went if it was
// kept — reached the global log only, and the per-job log both UIs show ended
// mid-finalize. The bracket the tree already had for RecoverAsides is what
// this function was missing.
//
// Mutants this kills:
//   - the TrackJobForLogs/restoreLogRouting bracket deleted: the buffer holds
//     nothing from the cleanup.
//   - restoreLogRouting dropped (track without the hand-back): a terminal ID
//     is left in the routed set forever, which is the scan CORE-12 removed.
func TestCleanupStagingAfterMuxKeepsItsLinesInTheJobLog(t *testing.T) {
	for _, tc := range []struct {
		name string
		// seed prepares the staging dir and the row, and returns the line the
		// job's own log must end with.
		seed func(t *testing.T, w *DownloadWorker, db *database.Database, staging string) string
	}{
		{
			name: "the ordinary path removes the directory",
			seed: func(*testing.T, *DownloadWorker, *database.Database, string) string {
				return "removed staging directory"
			},
		},
		{
			name: "a preserved directory says why",
			seed: func(t *testing.T, _ *DownloadWorker, db *database.Database, _ string) string {
				t.Helper()
				db.UpdateJobFields("j-fin", map[string]any{"incomplete_tail": true})
				return "preserving staging dir: recording tail incomplete"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			w.logger = routingLogger{db: db}

			staging, _ := muxFixtureJob(t, w, db, "j-fin")
			if err := os.WriteFile(filepath.Join(staging, "leftover.bin"), []byte("x"), 0o644); err != nil {
				t.Fatalf("seed staging: %v", err)
			}
			want := tc.seed(t, w, db, staging)

			// The job is live and routed to, exactly as it is while downloading.
			db.TrackJobForLogs("j-fin")
			untrackOnTerminal(t, db)

			// The finalize write, with the real subscriber behaviour attached.
			db.UpdateJobFields("j-fin", map[string]any{"status": database.StatusFinished})
			db.RouteLogToJobs("between the finalize and the cleanup, job j-fin")
			if got := db.GetJobLogs("j-fin"); len(got) != 0 {
				t.Fatalf("precondition: the Finished write must untrack the job (CORE-12); routed %v", got)
			}

			w.cleanupStagingAfterMux("j-fin", staging)

			got := db.GetJobLogs("j-fin")
			if len(got) == 0 {
				t.Fatalf("the staging outcome reached the global log only — the per-job log every UI shows ends mid-finalize")
			}
			if !strings.Contains(strings.Join(got, "\n"), want) {
				t.Errorf("the job's log does not carry %q; it holds %v", want, got)
			}

			// And the routing is handed back: a terminal job left tracked is
			// the whole-history scan CORE-12 removed.
			db.RouteLogToJobs("after the cleanup, job j-fin")
			after := db.GetJobLogs("j-fin")
			if strings.Contains(after[len(after)-1], "after the cleanup") {
				t.Error("the job is still tracked for log routing after the cleanup")
			}
		})
	}
}

// TestTheCompletionLineIsLoggedBeforeTheFinishedWrite is the other half of the
// same rule, for the two lines a bracket cannot save: both finalize paths end
// by writing status=Finished, and that write untracks the job's log routing
// inside UpdateJobFields. A completion line emitted after it is in the global
// log and nowhere else, so each one is logged BEFORE the write instead.
//
// A source-order pin rather than a behavioural one: reaching muxAndFinalize
// takes a real FFmpeg mux of a real fixture, and what is being protected is an
// ordering inside the function, which a reader (or a later refactor moving the
// line back down beside its notification) can undo without any test noticing.
// Same shape as the composer-body pin in internal/web.
//
// Mutant this kills: either Info call moved back below its UpdateJobFields.
func TestTheCompletionLineIsLoggedBeforeTheFinishedWrite(t *testing.T) {
	src, err := os.ReadFile("orchestrator_mux.go")
	if err != nil {
		t.Fatalf("read orchestrator_mux.go: %v", err)
	}
	text := string(src)

	for _, tc := range []struct{ fn, logLine string }{
		{"func (o *DownloadOrchestrator) muxAndFinalize(", `o.logger.Info("download complete"`},
		{"func (o *DownloadOrchestrator) finalizeMultiSegmentJob(", `o.logger.Info("multi-segment download complete"`},
	} {
		start := strings.Index(text, tc.fn)
		if start < 0 {
			t.Fatalf("%s not found — rename the pin with the function", tc.fn)
		}
		// The body runs to the first closing brace in column 0, the way
		// internal/web's composer pin slices its function.
		end := strings.Index(text[start:], "\n}\n")
		if end < 0 {
			t.Fatalf("no column-0 close found after %s", tc.fn)
		}
		body := text[start : start+end]

		logAt := strings.Index(body, tc.logLine)
		writeAt := strings.Index(body, "UpdateJobFields(jobCtx.Job.ID, updates)")
		if logAt < 0 {
			t.Errorf("%s no longer logs %s", tc.fn, tc.logLine)
			continue
		}
		if writeAt < 0 {
			t.Errorf("%s no longer writes the finalize update map", tc.fn)
			continue
		}
		if logAt > writeAt {
			t.Errorf("%s logs its completion line AFTER the Finished write — the write untracks the "+
				"job's per-job log routing synchronously (CORE-12), so that line lands in the global log only", tc.fn)
		}
	}
}
