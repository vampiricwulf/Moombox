package utils

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Chapter is one chapter marker of an archive, in milliseconds of the
// archive's own timeline. EndMs is 0 while it is not yet known (a list read
// from a description or a Twitch category log knows only where each chapter
// begins); SanitizeChapters fills it from the next chapter's start, or from
// the archive's duration for the last one.
//
// One shape for both platforms (owner decision D23 and its follow-ups): the
// Twitch VOD's moments, the categories a live Twitch capture observed, and a
// YouTube video's chapter markers or description timestamps all become this,
// and the muxer embeds it as an FFMETADATA [CHAPTER] list (FFMetadataChapters).
type Chapter struct {
	StartMs int64  `json:"startMs"`
	EndMs   int64  `json:"endMs,omitempty"`
	Title   string `json:"title,omitempty"`
}

// ChaptersFromStarts is yt-dlp's _extract_chapters_helper
// (yt_dlp/extractor/common.py, b77c9304e): it keeps the chapters whose start
// is known, not before the previous kept one and not past durationMs, in the
// order given — or sorted first when sorted is true, which is the helper's
// strict=False mode the description parser runs in. Upstream returns nothing
// without a duration, and so does this: without one, the "not past the end"
// half of the rule cannot be applied, and the muxer has no END for the last
// chapter either. A negative start is "unknown" (upstream's None).
func ChaptersFromStarts(chapters []Chapter, durationMs int64, sorted bool) []Chapter {
	if durationMs <= 0 || len(chapters) == 0 {
		return nil
	}
	list := slices.Clone(chapters)
	if sorted {
		slices.SortStableFunc(list, func(a, b Chapter) int {
			return cmp.Compare(max(a.StartMs, 0), max(b.StartMs, 0))
		})
	}
	var out []Chapter
	last := int64(0)
	for _, c := range list {
		if c.StartMs < 0 {
			continue
		}
		if last <= c.StartMs && c.StartMs <= durationMs {
			out = append(out, Chapter{StartMs: c.StartMs, Title: c.Title})
			last = c.StartMs
		}
	}
	return out
}

// SanitizeChapters is yt-dlp's chapter fill-in (YoutubeDL.py process_info,
// b77c9304e), clamped to the archive: a list whose first chapter starts after
// 0 gets an untitled chapter in front of it, every chapter without an end ends
// where the next begins (the last one at durationMs), and an untitled one is
// named "<Untitled Chapter N>". Beyond upstream, which embeds whatever the
// extractor produced, a chapter is cut to [0, durationMs] and one left with
// nothing of the archive is dropped: the Twitch lists arrive in the stream's
// timeline and are shifted into the capture's before they get here
// (ShiftChapters), and FFmpeg's mov muxer writes a chapter that runs past the
// last frame as a chapter track longer than the movie. Returns nil when
// nothing is left or durationMs is unknown.
func SanitizeChapters(chapters []Chapter, durationMs int64) []Chapter {
	if durationMs <= 0 || len(chapters) == 0 {
		return nil
	}
	list := slices.Clone(chapters)
	if list[0].StartMs > 0 {
		list = append([]Chapter{{StartMs: 0}}, list...)
	}
	for i := range list {
		if list[i].EndMs <= 0 {
			if i+1 < len(list) {
				list[i].EndMs = list[i+1].StartMs
			} else {
				list[i].EndMs = durationMs
			}
		}
		if list[i].Title == "" {
			list[i].Title = fmt.Sprintf("<Untitled Chapter %d>", i+1)
		}
	}
	var out []Chapter
	for _, c := range list {
		c.StartMs = max(c.StartMs, 0)
		c.EndMs = min(c.EndMs, durationMs)
		if c.EndMs <= c.StartMs {
			continue
		}
		out = append(out, c)
	}
	return out
}

// ShiftChapters moves a list from a broadcast's timeline into a capture's that
// began offsetMs into the broadcast: every start and known end moves back by
// offsetMs. A chapter that ENDED before the capture began is dropped; one
// already running when it began is cut to start at 0, keeping its title. A
// chapter with no end is taken to run until the next one starts, so the
// chapter in progress at the capture's start is the last one that started
// before it. A capture that began before the broadcast (offsetMs < 0) moves
// the list forward instead.
func ShiftChapters(chapters []Chapter, offsetMs int64) []Chapter {
	var out []Chapter
	for i, c := range chapters {
		end := c.EndMs
		if end <= 0 && i+1 < len(chapters) {
			end = chapters[i+1].StartMs
		}
		if end > 0 && end-offsetMs <= 0 {
			continue
		}
		s := Chapter{StartMs: max(c.StartMs-offsetMs, 0), Title: c.Title}
		if c.EndMs > 0 {
			s.EndMs = c.EndMs - offsetMs
		}
		out = append(out, s)
	}
	return out
}

// ffmetadataEscapeRe is the set FFmpeg's FFMETADATA format reserves:
// '=', ';', '#', '\' and a newline are escaped with a backslash. yt-dlp's
// ffmpeg_escape (postprocessor/ffmpeg.py _get_chapter_opts) escapes exactly
// these.
var ffmetadataEscapeRe = regexp.MustCompile(`([\\=;#\n])`)

// FFMetadataChapters renders a sanitized chapter list as an FFMETADATA1 file,
// as yt-dlp's FFmpegMetadataPP writes it for --embed-chapters
// (_get_chapter_opts): one [CHAPTER] block per chapter, TIMEBASE 1/1000,
// START and END in milliseconds, the title escaped. A carriage return, which
// the format has no escape for, is dropped.
func FFMetadataChapters(chapters []Chapter) string {
	var b strings.Builder
	b.WriteString(";FFMETADATA1\n")
	for _, c := range chapters {
		b.WriteString("[CHAPTER]\nTIMEBASE=1/1000\n")
		fmt.Fprintf(&b, "START=%d\nEND=%d\n", c.StartMs, c.EndMs)
		if c.Title != "" {
			title := strings.ReplaceAll(c.Title, "\r", "")
			b.WriteString("title=" + ffmetadataEscapeRe.ReplaceAllString(title, `\$1`) + "\n")
		}
	}
	return b.String()
}

// descriptionChapterDuration is yt-dlp's duration_re for description
// chapters, `(?:\d+:)?\d{1,2}:\d{2}`.
const descriptionChapterDuration = `(?:\d+:)?\d{1,2}:\d{2}`

// pySpace, pyNonWord and pyWord are Python 3's Unicode `\s`, `\W` and `\w`
// (str.isspace; not str.isalnum and not '_'; str.isalnum or '_'), which
// yt-dlp's sep_re is written against. RE2's `\s`, `\W` and `\b` are
// ASCII-only, and a literal port lets `\W*` eat a Japanese title and the
// newline after it, so "0:00 雑談\n5:00 歌枠" came out as one chapter titled
// "5:00 歌枠", and a CJK title ending never met `\b`. Like Python's, pySpace
// and pyNonWord include the newline: the shapes must match exactly what
// upstream's do, line joins included.
const (
	pySpace   = `[\t\n\v\f\r\x1c-\x1f\x85\p{Z}]`
	pyNonWord = `[^\p{L}\p{N}_]`
	pyWord    = `[\p{L}\p{N}_]`
)

// descriptionChapterTimeFirst and descriptionChapterTimeLast are yt-dlp's two
// sep_re shapes (_extract_chapters_from_description, common.py b77c9304e),
// `^\s*(dur)\b\W*\s(.+?)\s*$` and `^\s*(.+?)\b\W*\s(dur)\s*$`: "0:00 Intro"
// lines, and failing those, "Intro 0:00" lines, with Python's Unicode classes
// spelled out. The `\b` after the duration is dropped: the `\W*\s` that
// follows already demands a non-word character there, which is all the
// boundary asks of a duration ending in a digit. The `\b` after a time-last
// title, with a non-word character after it, means the title ends on a word
// character, so the title group is written to end on one.
var (
	descriptionChapterTimeFirst = regexp.MustCompile(`(?m)^` + pySpace + `*(` + descriptionChapterDuration + `)` + pyNonWord + `*` + pySpace + `(.+?)` + pySpace + `*$`)
	descriptionChapterTimeLast  = regexp.MustCompile(`(?m)^` + pySpace + `*(.*?` + pyWord + `)` + pyNonWord + `*` + pySpace + `(` + descriptionChapterDuration + `)` + pySpace + `*$`)
)

// ChaptersFromDescription is yt-dlp's _extract_chapters_from_description: the
// timestamped lines of a video's description, time first ("0:00 Intro") and,
// when that shape yields nothing, time last ("Intro 0:00"), each run through
// ChaptersFromStarts in its sorted mode. Returns nil without a duration, as
// upstream does.
func ChaptersFromDescription(description string, durationMs int64) []Chapter {
	if description == "" {
		return nil
	}
	description = strings.ReplaceAll(description, "\r\n", "\n")
	collect := func(re *regexp.Regexp, timeIdx, titleIdx int) []Chapter {
		var list []Chapter
		for _, m := range re.FindAllStringSubmatch(description, -1) {
			list = append(list, Chapter{StartMs: ParseClockMs(m[timeIdx]), Title: m[titleIdx]})
		}
		return ChaptersFromStarts(list, durationMs, true)
	}
	if out := collect(descriptionChapterTimeFirst, 1, 2); len(out) > 0 {
		return out
	}
	return collect(descriptionChapterTimeLast, 2, 1)
}

// ParseClockMs reads "[H:]MM:SS" (or "M:SS") as milliseconds, the subset of
// yt-dlp's parse_duration the chapter sources use. -1 for anything else.
func ParseClockMs(s string) int64 {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return -1
	}
	var total int64
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return -1
		}
		total = total*60 + int64(n)
	}
	return total * 1000
}

// StagedChaptersFile is the chapter list a capture leaves in its staging
// directory for the mux to embed, already in the capture's own timeline: the
// Twitch paths write it before they finalize (the VOD's moments, or a live
// capture's archive-VOD or observed-category list), and muxAndFinalize reads
// it. A file, not a column, so the Mux action of a crashed capture still
// finds it.
const StagedChaptersFile = "chapters.json"

// WriteStagedChapters writes chapters to dir/StagedChaptersFile atomically.
// An empty list removes the file instead, so a re-run that found no chapters
// does not leave an earlier run's behind.
func WriteStagedChapters(dir string, chapters []Chapter) error {
	path := filepath.Join(dir, StagedChaptersFile)
	if len(chapters) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := json.Marshal(chapters)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o644)
}

// ReadStagedChapters reads dir/StagedChaptersFile. A missing file is no
// chapters and no error.
func ReadStagedChapters(dir string) ([]Chapter, error) {
	data, err := os.ReadFile(filepath.Join(dir, StagedChaptersFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Chapter
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse staged chapters: %w", err)
	}
	return out, nil
}
