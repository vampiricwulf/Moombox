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

// DownloadVod downloads a VOD using direct format URLs.
// For YouTube VODs, format URLs point to complete files (not segmented).
// Downloads video and audio streams as whole files via HTTP GET.
//
// routedSolver is the composite cipher.Solver used for sig + n
// decryption on the chosen format URL(s). cipherSolver is the legacy
// goja resolver used as the n-fallback path. Both are accepted (rather
// than only routedSolver) so the wiring stays consistent with the
// other YouTube strategies — the orchestrator passes whatever it has.
func DownloadVod(ctx context.Context, job *JobContext, videoInfo *youtube.VideoInfo, routedSolver cipher.Solver, cipherSolver *cipher.GojaResolver, potProvider *bgutils.PotProvider) (*DownloadResult, error) {
	selected := youtube.SelectBestFormatsWithLogger(videoInfo.Formats, job.Config.MaxVideoResolution, job.Config.Prefer60fps, job.Logger)

	// Per-job itag overrides (from manual format selection in the UI).
	// A value of -1 means "explicitly no video/audio" (skip that track).
	if job.Job.SelectedVideoItag != nil {
		itag := *job.Job.SelectedVideoItag
		if itag == -1 {
			selected.Video = nil
		} else if itag > 0 {
			found := false
			for i := range videoInfo.Formats {
				f := &videoInfo.Formats[i]
				if f.Itag == itag && strings.Contains(f.MimeType, "video") && f.URL != "" {
					selected.Video = f
					found = true
					break
				}
			}
			if found {
				w, h, fps := 0, 0, 0
				if selected.Video.Width != nil {
					w = *selected.Video.Width
				}
				if selected.Video.Height != nil {
					h = *selected.Video.Height
				}
				if selected.Video.Fps != nil {
					fps = *selected.Video.Fps
				}
				job.Logger.Debug(fmt.Sprintf("[FormatSelector] Manual video selection: itag %d %dx%d@%dfps", itag, w, h, fps))
			} else {
				job.Logger.Warn(fmt.Sprintf("[FormatSelector] Manual video itag %d not found, falling back to auto", itag))
			}
		}
	}
	if job.Job.SelectedAudioItag != nil {
		itag := *job.Job.SelectedAudioItag
		if itag == -1 {
			selected.Audio = nil
		} else if itag > 0 {
			found := false
			for i := range videoInfo.Formats {
				f := &videoInfo.Formats[i]
				if f.Itag == itag && strings.Contains(f.MimeType, "audio") && f.URL != "" {
					selected.Audio = f
					found = true
					break
				}
			}
			if found {
				job.Logger.Debug(fmt.Sprintf("[FormatSelector] Manual audio selection: itag %d %dbps", itag, selected.Audio.Bitrate))
			} else {
				job.Logger.Warn(fmt.Sprintf("[FormatSelector] Manual audio itag %d not found, falling back to auto", itag))
			}
		}
	}

	// audio_only quality preference skips video (unless user manually selected a video itag)
	if job.Job.QualityPreference == "audio_only" && job.Job.SelectedVideoItag == nil {
		selected.Video = nil
		job.Logger.Info("audio_only preference: skipping video format")
	}

	result := &DownloadResult{}

	if selected.Video != nil && selected.Video.URL != "" {
		result.HasVideo = true
		result.VideoFormat = selected.Video
		result.VideoPath = filepath.Join(job.StagingDir, "video.mp4")

		// Check if progressive (combined audio+video)
		if IsProgressiveFormat(selected.Video) {
			result.HasAudio = true
			result.AudioFormat = nil // Audio is embedded
		}
	}

	if selected.Audio != nil && selected.Audio.URL != "" && (selected.Video == nil || !IsProgressiveFormat(selected.Video)) {
		result.HasAudio = true
		result.AudioFormat = selected.Audio
		result.AudioPath = filepath.Join(job.StagingDir, "audio.m4a")
	}

	// Handle audio-only case: use audio as primary video input for muxer
	if !result.HasVideo && result.HasAudio && selected.Audio != nil && selected.Audio.URL != "" {
		result.HasVideo = true
		result.VideoFormat = selected.Audio
		result.VideoPath = filepath.Join(job.StagingDir, "audio.m4a")
		result.HasAudio = false
		result.AudioFormat = nil
		result.AudioPath = ""
	}

	if !result.HasVideo && !result.HasAudio {
		return nil, fmt.Errorf("no suitable formats found for VOD download")
	}

	// Stage 3 of the cipher pipeline rework: parseFormats leaves URLs raw
	// (with sigCipher entries carrying EncryptedSig + the bare `url=`
	// value). Resolve sig + n on the chosen video / audio formats now so
	// we send fetchable URLs to the engine. Bound to two attempts via
	// re-selection so a fully broken cipher state surfaces cleanly.
	videoResolved, audioResolved := "", ""
	if result.HasVideo && result.VideoFormat != nil {
		resolvedURL, err := resolveFormatURL(ctx, result.VideoFormat, routedSolver, cipherSolver, videoInfo.PlayerURL, job.Logger)
		if err != nil {
			job.Logger.Warn("[Cipher] VOD video resolve failed; trying re-selection",
				"itag", result.VideoFormat.Itag, "err", err)
			retry := pickAlternateVodFormat(videoInfo.Formats, true, result.VideoFormat.Itag)
			if retry == nil {
				return nil, fmt.Errorf("VOD: video URL resolve failed and no alternate format: %w", err)
			}
			resolvedURL, err = resolveFormatURL(ctx, retry, routedSolver, cipherSolver, videoInfo.PlayerURL, job.Logger)
			if err != nil {
				return nil, fmt.Errorf("VOD: video URL resolve failed for primary and alternate: %w", err)
			}
			result.VideoFormat = retry
			job.Logger.Info("[Cipher] VOD video re-selection succeeded", "newItag", retry.Itag)
		}
		videoResolved = resolvedURL
	}
	if result.HasAudio && result.AudioFormat != nil {
		resolvedURL, err := resolveFormatURL(ctx, result.AudioFormat, routedSolver, cipherSolver, videoInfo.PlayerURL, job.Logger)
		if err != nil {
			job.Logger.Warn("[Cipher] VOD audio resolve failed; trying re-selection",
				"itag", result.AudioFormat.Itag, "err", err)
			retry := pickAlternateVodFormat(videoInfo.Formats, false, result.AudioFormat.Itag)
			if retry == nil {
				return nil, fmt.Errorf("VOD: audio URL resolve failed and no alternate format: %w", err)
			}
			resolvedURL, err = resolveFormatURL(ctx, retry, routedSolver, cipherSolver, videoInfo.PlayerURL, job.Logger)
			if err != nil {
				return nil, fmt.Errorf("VOD: audio URL resolve failed for primary and alternate: %w", err)
			}
			result.AudioFormat = retry
			job.Logger.Info("[Cipher] VOD audio re-selection succeeded", "newItag", retry.Itag)
		}
		audioResolved = resolvedURL
	}

	// NOTE: Do NOT apply PO token to VOD format URLs. The TS implementation's
	// PoTokenGenerator.getPoToken() returns empty for VOD downloads (BotGuard
	// token is not generated for direct format URL access). Adding a PO token
	// to these URLs causes HTTP 403 from YouTube's CDN.

	// Store video metadata on job (matching DASH strategy behavior)
	if selected.Video != nil {
		updates := map[string]any{}
		if selected.Video.Width != nil && *selected.Video.Width > 0 {
			updates["video_width"] = *selected.Video.Width
		}
		if selected.Video.Height != nil && *selected.Video.Height > 0 {
			updates["video_height"] = *selected.Video.Height
		}
		if selected.Video.Fps != nil && *selected.Video.Fps > 0 {
			updates["video_fps"] = *selected.Video.Fps
		}
		if len(updates) > 0 {
			job.DB.UpdateJobFields(job.Job.ID, updates)
		}
	}

	// NOTE: Do NOT send cookies with VOD format URL downloads. The TS
	// downloadFile() only sends User-Agent (ANDROID), not cookies. TV auth
	// format URLs are obtained via cookies in the API call but the CDN
	// download itself does not require cookies.

	// Create downloaders for direct URLs. videoResolved / audioResolved are
	// the post-cipher-resolution URLs; these (not the raw Format.URL) are
	// what the engine fetches.
	if result.HasVideo && result.VideoPath != "" && videoResolved != "" {
		result.VideoDownloader = engine.NewSegmentDownloader(engine.DownloaderOptions{
			BaseURL:     videoResolved,
			OutputFile:  result.VideoPath,
			StartSeq:    0,
			EndSeq:      0, // Single file download
			IsDirectURL: true,
			Logger:      job.Logger,
		})
	}

	if result.HasAudio && result.AudioPath != "" && audioResolved != "" {
		result.AudioDownloader = engine.NewSegmentDownloader(engine.DownloaderOptions{
			BaseURL:     audioResolved,
			OutputFile:  result.AudioPath,
			StartSeq:    0,
			EndSeq:      0,
			IsDirectURL: true,
			Logger:      job.Logger,
		})
	}

	return result, nil
}
