package tui

import "testing"

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
