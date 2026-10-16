package tui

import (
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestFFmpegVerifyBlocksInput: after a successful install the overlay shows
// "Verifying installation..." while it checks the result, but the success arm
// cleared installing without setting checking, so HandleKey was open: a second
// install could be queued, or Esc Esc quit, while the check ran.
//
// Mutant: beginVerify not setting checking — the key reaches the menu.
func TestFFmpegVerifyBlocksInput(t *testing.T) {
	a := NewApp()
	a.width, a.height = 100, 40
	a.ffmpegCheck.SetSize(100, 40)
	a.ffmpegCheck.Open()
	a.ffmpegCheck.installing = true

	a.Update(ffmpegConfirmResultMsg{})
	if !a.ffmpegCheck.checking {
		t.Fatal("the verify phase does not block input")
	}
	if got := a.ffmpegCheck.HandleKey(keyEsc); got != "" {
		t.Errorf("Esc during the verify = %q, want it ignored", got)
	}
	a.Update(ffmpegCheckResultMsg{Valid: false})
	if a.ffmpegCheck.checking {
		t.Error("the verify's answer did not release input")
	}
}

// TestFFmpegPathPromptsTakePastedQuotesOff: Explorer's "Copy as path" is the
// natural way to get ffmpeg.exe's path into the overlay's custom-path and
// manual-install prompts, as it is into E I's, and both sent the quoted form
// to the checker, which ran it as typed and reported the path not valid. The
// path checked — and saved, from the check's answer — is the one inside one
// pair of surrounding quotes, trimmed; two quotes alone check nothing.
//
// Mutants it kills: either Enter handler sending the raw value (its quoted
// rows keep their quotes); pastedPath without strings.TrimSpace (the row with
// a trailing space keeps its quotes); checking the raw value for emptiness
// (`""` checks an empty path).
func TestFFmpegPathPromptsTakePastedQuotesOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ffmpeg", "bin", "ffmpeg.exe")
	for _, mode := range []string{"custom", "manual"} {
		for typed, want := range map[string]string{
			`"` + path + `"`:  "check_custom:" + path,
			`'` + path + `'`:  "check_custom:" + path,
			`"` + path + `" `: "check_custom:" + path,
			path:              "check_custom:" + path,
			`""`:              "",
		} {
			m := NewFFmpegCheckModel()
			m.Open()
			m.SetSize(120, 40)
			if mode == "custom" {
				m.mode = ffmpegCustom
				m.textInput.Focus()
			} else {
				m.ShowManual()
			}
			for _, r := range typed {
				msg := tea.KeyPressMsg{Code: r, Text: string(r)}
				m.UpdateComponents(msg)
				m.HandleKey(msg.String())
			}
			if got := m.HandleKey(keyEnter); got != want {
				t.Errorf("%s prompt, typed %q: Enter = %q, want %q", mode, typed, got, want)
			}
		}
	}
}
