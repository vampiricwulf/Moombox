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

	// Build bestItags matching TypeScript format: bestWebmVideo, bestMp4Video, bestOpusAudio, bestAacAudio
	bestItags := map[string]any{
		"bestWebmVideo": nil,
		"bestMp4Video":  nil,
		"bestOpusAudio": nil,
		"bestAacAudio":  nil,
	}
	for _, f := range videoFormats {
		if bestItags["bestWebmVideo"] == nil && strings.Contains(f.MimeType, "webm") {
			bestItags["bestWebmVideo"] = f.Itag
		}
		if bestItags["bestMp4Video"] == nil && strings.Contains(f.MimeType, "mp4") {
			bestItags["bestMp4Video"] = f.Itag
		}
	}
	for _, f := range audioFormats {
		if bestItags["bestOpusAudio"] == nil && strings.Contains(f.MimeType, "opus") {
			bestItags["bestOpusAudio"] = f.Itag
		}
		if bestItags["bestAacAudio"] == nil && strings.Contains(f.MimeType, "mp4a") {
			bestItags["bestAacAudio"] = f.Itag
		}
	}

	return map[string]any{
		"videoId":       videoID,
		"title":         info.Title,
		"channelName":   info.ChannelName,
		"lengthSeconds": info.LengthSeconds,
		"streamStatus":  info.StreamStatus,
		"videoFormats":  videoFormats,
		"audioFormats":  audioFormats,
		"bestItags":     bestItags,
	}, nil
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
