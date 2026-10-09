package routes

import (
	"net/http"
	"testing"
)

// W25-08: the id a file name carries is its LAST bracketed id — Moombox and
// yt-dlp both append it — so a title's own 11-character tag no longer takes
// its place. "[Holo-Live3D] Anniversary Concert [dQw4w9WgXcQ].mp4" imported
// as id "Holo-Live3D": a watch URL for the wrong video, the real id left in
// the title and the file, and no dedupe against the real id.
//
// Mutants: importNameID taking the first match (all[0]) — the tag becomes
// the id; returning the stem unchanged as rest — the id stays in the title.
func TestImportTakesTheLastBracketedID(t *testing.T) {
	f := newImportFixture(t)
	rec, job := importZip(t, f, makeImportZip(t, map[string][]byte{
		"[Holo-Live3D] Anniversary Concert [dQw4w9WgXcQ].mp4": []byte("v"),
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	if job.ID != "dQw4w9WgXcQ" || job.URL != "https://www.youtube.com/watch?v=dQw4w9WgXcQ" {
		t.Errorf("id %q url %q: the title's own tag was taken for the video id", job.ID, job.URL)
	}
	if job.Title != "[Holo-Live3D] Anniversary Concert" {
		t.Errorf("title %q, want the name less its trailing id", job.Title)
	}
	if want := "imports/[Holo-Live3D] Anniversary Concert [dQw4w9WgXcQ].mp4"; job.Filename != want {
		t.Errorf("filename %q, want %q", job.Filename, want)
	}
}

// An archive an earlier import wrote is named "<title> [imp_<8 hex>]": its id
// is the placeholder that import minted, and re-importing it keeps that id
// (and so its dedupe) rather than minting a second one beside it — "Stream
// [imp_0a1b2c3d] [imp_9f8e7d6c].mp4".
//
// Mutant: dropping the imp_ alternative from importNameIDRe.
func TestImportKeepsAnImportedArchivesPlaceholderID(t *testing.T) {
	f := newImportFixture(t)
	rec, job := importZip(t, f, makeImportZip(t, map[string][]byte{"Stream [imp_0a1b2c3d].mp4": []byte("v")}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	if job.ID != "imp_0a1b2c3d" || job.Title != "Stream" {
		t.Errorf("id %q title %q, want imp_0a1b2c3d / Stream", job.ID, job.Title)
	}
}

func TestImportNameID(t *testing.T) {
	for _, tc := range []struct{ stem, id, rest string }{
		{"a [AAAAAAAAAAA] b [BBBBBBBBBBB]", "BBBBBBBBBBB", "a [AAAAAAAAAAA] b"},
		{"Stream [dQw4w9WgXcQ] - part1", "dQw4w9WgXcQ", "Stream - part1"},
		{"[dQw4w9WgXcQ]", "dQw4w9WgXcQ", ""},
		{"no id [short]", "", "no id [short]"},
		{"x [imp_0A1B2C3D]", "", "x [imp_0A1B2C3D]"}, // a placeholder is lowercase hex
	} {
		id, rest := importNameID(tc.stem)
		if id != tc.id || rest != tc.rest {
			t.Errorf("importNameID(%q) = %q, %q; want %q, %q", tc.stem, id, rest, tc.id, tc.rest)
		}
	}
}
