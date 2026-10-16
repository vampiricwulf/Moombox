package worker

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// The Muxing writes a run makes once its media is complete were decided on
// the run's own view of the job: the YouTube VOD branch's, the live branch's,
// the live loop's stream end, and the finalize's own in muxAndFinalize and
// muxFromStaging. An operator's Cancel writes the row before it cancels the
// run, and a subscriber ahead of the run's listener can hold that cancel back,
// so a Cancel could be in the row with the run's ctx still live — and each of
// those writes put Muxing over it and let the mux run on, announced, until
// something wrote Cancelled again (or, in MuxJob, Error over it). They now
// apply only to a row with no outcome, and a refusal stops the run with
// nothing muxed and staging kept.

// orchestratorMuxingWrites are the Muxing writes each orchestrator file
// makes, by file. MuxJob's own Muxing write (worker.go) is the operator's Mux
// action, decided on the row they chose, and is not one of them.
var orchestratorMuxingWrites = map[string]int{
	"orchestrator.go":         2, // ExecuteWithChat: the VOD branch, the live branch
	"orchestrator_youtube.go": 1, // runLiveStreamDownload's stream end
	"orchestrator_twitch.go":  1, // ExecuteTwitch, on the way to a finalize
	"orchestrator_mux.go":     2, // muxAndFinalize; muxFromStaging's direct multi-part arm
}

// Every Muxing write in the orchestrators is the fields of a
// writeUnlessOutcome call that is an if's condition, and that if stops the
// run: it returns or sets ErrCancelled, or takes the user's cancel. A source
// pin, because three of the six sit in paths no unit test can drive without
// a live YouTube service (the live loop and the live branch); the behaviour
// of the refusal is pinned by the tests below and by writeUnlessOutcome's
// own (TestWriteUnlessOutcome).
//
// Mutants: any of the six back on UpdateJobFields (that file's count of
// conditional writes falls short); any refusal ignored (its if body no longer
// stops the run).
func TestEveryOrchestratorMuxingWriteIsConditional(t *testing.T) {
	for file, want := range orchestratorMuxingWrites {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		// Every map literal that sets status to Muxing, and those that are
		// writeUnlessOutcome's fields in an if condition whose body stops.
		muxing := map[*ast.CompositeLit]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.CompositeLit); ok && setsMuxing(lit) {
				muxing[lit] = false
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			ifs, ok := n.(*ast.IfStmt)
			if !ok {
				return true
			}
			call, ok := ifs.Cond.(*ast.CallExpr)
			if !ok || len(call.Args) != 3 {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "writeUnlessOutcome" {
				return true
			}
			lit, ok := call.Args[2].(*ast.CompositeLit)
			if !ok || !setsMuxing(lit) {
				return true
			}
			if stopsTheRun(ifs.Body) {
				muxing[lit] = true
			}
			return true
		})
		conditional := 0
		for lit, ok := range muxing {
			if ok {
				conditional++
			} else {
				t.Errorf("%s:%d: a Muxing write that is not writeUnlessOutcome's, stopping the run on a refusal",
					file, fset.Position(lit.Pos()).Line)
			}
		}
		if conditional != want {
			t.Errorf("%s holds %d conditional Muxing writes, want %d (update orchestratorMuxingWrites if a write moved)",
				file, conditional, want)
		}
	}
}

// setsMuxing reports whether lit is a map literal with "status":
// database.StatusMuxing among its elements.
func setsMuxing(lit *ast.CompositeLit) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.BasicLit)
		if !ok || key.Value != `"status"` {
			continue
		}
		if sel, ok := kv.Value.(*ast.SelectorExpr); ok && sel.Sel.Name == "StatusMuxing" {
			return true
		}
	}
	return false
}

// stopsTheRun reports whether body names ErrCancelled or takes the user's
// cancel (ExecuteTwitch's userCancelled and callCancel).
func stopsTheRun(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && (id.Name == "ErrCancelled" || id.Name == "callCancel") {
			found = true
		}
		return !found
	})
	return found
}

// A YouTube VOD whose download completes with an operator's Cancel already in
// the row — its ctx cancel held back behind a slower subscriber, so the run
// has not heard it — is not written Muxing: the run stops as a cancelled one
// with the download kept in staging, and no mux is announced.
//
// Mutant: the VOD branch's Muxing write unconditional again (UpdateJobFields:
// Muxing lands after Cancelled and the finalize runs on, announced).
func TestYouTubeVodCancelBeforeTheMuxingWriteStands(t *testing.T) {
	w, db := testWorkerSetup(t)
	rec := notificationtest.New()
	job := &database.Job{ID: "yt_vod_cancel", VideoID: "vodcancel", URL: "u", Platform: "youtube",
		Title: "T", ChannelName: "C", Status: database.StatusUpcoming, OutputDirectory: t.TempDir()}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	stored, _ := db.GetJob(job.ID)
	jobCtx := w.buildJobContext(stored)

	cancelEntered := make(chan struct{})
	released := make(chan struct{})
	var once sync.Once
	unsub := db.OnJobUpdate(func(j *database.Job) {
		if j.ID != job.ID || j.Status != database.StatusCancelled {
			return
		}
		first := false
		once.Do(func() { first = true; close(cancelEntered) })
		if first {
			select {
			case <-released:
			case <-time.After(10 * time.Second):
			}
		}
	})
	defer unsub()

	body := bytes.Repeat([]byte("moombox-vod "), 4096)
	whole := wholeFileHandler(body)
	cancelDone := make(chan struct{})
	var fired atomic.Bool
	// The operator's Cancel lands as the download is served.
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if fired.CompareAndSwap(false, true) {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("panic in the Cancel write: %v", r)
					}
				}()
				defer close(cancelDone)
				db.UpdateJobFieldsUnlessTerminal(job.ID, map[string]any{"status": database.StatusCancelled})
			}()
			<-cancelEntered
		}
		whole(rw, r)
	}))
	defer srv.Close()

	log := watchStatuses(t, db, job.ID)
	o := NewDownloadOrchestrator(db, nil, "", discardLogger{}, nil, stubCipherSolver{}, nil, rec, nil)
	err := o.ExecuteWithChat(context.Background(), jobCtx, wholeFileVodInfo(srv, len(body)), true, nil)
	close(released)
	<-cancelDone

	if !errors.Is(err, ErrCancelled) {
		t.Errorf("returned %v, want ErrCancelled", err)
	}
	if seq := log.statuses(); len(seq) == 0 || seq[len(seq)-1] != database.StatusCancelled ||
		strings.Contains(strings.Join(statusNames(seq), " "), string(database.StatusMuxing)) {
		t.Errorf("statuses %v, want no Muxing and the Cancel standing", seq)
	}
	if got := rec.ByEvent("muxing"); len(got) != 0 {
		t.Errorf("%q was sent for a download the operator had cancelled", got[0].Title)
	}
	if media := discoverStagingMedia(jobCtx.StagingDir); media == nil {
		t.Error("the completed download is not in staging")
	}
}

func statusNames(seq []database.JobStatus) []string {
	out := make([]string, len(seq))
	for i, s := range seq {
		out[i] = string(s)
	}
	return out
}

// A finalize handed a row that holds an outcome — Cancelled, or Finished by
// another mux — writes nothing over it, announces nothing and muxes nothing:
// muxAndFinalize in either shape (one staged file; recorded parts), and
// muxFromStaging's direct arm for a post-split job with no root media.
//
// Mutants: muxAndFinalize's write unconditional (UpdateJobFields: the row
// reads Muxing and the mux is announced); its refusal ignored (the same);
// muxFromStaging's direct-arm write unconditional or its refusal ignored (the
// direct-arm rows); the write moved back below sendMuxingStarting (announced).
func TestFinalizeOnARowWithAnOutcomeMuxesNothing(t *testing.T) {
	type shape struct {
		name     string
		segments int
		root     bool // a staged media file in the staging root
		direct   bool // through muxFromStaging rather than muxAndFinalize
	}
	for _, sh := range []shape{
		{"muxAndFinalize, one staged file", 0, true, false},
		{"muxAndFinalize, recorded parts", 2, false, false},
		{"muxFromStaging, recorded parts and no root media", 2, false, true},
	} {
		for _, outcome := range []database.JobStatus{database.StatusCancelled, database.StatusFinished} {
			t.Run(sh.name+"/"+string(outcome), func(t *testing.T) {
				rec := notificationtest.New()
				o, jobCtx := muxTestOrchestrator(t, rec, sh.segments)
				// Twitch: finalizeMultiSegmentJob's part merge, the one branch
				// that would reach the (nil) muxer, is YouTube-gated.
				o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{"platform": "twitch"})
				jobCtx.Job.Platform = "twitch"
				jobCtx.Config = &JobConfig{}
				jobCtx.StagingDir = t.TempDir()
				jobCtx.OutputDir = t.TempDir()
				jobCtx.Filename = "a-job"
				result := &DownloadResult{HasVideo: true}
				if sh.root {
					result.VideoPath = filepath.Join(jobCtx.StagingDir, "video.ts")
					if err := os.WriteFile(result.VideoPath, []byte("staged"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				if !o.db.UpdateJobFieldsUnlessTerminal(jobCtx.Job.ID, map[string]any{"status": outcome}) {
					t.Fatal("could not write the outcome")
				}
				log := watchStatuses(t, o.db, jobCtx.Job.ID)

				var err error
				if sh.direct {
					err = o.muxFromStaging(context.Background(), jobCtx)
				} else {
					err = o.muxAndFinalize(context.Background(), jobCtx, result)
				}
				if !errors.Is(err, ErrCancelled) {
					t.Errorf("returned %v, want ErrCancelled", err)
				}
				if seq := log.statuses(); len(seq) != 0 {
					t.Errorf("statuses written %v over the %s row, want none", seq, outcome)
				}
				if calls := rec.Calls(); len(calls) != 0 {
					t.Errorf("sent %q, want nothing", calls[0].Title)
				}
				if entries, _ := os.ReadDir(jobCtx.OutputDir); len(entries) != 0 {
					t.Errorf("the output directory holds %d entries, want nothing muxed", len(entries))
				}
			})
		}
	}
}

// The off-queue Mux leaves an outcome its finalize found on the row: a mux
// whose finalize refused its Muxing write returned an error MuxJob read as a
// failure, and its Error write landed on whatever the row held — here a
// Finished archive another mux wrote while this one waited for its slot.
// Staging is kept.
//
// Mutant: MuxJob's ErrCancelled arm dropped (the row reads Error).
func TestMuxJobLeavesAnOutcomeItsFinalizeFound(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	job := &database.Job{ID: "tw_muxjob_outcome", VideoID: "v1", URL: "https://twitch.tv/videos/1", Platform: "twitch",
		Status: database.StatusError, OutputDirectory: t.TempDir()}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	stageMediaFor(t, stagingBase, job.ID)

	// Hold the one download slot, so the mux waits for it with the row read.
	w.queue.SetMaxDownloads(1)
	if !w.queue.TryAcquireDownloadSlot("holder") {
		t.Fatal("could not take the download slot")
	}
	if err := w.MuxJob(job.ID); err != nil {
		t.Fatalf("MuxJob: %v", err)
	}
	db.UpdateJobFields(job.ID, map[string]any{"status": database.StatusFinished})
	w.queue.ReleaseDownloadSlot("holder")
	w.wg.Wait()

	row, _ := db.GetJob(job.ID)
	if row == nil || row.Status != database.StatusFinished {
		t.Errorf("row = %+v, want the Finished outcome left standing", row)
	}
	if !HasSegmentFiles(stagingBase, job.ID) {
		t.Error("staging was not kept")
	}
}
