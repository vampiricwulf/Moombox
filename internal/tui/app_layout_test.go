package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestFeedbackColorChordMessages exercises the chord-prefix branch of
// the color-routing decision tree. Per audit reports/tui.md F18 (and
// the coverage gap in cross-cutting C11), the routing is pure logic
// that must stay covered as new prefixes are added.
func TestFeedbackColorChordMessages(t *testing.T) {
	tests := []string{
		"Press A for Action menu",
		"Action: Resume Job",
		"Request: Update Channel",
		"Open: Stream URL",
		"Quit: Confirm with Q",
	}
	for _, msg := range tests {
		t.Run(msg, func(t *testing.T) {
			if got := feedbackColor(msg); got != ColorYellow {
				t.Errorf("feedbackColor(%q) = %v, want ColorYellow", msg, got)
			}
		})
	}
}

// TestFeedbackColorErrorMessages covers the red-error branch.
func TestFeedbackColorErrorMessages(t *testing.T) {
	tests := []string{
		"Cancelled: User aborted",
		"Invalid Chord: A R Z",
		"Job download failed",
		"Save failed: disk full",
	}
	for _, msg := range tests {
		t.Run(msg, func(t *testing.T) {
			if got := feedbackColor(msg); got != ColorRed {
				t.Errorf("feedbackColor(%q) = %v, want ColorRed", msg, got)
			}
		})
	}
}

// TestFeedbackColorDeletionGray covers the deletion-gray branch.
func TestFeedbackColorDeletionGray(t *testing.T) {
	if got := feedbackColor("Deleted: 3 jobs"); got != ColorGray {
		t.Errorf("feedbackColor deletion: want gray, got %v", got)
	}
}

// TestFeedbackColorWarningMessages covers all 9 warning sub-prefixes.
// Each is a real string emitted by the TUI today; if any prefix is
// renamed without updating feedbackColor, this test catches the drift.
//
// Note: messages containing the substring "failed" route to error
// (red) because the error branch precedes the warning branch in the
// routing tree — that's intentional. The test intentionally avoids
// such mixed messages so the warning branch is unambiguously
// exercised.
func TestFeedbackColorWarningMessages(t *testing.T) {
	tests := []string{
		"Can only retry crashed jobs",
		"Trim only available on finished jobs",
		"No update available",
		"No stream selected",
		"A trim is already in progress",
		"Auth: no cookies acquired",
		"YouTube: not authenticated",
		"No platforms configured",
		"Already up to date",
		"Channel already exists in config",
	}
	for _, msg := range tests {
		t.Run(msg, func(t *testing.T) {
			if got := feedbackColor(msg); got != ColorYellow {
				t.Errorf("feedbackColor(%q): want yellow, got %v", msg, got)
			}
		})
	}
}

// TestFeedbackColorDefaultGreen covers the catch-all success branch.
func TestFeedbackColorDefaultGreen(t *testing.T) {
	tests := []string{
		"Saved",
		"Channel added",
		"Job restarted",
		"",
	}
	for _, msg := range tests {
		t.Run("["+msg+"]", func(t *testing.T) {
			if got := feedbackColor(msg); got != ColorGreen {
				t.Errorf("feedbackColor(%q): want green, got %v", msg, got)
			}
		})
	}
}

// TestFeedbackColorPriorityOrder locks the specific ordering documented
// in the function godoc: chord > error > deletion > warning > success.
// "Cancelled: failed" matches both error prefixes and would otherwise be
// ambiguous; the chord-style "Action: failed" should still resolve to
// chord (yellow) because chord prefixes are matched first.
func TestFeedbackColorPriorityOrder(t *testing.T) {
	// "Action:" prefix wins over the "failed" substring → yellow.
	if got := feedbackColor("Action: download failed"); got != ColorYellow {
		t.Errorf("Action prefix should beat 'failed' substring: got %v", got)
	}
	// "Cancelled:" prefix wins over the warning prefix later.
	if got := feedbackColor("Cancelled: user aborted"); got != ColorRed {
		t.Errorf("Cancelled prefix should be red: got %v", got)
	}
}

// TestSecurityBannerText locks the single warned state: external/public
// network access with no dashboard password. Every interactive surface
// refuses to SET that combination, so the banner exists only for a
// hand-edited config file (block-set / warn-boot policy).
func TestSecurityBannerText(t *testing.T) {
	tests := []struct {
		name     string
		access   string
		hash     string
		wantWarn bool
	}{
		{"external no password warns", "external", "", true},
		{"public no password warns", "public", "", true},
		{"external with password silent", "external", "scrypt:salt:hash", false},
		{"lan no password silent", "lan", "", false},
		{"localhost silent", "localhost", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Network.NetworkAccess = tt.access
			cfg.Network.PasswordHash = tt.hash
			a := &App{configStore: config.NewStore(cfg, "")}
			got := a.securityBannerText()
			if (got != "") != tt.wantWarn {
				t.Errorf("securityBannerText() = %q, wantWarn=%v", got, tt.wantWarn)
			}
		})
	}
	// Nil store (tests / early init) must not panic.
	if got := (&App{}).securityBannerText(); got != "" {
		t.Errorf("nil store: got %q, want empty", got)
	}
}
