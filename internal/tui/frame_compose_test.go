package tui

import (
	"fmt"
	"strings"
	"testing"
	"unsafe"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// referenceFrame is App.View's panel path as it was written before the
// composer: lipgloss joins on every call, nothing remembered. Every test here
// compares the frame View returns with it.
func referenceFrame(a *App) string {
	a.statusBar.ShowChordHint = !a.seenChordHint
	a.statusBar.SelectedCount = a.taskList.SelectedCount()
	topRow := lipgloss.JoinHorizontal(lipgloss.Top, a.taskList.View(), a.details.View())
	mainParts := []string{}
	if a.restartPending {
		mainParts = append(mainParts, restartBanner(a.width))
	}
	if warn := a.securityBannerText(); warn != "" {
		mainParts = append(mainParts, securityBanner(a.width, warn))
	}
	mainParts = append(mainParts, topRow, a.logs.View(), a.statusBar.View())
	content := lipgloss.JoinVertical(lipgloss.Left, mainParts...)
	if a.feedback.msg != "" {
		style := lipgloss.NewStyle().Foreground(feedbackColor(a.feedback.msg, a.feedback.sev))
		rows := []string{a.feedback.msg}
		if a.feedback.wrap {
			rows = wrapFeedback(a.feedback.msg, a.width, feedbackRowCap(a.height))
		}
		for i := range rows {
			rows[i] = style.Render(rows[i])
		}
		content = addOverlayMessage(content, a.width, rows...)
	}
	return content
}

// composeTestApp is a sized App with rows that stress width measuring: wide
// CJK, emoji with joiners and variation selectors, combining marks, and a
// title long enough to be cut.
func composeTestApp(t testing.TB, w, h int, restart, security bool, focus FocusPanel) *App {
	t.Helper()
	a := NewApp()
	cfg := config.Defaults()
	if security {
		cfg.Network.NetworkAccess = "external"
	}
	a.configStore = config.NewStore(cfg, "")
	a.restartPending = restart
	a.width, a.height = w, h
	a.focusedPanel = focus
	a.recalcLayout()

	titles := []string{
		"plain ASCII title",
		"【歌枠】懐かしい曲を歌います！初見さん大歓迎【ホロライブ】",
		"emoji 👨‍👩‍👧‍👦 family ❤️ and flags 🇯🇵🇺🇸 in a title",
		"combining é ä and a very long tail " + strings.Repeat("x", 150),
		"한국어 제목 테스트 방송",
		"tab\there and trailing space ",
	}
	statuses := []database.JobStatus{
		database.StatusDownloading, database.StatusLive, database.StatusUpcoming,
		database.StatusFinished, database.StatusError, database.StatusMuxing,
	}
	jobs := make([]*database.Job, 0, len(titles))
	for i, title := range titles {
		j := &database.Job{
			ID: fmt.Sprintf("j%d", i), VideoID: fmt.Sprintf("vid%08d", i), Title: title,
			ChannelName: fmt.Sprintf("チャンネル %d 👍", i), Platform: "youtube", Status: statuses[i],
			CreatedAt: "2026-11-10T00:00:00Z", UpdatedAt: "2026-11-10T00:00:00Z",
		}
		jobs = append(jobs, j)
		a.statusMap[j.ID] = j.Status
	}
	a.progressStore.Set("j0", &ProgressData{Progress: "V:12 A:12", Percent: 42})
	a.taskList.SetJobs(jobs)
	a.statusBar.SetJobs(jobs)
	a.details.SetJob(jobs[1])
	a.logs.AddLines([]string{
		"2026-11-10 12:00:00 INFO a plain line",
		"2026-11-10 12:00:01 WARN 日本語のログ行 👍 with wide runes",
		"2026-11-10 12:00:02 ERROR " + strings.Repeat("long ", 80),
	})
	return a
}

// The composed frame is byte-identical to the lipgloss joins it replaced, at
// the floor size and up, with every combination of the two banners, each
// focus (the focus moves the split), and with and without a feedback line.
// It also asserts the frames were built by the composer itself and not by
// its lipgloss fallback: real panels must pass lineIsAdditive, or the
// measuring the composer is there to skip is still being paid.
//
// Mutant: padding a row to the block's width + 1 in compose; dropping the
// widest-line padding of a banner; joining blocks without the newline.
func TestComposedFrameEqualsTheLipglossJoins(t *testing.T) {
	sizes := [][2]int{{60, 20}, {61, 21}, {80, 24}, {100, 30}, {120, 40}, {200, 60}, {237, 71}}
	for _, sz := range sizes {
		for _, restart := range []bool{false, true} {
			for _, security := range []bool{false, true} {
				for _, focus := range []FocusPanel{PanelTasks, PanelDetails, PanelLogs} {
					name := fmt.Sprintf("%dx%d restart=%v security=%v focus=%v", sz[0], sz[1], restart, security, focus)
					observeInOneSecond(t, func() {
						a := composeTestApp(t, sz[0], sz[1], restart, security, focus)
						for _, fb := range []func(){
							func() {},
							func() { a.setFeedback("Deleted: " + strings.Repeat("a long title ", 20)) },
							func() { a.setWrappedFeedback("Import: "+strings.Repeat("words that wrap ", 30), severityWarning) },
							func() { a.setFeedbackWithSeverity("失敗しました 👎", severityError) },
						} {
							fb()
							want := referenceFrame(a)
							got := a.View().Content
							if got != want {
								t.Errorf("%s feedback=%q: composed frame differs from the lipgloss joins\n got: %q\nwant: %q",
									name, a.feedback.msg, got, want)
							}
							// And again from the memo.
							if again := a.View().Content; again != want {
								t.Errorf("%s: memoised frame differs from the lipgloss joins", name)
							}
						}
						if !a.frame.task.additive || !a.frame.details.additive {
							t.Errorf("%s: a panel failed lineIsAdditive (task=%v details=%v), so the composer fell back to lipgloss",
								name, a.frame.task.additive, a.frame.details.additive)
						}
					})
				}
			}
		}
	}
}

// compose against the lipgloss joins on blocks no panel produces: ragged
// lines, blocks of different heights, empty blocks, tabs, CRLF, banners
// wider and narrower than the row. The padding paths the real panels never
// take (they render full width) are taken here.
func TestComposeEqualsLipglossOnRaggedBlocks(t *testing.T) {
	blocks := []string{
		"",
		"x",
		"short\nlonger line\nmid",
		"one\ntwo\nthree\nfour\nfive",
		"\x1b[31mred\x1b[0m\n\x1b[1;32mgreen and longer\x1b[0m",
		"│ box │\n└─────┘",
		"tab\there\nno tab",
		"crlf\r\nline two\r\nthree",
		"日本語の行\nabc",          // not additive: starts with a wide rune
		"ends wide 日本\nabc",   // not additive: ends with a wide rune
		"emoji 👨‍👩‍👧 x\né x", // additive ends, wide middle
		"\n\n",
		"trailing newline\n",
		strings.Repeat("wide line ", 12),
	}
	banners := []string{"", "banner", strings.Repeat("B", 200), "two\nlines of banner"}
	n := 0
	for _, task := range blocks {
		for _, details := range blocks {
			for bi, logs := range blocks {
				status := blocks[(bi+3)%len(blocks)]
				restart := banners[n%len(banners)]
				security := banners[(n/len(banners))%len(banners)]
				n++
				var f frameComposer
				want := joinPanelsWithLipgloss(restart, security, task, details, logs, status)
				if got := f.compose(restart, security, task, details, logs, status); got != want {
					t.Fatalf("compose differs\nrestart=%q security=%q\ntask=%q\ndetails=%q\nlogs=%q\nstatus=%q\n got: %q\nwant: %q",
						restart, security, task, details, logs, status, got, want)
				}
				// A composer that has measured other blocks must not reuse
				// anything of them.
				f.compose(security, restart, details, task, status, logs)
				if got := f.compose(restart, security, task, details, logs, status); got != want {
					t.Fatalf("compose differs after measuring other blocks (task=%q details=%q)", task, details)
				}
			}
		}
	}
}

// What lineIsAdditive promises: joining lines it accepts, with or without
// spaces between, gives a line whose width is the sum of the widths. The
// rejected rows are the ways a junction can go wrong, each a real
// non-additive pair or a parser state left open.
func TestLineIsAdditive(t *testing.T) {
	accepted := []string{
		"", "a", " ", "plain text", "│ 日本語 👨‍👩‍👧 é │", "\x1b[31mred\x1b[0m",
		"\x1b[38;2;1;2;3m█▀▄\x1b[m", "╭──────╮", "x 🇯🇵 y", "\x1b[?25l.", "a\x1b[0m",
	}
	for _, l := range accepted {
		if !lineIsAdditive(l) {
			t.Errorf("lineIsAdditive(%q) = false, want true", l)
		}
	}
	for _, a := range accepted {
		for _, b := range accepted {
			for _, mid := range []string{"", " ", "   "} {
				joined := a + mid + b
				want := ansi.StringWidth(a) + len(mid) + ansi.StringWidth(b)
				if got := ansi.StringWidth(joined); got != want {
					t.Errorf("width(%q) = %d, want the sum %d", joined, got, want)
				}
			}
		}
	}

	rejected := map[string]string{
		"starts with a combining mark": "́abc",
		"starts with a wide rune":      "日本",
		"ends with a wide rune":        "abc日",
		"ends with a regional flag":    "abc🇯",
		"ends with a ZWJ":              "abc👨‍",
		"ends with a prepend rune":     "abc؀",
		"unfinished CSI":               "abc\x1b[31",
		"bare ESC at the end":          "abc\x1b",
		"OSC sequence":                 "\x1b]8;;http://x\x07link",
		"two-byte escape":              "\x1bMabc",
		"carriage return":              "abc\rdef",
		"tab":                          "a\tb",
		"DEL":                          "a\x7fb",
		"invalid UTF-8":                "a\x9bb",
		"ends with a C1 control":       "abc\u009b",
	}
	for why, l := range rejected {
		if lineIsAdditive(l) {
			t.Errorf("lineIsAdditive(%q) = true, want false: %s", l, why)
		}
	}
}

// Each input of the frame is in frameKey: after any of them moves, the next
// View is the frame the lipgloss joins give for the new state, never the
// remembered one. One row per key field that is not a panel string (the
// panels' own cache tests cover those; a panel that re-renders hands View a
// new string).
//
// Mutant: deleting a field from the frameKey literal in View — its row
// fails.
func TestFrameMemoFollowsEveryInput(t *testing.T) {
	mutations := []struct {
		name string
		do   func(a *App)
	}{
		{"feedback text", func(a *App) { a.setFeedback("Action: something else") }},
		{"feedback severity, same text", func(a *App) { a.feedback.sev = severityError }},
		{"feedback wrap, same text", func(a *App) { a.feedback.wrap = true }},
		{"feedback cleared", func(a *App) { a.feedback = appFeedback{} }},
		{"restart banner", func(a *App) { a.restartPending = true; a.recalcLayout() }},
		{"security banner", func(a *App) {
			cfg := config.Defaults()
			cfg.Network.NetworkAccess = "external"
			a.configStore = config.NewStore(cfg, "")
			a.recalcLayout()
		}},

		{"width", func(a *App) { a.width = 101; a.recalcLayout() }},
		{"height", func(a *App) { a.height = 31; a.recalcLayout() }},
		{"focus", func(a *App) { a.focusedPanel = PanelLogs; a.recalcLayout() }},
		{"a log line", func(a *App) { a.logs.AddLines([]string{"2026-11-10 12:00:09 INFO one more"}) }},
		{"selection", func(a *App) { a.taskList.MoveDown(); a.updateSelectedJob() }},
		{"status bar jobs", func(a *App) { a.statusBar.SetJobs(nil) }},
	}
	externalStore := func(access string) *config.Store {
		cfg := config.Defaults()
		cfg.Network.NetworkAccess = access
		return config.NewStore(cfg, "")
	}
	// The same inputs moved ALONE, with no relayout: the panels hand View
	// the strings they did before, so only the key field can tell.
	alone := []struct {
		name  string
		setup func(a *App)
		do    func(a *App)
	}{
		{"restart flag alone", nil, func(a *App) { a.restartPending = true }},
		{"security banner alone", nil, func(a *App) { a.configStore = externalStore("external") }},
		{"security banner text alone",
			func(a *App) { a.configStore = externalStore("external"); a.recalcLayout() },
			func(a *App) { a.configStore = externalStore("public") }},
		{"width alone", nil, func(a *App) { a.width = 90 }},
		{"height alone",
			func(a *App) { a.setWrappedFeedback(strings.Repeat("many rows of feedback ", 70), severityWarning) },
			func(a *App) { a.height = 20 }},
	}
	for _, m := range alone {
		mutations = append(mutations, struct {
			name string
			do   func(a *App)
		}{m.name, m.do})
	}
	setups := map[string]func(a *App){}
	for _, m := range alone {
		setups[m.name] = m.setup
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			observeInOneSecond(t, func() {
				a := composeTestApp(t, 100, 30, false, false, PanelTasks)
				// Long enough to wrap at 100 columns, so the wrap row moves
				// the frame.
				a.setFeedback("Press a key: " + strings.Repeat("a feedback line that is long ", 8))
				if setup := setups[m.name]; setup != nil {
					setup(a)
				}
				before := a.View().Content
				if before != referenceFrame(a) {
					t.Fatal("the first frame already differs from the lipgloss joins")
				}
				m.do(a)
				want := referenceFrame(a)
				if want == before {
					t.Fatalf("the mutation does not change the frame: the row tests nothing")
				}
				if got := a.View().Content; got != want {
					t.Errorf("after the mutation View returned a stale or wrong frame\n got: %q\nwant: %q", got, want)
				}
			})
		})
	}
}

// An unchanged frame is the remembered string itself, not an equal copy:
// that is what makes it free.
func TestUnchangedFrameIsTheRememberedString(t *testing.T) {
	observeInOneSecond(t, func() {
		a := composeTestApp(t, 100, 30, true, true, PanelTasks)
		first := a.View().Content
		second := a.View().Content
		if first != second {
			t.Fatal("two frames of one state differ")
		}
		if first == "" || unsafe.StringData(first) != unsafe.StringData(second) {
			t.Fatal("the second frame is not the remembered string")
		}
		if a.frame.content != first || !a.frame.valid {
			t.Fatal("View did not remember the frame it returned")
		}
	})
}
