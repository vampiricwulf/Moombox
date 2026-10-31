package main

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/twitch"
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
	var prefer60 bool
	a.store.Read(func(c *config.MoomboxConfig) {
		maxRes = c.Downloader.MaxVideoResolution
		prefer60 = c.Downloader.Prefer60fps
	})
	info, _, err := a.svc.GetFormats(ctx, videoID, maxRes, prefer60)
	if err != nil {
		return nil, err
	}
	return formatsPayload(videoID, info), nil
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
	// the whole-file VOD path cannot fetch it.
	Segmented bool `json:"segmented"`
}

// formatsPayload is GET /api/formats' answer for one extraction: the video
// and audio formats that carry a URL, each with its damaged/segmented marks
// (pickerFormat), in the picker's order, and bestItags — the one rendition
// per container/codec the picker badges [Best …] and preselects.
//
// A badge goes to the first rendition of its kind in that order that is
// neither damaged nor segmented (owner decisions on the W9 and W4
// follow-ups): automatic selection passes a damaged one over while an intact
// one of its kind exists, and a whole-file VOD cannot fetch a segmented one,
// so badging either pointed the operator — and the dashboard's preselection,
// which follows the badges — at a rendition the downloader would not choose
// itself. When every rendition of a kind is damaged or segmented, as on a
// live or post-live stream, where every format is live adaptive, the badge
// falls back to the first of the kind, the pick it always made, so a badge
// still exists. Neither mark stops a manual pin: the operator can still
// choose a marked row by hand.
func formatsPayload(videoID string, info *youtube.VideoInfo) map[string]any {
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

	// Build bestItags matching TypeScript format: bestWebmVideo, bestMp4Video, bestOpusAudio, bestAacAudio
	bestItags := map[string]any{
		"bestWebmVideo": bestPickerItag(videoRows, mime("webm")),
		"bestMp4Video":  bestPickerItag(videoRows, mime("mp4")),
		"bestOpusAudio": bestPickerItag(audioRows, mime("opus")),
		"bestAacAudio":  bestPickerItag(audioRows, mime("mp4a")),
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

// bestPickerItag is the itag of the first row of kind that is neither damaged
// nor segmented, else of the first row of kind at all (see formatsPayload),
// or nil — JSON null, as the badge has always been — when no row is of kind.
func bestPickerItag(rows []pickerFormat, kind func(*pickerFormat) bool) any {
	var fallback any
	for i := range rows {
		r := &rows[i]
		if !kind(r) {
			continue
		}
		if !r.Damaged && !r.Segmented {
			return r.Itag
		}
		if fallback == nil {
			fallback = r.Itag
		}
	}
	return fallback
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

func (a *twitchMetadataAdapter) FetchStreamMetadata(ctx context.Context, login string) (*routes.TwitchJobMetadata, error) {
	info, err := a.svc.GetStreamInfo(ctx, login)
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
