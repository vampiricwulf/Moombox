package worker

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// orphanFixture is an output tree and staging directory beside a fresh
// database, with the config the sweep and the delete read.
type orphanFixture struct {
	db        *database.Database
	cfg       *config.MoomboxConfig
	outputDir string
}

func newOrphanFixture(t *testing.T) *orphanFixture {
	t.Helper()
	_, db := testWorkerSetup(t)
	root := t.TempDir()
	f := &orphanFixture{db: db, outputDir: filepath.Join(root, "output")}
	f.cfg = &config.MoomboxConfig{Paths: config.PathsConfig{
		OutputDirectory:  f.outputDir,
		StagingDirectory: filepath.Join(root, "staging"),
	}}
	return f
}

// write creates a file under the output tree (rel is relative to it).
func (f *orphanFixture) write(t *testing.T, rel string) string {
	t.Helper()
	p := filepath.Join(f.outputDir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// refusedAsStale asserts that DeleteOrphanedFile refused path as no longer
// an orphan — owned by owner, the message telling the operator to refresh
// the list — and left it on disk.
func refusedAsStale(t *testing.T, f *orphanFixture, path, owner string) {
	t.Helper()
	err := DeleteOrphanedFile(path, f.db, f.cfg)
	var notOrphan *NotOrphanError
	if !errors.As(err, &notOrphan) {
		t.Fatalf("DeleteOrphanedFile(%s) = %v, want a *NotOrphanError", filepath.Base(path), err)
	}
	if notOrphan.Owner != owner {
		t.Errorf("refused as owned by %q, want %q", notOrphan.Owner, owner)
	}
	if !strings.HasSuffix(err.Error(), "Refresh the list.") || !strings.HasPrefix(err.Error(), "No longer an orphan: "+owner+" ") {
		t.Errorf("the refusal reads %q — it must say what owns the path and tell the operator to refresh the list", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("the refused path is gone: %v", statErr)
	}
}

// The delete re-checked only ACTIVE jobs' columns and trim rows, so a file a
// job in any other status named — the commonest being a Finished job's
// archive that appeared after the listing: a mux that finished between the
// sweep and the click, a re-download onto a listed leftover's name — was
// deleted by a Delete click on the stale list (owner decision, W24). The
// delete now refuses any path any job or trim row names, whatever the job's
// status, with a message telling the operator to refresh the list; the sweep,
// asked again, agrees. With the row gone the file is a genuine orphan again
// and deletable: nothing a row does not name is lost.
//
// Each case lists the file first (an orphan), then lands the row, then
// deletes from the stale list.
//
// Mutants:
//   - orphanOwner without the rows check: every case's file is deleted.
//   - newOutputOwners counting only active jobs' rows (the old recheck's
//     status filter): every job case is deleted.
//   - jobFileLocations without the thumbnail and description columns: those
//     two cases are deleted.
//   - jobFileLocations without the parts, or without a part's chat: the part
//     cases are deleted.
//   - jobFileLocations without the relative columns: the relative case is
//     deleted.
//   - newOutputOwners without the trim rows: the trim case is deleted.
func TestDeleteOrphanedFileRefusesWhatAnyRowNamesNow(t *testing.T) {
	cases := []struct {
		name   string
		file   string
		status database.JobStatus
		// land writes the row that names path, for job id.
		land  func(t *testing.T, f *orphanFixture, id, path string)
		owner func(id string) string
	}{
		{name: "a Finished job's archive", file: "Chan/Stream [rowvid00001].mp4", status: database.StatusFinished,
			land: addJobNaming(func(j *database.Job, p string) { j.OutputFile = p })},
		{name: "an Error job's chat", file: "Chan/Stream [rowvid00002].chat.json", status: database.StatusError,
			land: addJobNaming(func(j *database.Job, p string) { j.ChatFile = p })},
		{name: "a Cancelled job's thumbnail", file: "Chan/Stream [rowvid00003].jpg", status: database.StatusCancelled,
			land: addJobNaming(func(j *database.Job, p string) { j.ThumbnailFile = p })},
		{name: "a Queued job's description", file: "Chan/Stream [rowvid00004].description", status: database.StatusQueued,
			land: addJobNaming(func(j *database.Job, p string) { j.DescriptionFile = p })},
		{name: "a COOKIES? job's part", file: "Chan/Stream [rowvid00005] - part2.mp4", status: database.StatusCookies,
			land: addJobWithPart(func(s *database.Segment, p string) { s.FilePath = p })},
		{name: "a Finished job's part chat", file: "Chan/Stream [rowvid00006] - part2.chat.json", status: database.StatusFinished,
			land: addJobWithPart(func(s *database.Segment, p string) { s.ChatFile = p })},
		{name: "a Finished job's archive by its relative column, under its own directory",
			file: "ChannelA/Stream [rowvid00007].mp4", status: database.StatusFinished,
			land: func(t *testing.T, f *orphanFixture, id, path string) {
				addJob(t, f, id, database.StatusFinished, func(j *database.Job) {
					j.OutputDirectory = filepath.Join(f.outputDir, "ChannelA")
					j.Filename = filepath.Base(path)
				})
			}},
		{name: "a Finished job's trim", file: "Chan/trim/rowvid00008 [1s-3s].mp4", status: database.StatusFinished,
			land: func(t *testing.T, f *orphanFixture, id, path string) {
				addJob(t, f, id, database.StatusFinished, func(j *database.Job) {
					j.OutputFile = filepath.Join(f.outputDir, "Chan", "Stream ["+id+"].mp4")
					j.Filename = filepath.Join("Chan", "Stream ["+id+"].mp4")
				})
				if err := f.db.AddTrim(&database.TrimRecord{
					ID: "trim_" + id + "_1", JobID: id, StartTime: 1, EndTime: 3, Duration: 2,
					Filename: filepath.Join("Chan", "trim", filepath.Base(path)), CreatedAt: "2026-10-01T00:00:00Z",
				}); err != nil {
					t.Fatal(err)
				}
			},
			owner: func(id string) string { return "a trim of job " + id }},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrphanFixture(t)
			id := "rowvid0000" + string(rune('1'+i))
			path := f.write(t, tc.file)
			if typ := orphanTypeOf(t, f.db, f.cfg, path); typ == "" {
				t.Fatalf("setup: %s is not listed before its row lands", tc.file)
			}

			tc.land(t, f, id, path)
			if tc.status != "" {
				f.db.UpdateJobFields(id, map[string]any{"status": tc.status})
			}
			owner := "job " + id
			if tc.owner != nil {
				owner = tc.owner(id)
			}
			refusedAsStale(t, f, path, owner)
			if typ := orphanTypeOf(t, f.db, f.cfg, path); typ != "" {
				t.Errorf("the sweep, asked again, still lists %s as %q — the delete and the list disagree", tc.file, typ)
			}

			if err := f.db.DeleteJob(id); err != nil {
				t.Fatal(err)
			}
			if err := DeleteOrphanedFile(path, f.db, f.cfg); err != nil {
				t.Errorf("with the row gone, the genuine orphan was refused: %v", err)
			}
		})
	}
}

// addJob adds a job with id, status and whatever edit sets.
func addJob(t *testing.T, f *orphanFixture, id string, status database.JobStatus, edit func(*database.Job)) {
	t.Helper()
	job := &database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube", Title: "t", Status: status}
	if edit != nil {
		edit(job)
	}
	if _, err := f.db.AddJob(job); err != nil {
		t.Fatal(err)
	}
}

// addJobNaming lands a Finished job whose column (set) names the path.
func addJobNaming(set func(*database.Job, string)) func(*testing.T, *orphanFixture, string, string) {
	return func(t *testing.T, f *orphanFixture, id, path string) {
		addJob(t, f, id, database.StatusFinished, func(j *database.Job) { set(j, path) })
	}
}

// addJobWithPart lands a job with one recorded part whose field (set) names
// the path.
func addJobWithPart(set func(*database.Segment, string)) func(*testing.T, *orphanFixture, string, string) {
	return func(t *testing.T, f *orphanFixture, id, path string) {
		addJob(t, f, id, database.StatusFinished, nil)
		seg := &database.Segment{JobID: id, SegmentIndex: 1, Quality: "1080p", Filename: filepath.Base(path)}
		set(seg, path)
		if err := f.db.AddSegment(seg); err != nil {
			t.Fatal(err)
		}
	}
}

// What the sweep owns beyond the files rows name, the delete refuses too: a
// recovered set-aside recording named after an archive a row names, a file a
// row names through a linked output directory, and a directory holding a file
// a row names or a finalize is writing — the sweep never lists a directory
// under the output tree, but the route takes any path, and RemoveAll on a
// channel's directory took every archive in it.
//
// Mutants:
//   - ownerOf without the set-aside arm: the sibling is deleted.
//   - ownerOf without the canonical-spelling arm: the file named through the
//     link is deleted.
//   - ownerOf without the directory arm: the channel directory is deleted.
//   - orphanOwner without outputClaimOwnerUnder: the directory a mux is
//     writing into is deleted.
func TestDeleteOrphanedFileRefusesWhatTheSweepWouldOwn(t *testing.T) {
	t.Run("a recovered set-aside recording whose archive a row names now", func(t *testing.T) {
		f := newOrphanFixture(t)
		sibling := f.write(t, filepath.Join("Chan", "Stream [asdvid00001].restart-1700000000.mp4"))
		if typ := orphanTypeOf(t, f.db, f.cfg, sibling); typ == "" {
			t.Fatal("setup: the lone sibling is not listed")
		}
		archive := f.write(t, filepath.Join("Chan", "Stream [asdvid00001].mp4"))
		addJob(t, f, "asdvid00001", database.StatusFinished, func(j *database.Job) { j.OutputFile = archive })
		refusedAsStale(t, f, sibling, "job asdvid00001")
	})

	t.Run("a file a row names through a linked output directory", func(t *testing.T) {
		f := newOrphanFixture(t)
		realOut := f.outputDir + "-real"
		if err := os.MkdirAll(realOut, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realOut, f.outputDir); err != nil {
			t.Skipf("cannot create a directory link here: %v", err)
		}
		f.write(t, filepath.Join("Chan", "Stream [lnkvid00001].mp4"))
		addJob(t, f, "lnkvid00001", database.StatusFinished, func(j *database.Job) {
			j.OutputFile = filepath.Join(f.outputDir, "Chan", "Stream [lnkvid00001].mp4")
		})
		refusedAsStale(t, f, filepath.Join(realOut, "Chan", "Stream [lnkvid00001].mp4"), "job lnkvid00001")
	})

	t.Run("a directory holding a file a row names", func(t *testing.T) {
		f := newOrphanFixture(t)
		archive := f.write(t, filepath.Join("Chan", "2026", "Stream [dirvid00001].mp4"))
		stray := f.write(t, filepath.Join("Strays", "stray.mp4"))
		addJob(t, f, "dirvid00001", database.StatusFinished, func(j *database.Job) { j.OutputFile = archive })
		refusedAsStale(t, f, filepath.Join(f.outputDir, "Chan"), "job dirvid00001")
		// A directory of nothing but orphans is no row's.
		if err := DeleteOrphanedFile(filepath.Dir(stray), f.db, f.cfg); err != nil {
			t.Errorf("a directory holding only an orphan was refused: %v", err)
		}
	})

	t.Run("a directory holding a file a finalize is writing", func(t *testing.T) {
		f := newOrphanFixture(t)
		f.write(t, filepath.Join("New Channel", "Stream [mtxvid00001].mp4"))
		release := claimOutputStem("mtxvid00001", filepath.Join(f.outputDir, "New Channel", "Stream [mtxvid00001]"))
		defer release()
		refusedAsStale(t, f, filepath.Join(f.outputDir, "New Channel"), "job mtxvid00001")
	})
}

// The delete's recheck reads the rows in one pass: one GetAllJobs (every job
// with its parts) and one GetAllTrims, and nothing per row — the trim half of
// the old recheck read each matching trim's job with its own GetJob. The
// shape is read from the syntax tree, as TestScanTrimOrphansIssuesOneQuery
// reads the sweep's.
//
// Mutant: a per-row GetJob (or GetTrimsForJob) back in orphanOwner.
func TestOrphanRecheckReadsTheRowsInOnePass(t *testing.T) {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "orphans.go", nil, 0)
	if err != nil {
		t.Fatalf("parse orphans.go: %v", err)
	}
	var recheck *ast.FuncDecl
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "orphanOwner" {
			recheck = fn
		}
	}
	if recheck == nil {
		t.Fatal("no orphanOwner declaration in orphans.go")
	}
	for method, want := range map[string]int{"GetAllJobs": 1, "GetAllTrims": 1, "GetJob": 0, "GetTrimsForJob": 0} {
		if n := len(methodCallPositions(recheck, method)); n != want {
			t.Errorf("orphanOwner calls %s %d time(s), want %d — the rows are read in one pass", method, n, want)
		}
	}
}
