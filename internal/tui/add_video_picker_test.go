package tui

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// pickerAnswer is GET /api/formats' shape as cmd/moombox's formatsPayload
// writes it (its own test decodes the real payload into FormatsData): the
// rows carry the format's fields plus damaged/segmented, and the Best picks
// sit in a nested bestItags object, already past the marked renditions. The
// WEBM badge is on 248, below the damaged 313 and the segmented 271; every
// AAC is damaged, so its badge kept the first.
const pickerAnswer = `{
  "videoId": "dQw4w9WgXcQ", "title": "t", "channelName": "c", "lengthSeconds": 600,
  "videoFormats": [
    {"itag": 313, "mimeType": "video/webm; codecs=\"vp9\"", "bitrate": 20000000, "width": 3840, "height": 2160, "damaged": true, "segmented": false},
    {"itag": 271, "mimeType": "video/webm; codecs=\"vp9\"", "bitrate": 9000000, "width": 2560, "height": 1440, "damaged": false, "segmented": true},
    {"itag": 248, "mimeType": "video/webm; codecs=\"vp9\"", "bitrate": 4000000, "width": 1920, "height": 1080, "damaged": false, "segmented": false},
    {"itag": 137, "mimeType": "video/mp4; codecs=\"avc1.640028\"", "bitrate": 6000000, "width": 1920, "height": 1080, "damaged": true, "segmented": true},
    {"itag": 136, "mimeType": "video/mp4; codecs=\"avc1.4d401f\"", "bitrate": 2000000, "width": 1280, "height": 720, "damaged": false, "segmented": false}
  ],
  "audioFormats": [
    {"itag": 251, "mimeType": "audio/webm; codecs=\"opus\"", "bitrate": 160000, "audioSampleRate": "48000", "damaged": false, "segmented": false},
    {"itag": 140, "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "bitrate": 128000, "audioSampleRate": "44100", "damaged": true, "segmented": false},
    {"itag": 139, "mimeType": "audio/mp4; codecs=\"mp4a.40.5\"", "bitrate": 48000, "audioSampleRate": "22050", "damaged": true, "segmented": true}
  ],
  "bestItags": {"bestWebmVideo": 248, "bestMp4Video": 136, "bestOpusAudio": 251, "bestAacAudio": 140}
}`

// TestAddVideoPickerNotesTheBestAndTheMarks: the TUI's format tables showed
// no Best note at all — FormatsData read the picks as flat fields, where the
// answer nests them in bestItags, so the decode filled none — and nothing of
// the two marks the downloader acts on (a damaged rendition, encoded short;
// a segmented one, which a whole-file VOD cannot fetch). Through the real
// fetch and decode, each row's Note now carries the server's Best pick and
// then the marks, joined as yt-dlp joins a format_note.
//
// Mutants (each run): FormatsData's picks flat again, or BestItags not
// decoded (no "Best" in any Note); formatNote dropping either mark; the
// video or audio table writing the Best label alone.
func TestAddVideoPickerNotesTheBestAndTheMarks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/formats/dQw4w9WgXcQ" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(pickerAnswer))
	}))
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	a := NewApp()
	a.SetWebPort(func() int { return port })
	msg, ok := a.fetchFormatsCmd("dQw4w9WgXcQ")().(fetchFormatsResultMsg)
	if !ok || msg.Err != "" || msg.Formats == nil {
		t.Fatalf("fetch: %#v", msg)
	}
	m := NewAddVideoModel()
	m.SetFormats(msg.Formats)

	notes := func(rows []table.Row, col int) []string {
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = r[col]
		}
		return out
	}
	for _, tc := range []struct {
		table string
		got   []string
		want  []string
	}{
		{"video", notes(m.videoTable.Rows(), 5), []string{"damaged", "segmented", "Best WEBM", "damaged, segmented", "Best MP4"}},
		{"audio", notes(m.audioTable.Rows(), 4), []string{"Best OPUS", "Best AAC, damaged", "damaged, segmented"}},
	} {
		if len(tc.got) != len(tc.want) {
			t.Fatalf("%s notes = %q, want %q", tc.table, tc.got, tc.want)
		}
		for i := range tc.want {
			if tc.got[i] != tc.want[i] {
				t.Errorf("%s row %d note = %q, want %q", tc.table, i+1, tc.got[i], tc.want[i])
			}
		}
	}

	// The longest note there is still fits its column, so a marked Best row
	// never loses a mark to the table's truncation.
	if n := len(formatNote("Best WEBM", true, true)); n > noteColumnWidth {
		t.Errorf("the longest note is %d wide, its column %d", n, noteColumnWidth)
	}
}

// TestAddVideoPickerDrawsEveryRow: the format tables drew their header and
// its rule and not one row — a bubbles v2 table draws its rows through a
// viewport, which draws nothing at width 0, and neither table was given a
// width — so the Notes above, and every format, never reached the screen.
// Each row is drawn now, right under the rule, at a wide terminal and at
// the TUI's narrowest (minTermWidth), where the Note column narrows to what
// the dialog leaves instead of the box wrapping the table's every line.
//
// Mutants (each run): fitFormatTable without SetWidth (no row drawn); its
// height without the header's lines (the three-row audio table draws one);
// the Note column left at noteColumnWidth (at minTermWidth the rule wraps
// onto the line the first row belongs on).
func TestAddVideoPickerDrawsEveryRow(t *testing.T) {
	var formats FormatsData
	if err := json.Unmarshal([]byte(pickerAnswer), &formats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, width := range []int{minTermWidth, 100} {
		for _, tc := range []struct {
			name string
			step AddVideoStep
			rows [][]string // what each row, in order, shows
		}{
			{"video", AddStepVideoFormat, [][]string{
				{"3840x2160", "damaged"},
				{"2560x1440", "segmented"},
				{"1920x1080", "Best WEBM"},
				{"1920x1080", "damaged, segmented"},
				{"1280x720", "Best MP4"},
			}},
			{"audio", AddStepAudioFormat, [][]string{
				{"160kbps", "48kHz", "Best OPUS"},
				{"128kbps", "44kHz", "Best AAC, damaged"},
				{"48kbps", "22kHz", "damaged, segmented"},
			}},
		} {
			t.Run(fmt.Sprintf("%s at width %d", tc.name, width), func(t *testing.T) {
				m := NewAddVideoModel()
				m.Open()
				m.SetSize(width, 40)
				m.step = tc.step
				m.SetFormats(&formats)
				lines := strings.Split(ansi.Strip(m.View()), "\n")
				header := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "Note") })
				if header < 0 || header+1+len(tc.rows) >= len(lines) {
					t.Fatalf("no table header in the dialog:\n%s", strings.Join(lines, "\n"))
				}
				if rule := strings.Trim(lines[header+1], " │"); rule == "" || strings.Trim(rule, "─") != "" {
					t.Fatalf("line under the header = %q, want its rule:\n%s", lines[header+1], strings.Join(lines, "\n"))
				}
				for i, want := range tc.rows {
					got := lines[header+2+i]
					for _, cell := range want {
						if !strings.Contains(got, cell) {
							t.Errorf("row %d = %q, want it to show %q:\n%s", i+1, got, cell, strings.Join(lines, "\n"))
						}
					}
				}
			})
		}
	}
}

// TestAddVideoPickerOpensOnTheBestRow is the owner's first picker follow-up:
// both Add Video tables opened with the cursor on row 0, the top rendition
// whatever the Best picks said — a damaged 2160p one in the answer above —
// so Enter on a freshly opened table took what the dashboard's Add dialog
// never preselects. Each table now opens on the row the dashboard
// preselects (populateFormatSelects): the WEBM pick ahead of the MP4 one and
// the Opus ahead of the AAC, the first that is intact, else the first that
// exists; row 0 when there is no Best row. Enter takes it.
//
// Mutants (each run): no SetCursor on the video table, or on the audio one
// (that table's rows open on 0); preselectedRow ignoring intact (the
// damaged WEBM fallback is opened on over the intact MP4, the segmented
// Opus over the intact AAC); preselectedRow skipping a marked pick even
// when no pick is intact (the live rows open on 0, no Best row there).
//
// Owner decision FQ-1: the audio table opens on the [Best] row (BestAudio,
// automatic selection's pick of any codec) ahead of the codec picks, which
// only an answer without BestAudio falls to (every earlier case). Mutant:
// BestAudio dropped from the audio table's picks (the E-AC-3 case opens on
// the Opus row, 1).
func TestAddVideoPickerOpensOnTheBestRow(t *testing.T) {
	v := func(itag int, mime string, height int, damaged, segmented bool) VideoFormat {
		return VideoFormat{Itag: itag, MimeType: mime, Width: height * 16 / 9, Height: height, Damaged: damaged, Segmented: segmented}
	}
	a := func(itag int, mime string, damaged, segmented bool) AudioFormat {
		return AudioFormat{Itag: itag, MimeType: mime, Damaged: damaged, Segmented: segmented}
	}
	const webm, mp4, opus, aac = `video/webm; codecs="vp9"`, `video/mp4; codecs="avc1"`, `audio/webm; codecs="opus"`, `audio/mp4; codecs="mp4a.40.2"`
	for _, tc := range []struct {
		name                   string
		formats                FormatsData
		wantVideo              int // the row each table's cursor opens on
		wantAudio              int
		wantVideoI, wantAudioI int // the itag Enter then takes
	}{
		{name: "an intact WEBM and Opus badge below marked rows",
			formats: FormatsData{
				VideoFormats: []VideoFormat{v(313, webm, 2160, true, false), v(271, webm, 1440, false, true), v(248, webm, 1080, false, false), v(136, mp4, 720, false, false)},
				AudioFormats: []AudioFormat{a(251, opus, true, false), a(250, opus, false, false), a(140, aac, false, false)},
				BestItags:    BestItags{BestWebmVideo: 248, BestMp4Video: 136, BestOpusAudio: 250, BestAacAudio: 140},
			},
			wantVideo: 2, wantAudio: 1, wantVideoI: 248, wantAudioI: 250},
		{name: "a marked WEBM and Opus badge yield to an intact MP4 and AAC one",
			formats: FormatsData{
				VideoFormats: []VideoFormat{v(313, webm, 2160, true, false), v(137, mp4, 1080, false, false)},
				AudioFormats: []AudioFormat{a(251, opus, false, true), a(140, aac, false, false)},
				BestItags:    BestItags{BestWebmVideo: 313, BestMp4Video: 137, BestOpusAudio: 251, BestAacAudio: 140},
			},
			wantVideo: 1, wantAudio: 1, wantVideoI: 137, wantAudioI: 140},
		{name: "every badge marked, as on a live stream: the WEBM and Opus one",
			formats: FormatsData{
				StreamStatus: "live",
				VideoFormats: []VideoFormat{v(18, `video/mp4; codecs="avc1, mp4a"`, 360, false, false), v(303, webm, 1080, false, true), v(299, mp4, 1080, false, true)},
				AudioFormats: []AudioFormat{a(140, aac, false, true), a(251, opus, false, true)},
				BestItags:    BestItags{BestWebmVideo: 303, BestMp4Video: 299, BestOpusAudio: 251, BestAacAudio: 140},
			},
			wantVideo: 1, wantAudio: 1, wantVideoI: 303, wantAudioI: 251},
		{name: "the [Best] audio pick, an E-AC-3 track above the Opus and AAC badges",
			formats: FormatsData{
				StreamStatus: "vod",
				VideoFormats: []VideoFormat{v(248, webm, 1080, false, false)},
				AudioFormats: []AudioFormat{a(251, opus, false, false), a(328, `audio/mp4; codecs="ec-3"`, false, false), a(140, aac, false, false)},
				BestItags:    BestItags{BestWebmVideo: 248, BestOpusAudio: 251, BestAacAudio: 140, BestAudio: 328},
			},
			wantVideo: 0, wantAudio: 1, wantVideoI: 248, wantAudioI: 328},
		{name: "no Best row: row 0",
			formats: FormatsData{
				VideoFormats: []VideoFormat{v(248, webm, 1080, false, false), v(136, mp4, 720, false, false)},
				AudioFormats: []AudioFormat{a(251, opus, false, false), a(140, aac, false, false)},
			},
			wantVideo: 0, wantAudio: 0, wantVideoI: 248, wantAudioI: 251},
		{name: "picks that match no row: row 0",
			formats: FormatsData{
				VideoFormats: []VideoFormat{v(248, webm, 1080, false, false), v(136, mp4, 720, false, false)},
				AudioFormats: []AudioFormat{a(251, opus, false, false), a(140, aac, false, false)},
				BestItags:    BestItags{BestWebmVideo: 999, BestOpusAudio: 998},
			},
			wantVideo: 0, wantAudio: 0, wantVideoI: 248, wantAudioI: 251},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewAddVideoModel()
			m.Open()
			m.SetSize(100, 40)
			m.step = AddStepVideoFormat
			m.SetFormats(&tc.formats)
			if got := m.videoTable.Cursor(); got != tc.wantVideo {
				t.Errorf("video table opens on row %d, want %d", got, tc.wantVideo)
			}
			if got := m.audioTable.Cursor(); got != tc.wantAudio {
				t.Errorf("audio table opens on row %d, want %d", got, tc.wantAudio)
			}
			m.View()
			m.HandleKey(keyEnter)
			m.View()
			m.HandleKey(keyEnter)
			if m.selectedVideoItag == nil || *m.selectedVideoItag != tc.wantVideoI {
				t.Errorf("Enter took video %v, want itag %d", m.selectedVideoItag, tc.wantVideoI)
			}
			if m.selectedAudioItag == nil || *m.selectedAudioItag != tc.wantAudioI {
				t.Errorf("Enter took audio %v, want itag %d", m.selectedAudioItag, tc.wantAudioI)
			}
		})
	}
}

// TestAddVideoPickerDrawsTheBestRowHighlighted: the cursor placed on a Best
// row at or past the table's drawn height sat one row below the last row
// drawn — the table draws from the cursor less its height on, and SetCursor
// leaves the viewport's offset at 0 — so the row Enter would take was not
// on screen, and nothing was highlighted. On a short terminal, with the Best
// row eleventh, the table now draws that row, highlighted, on its last line,
// and the operator's Down and Up from there move the highlight a drawn row
// at a time, as the table's own scrolling always did.
//
// Mutant: fitFormatTable without its MoveDown(0) (no row is drawn
// highlighted, and the Best row is not drawn).
func TestAddVideoPickerDrawsTheBestRowHighlighted(t *testing.T) {
	var formats FormatsData
	for i := range 12 {
		f := VideoFormat{Itag: 400 + i, MimeType: `video/webm; codecs="vp9"`, Width: 3840 - 160*i, Height: 2160 - 90*i, Damaged: i < 10}
		formats.VideoFormats = append(formats.VideoFormats, f)
	}
	formats.BestItags.BestWebmVideo = 410 // the first intact WEBM, the eleventh row
	formats.AudioFormats = []AudioFormat{{Itag: 251, MimeType: `audio/webm; codecs="opus"`}}

	m := NewAddVideoModel()
	m.Open()
	m.SetSize(100, 24) // the dialog's box is 20 high, the table's rows 10
	m.step = AddStepVideoFormat
	m.SetFormats(&formats)
	if got := m.videoTable.Cursor(); got != 10 {
		t.Fatalf("cursor on row %d, want 10", got)
	}

	sel := formatTableStyles().Selected.Render("\x00")
	selPrefix, _, _ := strings.Cut(sel, "\x00")
	if selPrefix == "" {
		t.Fatal("the Selected style renders no escape to find the cursor by")
	}
	// highlighted draws the dialog and answers the # cell of the one row
	// drawn highlighted, "" for none.
	highlighted := func() string {
		raw := strings.Split(m.View(), "\n")
		var rows []string
		for _, l := range raw {
			if strings.Contains(l, selPrefix) {
				rows = append(rows, strings.Fields(strings.Trim(ansi.Strip(l), " │"))[0])
			}
		}
		if len(rows) != 1 {
			t.Fatalf("%d rows drawn highlighted, want one:\n%s", len(rows), ansi.Strip(strings.Join(raw, "\n")))
		}
		return rows[0]
	}
	if got := highlighted(); got != "11" {
		t.Errorf("row %s drawn highlighted, want the Best row, 11", got)
	}
	if !strings.Contains(ansi.Strip(m.View()), "Best WEBM") {
		t.Errorf("the Best row is not drawn:\n%s", ansi.Strip(m.View()))
	}
	// The operator's own moves from there are the table's, each drawn.
	for _, step := range []struct {
		key  rune
		want string
	}{{tea.KeyDown, "12"}, {tea.KeyUp, "11"}, {tea.KeyUp, "10"}, {tea.KeyUp, "9"}} {
		m.UpdateComponents(tea.KeyPressMsg{Code: step.key})
		if got := highlighted(); got != step.want {
			t.Errorf("after %v, row %s drawn highlighted, want %s", step.key, got, step.want)
		}
	}
}

// TestAddVideoPickerHidesTheSegmentedNoteWhileOnAir is the owner's third
// picker follow-up, on the TUI: while a stream is live or upcoming every
// adaptive rendition it lists is segmented — the live path fetches exactly
// those — so the Note column's "segmented" on every row said nothing. The
// note is left off for a live or upcoming stream (the answer's
// streamStatus, decoded into FormatsData through the real fetch), and kept
// for a VOD, a finished stream and a status the answer did not state. The
// damaged note and the Best picks are the same for every status.
//
// Mutants (each run): notesSegmented answering true for "live", or for
// "upcoming" (that row's Notes say segmented); answering false for
// "post_live" or "vod" (theirs do not); FormatsData without StreamStatus
// (every status notes segmented); either table passing f.Segmented
// unfiltered (that table's live notes say segmented).
func TestAddVideoPickerHidesTheSegmentedNoteWhileOnAir(t *testing.T) {
	for _, tc := range []struct {
		status    string
		wantVideo []string
		wantAudio []string
	}{
		{"live", []string{"damaged", "", "Best WEBM", "damaged", "Best MP4"}, []string{"Best OPUS", "Best AAC, damaged", "damaged"}},
		{"upcoming", []string{"damaged", "", "Best WEBM", "damaged", "Best MP4"}, []string{"Best OPUS", "Best AAC, damaged", "damaged"}},
		{"vod", []string{"damaged", "segmented", "Best WEBM", "damaged, segmented", "Best MP4"}, []string{"Best OPUS", "Best AAC, damaged", "damaged, segmented"}},
		{"post_live", []string{"damaged", "segmented", "Best WEBM", "damaged, segmented", "Best MP4"}, []string{"Best OPUS", "Best AAC, damaged", "damaged, segmented"}},
		{"not_a_stream", []string{"damaged", "segmented", "Best WEBM", "damaged, segmented", "Best MP4"}, []string{"Best OPUS", "Best AAC, damaged", "damaged, segmented"}},
		{"", []string{"damaged", "segmented", "Best WEBM", "damaged, segmented", "Best MP4"}, []string{"Best OPUS", "Best AAC, damaged", "damaged, segmented"}},
	} {
		t.Run(fmt.Sprintf("streamStatus %q", tc.status), func(t *testing.T) {
			answer := pickerAnswer
			if tc.status != "" {
				answer = strings.Replace(pickerAnswer, `"lengthSeconds": 600,`, `"lengthSeconds": 600, "streamStatus": "`+tc.status+`",`, 1)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(answer))
			}))
			defer srv.Close()
			_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
			port, _ := strconv.Atoi(portStr)
			app := NewApp()
			app.SetWebPort(func() int { return port })
			msg, ok := app.fetchFormatsCmd("dQw4w9WgXcQ")().(fetchFormatsResultMsg)
			if !ok || msg.Err != "" || msg.Formats == nil {
				t.Fatalf("fetch: %#v", msg)
			}
			if msg.Formats.StreamStatus != tc.status {
				t.Fatalf("decoded streamStatus %q, want %q", msg.Formats.StreamStatus, tc.status)
			}
			m := NewAddVideoModel()
			m.SetFormats(msg.Formats)
			for _, tbl := range []struct {
				name string
				rows []table.Row
				want []string
			}{
				{"video", m.videoTable.Rows(), tc.wantVideo},
				{"audio", m.audioTable.Rows(), tc.wantAudio},
			} {
				got := make([]string, len(tbl.rows))
				for i, r := range tbl.rows {
					got[i] = r[len(r)-1]
				}
				if !slices.Equal(got, tbl.want) {
					t.Errorf("%s notes = %q, want %q", tbl.name, got, tbl.want)
				}
			}
			// The hiding is the note's alone: the cursor still opens on the
			// intact Best WEBM (row 2), past the segmented 271.
			if got := m.videoTable.Cursor(); got != 2 {
				t.Errorf("video cursor on row %d, want 2", got)
			}
		})
	}
}

// TestAddVideoPickerNotesTheAutomaticAudioPick is owner decision FE-10 in the
// TUI: the audio row automatic selection takes (bestItags.bestAudio) is noted
// "Best" — a 5.1 E-AC-3 track, which neither codec pick names, alone; a track
// that is also its codec's pick carries both — and the E-AC-3 track's Codec
// column says EC-3, where it read blank. The longest audio note still fits
// its column, and the audio table is still the narrower of the two.
//
// Mutants (each run): the "Best" note left out (row 0 reads ""); the codec
// note replacing it rather than joining it (row 1 reads "Best OPUS");
// audioCodecLabel without EC-3 (row 0's codec blank); the audio Note column
// at noteColumnWidth (the longest note does not fit).
func TestAddVideoPickerNotesTheAutomaticAudioPick(t *testing.T) {
	for _, tc := range []struct {
		best      int
		wantNotes []string
	}{
		{328, []string{"Best", "Best OPUS", "Best AAC"}},
		{251, []string{"", "Best, Best OPUS", "Best AAC"}},
	} {
		m := NewAddVideoModel()
		m.SetFormats(&FormatsData{
			StreamStatus: "vod",
			AudioFormats: []AudioFormat{
				{Itag: 328, MimeType: `audio/mp4; codecs="ec-3"`, Bitrate: 384000},
				{Itag: 251, MimeType: `audio/webm; codecs="opus"`, Bitrate: 160000},
				{Itag: 140, MimeType: `audio/mp4; codecs="mp4a.40.2"`, Bitrate: 128000},
			},
			BestItags: BestItags{BestOpusAudio: 251, BestAacAudio: 140, BestAudio: tc.best},
		})
		rows := m.audioTable.Rows()
		for i, want := range tc.wantNotes {
			if rows[i][4] != want {
				t.Errorf("best %d: row %d note = %q, want %q", tc.best, i, rows[i][4], want)
			}
		}
		if rows[0][3] != "EC-3" || rows[1][3] != "OPUS" || rows[2][3] != "AAC" {
			t.Errorf("codecs = %q %q %q, want EC-3, OPUS, AAC", rows[0][3], rows[1][3], rows[2][3])
		}
	}
	m := NewAddVideoModel()
	m.SetFormats(&FormatsData{AudioFormats: []AudioFormat{{Itag: 1}}})
	cols := m.audioTable.Columns()
	if n := len(formatNote("Best, Best OPUS", true, true)); n > cols[len(cols)-1].Width {
		t.Errorf("the longest audio note is %d wide, its column %d", n, cols[len(cols)-1].Width)
	}
	audioW, videoW := 0, 0
	for _, c := range cols {
		audioW += c.Width
	}
	m.SetFormats(&FormatsData{VideoFormats: []VideoFormat{{Itag: 1}}})
	for _, c := range m.videoTable.Columns() {
		videoW += c.Width
	}
	if audioW > videoW {
		t.Errorf("the audio table is %d wide, the video table %d", audioW, videoW)
	}
}
