package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/updater"
)

// writePlatformBinaries writes a stand-in for every binary the release job
// builds, each holding its own name.
func writePlatformBinaries(t *testing.T, dir string) []string {
	t.Helper()
	names := []string{"Moombox.exe", "moombox-linux-amd64", "moombox-linux-arm64"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("bytes of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return names
}

// writeManifest is what release.yml's manifest step runs before signing: the
// file it writes must be one an installed Moombox parses back into the
// binaries' real hashes, under the asset name the updater looks for.
//
// THE MUTANTS: hash anything but the file — the hash assertion fails; write
// under another name — the name assertion fails. The empty-tag row is held
// twice over, by BuildManifest's own check (pinned in the updater package)
// and by the read-back through ParseManifest, so only dropping both passes it.
func TestWriteManifestIsWhatTheUpdaterReads(t *testing.T) {
	dir := t.TempDir()
	names := writePlatformBinaries(t, dir)

	path, err := writeManifest("2.9.0", "v2.9.0", dir)
	if err != nil {
		t.Fatalf("writeManifest: %v", err)
	}
	if filepath.Base(path) != updater.ManifestAsset {
		t.Errorf("wrote %s, want %s — the name release.yml uploads and the updater looks for", path, updater.ManifestAsset)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err := updater.ParseManifest(data)
	if err != nil {
		t.Fatalf("the updater cannot parse what was written: %v", err)
	}
	if m.Version != "2.9.0" || m.Tag != "v2.9.0" || len(m.Platforms) != len(names) {
		t.Fatalf("manifest = %+v", m)
	}
	for _, p := range m.Platforms {
		sum := sha256.Sum256([]byte("bytes of " + p.Asset))
		if p.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: sha256 %s is not the file's", p.Asset, p.SHA256)
		}
	}

	if _, err := writeManifest("2.9.0", "", dir); err == nil {
		t.Error("writeManifest accepted an empty tag")
	}
}

// TestReleaseWorkflowPublishesTheSignedManifest ties release.yml to the
// names the updater looks for: the job writes the manifest with this tool,
// uploads both files, and the dry run's draft check counts what the upload
// lists — a dry run is the only rehearsal the pipeline gets before a tag.
//
// THE MUTANTS: drop the manifest step, or either manifest file from the
// upload list, or leave the draft check at the old count — each fails its
// assertion.
func TestReleaseWorkflowPublishesTheSignedManifest(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	wf := string(data)
	if !strings.Contains(wf, `go run ./cmd/sign -manifest -version "$VERSION" -tag "$RELEASE_TAG"`) {
		t.Error("release.yml no longer writes and signs the manifest with cmd/sign -manifest")
	}

	_, after, ok := strings.Cut(wf, "          files: |\n")
	if !ok {
		t.Fatal("release.yml has no `files: |` upload list")
	}
	var files []string
	for line := range strings.Lines(after) {
		name, isFile := strings.CutPrefix(strings.TrimRight(line, "\n"), "            ")
		if !isFile || name == "" || strings.HasPrefix(name, " ") {
			break
		}
		files = append(files, name)
	}
	for _, want := range []string{updater.ManifestAsset, updater.ManifestSignatureAsset} {
		if !slices.Contains(files, want) {
			t.Errorf("the upload list %v does not publish %s", files, want)
		}
	}
	if want := fmt.Sprintf(`"draft=true assets=%d"`, len(files)); !strings.Contains(wf, want) {
		t.Errorf("the dry run's draft check does not expect the %d assets the upload lists (want %s)", len(files), want)
	}
}

// A wrong SIGNING_KEY must fail the manifest step the way it fails a binary's,
// leaving neither the manifest nor a .sig for the publish step to upload.
//
// THE MUTANT: writeSignedManifest without the os.Remove of the manifest on a
// signing failure — the manifest survives.
func TestWriteSignedManifestRefusesAKeyTheUpdaterDoesNotTrust(t *testing.T) {
	_, strangerKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writePlatformBinaries(t, dir)
	if _, _, err := writeSignedManifest(hex.EncodeToString(strangerKey), "2.9.0", "v2.9.0", dir); err == nil {
		t.Fatal("writeSignedManifest accepted a key the updater's public key does not verify")
	}
	for _, name := range []string{updater.ManifestAsset, updater.ManifestSignatureAsset} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s was left behind (stat err = %v)", name, err)
		}
	}
}

// The release job signs with whatever the SIGNING_KEY secret holds. If that is
// not the private half of the public key the updater embeds, the signatures
// are well-formed and worthless: the release goes green and every installed
// Moombox refuses the update. Signing has to refuse instead, and leave no
// .sig behind for the publish step to upload.
func TestSignFileRefusesAKeyTheUpdaterDoesNotTrust(t *testing.T) {
	_, strangerKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "Moombox.exe")
	if err := os.WriteFile(binary, []byte("a release binary"), 0o644); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	sigPath, err := signFile(hex.EncodeToString(strangerKey), binary)
	if err == nil {
		t.Errorf("signFile accepted a key the updater's public key does not verify; wrote %s", sigPath)
	}
	if _, statErr := os.Stat(binary + ".sig"); !os.IsNotExist(statErr) {
		t.Errorf("a .sig was left beside the binary (stat err = %v)", statErr)
	}
}
