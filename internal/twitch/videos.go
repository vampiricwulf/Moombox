package twitch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// TwitchVideo is one video of a channel's list (FilterableVideoTower_Videos)
// or of a collection (CollectionSideBar): the fields yt-dlp's
// _make_video_result reads (id, title, previewThumbnailURL, lengthSeconds,
// viewCount — twitch.py b77c9304e), plus publishedAt, which its
// live_from_start rule compares with the stream's createdAt, and
// broadcastType when the node carries it ("" when it does not: the
// persisted query's selection set is Twitch's, and VideoMetadata is the
// confirmation — see ArchiveVodBetween).
type TwitchVideo struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	PublishedAt   string `json:"publishedAt"`
	BroadcastType string `json:"broadcastType"`
	LengthSeconds int    `json:"lengthSeconds"`
	ViewCount     int    `json:"viewCount"`
	// PreviewThumbnailURL is the node's thumbnail as Twitch sent it; a VOD
	// still being processed sends the 404_processing placeholder, which the
	// job built from it leaves out (the worker's VideoMetadata read fills
	// the thumbnail in once the VOD has one).
	PreviewThumbnailURL string `json:"previewThumbnailURL"`
	Owner               *struct {
		Login       string `json:"login"`
		DisplayName string `json:"displayName"`
	} `json:"owner"`
}

// Published is PublishedAt parsed, zero when it is missing or unparseable.
func (v TwitchVideo) Published() time.Time {
	t, err := time.Parse(time.RFC3339, v.PublishedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// videosPageLimit is TwitchPlaylistBaseIE._PAGE_LIMIT (twitch.py b77c9304e):
// the page FilterableVideoTower_Videos is asked for.
const videosPageLimit = 100

// VideoListBroadcastType maps a /<ch>/videos ?filter= value onto the
// broadcast type it names: TwitchVideosIE._BROADCASTS (twitch.py b77c9304e),
// "" for "all", for an absent filter and for any value it does not know (its
// _DEFAULT_BROADCAST). The type is what a listing keeps, read off each video
// (see ListChannelVideos), never what it asks Twitch for.
func VideoListBroadcastType(filter string) string {
	switch filter {
	case "archives":
		return "ARCHIVE"
	case "highlights":
		return "HIGHLIGHT"
	case "uploads":
		return "UPLOAD"
	case "past_premieres":
		return "PAST_PREMIERE"
	}
	return ""
}

// VideoListSort maps a ?sort= value onto the videoSort variable, as
// TwitchVideosIE does (`sort.upper()`, with "time" its default): "VIEWS" for
// "views", "TIME" for anything else.
func VideoListSort(sort string) string {
	if sort == "views" {
		return "VIEWS"
	}
	return "TIME"
}

// EachChannelVideo walks a channel's video list newest first (sort "TIME", or
// "VIEWS" most viewed first), page by page, calling visit with each video
// until it returns false, the list ends, or maxPages pages have been read
// (maxPages <= 0: no bound). It is yt-dlp's TwitchPlaylistBaseIE._entries over
// TwitchVideosBaseIE's variables (twitch.py b77c9304e):
//
//   - variables channelOwnerLogin, broadcastType, videoSort, limit 100 and,
//     after the first page, the cursor;
//   - broadcastType is ALWAYS null. yt-dlp passes the ?filter= type, but its
//     own tests mark filter=archives, uploads and past_premieres "TODO:
//     Investigate why we get 0 entries" (twitch.py 735-761, cb78440e4) — the
//     server-side filter returns nothing — and its live_from_start lookup
//     passes None. A caller that wants one type filters what visit is given
//     (ListChannelVideos);
//   - an edge that is not a VideoEdge, or a node that is not a Video, is
//     skipped, and the cursor carried to the next page is the last kept
//     edge's;
//   - a user whose id is "" is a login that does not resolve, ErrChannelNotFound
//     (twitch.py 672-675, yt-dlp #15450): Twitch answers an unknown login with
//     an empty user rather than null, and its video list is someone else's.
func (a *API) EachChannelVideo(ctx context.Context, channelLogin, sort string, maxPages int, authToken string, visit func(TwitchVideo) bool) error {
	login := strings.ToLower(channelLogin)
	cursor := ""
	for page := 1; maxPages <= 0 || page <= maxPages; page++ {
		vars := map[string]any{
			"channelOwnerLogin": login,
			"broadcastType":     nil,
			"videoSort":         VideoListSort(strings.ToLower(sort)),
			"limit":             videosPageLimit,
		}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		query := newPersistedQuery("FilterableVideoTower_Videos", constants.TwitchGQLHashes.FilterableVideoTowerVideos, vars)
		respData, err := a.gqlRequest(ctx, "FilterableVideoTower_Videos", query, authToken)
		if err != nil {
			return err
		}
		var resp struct {
			Data struct {
				User *struct {
					ID     *string `json:"id"`
					Videos *struct {
						Edges []struct {
							TypeName string `json:"__typename"`
							Cursor   string `json:"cursor"`
							Node     *struct {
								TypeName string `json:"__typename"`
								TwitchVideo
							} `json:"node"`
						} `json:"edges"`
					} `json:"videos"`
				} `json:"user"`
			} `json:"data"`
		}
		if err := json.Unmarshal(respData, &resp); err != nil {
			return fmt.Errorf("parse channel videos: %w", err)
		}
		user := resp.Data.User
		if user != nil && user.ID != nil && *user.ID == "" {
			return ErrChannelNotFound
		}
		if user == nil || user.Videos == nil || len(user.Videos.Edges) == 0 {
			return nil
		}
		next := ""
		for _, e := range user.Videos.Edges {
			if e.TypeName != "VideoEdge" || e.Node == nil || e.Node.TypeName != "Video" || e.Node.ID == "" {
				continue
			}
			next = e.Cursor
			if !visit(e.Node.TwitchVideo) {
				return nil
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
	return nil
}

// ListChannelVideos is a /<ch>/videos listing: every video EachChannelVideo
// walks (up to maxPages pages), kept when broadcastType ("" for all — see
// VideoListBroadcastType) matches. A node that carries no broadcastType is
// classified through VideoMetadata, one request per such node, and only when
// a type was asked for; a node whose VideoMetadata cannot be read is left out
// (it cannot be shown to be of the type asked for).
func (a *API) ListChannelVideos(ctx context.Context, channelLogin, broadcastType, sort string, maxPages int, authToken string) ([]TwitchVideo, error) {
	var out []TwitchVideo
	err := a.EachChannelVideo(ctx, channelLogin, sort, maxPages, authToken, func(v TwitchVideo) bool {
		out = append(out, v)
		return true
	})
	if err != nil {
		return nil, err
	}
	if broadcastType == "" {
		return out, nil
	}
	kept := out[:0]
	for _, v := range out {
		if a.videoBroadcastType(ctx, v, authToken) == broadcastType {
			kept = append(kept, v)
		}
	}
	return kept, nil
}

// videoBroadcastType is v's broadcastType: the node's own, else
// VideoMetadata's (yt-dlp reads it there, _extract_info_gql twitch.py 519).
// "" when neither says.
func (a *API) videoBroadcastType(ctx context.Context, v TwitchVideo, authToken string) string {
	if v.BroadcastType != "" {
		return v.BroadcastType
	}
	info, err := a.GetVodInfo(ctx, v.ID, authToken)
	if err != nil || info == nil {
		return ""
	}
	return info.BroadcastType
}

// CollectionVideos reads a collection (/collections/<id>): its title and its
// videos in the collection's own order. yt-dlp's TwitchCollectionIE
// (twitch.py b77c9304e): one CollectionSideBar request with collectionID,
// data.collection.items.edges[].node, every node with an id kept. A null
// collection is ErrCollectionNotFound.
func (a *API) CollectionVideos(ctx context.Context, collectionID, authToken string) (string, []TwitchVideo, error) {
	query := newPersistedQuery("CollectionSideBar", constants.TwitchGQLHashes.CollectionSideBar, map[string]any{
		"collectionID": collectionID,
	})
	respData, err := a.gqlRequest(ctx, "CollectionSideBar", query, authToken)
	if err != nil {
		return "", nil, err
	}
	var resp struct {
		Data struct {
			Collection *struct {
				Title string `json:"title"`
				Items struct {
					Edges []struct {
						Node *TwitchVideo `json:"node"`
					} `json:"edges"`
				} `json:"items"`
			} `json:"collection"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respData, &resp); err != nil {
		return "", nil, fmt.Errorf("parse collection: %w", err)
	}
	if resp.Data.Collection == nil {
		return "", nil, ErrCollectionNotFound
	}
	var out []TwitchVideo
	for _, e := range resp.Data.Collection.Items.Edges {
		if e.Node != nil && e.Node.ID != "" {
			out = append(out, *e.Node)
		}
	}
	return resp.Data.Collection.Title, out, nil
}

// ErrCollectionNotFound is CollectionVideos' answer for a collection Twitch
// does not have (deleted, private, or a mistyped ID).
var ErrCollectionNotFound = fmt.Errorf("twitch collection not found")

// ArchiveVodFor finds the archive VOD of the broadcast a live capture
// recorded, with no end to bound it (ArchiveVodBetween with a zero end): the
// lookup a live capture's finalize makes for its chapters (owner decision D23
// follow-up 1), when no later broadcast can have started yet.
//
// Returns "" with no error when the channel has no such video — VODs off, or
// Twitch has not listed it yet.
func (a *API) ArchiveVodFor(ctx context.Context, channelLogin, startedAt, authToken string) (string, error) {
	start, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		return "", fmt.Errorf("archive vod lookup: stream start %q: %w", startedAt, err)
	}
	return a.ArchiveVodBetween(ctx, channelLogin, start, time.Time{}, authToken)
}

// ArchiveVodBetween finds the archive VOD of the broadcast that started at
// start (and, when end is not zero, ended at end) on channelLogin. It is
// yt-dlp's live_from_start lookup (TwitchStreamIE._real_extract, twitch.py
// b77c9304e: `next(self._entries(channel_name, None, 'time'))`, kept when its
// timestamp >= the stream's createdAt, 00b1bec55), with two corrections an
// audit verifier made to it:
//
//   - the list is asked for with broadcastType null, as upstream asks — the
//     server-side ARCHIVE filter returns nothing (see EachChannelVideo) — and
//     the candidate is CONFIRMED through VideoMetadata to be an ARCHIVE.
//     Upstream takes the newest video of any type, which is right as the
//     stream starts but wrong later: a highlight or upload published during
//     the broadcast is newer than its archive (whose publishedAt is the
//     stream's start), and Moombox looks at finalize and, for gap-fill
//     (owner decision D17), up to a day later;
//   - the video taken is the OLDEST archive published at or after start, not
//     the newest, and with an end, one published before it. A day after a
//     broadcast the channel may have streamed again, and that broadcast's
//     archive is newer than this one's and also published after start; gap
//     footage cut from it would be another broadcast's. The walk stops at the
//     first video published before start, every one after it being older.
//
// Returns "" with no error when no such video exists: VODs off, the archive
// deleted, or not listed yet. Reads at most archiveVodLookupPages pages.
func (a *API) ArchiveVodBetween(ctx context.Context, channelLogin string, start, end time.Time, authToken string) (string, error) {
	var candidates []TwitchVideo // newest first
	err := a.EachChannelVideo(ctx, channelLogin, "time", archiveVodLookupPages, authToken, func(v TwitchVideo) bool {
		published := v.Published()
		if published.IsZero() {
			return true
		}
		if published.Before(start) {
			return false
		}
		if end.IsZero() || published.Before(end) {
			candidates = append(candidates, v)
		}
		return true
	})
	if err != nil {
		return "", err
	}
	for i := len(candidates) - 1; i >= 0; i-- {
		v := candidates[i]
		if v.BroadcastType != "" && v.BroadcastType != "ARCHIVE" {
			continue // the node already says it is not a past broadcast
		}
		info, err := a.GetVodInfo(ctx, v.ID, authToken)
		if err != nil {
			return "", fmt.Errorf("archive vod lookup: confirm video %s: %w", v.ID, err)
		}
		if info != nil && info.BroadcastType == "ARCHIVE" {
			return v.ID, nil
		}
	}
	return "", nil
}

// archiveVodLookupPages bounds ArchiveVodBetween's walk: three pages of 100
// newest videos. A channel that published more than that since the broadcast
// started (highlights cut from a marathon) is a channel the lookup gives up
// on, rather than paging its whole history.
const archiveVodLookupPages = 3
