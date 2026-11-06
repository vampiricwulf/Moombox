package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/engine"
)

// A quality split that closes a span shorter than minSegmentDuration used to
// os.Remove its media: up to ten wall-clock seconds of footage, and more for
// a from-start capture catching up through a backlog. The span is set aside
// instead (owner decision PR-R9), as any other closed recording a part cannot
// carry is: muxed into its own file beside the archive at finalize, shielded
// in staging until it is, and recoverable by A S. The dir is left clear of
// live-shape media and resume state, which is what lets the next part reuse
// it without the engine resume-appending the new quality onto the old.
//
// Mutant: setAsideShortSpan removing the files (the discard it replaced) —
// no aside is left to recover.
func TestShortSpanIsSetAsideNotDeleted(t *testing.T) {
	w, _ := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	dir := t.TempDir()
	for _, name := range []string{"video_stream", "audio_stream"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+" bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(engine.StagedRestartSidecar(filepath.Join(dir, name)), []byte(`{"seq":3}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if !w.orchestrator.setAsideShortSpan(dir, "j-short") {
		t.Fatal("setAsideShortSpan reported the dir still holds the span")
	}
	for _, name := range liveShapeStagingNames {
		p := filepath.Join(dir, name)
		if fileExists(p) || fileExists(engine.StagedRestartSidecar(p)) {
			t.Errorf("%s (or its resume state) is still in the dir the next part reuses", name)
		}
	}
	asides := stagedAsideRecordings(dir)
	if len(asides) != 2 {
		t.Fatalf("asides = %v, want the span's video and audio set aside", asides)
	}
	groups := groupStagedAsides(asides)
	if len(groups) != 1 {
		t.Errorf("the span's two halves are %d recordings, want one", len(groups))
	}
	for _, a := range asides {
		b, err := os.ReadFile(a)
		if err != nil || !strings.HasSuffix(string(b), " bytes") {
			t.Errorf("aside %s = %q, %v — the span's bytes were not kept", filepath.Base(a), b, err)
		}
	}
}

// Both quality splits close a short span through setAsideShortSpan and remove
// none of its media themselves: YouTube's splitPart and Twitch's
// advanceToNewPart. Pinned on their source, as the refused-segment split is
// (TestLiveLoopSplitsAtARefusedSegment): the branch sits behind a real live
// loop and the minimum span's wall clock, which no deterministic test can
// drive; the set-aside itself is driven above.
//
// Mutants: either split back on its os.Remove discard, or not calling
// setAsideShortSpan, or no longer falling back to muxing the span as a part
// when the set-aside fails.
func TestQualitySplitsSetTheShortSpanAside(t *testing.T) {
	section := func(file, from, to string) string {
		t.Helper()
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		body := string(src)
		i := strings.Index(body, from)
		if i < 0 {
			t.Fatalf("%s lacks %q", file, from)
		}
		j := strings.Index(body[i:], to)
		if j < 0 {
			t.Fatalf("%s lacks %q after %q", file, to, from)
		}
		return body[i : i+j]
	}
	yt := section("orchestrator_youtube.go", "splitPart := func(", "segJobCtx := *jobCtx")
	if !strings.Contains(yt, "if !shortSegment || !o.setAsideShortSpan(curCtx.StagingDir, jobCtx.Job.ID) {") {
		t.Error("splitPart does not set a short span aside (or mux it when it cannot be)")
	}
	tw := section("orchestrator_twitch.go", "advanceToNewPart := func(", "segmentIndex = nextIdx")
	if !strings.Contains(tw, "if !muxCurrent && !o.setAsideShortSpan(curStagingDir, jobCtx.Job.ID) {\n\t\t\tmuxCurrent = true") {
		t.Error("advanceToNewPart does not set a short span aside (or mux it when it cannot be)")
	}
	for name, body := range map[string]string{"splitPart": yt, "advanceToNewPart": tw} {
		if strings.Contains(body, "os.Remove(") {
			t.Errorf("%s removes a file itself — a quality split never deletes media", name)
		}
	}
}
