package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionStampIncludesAllPlatforms(t *testing.T) {
	stamp := versionStamp()
	for _, platform := range []string{"windows-amd64", "linux-amd64", "linux-arm64"} {
		if !strings.Contains(stamp, platform) {
			t.Errorf("versionStamp missing platform %q: %s", platform, stamp)
		}
	}
}

func TestNodeTargetsCoverAllPlatforms(t *testing.T) {
	targets := nodeTargets()
	if len(targets) != 3 {
		t.Fatalf("expected 3 targets, got %d", len(targets))
	}
	gotPlatforms := map[string]bool{}
	for _, tgt := range targets {
		key := tgt.goos + "-" + tgt.goarch
		gotPlatforms[key] = true
		if tgt.expectedSHA == "" {
			t.Errorf("target %s has empty expectedSHA", key)
		}
		if tgt.embedName == "" {
			t.Errorf("target %s has empty embedName", key)
		}
	}
	for _, want := range []string{"windows-amd64", "linux-amd64", "linux-arm64"} {
		if !gotPlatforms[want] {
			t.Errorf("missing target for %s", want)
		}
	}
}

// TestBlobsUpToDate: the skip decision trusts only the stamp written beside
// the blobs by the run that produced them — never the tracked version.txt,
// which can be newer than the gitignored blobs after a merge (2026-09-04:
// Node 24 version.txt beside Node 22 blobs reported "already up to date").
func TestBlobsUpToDate(t *testing.T) {
	want := versionStamp()
	writeAll := func(dir string) {
		for _, tgt := range nodeTargets() {
			if err := os.WriteFile(filepath.Join(dir, tgt.embedName), []byte("gz"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	stamp := func(dir, s string) {
		if err := os.WriteFile(filepath.Join(dir, blobStampName), []byte(s+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("stamp matches and every blob present → up to date", func(t *testing.T) {
		dir := t.TempDir()
		writeAll(dir)
		stamp(dir, want)
		if !blobsUpToDate(dir, want) {
			t.Fatal("want true")
		}
	})
	t.Run("no stamp → not up to date, even with a matching version.txt", func(t *testing.T) {
		dir := t.TempDir()
		writeAll(dir)
		if err := os.WriteFile(filepath.Join(dir, "version.txt"), []byte(want+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if blobsUpToDate(dir, want) {
			t.Fatal("version.txt must not vouch for the blobs")
		}
	})
	t.Run("stale stamp → not up to date", func(t *testing.T) {
		dir := t.TempDir()
		writeAll(dir)
		stamp(dir, "node@v22.0.0 old")
		if blobsUpToDate(dir, want) {
			t.Fatal("want false")
		}
	})
	t.Run("a missing blob → not up to date", func(t *testing.T) {
		dir := t.TempDir()
		writeAll(dir)
		stamp(dir, want)
		if err := os.Remove(filepath.Join(dir, nodeTargets()[0].embedName)); err != nil {
			t.Fatal(err)
		}
		if blobsUpToDate(dir, want) {
			t.Fatal("want false")
		}
	})
	t.Run("an empty blob → not up to date", func(t *testing.T) {
		dir := t.TempDir()
		writeAll(dir)
		stamp(dir, want)
		if err := os.WriteFile(filepath.Join(dir, nodeTargets()[0].embedName), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if blobsUpToDate(dir, want) {
			t.Fatal("want false")
		}
	})
}

// TestTrackedVersionTxtMatchesTheManifest: version.txt is tracked and is the
// embedded sidecar's cache-invalidation key (embed.Version), and nothing else
// pinned it to the manifest in this file — a pin bump that forgot it, or a
// merge that took one side of each, shipped a stale key.
func TestTrackedVersionTxtMatchesTheManifest(t *testing.T) {
	got, err := os.ReadFile(filepath.Join("..", "..", "internal", "bgutils", "embed", "version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if want := versionStamp() + "\n"; string(got) != want {
		t.Errorf("version.txt = %q, want %q — run go run ./tools/fetch-node", got, want)
	}
}
