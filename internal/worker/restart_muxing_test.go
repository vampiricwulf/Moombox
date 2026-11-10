package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// stageMediaFor drops a recognised media file into <stagingBase>/<jobID> so
// HasSegmentFiles answers true.
func stageMediaFor(t *testing.T, stagingBase, jobID string) {
	t.Helper()
	dir := filepath.Join(stagingBase, jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "video.ts"), []byte("staged"), 0o644); err != nil {
		t.Fatalf("write staged media: %v", err)
	}
}

// TestMuxOnRestart pins owner decision O-B's predicate: an interrupted Muxing
// row whose staging still holds recognised media is re-muxed from what is
// there, EXCEPT when incomplete_tail is set — that row still needs the
// post-live VOD-refresh loop, so it goes back through Downloading.
//
// Mutants, one per row:
//   - dropping the IncompleteTail term: an incomplete-tail row is muxed short
//     and its missing tail is never refreshed.
//   - dropping the HasSegmentFiles term: a Muxing row with empty staging is
//     handed to muxFromStaging, which fails with "no segment files found".
//   - dropping the status term: every job on disk is muxed at boot.
func TestMuxOnRestart(t *testing.T) {
	base := t.TempDir()
	stageMediaFor(t, base, "staged")

	for _, tc := range []struct {
		name string
		job  *database.Job
		want bool
	}{
		{"muxing with staged media", &database.Job{ID: "staged", Status: database.StatusMuxing}, true},
		{"muxing with incomplete tail", &database.Job{ID: "staged", Status: database.StatusMuxing, IncompleteTail: true}, false},
		{"muxing with empty staging", &database.Job{ID: "empty", Status: database.StatusMuxing}, false},
		{"downloading with staged media", &database.Job{ID: "staged", Status: database.StatusDownloading}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := muxOnRestart(tc.job, base); got != tc.want {
				t.Errorf("muxOnRestart(%s/%s) = %v, want %v", tc.job.Status, tc.job.ID, got, tc.want)
			}
		})
	}
}

// TestEnqueueExistingJobsRoutesMuxingRow pins the routing itself: a Muxing row
// with staged media is NOT reset to Downloading and NOT put on the queue (the
// mux runs off-queue via MuxJob), while an incomplete-tail Muxing row is. The
// staged row is a Twitch job on purpose: the historical reset handed it to the
// stream processor, which lands a post-live Twitch job in Error "channel is
// offline" — the O-B route never touches the network.
//
// Mutant: restoring the unconditional "reset interrupted mux job" block — the
// staged row reappears as Downloading in the pending set and re-downloads
// from sq=0 over the complete recording.
func TestEnqueueExistingJobsRoutesMuxingRow(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)

	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })

	// Keep the off-queue mux's output inside the test's temp tree; the config
	// default would write ./output relative to the package directory.
	outDir := t.TempDir()
	staged := &database.Job{ID: "j-staged", VideoID: "v1", Platform: "twitch", Status: database.StatusMuxing, OutputDirectory: outDir}
	tail := &database.Job{ID: "j-tail", VideoID: "v2", Platform: "youtube", Status: database.StatusMuxing, IncompleteTail: true, OutputDirectory: outDir}
	for _, j := range []*database.Job{staged, tail} {
		if _, err := db.AddJob(j); err != nil {
			t.Fatalf("AddJob %s: %v", j.ID, err)
		}
		stageMediaFor(t, stagingBase, j.ID)
	}
	// insertJobExec does not carry incomplete_tail (it is only ever set on a
	// live row), so the flag has to be written the way the download path
	// writes it — otherwise the tail row reads back as a plain Muxing row.
	if db.UpdateJobFields(tail.ID, map[string]any{"incomplete_tail": true}) == nil {
		t.Fatalf("UpdateJobFields(incomplete_tail) returned nil for %s", tail.ID)
	}

	w.enqueueExistingJobs()

	if fresh, _ := db.GetJob("j-tail"); fresh == nil || fresh.Status != database.StatusDownloading {
		t.Fatalf("incomplete-tail Muxing row = %v, want Downloading (the VOD-refresh loop must still run)", fresh)
	}
	if !w.queue.isPending("j-tail") {
		t.Error("incomplete-tail row was not enqueued")
	}
	if w.queue.isPending("j-staged") {
		t.Error("a Muxing row with staged media must not be enqueued for re-download — it muxes off-queue")
	}
}

// TestYouTubeVodFlipsToMuxingBeforeItsChatWait pins, by source inspection
// (ExecuteWithChat's chat parameter is the concrete *chat.ChatDownloader, so
// the path cannot be driven with a fake — see
// TestYouTubeVodChatWaitRoutesThroughResolveVodChatOutcome), that the YouTube
// VOD branch writes Muxing as soon as its media is complete. The chat wait
// that follows is bounded by the video's own length — hours for a long
// stream — and a restart inside it used to find the row still Downloading,
// re-probe it and download the whole recording again. As Muxing,
// enqueueExistingJobs re-muxes it from staging instead (TestMuxOnRestart).
//
// Mutant: dropping the status key from the VOD branch's progress write — the
// row stays Downloading through the chat wait and this test fails.
func TestYouTubeVodFlipsToMuxingBeforeItsChatWait(t *testing.T) {
	fset := token.NewFileSet()
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	file, err := parser.ParseFile(fset, "orchestrator.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var vodBranch *ast.BlockStmt
	ast.Inspect(file, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || vodBranch != nil {
			return true
		}
		if id, ok := ifs.Cond.(*ast.Ident); !ok || id.Name != "isVod" {
			return true
		}
		body := string(src[fset.Position(ifs.Body.Pos()).Offset:fset.Position(ifs.Body.End()).Offset])
		if strings.Contains(body, "runVodDownloadWithRefresh(") {
			vodBranch = ifs.Body
		}
		return true
	})
	if vodBranch == nil {
		t.Fatal("no `if isVod` branch calling runVodDownloadWithRefresh in orchestrator.go")
	}
	body := string(src[fset.Position(vodBranch.Pos()).Offset:fset.Position(vodBranch.End()).Offset])
	if !strings.Contains(body, `"status":   database.StatusMuxing`) {
		t.Error("the YouTube VOD branch no longer writes Muxing once its media is complete — a " +
			"restart during the chat wait then re-downloads the whole recording")
	}
	if wait := strings.Index(string(src), "o.resolveVodChatOutcome(ctx,"); wait < 0 ||
		wait < fset.Position(vodBranch.End()).Offset {
		t.Error("the VOD chat wait no longer follows the VOD branch's Muxing write")
	}
}

// TestYouTubeLiveFlipsToMuxingBeforeItsChatWait is the live twin of the VOD
// pin above. The engine clears a live capture's resume sidecar the moment it
// sees the stream end, and the live chat wait (up to chatWaitTimeout) came
// before muxAndFinalize's Muxing write — so a restart in between found the
// row Downloading, re-probed a stream now post-live, set the complete
// recording aside and downloaded it again from the start.
//
// Mutant: dropping the live branch's Muxing write — the row stays
// Downloading through the chat wait.
func TestYouTubeLiveFlipsToMuxingBeforeItsChatWait(t *testing.T) {
	fset := token.NewFileSet()
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	file, err := parser.ParseFile(fset, "orchestrator.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var liveBranch ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || liveBranch != nil {
			return true
		}
		if id, ok := ifs.Cond.(*ast.Ident); !ok || id.Name != "isVod" || ifs.Else == nil {
			return true
		}
		elseBody := string(src[fset.Position(ifs.Else.Pos()).Offset:fset.Position(ifs.Else.End()).Offset])
		if strings.Contains(elseBody, "runLiveStreamDownload(") {
			liveBranch = ifs.Else
		}
		return true
	})
	if liveBranch == nil {
		t.Fatal("no `if isVod { ... } else { ... }` whose else calls runLiveStreamDownload in orchestrator.go")
	}
	body := string(src[fset.Position(liveBranch.Pos()).Offset:fset.Position(liveBranch.End()).Offset])
	if !strings.Contains(body, `"status": database.StatusMuxing`) {
		t.Error("the YouTube live branch no longer writes Muxing once its media is complete — a " +
			"restart during the chat wait then re-downloads the whole recording")
	}
	if wait := strings.Index(string(src), "chatDl.MarkStreamEnded()"); wait < 0 ||
		wait < fset.Position(liveBranch.End()).Offset {
		t.Error("the live chat wait no longer follows the live branch's Muxing write")
	}
}

// TestBootLeavesAMuxingRowCancelledSinceItsRead pins State machines rule 3 on
// the boot pass: it reads every row once, and an earlier row's re-mux can
// take a while, so a later Muxing row may have been cancelled by the time its
// turn comes. Both of the pass's writes on a Muxing row (the reset to
// Downloading, and the error clear ahead of a re-mux) apply only while the
// row is still Muxing, and a row that is not is neither enqueued nor muxed.
//
// The cancel lands from a database subscriber: the pass's first write to
// either row of the pair cancels the other one, whichever order the rows
// come in, which is exactly "after the read, before the write".
//
// Mutants: a plain UpdateJobFields for the reset — the cancelled row reads
// Downloading and sits in the queue; skipping the check ahead of muxJob — the
// cancelled row is written Muxing (the re-mux accepts a Cancelled row, as the
// Mux action does) and archived.
func TestBootLeavesAMuxingRowCancelledSinceItsRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		staged bool
	}{
		{"reset to Downloading", false},
		{"re-mux from staging", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			t.Cleanup(w.Stop)

			var stagingBase string
			w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
			outDir := t.TempDir()
			pair := []string{"j-one", "j-two"}
			for _, id := range pair {
				if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, Platform: "twitch", Status: database.StatusMuxing, OutputDirectory: outDir}); err != nil {
					t.Fatalf("AddJob %s: %v", id, err)
				}
				if tc.staged {
					stageMediaFor(t, stagingBase, id)
				}
			}

			var mu sync.Mutex
			victim := ""
			var victimWrites []database.JobStatus
			unsubscribe := db.OnJobUpdate(func(j *database.Job) {
				mu.Lock()
				if victim == "" {
					for _, id := range pair {
						if id != j.ID {
							victim = id
						}
					}
					mu.Unlock()
					db.UpdateJobFields(victim, map[string]any{"status": database.StatusCancelled})
					return
				}
				if j.ID == victim {
					victimWrites = append(victimWrites, j.Status)
				}
				mu.Unlock()
			})
			defer unsubscribe()

			w.enqueueExistingJobs()

			mu.Lock()
			defer mu.Unlock()
			if victim == "" {
				t.Fatal("the boot pass wrote neither row; the test never cancelled one")
			}
			for _, st := range victimWrites {
				if st != database.StatusCancelled {
					t.Errorf("the cancelled row was written %s by the boot pass (writes seen: %v)", st, victimWrites)
					break
				}
			}
			if fresh, _ := db.GetJob(victim); fresh == nil || fresh.Status != database.StatusCancelled {
				t.Errorf("cancelled row = %v, want it left Cancelled", fresh)
			}
			if w.queue.isPending(victim) {
				t.Error("the cancelled row was enqueued")
			}
		})
	}
}

// TestBootReMuxRefusesARowCancelledAfterItsErrorClear: the boot pass confirms
// a row is still Muxing with its error-clear write and then calls muxJob,
// which reads the row again. A Cancel landing between the two must stand: the
// boot re-mux takes a row from Muxing only, where the Mux action's source set
// holds Cancelled and would archive what the operator just cancelled.
//
// The cancel lands from a database subscriber on the error-clear write of the
// same row, which is exactly that window.
//
// Mutant: appending Muxing to jobverbs.MuxableStatuses() for the boot re-mux
// — the cancelled row is written Muxing and muxed.
func TestBootReMuxRefusesARowCancelledAfterItsErrorClear(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)

	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	if _, err := db.AddJob(&database.Job{ID: "j", VideoID: "j", Platform: "twitch", Status: database.StatusMuxing, OutputDirectory: t.TempDir()}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	stageMediaFor(t, stagingBase, "j")

	var mu sync.Mutex
	cancelled := false
	var later []database.JobStatus
	unsubscribe := db.OnJobUpdate(func(j *database.Job) {
		mu.Lock()
		if !cancelled {
			cancelled = true
			mu.Unlock()
			db.UpdateJobFields("j", map[string]any{"status": database.StatusCancelled})
			return
		}
		later = append(later, j.Status)
		mu.Unlock()
	})
	defer unsubscribe()

	w.enqueueExistingJobs()

	mu.Lock()
	defer mu.Unlock()
	if !cancelled {
		t.Fatal("the boot pass never wrote the row; the test never cancelled it")
	}
	for _, st := range later {
		if st != database.StatusCancelled {
			t.Errorf("the cancelled row was written %s by the boot pass (writes seen: %v)", st, later)
			break
		}
	}
	if fresh, _ := db.GetJob("j"); fresh == nil || fresh.Status != database.StatusCancelled {
		t.Errorf("cancelled row = %v, want it left Cancelled", fresh)
	}
	if w.queue.isPending("j") {
		t.Error("the cancelled row was enqueued")
	}
	if !HasSegmentFiles(stagingBase, "j") {
		t.Error("the cancelled row's staging is gone")
	}
}

// TestBootKeepsAMuxingRowWhoseConditionalWriteFailed: the conditional writers
// answer false for a statement that failed as well as for a row that moved
// on, and nothing but the next boot picks a Muxing row up again. So a write
// that did not apply gives the row up only when a second read shows it gone
// or no longer Muxing.
//
// Mutant: treating every false as "moved on" — a row whose reset failed stays
// Muxing, unqueued, until the next start.
func TestBootKeepsAMuxingRowWhoseConditionalWriteFailed(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	if _, err := db.AddJob(&database.Job{ID: "j", VideoID: "v", Platform: "youtube", Status: database.StatusMuxing}); err != nil {
		t.Fatal(err)
	}

	// A field map the database ignores entirely writes nothing and answers
	// false, with the row still Muxing: the shape of a failed statement.
	if !w.stillMuxingAfter("j", map[string]any{"no_such_field": 1}) {
		t.Error("stillMuxingAfter = false for a row that is still Muxing after a write that did not apply")
	}
	db.UpdateJobFields("j", map[string]any{"status": database.StatusCancelled})
	if w.stillMuxingAfter("j", map[string]any{"error": ""}) {
		t.Error("stillMuxingAfter = true for a row cancelled since it was read")
	}
	if w.stillMuxingAfter("gone", map[string]any{"error": ""}) {
		t.Error("stillMuxingAfter = true for a row that no longer exists")
	}
	if fresh, _ := db.GetJob("j"); fresh.Status != database.StatusCancelled {
		t.Errorf("status = %s, want Cancelled left alone", fresh.Status)
	}
}
