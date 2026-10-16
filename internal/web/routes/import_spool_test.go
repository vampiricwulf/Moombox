package routes

import (
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// importTempEntries lists the names in dir that carry the import's temporary
// prefix; a dir that does not exist holds none.
func importTempEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), importTempPrefix) {
			names = append(names, e.Name())
		}
	}
	return names
}

// TestImportSpoolsTheUploadOnTheOutputVolume: the upload, up to 500 MB, is
// spooled into <output>/imports — where its entries are extracted, on the
// volume the disk alerts watch — not the system temp directory. In Docker
// that was the container's /tmp, the writable layer on the host's
// /var/lib/docker that nothing watches, and on a tmpfs /tmp it was RAM: a
// large import failed there with ENOSPC however much room the output disk
// had, the trims' intermediates' own failure (W28-A08).
//
// The body is fed through a pipe, so the request is held mid-upload while
// the test looks: once the first half is read the spool exists. When the
// import is done it is gone again.
//
// Mutants: spool into "" (the temp directory gets it, imports/ does not);
// drop the deferred os.Remove (a spool is left in imports/).
func TestImportSpoolsTheUploadOnTheOutputVolume(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	if os.TempDir() != tmp {
		t.Skipf("os.TempDir() = %q, not the test's %q", os.TempDir(), tmp)
	}
	f := newImportFixture(t)
	imports := filepath.Join(f.outputDir, "imports")

	// Incompressible, so the zip is big enough to split and passes the
	// compression-ratio check.
	video := make([]byte, 64<<10)
	rand.NewChaCha8([32]byte{}).Read(video)
	body := makeImportZip(t, map[string][]byte{"Spooled Stream [abcd1234567].mp4": video})
	pr, pw := io.Pipe()
	req := httptest.NewRequest(http.MethodPost, "/api/import", pr)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("import panicked: %v", r)
			}
		}()
		f.router.ServeHTTP(rec, req)
	}()
	// A test that stops early must not leave the route reading.
	t.Cleanup(func() {
		pw.CloseWithError(io.ErrUnexpectedEOF)
		<-done
	})

	// A pipe's Write returns once the route has read every byte of it: past
	// the signature peek, so from the spool's copy.
	half := len(body) / 2
	if _, err := pw.Write(body[:half]); err != nil {
		t.Fatalf("write the first half: %v", err)
	}
	spools := importTempEntries(t, imports)
	if len(spools) != 1 || !strings.HasSuffix(spools[0], ".zip") {
		t.Errorf("imports/ holds %q mid-upload, want the one spool file", spools)
	}
	if stray := importTempEntries(t, tmp); len(stray) != 0 {
		t.Errorf("the temp directory holds %q mid-upload; the upload belongs on the output volume", stray)
	}

	if _, err := pw.Write(body[half:]); err != nil {
		t.Fatalf("write the second half: %v", err)
	}
	pw.Close()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the import did not finish")
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("import = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	if left := importTempEntries(t, imports); len(left) != 0 {
		t.Errorf("imports/ still holds %q after the import", left)
	}
}

// TestCleanupOldImportTempSweepsAStaleSpoolInImports: what a hard abort
// mid-upload leaves is the spool in imports/ (up to 500 MB), swept once a day
// old — by its exact shape, os.CreateTemp's decimal random part and .zip, so
// an archive whose title starts with the prefix is never touched.
//
// Mutants: the imports/ sweep matching .partial alone (the spool stays);
// matching by prefix alone (the archive goes).
func TestCleanupOldImportTempSweepsAStaleSpoolInImports(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	out := t.TempDir()
	imports := filepath.Join(out, "imports")
	if err := os.MkdirAll(imports, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(imports, importTempPrefix+"*"+importSpoolExt) // the route's own pattern
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	leftover := f.Name()
	fresh := filepath.Join(imports, importTempPrefix+"987654321"+importSpoolExt)
	archive := filepath.Join(imports, importTempPrefix+"notes [dQw4w9WgXcQ].zip")
	for _, p := range []string{fresh, archive} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, p := range []string{leftover, archive} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	if removed, err := CleanupOldImportTemp(out); err != nil || removed != 1 {
		t.Errorf("CleanupOldImportTemp = %d, %v; want 1, nil", removed, err)
	}
	if _, err := os.Stat(leftover); err == nil {
		t.Error("the day-old spool survived")
	}
	for _, p := range []string{fresh, archive} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was swept: %v", filepath.Base(p), err)
		}
	}
}
