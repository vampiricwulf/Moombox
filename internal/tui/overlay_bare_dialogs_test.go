package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/stats"
	"github.com/vampiricwulf/Moombox/internal/ytdlpplugin"
)

// plainRows is a view as plain rows with the padding on the right removed.
func plainRows(view string) []string {
	lines := strings.Split(ansi.Strip(view), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return lines
}

// wantFrame fails unless view is exactly w x h cells and reads as want.
func wantFrame(t *testing.T, what, view string, w, h int, want []string) {
	t.Helper()
	wantExact(t, what, view, w, h)
	if got := strings.Join(plainRows(view), "\n"); got != strings.Join(want, "\n") {
		t.Errorf("%s:\n%s\nwant:\n%s", what, got, strings.Join(want, "\n"))
	}
}

// compactSizes calls fn for a spread of sizes over the whole compact layout,
// the floor, the largest, wide-short and narrow-tall among them.
func compactSizes(fn func(w, h int)) {
	for _, w := range []int{30, 31, 35, 36, 39, 40, 47, 48, 59, 60, 120, 200} {
		for _, h := range []int{6, 7, 8, 10, 13, 19, 20, 50} {
			if layoutModeFor(w, h) == layoutCompact {
				fn(w, h)
			}
		}
	}
}

// ---- Job Log ----

func bareLogLines(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		switch i % 3 {
		case 0:
			lines[i] = fmt.Sprintf("12:01:%02d INFO  segment %d written", i%60, 1200+i)
		case 1:
			lines[i] = fmt.Sprintf("12:01:%02d WARN  segment %d 403, refreshing credentials", i%60, 1200+i)
		default:
			lines[i] = fmt.Sprintf("12:01:%02d INFO  token refreshed, resuming", i%60)
		}
	}
	return lines
}

func bareJobLog(w, h int) *JobLogModel {
	m := NewJobLogModel()
	m.SetSize(w, h)
	m.Open(&database.Job{ID: "j1", Title: "Karaoke stream part two of the endurance arc"})
	m.SetLines(bareLogLines(57))
	return m
}

// typeInto sends each rune of text, then the named keys, through the two
// calls App.Update makes for a key: the components, then the key handler.
func (m *JobLogModel) testKeys(keys ...string) {
	for _, k := range keys {
		msg := namedKey(k)
		m.UpdateComponents(msg)
		m.HandleKey(msg)
	}
}

// ---- fixtures for the other dialogs ----

func bareStatsSnapshot() stats.Snapshot {
	const gb = int64(1) << 30
	return stats.Snapshot{
		Disk:              stats.Disk{Total: uint64(1800 * gb), Free: uint64(500 * gb), UsedPct: 72.2},
		TotalSize:         1200 * gb,
		JobCount:          1234,
		SizeByPlatform:    map[string]int64{"youtube": 900 * gb, "twitch": 300 * gb},
		SizeByStatus:      map[string]int64{"finished": 1100 * gb, "error": 2 * gb, "cancelled": gb},
		TotalFinished:     1100,
		TotalDuration:     3412*3600 + 5*60 + 7,
		TotalChatMessages: 12345678,
		ActiveDownloads:   2,
		ActiveMuxing:      1,
		CountByPlatform:   map[string]int{"youtube": 900, "twitch": 334},
		Uptime:            26*time.Hour + 3*time.Minute,
	}
}

func bareStats(w, h int) *StatsDialogModel {
	m := NewStatsDialogModel()
	m.SetSize(w, h)
	m.Open()
	m.SetSnapshot(bareStatsSnapshot())
	return m
}

func bareTokens(w, h int) *ClientTokensDialogModel {
	m := NewClientTokensDialogModel()
	m.SetSize(w, h)
	m.Open()
	m.SetTokens([]*database.ClientToken{
		{ID: "t1", Label: "Firefox on the desktop upstairs", LastUsedAt: time.Now().Add(-2*time.Hour - time.Minute).Format(time.RFC3339), LastIP: "192.168.1.20"},
		{ID: "t2", Label: "Phone"},
		{ID: "t3", Label: "Old laptop", LastUsedAt: time.Now().Add(-12*24*time.Hour - time.Hour).Format(time.RFC3339), LastIP: "2001:0db8:85a3:0000:0000:8a2e:0370:7334"},
	})
	return m
}

func bareYtdlp(w, h int, info ytdlpplugin.Info) *YtdlpDialogModel {
	m := NewYtdlpDialogModel()
	m.SetSize(w, h)
	m.Open()
	m.SetStatus(info)
	return m
}

func mismatchedPlugin() ytdlpplugin.Info {
	port := 8080
	return ytdlpplugin.Info{
		Installed: true, CurrentPort: 774, InstalledPort: &port, InstalledScheme: "http", PortMismatch: true,
		PluginDir:     `C:\Users\name\AppData\Roaming\yt-dlp\plugins`,
		ExtractedPath: `C:\Users\name\AppData\Roaming\yt-dlp\plugins\moombox-pot`,
	}
}

func bareCookie(w, h int) *CookieImportDialogModel {
	m := NewCookieImportDialogModel()
	m.SetSize(w, h)
	m.Open()
	return m
}

func bareFiles(w, h int) *FilesDialogModel {
	m := NewFilesDialogModel()
	m.SetSize(w, h)
	m.Open()
	m.SetFiles([]OrphanedFileEntry{
		{Path: "/s/job-abc123", RelPath: "job-abc123/segments", Type: "staging", Size: 1288490188, Asides: []string{"a", "b"}},
		{Path: "/o/title.mkv", RelPath: "Some Stream Title.mkv", Type: "output", Size: 3650722201},
		{Path: "/t/clip.mp4", RelPath: "clip-0001.mp4", Type: "trim", Size: 12582912},
	})
	m.SetHistory([]OrphanedHistoryEntry{
		{VideoID: "dQw4w9WgXcQ", AddedAt: time.Now().Add(-2*time.Hour - time.Minute).Format(time.RFC3339)},
		{VideoID: "aaaaaaaaaaa", AddedAt: time.Now().Add(-3*24*time.Hour - time.Hour).Format(time.RFC3339)},
	})
	return m
}

func rolledBackImport() cookies.ImportResult {
	return cookies.ImportResult{YouTubeOutcome: cookies.ImportRolledBack, YouTubeAccepted: true, TwitchOutcome: cookies.ImportInstalled, Twitch: cookies.RefreshOK}
}

const longError = "the database is locked by another process and could not be read after five attempts; the last error was: SQLITE_BUSY (5) while reading " + `C:/Users/name/AppData/Roaming/Moombox/moombox.db` + ", and the retry budget is spent"

// Every state of the six dialogs is exactly the terminal's size at every
// compact size, wide-short and narrow-tall included, so App.View has nothing
// to cut: a cut row would be a cursor cell or a key line.
//
// Mutant: a message row appended without taking a list row; an input one
// cell too wide.
func TestBareDialogsFillEveryCompactSize(t *testing.T) {
	compactSizes(func(w, h int) {
		at := func(what string) string { return fmt.Sprintf("%s at %dx%d", what, w, h) }

		jl := bareJobLog(w, h)
		wantExact(t, at("job log, following"), jl.View(), w, h)
		jl.testKeys("up", "up")
		wantExact(t, at("job log, paused"), jl.View(), w, h)
		jl.testKeys("/", "4", "0", "3")
		wantExact(t, at("job log, typing"), jl.View(), w, h)
		jl.testKeys("enter")
		wantExact(t, at("job log, search applied"), jl.View(), w, h)
		jl = NewJobLogModel()
		jl.SetSize(w, h)
		jl.Open(&database.Job{ID: "j1"})
		wantExact(t, at("job log, loading"), jl.View(), w, h)
		jl.SetLines(nil)
		wantExact(t, at("job log, empty"), jl.View(), w, h)

		st := NewStatsDialogModel()
		st.SetSize(w, h)
		st.Open()
		wantExact(t, at("stats, loading"), st.View(), w, h)
		st.SetSnapshot(bareStatsSnapshot())
		wantExact(t, at("stats"), st.View(), w, h)
		st.bareScroll(100)
		wantExact(t, at("stats, scrolled"), st.View(), w, h)
		st.SetSnapshot(stats.Snapshot{})
		wantExact(t, at("stats, no disk reading"), st.View(), w, h)
		st.SetError(longError)
		wantExact(t, at("stats, error"), st.View(), w, h)

		tk := NewClientTokensDialogModel()
		tk.SetSize(w, h)
		tk.Open()
		wantExact(t, at("tokens, loading"), tk.View(), w, h)
		tk.SetTokens(nil)
		wantExact(t, at("tokens, none"), tk.View(), w, h)
		tk = bareTokens(w, h)
		wantExact(t, at("tokens"), tk.View(), w, h)
		tk.HandleKey(keyMsg("d"))
		wantExact(t, at("tokens, armed"), tk.View(), w, h)
		tk.HandleKey(namedKey("down"))
		tk.HandleKey(namedKey("down"))
		wantExact(t, at("tokens, IPv6 selected"), tk.View(), w, h)
		tk.SetError(longError)
		wantExact(t, at("tokens, error"), tk.View(), w, h)

		yt := NewYtdlpDialogModel()
		yt.SetSize(w, h)
		yt.Open()
		wantExact(t, at("ytdlp, loading"), yt.View(), w, h)
		yt.SetStatus(mismatchedPlugin())
		wantExact(t, at("ytdlp, mismatch"), yt.View(), w, h)
		yt.SetStatus(ytdlpplugin.Info{Installed: true, Unparseable: true, CurrentPort: 65535, HTTPSEnabled: true, ExtractedPath: strings.Repeat(`C:\a long folder name\`, 12)})
		wantExact(t, at("ytdlp, not recognized"), yt.View(), w, h)
		yt.SetInstalling()
		wantExact(t, at("ytdlp, installing"), yt.View(), w, h)
		yt.SetError(longError)
		wantExact(t, at("ytdlp, error"), yt.View(), w, h)

		ck := bareCookie(w, h)
		wantExact(t, at("cookie import, path"), ck.View(), w, h)
		ck.input.SetValue(strings.Repeat(`C:\Users\name\Downloads\`, 9) + "cookies.txt")
		wantExact(t, at("cookie import, long path typed"), ck.View(), w, h)
		ck.HandleKey("enter")
		wantExact(t, at("cookie import, path error"), ck.View(), w, h)
		ck.input.SetValue(t.TempDir())
		ck.HandleKey("enter")
		wantExact(t, at("cookie import, directory error"), ck.View(), w, h)
		ck.SetImporting()
		wantExact(t, at("cookie import, running"), ck.View(), w, h)
		ck.SetResult(rolledBackImport(), nil)
		wantExact(t, at("cookie import, result"), ck.View(), w, h)
		ck.SetResult(cookies.ImportResult{}, errors.New(longError))
		wantExact(t, at("cookie import, failed"), ck.View(), w, h)

		fl := NewFilesDialogModel()
		fl.SetSize(w, h)
		fl.Open()
		wantExact(t, at("orphans, scanning"), fl.View(), w, h)
		fl.SetFiles(nil)
		fl.SetHistory(nil)
		wantExact(t, at("orphans, none"), fl.View(), w, h)
		fl.SetFilesError(longError)
		wantExact(t, at("orphans, none and a load error"), fl.View(), w, h)
		fl = bareFiles(w, h)
		wantExact(t, at("orphans"), fl.View(), w, h)
		fl.HandleKey(keyMsg("d"))
		wantExact(t, at("orphans, D armed"), fl.View(), w, h)
		fl.HandleKey(keyMsg("a"))
		wantExact(t, at("orphans, A armed"), fl.View(), w, h)
		fl.HandleKey(namedKey("down"))
		fl.SetActionError(longError)
		wantExact(t, at("orphans, delete error"), fl.View(), w, h)
		fl.SetHistoryError(longError)
		fl.actionErr = ""
		wantExact(t, at("orphans, load error beside a list"), fl.View(), w, h)
		fl.SetBulkResult(2, []string{"/o/title.mkv", "/t/clip.mp4", "/s/job-abc123"})
		wantExact(t, at("orphans, bulk result"), fl.View(), w, h)
	})
}

// A populated dialog shrunk to the compact floor, through a wide-short and
// a narrow-tall terminal, and grown back draws the frame of one that was
// never shrunk: nothing the bare form sets on the list, the input, the
// viewport or the log viewer outlives it.
//
// Mutant: a bare SetSize that leaves the list at the bare height; a log
// viewer still flagged bare.
func TestBareDialogsSurviveAShrinkAndBack(t *testing.T) {
	type dialog interface {
		SetSize(w, h int)
		View() string
	}
	sizes := [][2]int{{30, 6}, {200, 8}, {30, 50}, {40, 10}}
	for name, build := range map[string]func(w, h int) dialog{
		"job log": func(w, h int) dialog {
			m := bareJobLog(w, h)
			// Following the tail: a paused view keeps a row number across a
			// re-wrap, at any size, so it has no one frame to come back to.
			m.testKeys("/", "4", "0", "3", "enter", "end")
			return m
		},
		"stats":  func(w, h int) dialog { return bareStats(w, h) },
		"tokens": func(w, h int) dialog { return bareTokens(w, h) },
		"ytdlp":  func(w, h int) dialog { return bareYtdlp(w, h, mismatchedPlugin()) },
		"cookie import": func(w, h int) dialog {
			m := bareCookie(w, h)
			m.input.SetValue(`C:\no such folder\cookies.txt`)
			m.HandleKey("enter")
			return m
		},
		"orphans": func(w, h int) dialog {
			m := bareFiles(w, h)
			m.SetActionError(longError)
			return m
		},
	} {
		for _, normal := range [][2]int{{60, 20}, {120, 40}} {
			never := build(normal[0], normal[1])
			want := never.View()
			m := build(normal[0], normal[1])
			for _, s := range sizes {
				m.SetSize(s[0], s[1])
				wantExact(t, fmt.Sprintf("%s shrunk to %dx%d", name, s[0], s[1]), m.View(), s[0], s[1])
			}
			m.SetSize(normal[0], normal[1])
			if got := m.View(); got != want {
				t.Errorf("%s: the %dx%d frame changed across a shrink:\n%s\nnever shrunk:\n%s", name, normal[0], normal[1], ansi.Strip(got), ansi.Strip(want))
			}
		}
	}
}
