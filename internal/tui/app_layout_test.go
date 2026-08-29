package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
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
//
// The two RecheckReport rows are Arc 8 Task 12b's ruling, and they are built
// from the function that emits them rather than typed out — the wording is
// shared with the dashboard and pinned there, so a reword must move this test
// with it instead of silently leaving the branch unexercised.
//
// The state is CONCLUSIVE: RecheckReport says "<platform> not authenticated"
// only for RefreshFailed, and "<platform> — could not establish" for the
// inconclusive verdict. R F's wording for the identical state ("...ran and auth
// verification failed") has always been red on the "failed" substring, so
// leaving R C's yellow made one surface answer one fact at two severities. Red
// is the actionable end — re-export the credentials — and yellow is reserved
// for a check that concluded nothing and asks for nothing.
//
// The mixed row is the precedence claim: a line naming one refused platform and
// one unreachable one is red, because the conclusive half is the half to act
// on. That is the same order the status-bar badge and the dashboard toast
// already apply.
func TestFeedbackColorErrorMessages(t *testing.T) {
	tests := []string{
		"Cancelled: User aborted",
		"Invalid Chord: A R Z",
		"Job download failed",
		"Save failed: disk full",
		cookies.RecheckReport(cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshFailed}),
		cookies.RecheckReport(
			cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshFailed},
			cookies.RecheckedPlatform{Label: "Twitch", Verdict: cookies.RefreshUnknown},
		),
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

// TestFeedbackColorWarningMessages covers every sub-condition in
// feedbackColor's warning branch, one row each. (It used to claim a count;
// the count was wrong before the branch grew, so it is stated as a
// correspondence instead — add a row whenever you add a condition.)
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
		"Browser cookie refresh declined to run (" + cookies.RefreshDeclinedCauses +
			") — nothing was learned about these cookies",
		"Browser cookie refresh ran but could not establish whether these cookies work",
		// The INCONCLUSIVE recheck, and the row that keeps the ruling above
		// from being "everything cookie-shaped is red". Built from the same
		// function as the two conclusive rows in the error table, so the two
		// halves of the split cannot drift apart.
		cookies.RecheckReport(cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshUnknown}),
		"Cookies: YouTube OK | Last cookie error: the browser profile held no credentials",
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

// feedbackSeverity ranks the colours the recheck line can take, so the
// invariant below can be stated as a comparison instead of a table.
func feedbackSeverity(t *testing.T, msg string) int {
	t.Helper()
	switch got := feedbackColor(msg); got {
	case ColorGreen:
		return 0
	case ColorYellow:
		return 1
	case ColorRed:
		return 2
	default:
		t.Fatalf("feedbackColor(%q) = %v, which is not a severity the recheck line can take", msg, got)
		return -1
	}
}

// TestLastCookieErrorNeverLowersSeverity is the property that makes appending
// AutoCookieStatus.LastError to the R C line safe.
//
// feedbackColor is a substring colorizer over the WHOLE line, so a suffix
// carrying somebody else's prose can move the colour. Only one direction is
// dangerous: a recorded failure that made the line LESS alarming would be the
// append actively hiding something. Raising it is at worst coarse — a recorded
// error whose own words say "failed" lands red, and a recorded verification
// failure is the actionable end of the scale by every other rule in this arc.
//
// The green case is why the marker exists at all rather than being left to
// chance: "Cookies: YouTube OK" is green, and a line that carries a recorded
// error must never be. Delete the "last cookie error" entry from feedbackColor
// and the first row below drops to green while announcing a failure.
func TestLastCookieErrorNeverLowersSeverity(t *testing.T) {
	verdicts := []string{
		cookies.RecheckReport(cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshOK}),
		cookies.RecheckReport(cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshUnknown}),
		cookies.RecheckReport(cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshFailed}),
	}
	recorded := []string{
		"the browser profile contained no cookies",
		"auth verification failed — manual re-login required",
		"refusing to overwrite cookies.txt",
	}

	for _, verdict := range verdicts {
		base := feedbackSeverity(t, verdict)
		for _, last := range recorded {
			line := verdict + " | Last cookie error: " + last
			got := feedbackSeverity(t, line)
			if got < base {
				t.Errorf("appending %q LOWERED the line's severity (%d → %d): %q. The append may "+
					"only ever raise it — a recorded failure that makes the line calmer is the "+
					"suffix hiding the thing it was added to show", last, base, got, line)
			}
			if got == 0 {
				t.Errorf("a line announcing a recorded cookie error renders as success: %q. That is "+
					"the one outcome that is simply wrong, whatever the verdict beside it says", line)
			}
		}
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
