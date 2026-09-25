package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// writeAsidePair drops one restart's worth of set-aside files into dir: the
// video half at `<stem>.restart-<stamp>`, optionally its resume twin, and
// optionally the audio half a DASH restart sets aside under the same second.
// Bytes, not media — every assertion in this file is about the SCAN, which
// never opens a file. The tests that actually mux use writeAsideFixture
// (mux_lifecycle_test.go), which needs FFmpeg.
func writeAsidePair(t *testing.T, dir, stamp string, size int, withSidecar, withAudio bool) {
	t.Helper()
	video := filepath.Join(dir, "video.mp4"+engine.StagedRestartSuffix+stamp)
	if err := os.WriteFile(video, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write aside %s: %v", video, err)
	}
	if withSidecar {
		if err := os.WriteFile(engine.StagedRestartSidecar(video), []byte(`{"seq":1}`), 0o644); err != nil {
			t.Fatalf("write aside sidecar: %v", err)
		}
	}
	if withAudio {
		audio := filepath.Join(dir, "audio_stream"+engine.StagedRestartSuffix+stamp)
		if err := os.WriteFile(audio, make([]byte, size), 0o644); err != nil {
			t.Fatalf("write audio aside: %v", err)
		}
	}
}

// TestAsideReportGroupsEachRestartOnce is the shape the whole arc is built on:
// one Aside per RESTART, not per file. A DASH restart sets the video and audio
// halves aside under the same second, and both halves' bytes belong to the one
// recording an operator will get back.
//
// Mutants this kills:
//   - reporting one Aside per FILE (dropping groupStagedAsides): the count is
//     3, not 2, and the first group's Size is half what it should be.
//   - summing only the video half: Size is 100, not 200.
//   - ordering newest-first within a directory: the 1600000000 group is not first.
func TestAsideReportGroupsEachRestartOnce(t *testing.T) {
	dir := t.TempDir()
	writeAsidePair(t, dir, "1600000000", 100, true, true) // video + audio + sidecar
	writeAsidePair(t, dir, "1700000000", 50, false, false)

	got := asideReport(dir)

	if len(got.Groups) != 2 {
		t.Fatalf("asideReport returned %d groups, want 2 (one per restart, not one per file): %+v", len(got.Groups), got.Groups)
	}
	// Oldest first WITHIN one directory. Across directories the order is the
	// scan's — root, then each seg_N — which is what stagedAsideRecordings
	// produces and what the doc comment promises.
	if got.Groups[0].Timestamp != "2020-09-13T12:26:40Z" {
		t.Errorf("first group Timestamp = %q, want the 1600000000 stamp rendered as RFC 3339 — asides in one directory come back oldest first", got.Groups[0].Timestamp)
	}
	if got.Groups[0].Size != 200 {
		t.Errorf("first group Size = %d, want 200 (both halves of the restart)", got.Groups[0].Size)
	}
	if !got.Groups[0].HasResumeSidecar {
		t.Error("first group HasResumeSidecar = false, want true — its .resume.json is on disk")
	}
	if !strings.HasSuffix(got.Groups[0].Path, engine.StagedRestartSuffix+"1600000000") {
		t.Errorf("first group Path = %q, want the video half of the 1600000000 restart", got.Groups[0].Path)
	}
	if got.Groups[1].Size != 50 || got.Groups[1].HasResumeSidecar {
		t.Errorf("second group = %+v, want Size 50 and no sidecar", got.Groups[1])
	}
	if got.KeptChatSidecar {
		t.Error("KeptChatSidecar = true with no chat.json anywhere under the dir")
	}
}

// TestAsideReportFindsAKeptChatCaptureAtAnyDepth pins the rule the copy in
// Task 2 depends on: keepOnlyChatCapture preserves chat.json AT ANY DEPTH,
// because a quality- or gap-split job keeps each part's chat beside that
// part's media in seg_N/. Each case stages an aside too, because asideReport
// only looks for the chat capture once it has something to recover.
//
// Mutants this kill:
//   - looking only in the staging root: the seg_1 case reports false.
//   - accepting any chat.json.* member as the capture: the sidecar-only case
//     reports true, and Task 2 would then try to copy a file it never found.
func TestAsideReportFindsAKeptChatCaptureAtAnyDepth(t *testing.T) {
	t.Run("chat.json in a seg_N part dir", func(t *testing.T) {
		dir := t.TempDir()
		writeAsidePair(t, dir, "1700000000", 8, false, false)
		seg := filepath.Join(dir, "seg_1")
		if err := os.MkdirAll(seg, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(seg, "chat.json"), []byte(`{"messages":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if !asideReport(dir).KeptChatSidecar {
			t.Error("KeptChatSidecar = false with a chat.json under seg_1 — a split job's chat lives beside its part")
		}
		if got := findKeptChatCapture(dir); got != filepath.Join(seg, "chat.json") {
			t.Errorf("findKeptChatCapture = %q, want the seg_1 capture", got)
		}
	})

	t.Run("only the resume sidecar, no chat.json", func(t *testing.T) {
		dir := t.TempDir()
		writeAsidePair(t, dir, "1700000000", 8, false, false)
		if err := os.WriteFile(filepath.Join(dir, "chat.json.resume.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if asideReport(dir).KeptChatSidecar {
			t.Error("KeptChatSidecar = true with no chat.json — there is nothing for a recovery to copy")
		}
		if got := findKeptChatCapture(dir); got != "" {
			t.Errorf("findKeptChatCapture = %q, want \"\"", got)
		}
	})

	t.Run("the walk is skipped when nothing was set aside", func(t *testing.T) {
		// Mutant: evaluating findKeptChatCapture before the group count —
		// every GET /api/jobs/{id} in the fleet walks a staging tree to
		// populate a flag neither UI renders without a recording to recover.
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "chat.json"), []byte(`{"messages":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := asideReport(dir); got.KeptChatSidecar {
			t.Error("asideReport reported a chat capture for a dir with no asides — the tree walk must be short-circuited")
		}
	})
}

// TestAsideReportGroupsIsNeverNil keeps the JSON payload an array. The
// dashboard reads `enriched.asides` straight into a .length, and the TUI
// wiring ranges over it.
//
// Mutant: `var out []Aside` instead of make(...,0,n) — the field marshals as
// null and the assertion below fails.
func TestAsideReportGroupsIsNeverNil(t *testing.T) {
	if got := asideReport(t.TempDir()).Groups; got == nil {
		t.Error("asideReport(empty dir).Groups is nil — it must marshal as [], not null")
	}
	if got := asideReport(filepath.Join(t.TempDir(), "does-not-exist")).Groups; got == nil {
		t.Error("asideReport(missing dir).Groups is nil — a job that never staged anything must still answer []")
	}
}

// TestScanAsidesJoinsTheStagingBaseLikeHasSegmentFiles pins the exported
// entry point's shape: (stagingBase, jobID), the same two arguments
// HasSegmentFiles takes, so the route can call it with the base it already
// reads out of the config store and no worker instance.
//
// Mutant: taking a full staging DIR instead of the base — the join is done
// twice (or not at all) and the scan reads the wrong directory, so the report
// comes back empty.
func TestScanAsidesJoinsTheStagingBaseLikeHasSegmentFiles(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "job-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAsidePair(t, dir, "1700000000", 8, false, false)

	if got := ScanAsides(base, "job-1"); len(got.Groups) != 1 {
		t.Errorf("ScanAsides(base, %q) found %d groups, want 1", "job-1", len(got.Groups))
	}
	if got := ScanAsides(base, "job-2"); len(got.Groups) != 0 {
		t.Errorf("ScanAsides(base, %q) found %d groups for a job with no staging dir, want 0", "job-2", len(got.Groups))
	}
}

// TestWorkerAsidesReadsTheConfiguredStagingBase is the method the TUI wiring
// calls: it resolves the staging base itself and refuses a job that is not in
// the database, so a caller cannot be handed a report for a row that no
// longer exists.
//
// Mutants this kills:
//   - dropping the job lookup: the unknown-job call returns a nil error.
//   - reading Paths.OutputDirectory instead of EffectiveStagingDir(): the
//     known job reports no asides.
func TestWorkerAsidesReadsTheConfiguredStagingBase(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)

	if _, err := db.AddJob(&database.Job{
		ID: "j-report", VideoID: "j-report", URL: "u", Platform: "youtube", Status: database.StatusCancelled,
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	var base string
	w.readConfig(func(c *config.MoomboxConfig) { base = c.Paths.EffectiveStagingDir() })
	dir := filepath.Join(base, "j-report")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAsidePair(t, dir, "1700000000", 16, true, false)
	if err := os.WriteFile(filepath.Join(dir, "chat.json"), []byte(`{"messages":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := w.Asides("j-report")
	if err != nil {
		t.Fatalf("Asides: %v", err)
	}
	if len(report.Groups) != 1 || !report.KeptChatSidecar {
		t.Errorf("Asides = %+v, want one group and KeptChatSidecar true", report)
	}

	if _, err := w.Asides("no-such-job"); err == nil {
		t.Error("Asides on an unknown job returned a nil error — the caller cannot tell an empty report from a missing row")
	}
}
