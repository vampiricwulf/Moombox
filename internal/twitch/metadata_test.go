package twitch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// stubGQL answers every request to the GQL endpoint with reply(body) and
// records the request headers. Other hosts are refused.
func stubGQL(t *testing.T, reply func(body string) string) *[]http.Header {
	t.Helper()
	var headers []http.Header
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(req.URL.String(), constants.TwitchURLs.GQL) {
			return nil, errUnexpectedStubHost
		}
		headers = append(headers, req.Header.Clone())
		var b []byte
		if req.Body != nil {
			b, _ = io.ReadAll(req.Body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(reply(string(b)))),
			Request:    req,
		}, nil
	})}
	return &headers
}

const vodMetadataReply = `{"data":{"video":{"id":"123","title":"Big day","description":"All the games.","lengthSeconds":3600,"createdAt":"2026-10-01T00:00:00Z","owner":{"id":"1","login":"streamer","displayName":"Streamer"},"game":{"displayName":"Just Chatting"}}}}`

// TestGetVodInfoReadsDescriptionAndChapters is G68 and owner decision D23:
// GetVodInfo batches VideoPlayer_ChapterSelectButtonVideo beside
// VideoMetadata, as yt-dlp's _download_info does, and returns the VOD's
// description and one chapter per moment (yt-dlp's _extract_chapters), the
// moment missing a time skipped.
//
// Mutants: the chapter op left out of the batch (no chapters); description
// not decoded; a moment with no duration kept.
func TestGetVodInfoReadsDescriptionAndChapters(t *testing.T) {
	var sent string
	stubGQL(t, func(body string) string {
		sent = body
		return `[` + vodMetadataReply + `,{"data":{"video":{"moments":{"edges":[
			{"node":{"positionMilliseconds":0,"durationMilliseconds":600000,"description":"Just Chatting"}},
			{"node":{"positionMilliseconds":600000,"description":"no duration"}},
			{"node":{"positionMilliseconds":600000,"durationMilliseconds":3000000,"description":"Elden Ring"}}]}}}}]`
	})
	info, err := NewAPI(&testLogger{}).GetVodInfo(context.Background(), "123", "")
	if err != nil {
		t.Fatal(err)
	}
	var ops []gqlPersistedQuery
	if err := json.Unmarshal([]byte(sent), &ops); err != nil || len(ops) != 2 ||
		ops[1].OperationName != "VideoPlayer_ChapterSelectButtonVideo" ||
		ops[1].Extensions.PersistedQuery.Hash != constants.TwitchGQLHashes.VideoPlayerChapterSelectButtonVideo {
		t.Fatalf("GetVodInfo sent %s, want VideoMetadata and VideoPlayer_ChapterSelectButtonVideo batched", sent)
	}
	if info.Description != "All the games." {
		t.Errorf("Description = %q", info.Description)
	}
	want := []utils.Chapter{
		{StartMs: 0, EndMs: 600000, Title: "Just Chatting"},
		{StartMs: 600000, EndMs: 3600000, Title: "Elden Ring"},
	}
	if !reflect.DeepEqual(info.Chapters, want) {
		t.Errorf("Chapters = %+v, want %+v", info.Chapters, want)
	}
}

// TestGetVodInfoChapterOpFailureKeepsTheMetadata: the chapter half is best
// effort. An errored chapter element still returns the VOD, with yt-dlp's
// no-moments fallback — the whole video as one chapter named after its game —
// and so does a reply that is a single object.
//
// Mutants: the errored element failing GetVodInfo; the no-moments fallback
// removed (no chapters); a single-object reply refused; the game chapter
// ended at lengthSeconds (a VOD still recording loses its tail).
func TestGetVodInfoChapterOpFailureKeepsTheMetadata(t *testing.T) {
	for name, reply := range map[string]string{
		"errored element": `[` + vodMetadataReply + `,{"errors":[{"message":"PersistedQueryNotFound"}]}]`,
		"single object":   vodMetadataReply,
	} {
		t.Run(name, func(t *testing.T) {
			stubGQL(t, func(string) string { return reply })
			info, err := NewAPI(&testLogger{}).GetVodInfo(context.Background(), "123", "")
			if err != nil {
				t.Fatal(err)
			}
			// Open-ended, as upstream yields it: the mux ends it at the
			// archive's span, not at a still-recording VOD's length so far.
			want := []utils.Chapter{{StartMs: 0, Title: "Just Chatting"}}
			if info.Title != "Big day" || !reflect.DeepEqual(info.Chapters, want) {
				t.Errorf("info = %q, %+v; want the metadata and %+v", info.Title, info.Chapters, want)
			}
		})
	}
}

// TestOriginalSizeThumbnailURL is yt-dlp's _get_thumbnails rewrite (G45): the
// size before the extension becomes 0x0, a query or fragment is kept, and a
// URL with no size yields nothing to try.
//
// Mutant: the `[?#]` alternative dropped — a URL with a query has no 0x0 form.
func TestOriginalSizeThumbnailURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://static-cdn.jtvnw.net/previews-ttv/live_user_x-640x360.jpg":      "https://static-cdn.jtvnw.net/previews-ttv/live_user_x-0x0.jpg",
		"https://static-cdn.jtvnw.net/cf_vods/abc/thumb/thumb0-640x360.jpg?t=1":  "https://static-cdn.jtvnw.net/cf_vods/abc/thumb/thumb0-0x0.jpg?t=1",
		"https://static-cdn.jtvnw.net/cf_vods/abc/thumb/thumb0-0x0.jpg":          "",
		"https://static-cdn.jtvnw.net/jtv_user_pictures/x-profile_image-300.png": "",
	} {
		if got := OriginalSizeThumbnailURL(in); got != want {
			t.Errorf("OriginalSizeThumbnailURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseStreamInfoTitleFallbackOrder is G69: with StreamMetadata's own
// stream.title empty, the title comes from Comscore user.broadcastSettings
// (yt-dlp's path), then StreamMetadata user.lastBroadcast (streamlink's), then
// the user.stream.broadcaster path read before; the category from
// stream.game's displayName or name, then the Comscore games.
//
// Mutants: the yt-dlp Comscore path not read; lastBroadcast not read; the
// order of the two swapped; game.name not read.
func TestParseStreamInfoTitleFallbackOrder(t *testing.T) {
	a := NewAPI(&testLogger{})
	const legacy = `"stream":{"broadcaster":{"broadcastSettings":{"title":"legacy title","game":{"displayName":"Legacy Game"}}}}`
	cases := []struct {
		name, sm, cs, title, game string
	}{
		{"stream title first",
			`{"data":{"user":{"login":"x","lastBroadcast":{"title":"last"},"stream":{"id":"1","title":"own","game":{"displayName":"Own Game"}}}}}`,
			`{"data":{"user":{"broadcastSettings":{"title":"yt-dlp title"},` + legacy + `}}}`, "own", "Own Game"},
		{"yt-dlp comscore path second",
			`{"data":{"user":{"login":"x","lastBroadcast":{"title":"last"},"stream":{"id":"1","title":"","game":{"name":"Named Game"}}}}}`,
			`{"data":{"user":{"broadcastSettings":{"title":"yt-dlp title","game":{"displayName":"CS Game"}},` + legacy + `}}}`, "yt-dlp title", "Named Game"},
		{"lastBroadcast third",
			`{"data":{"user":{"login":"x","lastBroadcast":{"title":"last"},"stream":{"id":"1"}}}}`,
			`{"data":{"user":{"broadcastSettings":{"title":"","game":{"name":"CS Game"}},` + legacy + `}}}`, "last", "CS Game"},
		{"legacy path last",
			`{"data":{"user":{"login":"x","stream":{"id":"1"}}}}`,
			`{"data":{"user":{` + legacy + `}}}`, "legacy title", "Legacy Game"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info, err := a.parseStreamInfo("x", json.RawMessage(c.sm), json.RawMessage(c.cs))
			if err != nil || info == nil {
				t.Fatalf("parseStreamInfo = %+v, %v", info, err)
			}
			if info.Title != c.title || info.GameCategory != c.game {
				t.Errorf("title, category = %q, %q; want %q, %q", info.Title, info.GameCategory, c.title, c.game)
			}
		})
	}
}

// TestClientIDOverride is G70: the built-in Client-ID is
// constants.TwitchGQLClientID (the production value, pinned), a configured
// override replaces it on GQL calls, and clearing it restores the default.
//
// Mutants: doGQLOnce, or FetchHLSMasterPlaylist, still sending the constant;
// SetClientID("") leaving the override in place.
func TestClientIDOverride(t *testing.T) {
	t.Cleanup(func() { SetClientID("") })
	if ClientID() != constants.TwitchGQLClientID || constants.TwitchGQLClientID != "kimne78kx3ncx6brgo4mv6wki5h1ko" {
		t.Fatalf("default Client-ID = %q", ClientID())
	}
	headers := stubGQL(t, func(string) string { return `{"data":{}}` })
	SetClientID("  ue6666qo983tsx6so1t0vnawi233wa ")
	if _, err := NewAPI(&testLogger{}).gqlRequest(context.Background(), "T", map[string]any{}, ""); err != nil {
		t.Fatal(err)
	}
	if got := (*headers)[0].Get("Client-ID"); got != "ue6666qo983tsx6so1t0vnawi233wa" {
		t.Errorf("GQL Client-ID = %q, want the override", got)
	}
	if _, err := FetchHLSMasterPlaylist(context.Background(), constants.TwitchURLs.GQL+"/master.m3u8"); err != nil {
		t.Fatal(err)
	}
	if got := (*headers)[1].Get("Client-ID"); got != "ue6666qo983tsx6so1t0vnawi233wa" {
		t.Errorf("Usher playlist Client-ID = %q, want the override", got)
	}
	SetClientID("")
	if ClientID() != constants.TwitchGQLClientID {
		t.Errorf("after clearing, Client-ID = %q, want the default", ClientID())
	}
}
