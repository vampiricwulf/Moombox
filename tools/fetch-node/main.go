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
// Idempotent: if internal/bgutils/embed/version.txt already matches the
// pinned manifest, this tool exits 0 without re-downloading.
//
// Bumping the pinned version:
//  1. Pick a new Node v22 LTS patch from https://nodejs.org/dist/index.json
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

// Pinned Node.js v22 LTS release. Bump quarterly or on critical CVE.
//
// Last bumped: 2026-04-26 — v22.22.2 was the latest v22 LTS (Jod) at the
// time the sidecar landed.
const nodeVersion = "v22.22.2"

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
			expectedSHA: "7c93e9d92bf68c07182b471aa187e35ee6cd08ef0f24ab060dfff605fcc1c57c",
		},
		{
			goos: "linux", goarch: "amd64",
			archiveType: "tar.xz", binaryName: "node",
			embedName: "node-linux-amd64.gz", urlInfix: "linux-x64",
			expectedSHA: "88fd1ce767091fd8d4a99fdb2356e98c819f93f3b1f8663853a2dee9b438068a",
		},
		{
			goos: "linux", goarch: "arm64",
			archiveType: "tar.xz", binaryName: "node",
			embedName: "node-linux-arm64.gz", urlInfix: "linux-arm64",
			expectedSHA: "e9e1930fd321a470e29bb68f30318bf58e3ecb4acb4f1533fb19c58328a091fe",
		},
	}
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

	// Idempotency: skip if every target already exists and version.txt
	// matches the pinned manifest.
	if existing, _ := os.ReadFile(versionPath); strings.TrimSpace(string(existing)) == wantStamp {
		allPresent := true
		for _, tgt := range nodeTargets() {
			if _, err := os.Stat(filepath.Join(embedDir, tgt.embedName)); err != nil {
				allPresent = false
				break
			}
		}
		if allPresent {
			fmt.Printf("fetch-node: already up to date (%s)\n", wantStamp)
			return nil
		}
	}

	for _, tgt := range nodeTargets() {
		if err := fetchOne(embedDir, tgt); err != nil {
			return fmt.Errorf("%s/%s: %w", tgt.goos, tgt.goarch, err)
		}
	}

	if err := os.WriteFile(versionPath, []byte(wantStamp+"\n"), 0o644); err != nil {
		return fmt.Errorf("write version.txt: %w", err)
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
		// e.g. node-v22.22.2-linux-x64/bin/node
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

func writeGzipped(outPath string, raw []byte) error {
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	zw, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		return err
	}
	if _, err := zw.Write(raw); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return out.Sync()
}
