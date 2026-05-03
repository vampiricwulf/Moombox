package tui

import (
	"strings"
	"testing"
)

func TestLinuxFFmpegInstallSuggestionFromOSRelease(t *testing.T) {
	cases := []struct {
		name         string
		osRelease    string
		wantContains string
	}{
		{"ubuntu", `ID=ubuntu` + "\n" + `ID_LIKE=debian`, "apt install ffmpeg"},
		{"debian", `ID=debian`, "apt install ffmpeg"},
		{"fedora", `ID=fedora`, "dnf install ffmpeg"},
		{"arch", `ID=arch`, "pacman -S ffmpeg"},
		{"alpine", `ID=alpine`, "apk add ffmpeg"},
		{"opensuse-tumbleweed", `ID=opensuse-tumbleweed`, "zypper install ffmpeg"},

		// Derivative distros that miss on ID but hit on ID_LIKE — this
		// is the case the previous implementation got wrong (TUI fell
		// back to URL while web/routes correctly suggested apt).
		{"linuxmint", `ID=linuxmint` + "\n" + `ID_LIKE="ubuntu debian"`, "apt install ffmpeg"},
		{"pop_os", `ID=pop` + "\n" + `ID_LIKE="ubuntu debian"`, "apt install ffmpeg"},

		{"unknown", `ID=void`, "https://ffmpeg.org/download.html"},
		{"empty", ``, "https://ffmpeg.org/download.html"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := linuxFFmpegInstallSuggestionFromOSRelease(tc.osRelease)
			if !strings.Contains(got, tc.wantContains) {
				t.Errorf("for %s, got %q, want substring %q", tc.name, got, tc.wantContains)
			}
		})
	}
}
