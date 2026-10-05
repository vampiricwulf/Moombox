// fetch-node downloads pinned Node.js binaries for Windows x64, Linux x64,
// and Linux arm64. Each platform's `node` binary (or `node.exe` on Windows)
// is extracted, gzipped, and written to internal/bgutils/embed/ behind a
// platform-specific filename. The Moombox build embeds the matching blob
// via go:embed under build tags.
//
// Usage (from repo root):
//
//	go run ./tools/fetch-node
//
// Idempotent: if the gitignored stamp beside the blobs (node-blobs.stamp)
// matches the pinned manifest and every blob is present, nothing is fetched.
// version.txt is the tracked build input and is not consulted.
//
// Bumping the pinned version:
//  1. Pick a new Node v24 LTS patch from https://nodejs.org/dist/index.json
//  2. Fetch SHASUMS256.txt for that release; copy the per-platform SHAs.
//  3. Update nodeVersion + per-target expectedSHA constants below.
//  4. `go run ./tools/fetch-node` to refresh all three embeds.
//  5. `MOOMBOX_LIVE_BG_TEST=1 go test ./internal/bgutils/...` to confirm.
//  6. Commit internal/bgutils/embed/version.txt only -- the .gz blobs are
//     gitignored and CI rebuilds them.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ulikunitz/xz"
)

// Pinned Node.js v24 LTS release. Bump quarterly or on critical CVE.
//
// Last bumped: 2026-09-04 — v24.20.0 was the latest v24 LTS (Krypton);
// moved off the v22 (Jod) line, which is in maintenance until 2027-04.
const nodeVersion = "v24.20.0"

// nodeTarget describes one platform's Node release artifact.
type nodeTarget struct {
	goos, goarch string
	archiveType  string // "zip" (windows) or "tar.xz" (linux)
	binaryName   string // "node.exe" or "node"
	embedName    string // file in internal/bgutils/embed/
	urlInfix     string // "win-x64" / "linux-x64" / "linux-arm64"
	expectedSHA  string // SHA-256 of the downloaded archive (from SHASUMS256.txt)
}

// nodeTargets returns the per-platform Node binary download manifest.
// SHA-256 values come from https://nodejs.org/dist/<nodeVersion>/SHASUMS256.txt.
// Each entry's expectedSHA is the line ending in the matching archive name.
func nodeTargets() []nodeTarget {
	return []nodeTarget{
		{
			goos: "windows", goarch: "amd64",
			archiveType: "zip", binaryName: "node.exe",
			embedName: "node-windows-amd64.gz", urlInfix: "win-x64",
			expectedSHA: "6cac9ffbca8f6a47091e4b5c772e0606049c3871cb67d900c0cedde630e545ba",
		},
		{
			goos: "linux", goarch: "amd64",
			archiveType: "tar.xz", binaryName: "node",
			embedName: "node-linux-amd64.gz", urlInfix: "linux-x64",
			expectedSHA: "2f2c0da162318f0de47665410c7c8c2ed3d36c8f3105de4bbc61176c70a7cbf2",
		},
		{
			goos: "linux", goarch: "arm64",
			archiveType: "tar.xz", binaryName: "node",
			embedName: "node-linux-arm64.gz", urlInfix: "linux-arm64",
			expectedSHA: "5f4ddab610c1ab2016b3c227cebdbf6d9495161487e4739c7b90090595f465f7",
		},
	}
}

// blobStampName is written beside the .gz blobs by the run that produced them
// and is gitignored with them. It — not the tracked version.txt — is what the
// idempotency check trusts: a checkout can carry a newer version.txt beside
// older blobs (seen 2026-09-04 after the Node 22→24 bump), and a tracked file
// cannot vouch for untracked ones.
const blobStampName = "node-blobs.stamp"

// blobsUpToDate reports whether every embed blob is present and non-empty and
// the stamp beside them matches the pinned manifest.
func blobsUpToDate(embedDir, wantStamp string) bool {
	stamp, err := os.ReadFile(filepath.Join(embedDir, blobStampName))
	if err != nil || strings.TrimSpace(string(stamp)) != wantStamp {
		return false
	}
	for _, tgt := range nodeTargets() {
		info, err := os.Stat(filepath.Join(embedDir, tgt.embedName))
		if err != nil || info.Size() == 0 {
			return false
		}
	}
	return true
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fetch-node:", err)
		os.Exit(1)
	}
}

func run() error {
	repoRoot, err := findRepoRoot()
	if err != nil {
		return err
	}
	embedDir := filepath.Join(repoRoot, "internal", "bgutils", "embed")
	if err := os.MkdirAll(embedDir, 0o755); err != nil {
		return fmt.Errorf("mkdir embed: %w", err)
	}

	versionPath := filepath.Join(embedDir, "version.txt")
	wantStamp := versionStamp()

	if blobsUpToDate(embedDir, wantStamp) {
		// The blobs match, but version.txt is tracked and can drift from the
		// manifest on its own (a bad merge, a hand edit). It is the sidecar's
		// on-disk cache-invalidation key, so repair it here rather than
		// reporting "up to date" over a wrong one forever.
		if cur, err := os.ReadFile(versionPath); err != nil || string(cur) != wantStamp+"\n" {
			if err := os.WriteFile(versionPath, []byte(wantStamp+"\n"), 0o644); err != nil {
				return fmt.Errorf("write version.txt: %w", err)
			}
			fmt.Println("fetch-node: version.txt did not match the manifest — rewritten")
		}
		fmt.Printf("fetch-node: already up to date (%s)\n", wantStamp)
		return nil
	}

	for _, tgt := range nodeTargets() {
		if err := fetchOne(embedDir, tgt); err != nil {
			return fmt.Errorf("%s/%s: %w", tgt.goos, tgt.goarch, err)
		}
	}

	if err := os.WriteFile(versionPath, []byte(wantStamp+"\n"), 0o644); err != nil {
		return fmt.Errorf("write version.txt: %w", err)
	}
	if err := os.WriteFile(filepath.Join(embedDir, blobStampName), []byte(wantStamp+"\n"), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", blobStampName, err)
	}
	fmt.Printf("fetch-node: %s\n", wantStamp)
	return nil
}

func fetchOne(embedDir string, tgt nodeTarget) error {
	url := fmt.Sprintf("https://nodejs.org/dist/%s/node-%s-%s.%s",
		nodeVersion, nodeVersion, tgt.urlInfix, tgt.archiveType)
	fmt.Printf("fetch-node: downloading %s\n", url)
	archiveBytes, err := download(url)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	gotSHA := hex.EncodeToString(sha256Sum(archiveBytes))
	if gotSHA != tgt.expectedSHA {
		return fmt.Errorf("SHA-256 mismatch for %s: got %s, want %s",
			tgt.embedName, gotSHA, tgt.expectedSHA)
	}
	fmt.Printf("fetch-node: SHA-256 verified (%s)\n", gotSHA)

	var binBytes []byte
	switch tgt.archiveType {
	case "zip":
		binBytes, err = extractFromZip(archiveBytes, tgt.binaryName)
	case "tar.xz":
		binBytes, err = extractFromTarXz(archiveBytes, tgt.binaryName)
	default:
		return fmt.Errorf("unknown archiveType %q", tgt.archiveType)
	}
	if err != nil {
		return fmt.Errorf("extract %s: %w", tgt.binaryName, err)
	}
	fmt.Printf("fetch-node: extracted %s (%.1f MB)\n",
		tgt.binaryName, float64(len(binBytes))/1024.0/1024.0)

	gzPath := filepath.Join(embedDir, tgt.embedName)
	if err := writeGzipped(gzPath, binBytes); err != nil {
		return fmt.Errorf("gzip write: %w", err)
	}
	if info, err := os.Stat(gzPath); err == nil {
		fmt.Printf("fetch-node: wrote %s (%.1f MB gzipped)\n",
			gzPath, float64(info.Size())/1024.0/1024.0)
	}
	return nil
}

func versionStamp() string {
	parts := []string{fmt.Sprintf("node@%s", nodeVersion)}
	for _, tgt := range nodeTargets() {
		parts = append(parts, fmt.Sprintf("%s-%s@%s", tgt.goos, tgt.goarch, tgt.expectedSHA))
	}
	return strings.Join(parts, " ")
}

// findRepoRoot walks up from cwd looking for go.mod so the tool works from
// any subdirectory. Returns an error rather than guessing if go.mod isn't
// found within 8 levels (deep nesting is a misuse signal).
func findRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("go.mod not found above %s; run from inside the Moombox repo", cwd)
}

func download(url string) ([]byte, error) {
	// 5-minute timeout: typical Node release archive is ~30-50 MB, slow
	// CI/dev networks fetch in <60s, 5min covers worst-case without
	// letting a hung nodejs.org block CI forever.
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// Cap at 200 MB: pinned Node is <50 MB compressed; anything an
	// order of magnitude larger is a malicious redirect or a typo'd
	// URL pointing at something else. Avoids unbounded RAM growth.
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func extractFromZip(zipBytes []byte, binaryName string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) != binaryName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open zip entry %q: %w", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read zip entry %q: %w", f.Name, err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("%s not found in zip archive", binaryName)
}

func extractFromTarXz(xzBytes []byte, binaryName string) ([]byte, error) {
	xr, err := xz.NewReader(bytes.NewReader(xzBytes))
	if err != nil {
		return nil, fmt.Errorf("xz reader: %w", err)
	}
	tr := tar.NewReader(xr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar header: %w", err)
		}
		// Linux Node tarballs put node at bin/node under a versioned dir,
		// e.g. node-v24.20.0-linux-x64/bin/node
		if filepath.Base(hdr.Name) == binaryName && strings.Contains(hdr.Name, "/bin/") {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("read tar entry %q: %w", hdr.Name, err)
			}
			return data, nil
		}
	}
	return nil, fmt.Errorf("%s not found in tar.xz archive", binaryName)
}

// writeGzipped writes via a temp file + rename so an interrupted run can't
// leave a truncated .gz at the final path — the idempotency check only
// stats for existence, so a partial blob would otherwise be accepted on the
// next run and embedded into release binaries.
func writeGzipped(outPath string, raw []byte) error {
	tmpPath := outPath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	zw, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		out.Close()
		os.Remove(tmpPath)
		return err
	}
	if _, err := zw.Write(raw); err != nil {
		out.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := zw.Close(); err != nil {
		out.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}
