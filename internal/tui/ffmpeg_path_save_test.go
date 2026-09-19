package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// newFfmpegSaveApp builds an App with the FFmpeg overlay open on the
// custom-path step and a live config store, the state the overlay is in when
// a validated path comes back from the checker.
func newFfmpegSaveApp(t *testing.T) *App {
	t.Helper()
	cfg := config.Defaults()
	a := NewApp()
	a.SetConfigStore(config.NewStore(cfg, ""))
	a.width, a.height = 120, 40
	a.ffmpegCheck.Open()
	a.ffmpegCheck.SetSize(120, 40)
	a.ffmpegCheck.mode = ffmpegCustom
	return a
}

// A refused FFmpeg-path save has to be readable WHERE THE USER IS LOOKING.
// The overlay is still up at that moment (the same handler sets
// successDismiss, i.e. "Press any key to continue") and App.View renders the
// overlay INSTEAD of the dashboard, so the dashboard's feedback line — which
// also self-clears after 3s — is never seen. The failure rides the overlay's
// own warning row instead (CORE-4, review I1).
//
// Mutant: reporting through a.setFeedback (the shape this replaced) — the
// rendered overlay no longer contains the reason.
func TestRefusedFfmpegPathSaveIsShownInTheOverlay(t *testing.T) {
	a := newFfmpegSaveApp(t)
	a.OnSaveConfig = func(*config.MoomboxConfig) error { return errors.New("diskfull") }

	a.Update(ffmpegCheckResultMsg{
		Valid:   true,
		Version: "7.1",
		Path:    "C:/tools/ffmpeg.exe",
		Warning: "ffmpeg 4.x is outdated",
	})

	// The field the overlay prints, exactly.
	if !strings.Contains(a.ffmpegCheck.warning, "diskfull") {
		t.Errorf("overlay warning = %q, want it to carry the save error", a.ffmpegCheck.warning)
	}
	if !strings.Contains(a.ffmpegCheck.warning, "outdated") {
		t.Errorf("overlay warning = %q, want the checker's own warning kept", a.ffmpegCheck.warning)
	}
	// And the same text actually rendered, through the view the user sees.
	// Single-word tokens only: the overlay box wraps at 60 columns.
	view := a.View().Content
	for _, want := range []string{"diskfull", "saved", "outdated"} {
		if !strings.Contains(view, want) {
			t.Errorf("the rendered overlay lacks %q:\n%s", want, view)
		}
	}
}

// The success path must not grow a phantom failure line.
//
// Mutant: appending the failure text unconditionally.
func TestSuccessfulFfmpegPathSaveLeavesTheWarningAlone(t *testing.T) {
	a := newFfmpegSaveApp(t)
	a.OnSaveConfig = func(*config.MoomboxConfig) error { return nil }

	a.Update(ffmpegCheckResultMsg{
		Valid:   true,
		Version: "7.1",
		Path:    "C:/tools/ffmpeg.exe",
		Warning: "ffmpeg 4.x is outdated",
	})

	if got := a.ffmpegCheck.warning; got != "ffmpeg 4.x is outdated" {
		t.Errorf("overlay warning = %q, want the checker's warning unchanged", got)
	}
}

// The validated path has to reach the muxing consumers whether or not the
// disk write succeeded. The overlay deliberately keeps a refused path LIVE
// (the user needs that binary this session — the premise of the owner's O-V
// ruling), and OnSaveConfig returns before its own hot-reload block on a save
// error, so this hook is the only thing that gets the path to the download
// worker and the trim service (review I2).
//
// Mutant: gating OnFfmpegPathChange on a nil save error — the failing row
// reports the empty path, i.e. the muxer keeps the old, missing binary for
// the rest of the session.
func TestFfmpegPathReachesTheMuxerOnBothSaveOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		saveErr error
	}{
		{"save succeeds", nil},
		{"save refused", errors.New("diskfull")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newFfmpegSaveApp(t)
			a.OnSaveConfig = func(*config.MoomboxConfig) error { return tc.saveErr }
			applied := ""
			calls := 0
			a.OnFfmpegPathChange = func(p string) { applied = p; calls++ }

			a.Update(ffmpegCheckResultMsg{Valid: true, Version: "7.1", Path: "C:/tools/ffmpeg.exe"})

			if applied != "C:/tools/ffmpeg.exe" {
				t.Errorf("OnFfmpegPathChange got %q after %d call(s), want the validated path", applied, calls)
			}
			// The live config keeps it too — that half was already true.
			if got := a.cfg.Paths.FfmpegPath; got != "C:/tools/ffmpeg.exe" {
				t.Errorf("cfg.Paths.FfmpegPath = %q, want the validated path", got)
			}
		})
	}
}

// Only a path the checker validated, on a step that asked for one, may be
// applied. The hook sits inside the same guard the save does.
//
// Mutants: hoisting the hook above the `msg.Valid` gate (the first row
// fires); hoisting it out of the mode / non-empty-path guard (the second
// row fires, and applying "" would drop the muxer back to whatever is on
// PATH — which is the state the overlay exists to fix).
func TestFfmpegPathIsAppliedOnlyWhenOneWasValidated(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode ffmpegMode
		msg  ffmpegCheckResultMsg
	}{
		{"invalid check", ffmpegCustom, ffmpegCheckResultMsg{Valid: false, Path: "C:/tools/ffmpeg.exe"}},
		{"valid install step with no path", ffmpegMain, ffmpegCheckResultMsg{Valid: true, Version: "7.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newFfmpegSaveApp(t)
			a.ffmpegCheck.mode = tc.mode
			calls := 0
			a.OnFfmpegPathChange = func(string) { calls++ }
			a.OnSaveConfig = func(*config.MoomboxConfig) error {
				t.Error("OnSaveConfig ran with no validated path to save")
				return nil
			}

			a.Update(tc.msg)

			if calls != 0 {
				t.Errorf("OnFfmpegPathChange called %d times, want 0", calls)
			}
		})
	}
}

// The overlay has to stay inside the smallest terminal the TUI renders at.
// A save error is an arbitrary string — a filesystem message naming a
// 300-character path is ordinary — and appended whole it grew the box from
// 20 rows to 25 at 60x20, pushing "Press any key to continue" below the
// frame. Capped, the reason still says what went wrong and the full text is
// in moombox.log (OnSaveConfig logs it).
//
// Mutant: drop the maxOverlayReasonRunes truncation — the rendered overlay is
// 25 rows and the height assertion fails.
func TestARefusedFfmpegSaveFitsTheSmallestTerminal(t *testing.T) {
	cfg := config.Defaults()
	a := NewApp()
	a.SetConfigStore(config.NewStore(cfg, ""))
	a.width, a.height = minTermWidth, minTermHeight
	a.recalcLayout()
	a.ffmpegCheck.Open()
	a.ffmpegCheck.SetSize(minTermWidth, minTermHeight)
	a.ffmpegCheck.mode = ffmpegCustom
	a.OnSaveConfig = func(*config.MoomboxConfig) error {
		return errors.New("open " + strings.Repeat("d", 300) + "/config.toml: permission denied")
	}

	a.Update(ffmpegCheckResultMsg{Valid: true, Version: "7.1", Path: "C:/tools/ffmpeg.exe"})

	if got := len([]rune(a.ffmpegCheck.warning)); got > maxOverlayReasonRunes+80 {
		t.Errorf("the overlay warning is %d runes — the appended reason is not capped", got)
	}
	view := stripANSI(a.View().Content)
	rows := strings.Count(view, "\n") + 1
	if rows > minTermHeight {
		t.Errorf("the overlay renders %d rows at %dx%d, want at most %d — the dismissal hint falls "+
			"below the frame:\n%s", rows, minTermWidth, minTermHeight, minTermHeight, view)
	}
	if !strings.Contains(view, "not saved") {
		t.Errorf("the capped overlay lost the failure line:\n%s", view)
	}
}
