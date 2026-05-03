// Package updater provides auto-update checking and self-replacement for Moombox.
package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"

	"github.com/vampiricwulf/Moombox/internal/httpx"
)

// markdownPolicy is the bluemonday HTML sanitizer policy applied to
// rendered release notes. UGCPolicy permits common formatting (headings,
// lists, links, code, emphasis) but strips scripts, event handlers, and
// dangerous protocols. Source markdown comes from our own RELEASE_NOTES.md
// but we sanitize anyway as defense-in-depth.
var markdownPolicy = bluemonday.UGCPolicy()

// stripDownloadLinks removes the leading download-link section from a
// GitHub release body. Our release workflow puts download links above a
// `\n---\n` separator and the actual changelog below; this returns just
// the changelog. Bodies without a separator are returned unchanged.
func stripDownloadLinks(body string) string {
	if i := strings.Index(body, "\n---\n"); i >= 0 {
		return strings.TrimSpace(body[i+len("\n---\n"):])
	}
	return body
}

// renderReleaseNotesHtml converts markdown release notes to sanitized HTML
// suitable for direct innerHTML assignment in the web UI.
func renderReleaseNotesHtml(markdown string) string {
	if markdown == "" {
		return ""
	}
	var buf bytes.Buffer
	if err := goldmark.Convert([]byte(markdown), &buf); err != nil {
		// Fall back to escaped plain text on render failure.
		return "<pre>" + html.EscapeString(markdown) + "</pre>"
	}
	return markdownPolicy.Sanitize(buf.String())
}

// logger is a type alias for the anonymous logger interface.
// Per CLAUDE.md convention, this avoids a named exported interface while
// keeping the repeated anonymous interface DRY within this package.
type logger = interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// ReleaseInfo holds information about an available update.
type ReleaseInfo struct {
	Version          string `json:"version"`                // "2.0.16" (stripped "v" prefix)
	TagName          string `json:"tagName"`                // "v2.0.16"
	DownloadURL      string `json:"downloadUrl"`            // asset browser_download_url for the platform binary (Moombox.exe / moombox-linux-{amd64,arm64})
	SignatureURL      string `json:"signatureUrl,omitempty"` // asset browser_download_url for the matching .sig
	ReleaseNotes     string `json:"releaseNotes"`           // stripped raw markdown (for TUI glamour rendering)
	ReleaseNotesHtml string `json:"releaseNotesHtml"`       // sanitized HTML (for web UI innerHTML)
	PublishedAt      string `json:"publishedAt"`
}

// assetNames bundles the GitHub release asset names for one platform.
// binary is the runnable executable; sig is its Ed25519 signature.
type assetNames struct {
	binary, sig string
}

// releaseAssetMap maps GOOS/GOARCH to the asset names CI publishes.
// Adding a new platform: extend this map AND ensure the release workflow
// uploads matching artifacts. The Windows entry keeps the historical
// Moombox.exe name so existing 2.6.2 clients continue to find it.
var releaseAssetMap = map[string]assetNames{
	"windows/amd64": {binary: "Moombox.exe", sig: "Moombox.exe.sig"},
	"linux/amd64":   {binary: "moombox-linux-amd64", sig: "moombox-linux-amd64.sig"},
	"linux/arm64":   {binary: "moombox-linux-arm64", sig: "moombox-linux-arm64.sig"},
}

// assetsForPlatform looks up the asset names for an explicit goos/goarch
// (used by tests). Production code calls currentPlatformAssets() below.
func assetsForPlatform(goos, goarch string) (assetNames, bool) {
	a, ok := releaseAssetMap[goos+"/"+goarch]
	return a, ok
}

// currentPlatformAssets returns the asset names for the running build's
// GOOS/GOARCH, sourced from runtime.GOOS and runtime.GOARCH.
func currentPlatformAssets() (assetNames, bool) {
	return assetsForPlatform(runtime.GOOS, runtime.GOARCH)
}

// Updater checks for and applies updates from GitHub Releases.
type Updater struct {
	currentVersion string
	exePath        string
	logger         logger
	repoOwner      string
	repoName       string
	client         *http.Client

	// apiBaseURL is the GitHub API origin. Tests override to point at an
	// httptest server so CheckForUpdate doesn't hit github.com.
	apiBaseURL string
	// verifySignature lets tests substitute a stub key-verifier without
	// needing access to the embedded production private key. Defaults
	// to the package-level VerifySignature in New().
	verifySignature func(binaryPath, sigPath string) error
}

// downloadClient is the shared HTTP client used for binary downloads.
// Backed by the shared httpx transport. The 5-minute timeout is
// generous to accommodate slow connections on 20-30 MB update
// payloads; the per-request u.client (10s timeout) is reserved for
// quick GitHub API calls.
var downloadClient = httpx.Client(5 * time.Minute)

// githubRelease is the subset of the GitHub API response we parse.
type githubRelease struct {
	TagName     string        `json:"tag_name"`
	Body        string        `json:"body"`
	PublishedAt string        `json:"published_at"`
	Assets      []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// New creates a new Updater. Returns an error if the executable path
// cannot be determined.
func New(currentVersion string, log logger) (*Updater, error) {
	exePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cannot determine executable path: %w", err)
	}
	return &Updater{
		currentVersion: strings.TrimPrefix(currentVersion, "v"),
		exePath:        exePath,
		logger:         log,
		repoOwner:      "vampiricwulf",
		repoName:       "Moombox",
		client:          httpx.Client(10 * time.Second),
		apiBaseURL:      "https://api.github.com",
		verifySignature: VerifySignature,
	}, nil
}

// CheckForUpdate queries GitHub for the latest release. Returns nil if
// already up-to-date, or a ReleaseInfo if a newer version exists.
func (u *Updater) CheckForUpdate(ctx context.Context) (*ReleaseInfo, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest",
		u.apiBaseURL, u.repoOwner, u.repoName)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Moombox/"+u.currentVersion)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to check for updates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("GitHub API rate limit exceeded (HTTP %d) — try again later", resp.StatusCode)
		}
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("failed to parse release: %w", err)
	}

	remoteVersion := strings.TrimPrefix(release.TagName, "v")
	if CompareVersions(remoteVersion, u.currentVersion) <= 0 {
		u.logger.Debug("[Updater] Already up to date",
			"current", u.currentVersion,
			"latest", remoteVersion,
		)
		return nil, nil
	}

	// Find the platform-appropriate binary and sig assets.
	assets, ok := currentPlatformAssets()
	if !ok {
		return nil, fmt.Errorf("auto-update unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	var downloadURL, signatureURL string
	for _, asset := range release.Assets {
		switch {
		case strings.EqualFold(asset.Name, assets.binary):
			downloadURL = asset.BrowserDownloadURL
		case strings.EqualFold(asset.Name, assets.sig):
			signatureURL = asset.BrowserDownloadURL
		}
	}
	if downloadURL == "" {
		return nil, fmt.Errorf("no %s asset found in release %s", assets.binary, release.TagName)
	}
	if signatureURL == "" {
		return nil, fmt.Errorf("no signature file found in release %s", release.TagName)
	}

	u.logger.Info("[Updater] Update available",
		"current", u.currentVersion,
		"latest", remoteVersion,
	)

	strippedBody := stripDownloadLinks(release.Body)
	return &ReleaseInfo{
		Version:          remoteVersion,
		TagName:          release.TagName,
		DownloadURL:      downloadURL,
		SignatureURL:     signatureURL,
		ReleaseNotes:     strippedBody,
		ReleaseNotesHtml: renderReleaseNotesHtml(strippedBody),
		PublishedAt:      release.PublishedAt,
	}, nil
}

// ApplyUpdate downloads the new binary and replaces the running executable.
// On Windows, the running exe is renamed to .old before the new one is placed.
//
// **Rename-window race**: between the os.Rename of the running exe to .old
// and the os.Rename of .new into place (~milliseconds), the original exe
// path does not exist. A concurrent process trying to launch Moombox during
// this window will fail. The caller MUST trigger a restart immediately after
// this returns nil — the launcher will pick up the freshly-renamed .exe and
// the running process exits cleanly. Audit reports/small-packages.md.
func (u *Updater) ApplyUpdate(ctx context.Context, release *ReleaseInfo) error {
	u.logger.Info("[Updater] Downloading update",
		"version", release.Version,
		"url", release.DownloadURL,
	)

	// Download to .new
	newPath := u.exePath + ".new"
	if err := u.downloadFile(ctx, release.DownloadURL, newPath); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("download failed: %w", err)
	}

	// Download and verify signature (mandatory — never apply unsigned binaries)
	if release.SignatureURL == "" {
		os.Remove(newPath)
		return fmt.Errorf("release has no signature URL — refusing to apply unsigned binary")
	}
	sigPath := newPath + ".sig"
	if err := u.downloadFile(ctx, release.SignatureURL, sigPath); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("signature download failed: %w", err)
	}

	if err := u.verifySignature(newPath, sigPath); err != nil {
		os.Remove(newPath)
		os.Remove(sigPath)
		return fmt.Errorf("signature verification failed: %w", err)
	}
	os.Remove(sigPath)
	u.logger.Info("[Updater] Signature verified", "version", release.Version)

	// Rename current exe to .old
	oldPath := u.exePath + ".old"
	os.Remove(oldPath) // remove stale .old if exists
	if err := os.Rename(u.exePath, oldPath); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("failed to rename current binary: %w", err)
	}

	// Rename .new to current
	if err := os.Rename(newPath, u.exePath); err != nil {
		// Attempt rollback
		u.logger.Error("[Updater] Failed to place new binary, rolling back",
			"error", err.Error(),
		)
		if rbErr := os.Rename(oldPath, u.exePath); rbErr != nil {
			// Both steps failed: the running binary no longer exists on disk
			// at its original path. Log very loudly so the user notices even
			// if the logger's file target is gone, and drop a marker file
			// next to the binary so the launcher / next run has a clear
			// breadcrumb for recovery.
			u.logger.Error("[Updater] Rollback also failed — binary may be missing",
				"original", u.exePath,
				"backup", oldPath,
				"staged", newPath,
				"placeError", err.Error(),
				"rollbackError", rbErr.Error(),
			)
			markerPath := u.exePath + ".update-broken"
			msg := fmt.Sprintf("Moombox update failed at %s\nplace error: %v\nrollback error: %v\nOriginal binary may be at %s and staged binary at %s — manual recovery required.\n",
				time.Now().UTC().Format(time.RFC3339), err, rbErr, oldPath, newPath)
			if mErr := os.WriteFile(markerPath, []byte(msg), 0o644); mErr != nil {
				u.logger.Error("[Updater] Failed to write broken-update marker",
					"path", markerPath,
					"error", mErr.Error(),
				)
			}
			return fmt.Errorf("failed to place new binary and rollback failed: place=%v rollback=%v", err, rbErr)
		}
		return fmt.Errorf("failed to place new binary: %w", err)
	}

	u.logger.Info("[Updater] Update applied successfully",
		"version", release.Version,
	)
	return nil
}

// VerifyCurrentSignature downloads the .sig for the current version from GitHub
// and verifies it against the running binary. Returns nil if the signature is valid.
func (u *Updater) VerifyCurrentSignature(ctx context.Context) error {
	tag := "v" + u.currentVersion

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/tags/%s",
		u.repoOwner, u.repoName, tag)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Moombox/"+u.currentVersion)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("no release found for %s (local/dev build?)", tag)
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("GitHub API rate limit exceeded (HTTP %d) — try again later", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return fmt.Errorf("failed to parse release: %w", err)
	}

	// Find the platform-appropriate sig asset.
	assets, ok := currentPlatformAssets()
	if !ok {
		return fmt.Errorf("signature verification unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	var signatureURL string
	for _, asset := range release.Assets {
		if strings.EqualFold(asset.Name, assets.sig) {
			signatureURL = asset.BrowserDownloadURL
			break
		}
	}
	if signatureURL == "" {
		return fmt.Errorf("no signature file in release %s (pre-signing release?)", tag)
	}

	// Download sig to temp file
	sigFile, err := os.CreateTemp("", "moombox-verify-*.sig")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	sigPath := sigFile.Name()
	sigFile.Close()
	defer os.Remove(sigPath)

	if err := u.downloadFile(ctx, signatureURL, sigPath); err != nil {
		return fmt.Errorf("signature download failed: %w", err)
	}

	if err := VerifySignature(u.exePath, sigPath); err != nil {
		return err
	}

	u.logger.Info("[Updater] Current binary signature verified", "version", u.currentVersion)
	return nil
}

// CleanupOldBinary removes stale files left over from previous updates:
// .old (previous binary), .new (interrupted download), .new.sig (interrupted
// verification), and .sig (VerifyCurrentSignature intermediate that may be
// left behind if ApplyUpdate was interrupted between its write and rename).
//
// On Windows, also sweeps `~` (orphaned by the launcher's deferred cleanup
// or by a prior installation that lacked the launcher startup sweep). The
// runtime.GOOS guard prevents accidentally targeting an editor backup file
// on Linux/macOS where `<name>~` is a legitimate file pattern.
//
// .update-broken markers from a failed double-rename rollback are
// intentionally NOT cleaned here — they are evidence for the user that
// manual recovery may be needed and should be deleted explicitly.
func (u *Updater) CleanupOldBinary() {
	suffixes := []string{".old", ".new", ".new.sig", ".sig"}
	if runtime.GOOS == "windows" {
		suffixes = append(suffixes, "~")
	}
	for _, suffix := range suffixes {
		path := u.exePath + suffix
		if _, err := os.Stat(path); err == nil {
			if err := os.Remove(path); err != nil {
				u.logger.Warn("[Updater] Failed to remove stale file",
					"path", path,
					"error", err.Error(),
				)
			} else {
				u.logger.Info("[Updater] Cleaned up stale file", "path", path)
			}
		}
	}
}

func (u *Updater) downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Moombox/"+u.currentVersion)

	// Use the package-level downloadClient (see its godoc for rationale).
	resp, err := downloadClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}

	// Cap download at 200 MB to prevent disk exhaustion from a compromised
	// source. Read one extra byte so a payload of *exactly* maxDownloadSize
	// is accepted while anything larger is rejected — without the +1 the
	// previous `n >= maxDownloadSize` check rejected the boundary case.
	// Audit reports/small-packages.md.
	const maxDownloadSize = 200 << 20
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxDownloadSize+1))
	if err != nil {
		f.Close()
		return err
	}
	if n > maxDownloadSize {
		f.Close()
		return fmt.Errorf("download exceeds %d MB size limit", maxDownloadSize>>20)
	}

	// Flush to disk before closing to prevent corruption on power loss
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}

	return f.Close()
}
