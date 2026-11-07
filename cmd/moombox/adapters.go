package main

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// ytFormatAdapter adapts the YouTube service to the FormatRoutesDeps interface.
type ytFormatAdapter struct {
	svc   *youtube.Service
	store *config.Store
}

func (a *ytFormatAdapter) GetFormats(ctx context.Context, videoID string) (map[string]any, error) {
	// Snapshot under the store lock — raw per-request field reads race
	// config.Store.Update, which mutates the shared struct in place.
	var maxRes int
	var prefer60, preferHDR bool
	a.store.Read(func(c *config.MoomboxConfig) {
		maxRes = c.Downloader.MaxVideoResolution
		prefer60 = c.Downloader.Prefer60fps
		preferHDR = c.Downloader.PreferHDR
	})
	info, _, err := a.svc.GetFormats(ctx, videoID, maxRes, prefer60, preferHDR)
	if err != nil {
		return nil, err
	}
	return formatsPayload(videoID, info, maxRes, prefer60, preferHDR), nil
}

// pickerFormat is one row of the format picker's list: the format as its own
// JSON serialises it, plus two properties the downloader acts on that the
// Format's JSON leaves out, so the picker (the dashboard's Add dialog and the
// TUI's Add Video) can say what it is offering. yt-dlp's format list notes a
// damaged rendition "DAMAGED" in its format_note (_video.py:3430-3438 decides
// it, 3458 writes the note), which render_formats_table prints in the MORE
// INFO column (YoutubeDL.py:4036); Moombox marks it "damaged". "segmented"
// is Moombox's own: upstream's https pass skips an OTF format always and a
// live adaptive one unless the stream needs live processing
// (_video.py:3492-3507), and lists the ones it keeps without a note. The
// whole-file path here leaves both kinds out of its pool the same way (the
// worker's wholeFileVodPool), while the manifest-free live and post-live
// paths fetch them as fragments, so the picker still offers them, marked.
type pickerFormat struct {
	youtube.Format
	// Damaged is Format.Damaged: a rendition serving under half the video's
	// length (markDamagedFormats), which automatic selection passes over
	// while an intact one of its kind exists.
	Damaged bool `json:"damaged"`
	// Segmented is Format.Segmented(): served only as &sq=N fragments, so
	// the whole-file VOD path cannot fetch it. Sent whatever the stream's
	// status: both pickers leave its mark off while the answer's
	// streamStatus is live or upcoming, when every adaptive row carries it,
	// and still pass a segmented row over for their Best picks.
	Segmented bool `json:"segmented"`
}

// formatsPayload is GET /api/formats' answer for one extraction: the video
// and audio formats that carry a URL, each with its damaged/segmented marks
// (pickerFormat), in the picker's order, and bestItags — the one rendition
// per container/codec the picker badges [Best …] and preselects.
//
// A badge goes to the adaptive rendition of its kind automatic selection
// ranks best — youtube.CompareVideoFormats / CompareAudioFormats, the shared
// rank under the operator's prefer_60fps and prefer_hdr (wave W20: the
// badges follow the selector, so an HDR or Premium rendition, or a 5.1 or
// HIGH-quality audio track, is badged where the downloader would take it) —
// among those that are neither damaged nor segmented (owner decisions on the W9 and W4
// follow-ups): automatic selection passes a damaged one over while an intact
// one of its kind exists, and a whole-file VOD cannot fetch a segmented one,
// so badging either pointed the operator — and the dashboard's preselection,
// which follows the badges — at a rendition the downloader would not choose
// itself. When every adaptive rendition of a kind is damaged or segmented,
// as on a live or post-live stream, whose adaptive formats are all live
// adaptive, the badge falls back to the best-ranked adaptive rendition of
// the kind, marked or not, so a badge still exists. The list order (size,
// frame rate, then bitrate ascending, as the TypeScript picker sorted) is the
// display's only; it no longer decides a badge. Neither mark stops a manual pin: the operator can
// still choose a marked row by hand.
//
// Adaptive, because a muxed progressive file (itag 18, 640x360 with its
// audio inside) is intact wherever its adaptive siblings are segmented — it
// is a whole file even beside OTF formats, and a post-live response can list
// one without a contentLength — so a skip that read the marks alone handed
// it the [Best MP4] badge on exactly the streams where the badge mattered,
// and the dashboard preselected it. On an OTF VOD with a manifest, which
// DashStrategy downloads, the manifest offers no itag 18, so that pin fell
// back to automatic selection with a warning, where the 1080p WEBM the
// badges used to preselect is honoured; the manifest-free path takes no
// muxed format at all (partitionManifestlessFormats). A muxed row is badged
// only when its kind has no adaptive row, the one case it was the old pick
// and nothing else could be.
//
// maxRes is downloader.max_video_resolution, which only the video [Best]
// badge reads (bestAutoVideoItag): it is the rendition automatic selection
// takes, and that selection resolves the cap first. The codec badges rank
// every size, as they always have.
func formatsPayload(videoID string, info *youtube.VideoInfo, maxRes int, prefer60fps, preferHDR bool) map[string]any {
	// Separate video and audio formats, only those with a URL
	var videoFormats, audioFormats []youtube.Format
	for _, f := range info.Formats {
		if f.URL == "" {
			continue
		}
		if strings.Contains(f.MimeType, "video") {
			videoFormats = append(videoFormats, f)
		} else if strings.Contains(f.MimeType, "audio") {
			audioFormats = append(audioFormats, f)
		}
	}

	// Sort video: resolution desc -> fps desc -> bitrate asc (match TypeScript)
	slices.SortStableFunc(videoFormats, func(a, b youtube.Format) int {
		aRes := maxDim(a.Width, a.Height)
		bRes := maxDim(b.Width, b.Height)
		if aRes != bRes {
			return cmp.Compare(bRes, aRes)
		}
		aFps := derefInt(a.Fps)
		bFps := derefInt(b.Fps)
		if aFps != bFps {
			return cmp.Compare(bFps, aFps)
		}
		return cmp.Compare(a.Bitrate, b.Bitrate)
	})

	// Sort audio: bitrate desc (match TypeScript)
	slices.SortStableFunc(audioFormats, func(a, b youtube.Format) int {
		return cmp.Compare(b.Bitrate, a.Bitrate)
	})

	videoRows, audioRows := pickerRows(videoFormats), pickerRows(audioFormats)
	mime := func(sub string) func(*pickerFormat) bool {
		return func(f *pickerFormat) bool { return strings.Contains(f.MimeType, sub) }
	}

	// Build bestItags matching TypeScript format: bestWebmVideo, bestMp4Video, bestOpusAudio, bestAacAudio,
	// and bestAudio — the audio rendition automatic selection takes, of any
	// codec (owner decision FE-10): the shared AudioRank ranks a 5.1 E-AC-3
	// track above stereo Opus and AAC alike, and it is neither, so no codec
	// badge was ever on the track the downloader would take. The two codec
	// badges stay beside it. bestVideo is its video half (owner decision
	// PE-1): the rendition automatic selection takes, of either container,
	// under the operator's resolution cap (bestAutoVideoItag) — the codec
	// badges named the best WEBM and the best MP4, but not which of the two,
	// if either, Auto would download.
	video := func(a, b *youtube.Format) int { return youtube.CompareVideoFormats(a, b, prefer60fps, preferHDR) }
	audio := youtube.CompareAudioFormats
	bestItags := map[string]any{
		"bestWebmVideo": bestPickerItag(videoRows, mime("webm"), video),
		"bestMp4Video":  bestPickerItag(videoRows, mime("mp4"), video),
		"bestOpusAudio": bestPickerItag(audioRows, mime("opus"), audio),
		"bestAacAudio":  bestPickerItag(audioRows, mime("mp4a"), audio),
		"bestAudio":     bestPickerItag(audioRows, mime("audio"), audio),
		"bestVideo":     bestAutoVideoItag(videoRows, maxRes, video),
	}

	return map[string]any{
		"videoId":       videoID,
		"title":         info.Title,
		"channelName":   info.ChannelName,
		"lengthSeconds": info.LengthSeconds,
		"streamStatus":  info.StreamStatus,
		"videoFormats":  videoRows,
		"audioFormats":  audioRows,
		"bestItags":     bestItags,
	}
}

// pickerRows marks each format for the picker (pickerFormat), in order.
func pickerRows(formats []youtube.Format) []pickerFormat {
	rows := make([]pickerFormat, 0, len(formats))
	for _, f := range formats {
		rows = append(rows, pickerFormat{Format: f, Damaged: f.Damaged, Segmented: f.Segmented()})
	}
	return rows
}

// bestPickerItag is the itag of the adaptive row of kind that rank puts
// first among those neither damaged nor segmented, else among every adaptive
// row of kind, else of the first muxed one (see formatsPayload), or nil —
// JSON null, as the badge has always been — when no row is of kind. rank is
// positive when its first argument ranks above its second; a tie keeps the
// row listed first.
func bestPickerItag(rows []pickerFormat, kind func(*pickerFormat) bool, rank func(a, b *youtube.Format) int) any {
	var intact, marked *pickerFormat
	var muxed any
	better := func(r, best *pickerFormat) bool { return best == nil || rank(&r.Format, &best.Format) > 0 }
	for i := range rows {
		r := &rows[i]
		switch {
		case !kind(r):
		case r.muxed():
			if muxed == nil {
				muxed = r.Itag
			}
		case !r.Damaged && !r.Segmented:
			if better(r, intact) {
				intact = r
			}
		case better(r, marked):
			marked = r
		}
	}
	switch {
	case intact != nil:
		return intact.Itag
	case marked != nil:
		return marked.Itag
	}
	return muxed
}

// bestAutoVideoItag is the video [Best] badge's itag (owner decision PE-1):
// the rendition automatic selection would take, ranked as the downloader
// ranks — youtube.SelectBestFormats resolves the resolution cap to one size
// (utils.SelectByCap: the largest short edge at or below maxRes, else the
// smallest above it, 0 unbounded) and takes the best of that size by
// youtube.CompareVideoFormats. The pool follows bestPickerItag's skip rules,
// the ones the audio [Best] pick uses: the adaptive rows that are neither
// damaged nor segmented, else every adaptive row, else the first muxed one,
// else nil. The cap is resolved within the pool the badge is chosen from,
// as the selector resolves it over the renditions it may take
// (selectBestFormatsImpl's videoCands), so a cap whose only rendition at
// the chosen size is damaged settles on an intact size instead.
func bestAutoVideoItag(rows []pickerFormat, maxRes int, rank func(a, b *youtube.Format) int) any {
	var intact, marked []*pickerFormat
	var muxed any
	for i := range rows {
		r := &rows[i]
		switch {
		case !strings.Contains(r.MimeType, "video"):
		case r.muxed():
			if muxed == nil {
				muxed = r.Itag
			}
		case !r.Damaged && !r.Segmented:
			intact = append(intact, r)
		default:
			marked = append(marked, r)
		}
	}
	for _, pool := range [][]*pickerFormat{intact, marked} {
		cands := make([]utils.Cand, 0, len(pool))
		for _, r := range pool {
			cands = append(cands, utils.Cand{Width: derefInt(r.Width), Height: derefInt(r.Height)})
		}
		size, ok := utils.SelectByCap(maxRes, cands)
		if !ok {
			continue
		}
		var best *pickerFormat
		for _, r := range pool {
			if r.CapDimension() == size && (best == nil || rank(&r.Format, &best.Format) > 0) {
				best = r
			}
		}
		return best.Itag
	}
	return muxed
}

// muxed reports whether r is a progressive file rather than an adaptive
// rendition: a video row that is no video half (youtube.Format.IsVideoOnly —
// its mimeType names an audio codec beside the video one, upstream's
// single_stream test), the row the manifest-free path's pool leaves out.
// Every audio row is adaptive.
func (r *pickerFormat) muxed() bool {
	return strings.Contains(r.MimeType, "video") && !r.IsVideoOnly()
}

func maxDim(w, h *int) int {
	wv, hv := derefInt(w), derefInt(h)
	if wv > hv {
		return wv
	}
	return hv
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// twitchMetadataAdapter adapts the Twitch service to the TwitchMetadataFetcher interface.
type twitchMetadataAdapter struct {
	svc *twitch.Service
}

// FetchStreamMetadata answers the add route with the not-found-aware lookup:
// a login Twitch resolves to no user is routes.ErrTwitchChannelNotFound — the
// route's 404 — rather than the "offline" GetStreamInfo would have made of it,
// on which the route created a manual job that waited forever (G53).
func (a *twitchMetadataAdapter) FetchStreamMetadata(ctx context.Context, login string) (*routes.TwitchJobMetadata, error) {
	info, err := a.svc.LookupStreamInfo(ctx, login)
	if errors.Is(err, twitch.ErrChannelNotFound) {
		return nil, routes.ErrTwitchChannelNotFound
	}
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, nil
	}
	return &routes.TwitchJobMetadata{
		StreamID:     info.StreamID,
		Title:        info.Title,
		ChannelName:  info.ChannelDisplayName,
		ThumbnailURL: info.ThumbnailURL,
		AvatarURL:    info.ProfileImageURL,
		StartedAt:    info.StartedAt,
		GameCategory: info.GameCategory,
		IsLive:       info.IsLive,
	}, nil
}

func (a *twitchMetadataAdapter) FetchVodMetadata(ctx context.Context, vodID string) (*routes.TwitchJobMetadata, error) {
	info, err := a.svc.GetVodInfo(ctx, vodID)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, nil
	}
	return &routes.TwitchJobMetadata{
		Title:        info.Title,
		ChannelName:  info.ChannelDisplayName,
		ThumbnailURL: info.ThumbnailURL,
		StartedAt:    info.CreatedAt,
		GameCategory: info.GameCategory,
	}, nil
}

// twitchListingPages bounds a pasted channel video list: 50 pages of 100,
// newest first — a channel's whole catalogue in all but the rarest case,
// without paging a 10-year history in one request.
const twitchListingPages = 50

// ListChannelVideos implements routes.TwitchListingFetcher.
func (a *twitchMetadataAdapter) ListChannelVideos(ctx context.Context, login, broadcastType, sort string) ([]twitch.TwitchVideo, error) {
	return a.svc.API.ListChannelVideos(ctx, login, broadcastType, sort, twitchListingPages, a.svc.Auth.GetAuthToken())
}

// CollectionVideos implements routes.TwitchListingFetcher.
func (a *twitchMetadataAdapter) CollectionVideos(ctx context.Context, collectionID string) (string, []twitch.TwitchVideo, error) {
	return a.svc.API.CollectionVideos(ctx, collectionID, a.svc.Auth.GetAuthToken())
}

// ChannelCollectionVideos implements routes.TwitchListingFetcher.
func (a *twitchMetadataAdapter) ChannelCollectionVideos(ctx context.Context, login string) ([]twitch.TwitchVideo, error) {
	return a.svc.API.ChannelCollectionVideos(ctx, login, twitchListingPages, a.svc.Auth.GetAuthToken())
}

// FetchClipMetadata reads a clip for the add route (routes.TwitchClipMetadataFetcher).
func (a *twitchMetadataAdapter) FetchClipMetadata(ctx context.Context, slug string) (*routes.TwitchJobMetadata, error) {
	info, err := a.svc.GetClip(ctx, slug)
	if err != nil {
		return nil, err
	}
	return &routes.TwitchJobMetadata{
		Title:        info.Title,
		ChannelName:  info.BroadcasterDisplayName,
		ChannelLogin: info.BroadcasterLogin,
		ThumbnailURL: info.ThumbnailURL,
		StartedAt:    info.CreatedAt,
		GameCategory: info.GameCategory,
	}, nil
}

// youtubeMetadataAdapter implements routes.YouTubeMetadataFetcher.
type youtubeMetadataAdapter struct {
	svc *youtube.Service
}

func (a *youtubeMetadataAdapter) FetchMetadata(ctx context.Context, videoID string) (*routes.YouTubeJobMetadata, error) {
	if a.svc == nil {
		return nil, nil
	}
	info, err := a.svc.ProbeVideoStatus(ctx, videoID)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, nil
	}
	return &routes.YouTubeJobMetadata{
		Title:        info.Title,
		ChannelName:  info.ChannelName,
		ChannelID:    info.ChannelID,
		ThumbnailURL: info.ThumbnailURL,
	}, nil
}
