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
