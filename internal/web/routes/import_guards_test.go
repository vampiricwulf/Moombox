package routes

import (
	"archive/zip"
	"bytes"
	"fmt"
	"hash/crc32"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The zip-bomb guards and extractZipEntry's partial-file cleanup had no test:
// disabling every one of them at once still passed the whole package. These
// pin each guard through the route, with an archive that only that guard
// stops — the archive is otherwise importable, so a guard taken out turns the
// refusal into a 201.

const guardVideoName = "v [dQw4w9WgXcQ].mp4"

// rawStoredEntry adds a stored entry holding data whose header declares
// declared bytes uncompressed and the given CRC — CreateRaw writes the header
// as given, which is how an archive lies about its sizes.
func rawStoredEntry(t *testing.T, zw *zip.Writer, name string, data []byte, declared uint64, crc uint32) {
	t.Helper()
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name:               name,
		Method:             zip.Store,
		CRC32:              crc,
		CompressedSize64:   uint64(len(data)),
		UncompressedSize64: declared,
	})
	if err != nil {
		t.Fatalf("CreateRaw %s: %v", name, err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// storedEntry adds an honest stored entry.
func storedEntry(t *testing.T, zw *zip.Writer, name string, data []byte) {
	t.Helper()
	rawStoredEntry(t, zw, name, data, uint64(len(data)), crc32.ChecksumIEEE(data))
}

func closeZip(t *testing.T, zw *zip.Writer, buf *bytes.Buffer) []byte {
	t.Helper()
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	return buf.Bytes()
}

// assertNothingImported fails when a refused import left a row or a file.
func assertNothingImported(t *testing.T, f *importFixture, what string) {
	t.Helper()
	if f.db.JobExists("dQw4w9WgXcQ") {
		t.Errorf("%s: a job was created", what)
	}
	entries, _ := os.ReadDir(filepath.Join(f.outputDir, "imports"))
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s: left %v in imports/", what, names)
	}
}

// More than 1000 entries is refused before anything is read; 1000 is not.
//
// Mutant: `false &&` on the len(zipReader.File) > maxFiles check (the 1001
// archive imports), or `>=` for `>` (the 1000 archive is refused).
func TestImportRefusesMoreEntriesThanTheCap(t *testing.T) {
	withEntries := func(n int) []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		storedEntry(t, zw, guardVideoName, []byte("v"))
		for i := 1; i < n; i++ {
			storedEntry(t, zw, fmt.Sprintf("pad/%04d.txt", i), nil)
		}
		return closeZip(t, zw, &buf)
	}

	f := newImportFixture(t)
	rec, _ := importZip(t, f, withEntries(1001))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "too many files") {
		t.Errorf("1001 entries: %d %s, want 400 too many files", rec.Code, rec.Body.String())
	}
	assertNothingImported(t, f, "1001 entries")

	if rec, _ := importZip(t, newImportFixture(t), withEntries(1000)); rec.Code != http.StatusCreated {
		t.Errorf("1000 entries: %d %s, want 201", rec.Code, rec.Body.String())
	}
}

// Declared uncompressed sizes summing past 2 GB are refused, even when no one
// entry declares that much and nothing that large is ever extracted. The
// archive carries 21 MiB of real bytes so its ratio (97) stays under the
// ratio guard's 100 — this guard alone stands between it and a 201. Exactly
// 2 GB is still taken.
//
// Mutant: `false &&` on the totalUncompressed > maxUncompressed check, or the
// check testing each entry's own size instead of the running total (both: the
// 2 GB + 1 archive imports); `>=` for `>` (the 2 GB archive is refused).
func TestImportRefusesDeclaredSizesOverTheCap(t *testing.T) {
	const filler = 21 << 20
	video := []byte("v")
	declaring := func(total uint64) []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		storedEntry(t, zw, guardVideoName, video)
		zeros := make([]byte, filler)
		rawStoredEntry(t, zw, "pad1.bin", zeros, 1<<30, crc32.ChecksumIEEE(zeros))
		rawStoredEntry(t, zw, "pad2.bin", nil, total-1<<30-uint64(len(video)), 0)
		return closeZip(t, zw, &buf)
	}

	f := newImportFixture(t)
	over := declaring(1<<31 + 1)
	if ratio := (uint64(1<<31) + 1) / uint64(len(over)); ratio > 100 {
		t.Fatalf("precondition: the archive's ratio is %d, which the ratio guard would stop", ratio)
	}
	rec, _ := importZip(t, f, over)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "uncompressed") {
		t.Errorf("2 GB + 1 declared: %d %s, want 400 too large (uncompressed)", rec.Code, rec.Body.String())
	}
	assertNothingImported(t, f, "2 GB + 1 declared")

	if rec, _ := importZip(t, newImportFixture(t), declaring(1<<31)); rec.Code != http.StatusCreated {
		t.Errorf("exactly 2 GB declared: %d %s, want 201", rec.Code, rec.Body.String())
	}
}

// An entry that inflates more than 100-fold over the upload is refused — a
// real deflated run of zeros, not a forged header. The threshold itself is
// pinned with declared sizes: a ratio of exactly 100 is taken, 101 is not.
//
// Mutant: `false &&` on the ratio check (the zeros import), `>=` for `>` or a
// lower maxCompressionRatio (the ratio-100 archive is refused), a higher one
// (the ratio-101 archive imports).
func TestImportRefusesASuspiciousCompressionRatio(t *testing.T) {
	f := newImportFixture(t)
	rec, _ := importZip(t, f, makeImportZip(t, map[string][]byte{guardVideoName: make([]byte, 4<<20)}))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "compression ratio") {
		t.Errorf("4 MiB of deflated zeros: %d %s, want 400 suspicious compression ratio", rec.Code, rec.Body.String())
	}
	assertNothingImported(t, f, "4 MiB of deflated zeros")

	// The archive's length does not depend on what pad.bin declares (a
	// 32-bit header field either way), so it is measured once.
	video := []byte("v")
	declaring := func(total uint64) []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		storedEntry(t, zw, guardVideoName, video)
		pad := make([]byte, 512)
		rawStoredEntry(t, zw, "pad.bin", pad, total-uint64(len(video)), crc32.ChecksumIEEE(pad))
		return closeZip(t, zw, &buf)
	}
	size := uint64(len(declaring(1024)))
	for _, tc := range []struct {
		ratio uint64
		want  int
	}{{100, http.StatusCreated}, {101, http.StatusBadRequest}} {
		body := declaring(tc.ratio * size)
		if uint64(len(body)) != size {
			t.Fatalf("precondition: the archive is %d bytes, measured %d", len(body), size)
		}
		if rec, _ := importZip(t, newImportFixture(t), body); rec.Code != tc.want {
			t.Errorf("ratio %d: %d %s, want %d", tc.ratio, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

// An entry whose bytes fail its CRC is an error, and the file extracted up to
// the failure is removed: no row will ever name it, and it would otherwise sit
// truncated in imports/ for the Files tab to find as an orphan.
//
// Mutant: dropping extractZipEntry's os.Remove(destPath) — the truncated
// video stays behind.
func TestImportBadChecksumLeavesNothingBehind(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	data := []byte("video bytes that do not match their checksum")
	rawStoredEntry(t, zw, guardVideoName, data, uint64(len(data)), ^crc32.ChecksumIEEE(data))

	f := newImportFixture(t)
	rec, _ := importZip(t, f, closeZip(t, zw, &buf))
	if rec.Code < http.StatusBadRequest {
		t.Errorf("a bad-CRC video: %d %s, want an error", rec.Code, rec.Body.String())
	}
	assertNothingImported(t, f, "a bad-CRC video")
}
