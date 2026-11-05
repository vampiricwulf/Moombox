package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestHlsFallbackPartIsAlwaysKeptAsItsOwnSibling is the owner's decision on
// W8's third follow-up, pinned on the built behaviour: a part the live HLS
// fallback recorded (its video.ts) is always kept and muxed as its own
// sibling file, even when a later from-start DASH capture — once solving
// works again, the job resumed into the same part — probably covers it. The
// DASH setup sets the video.ts aside before it writes (setAsideOtherLiveShape),
// and the finalize's aside mux (muxStagedAsides) muxes it into a file of its
// own beside the archive, outside the job's part list, with the DASH capture
// left where it is for the archive's own mux. Nothing ever judges the HLS
// part covered and deletes it: footage is never thrown away on a guess.
//
// Mutants: drop setAsideOtherLiveShape from setupWithFallback — the video.ts
// is never set aside and no sibling is written; skip an HLS aside in
// muxStagedAsides (or delete it unmuxed) — no sibling; register the sibling
// as a part — the job has a segment row.
func TestHlsFallbackPartIsAlwaysKeptAsItsOwnSibling(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	staging, outputDir := muxFixtureJob(t, w, db, "j-hls-sibling")
	ts := filepath.Join(staging, "video.ts")
	writeMuxFixture(t, ffmpegPath, ts, 3) // the HLS fallback's capture
	row, _ := db.GetJob("j-hls-sibling")
	jobCtx := w.buildJobContext(row)
	jobCtx.StagingDir, jobCtx.OutputDir = staging, outputDir

	// Solving works again: the from-start DASH setup resumes into the part.
	dash := &fakeStrategy{kind: "manifestless_dash"}
	info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, Formats: []youtube.Format{adaptiveVideo(), adaptiveAudio()}}
	if _, err := setupWithFallback(context.Background(), jobCtx, dash, false, info, &StrategyDeps{}); err != nil {
		t.Fatalf("setup = %v", err)
	}
	if fileExists(ts) {
		t.Fatal("the HLS capture is still video.ts beside the DASH capture")
	}
	// The DASH capture that probably covers it.
	for _, name := range []string{"video_stream", "audio_stream"} {
		if err := os.WriteFile(filepath.Join(staging, name), []byte("the from-start capture's "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	outs := w.orchestrator.muxStagedAsides(context.Background(), jobCtx, outputDir, "j-hls-sibling")
	if len(outs) != 1 || !strings.Contains(filepath.Base(outs[0]), engine.StagedRestartSuffix) || !fileExists(outs[0]) {
		t.Fatalf("siblings = %v, want the HLS part muxed to its own file beside the archive", outs)
	}
	if left := stagedAsideRecordings(staging); len(left) != 0 {
		t.Errorf("asides left in staging after their sibling was verified: %v", left)
	}
	for _, name := range []string{"video_stream", "audio_stream"} {
		if !fileExists(filepath.Join(staging, name)) {
			t.Errorf("%s was moved; it is the archive's own recording", name)
		}
	}
	if segs, err := db.GetSegments("j-hls-sibling"); err != nil || len(segs) != 0 {
		t.Errorf("segments = %v (err %v), want the sibling outside the job's part list", segs, err)
	}
}
