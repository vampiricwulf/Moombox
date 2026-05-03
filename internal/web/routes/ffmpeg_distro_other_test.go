//go:build !windows

package routes

import (
	"strings"
	"testing"
)

func TestSuggestFFmpegInstallByOSRelease(t *testing.T) {
	cases := []struct {
		name         string
		osRelease    string
		wantContains string
	}{
		{"ubuntu", "ID=ubuntu\nID_LIKE=debian", "apt install ffmpeg"},
		{"debian", "ID=debian", "apt install ffmpeg"},
		{"fedora", "ID=fedora", "dnf install ffmpeg"},
		{"arch", "ID=arch", "pacman -S ffmpeg"},
		{"alpine", "ID=alpine", "apk add ffmpeg"},
		{"unknown", "ID=void", "https://ffmpeg.org/download.html"},
		{"empty", ``, "https://ffmpeg.org/download.html"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := suggestFFmpegInstallFromOSRelease(tc.osRelease)
			if !strings.Contains(got, tc.wantContains) {
				t.Errorf("for %s, got %q, want substring %q", tc.name, got, tc.wantContains)
			}
		})
	}
}
