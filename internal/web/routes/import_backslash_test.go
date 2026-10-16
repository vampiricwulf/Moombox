package routes

import (
	"archive/zip"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestImportReadsABackslashEntryNameAsAFolder is the W27-04 reproduction:
// Windows PowerShell 5.1's Compress-Archive writes a folder's entries with
// backslashes ("Folder\Stream [id].mp4"), and the import reads such a zip the
// same way on every host — as a folder holding "Stream [id].mp4". A Linux
// host (the Docker image) titled it "Folder\Stream" and wrote
// "imports/Folder_Stream [id].mp4", where a Windows host titled it "Stream".
//
// Mutant it kills: zipEntryName returning the name without its backslashes
// rewritten (the title, the stored name and the paired chat all carry the
// folder; on Windows too, since the names are now read with path.Base).
// filepath.Base in place of path.Base at the three name reads is an
// equivalent mutant once every name is slash-separated, and is not killed.
func TestImportReadsABackslashEntryNameAsAFolder(t *testing.T) {
	f := newImportFixture(t)
	rec, job := importZip(t, f, orderedImportZip(t,
		importEntry{name: `Folder\Stream [abcd1234567].mp4`, data: []byte("VIDEO")},
		importEntry{name: `Folder\Stream [abcd1234567].chat.json`, data: chatJSONFor(t, map[string]any{})},
	))
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	if job.Title != "Stream" {
		t.Errorf("title %q, want \"Stream\" (the entry's own name, as a Windows host reads it)", job.Title)
	}
	if want := filepath.FromSlash("imports/Stream [abcd1234567].mp4"); job.Filename != want {
		t.Errorf("filename %q, want %q", job.Filename, want)
	}
	if want := filepath.FromSlash("imports/Stream [abcd1234567].chat.json"); job.ChatFilename != want {
		t.Errorf("chat %q, want %q (the chat named after the video, paired)", job.ChatFilename, want)
	}
}

// TestImportNamesABackslashEntryWithoutItsFolder: a chat named after no video
// is reported by its own name, and a zip of two recordings is refused naming
// their files — the folder a backslash name carries left off, as it is for a
// forward-slash one.
//
// Mutant it kills: zipEntryName returning the name without its backslashes
// rewritten ("Folder\other.chat.json" is reported; the refusal names
// "Folder\A [...].mp4").
func TestImportNamesABackslashEntryWithoutItsFolder(t *testing.T) {
	f := newImportFixture(t)
	rec, _ := importZip(t, f, orderedImportZip(t,
		importEntry{name: `Folder\Stream [abcd1234567].mp4`, data: []byte("VIDEO")},
		importEntry{name: `Folder\other.chat.json`, data: chatJSONFor(t, map[string]any{})},
	))
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	if r := decodeImportResult(t, rec.Body.Bytes()); !slices.Equal(r.Import.UnpairedChats, []string{"other.chat.json"}) {
		t.Errorf("unpaired chats %q, want [\"other.chat.json\"]", r.Import.UnpairedChats)
	}

	rec, _ = importZip(t, newImportFixture(t), orderedImportZip(t,
		importEntry{name: `Folder\A [abcd1234567].mp4`, data: []byte("A")},
		importEntry{name: `Folder\B [efgh1234567].mp4`, data: []byte("B")},
	))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("two recordings: %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "Folder") || !strings.Contains(body, "A [abcd1234567].mp4") {
		t.Errorf("refusal %s: want the videos named without their folder", body)
	}
}

// TestImportRefusesABackslashTraversalOnEveryHost: the entry-name traversal
// rule reads the name through zipEntryName, so a ".." component or a leading
// separator written with backslashes is refused on a Linux host as a Windows
// one's filepath already refused it.
//
// Mutants it kills: the check reading the raw f.Name (path.Clean(f.Name):
// neither name has a "/" to split on, so both import); zipEntryName returning
// the name without its backslashes rewritten (the same, on Linux).
func TestImportRefusesABackslashTraversalOnEveryHost(t *testing.T) {
	for _, name := range []string{`Folder\..\..\escape [abcd1234567].mp4`, `\abs [abcd1234567].mp4`} {
		rec, _ := importZip(t, newImportFixture(t), makeImportZip(t, map[string][]byte{name: []byte("v")}))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid zip entry path") {
			t.Errorf("%q: %d %s, want 400 invalid zip entry path", name, rec.Code, rec.Body.String())
		}
	}
}

// A Linux host imported a backslash-named archive, before a backslash was a
// separator, as "Folder\Stream" — "imports/Folder_Stream [<id>].mp4" and its
// chat. Deleting the row (which leaves the files) and re-importing the same
// zip after the upgrade, the natural way to correct that title, found the new
// name "Stream [<id>]" free and wrote a second full copy of the recording and
// its chat beside the old pair, with a plain 201. The old pair is re-adopted
// when it holds the same bytes; a different file there is left alone, and the
// import takes the new name as any import would.
//
// Mutants: importBackslashTitle always returning this import's title (the
// second copy); zipEntryWrittenName returning the rewritten name (the same:
// the written name then never differs); the chat loop not trying the legacy
// stems (the chat is copied a second time beside the re-adopted video).
func TestImportReadoptsAPreUpgradeBackslashName(t *testing.T) {
	const oldStem = "Folder_Stream [abcd1234567]"
	const newStem = "Stream [abcd1234567]"
	chat := chatJSONFor(t, map[string]any{})
	entries := []importEntry{
		{name: `Folder\Stream [abcd1234567].mp4`, data: []byte("ORIGINAL-RECORDING")},
		{name: `Folder\Stream [abcd1234567].chat.json`, data: chat},
	}
	seed := func(t *testing.T, f *importFixture, name string, data []byte) string {
		t.Helper()
		p := filepath.Join(f.outputDir, "imports", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("identical", func(t *testing.T) {
		f := newImportFixture(t)
		video := seed(t, f, oldStem+".mp4", []byte("ORIGINAL-RECORDING"))
		oldChat := seed(t, f, oldStem+".chat.json", chat)
		rec, job := importZip(t, f, orderedImportZip(t, entries...))
		if rec.Code != http.StatusCreated {
			t.Fatalf("re-import: %d (body %s)", rec.Code, rec.Body.String())
		}
		r := decodeImportResult(t, rec.Body.Bytes())
		if r.OutputFile != video || r.ChatFile != oldChat {
			t.Errorf("the row names %q / %q, want the pre-upgrade files %q / %q",
				filepath.Base(r.OutputFile), filepath.Base(r.ChatFile), filepath.Base(video), filepath.Base(oldChat))
		}
		if job.Title != "Stream" {
			t.Errorf("title %q, want \"Stream\" (only the files keep the old name)", job.Title)
		}
		assertUntouched(t, video, "ORIGINAL-RECORDING")
		assertUntouched(t, oldChat, string(chat))
		assertNoLeftovers(t, f, filepath.Base(video), filepath.Base(oldChat))
		if want := []string{filepath.Base(video), filepath.Base(oldChat)}; !slices.Equal(r.Import.Readopted, want) {
			t.Errorf("readopted %q, want %q", r.Import.Readopted, want)
		}
	})

	t.Run("different", func(t *testing.T) {
		f := newImportFixture(t)
		other := seed(t, f, oldStem+".mp4", []byte("ANOTHER-RECORDING!"))
		rec, _ := importZip(t, f, orderedImportZip(t, entries...))
		if rec.Code != http.StatusCreated {
			t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
		}
		r := decodeImportResult(t, rec.Body.Bytes())
		if filepath.Base(r.OutputFile) != newStem+".mp4" {
			t.Errorf("output %q, want the new name %q", filepath.Base(r.OutputFile), newStem+".mp4")
		}
		assertUntouched(t, other, "ANOTHER-RECORDING!")
		assertNoLeftovers(t, f, filepath.Base(other), newStem+".mp4", newStem+".chat.json")
		if len(r.Import.Readopted) != 0 || len(r.Import.Renamed) != 0 {
			t.Errorf("outcome %+v, want neither re-adopted nor renamed", r.Import)
		}
	})
}

// importBackslashTitle reads the title a Linux host gave an archive before a
// backslash was a separator: the name as written, split on "/" alone, less
// its last bracketed id — and only where that differs from this import's
// reading and names the same id.
//
// Mutants: dropping the id guard (the folder's own id case answers
// "Other \Stream", a title for another row's name); reading the id and title
// from the name with its backslashes rewritten (every backslash case answers
// the import's own title); preferring the name's title over a header's or
// the chat's.
func TestImportBackslashTitle(t *testing.T) {
	rec := func(stem string, names ...string) *importRecording {
		r := &importRecording{stem: stem}
		for _, n := range names {
			r.parts = append(r.parts, importPart{video: &zip.File{FileHeader: zip.FileHeader{Name: n}}})
		}
		return r
	}
	for _, tc := range []struct {
		desc            string
		rec             *importRecording
		videoID, prefer string
		title, want     string
	}{
		{"a folder written with a backslash",
			rec("Folder/Stream [abcd1234567]", `Folder\Stream [abcd1234567].mp4`), "abcd1234567", "", "Stream", `Folder\Stream`},
		{"no backslash: the import's own title",
			rec("Folder/Stream [abcd1234567]", "Folder/Stream [abcd1234567].mp4"), "abcd1234567", "", "Stream", "Stream"},
		{"a slash folder outside it: split on \"/\" alone",
			rec("Outer/Folder/Stream [abcd1234567]", `Outer/Folder\Stream [abcd1234567].mp4`), "abcd1234567", "", "Stream", `Folder\Stream`},
		{"a split recording's parts",
			rec("Folder/Stream [abcd1234567]", `Folder\Stream [abcd1234567] - part1.mp4`, `Folder\Stream [abcd1234567] - part2.mp4`), "abcd1234567", "", "Stream", `Folder\Stream`},
		{"no id in the name: the chat's id names both readings",
			rec("Folder/Stream", `Folder\Stream.mp4`), "abcd1234567", "", "Stream", `Folder\Stream`},
		{"the folder's own id: another row's name",
			rec("Other [efgh1234567]/Stream", `Other [efgh1234567]\Stream.mp4`), "abcd1234567", "", "Stream", "Stream"},
		{"a header's or chat's title wins",
			rec("Folder/Stream [abcd1234567]", `Folder\Stream [abcd1234567].mp4`), "abcd1234567", "Chat Title", "Chat Title", "Chat Title"},
	} {
		if got := importBackslashTitle(tc.rec, tc.videoID, tc.prefer, tc.title); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.desc, got, tc.want)
		}
	}
}
