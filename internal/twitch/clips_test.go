package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// clipReply is a ShareClipRenderStatus answer modelled on yt-dlp's first
// TwitchClipsIE test case (FaintLightGullWholeWheat, id 396245304, "EA Play
// 2016 Live from the Novo Theatre", 32 s, broadcaster EA, curator
// stereotype_), with a portrait asset beside the default one, a quality
// whose URL is no URL, and a third asset yt-dlp does not read.
const clipReply = `{"data":{"clip":{
	"id":"396245304","slug":"FaintLightGullWholeWheat",
	"title":"EA Play 2016 Live from the Novo Theatre",
	"durationSeconds":32,"viewCount":"1234","createdAt":"2016-06-12T21:36:33Z",
	"thumbnailURL":"https://clips-media-assets2.twitch.tv/small-480x272.jpg",
	"broadcaster":{"id":"25163635","login":"EA","displayName":"EA"},
	"curator":{"id":"43566419","login":"stereotype_","displayName":"stereotype_"},
	"game":{"displayName":"Just Chatting"},
	"playbackAccessToken":{"signature":"sig123","value":"{\"clip_uri\":\"x\"}"},
	"assets":[
		{"aspectRatio":1.77,"thumbnailURL":"https://clips-media-assets2.twitch.tv/default-preview.jpg","videoQualities":[
			{"sourceURL":"https://production.assets.clips.twitchcdn.net/v2/media/A/video-720.mp4","quality":"720","frameRate":60},
			{"sourceURL":"https://production.assets.clips.twitchcdn.net/v2/media/A/video-1080.mp4?x=1","quality":"1080","frameRate":"30"},
			{"sourceURL":"not a url","quality":"1440","frameRate":60}
		]},
		{"aspectRatio":0.56,"thumbnailURL":"https://clips-media-assets2.twitch.tv/portrait-preview.jpg","videoQualities":[
			{"sourceURL":"https://production.assets.clips.twitchcdn.net/v2/media/A/portrait-1920.mp4","quality":"1920","frameRate":60}
		]},
		{"aspectRatio":1.0,"thumbnailURL":"https://clips-media-assets2.twitch.tv/third-preview.jpg","videoQualities":[
			{"sourceURL":"https://production.assets.clips.twitchcdn.net/v2/media/A/third-2160.mp4","quality":"2160","frameRate":60}
		]}
	]}}}`

// TestGetClipReadsShareClipRenderStatus pins the port of yt-dlp's
// TwitchClipsIE._real_extract (twitch.py b77c9304e 1214-1336): the request is
// the ShareClipRenderStatus persisted query by the one hash in
// constants.TwitchGQLHashes, with the slug as its only variable; every
// rendition's URL carries the playback access token's signature and value as
// `sig` and `token`; a quality whose sourceURL is no URL is skipped; the
// portrait asset's renditions are marked; the metadata is the clip's; and the
// default asset's thumbnail outranks the clip's small one.
//
// Mutants: the URL left unsigned (no sig/token); the hash taken from another
// op; the portrait flag dropped; the url_or_none guard dropped (the "not a
// url" quality kept); the small thumbnail preferred to the default asset's;
// the assets[1] bound dropped (the third asset's taller 2160 rendition
// listed, as a landscape one that outranks the real clip).
func TestGetClipReadsShareClipRenderStatus(t *testing.T) {
	var sent string
	stubGQL(t, func(body string) string { sent = body; return clipReply })

	info, err := NewAPI(&testLogger{}).GetClip(context.Background(), "FaintLightGullWholeWheat", "")
	if err != nil {
		t.Fatalf("GetClip: %v", err)
	}

	var q gqlPersistedQuery
	if err := json.Unmarshal([]byte(sent), &q); err != nil {
		t.Fatalf("request is not one persisted query: %v (%s)", err, sent)
	}
	if q.OperationName != "ShareClipRenderStatus" || q.Extensions.PersistedQuery.Hash != constants.TwitchGQLHashes.ShareClipRenderStatus {
		t.Errorf("request = %s / %s, want ShareClipRenderStatus by its hash", q.OperationName, q.Extensions.PersistedQuery.Hash)
	}
	if vars, _ := q.Variables.(map[string]any); vars["slug"] != "FaintLightGullWholeWheat" {
		t.Errorf("variables = %v, want the slug", q.Variables)
	}

	if info.ID != "396245304" || info.Slug != "FaintLightGullWholeWheat" ||
		info.Title != "EA Play 2016 Live from the Novo Theatre" || info.DurationSeconds != 32 ||
		info.ViewCount != 1234 || info.CreatedAt != "2016-06-12T21:36:33Z" ||
		info.BroadcasterDisplayName != "EA" || info.BroadcasterLogin != "ea" || info.BroadcasterID != "25163635" ||
		info.CuratorDisplayName != "stereotype_" || info.GameCategory != "Just Chatting" {
		t.Errorf("metadata = %+v", info)
	}
	if info.ThumbnailURL != "https://clips-media-assets2.twitch.tv/default-preview.jpg" {
		t.Errorf("ThumbnailURL = %q, want the default asset's", info.ThumbnailURL)
	}
	if len(info.Qualities) != 3 {
		t.Fatalf("qualities = %+v, want the two landscape and the one portrait rendition, nothing of a third asset", info.Qualities)
	}
	for _, rq := range info.Qualities {
		u, err := url.Parse(rq.URL)
		if err != nil {
			t.Fatalf("rendition URL %q: %v", rq.URL, err)
		}
		if u.Query().Get("sig") != "sig123" || u.Query().Get("token") != `{"clip_uri":"x"}` {
			t.Errorf("rendition %s is not signed: %s", rq.Quality, rq.URL)
		}
		if wantPortrait := strings.Contains(rq.URL, "portrait"); rq.Portrait != wantPortrait {
			t.Errorf("rendition %s Portrait = %v, want %v", rq.Quality, rq.Portrait, wantPortrait)
		}
	}
	if u, _ := url.Parse(info.Qualities[1].URL); u.Query().Get("x") != "1" || info.Qualities[1].FrameRate != 30 || info.Qualities[1].Height != 1080 {
		t.Errorf("1080 rendition = %+v: the URL's own query, a string frame rate or the height was lost", info.Qualities[1])
	}
}

// TestBestClipQualityRanksLikeYtDlp pins the rendition a clip job downloads:
// yt-dlp gives the portrait asset's formats quality -2, below every landscape
// one, and otherwise ranks by height, then fps. So the 1080p30 landscape
// rendition beats the 720p60 one and the taller 1920 portrait crop, and a
// portrait one is taken only when nothing else is there.
//
// Mutants: ranking fps before height (720p60 wins); ignoring Portrait (the
// 1920 portrait wins); an empty list answering anything but nil.
func TestBestClipQualityRanksLikeYtDlp(t *testing.T) {
	land720 := TwitchClipQuality{Quality: "720", Height: 720, FrameRate: 60}
	land1080 := TwitchClipQuality{Quality: "1080", Height: 1080, FrameRate: 30}
	land1080p60 := TwitchClipQuality{Quality: "1080", Height: 1080, FrameRate: 60}
	portrait := TwitchClipQuality{Quality: "1920", Height: 1920, FrameRate: 60, Portrait: true}
	cases := []struct {
		name string
		qs   []TwitchClipQuality
		want *TwitchClipQuality
	}{
		{"height before fps", []TwitchClipQuality{land720, land1080}, &land1080},
		{"fps at the same height", []TwitchClipQuality{land1080, land1080p60}, &land1080p60},
		{"landscape before a taller portrait", []TwitchClipQuality{portrait, land720}, &land720},
		{"portrait when it is all there is", []TwitchClipQuality{portrait}, &portrait},
		{"none", nil, nil},
	}
	for _, tc := range cases {
		got := BestClipQuality(tc.qs)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("%s: BestClipQuality = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestClipPersistedQueryNotFoundNamesTheOp is owner decision D26's rule for a
// re-hashed clip op: Twitch's `PersistedQueryNotFound` fails the call with
// "Twitch changed its clip API (<op>); update Moombox" — the op named, never
// the generic GQL error — for both clip ops, and the error still answers
// errors.Is(ErrPersistedQueryNotFound).
//
// Mutants: clipOpError returning the error unchanged (the generic
// "gql error (…): PersistedQueryNotFound"); parseGQLBody not wrapping the
// sentinel (errors.Is false, and so the generic text again).
func TestClipPersistedQueryNotFoundNamesTheOp(t *testing.T) {
	stubGQL(t, func(string) string {
		return `{"errors":[{"message":"PersistedQueryNotFound","extensions":{"code":"PERSISTED_QUERY_NOT_FOUND"}}]}`
	})
	api := NewAPI(&testLogger{})

	_, err := api.GetClip(context.Background(), "FaintLightGullWholeWheat", "")
	if err == nil || err.Error() != "Twitch changed its clip API (ShareClipRenderStatus); update Moombox" {
		t.Errorf("GetClip error = %v, want the clear message naming ShareClipRenderStatus", err)
	}
	if !errors.Is(err, ErrPersistedQueryNotFound) {
		t.Errorf("GetClip error %v does not wrap ErrPersistedQueryNotFound", err)
	}
	var changed *ClipAPIChangedError
	if !errors.As(err, &changed) || changed.Op != "ShareClipRenderStatus" {
		t.Errorf("GetClip error %v is not a ClipAPIChangedError for ShareClipRenderStatus", err)
	}

	_, err = api.ListClips(context.Background(), "vanillatv", ClipFilterLastDay, "")
	if err == nil || err.Error() != "Twitch changed its clip API (ClipsCards__User); update Moombox" {
		t.Errorf("ListClips error = %v, want the clear message naming ClipsCards__User", err)
	}
}

// TestGetClipGoneIsUnavailable: a slug Twitch answers with `"clip": null` is
// ErrClipUnavailable, yt-dlp's "This clip is no longer available"; a clip
// with no playback access token is an error, never unsigned URLs.
//
// Mutants: a nil clip mapped to an empty TwitchClipInfo; the token check
// dropped (URLs come back with empty sig/token).
func TestGetClipGoneIsUnavailable(t *testing.T) {
	stubGQL(t, func(string) string { return `{"data":{"clip":null}}` })
	if _, err := NewAPI(&testLogger{}).GetClip(context.Background(), "Gone", ""); !errors.Is(err, ErrClipUnavailable) {
		t.Errorf("null clip: error = %v, want ErrClipUnavailable", err)
	}

	stubGQL(t, func(string) string {
		return `{"data":{"clip":{"id":"1","assets":[{"videoQualities":[{"sourceURL":"https://example.invalid/a.mp4","quality":"720"}]}]}}}`
	})
	if info, err := NewAPI(&testLogger{}).GetClip(context.Background(), "NoToken", ""); err == nil {
		t.Errorf("tokenless clip: got %+v, want an error", info)
	}
}

// clipsPage renders one ClipsCards__User page: the given edges, each a
// ClipEdge with its cursor.
func clipsPage(edges ...string) string {
	return `{"data":{"user":{"id":"41245072","clips":{"edges":[` + strings.Join(edges, ",") + `]}}}}`
}

func clipEdge(slug, cursor string) string {
	return `{"__typename":"ClipEdge","cursor":` + cursor + `,"node":{"__typename":"Clip","id":"id-` + slug +
		`","url":"https://clips.twitch.tv/` + slug + `","title":"T ` + slug +
		`","thumbnailURL":"https://clips-media-assets2.twitch.tv/` + slug + `.jpg","durationSeconds":30.4,"createdAt":"2026-10-30T10:00:00Z","viewCount":7}}`
}

// TestListClipsPagesClipsCardsUser pins the port of yt-dlp's
// TwitchVideosClipsIE over TwitchPlaylistBaseIE._entries (twitch.py
// b77c9304e 649-697, 823-880): ClipsCards__User by its hash, with the login,
// criteria.filter and a page of 20; the next page asked for with the cursor
// of the last entry the page yielded; edges and nodes of another __typename,
// and a node with no URL, skipped; the listing over at a page whose last
// entry has no cursor; the slug the one the clip URL names.
//
// Mutants: the page limit not 20; the cursor not sent (the first page asked
// for again, forever until the cap); the __typename filter dropped (the
// VideoEdge entry kept); paging on past a null cursor.
func TestListClipsPagesClipsCardsUser(t *testing.T) {
	var sent []map[string]any
	stubGQL(t, func(body string) string {
		var q gqlPersistedQuery
		if err := json.Unmarshal([]byte(body), &q); err != nil {
			t.Fatalf("request: %v", err)
		}
		if q.OperationName != "ClipsCards__User" || q.Extensions.PersistedQuery.Hash != constants.TwitchGQLHashes.ClipsCardsUser {
			t.Errorf("request = %s / %s, want ClipsCards__User by its hash", q.OperationName, q.Extensions.PersistedQuery.Hash)
		}
		vars, _ := q.Variables.(map[string]any)
		sent = append(sent, vars)
		switch vars["cursor"] {
		case nil:
			return clipsPage(
				clipEdge("FirstClipSlug", `"c1"`),
				`{"__typename":"VideoEdge","cursor":"cx","node":{"__typename":"Video","id":"9","url":"https://clips.twitch.tv/NotAClip"}}`,
				`{"__typename":"ClipEdge","cursor":"cy","node":{"__typename":"Clip","id":"8","url":""}}`,
				clipEdge("SecondClipSlug", `"c2"`))
		case "c2":
			return clipsPage(clipEdge("ThirdClipSlug", `null`))
		}
		t.Errorf("unexpected cursor %v", vars["cursor"])
		return clipsPage()
	})

	cards, err := NewAPI(&testLogger{}).ListClips(context.Background(), "VanillaTV", ClipFilterLastWeek, "")
	if err != nil {
		t.Fatalf("ListClips: %v", err)
	}
	var slugs []string
	for _, c := range cards {
		slugs = append(slugs, c.Slug)
	}
	if strings.Join(slugs, ",") != "FirstClipSlug,SecondClipSlug,ThirdClipSlug" {
		t.Errorf("slugs = %v", slugs)
	}
	if len(sent) != 2 {
		t.Fatalf("pages asked for = %d, want 2", len(sent))
	}
	first := sent[0]
	crit, _ := first["criteria"].(map[string]any)
	if first["login"] != "vanillatv" || crit["filter"] != "LAST_WEEK" || first["limit"] != float64(20) {
		t.Errorf("first page variables = %v", first)
	}
	if c := cards[0]; c.Title != "T FirstClipSlug" || c.DurationSeconds != 30 || c.ViewCount != 7 ||
		c.CreatedAt != "2026-10-30T10:00:00Z" || c.URL != "https://clips.twitch.tv/FirstClipSlug" ||
		c.ThumbnailURL != "https://clips-media-assets2.twitch.tv/FirstClipSlug.jpg" {
		t.Errorf("card = %+v", c)
	}
}

// TestListClipsChannelNotFound: a user ID Twitch answers empty is a channel
// that does not exist (yt-dlp #15450), not a channel with no clips; a null
// user is no clips.
//
// Mutant: the empty-ID check dropped (nil error, no clips).
func TestListClipsChannelNotFound(t *testing.T) {
	stubGQL(t, func(string) string { return `{"data":{"user":{"id":"","clips":null}}}` })
	if _, err := NewAPI(&testLogger{}).ListClips(context.Background(), "nobody", ClipFilterLastDay, ""); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("empty user id: error = %v, want ErrChannelNotFound", err)
	}
	stubGQL(t, func(string) string { return `{"data":{"user":null}}` })
	if cards, err := NewAPI(&testLogger{}).ListClips(context.Background(), "nobody", ClipFilterLastDay, ""); err != nil || len(cards) != 0 {
		t.Errorf("null user: = %v, %v, want no clips and no error", cards, err)
	}
}

// TestClipJobIDRoundTrip pins the clip kind's job ID: "tw_clip_" + slug, and
// a slug starting with "v" is not read as a VOD's "tw_v" ID.
func TestClipJobIDRoundTrip(t *testing.T) {
	id := ClipJobID("VibrantCatClip-AbC")
	if id != "tw_clip_VibrantCatClip-AbC" {
		t.Errorf("ClipJobID = %q", id)
	}
	if slug, ok := ClipSlugFromJobID(id); !ok || slug != "VibrantCatClip-AbC" {
		t.Errorf("ClipSlugFromJobID(%q) = %q, %v", id, slug, ok)
	}
	for _, other := range []string{"tw_v123", "tw_123", "tw_clip_", "dQw4w9WgXcQ"} {
		if _, ok := ClipSlugFromJobID(other); ok {
			t.Errorf("ClipSlugFromJobID(%q) claims a clip", other)
		}
	}
}

// TestSelectClipQualityFollowsTheVodRule is owner decision FE-29: a clip
// takes the rendition the VOD selection (SelectBestVariant) takes under the
// job's quality_preference and max_video_resolution — the tallest at or under
// the cap, the preference's own size, the next lower size for a size the clip
// lacks — always from its landscape renditions while it has any, and the
// best rendition when nothing fits.
//
// Mutants: SelectBestVariant skipped (BestClipQuality: 1080 in every row);
// the cap fallback taking the VOD rule's smallest size above the cap (360 in
// the cap-below-everything row); portrait renditions kept in the pool beside
// landscape ones (the 1920 portrait crop for "1920p"); audio_only handed to
// SelectBestVariant (its last-variant fallback, 360).
func TestSelectClipQualityFollowsTheVodRule(t *testing.T) {
	qs := []TwitchClipQuality{
		{Quality: "1080", Height: 1080, FrameRate: 60},
		{Quality: "720", Height: 720, FrameRate: 60},
		{Quality: "480", Height: 480, FrameRate: 30},
		{Quality: "360", Height: 360, FrameRate: 30},
		{Quality: "1920", Height: 1920, FrameRate: 60, Portrait: true},
	}
	label := func(q *TwitchClipQuality) string {
		if q == nil {
			return "nil"
		}
		if q.Portrait {
			return "portrait " + q.Quality
		}
		return q.Quality
	}
	for _, c := range []struct {
		pref string
		cap  int
		want string
	}{
		{"best", 0, "1080"},
		{"", 0, "1080"},
		{"best", 720, "720"},
		{"480p", 1080, "480"},
		{"900p", 0, "720"},           // no 900: the next lower size
		{"1080p60", 720, "720"},      // the cap bounds the preference
		{"1920p", 0, "1080"},         // landscape ranks above portrait
		{"best", 240, "1080"},        // nothing fits: the best rendition
		{"audio_only", 1080, "1080"}, // no audio-only rendition: the best
	} {
		if got := label(SelectClipQuality(qs, c.pref, c.cap, false)); got != c.want {
			t.Errorf("SelectClipQuality(%q, cap %d) = %s, want %s", c.pref, c.cap, got, c.want)
		}
	}
	portrait := []TwitchClipQuality{
		{Quality: "1920", Height: 1920, Portrait: true},
		{Quality: "1280", Height: 1280, Portrait: true},
	}
	if got := label(SelectClipQuality(portrait, "best", 1280, false)); got != "portrait 1280" {
		t.Errorf("portrait-only clip under a 1280 cap = %s, want portrait 1280", got)
	}
	if SelectClipQuality(nil, "best", 0, false) != nil {
		t.Error("no renditions must select nothing")
	}
}
