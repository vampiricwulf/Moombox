package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// A per-binary signature says "this key signed these bytes" and nothing else:
// a validly signed OLDER binary, or another platform's, verifies against it
// just as well. Someone able to answer the update check — a compromised
// GitHub account short of the signing key, or a tampered response — could
// serve either under a newer tag and every install would take it. The
// manifest is what binds bytes to a release: release.yml writes one per
// release naming its version and tag and, per platform, the binary's asset
// name and SHA-256, and signs it with the same key (cmd/sign -manifest).
// ApplyUpdate refuses unless that signed document names the version being
// applied, that version is newer than the running one, and the running
// platform's entry hashes to the downloaded bytes. The per-binary .sig files
// are still published (clients that predate the manifest verify only them)
// and still checked.

// ManifestAsset is the release asset holding the manifest;
// ManifestSignatureAsset is its detached Ed25519 signature, in the same raw
// 64-byte form as every binary's .sig.
const (
	ManifestAsset          = "moombox-manifest.json"
	ManifestSignatureAsset = ManifestAsset + ".sig"
)

// maxManifestSize bounds the manifest a client reads into memory. The real
// one is a few hundred bytes.
const maxManifestSize = 64 << 10

// Manifest is the signed release manifest. Platforms is keyed by
// "<goos>/<goarch>", the releaseAssetMap key.
type Manifest struct {
	Version   string                      `json:"version"`
	Tag       string                      `json:"tag"`
	Platforms map[string]ManifestPlatform `json:"platforms"`
}

// ManifestPlatform is one platform's entry: the release asset name of its
// binary and that binary's SHA-256, lowercase hex.
type ManifestPlatform struct {
	Asset  string `json:"asset"`
	SHA256 string `json:"sha256"`
}

// BuildManifest hashes, from dir, the binary of every platform
// releaseAssetMap lists and returns the manifest for version and tag. A
// missing binary is an error: a platform the updater looks for must never be
// published without its entry. Used by cmd/sign -manifest in the release job.
func BuildManifest(version, tag, dir string) (*Manifest, error) {
	if version == "" || tag == "" {
		return nil, fmt.Errorf("manifest needs both a version and a tag")
	}
	m := &Manifest{Version: version, Tag: tag, Platforms: make(map[string]ManifestPlatform, len(releaseAssetMap))}
	for key, a := range releaseAssetMap {
		sum, err := fileSHA256(filepath.Join(dir, a.binary))
		if err != nil {
			return nil, fmt.Errorf("hashing %s for %s: %w", a.binary, key, err)
		}
		m.Platforms[key] = ManifestPlatform{Asset: a.binary, SHA256: hex.EncodeToString(sum)}
	}
	return m, nil
}

// ParseManifest decodes a manifest and checks it is well-formed: a version,
// a tag, and at least one platform, each naming an asset and a 32-byte
// SHA-256. Unknown fields are ignored, so a later release can add some.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest is not valid JSON: %w", err)
	}
	if m.Version == "" || m.Tag == "" {
		return nil, fmt.Errorf("manifest names no version or no tag")
	}
	if len(m.Platforms) == 0 {
		return nil, fmt.Errorf("manifest lists no platforms")
	}
	for key, p := range m.Platforms {
		if p.Asset == "" {
			return nil, fmt.Errorf("manifest entry %s names no asset", key)
		}
		if sum, err := hex.DecodeString(p.SHA256); err != nil || len(sum) != sha256.Size {
			return nil, fmt.Errorf("manifest entry %s carries no valid SHA-256", key)
		}
	}
	return &m, nil
}

// entryFor applies the manifest to the release being applied and returns the
// running platform's entry, refusing unless the manifest names exactly that
// release (version and tag), the version is newer than currentVersion, and
// it lists goos/goarch under the asset name the updater downloads there.
func (m *Manifest) entryFor(release *ReleaseInfo, currentVersion, goos, goarch string) (ManifestPlatform, error) {
	if strings.TrimPrefix(m.Version, "v") != strings.TrimPrefix(release.Version, "v") || m.Tag != release.TagName {
		return ManifestPlatform{}, fmt.Errorf("the signed manifest is for %s (%s), not the release being applied, %s (%s)",
			m.Version, m.Tag, release.Version, release.TagName)
	}
	if CompareVersions(m.Version, currentVersion) <= 0 {
		return ManifestPlatform{}, fmt.Errorf("the signed manifest's version %s is not newer than the running %s", m.Version, currentVersion)
	}
	key := goos + "/" + goarch
	p, ok := m.Platforms[key]
	if !ok {
		return ManifestPlatform{}, fmt.Errorf("the signed manifest for %s has no entry for %s", m.Tag, key)
	}
	want, ok := assetsForPlatform(goos, goarch)
	if !ok || !strings.EqualFold(p.Asset, want.binary) {
		return ManifestPlatform{}, fmt.Errorf("the signed manifest's %s entry names %q, not %q", key, p.Asset, want.binary)
	}
	return p, nil
}

// verifiedManifestEntry downloads the release's manifest and its signature,
// verifies the signature with the updater's key, and returns the running
// platform's entry for this release (entryFor). A release that publishes no
// manifest is refused outright: there is nothing to bind its binary to it.
func (u *Updater) verifiedManifestEntry(ctx context.Context, release *ReleaseInfo) (ManifestPlatform, error) {
	if release.ManifestURL == "" || release.ManifestSignatureURL == "" {
		where := "its release page"
		if release.ReleaseURL != "" {
			where = release.ReleaseURL
		}
		return ManifestPlatform{}, fmt.Errorf("release %s publishes no signed manifest (%s), so it cannot be verified for automatic update — update manually: download it from %s and replace the binary",
			release.TagName, ManifestAsset, where)
	}

	dir, err := os.MkdirTemp("", "moombox-manifest-*")
	if err != nil {
		return ManifestPlatform{}, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)
	manifestPath := filepath.Join(dir, ManifestAsset)
	sigPath := filepath.Join(dir, ManifestSignatureAsset)

	if err := u.downloadFile(ctx, release.ManifestURL, manifestPath); err != nil {
		return ManifestPlatform{}, fmt.Errorf("manifest download failed: %w", err)
	}
	if err := u.downloadFile(ctx, release.ManifestSignatureURL, sigPath); err != nil {
		return ManifestPlatform{}, fmt.Errorf("manifest signature download failed: %w", err)
	}
	// Bounded before anything reads it whole — the signature check included.
	if fi, err := os.Stat(manifestPath); err != nil {
		return ManifestPlatform{}, err
	} else if fi.Size() > maxManifestSize {
		return ManifestPlatform{}, fmt.Errorf("manifest is %d bytes, over the %d-byte limit", fi.Size(), maxManifestSize)
	}
	if err := u.verifySignature(manifestPath, sigPath); err != nil {
		return ManifestPlatform{}, fmt.Errorf("manifest signature verification failed: %w", err)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return ManifestPlatform{}, fmt.Errorf("reading manifest: %w", err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		return ManifestPlatform{}, err
	}
	return m.entryFor(release, u.currentVersion, runtime.GOOS, runtime.GOARCH)
}

// verifyFileSHA256 reports whether the file at path hashes to wantHex.
func verifyFileSHA256(path, wantHex string) error {
	want, err := hex.DecodeString(wantHex)
	if err != nil || len(want) != sha256.Size {
		return fmt.Errorf("manifest carries no valid SHA-256")
	}
	got, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("SHA-256 %x does not match the signed manifest's %x — not the binary this release published", got, want)
	}
	return nil
}

// fileSHA256 streams the file at path through SHA-256.
func fileSHA256(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
