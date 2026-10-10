package routes

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// importEntry is one zip entry for orderedImportZip; badCRC stores it with a
// checksum that does not match its bytes, so the central directory opens but
// reading the entry to its end fails (zip.ErrChecksum).
type importEntry struct {
	name   string
	data   []byte
	badCRC bool
}

// orderedImportZip writes the entries in the order given (makeImportZip
// ranges a map).
func orderedImportZip(t *testing.T, entries ...importEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		var (
			w   io.Writer
			err error
		)
		if e.badCRC {
			w, err = zw.CreateRaw(&zip.FileHeader{
				Name:               e.name,
				Method:             zip.Store,
				CRC32:              crc32.ChecksumIEEE(e.data) ^ 0xdeadbeef,
				CompressedSize64:   uint64(len(e.data)),
				UncompressedSize64: uint64(len(e.data)),
			})
		} else {
			w, err = zw.Create(e.name)
		}
		if err != nil {
			t.Fatalf("zip entry %s: %v", e.name, err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatalf("zip write %s: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// importsDirNames lists what is in the fixture's imports/ directory.
func importsDirNames(t *testing.T, f *importFixture) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.outputDir, "imports"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read imports/: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// chatJSONFor is a chat archive's header with one message.
func chatJSONFor(t *testing.T, header map[string]any) []byte {
	t.Helper()
	header["messages"] = []map[string]any{{"offsetMs": 0, "message": "x"}}
	b, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

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
	if want := filepath.FromSlash("imports/[Holo-Live3D] Anniversary Concert [dQw4w9WgXcQ].mp4"); job.Filename != want {
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

// A placeholder id names no video, so a row carrying one gets no thumbnail:
// it got i.ytimg.com/vi/imp_…/maxresdefault.jpg, which 404s — whether the
// import minted the placeholder or took an earlier import's from the name. A
// real id keeps its thumbnail.
//
// Mutants: the placeholder check dropped (the i.ytimg URL is back on both
// placeholder rows); every YouTube import's thumbnail dropped (the real id
// loses its own).
func TestImportPlaceholderRowHasNoThumbnail(t *testing.T) {
	for _, tc := range []struct {
		name, file, wantThumb string
	}{
		{"minted", "Some recording.mp4", ""},
		{"from an earlier import's name", "Stream [imp_0a1b2c3d].mp4", ""},
		{"a real id", "Stream [dQw4w9WgXcQ].mp4", "https://i.ytimg.com/vi/dQw4w9WgXcQ/maxresdefault.jpg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newImportFixture(t)
			rec, job := importZip(t, f, makeImportZip(t, map[string][]byte{tc.file: []byte("v")}))
			if rec.Code != http.StatusCreated {
				t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
			}
			if tc.wantThumb == "" && !importPlaceholderRe.MatchString(job.ID) {
				t.Fatalf("setup: id %q is not a placeholder", job.ID)
			}
			if job.ThumbnailURL != tc.wantThumb {
				t.Errorf("thumbnailUrl %q, want %q", job.ThumbnailURL, tc.wantThumb)
			}
		})
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

// W25-02: the title in an import's file names is cut by BYTES. The
// sanitizer's 200-character cap counts runes, and a CJK title is three bytes
// a rune, so a 90-character Japanese title — which a Moombox chat archive
// carries in full as videoTitle — made "<title> [<id>].mp4" longer than a
// 255-byte name: 500 "failed to extract video". At 78-79 characters the
// video fitted and its ".chat.json" did not, and the import answered 201
// without the chat. Each case keeps the row's whole title, writes the chat,
// and leaves no name past 255 bytes.
//
// Mutant: importStem without the byte cut (the sanitizer's rune cap only) —
// the 90- and 100-character cases fail to extract the video and the
// 79-character one fails on its chat.
func TestImportFitsALongCJKTitleInAFileName(t *testing.T) {
	for _, tc := range []struct {
		name   string
		title  string
		header bool
	}{
		{"90 chars from the chat", strings.Repeat("配信", 45), false},
		{"79 chars from the chat", strings.Repeat("あ", 79), false},
		{"100 chars from the title box", strings.Repeat("歌枠", 50), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newImportFixture(t)
			header := map[string]any{"videoId": "dQw4w9WgXcQ"}
			if !tc.header {
				header["videoTitle"] = tc.title
			}
			req := importRequest(orderedImportZip(t,
				importEntry{name: "x [dQw4w9WgXcQ].mp4", data: []byte("video")},
				importEntry{name: "x [dQw4w9WgXcQ].chat.json", data: chatJSONFor(t, header)},
			))
			if tc.header {
				req.Header.Set("X-Import-Title", url.PathEscape(tc.title))
			}
			rec := httptest.NewRecorder()
			f.router.ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
			}
			var job struct {
				Title        string `json:"title"`
				ChatFilename string `json:"chatFilename"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
				t.Fatal(err)
			}
			if job.Title != tc.title {
				t.Errorf("the row's title was cut too: %q", job.Title)
			}
			if job.ChatFilename == "" {
				t.Error("201 without the zip's chat")
			}
			for _, n := range importsDirNames(t, f) {
				if len(n) > 255 {
					t.Errorf("%q is %d bytes", n, len(n))
				}
				if !utf8.ValidString(n) {
					t.Errorf("%q is cut inside a rune", n)
				}
			}
		})
	}
}

// importStem's budget: the template's 180 bytes for the title, less whatever
// a long id and importNameReserve need of a 255-byte name, and a cut that
// leaves no trailing dot (Windows strips one, and the recorded name would not
// be the file's).
//
// Mutants: dropping importNameReserve or the id from the budget (the long-id
// case runs past 255); not sanitizing the cut (it ends in ".").
func TestImportStemBudget(t *testing.T) {
	long := strings.Repeat("配", 100)
	if s := importStem(long, "dQw4w9WgXcQ"); len(strings.TrimSuffix(s, " [dQw4w9WgXcQ]")) > config.TemplateTitleMaxBytes {
		t.Errorf("title part of %q is past the template's %d bytes", s, config.TemplateTitleMaxBytes)
	}
	id := "tw_manual_" + strings.Repeat("a", 25) + "_1234567890123456789"
	if s := importStem(long, id); len(s)+importNameReserve > 255 {
		t.Errorf("%q (%d bytes) leaves no room for the suffixes under 255", s, len(s))
	}
	dotted := strings.Repeat("あ", 59) + "." + strings.Repeat("あ", 30)
	if s := importStem(dotted, "dQw4w9WgXcQ"); strings.HasSuffix(strings.TrimSuffix(s, " [dQw4w9WgXcQ]"), ".") {
		t.Errorf("the cut left a trailing dot: %q", s)
	}
	if s := importStem("Short title", "dQw4w9WgXcQ"); s != "Short title [dQw4w9WgXcQ]" {
		t.Errorf("a short title was changed: %q", s)
	}
}

// A chat the zip carries but the import cannot write fails the import, and
// the error is logged: the import used to answer 201 "Archive imported
// successfully" without it, and nothing said why.
//
// Mutants: the old "non-fatal, just skip chat" arm (201, the video kept);
// dropping the logger.Error call.
func TestImportFailsWhenItsChatCannotBeExtracted(t *testing.T) {
	f := newImportFixture(t)
	rec, _ := importZip(t, f, orderedImportZip(t,
		importEntry{name: "Stream [dQw4w9WgXcQ].mp4", data: []byte("video")},
		importEntry{name: "Stream [dQw4w9WgXcQ].chat.json", data: chatJSONFor(t, map[string]any{}), badCRC: true},
	))
	if rec.Code == http.StatusCreated {
		t.Fatalf("201 for an import whose chat could not be written (body %s)", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "chat") {
		t.Errorf("the refusal does not say the chat failed: %s", rec.Body.String())
	}
	if f.db.JobExists("dQw4w9WgXcQ") {
		t.Error("a row was created")
	}
	if names := importsDirNames(t, f); len(names) != 0 {
		t.Errorf("the failed import left %v in imports/", names)
	}
	if !strings.Contains(f.log.all(), "could not extract the chat") {
		t.Errorf("the extraction error was not logged: %q", f.log.all())
	}
}
