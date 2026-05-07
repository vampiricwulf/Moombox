package worker

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// HasManifestlessDashFormats reports whether videoInfo.Formats[] contains
// the split video + audio adaptive entries that make a manifest-free DASH
// download viable. The signal is: at least one audio-only entry AND at
// least one video-only entry, both with non-empty URLs. HLS variants are
// muxed (carry both video and audio in the same itag) so they don't pass
// this check; only true DASH-style adaptive splits do.
//
// Used by the orchestrator strategy switch to detect the experiment case
// where YouTube withholds dashManifestUrl but still ships adaptiveFormats[]
// in the watch-page player response (yt-dlp issue #15274).
func HasManifestlessDashFormats(formats []youtube.Format) bool {
	hasAudio, hasVideo := false, false
	for i := range formats {
		f := &formats[i]
		if f.URL == "" {
			continue
		}
		if f.IsAudio() {
			hasAudio = true
		} else if f.Width != nil && *f.Width > 0 {
			hasVideo = true
		}
		if hasAudio && hasVideo {
			return true
		}
	}
	return false
}

// formatToDashStreamInfo converts a youtube.Format into the DashStreamInfo
// shape that SelectBestDashStream expects. Mirrors the conversion done in
// strategy_youtube_dash.go:160-172 for parsed-manifest streams; sharing a
// single helper would couple two unrelated types so we keep it inline.
func formatToDashStreamInfo(f *youtube.Format) DashStreamInfo {
	info := DashStreamInfo{
		Itag:      f.Itag,
		MimeType:  f.MimeType,
		Bandwidth: f.Bitrate,
		BaseURL:   f.URL,
	}
	if f.Width != nil {
		info.Width = *f.Width
	}
	if f.Height != nil {
		info.Height = *f.Height
	}
	if f.Fps != nil {
		info.FPS = *f.Fps
	}
	return info
}

// DownloadManifestlessDash sets up a download for streams that ship adaptive
// formats with direct URLs but no DASH manifest. Triggered by the YouTube
// account experiment that strips dashManifestUrl from cookied responses
// (yt-dlp issue #15274) — the watch-page player response still carries
// streamingData.adaptiveFormats[] with full URLs that we can fetch with
// `&sq=N` appended.
//
// Pipeline:
//  1. Filter videoInfo.Formats[] into video and audio adaptive entries.
//  2. Use SelectBestDashStream to pick best video + audio per the user's
//     quality preference (same selector DASH manifest path uses).
//  3. Decrypt the n-param in each selected URL via the routed cipher solver
//     (sidecar primary, goja fallback). parseFormatsWithCipher already does
//     this during player-response parsing, but we re-route here so that any
//     orchestrator-driven URL refresh on cipher rotation goes through the
//     same path.
//  4. Mint a POT bound to videoID — the experiment's GVS POT policy for
//     cookied clients is video-id-bound, not visitor-data-bound.
//  5. Build engine.SegmentDownloader instances. The downloader's
//     buildSegmentURL auto-detects query-style adaptive URLs and appends
//     `&sq=N`, while applyPoTokenQuery appends `&pot=POT` per fetch. No
//     separate init segment — sq=0 carries ftyp+moov inline.
func DownloadManifestlessDash(
	ctx context.Context,
	job *JobContext,
	videoInfo *youtube.VideoInfo,
	routedSolver cipher.Solver,
	cipherSolver *cipher.GojaResolver,
	potProvider *bgutils.PotProvider,
	isOnline func() bool,
) (*DownloadResult, error) {
	// Partition videoInfo.Formats into video + audio adaptive pools.
	var videoStreams, audioStreams []DashStreamInfo
	for i := range videoInfo.Formats {
		f := &videoInfo.Formats[i]
		if f.URL == "" {
			continue
		}
		info := formatToDashStreamInfo(f)
		if f.IsAudio() {
			audioStreams = append(audioStreams, info)
		} else if info.Width > 0 {
			videoStreams = append(videoStreams, info)
		}
	}

	// Per-job itag selection overrides config defaults.
	videoItag := job.Config.VideoItag
	audioItag := job.Config.AudioItag
	if job.Job.SelectedVideoItag != nil {
		videoItag = *job.Job.SelectedVideoItag
	}
	if job.Job.SelectedAudioItag != nil {
		audioItag = *job.Job.SelectedAudioItag
	}
	if job.Job.QualityPreference == "audio_only" && job.Job.SelectedVideoItag == nil {
		videoItag = -1
		job.Logger.Info("audio_only preference: skipping video stream")
	}

	var videoStream, audioStream *DashStreamInfo
	if videoItag != -1 {
		videoStream = SelectBestDashStream(videoStreams, videoItag, job.Config.MaxVideoResolution, true, job.Job.QualityPreference)
	}
	if audioItag != -1 {
		audioStream = SelectBestDashStream(audioStreams, audioItag, 0, false, "")
	}
	if videoStream == nil && videoItag != -1 {
		return nil, fmt.Errorf("manifestless DASH: no suitable video adaptive format")
	}
	if audioStream == nil && audioItag != -1 {
		return nil, fmt.Errorf("manifestless DASH: no suitable audio adaptive format")
	}
	if videoStream == nil && audioStream == nil {
		return nil, fmt.Errorf("manifestless DASH: no streams selected")
	}

	job.Logger.Info("manifestless DASH selected",
		"videoItag", itagOf(videoStream),
		"audioItag", itagOf(audioStream),
		"videoQuality", qualityOf(videoStream))

	// NOTE: do NOT re-decrypt n here. parseFormatsWithCipher already
	// decrypted n on every URL during player-response parsing
	// (player_api_parsing.go:185-195). Running RoutedDecryptNInURL on an
	// already-decrypted URL would put the result through the n cipher a
	// second time, producing garbage that YouTube 403s on every fetch.
	// The composite solver has no idempotency guard; the contract here
	// is "URLs from videoInfo.Formats[] are already cipher-resolved."
	// Cipher rotation mid-stream is handled via the OnCipherFailure
	// callback below, which invalidates the solver and triggers a
	// manifest refresh through the orchestrator.

	// Mint a POT bound to the videoID rather than visitor data. The
	// experiment that strips dashManifestUrl from cookied clients also
	// switches GVS POT enforcement to videoID-binding (yt-dlp logs
	// "Detected experiment to bind GVS PO Token to video id"). Falling
	// back to visitorData here would produce a POT that YouTube's CDN
	// rejects on every segment fetch.
	var pot string
	if potProvider != nil {
		token, err := potProvider.GeneratePoTokenString(ctx, job.Job.VideoID, false)
		if err != nil {
			job.Logger.Warn("manifestless DASH: PO token mint failed", "err", err)
		} else if token != "" {
			pot = token
			job.Logger.Info("[POT] PO token ready for manifestless DASH",
				"binding", "videoID", "tokenLength", len(token))
		}
	}

	result := &DownloadResult{}

	var cookieHeader string
	if job.YT != nil {
		cookieHeader = job.YT.GetCookieHeader()
	}

	// Orchestrator-provided start sequences (quality recovery / split path);
	// fall back to DB persisted last seq for crash recovery.
	videoStartSeq := 0
	forceVideoSeq := false
	if job.VideoStartSeq > 0 {
		videoStartSeq = job.VideoStartSeq
		forceVideoSeq = true
	} else if job.Job.LastVideoSeq != nil && *job.Job.LastVideoSeq > 0 {
		videoStartSeq = *job.Job.LastVideoSeq
	}
	audioStartSeq := 0
	forceAudioSeq := false
	if job.AudioStartSeq > 0 {
		audioStartSeq = job.AudioStartSeq
		forceAudioSeq = true
	} else if job.Job.LastAudioSeq != nil && *job.Job.LastAudioSeq > 0 {
		audioStartSeq = *job.Job.LastAudioSeq
	}

	if videoStream != nil {
		result.HasVideo = true
		result.VideoPath = filepath.Join(job.StagingDir, "video_stream")
		result.VideoWidth = videoStream.Width
		result.VideoHeight = videoStream.Height
		result.VideoFps = videoStream.FPS
		result.VideoDownloader = engine.NewSegmentDownloader(engine.DownloaderOptions{
			BaseURL:          videoStream.BaseURL,
			OutputFile:       result.VideoPath,
			StartSeq:         videoStartSeq,
			ForceStartSeq:    forceVideoSeq,
			PoToken:          pot,
			CookieHeader:     cookieHeader,
			RetryDelayCap:    job.Config.SegmentRetryDelayCap,
			LiveCheckRetries: job.Config.SegmentLiveCheckRetries,
			IsOnline:         isOnline,
			Logger:           job.Logger,
			CheckStreamStatus: func(ctx context.Context) (bool, error) {
				info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
				if err != nil {
					return false, err
				}
				return info.StreamStatus != youtube.StreamLive, nil
			},
		})
		// Same 403 invalidation chain as DASH-manifest path. The shared
		// helper handles cipher solver + POT cache + visitor-data wipe.
		if videoInfo.PlayerURL != "" && (routedSolver != nil || cipherSolver != nil) {
			result.VideoDownloader.OnCipherFailure = func() string {
				invalidate403Caches(job, videoInfo.PlayerURL, cipherSolver, potProvider, "manifestless DASH video")
				return ""
			}
		}
		if videoStream.Width > 0 || videoStream.Height > 0 {
			job.DB.UpdateJobFields(job.Job.ID, map[string]any{
				"video_width":  videoStream.Width,
				"video_height": videoStream.Height,
			})
		}
	}

	if audioStream != nil {
		result.HasAudio = true
		result.AudioPath = filepath.Join(job.StagingDir, "audio_stream")
		result.AudioDownloader = engine.NewSegmentDownloader(engine.DownloaderOptions{
			BaseURL:          audioStream.BaseURL,
			OutputFile:       result.AudioPath,
			StartSeq:         audioStartSeq,
			ForceStartSeq:    forceAudioSeq,
			PoToken:          pot,
			CookieHeader:     cookieHeader,
			RetryDelayCap:    job.Config.SegmentRetryDelayCap,
			LiveCheckRetries: job.Config.SegmentLiveCheckRetries,
			IsOnline:         isOnline,
			Logger:           job.Logger,
			CheckStreamStatus: func(ctx context.Context) (bool, error) {
				info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
				if err != nil {
					return false, err
				}
				return info.StreamStatus != youtube.StreamLive, nil
			},
		})
		if videoInfo.PlayerURL != "" && (routedSolver != nil || cipherSolver != nil) {
			result.AudioDownloader.OnCipherFailure = func() string {
				invalidate403Caches(job, videoInfo.PlayerURL, cipherSolver, potProvider, "manifestless DASH audio")
				return ""
			}
		}
	}

	return result, nil
}

func itagOf(s *DashStreamInfo) int {
	if s == nil {
		return 0
	}
	return s.Itag
}

func qualityOf(s *DashStreamInfo) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%dx%d@%d", s.Width, s.Height, s.FPS))
}
