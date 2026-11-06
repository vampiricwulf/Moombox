package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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

// VideoListSort maps a ?sort= value onto the videoSort variable: "VIEWS" for
// "views", "TIME" for anything else. TwitchVideosIE sends whatever ?sort= says
// upper-cased (`sort.upper()`, "time" its default — twitch.py b77c9304e), so
// ?sort=foo asks Twitch for FOO; Moombox sends only the two values Twitch's
// own site uses, TIME (its default) for any other.
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
// (it cannot be shown to be of the type asked for). That is a request per
// node of a filtered listing whose nodes come without the field — up to
// maxPages × 100 of them, one after another, inside the add request. Whether
// FilterableVideoTower_Videos' nodes carry broadcastType is the persisted
// query's selection set, Twitch's, and only a live listing shows it.
//
// A page that fails after the first ends the walk with what was read, as
// yt-dlp's _entries does (`fatal=False`, then `if not page: break` —
// twitch.py b77c9304e): a 50-page catalogue that hits one passing GQL error
// on page 37 is queued as far as it was read, with a warning, rather than
// not at all. A first page that fails, or a channel that does not exist, is
// the error.
func (a *API) ListChannelVideos(ctx context.Context, channelLogin, broadcastType, sort string, maxPages int, authToken string) ([]TwitchVideo, error) {
	var out []TwitchVideo
	err := a.EachChannelVideo(ctx, channelLogin, sort, maxPages, authToken, func(v TwitchVideo) bool {
		out = append(out, v)
		return true
	})
	if err != nil {
		if len(out) == 0 || errors.Is(err, ErrChannelNotFound) || ctx.Err() != nil {
			return nil, err
		}
		if a.logger != nil {
			a.logger.Warn("twitch: a page of the channel's video list could not be read; the listing ends with what was read",
				"channel", channelLogin, "videos", len(out), "err", err)
		}
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
	// A collection spans channels and its nodes need not name their owner
	// (yt-dlp's _make_video_result reads none), yet a backlog VOD is paced
	// by its owner's channel and a row without one is never admitted. A
	// node with no owner login takes VideoMetadata's, one request per such
	// node; one whose VideoMetadata cannot be read stays without, and
	// worker.QueueTwitchVideos refuses it.
	for i := range out {
		if out[i].Owner != nil && out[i].Owner.Login != "" {
			continue
		}
		info, err := a.GetVodInfo(ctx, out[i].ID, authToken)
		if err != nil || info == nil || info.ChannelLogin == "" {
			continue
		}
		out[i].Owner = &struct {
			Login       string `json:"login"`
			DisplayName string `json:"displayName"`
		}{Login: info.ChannelLogin, DisplayName: info.ChannelDisplayName}
	}
	return resp.Data.Collection.Title, out, nil
}

// ErrCollectionNotFound is CollectionVideos' answer for a collection Twitch
// does not have (deleted, private, or a mistyped ID).
var ErrCollectionNotFound = fmt.Errorf("twitch collection not found")

// TwitchCollectionRef is one entry of a channel's collection list
// (ChannelCollectionsContent): the collection's ID, which its own listing
// (CollectionVideos) is read by, and its title. yt-dlp's
// TwitchVideosCollectionsIE._extract_entry reads these two and the thumbnail,
// length, update time and view count beside them (twitch.py b77c9304e), none
// of which a queued VOD row needs.
type TwitchCollectionRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// collectionsPageLimit is TwitchPlaylistBaseIE._PAGE_LIMIT (twitch.py
// b77c9304e), the page ChannelCollectionsContent is asked for, as
// FilterableVideoTower_Videos is.
const collectionsPageLimit = 100

// ChannelCollections walks a channel's collection list, the page
// /<ch>/videos?filter=collections shows — yt-dlp's TwitchVideosCollectionsIE
// over TwitchPlaylistBaseIE._entries (twitch.py b77c9304e 649-697, 896-949):
//
//   - op ChannelCollectionsContent with variables ownerLogin, limit 100 and,
//     after the first page, the cursor;
//   - the edges at data.user.collections.edges; an edge that is not a
//     CollectionsItemEdge, a node that is not a Collection, or one without an
//     id is skipped, and the cursor carried to the next page is the last kept
//     edge's;
//   - a user whose id is "" is a login that does not resolve,
//     ErrChannelNotFound (yt-dlp #15450, as EachChannelVideo);
//   - the walk ends on a page without edges or without a cursor, or after
//     maxPages pages (maxPages <= 0: no bound).
//
// A page that fails after the first ends the walk with what was read, as
// _entries does (fatal=False, then `if not page: break`) and as
// ListChannelVideos does; a first page that fails is the error.
func (a *API) ChannelCollections(ctx context.Context, channelLogin string, maxPages int, authToken string) ([]TwitchCollectionRef, error) {
	login := strings.ToLower(channelLogin)
	var out []TwitchCollectionRef
	cursor := ""
	for page := 1; maxPages <= 0 || page <= maxPages; page++ {
		vars := map[string]any{"ownerLogin": login, "limit": collectionsPageLimit}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		query := newPersistedQuery("ChannelCollectionsContent", constants.TwitchGQLHashes.ChannelCollectionsContent, vars)
		respData, err := a.gqlRequest(ctx, "ChannelCollectionsContent", query, authToken)
		if err == nil {
			var resp struct {
				Data struct {
					User *struct {
						ID          *string `json:"id"`
						Collections *struct {
							Edges []struct {
								TypeName string `json:"__typename"`
								Cursor   string `json:"cursor"`
								Node     *struct {
									TypeName string `json:"__typename"`
									TwitchCollectionRef
								} `json:"node"`
							} `json:"edges"`
						} `json:"collections"`
					} `json:"user"`
				} `json:"data"`
			}
			if err = json.Unmarshal(respData, &resp); err == nil {
				user := resp.Data.User
				if user != nil && user.ID != nil && *user.ID == "" {
					return nil, ErrChannelNotFound
				}
				if user == nil || user.Collections == nil || len(user.Collections.Edges) == 0 {
					return out, nil
				}
				next := ""
				for _, e := range user.Collections.Edges {
					if e.TypeName != "CollectionsItemEdge" || e.Node == nil || e.Node.TypeName != "Collection" || e.Node.ID == "" {
						continue
					}
					next = e.Cursor
					out = append(out, e.Node.TwitchCollectionRef)
				}
				if next == "" {
					return out, nil
				}
				cursor = next
				continue
			}
			err = fmt.Errorf("parse channel collections: %w", err)
		}
		if page == 1 || ctx.Err() != nil {
			return nil, err
		}
		if a.logger != nil {
			a.logger.Warn("twitch: a page of the channel's collection list could not be read; the listing ends with what was read",
				"channel", channelLogin, "collections", len(out), "err", err)
		}
		return out, nil
	}
	return out, nil
}

// ChannelCollectionVideos is a /<ch>/videos?filter=collections listing (owner
// decision FE-27): every collection ChannelCollections lists, each read
// through CollectionVideos — yt-dlp hands each entry to TwitchCollectionIE
// (url_transparent) — their videos in list order, then each collection's own
// order, a VOD that two collections hold kept once.
//
// A collection that cannot be read — gone since the list named it, or one
// passing failure — is left out, with a warning, and the rest are still
// queued: a playlist of playlists that fails whole for one bad entry would
// queue nothing of a channel's dozen collections. yt-dlp, without
// --ignore-errors, stops at that entry instead; Moombox's add has no partial
// "stop here", so it keeps what it read. Only when nothing was read at all is
// the failure the answer — and then only a failure to read: a collection
// that is gone (ErrCollectionNotFound — deleted or made private since the
// list named it) is not, as the paste named a channel, not that collection,
// and a channel whose every listed collection is gone has a collection list
// with no videos in it, which is what the caller is told (an empty result).
func (a *API) ChannelCollectionVideos(ctx context.Context, channelLogin string, maxPages int, authToken string) ([]TwitchVideo, error) {
	cols, err := a.ChannelCollections(ctx, channelLogin, maxPages, authToken)
	if err != nil {
		return nil, err
	}
	var (
		out     []TwitchVideo
		seen    = map[string]bool{}
		lastErr error
	)
	for _, c := range cols {
		_, vids, err := a.CollectionVideos(ctx, c.ID, authToken)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !errors.Is(err, ErrCollectionNotFound) {
				lastErr = err
			}
			if a.logger != nil {
				a.logger.Warn("twitch: a collection of the channel's collection list could not be read; it is left out",
					"channel", channelLogin, "collection", c.ID, "err", err)
			}
			continue
		}
		for _, v := range vids {
			if !seen[v.ID] {
				seen[v.ID] = true
				out = append(out, v)
			}
		}
	}
	if len(out) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}

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
// timestamp >= the stream's createdAt, 00b1bec55), with three corrections an
// audit verifier and the purpose review made to it:
//
//   - the list is asked for with broadcastType null, as upstream asks — the
//     server-side ARCHIVE filter returns nothing (see EachChannelVideo) — and
//     the candidate is CONFIRMED through VideoMetadata to be an ARCHIVE.
//     Upstream takes the newest video of any type, which is right as the
//     stream starts but wrong later: a highlight or upload published during
//     the broadcast is newer than its archive (whose publishedAt is the
//     stream's start), and Moombox looks at finalize and, for gap-fill
//     (owner decision D17), up to a day later;
//   - only a video published within ArchiveVodStartTolerance of start, on
//     either side, is a candidate (owner decision PR-M3, purpose review M3
//     and A1). Upstream's ">= createdAt" has no upper bound, because it looks
//     the moment the stream is live, when no later broadcast can exist yet.
//     Moombox looks up to a day later (gap-fill), and on the Mux path and the
//     outage finalize end is the time of the look, not the broadcast's end:
//     with this broadcast's archive gone (expired, deleted, DMCA) the next
//     broadcast's was "the oldest archive published after start", and its
//     footage was cut at this broadcast's offsets and inserted as recovered
//     parts. And the lower bound has the same tolerance: an archive whose
//     publishedAt sits a second before the stream's createdAt stopped the
//     newest-first walk short of it, leaving only later broadcasts' archives
//     to be found. The window is the backlog's (the monitor's
//     twitchBacklogCapturedWindow names this one), the two being the same
//     question: is this video that broadcast's archive;
//   - of the candidates, the one published closest to start is taken first,
//     and with an end, only one published before it.
//
// Exact identity — the VOD's stream ID against the capture's — would be the
// better test, but no query this package makes returns one for a video:
// VideoMetadata and FilterableVideoTower_Videos are persisted queries whose
// selection sets are Twitch's, and neither yt-dlp nor streamlink reads a
// stream or broadcast ID off a video. The publish time is all there is.
//
// Returns "" with no error when no such video exists: VODs off, the archive
// deleted, or not listed yet. Reads at most archiveVodLookupPages pages.
func (a *API) ArchiveVodBetween(ctx context.Context, channelLogin string, start, end time.Time, authToken string) (string, error) {
	var candidates []TwitchVideo
	err := a.EachChannelVideo(ctx, channelLogin, "time", archiveVodLookupPages, authToken, func(v TwitchVideo) bool {
		published := v.Published()
		if published.IsZero() {
			return true
		}
		if published.Before(start.Add(-ArchiveVodStartTolerance)) {
			return false // every video after it is older still
		}
		if published.After(start.Add(ArchiveVodStartTolerance)) {
			return true // a later broadcast's, or something published during this one
		}
		if end.IsZero() || published.Before(end) {
			candidates = append(candidates, v)
		}
		return true
	})
	if err != nil {
		return "", err
	}
	distance := func(v TwitchVideo) time.Duration {
		d := v.Published().Sub(start)
		if d < 0 {
			return -d
		}
		return d
	}
	sort.SliceStable(candidates, func(i, j int) bool { return distance(candidates[i]) < distance(candidates[j]) })
	for _, v := range candidates {
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

// ArchiveVodStartTolerance is how far a video's publishedAt may sit from a
// broadcast's createdAt, on either side, and still be that broadcast's
// archive (ArchiveVodBetween; the monitor's VOD backlog matches with the same
// window). Twitch publishes a broadcast's archive as the stream starts; a
// broadcast that started within ten minutes of another one's start on the
// same channel is a restart of it, not another broadcast an archive could be
// mistaken for.
const ArchiveVodStartTolerance = 10 * time.Minute

// archiveVodLookupPages bounds ArchiveVodBetween's walk: three pages of 100
// newest videos. A channel that published more than that since the broadcast
// started (highlights cut from a marathon) is a channel the lookup gives up
// on, rather than paging its whole history.
const archiveVodLookupPages = 3
