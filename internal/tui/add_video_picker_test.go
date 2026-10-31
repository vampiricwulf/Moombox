package tui

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"charm.land/bubbles/v2/table"
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
