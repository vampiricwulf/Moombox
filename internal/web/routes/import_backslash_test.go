package routes

import (
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
