package routes

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestPathStrictlyInside pins the containment rule every read route, Open
// Folder and the import's writes share, over canonical path strings in the
// host's own spelling: a volume root as the directory (`E:\`, a UNC share
// root, "/") contains what is on it, and the directory itself, its parent, a
// sibling sharing its name as a prefix and another volume do not. A name
// that merely starts with ".." is a file like any other.
//
// Mutants it kills (each compiles):
//   - the old `strings.HasPrefix(file, dir+string(filepath.Separator))`:
//     the volume-root rows fail (the prefix becomes `E:\\` or "//");
//   - `rel == "."` dropped: the directory-itself row passes as inside;
//   - `rel != ".."` dropped: the parent row passes as inside;
//   - the `".."+separator` prefix test dropped: the outside-sibling row
//     passes as inside;
//   - that prefix test narrowed to `strings.HasPrefix(rel, "..")`: the
//     "..name" row fails;
//   - `err != nil` dropped (Windows only): the other-volume row, whose Rel
//     fails with an empty rel, passes as inside.
func TestPathStrictlyInside(t *testing.T) {
	type row struct {
		name      string
		file, dir string
		want      bool
	}
	var rows []row
	if runtime.GOOS == "windows" {
		rows = []row{
			{"under a drive root", `E:\Chan\Stream [abcdefghijk].mp4`, `E:\`, true},
			{"directly on a drive root", `E:\Stream.mp4`, `E:\`, true},
			{"under a UNC share root", `\\nas\share\Chan\x.mp4`, `\\nas\share\`, true},
			{"under a plain directory", `E:\Archive\Chan\x.mp4`, `E:\Archive`, true},
			{"a name starting with ..", `E:\Archive\..x.mp4`, `E:\Archive`, true},
			{"the drive root itself", `E:\`, `E:\`, false},
			{"the directory itself", `E:\Archive`, `E:\Archive`, false},
			{"the parent", `E:\`, `E:\Archive`, false},
			{"a sibling sharing the prefix", `E:\Archive2\x.mp4`, `E:\Archive`, false},
			{"outside the directory", `E:\Other\x.mp4`, `E:\Archive`, false},
			{"another volume", `D:\Chan\x.mp4`, `E:\`, false},
		}
	} else {
		rows = []row{
			{"under the root", "/Chan/Stream [abcdefghijk].mp4", "/", true},
			{"directly on the root", "/Stream.mp4", "/", true},
			{"under a plain directory", "/srv/archive/Chan/x.mp4", "/srv/archive", true},
			{"a name starting with ..", "/srv/archive/..x.mp4", "/srv/archive", true},
			{"the root itself", "/", "/", false},
			{"the directory itself", "/srv/archive", "/srv/archive", false},
			{"the parent", "/srv", "/srv/archive", false},
			{"a sibling sharing the prefix", "/srv/archive2/x.mp4", "/srv/archive", false},
			{"outside the directory", "/srv/other/x.mp4", "/srv/archive", false},
		}
	}
	for _, r := range rows {
		if got := pathStrictlyInside(r.file, r.dir); got != r.want {
			t.Errorf("%s: pathStrictlyInside(%q, %q) = %v, want %v", r.name, r.file, r.dir, got, r.want)
		}
	}
}

// volumeRootOf is the root of the volume dir is on: "/" off Windows, `C:\`
// (or the runner's `D:\`) on it — the layout of an archive given its own
// drive, output_directory = 'E:\'.
func volumeRootOf(dir string) string {
	return filepath.VolumeName(dir) + string(filepath.Separator)
}

// TestReadRoutesServeUnderAVolumeRootOutputDirectory is the W27-01
// reproduction through the router: a job whose output directory is the root
// of the volume its recording is on plays and serves its chat. Every one of
// these answered 403 "access denied", because the containment check required
// the root plus a second separator.
//
// Mutant it kills: the old `dir+separator` prefix test in place of
// pathStrictlyInside (both requests answer 403).
func TestReadRoutesServeUnderAVolumeRootOutputDirectory(t *testing.T) {
	f := newJobsFixture(t)
	dir := t.TempDir()
	root := volumeRootOf(dir)
	video := filepath.Join(dir, "Chan", "Stream [abcdefghijk].mp4")
	chat := filepath.Join(dir, "Chan", "Stream [abcdefghijk].chat.json")
	if err := os.MkdirAll(filepath.Dir(video), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(video, []byte("root bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chat, []byte(`{"messages":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	relVideo, err := filepath.Rel(root, video)
	if err != nil {
		t.Fatal(err)
	}
	relChat, err := filepath.Rel(root, chat)
	if err != nil {
		t.Fatal(err)
	}
	f.addJob(t, "yt_rootdir", func(j *database.Job) {
		j.OutputDirectory = root
		j.Filename = relVideo
		j.ChatFilename = relChat
	})

	if rec := doRequest(t, f.router, http.MethodGet, "/api/jobs/yt_rootdir/video", nil); rec.Code != http.StatusOK {
		t.Errorf("video under output_directory %q: want 200, got %d %s", root, rec.Code, rec.Body.String())
	}
	if rec := doRequest(t, f.router, http.MethodGet, "/api/jobs/yt_rootdir/chat", nil); rec.Code != http.StatusOK {
		t.Errorf("chat under output_directory %q: want 200, got %d %s", root, rec.Code, rec.Body.String())
	}
}

// TestChooseImportStemUnderAVolumeRootOutputDirectory: an import into an
// output directory at a volume root picks its stem rather than failing with
// errImportInvalidPath ("invalid output path", 400) — the import's writes go
// through the same containment check as the read routes.
//
// Mutant it kills: the old `dir+separator` prefix test in place of
// pathStrictlyInside (chooseImportStem returns errImportInvalidPath).
func TestChooseImportStemUnderAVolumeRootOutputDirectory(t *testing.T) {
	importsDir := filepath.Join(t.TempDir(), "imports")
	root := volumeRootOf(importsDir)
	f := &importFile{kind: "video", suffix: ".mp4"}
	stem, err := chooseImportStem(importsDir, root, "Stream [abcdefghijk]", []*importFile{f})
	if err != nil {
		t.Fatalf("chooseImportStem under output directory %q: %v", root, err)
	}
	if want := filepath.Join(importsDir, "Stream [abcdefghijk].mp4"); stem != "Stream [abcdefghijk]" || f.dest != want {
		t.Errorf("stem %q dest %q, want stem %q dest %q", stem, f.dest, "Stream [abcdefghijk]", want)
	}
}
