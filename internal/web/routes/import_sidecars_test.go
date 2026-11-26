package routes

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// Owner ruling R4-12 (imports-4): a zip of a Moombox archive brings its
// sidecars back — the thumbnail, the description, the chapter list and each
// chat's raw sidecar — under the names the finalize writes, which are the
// names the thumbnail route (thumbnail_file), the chapter routes
// (worker.ArchiveSidecarPath) and the orphan sweep look for. Whatever else
// the zip carried is named in the import's note.

const sidecarChapters = `{"chapters":[{"start":0,"end":10,"title":"Intro"}]}`

func importFileBytes(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// Mutants: a sidecar suffix not recognised by the scan (left out, and named
// as skipped); the row's thumbnail_file, description_file or description not
// set; the chapter list placed under any name but the one
// worker.ArchiveSidecarPath derives; the raw chat placed beside the video
// rather than its chat; the second thumbnail kept over the first by the
// finalize's order; an unrelated entry not reported.
func TestImportRestoresAnArchivesSidecars(t *testing.T) {
	f := newImportFixture(t)
	rec, job := importZip(t, f, orderedImportZip(t,
		importEntry{name: "Stream [dQw4w9WgXcQ].mp4", data: []byte("VIDEO")},
		importEntry{name: "Stream [dQw4w9WgXcQ].chat.json", data: []byte(reimportChat)},
		importEntry{name: "Stream [dQw4w9WgXcQ].chat.raw.jsonl", data: []byte("{}\n")},
		importEntry{name: "Stream [dQw4w9WgXcQ].png", data: []byte("PNG")},
		importEntry{name: "Stream [dQw4w9WgXcQ].jpg", data: []byte("JPG")},
		importEntry{name: "Stream [dQw4w9WgXcQ].description", data: []byte("About the stream")},
		importEntry{name: "Stream [dQw4w9WgXcQ].chapters.json", data: []byte(sidecarChapters)},
		importEntry{name: "Stream [dQw4w9WgXcQ].info.json", data: []byte(`{}`)},
		importEntry{name: "notes.txt", data: []byte("x")},
	))
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	imports := filepath.Join(f.outputDir, "imports")
	stored, err := f.db.GetJob(job.ID)
	if err != nil || stored == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if want := filepath.Join(imports, "Stream [dQw4w9WgXcQ].jpg"); stored.ThumbnailFile != want {
		t.Errorf("thumbnail_file %q, want the .jpg %q", stored.ThumbnailFile, want)
	} else if importFileBytes(t, want) != "JPG" {
		t.Errorf("the .jpg holds the wrong bytes")
	}
	if want := filepath.Join(imports, "Stream [dQw4w9WgXcQ].description"); stored.DescriptionFile != want {
		t.Errorf("description_file %q, want %q", stored.DescriptionFile, want)
	}
	if stored.Description != "About the stream" {
		t.Errorf("description %q", stored.Description)
	}
	chapters := worker.ArchiveSidecarPath(stored.OutputFile, utils.ChaptersSidecarSuffix)
	if list, err := utils.ReadChaptersSidecar(chapters); err != nil || len(list) != 1 || list[0].Title != "Intro" {
		t.Errorf("chapter list at %s: %+v (%v)", chapters, list, err)
	}
	if got := importFileBytes(t, strings.TrimSuffix(stored.ChatFile, ".chat.json")+".chat.raw.jsonl"); got != "{}\n" {
		t.Errorf("raw chat %q", got)
	}

	r := decodeImportResult(t, rec.Body.Bytes())
	if !slices.Equal(r.Import.Skipped, []string{"Stream [dQw4w9WgXcQ].png", "Stream [dQw4w9WgXcQ].info.json", "notes.txt"}) {
		t.Errorf("skipped %q", r.Import.Skipped)
	}
	for _, want := range []string{
		"left out Stream [dQw4w9WgXcQ].png: " + importSkipThumbnail,
		"left out Stream [dQw4w9WgXcQ].info.json: " + importSkipOther,
		"left out notes.txt: " + importSkipUnrelated,
	} {
		if !strings.Contains(r.Import.Note, want) {
			t.Errorf("note %q lacks %q", r.Import.Note, want)
		}
	}
	assertNoLeftovers(t, f, "Stream [dQw4w9WgXcQ].mp4", "Stream [dQw4w9WgXcQ].chat.json", "Stream [dQw4w9WgXcQ].chat.raw.jsonl",
		"Stream [dQw4w9WgXcQ].jpg", "Stream [dQw4w9WgXcQ].description", "Stream [dQw4w9WgXcQ].chapters.json")
}

// A split recording's sidecars sit under the base its parts share, as the
// finalize writes them; a part's raw chat beside that part's chat, and one
// whose chat the zip lacks is left out and named.
//
// Mutants: the chapter list named after part 1 (ArchiveSidecarPath of the
// first segment no longer finds it); the raw chat paired by the recording's
// stem rather than its chat's; a raw chat with no chat kept.
func TestImportRestoresASplitArchivesSidecars(t *testing.T) {
	stubImportDurations(t)
	f := newImportFixture(t)
	rec, job := importZip(t, f, orderedImportZip(t,
		importEntry{name: "Stream [tw_123456789] - part1.mp4", data: []byte("ONE")},
		importEntry{name: "Stream [tw_123456789] - part2.mp4", data: []byte("TWO")},
		importEntry{name: "Stream [tw_123456789] - part2.chat.json", data: []byte(reimportChat)},
		importEntry{name: "Stream [tw_123456789] - part2.chat.raw.jsonl", data: []byte("raw2")},
		importEntry{name: "Stream [tw_123456789] - part1.chat.raw.jsonl", data: []byte("raw1")},
		importEntry{name: "Stream [tw_123456789].chapters.json", data: []byte(sidecarChapters)},
		importEntry{name: "Stream [tw_123456789].webp", data: []byte("WEBP")},
	))
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	segs, err := f.db.GetSegments(job.ID)
	if err != nil || len(segs) != 2 {
		t.Fatalf("segments %+v (%v)", segs, err)
	}
	chapters := worker.ArchiveSidecarPath(segs[0].FilePath, utils.ChaptersSidecarSuffix)
	if list, err := utils.ReadChaptersSidecar(chapters); err != nil || len(list) != 1 {
		t.Errorf("chapter list at %s: %+v (%v)", chapters, list, err)
	}
	if got := importFileBytes(t, strings.TrimSuffix(segs[1].ChatFile, ".chat.json")+".chat.raw.jsonl"); got != "raw2" {
		t.Errorf("part 2's raw chat %q", got)
	}
	stored, _ := f.db.GetJob(job.ID)
	if stored == nil || filepath.Base(stored.ThumbnailFile) != "Stream [tw_123456789].webp" {
		t.Errorf("thumbnail_file %+v", stored)
	}
	r := decodeImportResult(t, rec.Body.Bytes())
	if !slices.Equal(r.Import.Skipped, []string{"Stream [tw_123456789] - part1.chat.raw.jsonl"}) ||
		!strings.Contains(r.Import.Note, importSkipRawNoChat) {
		t.Errorf("outcome %+v, want part 1's raw chat named as left out", r.Import)
	}
	assertNoLeftovers(t, f, "Stream [tw_123456789] - part1.mp4", "Stream [tw_123456789] - part2.mp4",
		"Stream [tw_123456789] - part2.chat.json", "Stream [tw_123456789] - part2.chat.raw.jsonl",
		"Stream [tw_123456789].chapters.json", "Stream [tw_123456789].webp")
}

// A sidecar follows its video's name when that name was taken: beside
// "Stream [id] (2).mp4", never a " (n)" of its own. The same bytes already
// under the sidecar's name are re-adopted; different bytes there are left
// alone, and the sidecar is left out and named. A chapter list that does not
// parse is left out too: beside the archive it would refuse every chapter
// save. Nothing temporary stays behind.
//
// Mutants: the sidecar placed under the base name whatever its video took;
// a taken sidecar name written over or given " (2)"; the unreadable chapter
// list kept; its temporary file not removed.
func TestImportSidecarsFollowTheirVideosName(t *testing.T) {
	f := newImportFixture(t)
	imports := filepath.Join(f.outputDir, "imports")
	if err := os.MkdirAll(imports, 0o755); err != nil {
		t.Fatal(err)
	}
	// A different video holds the base name, and a different thumbnail and
	// the same description sit under the " (2)" stem.
	for name, data := range map[string]string{
		"Stream [dQw4w9WgXcQ].mp4":             "OTHER VIDEO",
		"Stream [dQw4w9WgXcQ] (2).jpg":         "SOMEONE ELSE'S",
		"Stream [dQw4w9WgXcQ] (2).description": "About",
	} {
		if err := os.WriteFile(filepath.Join(imports, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec, job := importZip(t, f, orderedImportZip(t,
		importEntry{name: "Stream [dQw4w9WgXcQ].mp4", data: []byte("VIDEO")},
		importEntry{name: "Stream [dQw4w9WgXcQ].jpg", data: []byte("JPG")},
		importEntry{name: "Stream [dQw4w9WgXcQ].description", data: []byte("About")},
		importEntry{name: "Stream [dQw4w9WgXcQ].chapters.json", data: []byte("not json")},
	))
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	stored, _ := f.db.GetJob(job.ID)
	if stored == nil || filepath.Base(stored.OutputFile) != "Stream [dQw4w9WgXcQ] (2).mp4" {
		t.Fatalf("row %+v, want the video under (2)", stored)
	}
	if stored.ThumbnailFile != "" {
		t.Errorf("thumbnail_file %q: the name held another file", stored.ThumbnailFile)
	}
	if importFileBytes(t, filepath.Join(imports, "Stream [dQw4w9WgXcQ] (2).jpg")) != "SOMEONE ELSE'S" {
		t.Error("the file under the thumbnail's name was written over")
	}
	if want := filepath.Join(imports, "Stream [dQw4w9WgXcQ] (2).description"); stored.DescriptionFile != want {
		t.Errorf("description_file %q, want the re-adopted %q", stored.DescriptionFile, want)
	}
	r := decodeImportResult(t, rec.Body.Bytes())
	if !slices.Contains(r.Import.Readopted, "Stream [dQw4w9WgXcQ] (2).description") {
		t.Errorf("readopted %q", r.Import.Readopted)
	}
	for _, want := range []string{
		"left out Stream [dQw4w9WgXcQ].chapters.json: " + importSkipChapters,
		"left out Stream [dQw4w9WgXcQ].jpg: " + importSkipNameTaken,
	} {
		if !strings.Contains(r.Import.Note, want) {
			t.Errorf("note %q lacks %q", r.Import.Note, want)
		}
	}
	assertNoLeftovers(t, f, "Stream [dQw4w9WgXcQ].mp4", "Stream [dQw4w9WgXcQ] (2).mp4",
		"Stream [dQw4w9WgXcQ] (2).jpg", "Stream [dQw4w9WgXcQ] (2).description")
}

// The note names at most importNoteNames entries for one reason; Skipped
// names them all.
//
// Mutant: the cap dropped (every name in the note).
func TestImportNoteBoundsTheEntriesItNames(t *testing.T) {
	f := newImportFixture(t)
	entries := []importEntry{{name: "Stream [dQw4w9WgXcQ].mp4", data: []byte("VIDEO")}}
	for i := range importNoteNames + 3 {
		entries = append(entries, importEntry{name: fmt.Sprintf("stray%02d.txt", i), data: []byte("x")})
	}
	rec, _ := importZip(t, f, orderedImportZip(t, entries...))
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	r := decodeImportResult(t, rec.Body.Bytes())
	if len(r.Import.Skipped) != importNoteNames+3 {
		t.Errorf("skipped %q", r.Import.Skipped)
	}
	last := fmt.Sprintf("stray%02d.txt", importNoteNames+2)
	if strings.Contains(r.Import.Note, last) || !strings.Contains(r.Import.Note, "and 3 more") {
		t.Errorf("note %q", r.Import.Note)
	}
}

// A second sidecar of a kind already kept (a zip can carry one name twice,
// or in another case) is named with that reason, never as a kind the import
// does not keep.
//
// Mutants: the second entry given no reason (the note then says the import
// keeps descriptions while leaving one out as "only ... description"); the
// second kept over the first.
func TestImportNamesASecondSidecarOfAKind(t *testing.T) {
	f := newImportFixture(t)
	rec, job := importZip(t, f, orderedImportZip(t,
		importEntry{name: "Stream [dQw4w9WgXcQ].mp4", data: []byte("VIDEO")},
		importEntry{name: "Stream [dQw4w9WgXcQ].chat.json", data: []byte(reimportChat)},
		importEntry{name: "Stream [dQw4w9WgXcQ].chat.raw.jsonl", data: []byte("{}\n")},
		importEntry{name: "Stream [dQw4w9WgXcQ].CHAT.RAW.JSONL", data: []byte("[]\n")},
		importEntry{name: "Stream [dQw4w9WgXcQ].description", data: []byte("First")},
		importEntry{name: "Stream [dQw4w9WgXcQ].DESCRIPTION", data: []byte("Second")},
	))
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	stored, err := f.db.GetJob(job.ID)
	if err != nil || stored == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if stored.Description != "First" {
		t.Errorf("description %q, want the first entry's", stored.Description)
	}
	r := decodeImportResult(t, rec.Body.Bytes())
	for _, want := range []string{
		"left out Stream [dQw4w9WgXcQ].CHAT.RAW.JSONL: a chat keeps one raw chat",
		"left out Stream [dQw4w9WgXcQ].DESCRIPTION: an archive keeps one description",
	} {
		if !strings.Contains(r.Import.Note, want) {
			t.Errorf("note %q lacks %q", r.Import.Note, want)
		}
	}
	if strings.Contains(r.Import.Note, importSkipOther) {
		t.Errorf("note %q names a second sidecar as a kind the import does not keep", r.Import.Note)
	}
}
