package routes

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// W25-01. A row's Delete leaves its files in imports/, so re-importing the
// same archive writes exactly the names already there. The import extracted
// straight onto them: os.Create truncated the only good copy, a damaged entry
// or a failed insert then removed it outright, and a different file under the
// same name replaced it without a word. Every entry is now extracted to a
// temporary name in imports/ and placed only once the row exists; a name that
// holds the same bytes is re-adopted, and one that holds anything else keeps
// it while the import takes " (2)".

// importResult is a 201 body: the row and the import's outcome.
type importResult struct {
	database.Job
	Import importOutcome `json:"import"`
}

func decodeImportResult(t *testing.T, body []byte) importResult {
	t.Helper()
	var r importResult
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode the 201 body: %v", err)
	}
	return r
}

// seedOrphanedImport imports the archive and deletes its row, the way the
// dashboard's Delete leaves an import: the files stay in imports/. Their
// modification time is put an hour back, so a test can tell they were not
// written again.
func seedOrphanedImport(t *testing.T, f *importFixture, entries ...importEntry) (video, chat string) {
	t.Helper()
	rec, job := importZip(t, f, orderedImportZip(t, entries...))
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed import: %d (body %s)", rec.Code, rec.Body.String())
	}
	if err := f.db.DeleteJob(job.ID); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for _, p := range []string{job.OutputFile, job.ChatFile} {
		if p != "" {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	return job.OutputFile, job.ChatFile
}

// assertUntouched fails unless path still holds want with its seeded mtime.
func assertUntouched(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s is GONE: %v", filepath.Base(path), err)
	}
	if string(got) != want {
		t.Errorf("%s was written over: %q, want %q", filepath.Base(path), got, want)
	}
	if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) < 30*time.Minute {
		t.Errorf("%s was written again (mtime %s)", filepath.Base(path), info.ModTime())
	}
}

// assertNoLeftovers fails when imports/ holds anything but names.
func assertNoLeftovers(t *testing.T, f *importFixture, names ...string) {
	t.Helper()
	got := importsDirNames(t, f)
	slices.Sort(got)
	slices.Sort(names)
	if !slices.Equal(got, names) {
		t.Errorf("imports/ holds %q, want %q", got, names)
	}
}

const reimportChat = `{"messages":[{"offsetMs":0,"message":"x"}]}`

// A re-import that fails — a damaged entry, or an insert the database
// refuses — costs nothing that was in imports/ before it, and leaves no
// temporary file behind.
//
// Mutants: the deferred cleanup also removing each file's destination (the
// old cleanup's rule — the failed insert takes the original with it);
// dropping the deferred removal of the temporary files (they are left in
// imports/).
func TestImportReimportFailureLeavesTheExistingArchive(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []importEntry
		breakDB bool
	}{
		{"damaged video", []importEntry{
			{name: "Stream [dQw4w9WgXcQ].mp4", data: []byte("DAMAGED-COPY-BYTES"), badCRC: true},
		}, false},
		{"damaged chat", []importEntry{
			{name: "Stream [dQw4w9WgXcQ].mp4", data: []byte("ORIGINAL-RECORDING")},
			{name: "Stream [dQw4w9WgXcQ].chat.json", data: []byte(reimportChat), badCRC: true},
		}, false},
		{"failed insert", []importEntry{
			{name: "Stream [dQw4w9WgXcQ].mp4", data: []byte("ORIGINAL-RECORDING")},
			{name: "Stream [dQw4w9WgXcQ].chat.json", data: []byte(reimportChat)},
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newImportFixture(t)
			video, chat := seedOrphanedImport(t, f,
				importEntry{name: "Stream [dQw4w9WgXcQ].mp4", data: []byte("ORIGINAL-RECORDING")},
				importEntry{name: "Stream [dQw4w9WgXcQ].chat.json", data: []byte(reimportChat)},
			)
			if tc.breakDB {
				f.db.Close()
			}
			rec, _ := importZip(t, f, orderedImportZip(t, tc.entries...))
			if rec.Code == http.StatusCreated {
				t.Fatalf("the re-import succeeded (body %s)", rec.Body.String())
			}
			assertUntouched(t, video, "ORIGINAL-RECORDING")
			assertUntouched(t, chat, reimportChat)
			assertNoLeftovers(t, f, filepath.Base(video), filepath.Base(chat))
		})
	}
}

// Re-importing the archive a deleted row left behind re-adopts its files: the
// row names them, nothing is copied a second time, and they are not written
// again. The response says so.
//
// Mutants: claim treating an identical file as taken (the import lands on
// " (2)" beside a duplicate); place ignoring adopted (the link onto the
// existing name fails and the import answers 500).
func TestImportReadoptsTheIdenticalArchiveLeftInImports(t *testing.T) {
	f := newImportFixture(t)
	entries := []importEntry{
		{name: "Stream [dQw4w9WgXcQ].mp4", data: []byte("ORIGINAL-RECORDING")},
		{name: "Stream [dQw4w9WgXcQ].chat.json", data: []byte(reimportChat)},
	}
	video, chat := seedOrphanedImport(t, f, entries...)

	rec, _ := importZip(t, f, orderedImportZip(t, entries...))
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-import: %d (body %s)", rec.Code, rec.Body.String())
	}
	r := decodeImportResult(t, rec.Body.Bytes())
	if r.OutputFile != video || r.ChatFile != chat {
		t.Errorf("the row names %q / %q, want the files already there: %q / %q", r.OutputFile, r.ChatFile, video, chat)
	}
	assertUntouched(t, video, "ORIGINAL-RECORDING")
	assertUntouched(t, chat, reimportChat)
	assertNoLeftovers(t, f, filepath.Base(video), filepath.Base(chat))
	if want := []string{filepath.Base(video), filepath.Base(chat)}; !slices.Equal(r.Import.Readopted, want) {
		t.Errorf("readopted %q, want %q", r.Import.Readopted, want)
	}
	if !strings.Contains(r.Import.Note, "identical") || len(r.Import.Renamed) != 0 {
		t.Errorf("outcome %+v", r.Import)
	}
}

// A different file under the name the import would take keeps it; the
// import takes " (2)", and says so. The new bytes are the SAME SIZE as the
// old: only the content tells them apart.
//
// Mutants: claim comparing sizes only (the different file is re-adopted and
// the new bytes are lost); claim treating an existing name as free (the link
// onto it fails: 500).
func TestImportTakesASuffixBesideADifferentFile(t *testing.T) {
	f := newImportFixture(t)
	video, chat := seedOrphanedImport(t, f,
		importEntry{name: "Stream [dQw4w9WgXcQ].mp4", data: []byte("FULL-RECORDING")},
		importEntry{name: "Stream [dQw4w9WgXcQ].chat.json", data: []byte(reimportChat)},
	)

	rec, _ := importZip(t, f, orderedImportZip(t,
		importEntry{name: "Stream [dQw4w9WgXcQ].mp4", data: []byte("CLIP-RECORDING")},
		importEntry{name: "Stream [dQw4w9WgXcQ].chat.json", data: []byte(reimportChat)},
	))
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	r := decodeImportResult(t, rec.Body.Bytes())
	if want := filepath.FromSlash("imports/Stream [dQw4w9WgXcQ] (2).mp4"); r.Filename != want {
		t.Errorf("filename %q, want %q", r.Filename, want)
	}
	if got, _ := os.ReadFile(r.OutputFile); string(got) != "CLIP-RECORDING" {
		t.Errorf("the imported file holds %q", got)
	}
	assertUntouched(t, video, "FULL-RECORDING")
	// The chat is the same bytes: re-adopted beside the renamed video rather
	// than copied again.
	if r.ChatFile != chat {
		t.Errorf("chat %q, want the identical one already there, %q", r.ChatFile, chat)
	}
	assertNoLeftovers(t, f, filepath.Base(video), filepath.Base(chat), "Stream [dQw4w9WgXcQ] (2).mp4")
	want := []importRename{{From: "Stream [dQw4w9WgXcQ].mp4", To: "Stream [dQw4w9WgXcQ] (2).mp4"}}
	if !slices.Equal(r.Import.Renamed, want) || !strings.Contains(r.Import.Note, "(2)") {
		t.Errorf("outcome %+v, want renamed %+v", r.Import, want)
	}
}

// A name that is taken between the check and the move — here by a file the
// row's own insert notification writes — is not replaced: the move fails, the
// row goes, and so does every file this request had already placed; the file
// that took the name stays.
//
// Mutants: placeNoReplace renaming over the destination (the intruder is
// replaced); dropping the DeleteJob call (a row names files that are not
// its); dropping the removal of the placed files (the video is left behind).
func TestImportPlacementNeverReplacesAFileThatAppeared(t *testing.T) {
	f := newImportFixture(t)
	intruder := filepath.Join(f.outputDir, "imports", "Stream [dQw4w9WgXcQ].chat.json")
	f.db.OnJobAdded(func(*database.JobAdded) {
		os.WriteFile(intruder, []byte("INTRUDER"), 0o644)
	})
	rec, _ := importZip(t, f, orderedImportZip(t,
		importEntry{name: "Stream [dQw4w9WgXcQ].mp4", data: []byte("VIDEO")},
		importEntry{name: "Stream [dQw4w9WgXcQ].chat.json", data: []byte(reimportChat)},
	))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("import: %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
	if got, _ := os.ReadFile(intruder); string(got) != "INTRUDER" {
		t.Errorf("the file that took the name was replaced: %q", got)
	}
	if f.db.JobExists("dQw4w9WgXcQ") {
		t.Error("the row survived a failed placement")
	}
	assertNoLeftovers(t, f, filepath.Base(intruder))
}

// placeNoReplace refuses an existing destination outright.
//
// Mutant: os.Rename(tmp, dest) — the destination is replaced.
func TestPlaceNoReplaceRefusesAnExistingName(t *testing.T) {
	dir := t.TempDir()
	tmp, dest := filepath.Join(dir, "tmp"), filepath.Join(dir, "dest")
	os.WriteFile(tmp, []byte("NEW"), 0o644)
	os.WriteFile(dest, []byte("OLD"), 0o644)
	if err := placeNoReplace(tmp, dest); err == nil {
		t.Error("placed over an existing name")
	}
	if got, _ := os.ReadFile(dest); string(got) != "OLD" {
		t.Errorf("dest holds %q", got)
	}
	os.Remove(dest)
	if err := placeNoReplace(tmp, dest); err != nil {
		t.Fatalf("a free name: %v", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "NEW" {
		t.Errorf("dest holds %q", got)
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Error("the temporary file is still there")
	}
}

// An extraction an aborted import left in imports/ is swept once it is a day
// old — and only that shape: an archive whose title happens to start with the
// temporary prefix is never touched, whatever its age.
//
// Mutants: sweeping by prefix alone (the archive goes); not sweeping imports/
// (the leftover stays).
func TestCleanupOldImportTempSweepsAnAbortedExtraction(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	out := t.TempDir()
	imports := filepath.Join(out, "imports")
	os.MkdirAll(imports, 0o755)
	leftover := filepath.Join(imports, importTempPrefix+"123456789"+importPartialExt)
	fresh := filepath.Join(imports, importTempPrefix+"987654321"+importPartialExt)
	archive := filepath.Join(imports, importTempPrefix+"notes [dQw4w9WgXcQ].mp4")
	for _, p := range []string{leftover, fresh, archive} {
		os.WriteFile(p, []byte("x"), 0o644)
	}
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(leftover, old, old)
	os.Chtimes(archive, old, old)

	if removed, err := CleanupOldImportTemp(out); err != nil || removed != 1 {
		t.Errorf("CleanupOldImportTemp = %d, %v; want 1, nil", removed, err)
	}
	if _, err := os.Stat(leftover); err == nil {
		t.Error("the day-old extraction survived")
	}
	for _, p := range []string{fresh, archive} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was swept: %v", filepath.Base(p), err)
		}
	}
}

// An archive imported before titles were cut by bytes is named
// SanitizeForFilename(title)+" [<id>]": a 70-character Japanese title — 210
// bytes — fits a file name whole, where the import now cuts it to 179.
// Re-importing that archive after its row was deleted never looked under
// the old name: the cut one was free, a second full copy of the video and
// chat went there, the old pair was left an orphan, and the answer was a
// plain 201. The old name is re-adopted when it holds the same bytes; a
// different file there is left alone, and the import takes the cut name as
// any import would.
//
// Mutants: not trying the legacy stem (a second copy beside the old one);
// adopting a legacy name without the byte check (the different file is
// taken for the import's); trying it for the videos only (the chat is
// copied a second time).
func TestImportReadoptsAPreUpgradeLongTitle(t *testing.T) {
	title := strings.Repeat("配", 70)
	legacyStem := utils.SanitizeForFilename(title) + " [dQw4w9WgXcQ]"
	cutStem := importStem(title, "dQw4w9WgXcQ")
	if legacyStem == cutStem {
		t.Fatalf("the title %d bytes long is not cut: %q", len(title), cutStem)
	}
	chat := chatJSONFor(t, map[string]any{"videoId": "dQw4w9WgXcQ", "videoTitle": title, "channelName": "Ch"})
	entries := []importEntry{
		{name: "Stream [dQw4w9WgXcQ].mp4", data: []byte("ORIGINAL-RECORDING")},
		{name: "Stream [dQw4w9WgXcQ].chat.json", data: chat},
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
		os.Chtimes(p, old, old)
		return p
	}

	t.Run("identical", func(t *testing.T) {
		f := newImportFixture(t)
		video := seed(t, f, legacyStem+".mp4", []byte("ORIGINAL-RECORDING"))
		oldChat := seed(t, f, legacyStem+".chat.json", chat)
		rec, _ := importZip(t, f, orderedImportZip(t, entries...))
		if rec.Code != http.StatusCreated {
			t.Fatalf("re-import: %d (body %s)", rec.Code, rec.Body.String())
		}
		r := decodeImportResult(t, rec.Body.Bytes())
		if r.OutputFile != video || r.ChatFile != oldChat {
			t.Errorf("the row names %q / %q, want the pre-upgrade files %q / %q",
				filepath.Base(r.OutputFile), filepath.Base(r.ChatFile), filepath.Base(video), filepath.Base(oldChat))
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
		other := seed(t, f, legacyStem+".mp4", []byte("ANOTHER-RECORDING!"))
		rec, _ := importZip(t, f, orderedImportZip(t, entries...))
		if rec.Code != http.StatusCreated {
			t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
		}
		r := decodeImportResult(t, rec.Body.Bytes())
		if filepath.Base(r.OutputFile) != cutStem+".mp4" {
			t.Errorf("output %q, want the cut name %q", filepath.Base(r.OutputFile), cutStem+".mp4")
		}
		assertUntouched(t, other, "ANOTHER-RECORDING!")
		assertNoLeftovers(t, f, filepath.Base(other), cutStem+".mp4", cutStem+".chat.json")
		if len(r.Import.Readopted) != 0 || len(r.Import.Renamed) != 0 {
			t.Errorf("outcome %+v, want neither re-adopted nor renamed", r.Import)
		}
	})
}
