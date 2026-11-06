package twitch

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestGetVodInfoRecordingPlaceholderAndUntitled pins three readings of
// VideoMetadata ported from yt-dlp's _extract_info_gql (twitch.py 513-526):
//
//   - a previewThumbnailURL that is Twitch's /404_processing_<size>.png
//     placeholder is dropped — never stored, never rewritten into a sized URL
//     that exists nowhere — whatever the broadcast type;
//   - behind that placeholder only an ARCHIVE is still recording: a
//     HIGHLIGHT shows it while it is processed and is not (yt-dlp #14455);
//   - an empty title is "Untitled Broadcast" — and, beyond yt-dlp, so is a
//     whitespace-only one; a title with text is kept as Twitch sent it.
//
// Mutants: storing the placeholder like any thumbnail fails the two
// placeholder rows' ThumbnailURL; IsRecording without the ARCHIVE check fails
// "highlight placeholder"; dropping the title default fails "untitled"; trimming the
// stored title fails "padded title kept as sent".
func TestGetVodInfoRecordingPlaceholderAndUntitled(t *testing.T) {
	const placeholder = "https://vod-secure.twitch.tv/_404/404_processing_320x180.png"
	cases := []struct {
		name          string
		title         string
		preview       string
		broadcastType string
		wantTitle     string
		wantThumb     string
		wantRecording bool
	}{
		{"recording archive", "Live now", placeholder, "ARCHIVE", "Live now", "", true},
		{"highlight placeholder", "Best of", placeholder, "HIGHLIGHT", "Best of", "", false},
		{"finished archive", "Done",
			"https://static-cdn.jtvnw.net/cf_vods/d1m7jfoe9zdc1j/abc/thumb/thumb0-%{width}x%{height}.jpg", "ARCHIVE",
			"Done", "https://static-cdn.jtvnw.net/cf_vods/d1m7jfoe9zdc1j/abc/thumb/thumb0-640x360.jpg", false},
		{"untitled", "", "", "ARCHIVE", "Untitled Broadcast", "", false},
		{"whitespace title", "   ", "", "ARCHIVE", "Untitled Broadcast", "", false},
		{"padded title kept as sent", " Late night ", "", "ARCHIVE", " Late night ", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"data": map[string]any{"video": map[string]any{
				"id":                  "2000000001",
				"title":               tc.title,
				"lengthSeconds":       3600,
				"previewThumbnailURL": tc.preview,
				"broadcastType":       tc.broadcastType,
				"owner":               map[string]any{"id": "1", "login": "chan", "displayName": "Chan"},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			installProbeStub(t, http.StatusOK, string(body))

			info, err := NewAPI(&testLogger{}).GetVodInfo(context.Background(), "2000000001", "")
			if err != nil {
				t.Fatalf("GetVodInfo: %v", err)
			}
			if info.Title != tc.wantTitle {
				t.Errorf("Title = %q, want %q", info.Title, tc.wantTitle)
			}
			if info.ThumbnailURL != tc.wantThumb {
				t.Errorf("ThumbnailURL = %q, want %q", info.ThumbnailURL, tc.wantThumb)
			}
			if info.IsRecording != tc.wantRecording {
				t.Errorf("IsRecording = %v, want %v", info.IsRecording, tc.wantRecording)
			}
		})
	}
}
