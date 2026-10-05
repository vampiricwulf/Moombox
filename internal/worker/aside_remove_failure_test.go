package worker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// A recovered aside whose removal failed (a Windows handle on it) stayed in
// staging as if never recovered, so the next finalize or recovery muxed it a
// second time, to "<stem>.restart-<ts>-2.mp4". It is marked recovered now:
// every aside scan skips it, and a later pass retries the removal.
//
// Mutant: no marker written — the second recovery finds the aside again and
// writes the "-2" duplicate.
func TestARecoveredAsideThatCannotBeRemovedIsNotMuxedTwice(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	staging, outputDir := muxFixtureJob(t, w, db, "j-aside-held")
	db.UpdateJobFields("j-aside-held", map[string]any{"status": database.StatusCancelled})
	aside := filepath.Join(staging, "video.mp4"+engine.StagedRestartSuffix+"1700000000")
	writeAsideFixture(t, ffmpegPath, aside, 3)

	held := true
	removeAsideFile = func(p string) error {
		if held && p == aside {
			return errors.New("the process cannot access the file because it is being used by another process")
		}
		return os.Remove(p)
	}
	t.Cleanup(func() { removeAsideFile = os.Remove })

	// muxStagedAsides directly, the step both finalize and RecoverAsides run:
	// their staging cleanup afterwards would RemoveAll the dir, which on
	// Windows fails on the same handle and here would not.
	job, _ := db.GetJob("j-aside-held")
	if first := w.orchestrator.muxStagedAsides(t.Context(), w.buildJobContext(job), outputDir, "archive"); len(first) != 1 {
		t.Fatalf("first pass recovered %v, want one sibling", first)
	}
	if !fileExists(aside) {
		t.Fatal("the held aside is gone — the removal stub did not take")
	}

	recovered := w.orchestrator.muxStagedAsides(t.Context(), w.buildJobContext(job), outputDir, "archive")
	if len(recovered) != 0 {
		t.Errorf("a second pass muxed the already-recovered aside again: %v", recovered)
	}
	if outs := mp4sIn(t, outputDir); len(outs) != 1 {
		t.Errorf("the output dir holds %v, want only the first sibling", outs)
	}

	// Once the handle is gone the next pass finishes the removal.
	held = false
	w.orchestrator.muxStagedAsides(t.Context(), w.buildJobContext(job), outputDir, "archive")
	if fileExists(aside) || fileExists(aside+asideRecoveredMarker) {
		t.Error("the aside or its marker survived a pass after the handle was released")
	}
}
