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
// The directory and the set-aside recording are refused by their real
// spelling too. Behind a symlinked output directory the rows store the
// link's spelling, and the containment check accepts the real one;
// only a file's exact name was matched canonically, so a request naming the
// channel's directory, or a recovered recording, through the real path
// matched no row, and RemoveAll took the Finished archives.
//
// Mutants:
//   - ownerOf without the set-aside arm: the sibling is deleted.
//   - newOutputOwners indexing the rows in their stored spelling alone: the
//     file named through the link, and the directory and the sibling by
//     their real spelling, are deleted.
//   - ownerOf without the directory arm: the channel directory is deleted.
//   - newOutputOwners recording only the stored spelling's directories in
//     dirs: the channel directory by its real spelling is deleted.
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

	// The output directory a link to realOut, the row naming the archive
	// through it.
	linkedOutput := func(t *testing.T, id string) (f *orphanFixture, realOut, archive string) {
		t.Helper()
		f = newOrphanFixture(t)
		realOut = f.outputDir + "-real"
		if err := os.MkdirAll(realOut, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realOut, f.outputDir); err != nil {
			t.Skipf("cannot create a directory link here: %v", err)
		}
		archive = f.write(t, filepath.Join("Chan", "Stream ["+id+"].mp4"))
		addJob(t, f, id, database.StatusFinished, func(j *database.Job) { j.OutputFile = archive })
		return f, realOut, archive
	}

	t.Run("a directory holding a file a row names, by its real spelling", func(t *testing.T) {
		f, realOut, archive := linkedOutput(t, "dirlnk00001")
		refusedAsStale(t, f, filepath.Join(realOut, "Chan"), "job dirlnk00001")
		if _, err := os.Stat(archive); err != nil {
			t.Fatalf("the archive is gone: %v", err)
		}
	})

	t.Run("a recovered set-aside recording, by its real spelling", func(t *testing.T) {
		f, realOut, _ := linkedOutput(t, "siblnk00001")
		f.write(t, filepath.Join("Chan", "Stream [siblnk00001].restart-1700000000.mp4"))
		refusedAsStale(t, f, filepath.Join(realOut, "Chan", "Stream [siblnk00001].restart-1700000000.mp4"), "job siblnk00001")
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

// The sweep owns what a row names through a link exactly as the delete does,
// so a file the delete refuses as "Refresh the list." is never on the list.
// The sweep matched the rows in their stored spelling only, while the delete
// also matched canonical spellings: a Finished job whose channel directory is
// spelled through a link into the global tree had its archive, thumbnail and
// description listed as "output" orphans, every Delete of them answered 409
// "Refresh the list.", and the refreshed list offered them again — a list
// that could never be cleared. The same held after the global directory was
// respelled from a link to its target, for every column the relative ones
// do not cover. Both sides now ask ownerOf, each spelling through its
// directory's canonical one.
//
// Mutants:
//   - newOutputOwners indexing the rows in their stored spelling alone: the
//     linked job's files are listed, and refused by the delete.
//   - ownerOf matching the path's own spelling alone: with the walk and the
//     row each through a different link, the archive is listed.
//   - scanOutputOrphans looking the walked path up in files and stems itself,
//     as before, rather than asking ownerOf: the same.
//   - newOutputOwners indexing the parts' stem in its stored spelling alone:
//     the recording set aside beside the linked split job is listed.
//   - trimDirs indexed in the stored spelling alone, or isTrimDir asking the
//     walked directory's own spelling alone: the leftover in the trims
//     directory is typed "output".
func TestOutputSweepOwnsWhatARowNamesThroughALink(t *testing.T) {
	// notListed asserts the sweep does not list path, the delete
	// refuses it as owned by owner, and the sweep asked again still does not
	// list it — the operator who follows "Refresh the list." finds it gone.
	notListed := func(t *testing.T, f *orphanFixture, path, owner string) {
		t.Helper()
		if typ := orphanTypeOf(t, f.db, f.cfg, path); typ != "" {
			t.Errorf("%s is listed as a %q orphan while a row names it through a link", filepath.Base(path), typ)
		}
		refusedAsStale(t, f, path, owner)
		if typ := orphanTypeOf(t, f.db, f.cfg, path); typ != "" {
			t.Errorf("after the delete refused %s, the refreshed list still offers it as %q", filepath.Base(path), typ)
		}
	}

	t.Run("a channel directory spelled through a link into the global tree", func(t *testing.T) {
		f := newOrphanFixture(t)
		if err := os.MkdirAll(f.outputDir, 0o755); err != nil {
			t.Fatal(err)
		}
		alias := f.outputDir + "-alias"
		if err := os.Symlink(f.outputDir, alias); err != nil {
			t.Skipf("cannot create a directory link here: %v", err)
		}
		chanDir := filepath.Join(alias, "ChannelA")
		archive := f.write(t, filepath.Join("ChannelA", "Stream [lnkch000001].mp4"))
		thumb := f.write(t, filepath.Join("ChannelA", "Stream [lnkch000001].jpg"))
		desc := f.write(t, filepath.Join("ChannelA", "Stream [lnkch000001].description"))
		addJob(t, f, "lnkch000001", database.StatusFinished, func(j *database.Job) {
			j.OutputDirectory = chanDir
			j.Filename = "Stream [lnkch000001].mp4"
			j.OutputFile = filepath.Join(chanDir, "Stream [lnkch000001].mp4")
			j.ThumbnailFile = filepath.Join(chanDir, "Stream [lnkch000001].jpg")
			j.DescriptionFile = filepath.Join(chanDir, "Stream [lnkch000001].description")
		})
		// A split job beside it, the dot in its title keeping its filename
		// from carrying the set-aside recording's stem: only its parts do.
		partBase := "Stream v1.5 [lnkprt00001]"
		f.write(t, filepath.Join("ChannelA", partBase+" - part1.mp4"))
		aside := f.write(t, filepath.Join("ChannelA", partBase+".restart-1700000000.mp4"))
		addJob(t, f, "lnkprt00001", database.StatusFinished, func(j *database.Job) {
			j.OutputDirectory = chanDir
			j.Filename = partBase
			j.OutputFile = filepath.Join(chanDir, partBase+" - part1.mp4")
		})
		if err := f.db.AddSegment(&database.Segment{JobID: "lnkprt00001", SegmentIndex: 0, Quality: "1080p",
			Filename: partBase + " - part1.mp4", FilePath: filepath.Join(chanDir, partBase+" - part1.mp4")}); err != nil {
			t.Fatal(err)
		}
		stray := f.write(t, filepath.Join("ChannelA", "stray.mp4"))

		for _, p := range []string{archive, thumb, desc} {
			notListed(t, f, p, "job lnkch000001")
		}
		notListed(t, f, aside, "job lnkprt00001")
		if typ := orphanTypeOf(t, f.db, f.cfg, stray); typ != "output" {
			t.Errorf("the stray beside them is listed as %q, want \"output\"", typ)
		}
		if err := DeleteOrphanedFile(stray, f.db, f.cfg); err != nil {
			t.Errorf("the stray beside them was refused: %v", err)
		}
	})

	t.Run("the walk and the row each through a different link", func(t *testing.T) {
		f := newOrphanFixture(t)
		root := filepath.Dir(f.outputDir)
		realOut := filepath.Join(root, "real", "out")
		if err := os.MkdirAll(realOut, 0o755); err != nil {
			t.Fatal(err)
		}
		// The global directory through a link to its parent, the row through
		// a link to the directory itself: neither spelling is the other's,
		// and they meet only in the canonical one.
		if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "viaA")); err != nil {
			t.Skipf("cannot create a directory link here: %v", err)
		}
		if err := os.Symlink(realOut, filepath.Join(root, "viaB")); err != nil {
			t.Fatal(err)
		}
		f.outputDir = filepath.Join(root, "viaA", "out")
		f.cfg.Paths.OutputDirectory = f.outputDir
		rowDir := filepath.Join(root, "viaB", "Chan")
		archive := f.write(t, filepath.Join("Chan", "Stream [twolnk00001].mp4"))
		leftover := f.write(t, filepath.Join("Chan", "trim", "twolnk00001 [1s-3s].mp4"))
		addJob(t, f, "twolnk00001", database.StatusFinished, func(j *database.Job) {
			j.OutputFile = filepath.Join(rowDir, "Stream [twolnk00001].mp4")
		})

		notListed(t, f, archive, "job twolnk00001")
		if typ := orphanTypeOf(t, f.db, f.cfg, leftover); typ != "trim" {
			t.Errorf("a leftover in the trims directory beside the archive is listed as %q, want \"trim\"", typ)
		}
		refusedAsStale(t, f, filepath.Join(f.outputDir, "Chan"), "job twolnk00001")
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
