package embed

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// sidecarSourceDir is bgutil-sidecar/ as seen from this package's directory.
var sidecarSourceDir = filepath.Join("..", "..", "..", "bgutil-sidecar")

// trackedFiles are the single files build.mjs packs straight from the source
// tree. vendor/ejs/VERSION stands in for the vendored ejs solver: the tarball
// carries ejs only as the generated vendor/ejs.bundle.js, which cannot be
// rebuilt here, but every re-pin of ejs rewrites VERSION.
var trackedFiles = []string{"package.json", "package-lock.json", "vendor/ejs/VERSION"}

// trackedDir is packed whole, so it is compared in both directions.
const trackedDir = "src"

func tracked(name string) bool {
	return strings.HasPrefix(name, trackedDir+"/") || slices.Contains(trackedFiles, name)
}

// TestEmbeddedSidecarMatchesTheSourceTree fails when sidecar.tar.gz was built
// from a different bgutil-sidecar/ than the one in this checkout.
//
// The tarball is a gitignored build output and `go build` embeds whatever is
// on disk. version.txt records the Node pin only, so nothing else notices a
// tarball left over from before a sidecar change: the build succeeds, the
// suite passes against the old JS, and the binary ships it. CI's embed cache
// did exactly that to its Go tests until 2026-10-01 — its key did not hash
// bgutil-sidecar/src or the vendored ejs.
func TestEmbeddedSidecarMatchesTheSourceTree(t *testing.T) {
	if _, err := os.Stat(filepath.Join(sidecarSourceDir, "build.mjs")); err != nil {
		t.Skipf("bgutil-sidecar/ is not beside this package (%v); nothing to compare against", err)
	}

	packed := map[string][]byte{}
	gz, err := gzip.NewReader(bytes.NewReader(SidecarTarGz))
	if err != nil {
		t.Fatalf("sidecar.tar.gz: %v", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("sidecar.tar.gz: %v", err)
		}
		name := path.Clean(hdr.Name)
		if hdr.Typeflag != tar.TypeReg || !tracked(name) {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("sidecar.tar.gz: read %s: %v", name, err)
		}
		packed[name] = data
	}

	source := map[string][]byte{}
	for _, f := range trackedFiles {
		data, err := os.ReadFile(filepath.Join(sidecarSourceDir, filepath.FromSlash(f)))
		if err != nil {
			t.Fatalf("bgutil-sidecar/%s: %v", f, err)
		}
		source[f] = data
	}
	root := filepath.Join(sidecarSourceDir, trackedDir)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(sidecarSourceDir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		source[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatalf("walk bgutil-sidecar/%s: %v", trackedDir, err)
	}

	var stale []string
	for name, want := range source {
		got, ok := packed[name]
		switch {
		case !ok:
			stale = append(stale, name+": in bgutil-sidecar/ but not in the tarball")
		case !bytes.Equal(got, want):
			stale = append(stale, name+": differs from bgutil-sidecar/"+name)
		}
	}
	for name := range packed {
		if _, ok := source[name]; !ok {
			stale = append(stale, name+": in the tarball but no longer in bgutil-sidecar/")
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Fatalf("internal/bgutils/embed/sidecar.tar.gz was not built from this checkout's bgutil-sidecar/:\n  %s\nRebuild it: cd bgutil-sidecar && node build.mjs",
			strings.Join(stale, "\n  "))
	}
}
