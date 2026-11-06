package utils

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The owner's three example comments (owner decision CI-2), saved verbatim in
// testdata/chapter_import as given — full-width spaces, tree marks and all —
// and each line's outcome pinned in the .golden beside it: one "H:MM:SS<TAB>
// title" row per chapter, in the order ParseChapterComment returns them.

func readChapterFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "chapter_import", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func formatParsedChapters(list []Chapter) string {
	var b strings.Builder
	for _, c := range list {
		s := c.StartMs / 1000
		fmt.Fprintf(&b, "%d:%02d:%02d\t%s\n", s/3600, s/60%60, s%60, c.Title)
	}
	return b.String()
}

func TestParseChapterCommentGoldens(t *testing.T) {
	for _, name := range []string{"setlist", "kronii", "irys"} {
		t.Run(name, func(t *testing.T) {
			list, err := ParseChapterComment(readChapterFixture(t, name+".txt"))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := formatParsedChapters(list), readChapterFixture(t, name+".golden"); got != want {
				t.Errorf("parsed list differs from %s.golden\n got:\n%s\nwant:\n%s", name, got, want)
			}
		})
	}
}

// TestParseChapterCommentSetlist pins example 1 beyond its golden: the tree
// marks (├) come off, the "shachiPink【…】shachiPink" header is ignored, the
// HH:MM:SS shape is read, and " - " inside a title is the title's own.
func TestParseChapterCommentSetlist(t *testing.T) {
	list, err := ParseChapterComment(readChapterFixture(t, "setlist.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 10 {
		t.Fatalf("want the 10 timestamped lines, got %d", len(list))
	}
	if list[0] != (Chapter{StartMs: (11*60 + 59) * 1000, Title: "Numb - Linkin Park"}) {
		t.Errorf("first chapter = %+v", list[0])
	}
	for _, c := range list {
		if strings.ContainsAny(c.Title, "├【") {
			t.Errorf("title %q kept a tree mark or the header", c.Title)
		}
	}
}

// TestParseChapterCommentKronii pins example 2: M:SS and H:MM:SS mixed, and a
// title carrying a colon of its own ("free Rapunzel: Sanctuary") kept whole.
func TestParseChapterCommentKronii(t *testing.T) {
	list, err := ParseChapterComment(readChapterFixture(t, "kronii.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{
		5*60 + 25:           "start",
		1*3600 + 32*60 + 34: "free Rapunzel: Sanctuary",
		3*3600 + 55*60 + 32: "why you should start the game now",
		20*60 + 32:          "game start / chapter 0",
		1*3600 + 36*60 + 18: "spoilers for the newest main story chapters and new event",
		3*3600 + 32*60 + 41: "gacha",
		1*3600 + 6*60 + 11:  "tutorial gacha",
		30*60 + 15:          "HUH",
		1*3600 + 15*60 + 57: "Kronii the thinker",
		1*3600 + 29*60 + 40: "switching account",
	}
	if len(list) != len(want) {
		t.Fatalf("want %d chapters, got %d", len(want), len(list))
	}
	for _, c := range list {
		if want[c.StartMs/1000] != c.Title {
			t.Errorf("at %ds: got %q, want %q", c.StartMs/1000, c.Title, want[c.StartMs/1000])
		}
	}
}

// TestParseChapterCommentIrys pins what the brief names for example 3.
func TestParseChapterCommentIrys(t *testing.T) {
	text := readChapterFixture(t, "irys.txt")
	list, err := ParseChapterComment(text)
	if err != nil {
		t.Fatal(err)
	}
	byStart := map[int64]string{}
	for _, c := range list {
		byStart[c.StartMs/1000] = c.Title
	}
	at := func(h, m, s int64) string { return byStart[h*3600+m*60+s] }

	// Every line that carries a timestamp, and none other, is a chapter.
	timed := 0
	for line := range strings.SplitSeq(text, "\n") {
		if strings.ContainsRune(line, ':') && strings.ContainsAny(line, "0123456789") && chapterClockLine(line) {
			timed++
		}
	}
	if len(list) != 80 || timed != 80 {
		t.Fatalf("want the comment's 80 timestamped lines as 80 chapters, got %d (lines %d)", len(list), timed)
	}
	// The nested ┣/┗ lines are flat chapters with the marks stripped.
	if got := at(0, 8, 36); got != "Calli (カリオペ): Vaseraga / バザラガ" {
		t.Errorf("┣ line: %q", got)
	}
	if got := at(0, 14, 19); got != "Gigi (ジジ): Sandalphon / サンダルフォン" {
		t.Errorf("┗ line: %q", got)
	}
	// The U+3000-indented lines too.
	if got := at(0, 21, 24); got != "BATTLE 2" {
		t.Errorf("U+3000-indented line: %q", got)
	}
	if got := at(0, 34, 30); got != "大騒ぎのIRyS irysSurprisedirysLaugh" {
		t.Errorf("U+3000-indented line with emotes: %q", got)
	}
	// The two spaces after "0:43:44" are separators, not title.
	if got := at(0, 43, 44); got != "【Gigi (ジジ) vs Ina (イナ)①】" {
		t.Errorf("double-space line: %q", got)
	}
	// Emote shortcodes and emoji are kept as pasted.
	if got := at(1, 23, 4); got != "🎮Stage 1: Rising Royale" {
		t.Errorf("emoji title: %q", got)
	}
	// The untimed headings, separators and lines are ignored.
	for _, c := range list {
		for _, heading := range []string{"⚔", "________", "Switch 2版", "カットシーンを観ていたら", "１位決定戦"} {
			if strings.Contains(c.Title, heading) {
				t.Errorf("untimed line %q became (part of) chapter %q", heading, c.Title)
			}
		}
		if c.Title == "🎮" {
			t.Errorf("the bare 🎮 heading became a chapter")
		}
	}
	// Time order, with no start repeated.
	for i := 1; i < len(list); i++ {
		if list[i].StartMs <= list[i-1].StartMs {
			t.Fatalf("chapter %d (%d ms) is not after chapter %d (%d ms)", i, list[i].StartMs, i-1, list[i-1].StartMs)
		}
	}
	if list[0].StartMs != (6*60+55)*1000 || list[len(list)-1].StartMs != (1*3600+37*60+7)*1000 {
		t.Errorf("first/last = %d/%d ms", list[0].StartMs, list[len(list)-1].StartMs)
	}
}

// chapterClockLine is the test's own, independent count of a line that holds
// an H:MM:SS or M:SS timestamp: a digit, a colon and two digits.
func chapterClockLine(line string) bool {
	r := []rune(line)
	for i := 1; i+2 < len(r); i++ {
		if r[i] == ':' && r[i-1] >= '0' && r[i-1] <= '9' && r[i+1] >= '0' && r[i+1] <= '9' && r[i+2] >= '0' && r[i+2] <= '9' {
			return true
		}
	}
	return false
}

func TestParseChapterCommentEdgeRows(t *testing.T) {
	type row struct {
		name  string
		text  string
		want  []Chapter
		noneQ bool // ErrNoTimestamps expected
	}
	ms := func(h, m, s int64) int64 { return ((h*60+m)*60 + s) * 1000 }
	rows := []row{
		{name: "trailing timestamp", text: "start 5:25\nend - 1:00:00",
			want: []Chapter{{StartMs: ms(0, 5, 25), Title: "start"}, {StartMs: ms(1, 0, 0), Title: "end"}}},
		{name: "trailing needs exactly one timestamp", text: "from 1:00 to 2:00", noneQ: true},
		{name: "trailing glued to its title", text: "オープニング0:30",
			want: []Chapter{{StartMs: ms(0, 0, 30), Title: "オープニング"}}},
		{name: "a timestamp mid-line is not a chapter", text: "see 1:00 here", noneQ: true},
		{name: "full-width digits and colon", text: "１：２３：４５　歌枠\n０１：０２ 雑談",
			want: []Chapter{{StartMs: ms(0, 1, 2), Title: "雑談"}, {StartMs: ms(1, 23, 45), Title: "歌枠"}}},
		{name: "empty title becomes Chapter N", text: "0:30\n0:10 intro\n1:00 -",
			want: []Chapter{{StartMs: ms(0, 0, 10), Title: "intro"}, {StartMs: ms(0, 0, 30), Title: "Chapter 2"}, {StartMs: ms(0, 1, 0), Title: "Chapter 3"}}},
		{name: "duplicates keep the first", text: "1:00 first\n2:00 b\n1:00 second",
			want: []Chapter{{StartMs: ms(0, 1, 0), Title: "first"}, {StartMs: ms(0, 2, 0), Title: "b"}}},
		{name: "hours above 9", text: "10:00:00 ten\n12:34:56 later",
			want: []Chapter{{StartMs: ms(10, 0, 0), Title: "ten"}, {StartMs: ms(12, 34, 56), Title: "later"}}},
		{name: "malformed 1:2:3 rejected", text: "1:2:3 bad\n1:2 bad too\n0:61 bad seconds\n1:60:00 bad minutes", noneQ: true},
		{name: "MM:SS past the hour", text: "75:30 late", want: []Chapter{{StartMs: ms(0, 75, 30), Title: "late"}}},
		{name: "out of order is sorted", text: "2:00 b\n1:00 a",
			want: []Chapter{{StartMs: ms(0, 1, 0), Title: "a"}, {StartMs: ms(0, 2, 0), Title: "b"}}},
		{name: "bullets and separators", text: "• 0:01 - one\n▶ 0:02 | two\n* 0:03 ： three\n・0:04・four\n— 0:05 – five\n┃ 0:06: six",
			want: []Chapter{{StartMs: 1000, Title: "one"}, {StartMs: 2000, Title: "two"}, {StartMs: 3000, Title: "three"}, {StartMs: 4000, Title: "four"}, {StartMs: 5000, Title: "five"}, {StartMs: 6000, Title: "six"}}},
		{name: "CRLF line ends", text: "0:01 a\r\n0:02 b\r\n",
			want: []Chapter{{StartMs: 1000, Title: "a"}, {StartMs: 2000, Title: "b"}}},
		{name: "nothing timed", text: "just a comment\n⚔1.\n________", noneQ: true},
		{name: "empty paste", text: "", noneQ: true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got, err := ParseChapterComment(r.text)
			if r.noneQ {
				if !errors.Is(err, ErrNoTimestamps) || got != nil {
					t.Fatalf("want ErrNoTimestamps, got %v %+v", err, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, r.want) {
				t.Errorf("got  %+v\nwant %+v", got, r.want)
			}
		})
	}
	if ErrNoTimestamps.Error() != "No timestamps found" {
		t.Errorf("the dialog's text is %q", ErrNoTimestamps.Error())
	}
}

func TestFitImportedChapters(t *testing.T) {
	parsed := []Chapter{{StartMs: 5000, Title: "a"}, {StartMs: 20000, Title: "b"}, {StartMs: 60000, Title: "past"}}
	// The offset shifts every chapter; one before 0 or at or past the end is
	// dropped; SanitizeChapters sets the ends and fills the opening.
	got, err := FitImportedChapters(parsed, -10000, 50000)
	if err != nil {
		t.Fatal(err)
	}
	want := []Chapter{{StartMs: 0, EndMs: 10000, Title: "<Untitled Chapter 1>"}, {StartMs: 10000, EndMs: 50000, Title: "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("offset -10s:\n got  %+v\nwant %+v", got, want)
	}
	// A positive offset moves the list later.
	got, err = FitImportedChapters(parsed[:2], 2000, 50000)
	if err != nil {
		t.Fatal(err)
	}
	if got[1].StartMs != 7000 || got[2].StartMs != 22000 {
		t.Errorf("offset +2s: %+v", got)
	}
	// A chapter exactly at the end is dropped.
	got, _ = FitImportedChapters([]Chapter{{StartMs: 0, Title: "x"}, {StartMs: 50000, Title: "end"}}, 0, 50000)
	if len(got) != 1 || got[0].EndMs != 50000 {
		t.Errorf("at-end chapter: %+v", got)
	}
	if _, err := FitImportedChapters(parsed, -100000, 50000); !errors.Is(err, ErrNoChaptersInRange) {
		t.Errorf("all shifted out: %v", err)
	}
	// All past the end is nothing too, not the lone untitled opening
	// SanitizeChapters would put in front of them.
	if got, err := FitImportedChapters(parsed, 60000, 50000); !errors.Is(err, ErrNoChaptersInRange) {
		t.Errorf("all past the end: %+v %v", got, err)
	}
	if _, err := FitImportedChapters(parsed, 0, 0); !errors.Is(err, ErrChapterLengthUnknown) {
		t.Errorf("no length: %v", err)
	}
}

func TestParseChapterOffset(t *testing.T) {
	for in, want := range map[string]int64{
		"":         0,
		"0":        0,
		"90":       90000,
		"-90":      -90000,
		"+12.5":    12500,
		"-1:30":    -90000,
		"1:02:03":  3723000,
		"-1:02:03": -3723000,
		"−0:05":    -5000,
		"－１：００":    0, // the full-width minus is not a sign: refused below
	} {
		got, err := ParseChapterOffset(in)
		if in == "－１：００" {
			if !errors.Is(err, ErrBadChapterOffset) {
				t.Errorf("%q: want ErrBadChapterOffset, got %d %v", in, got, err)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%q: got %d %v, want %d", in, got, err, want)
		}
	}
	if got, err := ParseChapterOffset("１：００"); err != nil || got != 60000 {
		t.Errorf("full-width clock: %d %v", got, err)
	}
	for _, bad := range []string{"abc", "1:2:3", "--5", "1e3", "5s", "1:60"} {
		if _, err := ParseChapterOffset(bad); !errors.Is(err, ErrBadChapterOffset) {
			t.Errorf("%q: want ErrBadChapterOffset, got %v", bad, err)
		}
	}
}

// TestImportedChaptersSidecar pins the import's file (owner decision CI-3):
// the list and the original round-trip; an empty list is written, not
// removed, and an empty original is still an import's ("original": []); a
// finalize's own file (WriteChaptersSidecar) is not an import's.
//
// Mutants: WriteImportedChaptersSidecar leaving Original nil for an empty
// original (the file reads as a finalize's); ReadImportedChaptersSidecar
// answering true for a finalize's file.
func TestImportedChaptersSidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "X"+ChaptersSidecarSuffix)
	orig := []SidecarChapter{{Start: 0, End: 3, Title: "A"}}
	if err := WriteImportedChaptersSidecar(path, []Chapter{{StartMs: 0, EndMs: 1500, Title: "one"}}, orig); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadImportedChaptersSidecar(path)
	if err != nil || !ok || !reflect.DeepEqual(got.Original, orig) || !reflect.DeepEqual(got.Chapters, []SidecarChapter{{Start: 0, End: 1.5, Title: "one"}}) {
		t.Fatalf("round trip = %+v %v %v", got, ok, err)
	}
	if list, _ := ReadChaptersSidecar(path); len(list) != 1 || list[0].Title != "one" {
		t.Errorf("the finalize's reader sees %+v", list)
	}
	if err := WriteImportedChaptersSidecar(path, nil, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != `{"chapters":[],"original":[]}` {
		t.Errorf("empty import = %s", raw)
	}
	if got, ok, _ := ReadImportedChaptersSidecar(path); !ok || len(got.Chapters) != 0 || len(got.Original) != 0 {
		t.Errorf("an empty import reads %+v %v", got, ok)
	}
	if err := WriteChaptersSidecar(path, []Chapter{{StartMs: 0, EndMs: 1000, Title: "auto"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := ReadImportedChaptersSidecar(path); ok {
		t.Error("a finalize's file reads as an import's")
	}
	if _, ok, err := ReadImportedChaptersSidecar(filepath.Join(dir, "missing.json")); ok || err != nil {
		t.Errorf("a missing file: %v %v", ok, err)
	}
}
