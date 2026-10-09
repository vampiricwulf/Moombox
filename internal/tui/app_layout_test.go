package tui

import (
	"fmt"
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
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
		"Extras: Y yt-dlp | L Login | I Import Cookies | T Stats (3s)",
		"Quit: Confirm with Q",
	}
	for _, msg := range tests {
		t.Run(msg, func(t *testing.T) {
			if got := feedbackColor(msg, severityUnstated); got != ColorYellow {
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
			if got := feedbackColor(msg, severityUnstated); got != ColorRed {
				t.Errorf("feedbackColor(%q) = %v, want ColorRed", msg, got)
			}
		})
	}
}

// TestFeedbackColorDeletionGray covers the deletion-gray branch.
func TestFeedbackColorDeletionGray(t *testing.T) {
	if got := feedbackColor("Deleted: 3 jobs", severityUnstated); got != ColorGray {
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
			if got := feedbackColor(msg, severityUnstated); got != ColorYellow {
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
			if got := feedbackColor(msg, severityUnstated); got != ColorGreen {
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
	if got := feedbackColor("Action: download failed", severityUnstated); got != ColorYellow {
		t.Errorf("Action prefix should beat 'failed' substring: got %v", got)
	}
	// "Cancelled:" prefix wins over the warning prefix later.
	if got := feedbackColor("Cancelled: user aborted", severityUnstated); got != ColorRed {
		t.Errorf("Cancelled prefix should be red: got %v", got)
	}
}

// renderedRank ranks a rendered colour by how much alarm it raises, so the
// invariant below can be stated as a comparison.
//
// GRAY IS RANKED WITH GREEN, and that is the point of the ranking rather than a
// shortcut. The previous version of this helper called t.Fatal on gray, which
// meant the test could not express the very lowering it forbids: a line whose
// appended error happened to contain "deleted:" would fall to the neutral
// branch and the assertion would abort instead of failing. Green and gray are
// the two colours an operator reads as "nothing here needs me", which is the
// only distinction this property is about.
func renderedRank(t *testing.T, c color.Color) int {
	t.Helper()
	switch c {
	case ColorGreen, ColorGray:
		return 0
	case ColorYellow:
		return 1
	case ColorRed:
		return 2
	default:
		t.Fatalf("feedback rendered in %v, which is not a colour this line can take", c)
		return -1
	}
}

// recheckColor renders one R C result AS THE OPERATOR SEES IT: composed at the
// terminal's width and then coloured by exactly the expression View uses.
//
// The width was the whole point. The previous version of this test fed
// feedbackColor unclamped strings, which was the one domain where the property
// held — fitFeedback ran inside cookieRecheckFeedback and feedbackColor ran on
// what survived it, so a 40-column terminal truncated the marker away and the
// line rendered green while announcing a recorded failure. The line wraps now
// rather than being cut, but going through the real Update path is still what
// makes the assertion about what is displayed.
func recheckColor(t *testing.T, width int, msg cookieRecheckResultMsg) (string, color.Color) {
	t.Helper()
	app := NewApp()
	app.width = width
	app.statusBar.SetActivePlatforms(true, false)
	app.Update(msg)
	return app.feedback.msg, feedbackColor(app.feedback.msg, app.feedback.sev)
}

// TestLastCookieErrorNeverLowersSeverity is the property that makes appending
// AutoCookieStatus.LastError to the R C line safe, asserted at the widths that
// broke it.
//
// TWO DEFECTS, one root: severity was being re-derived from prose by a reader
// standing downstream of the clamp and of the branch order.
//
//   - THE CLAMP. cookieRecheckFeedback appended the clause and then truncated
//     the line to the terminal width; feedbackColor then read the truncated
//     line. At 40 columns "…| Last cookie error: the browser…" arrived as
//     "…| Last cookie err…", the marker was gone, and the line fell through to
//     the SUCCESS colour. An operator in a split pane pressed R C, was told
//     their cookies were fine, and the browser refresh had been failing for
//     days. The line wraps now (setWrappedFeedback) and is no longer cut
//     before the colour is chosen; the widths stay, as the guard against a
//     clamp coming back.
//   - THE BRANCH ORDER. The gray "deleted:" branch sits above the warning
//     branch, so a recorded error whose words contained it would render
//     NEUTRAL. No setError composes that word today — this is the row that
//     stops it becoming reachable, and the row the old helper could not even
//     express, because it aborted on gray.
//
// Both close the same way: cookieRecheckFeedback states the severity from the
// facts it holds, and feedbackColor obeys a stated severity over its own scan.
// The widths below straddle the old truncation point (~42 columns) on purpose;
// 0 is the unclamped case, which is the domain the old test lived in.
func TestLastCookieErrorNeverLowersSeverity(t *testing.T) {
	verdicts := []struct {
		name    string
		verdict cookies.RefreshVerdict
	}{
		{"healthy", cookies.RefreshOK},
		{"inconclusive", cookies.RefreshUnknown},
		{"conclusively refused", cookies.RefreshFailed},
	}
	recorded := []struct {
		name string
		text string
	}{
		{"empty profile", "the browser profile contained no cookies"},
		{"says failed", "auth verification failed — manual re-login required"},
		{"says refusing", "refusing to overwrite cookies.txt"},
		// FINDING 2. Reachable only if a future setError composes the word;
		// with the fact stated it cannot lower anything, and with severity
		// re-derived from text it renders gray — neutral — over a recorded
		// failure.
		{"says deleted:", "staging deleted: could not restore the previous cookies.txt"},
	}

	for _, width := range []int{0, 30, 40, 50, 200} {
		for _, v := range verdicts {
			base := renderedRank(t, mustColor(t, width, v.verdict, ""))
			for _, r := range recorded {
				t.Run(fmt.Sprintf("w%d/%s/%s", width, v.name, r.name), func(t *testing.T) {
					line, c := recheckColor(t, width, cookieRecheckResultMsg{
						YouTube: v.verdict, LastError: r.text,
					})
					got := renderedRank(t, c)
					if got == 0 {
						t.Errorf("at width %d a line announcing a recorded cookie error renders as "+
							"%v — a colour that reads \"nothing here needs you\": %q\n\n"+
							"This is the outcome the whole clause exists to prevent, and it is what "+
							"deriving severity from the CLAMPED text produces: the marker is at the "+
							"end of the line and the truncation takes it first", width, c, line)
					}
					if got < base {
						t.Errorf("at width %d appending %q LOWERED the line's severity (%d → %d): "+
							"%q. The append may only ever raise it", width, r.text, base, got, line)
					}
				})
			}
		}
	}
}

// mustColor is recheckColor's colour for a verdict with no recorded error — the
// baseline each row above is compared against.
func mustColor(t *testing.T, width int, verdict cookies.RefreshVerdict, lastError string) color.Color {
	t.Helper()
	_, c := recheckColor(t, width, cookieRecheckResultMsg{YouTube: verdict, LastError: lastError})
	return c
}

// TestRecheckColourSurvivesTheClampUnchanged is the premise the table above
// rests on, stated separately because a property that holds at every width is
// also satisfied by a colour that ignores the line entirely.
//
// The same result must render the same colour whatever the terminal is doing.
// The message may be laid out differently — wrapped, cut at the row cap — but
// what it MEANS does not change with the width of the pane it is displayed in,
// and any colour that varies with the width is deriving severity from the
// wrong thing.
func TestRecheckColourSurvivesTheClampUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  cookieRecheckResultMsg
		want color.Color
	}{
		{"healthy, nothing recorded", cookieRecheckResultMsg{YouTube: cookies.RefreshOK}, ColorGreen},
		{"healthy, error recorded", cookieRecheckResultMsg{
			YouTube: cookies.RefreshOK, LastError: "the browser profile contained no cookies",
		}, ColorYellow},
		{"inconclusive", cookieRecheckResultMsg{YouTube: cookies.RefreshUnknown}, ColorYellow},
		{"conclusively refused", cookieRecheckResultMsg{YouTube: cookies.RefreshFailed}, ColorRed},
		{"refused, and an error recorded too", cookieRecheckResultMsg{
			YouTube: cookies.RefreshFailed, LastError: "refusing to overwrite cookies.txt",
		}, ColorRed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, width := range []int{0, 30, 40, 50, 200} {
				line, c := recheckColor(t, width, tc.msg)
				if c != tc.want {
					t.Errorf("at width %d the colour is %v, want %v: %q", width, c, tc.want, line)
				}
			}
		})
	}
}

// TestRecheckWithNoPlatformsIsAnAdvisory covers the arm the verdict loop cannot
// reach.
//
// With neither platform active RecheckReport emits "Cookies: no platforms
// configured" and there is no verdict to raise the severity off — so a stated
// severity computed from verdicts alone would report success and render the
// line GREEN, where the fallback scan's "no platforms" entry has always
// rendered it yellow. Stating a severity must not silently change a colour the
// scan already got right.
func TestRecheckWithNoPlatformsIsAnAdvisory(t *testing.T) {
	app := NewApp()
	app.width = 200
	app.statusBar.SetActivePlatforms(false, false)
	app.Update(cookieRecheckResultMsg{})
	if got := feedbackColor(app.feedback.msg, app.feedback.sev); got != ColorYellow {
		t.Errorf("%q rendered %v, want ColorYellow — it is an advisory about an unconfigured "+
			"install, and it was yellow before any severity was stated", app.feedback.msg, got)
	}
}

// TestStatedSeverityDoesNotLeakToTheNextMessage pins the field invariant
// feedbackSev's doc comment claims.
//
// The severity lives in the same struct as the message, so a setter cannot
// write one without the other — this is what says the two setters still
// replace the whole value rather than patching a field, so a setter that
// wrote the message and left the severity alone would colour the NEXT line by a
// fact about the previous one — a green "Saved" rendered red because an R C
// three seconds earlier found dead credentials. setFeedback and
// setFeedbackWithDuration both reset it, and this is what says so.
func TestStatedSeverityDoesNotLeakToTheNextMessage(t *testing.T) {
	app := NewApp()
	app.width = 200
	app.statusBar.SetActivePlatforms(true, false)
	app.Update(cookieRecheckResultMsg{YouTube: cookies.RefreshFailed})
	if got := feedbackColor(app.feedback.msg, app.feedback.sev); got != ColorRed {
		t.Fatalf("premise lost: the conclusive recheck no longer renders red (%v)", got)
	}

	for _, set := range []struct {
		name string
		call func()
	}{
		{"setFeedback", func() { app.setFeedback("Saved") }},
		{"setFeedbackWithDuration", func() { app.setFeedbackWithDuration("Saved", time.Second) }},
	} {
		t.Run(set.name, func(t *testing.T) {
			app.Update(cookieRecheckResultMsg{YouTube: cookies.RefreshFailed})
			set.call()
			if got := feedbackColor(app.feedback.msg, app.feedback.sev); got != ColorGreen {
				t.Errorf("%q rendered %v after a stated-severity line — the fact outlived the "+
					"message it was stated for", app.feedback.msg, got)
			}
		})
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

// TestViewWarnsBelowMinimumTerminalSize pins the minimum-size guard: below
// minTermWidth x minTermHeight, every panel/overlay renders garbage (negative
// widths, wrapped borders), so View() must short-circuit to a single-line
// warning instead — the same shape as the "Initializing..." guard it sits
// beside, one check further down.
func TestViewWarnsBelowMinimumTerminalSize(t *testing.T) {
	tests := []struct {
		name        string
		w, h        int
		wantTooSmal bool
	}{
		{"narrow width", minTermWidth - 1, minTermHeight + 5, true},
		{"short height", minTermWidth + 5, minTermHeight - 1, true},
		{"both under", 50, 10, true},
		{"exactly at minimum", minTermWidth, minTermHeight, false},
		{"comfortably sized", 120, 40, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := NewApp()
			app.width = tt.w
			app.height = tt.h
			view := app.View().Content
			got := strings.Contains(view, "Terminal too small")
			if got != tt.wantTooSmal {
				t.Errorf("at %dx%d: Contains(view, \"Terminal too small\") = %v, want %v\nview:\n%s",
					tt.w, tt.h, got, tt.wantTooSmal, view)
			}
		})
	}
}

// TestViewTerminalTooSmallNamesTheSize pins the message content: the current
// and required dimensions, so an operator resizing a pane knows how far to
// go rather than guessing.
func TestViewTerminalTooSmallNamesTheSize(t *testing.T) {
	app := NewApp()
	app.width = 50
	app.height = 10
	view := app.View().Content
	for _, want := range []string{"50", "10", fmt.Sprintf("%d", minTermWidth), fmt.Sprintf("%d", minTermHeight)} {
		if !strings.Contains(view, want) {
			t.Errorf("terminal-too-small view lacks %q:\n%s", want, view)
		}
	}

	// It must also FIT: shown only in terminals narrower than minTermWidth,
	// the message is no use if the required size is past the right edge.
	app.width = 40
	for _, line := range strings.Split(app.View().Content, "\n") {
		if w := runewidth.StringWidth(line); w > app.width {
			t.Errorf("line %q is %d columns, wider than the %d-column terminal it is shown in", line, w, app.width)
		}
	}
}

// TestBothBannersAndLogsFocusFitTheFloorTerminal: with the restart and
// security banners up, Logs focused gave the top row 25% of what was left —
// 3 rows at 20 — while each top panel still draws 4, so the frame came out a
// row taller than the terminal and bubbletea dropped the top line.
//
// Mutant: dropping the minPanelH clamp — the frame is 21 rows.
func TestBothBannersAndLogsFocusFitTheFloorTerminal(t *testing.T) {
	for _, w := range []int{60, 80} {
		for _, focus := range []FocusPanel{PanelLogs, PanelTasks} {
			cfg := config.Defaults()
			cfg.Network.NetworkAccess = "external"
			app := NewApp()
			app.configStore = config.NewStore(cfg, "")
			app.restartPending = true
			app.width, app.height = w, 20
			app.focusedPanel = focus
			app.recalcLayout()
			if n := strings.Count(app.View().Content, "\n") + 1; n > 20 {
				t.Errorf("%dx20, focus %v: frame is %d rows", w, focus, n)
			}
		}
	}
}

// TestFeedbackLineIsCutToTheWidth: only the R C line was clamped; any other
// long one — "Deleted: <a 128-character title>" — ran past the terminal and
// bubbletea clipped it, losing the end with no sign it was cut. The row is
// cut at render with an ellipsis, so it stays within the width.
//
// Mutant: dropping the truncation in addOverlayMessage — the row is wider
// than the terminal.
func TestFeedbackLineIsCutToTheWidth(t *testing.T) {
	app := NewApp()
	app.width, app.height = 80, 24
	app.recalcLayout()
	app.setFeedback("Deleted: " + strings.Repeat("a very long stream title ", 6) + "(3s)")
	for _, line := range strings.Split(app.View().Content, "\n") {
		if w := ansi.StringWidth(line); w > 80 {
			t.Fatalf("a %d-cell line on an 80-column terminal: %q", w, stripANSI(line))
		}
	}
	if !strings.Contains(stripANSI(app.View().Content), "Deleted: a very long stream title") ||
		!strings.Contains(stripANSI(app.View().Content), "…") {
		t.Error("the long feedback line is not shown cut with an ellipsis")
	}
}

// TestShiftTabCyclesFocusBackwards: the spec has always listed Shift-Tab
// beside Tab, and the key was a silent no-op.
//
// Mutant: dropping the keyShiftTab case — focus stays put.
func TestShiftTabCyclesFocusBackwards(t *testing.T) {
	app := NewApp()
	app.width, app.height = 100, 30
	app.recalcLayout()
	for _, want := range []FocusPanel{PanelLogs, PanelDetails, PanelTasks} {
		app.handleKey(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
		if app.focusedPanel != want {
			t.Fatalf("Shift-Tab focused %v, want %v", app.focusedPanel, want)
		}
	}
}

// TestTUIDropsAJobUpdateOlderThanOneApplied: writers notify after releasing
// the database lock, so a job's updates can arrive out of write order, and
// the older one, applied last, put a stale row (Downloading during a mux)
// back on the task list. The newest version wins — except that a JobAdded is
// never dropped, so one overtaken by its job's first update is still added.
//
// Mutants: dropping the stale check — the list shows Downloading; dropping a
// stale JobAdded — the overtaken add is lost.
func TestTUIDropsAJobUpdateOlderThanOneApplied(t *testing.T) {
	app := NewApp()
	app.width, app.height = 100, 30
	app.recalcLayout()
	row := func(id string, st database.JobStatus, v uint64) *database.Job {
		return &database.Job{ID: id, VideoID: id, Title: id, Platform: "youtube", Status: st, Version: v}
	}
	app.handleJobAdded(&database.JobAdded{Job: row("j", database.StatusUpcoming, 1)})
	app.handleJobUpdate(&database.JobChange{Job: row("j", database.StatusMuxing, 7), Changes: []string{"status"}})
	app.handleJobUpdate(&database.JobChange{Job: row("j", database.StatusDownloading, 6), Changes: []string{"status"}})
	if got := app.taskList.GetJobByID("j"); got == nil || got.Status != database.StatusMuxing {
		t.Errorf("task list holds %v, want the newer Muxing row", got)
	}

	app.handleJobUpdate(&database.JobChange{Job: row("k", database.StatusDownloading, 9), Changes: []string{"status"}})
	app.handleJobAdded(&database.JobAdded{Job: row("k", database.StatusUpcoming, 8)})
	if app.taskList.GetJobByID("k") == nil {
		t.Error("a JobAdded overtaken by the job's first update was never added")
	}
}
