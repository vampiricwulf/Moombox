// Package updater provides auto-update checking and self-replacement for Moombox.
package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// ReleaseInfo holds information about an available update.
type ReleaseInfo struct {
	Version      string `json:"version"`      // "2.0.16" (stripped "v" prefix)
	TagName      string `json:"tagName"`      // "v2.0.16"
	DownloadURL  string `json:"downloadUrl"`  // asset browser_download_url for Moombox.exe
	ReleaseNotes string `json:"releaseNotes"` // body from GitHub release
	PublishedAt  string `json:"publishedAt"`
}

// Updater checks for and applies updates from GitHub Releases.
type Updater struct {
	currentVersion string
	exePath        string
	logger         logger
	repoOwner      string
	repoName       string
	client         *http.Client
}

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
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

// CheckForUpdate queries GitHub for the latest release. Returns nil if
// already up-to-date, or a ReleaseInfo if a newer version exists.
func (u *Updater) CheckForUpdate(ctx context.Context) (*ReleaseInfo, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest",
		u.repoOwner, u.repoName)

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

	// Find the Moombox.exe asset
	var downloadURL string
	for _, asset := range release.Assets {
		if strings.EqualFold(asset.Name, "Moombox.exe") {
			downloadURL = asset.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" {
		return nil, fmt.Errorf("no Moombox.exe asset found in release %s", release.TagName)
	}

	u.logger.Info("[Updater] Update available",
		"current", u.currentVersion,
		"latest", remoteVersion,
	)

	return &ReleaseInfo{
		Version:      remoteVersion,
		TagName:      release.TagName,
		DownloadURL:  downloadURL,
		ReleaseNotes: release.Body,
		PublishedAt:  release.PublishedAt,
	}, nil
}

// ApplyUpdate downloads the new binary and replaces the running executable.
// On Windows, the running exe is renamed to .old before the new one is placed.
// The caller should trigger a restart after this returns nil.
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
			u.logger.Error("[Updater] Rollback also failed",
				"error", rbErr.Error(),
			)
		}
		return fmt.Errorf("failed to place new binary: %w", err)
	}

	u.logger.Info("[Updater] Update applied successfully",
		"version", release.Version,
	)
	return nil
}

// CleanupOldBinary removes .old and .exe~ binaries left over from previous
// updates. The .old file is the standard leftover; .exe~ is the launcher's
// renamed copy (Windows locks running executables, so the launcher renames
// .old → .exe~ to free the name for future updates).
func (u *Updater) CleanupOldBinary() {
	for _, suffix := range []string{".old", "~"} {
		path := u.exePath + suffix
		if _, err := os.Stat(path); err == nil {
			if err := os.Remove(path); err != nil {
				u.logger.Warn("[Updater] Failed to remove old binary",
					"path", path,
					"error", err.Error(),
				)
			} else {
				u.logger.Info("[Updater] Cleaned up old binary", "path", path)
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

	// Use a separate client with a generous timeout for binary downloads.
	// The main u.client has a 10s timeout suited for API calls, but binary
	// downloads (10-30MB) need much longer.
	dlClient := &http.Client{Timeout: 5 * time.Minute}
	resp, err := dlClient.Do(req)
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

	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}

	return f.Close()
}
