package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// aliasedDirs makes two names for one directory: real, and alias, a symlink
// to it. A file reached through either is the same file — the shape NTFS
// gives "X.chapters.json" and "x.chapters.json", which no case-sensitive test
// filesystem can: removing the file under one name deletes it under the
// other. Skips where the platform will not make the link (Windows without
// the symlink privilege).
func aliasedDirs(t *testing.T) (real, alias string) {
	t.Helper()
	root := t.TempDir()
	real = filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	alias = filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlink: %v", err)
	}
	return real, alias
}

// A re-finalize that re-resolves the archive to a name the filesystem treats
// as the one it read the kept import from (on Windows, a name differing only
// in case) wrote the import, compared the two names as strings, and removed
// the "old" one: the file it had just written. The import and the original
// it kept for Restore were gone (owner decision PR-M8).
//
// Mutant: keptImport.write back on `k.from != path`, the sidecar written is
// deleted.
func TestKeptImportWriteNeverRemovesTheFileItWrote(t *testing.T) {
	real, alias := aliasedDirs(t)
	path := filepath.Join(real, "Title [v1]"+utils.ChaptersSidecarSuffix)
	k := &keptImport{
		from:     filepath.Join(alias, "Title [v1]"+utils.ChaptersSidecarSuffix),
		chapters: []utils.Chapter{{StartMs: 0, EndMs: 60000, Title: "Intro"}, {StartMs: 60000, EndMs: 120000, Title: "Game"}},
		original: []utils.Chapter{{StartMs: 0, EndMs: 120000, Title: "Stream"}},
	}
	k.write(path, discardLogger{})

	imp, imported, err := utils.ReadImportedChaptersSidecar(path)
	if err != nil || !imported {
		t.Fatalf("the kept import is gone after its own write: imported=%v err=%v", imported, err)
	}
	if len(imp.Chapters) != 2 || len(imp.Original) != 1 {
		t.Errorf("kept import = %+v, want the two chapters and the original", imp)
	}
}

// keptImport.write still removes a sidecar under a really different name: the
// guard is for one file under two names, not for keeping strays.
func TestKeptImportWriteRemovesTheOldNameWhenItIsAnotherFile(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "Old [v1]"+utils.ChaptersSidecarSuffix)
	if err := utils.WriteImportedChaptersSidecar(from, []utils.Chapter{{StartMs: 0, EndMs: 1000, Title: "A"}}, nil); err != nil {
		t.Fatal(err)
	}
	k := &keptImport{from: from, chapters: []utils.Chapter{{StartMs: 0, EndMs: 1000, Title: "A"}}}
	path := filepath.Join(dir, "New [v1]"+utils.ChaptersSidecarSuffix)
	k.write(path, discardLogger{})
	if fileExists(from) {
		t.Error("the sidecar under the archive's old name was left behind")
	}
	if !fileExists(path) {
		t.Error("the sidecar was not written under the new name")
	}
}

// The gap-fill's refit of a split archive's chapter list had the same string
// compare before removing the list under the "old" base (PR-M8).
//
// Mutant: refitSplitArchiveChapters back on `oldPath != newPath` — the list it
// just rewrote is deleted.
func TestRefitSplitArchiveChaptersNeverRemovesTheFileItWrote(t *testing.T) {
	real, alias := aliasedDirs(t)
	oldSeg := database.Segment{FilePath: filepath.Join(alias, "Title [v1] - part1.mp4"), Filename: "Title [v1] - part1.mp4", DurationSeconds: 120}
	newSegs := []database.Segment{
		{FilePath: filepath.Join(real, "Title [v1] - part1.mp4"), Filename: "Title [v1] - part1.mp4", DurationSeconds: 120},
		{FilePath: filepath.Join(real, "Title [v1] - part2.mp4"), Filename: "Title [v1] - part2.mp4", DurationSeconds: 30},
	}
	list := filepath.Join(real, "Title [v1]"+utils.ChaptersSidecarSuffix)
	if err := utils.WriteChaptersSidecar(list, []utils.Chapter{{StartMs: 0, EndMs: 60000, Title: "A"}, {StartMs: 60000, EndMs: 120000, Title: "B"}}); err != nil {
		t.Fatal(err)
	}
	refitSplitArchiveChapters([]database.Segment{oldSeg}, newSegs, map[string]bool{newSegs[1].FilePath: true}, discardLogger{})
	got, err := utils.ReadChaptersSidecar(list)
	if err != nil || len(got) != 2 {
		t.Fatalf("the refitted chapter list is gone after its own write: %v (%d chapters)", err, len(got))
	}
}

// sameFilePath's two arms: one path spelled two ways, and two names for one
// file. Distinct files are distinct.
func TestSameFilePath(t *testing.T) {
	real, alias := aliasedDirs(t)
	a := filepath.Join(real, "a.json")
	if err := os.WriteFile(a, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := filepath.Join(real, "b.json")
	if err := os.WriteFile(b, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !sameFilePath(a, filepath.Join(real, ".", "a.json")) {
		t.Error("one path spelled two ways read as two files")
	}
	if !sameFilePath(a, filepath.Join(alias, "a.json")) {
		t.Error("one file under two names read as two files")
	}
	if sameFilePath(a, b) {
		t.Error("two files read as one")
	}
	if sameFilePath(a, filepath.Join(real, "missing.json")) {
		t.Error("a missing file read as an existing one")
	}
}
