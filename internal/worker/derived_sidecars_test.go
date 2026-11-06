package worker

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestOrphanSweepTreatsDerivedSidecarsLikeTheChat is owner decision FE-16:
// the chat's raw sidecar ("<name>.chat.raw.jsonl") and a split archive's
// chapter list ("<name>.chapters.json") are named after the archive and on
// no column, and the sweep looked at neither — so once the job was deleted
// they stayed in the output tree for good, never offered, while the archive
// and chat beside them were. They are listed now exactly when the archive
// they derive from is no row's: a single-file job's (by its output file or
// its chat), a split job's (the base its parts share), and a recovered
// recording's raw chat folded under its archive. The delete agrees with the
// sweep both ways.
//
// Mutants (each run): the walk not looking at derived files (the orphaned
// ones are not listed); ownerOf without archiveDerivedStem (every owned one
// is listed, and the delete takes it); archiveDerivedStem without the
// "<X>.chat" stem (a raw sidecar whose job names only its chat is listed);
// asideSiblingStem without the raw sidecar fold (the recovered recording's
// raw chat is listed beside its owned archive).
func TestOrphanSweepTreatsDerivedSidecarsLikeTheChat(t *testing.T) {
	f := newOrphanFixture(t)
	single := f.write(t, "Chan/Single.mp4")
	f.write(t, "Chan/Single.chat.json")
	singleRaw := f.write(t, "Chan/Single.chat.raw.jsonl")
	addJob(t, f, "yt_single", database.StatusFinished, func(j *database.Job) { j.OutputFile = single })

	chatOnly := f.write(t, "Chan/ChatOnly.chat.json")
	chatOnlyRaw := f.write(t, "Chan/ChatOnly.chat.raw.jsonl")
	addJob(t, f, "yt_chatonly", database.StatusFinished, func(j *database.Job) { j.ChatFile = chatOnly })

	part1 := f.write(t, "Chan/Split - part1.mp4")
	splitChapters := f.write(t, "Chan/Split"+utils.ChaptersSidecarSuffix)
	splitRaw := f.write(t, "Chan/Split.chat.raw.jsonl")
	addJob(t, f, "tw_split", database.StatusFinished, func(j *database.Job) {
		j.OutputFile = part1
		j.Segments = []database.Segment{{SegmentIndex: 0, Filename: filepath.Base(part1), FilePath: part1}}
	})

	asideRaw := f.write(t, "Chan/Single"+engine.StagedRestartSuffix+"1700000000.chat.raw.jsonl")

	goneRaw := f.write(t, "Chan/Gone.chat.raw.jsonl")
	goneChapters := f.write(t, "Chan/Gone"+utils.ChaptersSidecarSuffix)

	entries, err := ScanOrphanedFiles(f.db, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, e := range entries {
		listed = append(listed, filepath.Base(e.Path))
	}
	for _, p := range []string{goneRaw, goneChapters} {
		if !slices.Contains(listed, filepath.Base(p)) {
			t.Errorf("%s, whose archive no row names, is not listed (listed %v)", filepath.Base(p), listed)
		}
	}
	for _, p := range []string{singleRaw, chatOnlyRaw, splitChapters, splitRaw, asideRaw} {
		if slices.Contains(listed, filepath.Base(p)) {
			t.Errorf("%s is its archive's, and is listed as an orphan", filepath.Base(p))
		}
	}

	refusedAsStale(t, f, splitChapters, "job tw_split")
	refusedAsStale(t, f, chatOnlyRaw, "job yt_chatonly")
	if err := DeleteOrphanedFile(goneChapters, f.db, f.cfg); err != nil || fileExists(goneChapters) {
		t.Errorf("deleting an orphaned chapter list: %v (still there: %v)", err, fileExists(goneChapters))
	}
}

// TestDerivedSidecarsFollowTheParts is FE-16 for the paths that move or
// delete a split job's parts: a Reinitialize removes the parts' chapter list
// with them, and a complete VOD download that supersedes the parts removes it
// too — the single file muxed next carries its own. And an aside recovery
// carries the kept chat capture's raw sidecar beside the recovered recording
// with the chat.
//
// Mutants (each run): clearJobParts without the removal; supersedePartsWithVod
// without it; copyKeptChatSidecar without the raw copy.
func TestDerivedSidecarsFollowTheParts(t *testing.T) {
	t.Run("reinitialize", func(t *testing.T) {
		w, db := testWorkerSetup(t)
		out := t.TempDir()
		part := filepath.Join(out, "Show - part1.mp4")
		chapters := filepath.Join(out, "Show"+utils.ChaptersSidecarSuffix)
		for _, p := range []string{part, chapters} {
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.AddJob(&database.Job{ID: "tw_re", VideoID: "tw_re", URL: "u", Platform: "twitch", Status: database.StatusFinished}); err != nil {
			t.Fatal(err)
		}
		if err := db.AddSegment(&database.Segment{JobID: "tw_re", SegmentIndex: 0, Filename: filepath.Base(part), FilePath: part}); err != nil {
			t.Fatal(err)
		}
		w.clearJobParts("tw_re")
		if fileExists(part) || fileExists(chapters) {
			t.Errorf("after the parts are cleared: part %v, chapter list %v; want neither", fileExists(part), fileExists(chapters))
		}
	})

	t.Run("supersede", func(t *testing.T) {
		w, db := testWorkerSetup(t)
		_, outputDir := muxFixtureJob(t, w, db, "j-ch-super")
		job, _ := db.GetJob("j-ch-super")
		jobCtx := w.buildJobContext(job)
		part := filepath.Join(outputDir, "Old - part1.mp4")
		chapters := filepath.Join(outputDir, "Old"+utils.ChaptersSidecarSuffix)
		for _, p := range []string{part, chapters} {
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.AddSegment(&database.Segment{JobID: "j-ch-super", SegmentIndex: 0, UnixStart: 1_700_000_000,
			Filename: filepath.Base(part), FilePath: part}); err != nil {
			t.Fatal(err)
		}
		if err := w.orchestrator.supersedePartsWithVod(jobCtx); err != nil {
			t.Fatalf("supersede: %v", err)
		}
		if fileExists(chapters) {
			t.Error("the superseded parts' chapter list is still beside the archive")
		}
	})

	t.Run("aside recovery", func(t *testing.T) {
		staging, out := t.TempDir(), t.TempDir()
		for name, body := range map[string]string{"chat.json": `{"messages":[]}`, "chat.raw.jsonl": "{\"a\":1}\n"} {
			if err := os.WriteFile(filepath.Join(staging, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		sibling := filepath.Join(out, "Title"+engine.StagedRestartSuffix+"1700000000.mp4")
		if err := os.WriteFile(sibling, []byte("recovered"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := copyKeptChatSidecar(staging, sibling, filepath.Join(out, "Title.chat.json")); err != nil {
			t.Fatal(err)
		}
		raw := filepath.Join(out, "Title"+engine.StagedRestartSuffix+"1700000000.chat.raw.jsonl")
		if b, err := os.ReadFile(raw); err != nil || string(b) != "{\"a\":1}\n" {
			t.Errorf("the raw sidecar beside the recovered recording: %q, %v", b, err)
		}
	})
}
