package utils

import (
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// ChapterImportError is a pasted chapter list (or its offset) refused for a
// reason the operator can act on. Its text is shown as it stands, in the Web
// player's import dialog and on the TUI's feedback line, so it is written as a
// sentence for them.
type ChapterImportError struct{ Reason string }

func (e *ChapterImportError) Error() string { return e.Reason }

var (
	// ErrNoTimestamps is a paste with no line that carries a timestamp
	// (owner decision CI-2: fewer than one chapter is an error the dialog
	// shows).
	ErrNoTimestamps = &ChapterImportError{Reason: "No timestamps found"}
	// ErrNoChaptersInRange is a paste whose timestamps, once the offset is
	// applied, all land before the recording's start or at or past its end
	// (owner decision CI-5: those are dropped, and nothing is left).
	ErrNoChaptersInRange = &ChapterImportError{Reason: "No timestamps fall within the recording"}
	// ErrChapterLengthUnknown is an archive whose length cannot be read, so
	// the list cannot be fitted to it (SanitizeChapters needs the end).
	ErrChapterLengthUnknown = &ChapterImportError{Reason: "The recording's length is unknown, so chapters cannot be fitted to it"}
	// ErrBadChapterOffset is an offset that is neither seconds nor
	// ±H:MM:SS / ±M:SS.
	ErrBadChapterOffset = &ChapterImportError{Reason: "Offset must be seconds or ±H:MM:SS"}
)

// chapterClockRe is the timestamp shapes a pasted comment's lines may carry
// (owner decision CI-2): H:MM:SS and HH:MM:SS, or M:SS and MM:SS. Matched
// against a whole run of digits and colons, already folded to ASCII
// (foldChapterDigit), so "1:2:3" — a run the shapes do not cover — is refused
// rather than read as some part of itself. yt-dlp's description duration_re,
// `(?:\d+:)?\d{1,2}:\d{2}` (ChaptersFromDescription), takes "1:2:03"; the
// owner's shapes do not, and this follows them.
var chapterClockRe = regexp.MustCompile(`^(?:(\d{1,2}):(\d{2})|(\d{1,2})):(\d{2})$`)

// foldChapterDigit maps a full-width digit or colon (U+FF10–U+FF19, U+FF1A),
// which a comment typed with a Japanese IME carries, onto its ASCII form, and
// reports whether r belongs to a timestamp run at all.
func foldChapterDigit(r rune) (rune, bool) {
	switch {
	case r >= '0' && r <= '9', r == ':':
		return r, true
	case r >= '０' && r <= '９':
		return '0' + (r - '０'), true
	case r == '：':
		return ':', true
	}
	return r, false
}

// parseChapterClock reads a folded timestamp run as milliseconds; ok is false
// for a run no accepted shape covers, or one with minutes or seconds out of
// range (MM:SS takes minutes up to 99 — a long stream's comment often counts
// past the hour that way — but H:MM:SS takes them below 60).
func parseChapterClock(run string) (int64, bool) {
	m := chapterClockRe.FindStringSubmatch(run)
	if m == nil {
		return 0, false
	}
	atoi := func(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }
	sec := atoi(m[4])
	if sec >= 60 {
		return 0, false
	}
	if m[1] != "" {
		h, mins := atoi(m[1]), atoi(m[2])
		if mins >= 60 {
			return 0, false
		}
		return ((h*60+mins)*60 + sec) * 1000, true
	}
	return (atoi(m[3])*60 + sec) * 1000, true
}

// isChapterLeadMark is what a pasted line may start with ahead of its
// timestamp, and is stripped (owner decision CI-2): whitespace (unicode.IsSpace
// covers the full-width space, U+3000, that indents a nested line), the whole
// Box Drawing block (U+2500–U+257F: ├ ┣ ┗ ┃ │ └ ╰ ┠ ┖ ─ and every other tree
// mark a setlist is drawn with), and the bullets - – — • ・ * ▶ ►.
func isChapterLeadMark(r rune) bool {
	if unicode.IsSpace(r) || (r >= 0x2500 && r <= 0x257F) {
		return true
	}
	switch r {
	case '-', '–', '—', '•', '・', '*', '▶', '►':
		return true
	}
	return false
}

// isChapterTitleSep is what separates a timestamp from its title, trimmed
// from the title's edge next to the timestamp (owner decision CI-2): spaces,
// "-", "–", "—", "|", ":", "：" and "・".
func isChapterTitleSep(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	switch r {
	case '-', '–', '—', '|', ':', '：', '・':
		return true
	}
	return false
}

// chapterRun is one maximal run of timestamp characters in a line: its byte
// span and its folded text.
type chapterRun struct {
	from, to int
	text     string
}

// chapterRuns returns line's maximal runs of digits and colons (either width)
// that hold a colon — the only runs that can be a timestamp.
func chapterRuns(line string) []chapterRun {
	var runs []chapterRun
	var b strings.Builder
	start, colon := -1, false
	flush := func(end int) {
		// A colon that ends the run is the separator after it ("0:06: six"),
		// not part of the timestamp: it is left to the title's trim.
		text := b.String()
		for strings.HasSuffix(text, ":") {
			text = text[:len(text)-1]
			if strings.HasSuffix(line[:end], "：") {
				end -= len("：")
			} else {
				end--
			}
		}
		if start >= 0 && colon && strings.Contains(text, ":") {
			runs = append(runs, chapterRun{from: start, to: end, text: text})
		}
		start, colon = -1, false
		b.Reset()
	}
	for i, r := range line {
		folded, ok := foldChapterDigit(r)
		if !ok {
			flush(i)
			continue
		}
		if start < 0 {
			start = i
		}
		if folded == ':' {
			colon = true
		}
		b.WriteRune(folded)
	}
	flush(len(line))
	return runs
}

// ParseChapterComment reads a pasted timestamp comment — a viewer's setlist
// or chapter list — as chapter starts (owner decision CI-2, "flat"). It is
// the one parser: the Web dialog's live preview asks the server for it
// (POST /api/jobs/{id}/chapters/preview), and the TUI's import calls the same
// path, so there is no second copy to drift.
//
// Every line that carries a timestamp becomes one chapter; a line without
// one — a heading, a separator, a decoration such as "shachiPink【…】shachiPink"
// — is ignored, and nesting is not kept: a ┣/┗ or full-width-space indented
// line is a chapter like any other. A line is read as:
//
//   - its leading decoration stripped (isChapterLeadMark);
//   - then a timestamp (chapterClockRe, ASCII or full-width), the title being
//     the rest of the line with the separators next to the timestamp
//     (isChapterTitleSep) and the surrounding whitespace trimmed. Everything
//     else in the title — an emote shortcode such as "irysLaugh", an emoji,
//     brackets — is kept as pasted;
//   - or, failing that, a line that ends in its one and only timestamp
//     ("start 5:25"), the title being what comes before it, trimmed the same
//     way. A line with a timestamp elsewhere, or with two, is ignored.
//
// A run of digits and colons the accepted shapes do not cover ("1:2:3") is no
// timestamp, and a line led by one is ignored rather than read some other way.
//
// The list comes back in time order with EndMs unset; a start pasted twice
// keeps its first line's title; an empty title becomes "Chapter N", N being
// its place in the parsed list — the paste's timed lines, in time order — not
// in the list as finally saved: FitImportedChapters may drop chapters outside
// the recording and put an "<Untitled Chapter 1>" in front of a late first
// one, and the numbers are not redone. ErrNoTimestamps when no line carries
// one. The list is not yet fitted to any archive — FitImportedChapters does
// that.
//
// This is not yt-dlp's _extract_chapters_from_description (ChaptersFromDescription,
// common.py b77c9304e), which reads a video's own description and keeps its
// lines only while they run in order: a comment is nested, decorated and
// often pasted out of order, so this is Moombox's own reader, built on the
// owner's three example comments (testdata/chapter_import).
func ParseChapterComment(text string) ([]Chapter, error) {
	var list []Chapter
	seen := map[int64]bool{}
	for line := range strings.SplitSeq(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		start, title, ok := parseChapterLine(line)
		if !ok || seen[start] {
			continue
		}
		seen[start] = true
		list = append(list, Chapter{StartMs: start, Title: title})
	}
	if len(list) == 0 {
		return nil, ErrNoTimestamps
	}
	slices.SortStableFunc(list, func(a, b Chapter) int {
		switch {
		case a.StartMs < b.StartMs:
			return -1
		case a.StartMs > b.StartMs:
			return 1
		}
		return 0
	})
	for i := range list {
		if list[i].Title == "" {
			list[i].Title = "Chapter " + strconv.Itoa(i+1)
		}
	}
	return list, nil
}

// parseChapterLine is ParseChapterComment's reading of one line.
func parseChapterLine(line string) (startMs int64, title string, ok bool) {
	body := strings.TrimLeftFunc(line, isChapterLeadMark)
	runs := chapterRuns(body)
	// Time first: a run at the very start of what the decoration left.
	if len(runs) > 0 && runs[0].from == 0 {
		ms, valid := parseChapterClock(runs[0].text)
		if !valid {
			return 0, "", false
		}
		return ms, strings.TrimSpace(strings.TrimLeftFunc(body[runs[0].to:], isChapterTitleSep)), true
	}
	// Time last: exactly one timestamp in the line, and it ends the line.
	var stamps []chapterRun
	for _, r := range runs {
		if _, valid := parseChapterClock(r.text); valid {
			stamps = append(stamps, r)
		}
	}
	if len(stamps) != 1 {
		return 0, "", false
	}
	last := stamps[0]
	if strings.TrimRightFunc(body[last.to:], unicode.IsSpace) != "" {
		return 0, "", false
	}
	ms, _ := parseChapterClock(last.text)
	return ms, strings.TrimSpace(strings.TrimRightFunc(body[:last.from], isChapterTitleSep)), true
}

// FitImportedChapters shifts a parsed list by offsetMs (owner decision CI-5:
// the operator's offset, or the capture's known one, moves every chapter),
// drops each chapter that lands before 0 or at or past durationMs, and fits
// what is left to the archive with SanitizeChapters, which sets every end and
// puts an untitled chapter in front of a list that does not start at 0, as
// yt-dlp's fill-in does. ErrChapterLengthUnknown without a length;
// ErrNoChaptersInRange when nothing is left.
func FitImportedChapters(parsed []Chapter, offsetMs, durationMs int64) ([]Chapter, error) {
	if durationMs <= 0 {
		return nil, ErrChapterLengthUnknown
	}
	var kept []Chapter
	for _, c := range parsed {
		start := c.StartMs + offsetMs
		if start < 0 || start >= durationMs {
			continue
		}
		kept = append(kept, Chapter{StartMs: start, Title: c.Title})
	}
	out := SanitizeChapters(kept, durationMs)
	if len(out) == 0 {
		return nil, ErrNoChaptersInRange
	}
	return out, nil
}

// chapterOffsetNumberRe is an offset given as seconds, its sign already taken
// off: a decimal number.
var chapterOffsetNumberRe = regexp.MustCompile(`^(?:\d+(?:\.\d*)?|\.\d+)$`)

// ParseChapterOffset reads the import's offset field (owner decision CI-5):
// seconds ("-90", "12.5") or a clock with an optional sign ("-1:02:03",
// "+1:30", full-width digits too), as milliseconds. Empty is 0.
// ErrBadChapterOffset for anything else.
func ParseChapterOffset(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	sign := int64(1)
	body := s
	// The minus sign (U+2212) is what a copied "−1:30" often carries.
	for _, p := range []string{"-", "−", "+"} {
		if rest, found := strings.CutPrefix(body, p); found {
			if p != "+" {
				sign = -1
			}
			body = strings.TrimSpace(rest)
			break
		}
	}
	var folded strings.Builder
	for _, r := range body {
		f, _ := foldChapterDigit(r)
		folded.WriteRune(f)
	}
	body = folded.String()
	if strings.Contains(body, ":") {
		ms, ok := parseChapterClock(body)
		if !ok {
			return 0, ErrBadChapterOffset
		}
		return sign * ms, nil
	}
	if !chapterOffsetNumberRe.MatchString(body) {
		return 0, ErrBadChapterOffset
	}
	sec, err := strconv.ParseFloat(body, 64)
	if err != nil || math.IsInf(sec, 0) || sec > 1e9 {
		return 0, ErrBadChapterOffset
	}
	return sign * int64(math.Round(sec*1000)), nil
}
