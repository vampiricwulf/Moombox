// fetch-node downloads a pinned Node.js Windows x64 release, verifies its
// SHA-256, extracts node.exe from the zip, gzips it to
// internal/bgutils/embed/node.exe.gz, and writes a version.txt with the
// pinned version + sha as a cache-invalidation key.
//
// The Moombox build embeds node.exe.gz via go:embed (Phase 3); the runtime
// extracts it on first launch alongside the bgutil-sidecar tarball, then
// pipes JSON-RPC requests to `node sidecar/src/server.js`.
//
// Usage (from repo root):
//
//	go run ./tools/fetch-node
//
// Idempotent: if internal/bgutils/embed/version.txt already matches the
// pinned version, this tool exits 0 without re-downloading.
//
// Bumping the pinned version:
//  1. Pick a new Node v22 LTS patch from https://nodejs.org/dist/index.json
//  2. Fetch SHASUMS256.txt for that release; copy the win-x64.zip line.
//  3. Update nodeVersion + expectedSHA constants below.
//  4. `go run ./tools/fetch-node` to refresh the embed.
//  5. `MOOMBOX_LIVE_BG_TEST=1 go test ./internal/bgutils/...` to confirm.
//  6. Commit internal/bgutils/embed/version.txt only -- the .gz blob is
//     gitignored and CI rebuilds it.
package main

import (
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
)

// Pinned Node.js Windows x64 release. Bump quarterly or on critical CVE.
//
// Last bumped: 2026-04-26 — v22.22.2 was the latest v22 LTS (Jod) at the
// time the sidecar landed.
const (
	nodeVersion = "v22.22.2"
	// SHA-256 from the official SHASUMS256.txt at
	// https://nodejs.org/dist/v22.22.2/SHASUMS256.txt -- the line ending
	// in `node-v22.22.2-win-x64.zip`.
	expectedSHA = "7c93e9d92bf68c07182b471aa187e35ee6cd08ef0f24ab060dfff605fcc1c57c"
)

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
	gzPath := filepath.Join(embedDir, "node.exe.gz")
	wantStamp := versionStamp()

	// Idempotency: skip if the embed already matches the pin.
	if existing, _ := os.ReadFile(versionPath); strings.TrimSpace(string(existing)) == wantStamp {
		if _, err := os.Stat(gzPath); err == nil {
			fmt.Printf("fetch-node: already up to date (%s)\n", wantStamp)
			return nil
		}
	}

	// Download the win-x64 ZIP.
	url := fmt.Sprintf("https://nodejs.org/dist/%s/node-%s-win-x64.zip", nodeVersion, nodeVersion)
	fmt.Printf("fetch-node: downloading %s\n", url)
	zipBytes, err := download(url)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	// Verify SHA-256 against the pinned constant -- guards against MITM and
	// against accidentally bumping nodeVersion without updating expectedSHA.
	gotSHA := hex.EncodeToString(sha256Sum(zipBytes))
	if gotSHA != expectedSHA {
		return fmt.Errorf("SHA-256 mismatch: got %s, want %s", gotSHA, expectedSHA)
	}
	fmt.Printf("fetch-node: SHA-256 verified (%s)\n", gotSHA)

	// Extract node.exe from the zip. Inside the archive the path is
	// "node-vX.Y.Z-win-x64/node.exe"; we accept any entry whose basename
	// matches so a future archive rearrangement doesn't silently break.
	nodeBytes, err := extractNodeExe(zipBytes)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	fmt.Printf("fetch-node: extracted node.exe (%.1f MB)\n", float64(len(nodeBytes))/1024.0/1024.0)

	// Gzip and write to the embed dir.
	if err := writeGzipped(gzPath, nodeBytes); err != nil {
		return fmt.Errorf("gzip write: %w", err)
	}
	gzInfo, _ := os.Stat(gzPath)
	if gzInfo != nil {
		fmt.Printf("fetch-node: wrote %s (%.1f MB gzipped)\n", gzPath, float64(gzInfo.Size())/1024.0/1024.0)
	}

	// Update version.txt last so a partial failure leaves the cache
	// invalidated (next run re-downloads).
	if err := os.WriteFile(versionPath, []byte(wantStamp+"\n"), 0o644); err != nil {
		return fmt.Errorf("write version.txt: %w", err)
	}
	fmt.Printf("fetch-node: %s\n", wantStamp)
	return nil
}

func versionStamp() string {
	return fmt.Sprintf("node@%s sha256@%s", nodeVersion, expectedSHA)
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
	// 5-minute timeout: typical Node release ZIP is ~30 MB, slow
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

func extractNodeExe(zipBytes []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) != "node.exe" {
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
	return nil, errors.New("node.exe not found in zip archive")
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
