package sidecar

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	bgembed "github.com/vampiricwulf/Moombox/internal/bgutils/embed"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// nodeBinaryName returns the runtime filename for the extracted Node
// binary. Prefixed with "moombox-sidecar" rather than the bare "node"
// so it's identifiable in Task Manager / `ps` / Process Explorer
// (otherwise it groups with any other Node process the user has —
// VS Code, npm dev servers, etc.).
func nodeBinaryName() string {
	if runtime.GOOS == "windows" {
		return "moombox-sidecar.exe"
	}
	return "moombox-sidecar"
}

// staleNodeBinaryNames returns historical names the binary may have
// been extracted as. Used to clean up orphan binaries left over from
// pre-rename installs that won't otherwise be removed (the cache key
// invalidation drops only the version.txt + re-extracts the current
// names; orphans linger forever without explicit removal).
func staleNodeBinaryNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"node.exe"}
	}
	return []string{"node"}
}

// buildCacheStamp combines the Node-binary version manifest with a runtime
// SHA256 of the embedded sidecar tarball. This makes the cache stamp
// reflect both Node identity (via the existing version.txt format) and
// sidecar source identity (via the tarball hash) so any change to either
// triggers a re-extract on next launch.
//
// Format: "<bgembed.Version>\nsidecar@<hex-sha256-of-SidecarTarGz>"
//
// Pure runtime computation — no build-time changes needed.
func buildCacheStamp() string {
	sum := sha256.Sum256(bgembed.SidecarTarGz)
	return strings.TrimSpace(bgembed.Version) + "\nsidecar@" + hex.EncodeToString(sum[:])
}

// extractIfNeeded compares the on-disk version.txt against the embedded
// stamp; if they differ (or the cache dir is incomplete), gunzip-extracts
// the Node binary and tar-extracts sidecar.tar.gz into cacheDir, then writes
// the new version.txt.
//
// Idempotent: a no-op when cacheDir already has the correct version
// stamp AND the key files (Node binary, src/server.js) are present.
//
// Pure Go: uses archive/tar + compress/gzip from stdlib. End users do
// NOT need a system tar binary -- that's only required at build time
// (by bgutil-sidecar/build.mjs).
func extractIfNeeded(cacheDir string) error {
	wantStamp := buildCacheStamp()
	stampPath := filepath.Join(cacheDir, "version.txt")

	// Always ensure the cache dir exists AND has a tightened DACL,
	// even on the cache-hit path. Users upgrading from v2.5.x already
	// have %LOCALAPPDATA%/Moombox/sidecar populated with the looser
	// inherited ACL; without re-applying here, their pre-existing
	// extraction would never get the security benefit because
	// cacheLooksGood would short-circuit before any DACL call. icacls
	// is one-shot per process startup; idempotent. Failure is
	// log-and-continue: the cache stays usable, just less locked-down.
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("mkdir cache dir: %w", err)
	}
	// Log-but-continue on failure. Demoted to Debug (matches the config +
	// cookie dir sites): the common failure is ACCESS_DENIED on a cache dir
	// created under an elevated/admin context, and on the single-user host
	// this app targets nobody else can read it anyway — a WARN there is just
	// noise. Raise the log level to Debug to surface the miss.
	if daclErr := utils.ApplyUserOnlyDACL(cacheDir); daclErr != nil {
		slog.Debug("could not restrict sidecar cache dir to current user", "dir", cacheDir, "err", daclErr)
	}

	if cacheLooksGood(cacheDir, stampPath, wantStamp) {
		return nil
	}

	// 1. Gunzip-extract the Node binary into cacheDir.
	nodePath := filepath.Join(cacheDir, nodeBinaryName())
	if err := writeGunzipped(nodePath, bgembed.EmbeddedNode, 0o755); err != nil {
		return fmt.Errorf("extract Node binary: %w", err)
	}

	// 2. Gunzip+tar-extract sidecar.tar.gz into cacheDir.
	if err := extractTarGz(cacheDir, bgembed.SidecarTarGz); err != nil {
		return fmt.Errorf("extract sidecar tarball: %w", err)
	}

	// 2b. Remove any stale orphan binaries from prior versions that
	// had different naming (e.g., the bare "node.exe" before the
	// moombox-sidecar rename). Cache invalidation refreshes the
	// current binary but doesn't remove orphans, so do it here.
	for _, stale := range staleNodeBinaryNames() {
		stalePath := filepath.Join(cacheDir, stale)
		if stalePath == nodePath {
			continue // current name; never delete
		}
		if err := os.Remove(stalePath); err != nil && !os.IsNotExist(err) {
			// Non-fatal: orphan stays on disk, slightly wasteful but harmless.
			_ = err
		}
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
		filepath.Join(cacheDir, nodeBinaryName()),
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
			// Clamp mode to 0o644 minimum -- a tar header with mode=0
			// (some bsdtar variants on Windows emit no mode info) would
			// otherwise produce an unreadable file.
			mode := os.FileMode(hdr.Mode & 0o777)
			if mode == 0 {
				mode = 0o644
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
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
