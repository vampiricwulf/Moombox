//go:build !windows

package routes

import (
	"os"
	"strings"
	"sync"
)

// suggestFFmpegInstall returns a copy-pasteable shell command (or a
// fallback URL) for installing FFmpeg on the user's Linux distro.
// Reads /etc/os-release once at first call; the result is cached for
// process lifetime via sync.Once.
func suggestFFmpegInstall() string {
	suggestFFmpegInstallOnce.Do(func() {
		data, _ := os.ReadFile("/etc/os-release")
		suggestFFmpegInstallCache = suggestFFmpegInstallFromOSRelease(string(data))
	})
	return suggestFFmpegInstallCache
}

var (
	suggestFFmpegInstallOnce  sync.Once
	suggestFFmpegInstallCache string
)

// suggestFFmpegInstallFromOSRelease is the testable seam — pure function
// over the contents of /etc/os-release.
func suggestFFmpegInstallFromOSRelease(osRelease string) string {
	id, idLike := parseOSReleaseIDs(osRelease)
	candidates := append([]string{id}, strings.Fields(idLike)...)
	for _, c := range candidates {
		switch strings.TrimSpace(c) {
		case "debian", "ubuntu":
			return "sudo apt install ffmpeg"
		case "fedora", "rhel", "centos":
			return "sudo dnf install ffmpeg"
		case "arch", "manjaro":
			return "sudo pacman -S ffmpeg"
		case "alpine":
			return "sudo apk add ffmpeg"
		case "opensuse", "opensuse-tumbleweed", "opensuse-leap", "suse":
			return "sudo zypper install ffmpeg"
		}
	}
	return "Visit https://ffmpeg.org/download.html for installation instructions"
}

// parseOSReleaseIDs extracts ID= and ID_LIKE= from /etc/os-release contents.
func parseOSReleaseIDs(content string) (id, idLike string) {
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "ID="):
			id = strings.Trim(strings.TrimPrefix(line, "ID="), `"`)
		case strings.HasPrefix(line, "ID_LIKE="):
			idLike = strings.Trim(strings.TrimPrefix(line, "ID_LIKE="), `"`)
		}
	}
	return
}
