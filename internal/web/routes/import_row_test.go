package routes

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

func importZip(t *testing.T, f *importFixture, zipBytes []byte) (*httptest.ResponseRecorder, database.Job) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, importRequest(zipBytes))
	var job database.Job
	if rec.Code == http.StatusCreated {
		if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
			t.Fatalf("decode the created job: %v", err)
		}
	}
	return rec, job
}

// An imported row is a recording's row: the absolute output and chat paths,
// the output directory its relative names resolve against, and the size —
// which Stats' recorded total and the details dialog's size line both read,
// and which an import used to leave unset. The filename-derived title loses
// the bracket id the output name appends anyway (it was "video [id]", stored
// as "video [id] [id].mp4").
//
// Mutant: dropping any of the four fields, or keeping the [id] in nameTitle.
func TestImportedRowCarriesARecordingsFields(t *testing.T) {
	f := newImportFixture(t)
	video := []byte("fake-video-bytes")
	rec, job := importZip(t, f, makeImportZip(t, map[string][]byte{
		"Some stream [dQw4w9WgXcQ].mp4":       video,
		"Some stream [dQw4w9WgXcQ].chat.json": []byte(`{"messages":[]}`),
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	if job.Title != "Some stream" {
		t.Errorf("title %q, want the name without its bracket id", job.Title)
	}
	if strings.Count(job.Filename, "dQw4w9WgXcQ") != 1 {
		t.Errorf("filename %q carries the id more than once", job.Filename)
	}
	if job.FileSize == nil || *job.FileSize != int64(len(video)) {
		t.Errorf("fileSize %v, want %d", job.FileSize, len(video))
	}
	want := filepath.Join(f.outputDir, job.Filename)
	if !filepath.IsAbs(job.OutputFile) || job.OutputFile != want {
		t.Errorf("outputFile %q, want the absolute %q", job.OutputFile, want)
	}
	if wantChat := filepath.Join(f.outputDir, job.ChatFilename); job.ChatFilename == "" || job.ChatFile != wantChat {
		t.Errorf("chatFile %q, want the absolute %q", job.ChatFile, wantChat)
	}
	if job.OutputDirectory != f.outputDir {
		t.Errorf("outputDirectory %q, want %q", job.OutputDirectory, f.outputDir)
	}
}

// The entry-name traversal rule is a ".." path COMPONENT, not the substring:
// Moombox's own naming writes "Wait... what_ [id].mp4", which the substring
// test refused to re-import as an "invalid zip entry path". A real ".."
// component is still refused.
//
// Mutant: restoring strings.Contains(name, "..").
func TestImportAcceptsDotsInsideAName(t *testing.T) {
	f := newImportFixture(t)
	rec, job := importZip(t, f, makeImportZip(t, map[string][]byte{"Wait... what_ [dQw4w9WgXcQ].mp4": []byte("v")}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("a name with \"...\" in it: %d (body %s)", rec.Code, rec.Body.String())
	}
	if job.Title != "Wait... what_" {
		t.Errorf("title %q", job.Title)
	}
	for _, name := range []string{"../escape.mp4", "a/../../escape.mp4", "/abs.mp4"} {
		rec, _ := importZip(t, newImportFixture(t), makeImportZip(t, map[string][]byte{name: []byte("v")}))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: %d, want 400", name, rec.Code)
		}
	}
}

// An archive whose entry names are CP437 (the zip format's encoding when the
// UTF-8 flag is clear) had its raw bytes stored as an invalid-UTF-8 title and
// filename. A non-UTF-8 name is decoded as CP437 now.
//
// Mutant: zipEntryName returning f.Name unconditionally.
func TestImportDecodesACP437EntryName(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "caf\x82 [dQw4w9WgXcQ].mp4", NonUTF8: true, Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("v"))
	zw.Close()

	rec, job := importZip(t, newImportFixture(t), buf.Bytes())
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	if job.Title != "café" {
		t.Errorf("title %q, want the CP437 name decoded to \"café\"", job.Title)
	}
}

// An insert that fails takes back what it extracted: no row will ever name
// those files, and they surfaced later as orphans in the Files tab.
//
// Mutant: dropping the os.Remove calls on the AddJob error path.
func TestImportFailedInsertLeavesNoFiles(t *testing.T) {
	f := newImportFixture(t)
	f.db.Close() // every write now fails
	rec, _ := importZip(t, f, makeImportZip(t, map[string][]byte{
		"video [dQw4w9WgXcQ].mp4":       []byte("v"),
		"video [dQw4w9WgXcQ].chat.json": []byte(`{"messages":[]}`),
	}))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("import against a closed DB: %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
	entries, _ := os.ReadDir(filepath.Join(f.outputDir, "imports"))
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("a failed insert left %v in imports/", names)
	}
}

// A body past the cap is "too large", not an "invalid zip file": the spool
// used to stop exactly at the cap and hand the truncated archive to the zip
// reader. copyWithLimit reads one byte past it to tell, and either its own
// verdict or MaxBytesReader's is a 413.
//
// Mutant: copyWithLimit reading only `limit` bytes, or importUploadTooLarge
// missing either error.
func TestImportUploadPastTheCapIsTooLarge(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "spool")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	if _, err := copyWithLimit(tmp, strings.NewReader("abcdef"), 6); err != nil {
		t.Errorf("a body exactly at the cap: %v", err)
	}
	if _, err := copyWithLimit(tmp, strings.NewReader("abcdefg"), 6); !importUploadTooLarge(err) {
		t.Errorf("a body one byte past the cap: %v, want too large", err)
	}
	if !importUploadTooLarge(fmt.Errorf("read: %w", &http.MaxBytesError{Limit: 1})) {
		t.Error("MaxBytesReader's error is not read as too large")
	}
	if importUploadTooLarge(errors.New("connection reset")) {
		t.Error("an ordinary read error is read as too large")
	}
}
