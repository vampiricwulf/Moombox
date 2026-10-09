package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// writeTrimSource renders a real seconds-long MP4 with video and audio, the
// shape CreateTrim's precise re-encode expects of an archive.
func writeTrimSource(t *testing.T, ffmpegPath, path string, seconds string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ffmpegPath, "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=10:duration="+seconds,
		"-f", "lavfi", "-i", "sine=duration="+seconds,
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", "-movflags", "+faststart", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate trim source %s: %v\n%s", path, err, out)
	}
}

// orphanTypeOf returns the type the sweep offers path as, or "" when it is
// not offered.
func orphanTypeOf(t *testing.T, db *database.Database, cfg *config.MoomboxConfig, path string) string {
	t.Helper()
	entries, err := ScanOrphanedFiles(db, cfg)
	if err != nil {
		t.Fatalf("ScanOrphanedFiles: %v", err)
	}
	for _, e := range entries {
		if normalizePath(e.Path) == normalizePath(path) {
			return e.Type
		}
	}
	return ""
}

// A trim's row names its file relative to the JOB's output directory, which
// is the job's own output_directory when a channel (or the Add dialog, per
// job) overrides the global one. The sweep resolved every trim row against
// the global directory, so under an override inside it a live trim was
// offered as a "trim" orphan and Delete removed it, leaving the row naming a
// file that was gone (W23-07). Each case makes its trim with the real
// CreateTrim, so the layout pinned here is the one the trim service writes.
//
// Every case walks a trim's whole life: owned and undeletable while its row
// exists — after a Reinitialize too, which clears the job's output_file and
// filename but keeps its trims — then, once DeleteTrim drops the row and
// leaves the file for this sweep, offered as a "trim" and deletable.
//
// Mutants:
//   - trimFileLocations resolving against absOutputDir alone (the old rule):
//     the override cases fail once Reinitialize clears output_file.
//   - trimFileLocations without the beside-the-output candidates: the
//     "global directory moved" case fails at once.
//   - DeleteOrphanedFile without the findTrimForPath recheck: the delete of a
//     live trim succeeds.
//   - findTrimForPath's name filter keyed on the row's whole filename rather
//     than its base name: no row ever matches, the same.
//   - scanOutputOrphans not registering trimDirsOf(job) as trims directories:
//     the leftover is offered as "output", not "trim".
func TestOrphanSweepResolvesTrimsWhereTheTrimServiceWroteThem(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)

	cases := []struct {
		name, id string
		// jobDir is the job's output_directory relative to the root ("" =
		// none); sweepDir is the sweep's global output directory, relative
		// to the root; rel is the job's filename column.
		jobDir, sweepDir, rel string
		// moved: the global directory is no longer the one the job was
		// recorded under, so once Reinitialize clears output_file nothing
		// says where the trim went — that phase is skipped.
		moved bool
	}{
		{name: "per-channel directory inside the global one", id: "chvid000001",
			jobDir: "output/ChannelA", sweepDir: "output", rel: "Title [chvid000001].mp4"},
		{name: "per-channel directory with a template subdirectory", id: "chvid000002",
			jobDir: "output/ChannelA", sweepDir: "output", rel: filepath.Join("Chan", "Title [chvid000002].mp4")},
		{name: "global directory, template subdirectory", id: "chvid000003",
			sweepDir: "output", rel: filepath.Join("Chan", "Title [chvid000003].mp4")},
		{name: "global directory moved up a level since", id: "chvid000004",
			sweepDir: ".", rel: filepath.Join("Chan", "Title [chvid000004].mp4"), moved: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, db := testWorkerSetup(t)
			root := t.TempDir()
			base := filepath.Join(root, "output") // where the job was recorded
			if tc.jobDir != "" {
				base = filepath.Join(root, tc.jobDir)
			}
			src := filepath.Join(base, tc.rel)
			writeTrimSource(t, ffmpegPath, src, "3")
			id := tc.id
			job := &database.Job{
				ID: id, VideoID: id, URL: "u", Platform: "youtube", Title: "t",
				Status: database.StatusFinished, OutputFile: src, Filename: tc.rel,
			}
			if tc.jobDir != "" {
				job.OutputDirectory = base
			}
			if _, err := db.AddJob(job); err != nil {
				t.Fatal(err)
			}
			cfg := &config.MoomboxConfig{Paths: config.PathsConfig{
				OutputDirectory:  filepath.Join(root, tc.sweepDir),
				StagingDirectory: filepath.Join(root, "staging"),
			}}

			ts := NewTrimService(db, ffmpegPath, discardLogger{})
			rec, err := ts.CreateTrim(t.Context(), job, 0.5, 2, nil)
			if err != nil {
				t.Fatalf("CreateTrim: %v", err)
			}
			trimFile := filepath.Join(filepath.Dir(src), "trim", filepath.Base(rec.Filename))
			if _, err := os.Stat(trimFile); err != nil {
				t.Fatalf("the trim is not beside its archive: %v (row %q)", err, rec.Filename)
			}

			assertLiveTrim := func(when string) {
				t.Helper()
				if typ := orphanTypeOf(t, db, cfg, trimFile); typ != "" {
					t.Errorf("%s: the sweep offers a trim its row still names, as %q (row %q)", when, typ, rec.Filename)
				}
				if err := DeleteOrphanedFile(trimFile, db, cfg); err == nil {
					t.Errorf("%s: DeleteOrphanedFile removed a trim its row still names", when)
				}
				if _, err := os.Stat(trimFile); err != nil {
					t.Fatalf("%s: the trim is gone: %v", when, err)
				}
			}
			assertLiveTrim("finished")

			if !tc.moved {
				// What Reinitialize leaves: the trims, and no output columns.
				db.UpdateJobFields(id, map[string]any{
					"status": database.StatusUpcoming, "output_file": "", "filename": "",
				})
				assertLiveTrim("after Reinitialize")
				db.UpdateJobFields(id, map[string]any{
					"status": database.StatusFinished, "output_file": src, "filename": tc.rel,
				})
			}

			// The trim deleted the dashboard's way: the row goes, the file
			// stays for this sweep.
			if err := ts.DeleteTrim(id, rec.ID); err != nil {
				t.Fatalf("DeleteTrim: %v", err)
			}
			if typ := orphanTypeOf(t, db, cfg, trimFile); typ != "trim" {
				t.Errorf("the deleted trim's file is offered as %q, want \"trim\"", typ)
			}
			if err := DeleteOrphanedFile(trimFile, db, cfg); err != nil {
				t.Errorf("DeleteOrphanedFile refused the deleted trim's file: %v", err)
			}
			if typ := orphanTypeOf(t, db, cfg, src); typ != "" {
				t.Errorf("the archive is offered as %q", typ)
			}
		})
	}
}

// A job carries an output_directory far more often than an override implies:
// the monitor and an import store the global directory there at creation when
// the channel has none of its own. When the operator then moves the archive
// tree and repoints paths.output_directory at it, the archive stays owned —
// its relative filename joins the new global directory — but every spelling
// of its trim the sweep tried named the old tree: the job's pinned directory,
// and beside its absolute output_file. So each trim of every such job was
// listed as an "output" orphan and Delete removed the file its row names; with
// the old path left as a link to the new tree (which keeps the player
// working), it was listed all the same and each Delete was refused. The trim
// is made with the real CreateTrim, so the row is the one the service writes.
//
// Mutants:
//   - trimFileLocations resolving against the job's own output_directory in
//     place of the global one rather than as well (the replacement this fixes):
//     both cases offer the trim, and the plain move deletes it.
//   - the global-directory candidate kept only for a job with no
//     output_directory: the same.
func TestOrphanSweepKeepsTrimsOwnedAfterTheOutputTreeMoves(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)

	for _, tc := range []struct {
		name, id string
		link     bool // the old path left as a link to the new tree
	}{
		{name: "tree moved", id: "mvvid000001"},
		{name: "tree moved, old path left as a link", id: "mvvid000002", link: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, db := testWorkerSetup(t)
			root := t.TempDir()
			oldDir, newDir := filepath.Join(root, "old"), filepath.Join(root, "new")
			rel := filepath.Join("ChannelA", "20260101 Stream ["+tc.id+"].mp4")
			src := filepath.Join(oldDir, rel)
			writeTrimSource(t, ffmpegPath, src, "3")
			job := &database.Job{
				ID: tc.id, VideoID: tc.id, URL: "u", Platform: "youtube", Title: "t",
				Status: database.StatusFinished, OutputFile: src, Filename: rel,
				OutputDirectory: oldDir, // the global directory, as the monitor pins it
			}
			if _, err := db.AddJob(job); err != nil {
				t.Fatal(err)
			}
			ts := NewTrimService(db, ffmpegPath, discardLogger{})
			rec, err := ts.CreateTrim(t.Context(), job, 0.5, 2, nil)
			if err != nil {
				t.Fatalf("CreateTrim: %v", err)
			}

			if err := os.Rename(oldDir, newDir); err != nil {
				t.Fatal(err)
			}
			if tc.link {
				if err := os.Symlink(newDir, oldDir); err != nil {
					t.Skipf("cannot create a directory link here: %v", err)
				}
			}
			cfg := &config.MoomboxConfig{Paths: config.PathsConfig{
				OutputDirectory:  newDir,
				StagingDirectory: filepath.Join(root, "staging"),
			}}
			archive := filepath.Join(newDir, rel)
			trimFile := filepath.Join(newDir, rec.Filename)
			if _, err := os.Stat(trimFile); err != nil {
				t.Fatalf("the moved trim is not where its row resolves against the new global directory: %v", err)
			}

			if typ := orphanTypeOf(t, db, cfg, archive); typ != "" {
				t.Fatalf("the moved archive is offered as %q — the case needs it owned", typ)
			}
			if typ := orphanTypeOf(t, db, cfg, trimFile); typ != "" {
				t.Errorf("the sweep offers a trim its row still names, as %q (row %q)", typ, rec.Filename)
			}
			if err := DeleteOrphanedFile(trimFile, db, cfg); err == nil {
				t.Errorf("DeleteOrphanedFile removed a trim its row still names")
			}
			if _, err := os.Stat(trimFile); err != nil {
				t.Fatalf("the trim is gone: %v", err)
			}

			// Owned because a row names it, not because of where it is: with
			// the row gone the file is offered and deletable.
			if err := ts.DeleteTrim(tc.id, rec.ID); err != nil {
				t.Fatalf("DeleteTrim: %v", err)
			}
			if typ := orphanTypeOf(t, db, cfg, trimFile); typ == "" {
				t.Errorf("the deleted trim's file is not offered")
			}
			if err := DeleteOrphanedFile(trimFile, db, cfg); err != nil {
				t.Errorf("DeleteOrphanedFile refused the deleted trim's file: %v", err)
			}
		})
	}
}

// A directory is a trims directory because the trim service writes into it
// — "trim" beside a job's output, or the directory a trim row resolves to —
// never because of its name. The sweep read every directory NAMED "trim" as
// one: the output walk skipped it, and the trim walk offered every file in it
// that no trim row named. The default template is "${channel}/...", so a
// channel whose name sanitises to "trim" archives into <output>/trim, and its
// archives, chat, thumbnail and description were all offered for deletion as
// trims (W23-08).
//
// Here that channel's directory holds the job's four files (owned), a stray
// MP4 no row names (offered, as "output": it is not in a trims directory),
// and a note (not offered: outside a trims directory only media, chat,
// thumbnails and descriptions are). The channel's own trims directory,
// <output>/trim/trim, holds a leftover no trim row names: offered as "trim".
//
// Mutants:
//   - scanOutputOrphans skipping every directory named "trim" again: the stray
//     and the leftover are never offered.
//   - inTrimDir decided by the directory's name: the stray is typed "trim" and
//     the note is offered.
//   - scanOutputOrphans not registering trimDirsOf(job): the leftover is typed
//     "output".
func TestOrphanSweepDoesNotReadAChannelNamedTrimAsTrims(t *testing.T) {
	_, db := testWorkerSetup(t)
	outputDir := t.TempDir()
	chanDir := filepath.Join(outputDir, "trim")
	write := func(name string) string {
		p := filepath.Join(chanDir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stem := "20260102 Stream [arcvid12345]"
	archive := write(stem + ".mp4")
	chat := write(stem + ".chat.json")
	thumb := write(stem + ".jpg")
	desc := write(stem + ".description")
	stray := write("stray.mp4")
	note := write("notes.txt")
	leftover := write(filepath.Join("trim", "arcvid12345 [1s-3s].mp4"))

	job := &database.Job{
		ID: "arcvid12345", VideoID: "arcvid12345", URL: "u", Platform: "youtube", Title: "Stream",
		ChannelName: "trim", Status: database.StatusFinished,
		OutputFile: archive, Filename: filepath.Join("trim", stem+".mp4"),
		ChatFile: chat, ChatFilename: filepath.Join("trim", stem+".chat.json"),
		ThumbnailFile: thumb, DescriptionFile: desc,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	cfg := &config.MoomboxConfig{Paths: config.PathsConfig{OutputDirectory: outputDir, StagingDirectory: t.TempDir()}}

	for _, p := range []string{archive, chat, thumb, desc} {
		if typ := orphanTypeOf(t, db, cfg, p); typ != "" {
			t.Errorf("the channel's %s is offered as a %q orphan while its job names it", filepath.Base(p), typ)
		}
	}
	if typ := orphanTypeOf(t, db, cfg, stray); typ != "output" {
		t.Errorf("a stray MP4 in the channel's directory is offered as %q, want \"output\"", typ)
	}
	if typ := orphanTypeOf(t, db, cfg, note); typ != "" {
		t.Errorf("a note in the channel's directory is offered as %q — it is no trims directory", typ)
	}
	if typ := orphanTypeOf(t, db, cfg, leftover); typ != "trim" {
		t.Errorf("a leftover in the channel's trims directory is offered as %q, want \"trim\"", typ)
	}
}
