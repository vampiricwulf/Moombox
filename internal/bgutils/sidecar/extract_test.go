package sidecar

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// tarGz builds a gzipped tarball holding the given path → content entries.
// Paths use forward slashes, as build.mjs's tar writes them.
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// gz gzips one blob, the shape the embedded Node binary has.
func gz(t *testing.T, data string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(data)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// seed writes path → content files under dir, creating parents.
func seed(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func mustNotExist(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); !os.IsNotExist(err) {
		t.Errorf("%s: want it gone, stat err = %v", name, err)
	}
}

func mustHold(t *testing.T, dir, name, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Errorf("%s: %v", name, err)
		return
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

// previousExtraction is a cache dir as an older Moombox left it: its stamp,
// its Node binary, and a payload that shipped a package, a nested copy of a
// dependency and a source file the next payload no longer has.
func previousExtraction(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	seed(t, dir, map[string]string{
		"version.txt":                                 "old-stamp\n",
		nodeBinaryName():                              "old node",
		"package.json":                                "old manifest",
		"src/server.js":                               "old server",
		"src/removed.js":                              "dropped from src",
		"node_modules/dropped-dep/index.js":           "dependency the new lockfile no longer has",
		"node_modules/kept/index.js":                  "old kept",
		"node_modules/kept/node_modules/dup/index.js": "nested copy the new lockfile hoisted away",
		"node_modules/kept/lib/removed-in-new-ver.js": "file the new version of kept dropped",
		"vendor/ejs.bundle.js":                        "old bundle",
		"notes-from-the-user.txt":                     "not ours",
	})
	return dir
}

var nextPayload = map[string]string{
	"package.json":               "new manifest",
	"src/server.js":              "new server",
	"node_modules/kept/index.js": "new kept",
	"node_modules/dup/index.js":  "hoisted",
	"vendor/ejs.bundle.js":       "new bundle",
}

// The 2026-10-01 audit found 15 packages in a real cache dir that the current
// tarball no longer shipped, and a nested node_modules copy shadowing the
// hoisted one: extraction wrote over the old tree and never removed anything,
// so an upgraded install resolved modules a fresh install does not have.
func TestReExtractRemovesWhatThePreviousPayloadLeft(t *testing.T) {
	dir := previousExtraction(t)

	if err := extractPayload(dir, "new-stamp", gz(t, "new node"), tarGz(t, nextPayload)); err != nil {
		t.Fatalf("extractPayload: %v", err)
	}

	mustNotExist(t, dir, "src/removed.js")
	mustNotExist(t, dir, "node_modules/dropped-dep")
	mustNotExist(t, dir, "node_modules/kept/node_modules")
	mustNotExist(t, dir, "node_modules/kept/lib")
	for name, want := range nextPayload {
		mustHold(t, dir, name, want)
	}
	mustHold(t, dir, nodeBinaryName(), "new node")
	mustHold(t, dir, "version.txt", "new-stamp\n")
}

// The clearing is scoped to the names the payload itself writes. A file that
// happens to sit in the cache dir is not Moombox's to delete.
func TestReExtractLeavesWhatThePayloadDoesNotNameAlone(t *testing.T) {
	dir := previousExtraction(t)

	if err := extractPayload(dir, "new-stamp", gz(t, "new node"), tarGz(t, nextPayload)); err != nil {
		t.Fatalf("extractPayload: %v", err)
	}

	mustHold(t, dir, "notes-from-the-user.txt", "not ours")
}

// A re-extract that dies part-way must not leave the previous stamp beside a
// half-replaced tree: the binary that wrote that stamp would find it, call the
// tree good and run it.
func TestFailedReExtractLeavesNoStamp(t *testing.T) {
	dir := previousExtraction(t)
	whole := tarGz(t, nextPayload)
	truncated := whole[:len(whole)/2]

	if err := extractPayload(dir, "new-stamp", gz(t, "new node"), truncated); err == nil {
		t.Fatal("extractPayload accepted a truncated tarball")
	}

	mustNotExist(t, dir, "version.txt")
}

// A matching stamp means the tree is in use as it stands — possibly by a
// sidecar another Moombox process started from it. Nothing is rewritten.
func TestMatchingStampRewritesNothing(t *testing.T) {
	dir := previousExtraction(t)

	if err := extractPayload(dir, "old-stamp", gz(t, "new node"), tarGz(t, nextPayload)); err != nil {
		t.Fatalf("extractPayload: %v", err)
	}

	mustHold(t, dir, "src/server.js", "old server")
	mustHold(t, dir, "src/removed.js", "dropped from src")
	mustHold(t, dir, nodeBinaryName(), "old node")
}

// The clearing deletes by a name taken from the tarball, so the tar-slip
// check has to refuse an escaping entry before anything is removed for it.
func TestEscapingEntryRemovesNothingOutsideTheCacheDir(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "sidecar")
	seed(t, parent, map[string]string{
		"sidecar/version.txt": "old-stamp\n",
		"neighbour/keep.txt":  "outside the cache dir",
	})

	err := extractPayload(dir, "new-stamp", gz(t, "new node"), tarGz(t, map[string]string{
		"../neighbour/evil.js": "escaped",
	}))
	if err == nil {
		t.Fatal("extractPayload accepted an entry that escapes the cache dir")
	}

	mustHold(t, parent, "neighbour/keep.txt", "outside the cache dir")
	mustNotExist(t, parent, "neighbour/evil.js")
}
