package utils

import (
	"reflect"
	"testing"
)

// TestChaptersFromDescription is yt-dlp's _extract_chapters_from_description
// (common.py, b77c9304e): time-first lines, then — only when those give
// nothing — time-last lines, sorted, kept only while each start is at or after
// the last kept one and inside the duration; nothing without a duration.
//
// Mutants: the time-last shape tried even when time-first matched (the
// "Outro 9:00" line joins); the duration bound dropped (the 99:00 line is
// kept); no sort (the 1:00 line after 2:00 is lost); the duration guard
// removed (a list comes back for durationMs 0).
func TestChaptersFromDescription(t *testing.T) {
	desc := "Timestamps:\r\n0:00 Intro\n2:00 - Second part\n1:00 First part\n1:02:03 Long one\n99:00:00 Past the end\nOutro 9:00\n"
	got := ChaptersFromDescription(desc, 2*3600*1000)
	want := []Chapter{
		{StartMs: 0, Title: "Intro"},
		{StartMs: 60_000, Title: "First part"},
		{StartMs: 120_000, Title: "Second part"},
		{StartMs: 3723_000, Title: "Long one"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("time-first chapters = %+v, want %+v", got, want)
	}

	got = ChaptersFromDescription("Intro 0:00\nMiddle: 5:30\n", 600_000)
	want = []Chapter{{StartMs: 0, Title: "Intro"}, {StartMs: 330_000, Title: "Middle"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("time-last chapters = %+v, want %+v", got, want)
	}

	if got := ChaptersFromDescription(desc, 0); got != nil {
		t.Errorf("chapters without a duration = %+v, want none", got)
	}

	// Non-ASCII titles, each expectation Python's re.findall with yt-dlp's
	// sep_re: RE2's ASCII `\W` would swallow the title and the newline.
	for _, tc := range []struct {
		desc string
		want []Chapter
	}{
		{"0:00 イントロ 開始\n5:00 歌枠 パート2\n10:00 エンディング", []Chapter{
			{StartMs: 0, Title: "イントロ 開始"}, {StartMs: 300_000, Title: "歌枠 パート2"}, {StartMs: 600_000, Title: "エンディング"}}},
		{"0:00 【雑談】 はじめに\n12:34 Song one", []Chapter{
			{StartMs: 0, Title: "【雑談】 はじめに"}, {StartMs: 754_000, Title: "Song one"}}},
		{"0:00 - 雑談 開始\n1:00\u3000歌", []Chapter{
			{StartMs: 0, Title: "雑談 開始"}, {StartMs: 60_000, Title: "歌"}}},
		{"オープニング 0:00\n歌】 5:00\n", []Chapter{
			{StartMs: 0, Title: "オープニング"}, {StartMs: 300_000, Title: "歌"}}},
	} {
		if got := ChaptersFromDescription(tc.desc, 3600_000); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ChaptersFromDescription(%q) = %+v, want %+v", tc.desc, got, tc.want)
		}
	}
}

// TestSanitizeChapters is yt-dlp's fill-in (YoutubeDL.py process_info) plus
// Moombox's clamp to the archive: an untitled chapter in front of a list that
// starts late, ends from the next start and the duration, untitled names, and
// a chapter cut to the archive or dropped when nothing of it is left.
//
// Mutants: no leading chapter; the last end left 0; the clamp to durationMs
// removed (the last chapter runs to 9000); empty chapters kept.
func TestSanitizeChapters(t *testing.T) {
	got := SanitizeChapters([]Chapter{
		{StartMs: 1000, Title: "A"},
		{StartMs: 3000},
		{StartMs: 4000, EndMs: 9000, Title: "C"},
		{StartMs: 5000, EndMs: 5000, Title: "empty"},
	}, 6000)
	want := []Chapter{
		{StartMs: 0, EndMs: 1000, Title: "<Untitled Chapter 1>"},
		{StartMs: 1000, EndMs: 3000, Title: "A"},
		{StartMs: 3000, EndMs: 4000, Title: "<Untitled Chapter 3>"},
		{StartMs: 4000, EndMs: 6000, Title: "C"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sanitized = %+v, want %+v", got, want)
	}
	open := SanitizeChapters([]Chapter{{StartMs: 0, Title: "A"}, {StartMs: 2000, Title: "B"}}, 5000)
	if len(open) != 2 || open[1].EndMs != 5000 {
		t.Errorf("an open last chapter = %+v, want it to end at the duration", open)
	}
	if got := SanitizeChapters(want, 0); got != nil {
		t.Errorf("sanitized without a duration = %+v, want none", got)
	}
}

// TestShiftChapters moves a broadcast-timeline list into a capture that began
// 90 s in: the chapter that ended before is dropped, the one in progress is
// cut to 0, the rest move back; an open-ended chapter ends at the next start.
//
// Mutants: an open-ended chapter's end not taken from the next start (the
// first chapter survives); the in-progress chapter dropped instead of cut.
func TestShiftChapters(t *testing.T) {
	got := ShiftChapters([]Chapter{
		{StartMs: 0, Title: "Pre"},
		{StartMs: 60_000, EndMs: 120_000, Title: "Running"},
		{StartMs: 120_000, Title: "Later"},
	}, 90_000)
	want := []Chapter{
		{StartMs: 0, EndMs: 30_000, Title: "Running"},
		{StartMs: 30_000, Title: "Later"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("shifted = %+v, want %+v", got, want)
	}
}

// TestFFMetadataChapters pins the file FFmpeg reads: yt-dlp's layout and its
// escapes for '=', ';', '#', '\' and a newline.
//
// Mutant: the escape regexp missing '#' — the title would start a comment.
func TestFFMetadataChapters(t *testing.T) {
	got := FFMetadataChapters([]Chapter{{StartMs: 0, EndMs: 1500, Title: "a=b;c#d\\e\nf\r"}})
	want := ";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=1500\ntitle=a\\=b\\;c\\#d\\\\e\\\nf\n"
	if got != want {
		t.Errorf("FFMetadataChapters = %q, want %q", got, want)
	}
}

// TestStagedChaptersRoundTrip: what the Twitch paths stage is what the mux
// reads, an empty list removes the file, and no file reads as none.
func TestStagedChaptersRoundTrip(t *testing.T) {
	dir := t.TempDir()
	list := []Chapter{{StartMs: 0, Title: "A"}, {StartMs: 5000, EndMs: 9000, Title: "B"}}
	if err := WriteStagedChapters(dir, list); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStagedChapters(dir)
	if err != nil || !reflect.DeepEqual(got, list) {
		t.Fatalf("read back %+v, %v; want %+v", got, err, list)
	}
	if err := WriteStagedChapters(dir, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadStagedChapters(dir); err != nil || got != nil {
		t.Errorf("after an empty write: %+v, %v; want none", got, err)
	}
}
