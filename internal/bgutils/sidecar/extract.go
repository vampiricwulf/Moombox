package sidecar

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	bgembed "github.com/vampiricwulf/Moombox/internal/bgutils/embed"
)

// extractIfNeeded compares the on-disk version.txt against the embedded
// stamp; if they differ (or the cache dir is incomplete), gunzip-extracts
// node.exe and tar-extracts sidecar.tar.gz into cacheDir, then writes the
// new version.txt.
//
// Idempotent: a no-op when cacheDir already has the correct version
// stamp AND the key files (node.exe, src/server.js) are present.
//
// Pure Go: uses archive/tar + compress/gzip from stdlib. End users do
// NOT need a system tar binary -- that's only required at build time
// (by bgutil-sidecar/build.mjs).
func extractIfNeeded(cacheDir string) error {
	wantStamp := strings.TrimSpace(bgembed.Version)
	stampPath := filepath.Join(cacheDir, "version.txt")
	if cacheLooksGood(cacheDir, stampPath, wantStamp) {
		return nil
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("mkdir cache dir: %w", err)
	}

	// 1. Gunzip-extract node.exe into cacheDir/node.exe.
	nodePath := filepath.Join(cacheDir, "node.exe")
	if err := writeGunzipped(nodePath, bgembed.NodeExeGz, 0o755); err != nil {
		return fmt.Errorf("extract node.exe: %w", err)
	}

	// 2. Gunzip+tar-extract sidecar.tar.gz into cacheDir.
	if err := extractTarGz(cacheDir, bgembed.SidecarTarGz); err != nil {
		return fmt.Errorf("extract sidecar tarball: %w", err)
	}

	// 3. Write version.txt LAST so a partial extraction next time forces a
	//    full redo (next launch sees the stamp missing/stale and re-extracts).
	if err := os.WriteFile(stampPath, []byte(wantStamp+"\n"), 0o644); err != nil {
		return fmt.Errorf("write version.txt: %w", err)
	}
	return nil
}

// cacheLooksGood returns true when the cache dir already holds an
// extraction matching the embedded stamp AND the two files we'll actually
// invoke are present. Anything missing forces a fresh extract.
func cacheLooksGood(cacheDir, stampPath, wantStamp string) bool {
	existing, err := os.ReadFile(stampPath)
	if err != nil || strings.TrimSpace(string(existing)) != wantStamp {
		return false
	}
	for _, mustExist := range []string{
		filepath.Join(cacheDir, "node.exe"),
		filepath.Join(cacheDir, "src", "server.js"),
	} {
		if _, err := os.Stat(mustExist); err != nil {
			return false
		}
	}
	return true
}

func writeGunzipped(outPath string, gzData []byte, mode os.FileMode) error {
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	gz, err := gzip.NewReader(bytes.NewReader(gzData))
	if err != nil {
		return err
	}
	defer gz.Close()

	if _, err := io.Copy(out, gz); err != nil {
		return err
	}
	return out.Sync()
}

func extractTarGz(destDir string, gzData []byte) error {
	gz, err := gzip.NewReader(bytes.NewReader(gzData))
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	cleanDest := filepath.Clean(destDir)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}

		target := filepath.Join(destDir, filepath.FromSlash(hdr.Name))
		// Defense against tar slip: ensure target stays inside destDir.
		// Without this, a malicious tarball could write to ../../etc/...
		// (Our tarball is built from our own bgutil-sidecar/, so this is
		// belt-and-suspenders against future tarball injection paths.)
		cleanTarget := filepath.Clean(target)
		if cleanTarget != cleanDest && !strings.HasPrefix(cleanTarget, cleanDest+string(os.PathSeparator)) {
			return fmt.Errorf("tar entry escapes destDir: %q", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode&0o777))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// Skip symlinks, hardlinks, char devices, etc. -- our tarball
			// only has regular files and directories.
		}
	}
	return nil
}
