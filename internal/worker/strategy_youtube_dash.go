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

// DownloadDash sets up DASH segment downloaders for a live/post-live stream.
// Applies cipher decryption to manifest URL, n-parameter decryption to stream URLs,
// and injects PO token into the manifest URL path.
func DownloadDash(ctx context.Context, job *JobContext, videoInfo *youtube.VideoInfo, cipherSolver *cipher.Solver, potProvider *bgutils.PotProvider) (*DownloadResult, error) {
	if videoInfo.DashManifestURL == "" {
		return nil, fmt.Errorf("no DASH manifest URL available")
	}

	dashURL := videoInfo.DashManifestURL

	// Step 1: Decrypt the DASH manifest URL itself (signature cipher)
	if cipherSolver != nil && videoInfo.PlayerURL != "" {
		resolved, err := cipherSolver.ResolveURL(ctx, cipher.ResolveURLRequest{
			StreamURL: dashURL,
			PlayerURL: videoInfo.PlayerURL,
		})
		if err != nil {
			job.Logger.Warn("[Cipher] failed to decrypt DASH manifest URL", "err", err)
		} else {
			dashURL = resolved.URL
			job.Logger.Debug("[Cipher] decrypted DASH manifest URL")
		}
	}

	// Step 2: Inject PO token into manifest URL (/pot/{token} path segment)
	if potProvider != nil {
		poToken, err := potProvider.GeneratePoTokenString(ctx, videoInfo.ChannelID, false)
		if err != nil {
			job.Logger.Warn("[POT] failed to generate PO token for manifest", "err", err)
		} else if poToken != "" {
			dashURL = strings.TrimRight(dashURL, "/") + "/pot/" + poToken
			job.Logger.Info("[POT] added PO token to manifest URL", "tokenLength", len(poToken))
		} else {
			job.Logger.Warn("[POT] generator returned empty token")
		}
	}

	// Step 3: Fetch and parse DASH manifest
	manifestData, _, err := fetchURL(ctx, dashURL)
	if err != nil {
		return nil, fmt.Errorf("fetch DASH manifest: %w", err)
	}

	streams, err := engine.ParseDash(string(manifestData), videoInfo.DashManifestURL)
	if err != nil {
		return nil, fmt.Errorf("parse DASH manifest: %w", err)
	}

	// Step 4: Decrypt n-parameter in each stream's BaseURL (prevents throttling/403)
	if cipherSolver != nil && videoInfo.PlayerURL != "" {
		solvers, solverErr := cipherSolver.GetSolvers(ctx, videoInfo.PlayerURL)
		if solverErr != nil {
			job.Logger.Warn("[Cipher] failed to get solvers for n-param decryption", "err", solverErr)
		} else if solvers != nil && solvers.N != nil {
			decryptCount := 0
			for i := range streams {
				if streams[i].BaseURL == "" {
					continue
				}
				decrypted, err := decryptNParamInURL(streams[i].BaseURL, solvers.DecryptN)
				if err != nil {
					job.Logger.Warn("[Cipher] n-param decrypt failed", "itag", streams[i].Itag, "err", err)
					continue
				}
				streams[i].BaseURL = decrypted
				decryptCount++
			}
			job.Logger.Debug("[Cipher] DASH n-param decryption complete", "decrypted", decryptCount, "total", len(streams))
		}
	}

	// Step 5: Generate PO token for DASH segment URLs (applied via PoToken in DownloaderOptions)
	var dashPoToken string
	if potProvider != nil {
		poToken, potErr := potProvider.GeneratePoTokenString(ctx, videoInfo.ChannelID, false)
		if potErr != nil {
			job.Logger.Warn("[POT] failed to generate PO token for DASH segments", "err", potErr)
		} else if poToken != "" {
			dashPoToken = poToken
			job.Logger.Info("[POT] PO token ready for DASH segment URLs", "tokenLength", len(poToken))
		}
	}

	// Convert to DashStreamInfo for selection
	streamInfos := make([]DashStreamInfo, 0, len(streams))
	for _, s := range streams {
		streamInfos = append(streamInfos, DashStreamInfo{
			Itag:           s.Itag,
			MimeType:       s.MimeType,
			Codecs:         s.Codecs,
			Width:          s.Width,
			Height:         s.Height,
			FPS:            s.FPS,
			Bandwidth:      s.Bandwidth,
			BaseURL:        s.BaseURL,
			Initialization: s.Initialization,
		})
	}

	// Select best video/audio streams — per-job itag selection overrides config defaults (matches TS)
	videoItag := job.Config.VideoItag
	audioItag := job.Config.AudioItag
	if job.Job.SelectedVideoItag != nil {
		videoItag = *job.Job.SelectedVideoItag
	}
	if job.Job.SelectedAudioItag != nil {
		audioItag = *job.Job.SelectedAudioItag
	}

	// audio_only quality preference skips video (unless user manually selected a video itag)
	if job.Job.QualityPreference == "audio_only" && job.Job.SelectedVideoItag == nil {
		videoItag = -1
		job.Logger.Info("audio_only preference: skipping video stream")
	}

	// -1 means user explicitly chose "no video/audio"
	var videoStream, audioStream *DashStreamInfo
	if videoItag != -1 {
		videoStream = SelectBestDashStream(streamInfos, videoItag, job.Config.MaxVideoResolution, true, job.Job.QualityPreference)
	}
	if audioItag != -1 {
		audioStream = SelectBestDashStream(streamInfos, audioItag, 0, false, "")
	}

	// DASH requires both video and audio streams (matching TS), unless user explicitly excluded one
	if videoStream == nil && videoItag != -1 {
		return nil, fmt.Errorf("could not find suitable video stream in DASH manifest")
	}
	if audioStream == nil && audioItag != -1 {
		return nil, fmt.Errorf("could not find suitable audio stream in DASH manifest")
	}
	if videoStream == nil && audioStream == nil {
		return nil, fmt.Errorf("no streams selected for DASH download")
	}

	// Store video metadata on job for notifications
	if videoStream != nil && (videoStream.Width > 0 || videoStream.Height > 0) {
		job.DB.UpdateJobFields(job.Job.ID, map[string]any{
			"video_width":  videoStream.Width,
			"video_height": videoStream.Height,
		})
	}

	result := &DownloadResult{}

	// Populate stream dimensions for quality monitoring
	if videoStream != nil {
		result.VideoWidth = videoStream.Width
		result.VideoHeight = videoStream.Height
		result.VideoFps = videoStream.FPS
	}

	// Get cookie header for authenticated downloads
	var dashCookieHeader string
	if job.YT != nil {
		dashCookieHeader = job.YT.GetCookieHeader()
	}

	// Use last downloaded sequence from DB as StartSeq fallback for crash recovery.
	// The resume file (.resume.json) takes priority if it exists; StartSeq is only
	// used when no resume file is found (e.g., after a crash without graceful shutdown).
	videoStartSeq := 0
	if job.Job.LastVideoSeq != nil && *job.Job.LastVideoSeq > 0 {
		videoStartSeq = *job.Job.LastVideoSeq
	}
	audioStartSeq := 0
	if job.Job.LastAudioSeq != nil && *job.Job.LastAudioSeq > 0 {
		audioStartSeq = *job.Job.LastAudioSeq
	}

	if videoStream != nil {
		result.HasVideo = true
		result.VideoPath = filepath.Join(job.StagingDir, "video_stream")
		result.VideoDownloader = engine.NewSegmentDownloader(engine.DownloaderOptions{
			BaseURL:          videoStream.BaseURL,
			OutputFile:       result.VideoPath,
			StartSeq:         videoStartSeq,
			InitURL:          videoStream.Initialization,
			PoToken:          dashPoToken,
			CookieHeader:     dashCookieHeader,
			RetryDelayCap:    job.Config.SegmentRetryDelayCap,
			LiveCheckRetries: job.Config.SegmentLiveCheckRetries,
			Logger:           job.Logger,
			CheckStreamStatus: func(ctx context.Context) (bool, error) {
				info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
				if err != nil {
					return false, err
				}
				return info.StreamStatus != youtube.StreamLive, nil
			},
		})
		if cipherSolver != nil && videoInfo.PlayerURL != "" {
			result.VideoDownloader.OnCipherFailure = func() {
				job.Logger.Warn("[Cipher] 403 before any data — invalidating solver", "playerURL", videoInfo.PlayerURL)
				cipherSolver.InvalidateSolver(videoInfo.PlayerURL)
			}
		}
	}

	if audioStream != nil {
		result.HasAudio = true
		result.AudioPath = filepath.Join(job.StagingDir, "audio_stream")
		result.AudioDownloader = engine.NewSegmentDownloader(engine.DownloaderOptions{
			BaseURL:          audioStream.BaseURL,
			OutputFile:       result.AudioPath,
			StartSeq:         audioStartSeq,
			InitURL:          audioStream.Initialization,
			PoToken:          dashPoToken,
			CookieHeader:     dashCookieHeader,
			RetryDelayCap:    job.Config.SegmentRetryDelayCap,
			LiveCheckRetries: job.Config.SegmentLiveCheckRetries,
			Logger:           job.Logger,
			CheckStreamStatus: func(ctx context.Context) (bool, error) {
				info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
				if err != nil {
					return false, err
				}
				return info.StreamStatus != youtube.StreamLive, nil
			},
		})
		if cipherSolver != nil && videoInfo.PlayerURL != "" {
			result.AudioDownloader.OnCipherFailure = func() {
				job.Logger.Warn("[Cipher] 403 before any data — invalidating solver", "playerURL", videoInfo.PlayerURL)
				cipherSolver.InvalidateSolver(videoInfo.PlayerURL)
			}
		}
	}

	return result, nil
}
