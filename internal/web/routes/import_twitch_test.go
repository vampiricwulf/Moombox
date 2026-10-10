package routes

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// twitchChatJSON is a Moombox Twitch chat archive's header
// (twitch.TwitchChatData) with one message.
func twitchChatJSON(t *testing.T, login, display, streamID string) []byte {
	t.Helper()
	return chatJSONFor(t, map[string]any{
		"platform":           "twitch",
		"channelLogin":       login,
		"channelDisplayName": display,
		"streamId":           streamID,
		"downloadedAt":       "2026-10-01T00:00:00Z",
		"messageCount":       1,
	})
}

// W25-03: a Moombox Twitch archive — named with its "tw_" job id, its chat a
// TwitchChatData — imported as platform youtube, channel "Import", a
// YouTube watch URL for an "imp_" id, and its "[tw_…]" id left in the title
// and doubled in the file name. It is a Twitch row now: platform twitch, the
// channel from the chat header (display name, else login), the title less
// its id, and the VOD's page when the id names one — never a YouTube URL or
// thumbnail.
//
// Mutants: dropping the tw_ alternative from importNameIDRe (an imp_ id,
// "[tw_…]" kept in the title); detecting Twitch from the chat header only
// (the chat-less archive is youtube) or from the id only (the id-less archive
// is youtube); dropping the channelDisplayName or channelLogin fallback;
// building no VOD URL, or one for a live capture's stream id; leaving isVod
// unset; keeping the YouTube thumbnail.
func TestImportRecognisesATwitchArchive(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		entries                  []importEntry
		wantID, wantTitle, wantC string
		wantURL                  string
		wantVod                  bool
	}{
		{"a VOD archive", []importEntry{
			{name: "Just Chatting [tw_v2134567890].mp4", data: []byte("v")},
			{name: "Just Chatting [tw_v2134567890].chat.json", data: twitchChatJSON(t, "somestreamer", "SomeStreamer", "2134567890")},
		}, "tw_v2134567890", "Just Chatting", "SomeStreamer", "https://www.twitch.tv/videos/2134567890", true},
		{"a live capture, no display name", []importEntry{
			{name: "Late Night [tw_316543210987].mp4", data: []byte("v")},
			{name: "Late Night [tw_316543210987].chat.json", data: twitchChatJSON(t, "somestreamer", "", "316543210987")},
		}, "tw_316543210987", "Late Night", "somestreamer", "", false},
		{"no chat: the id alone", []importEntry{
			{name: "Speedrun [tw_v42].mp4", data: []byte("v")},
		}, "tw_v42", "Speedrun", "Import", "https://www.twitch.tv/videos/42", true},
		{"no id: the chat header alone", []importEntry{
			{name: "recording.mp4", data: []byte("v")},
			{name: "recording.chat.json", data: twitchChatJSON(t, "somestreamer", "SomeStreamer", "99")},
		}, "", "recording", "SomeStreamer", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newImportFixture(t)
			rec, job := importZip(t, f, orderedImportZip(t, tc.entries...))
			if rec.Code != http.StatusCreated {
				t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
			}
			if job.Platform != "twitch" {
				t.Errorf("platform %q, want twitch", job.Platform)
			}
			if tc.wantID != "" && job.ID != tc.wantID {
				t.Errorf("id %q, want %q", job.ID, tc.wantID)
			}
			if tc.wantID == "" && !strings.HasPrefix(job.ID, "imp_") {
				t.Errorf("id %q, want the imp_ placeholder", job.ID)
			}
			if job.Title != tc.wantTitle || job.ChannelName != tc.wantC {
				t.Errorf("title %q channel %q, want %q / %q", job.Title, job.ChannelName, tc.wantTitle, tc.wantC)
			}
			if job.URL != tc.wantURL || job.IsVod != tc.wantVod {
				t.Errorf("url %q isVod %v, want %q / %v", job.URL, job.IsVod, tc.wantURL, tc.wantVod)
			}
			if job.ThumbnailURL != "" {
				t.Errorf("thumbnail %q on a Twitch row", job.ThumbnailURL)
			}
			if strings.Count(job.Filename, job.ID) != 1 {
				t.Errorf("filename %q carries the id %q other than once", job.Filename, job.ID)
			}
		})
	}
}

// Only Moombox's own Twitch ids say an archive is Twitch's: "tw_v<vod id>",
// "tw_<stream id>" and "tw_manual_<login>_<n>". Any "tw_" id did, and
// "tw_abcdEFGH" is an ordinary 11-character YouTube id (about one in 262,000
// start so): its archive imported as a Twitch row with no URL and no
// thumbnail, its YouTube chat header ignored. A YouTube header — a videoId
// and no platform — also outweighs a Twitch-shaped id, since "tw_12345678"
// is 11 characters too.
//
// Mutants: detecting Twitch by the "tw_" prefix again (the header-less
// "tw_abcdEFGH" is twitch); ignoring the YouTube header ("tw_12345678" with
// one is twitch); dropping the manual add's shape from the pattern (its id
// is not taken, and stays in the title).
func TestImportTakesOnlyMoomboxTwitchIDsForTwitch(t *testing.T) {
	ytChat := func(id string) []byte {
		return chatJSONFor(t, map[string]any{"videoId": id, "videoTitle": "Song", "channelName": "Chan"})
	}
	for _, tc := range []struct {
		name         string
		entries      []importEntry
		wantID       string
		wantPlatform string
		wantURL      string
	}{
		{"a tw_ YouTube id, no chat", []importEntry{
			{name: "Song [tw_abcdEFGH].mp4", data: []byte("v")},
		}, "tw_abcdEFGH", "youtube", "https://www.youtube.com/watch?v=tw_abcdEFGH"},
		{"a tw_ YouTube id with its chat", []importEntry{
			{name: "Song [tw_abcdEFGH].mp4", data: []byte("v")},
			{name: "Song [tw_abcdEFGH].chat.json", data: ytChat("tw_abcdEFGH")},
		}, "tw_abcdEFGH", "youtube", "https://www.youtube.com/watch?v=tw_abcdEFGH"},
		{"a Twitch-shaped YouTube id with its chat", []importEntry{
			{name: "Song [tw_12345678].mp4", data: []byte("v")},
			{name: "Song [tw_12345678].chat.json", data: ytChat("tw_12345678")},
		}, "tw_12345678", "youtube", "https://www.youtube.com/watch?v=tw_12345678"},
		{"a manual add's capture", []importEntry{
			{name: "Late [tw_manual_some_streamer_1759000000000000000].mp4", data: []byte("v")},
		}, "tw_manual_some_streamer_1759000000000000000", "twitch", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newImportFixture(t)
			rec, job := importZip(t, f, orderedImportZip(t, tc.entries...))
			if rec.Code != http.StatusCreated {
				t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
			}
			if job.ID != tc.wantID || job.Platform != tc.wantPlatform || job.URL != tc.wantURL {
				t.Errorf("id %q platform %q url %q, want %q / %q / %q",
					job.ID, job.Platform, job.URL, tc.wantID, tc.wantPlatform, tc.wantURL)
			}
			if strings.Contains(job.Title, "[") {
				t.Errorf("title %q keeps its id", job.Title)
			}
			if tc.wantPlatform == "youtube" && job.ThumbnailURL != "https://i.ytimg.com/vi/"+tc.wantID+"/maxresdefault.jpg" {
				t.Errorf("thumbnail %q", job.ThumbnailURL)
			}
		})
	}
}

// A YouTube archive is untouched by the Twitch detection.
func TestImportKeepsAYouTubeArchiveYouTube(t *testing.T) {
	f := newImportFixture(t)
	rec, job := importZip(t, f, orderedImportZip(t,
		importEntry{name: "Stream [dQw4w9WgXcQ].mp4", data: []byte("v")},
		importEntry{name: "Stream [dQw4w9WgXcQ].chat.json", data: chatJSONFor(t, map[string]any{
			"videoId": "dQw4w9WgXcQ", "videoTitle": "Stream", "channelName": "Chan",
		})},
	))
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d (body %s)", rec.Code, rec.Body.String())
	}
	if job.Platform != "youtube" || job.URL != "https://www.youtube.com/watch?v=dQw4w9WgXcQ" || job.ChannelName != "Chan" ||
		job.ThumbnailURL != "https://i.ytimg.com/vi/dQw4w9WgXcQ/maxresdefault.jpg" {
		t.Errorf("platform %q url %q channel %q thumbnail %q", job.Platform, job.URL, job.ChannelName, job.ThumbnailURL)
	}
}

// A Twitch chat's header stops the read too: TwitchChatData writes platform,
// channelLogin and channelDisplayName ahead of its messages, and a long
// stream's chat runs to hundreds of MB.
//
// Mutant: stopping only on the YouTube header set (the Twitch archive is
// read to its end).
func TestImportChatMetaStopsAfterATwitchHeader(t *testing.T) {
	header := `{"platform":"twitch","channelLogin":"somestreamer","channelDisplayName":"SomeStreamer","streamId":"1","messages":[`
	body := strings.Repeat(`{"id":"m","message":"hello there"},`, 300_000) + `{"id":"last"}]}`
	cr := &countingReader{r: io.MultiReader(strings.NewReader(header), strings.NewReader(body))}

	got := readImportChatMeta(cr)
	if got != (importChatMeta{Platform: "twitch", ChannelLogin: "somestreamer", ChannelDisplayName: "SomeStreamer"}) {
		t.Errorf("meta = %+v", got)
	}
	if cr.n > 64<<10 {
		t.Errorf("read %d bytes of a %d-byte archive for its header", cr.n, len(header)+len(body))
	}
}
