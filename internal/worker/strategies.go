package worker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// nPathRe matches n-parameter encoded in URL path: /n/{encrypted_value}/
var nPathRe = regexp.MustCompile(`/n/([a-zA-Z0-9_-]{10,})/`)

// DownloadResult contains the result of a download strategy.
type DownloadResult struct {
	VideoDownloader *engine.SegmentDownloader
	AudioDownloader *engine.SegmentDownloader
	VideoPath       string
	AudioPath       string
	HasVideo        bool
	HasAudio        bool
	VideoFormat     *youtube.Format
	AudioFormat     *youtube.Format
	IsHls           bool // true if HLS strategy was used
}

// DownloadVod downloads a VOD using direct format URLs.
// For YouTube VODs, format URLs point to complete files (not segmented).
// Downloads video and audio streams as whole files via HTTP GET.
func DownloadVod(ctx context.Context, job *JobContext, videoInfo *youtube.VideoInfo, _ *cipher.Solver, potProvider *bgutils.PotProvider) (*DownloadResult, error) {
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

	if selected.Audio != nil && selected.Audio.URL != "" && !IsProgressiveFormat(selected.Video) {
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

	// NOTE: n-param decryption is already done during format extraction in
	// parseFormatsWithCipher (player_api.go). Do NOT decrypt again here —
	// double-decrypting corrupts the n-param and causes HTTP 403.

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

	// Create downloaders for direct URLs
	if result.HasVideo && result.VideoPath != "" && selected.Video.URL != "" {
		result.VideoDownloader = engine.NewSegmentDownloader(engine.DownloaderOptions{
			BaseURL:    selected.Video.URL,
			OutputFile: result.VideoPath,
			StartSeq:   0,
			EndSeq:     0, // Single file download
			IsDirectURL: true,
			Logger:     job.Logger,
		})
	}

	if result.HasAudio && result.AudioPath != "" && selected.Audio != nil && selected.Audio.URL != "" {
		result.AudioDownloader = engine.NewSegmentDownloader(engine.DownloaderOptions{
			BaseURL:    selected.Audio.URL,
			OutputFile: result.AudioPath,
			StartSeq:   0,
			EndSeq:     0,
			IsDirectURL: true,
			Logger:     job.Logger,
		})
	}

	return result, nil
}

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
	var streamInfos []DashStreamInfo
	for _, s := range streams {
		streamInfos = append(streamInfos, DashStreamInfo{
			Itag:           s.Itag,
			MimeType:       s.MimeType,
			Codecs:         s.Codecs,
			Width:          s.Width,
			Height:         s.Height,
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

	// -1 means user explicitly chose "no video/audio"
	var videoStream, audioStream *DashStreamInfo
	if videoItag != -1 {
		videoStream = SelectBestDashStream(streamInfos, videoItag, job.Config.MaxVideoResolution, true)
	}
	if audioItag != -1 {
		audioStream = SelectBestDashStream(streamInfos, audioItag, 0, false)
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
	if videoStream.Width > 0 || videoStream.Height > 0 {
		job.DB.UpdateJobFields(job.Job.ID, map[string]any{
			"video_width":  videoStream.Width,
			"video_height": videoStream.Height,
		})
	}

	result := &DownloadResult{}

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
			CheckStreamStatus: func(ctx context.Context) (bool, error) {
				info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
				if err != nil {
					return false, err
				}
				return info.StreamStatus != youtube.StreamLive, nil
			},
		})
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
			CheckStreamStatus: func(ctx context.Context) (bool, error) {
				info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
				if err != nil {
					return false, err
				}
				return info.StreamStatus != youtube.StreamLive, nil
			},
		})
	}

	return result, nil
}

// decryptNParamInURL finds and decrypts the 'n' parameter in a URL.
// YouTube encodes n-params both in query strings (?n=value) and in URL paths (/n/{value}/).
// Both forms must be decrypted to avoid throttling/403 errors.
func decryptNParamInURL(rawURL string, nDecrypt func(string) (string, error)) (string, error) {
	result := rawURL

	// Check for n parameter in path: /n/{encrypted_value}/
	// Match values that look like encrypted n-params (10+ alphanumeric/special chars)
	if nPathRe.MatchString(result) {
		matches := nPathRe.FindStringSubmatch(result)
		if len(matches) >= 2 {
			encryptedN := matches[1]
			decrypted, err := nDecrypt(encryptedN)
			if err == nil && decrypted != encryptedN {
				result = strings.Replace(result, "/n/"+encryptedN+"/", "/n/"+decrypted+"/", 1)
			}
		}
	}

	// Also check query string n param.
	// Use string replacement to preserve original parameter order —
	// Go's url.Values.Encode() sorts parameters alphabetically, which breaks
	// YouTube's URL signature verification and causes HTTP 403.
	parsed, err := url.Parse(result)
	if err != nil {
		return result, err
	}
	nParam := parsed.Query().Get("n")
	if nParam != "" {
		decrypted, err := nDecrypt(nParam)
		if err == nil && decrypted != nParam {
			result = strings.Replace(result, "n="+nParam, "n="+decrypted, 1)
		}
	}

	return result, nil
}

// appendPotQuery appends a GVS PO token to a URL as a query parameter (?pot=token).
// Used for format URLs, DASH segment BaseURLs, and HLS segment URLs.
// Uses naive string append to avoid re-encoding existing query parameters
// (re-encoding can change parameter order/encoding and break URL signatures).
func appendPotQuery(rawURL, poToken string) string {
	if poToken == "" {
		return rawURL
	}
	sep := "&"
	if !strings.Contains(rawURL, "?") {
		sep = "?"
	}
	return rawURL + sep + "pot=" + url.QueryEscape(poToken)
}

// DownloadHls sets up an HLS segment downloader.
// Fetches the HLS master playlist, selects the best variant based on
// max_video_resolution config, and passes the selected variant's playlist URL
// to the SegmentDownloader.
func DownloadHls(ctx context.Context, job *JobContext, videoInfo *youtube.VideoInfo, potProvider *bgutils.PotProvider) (*DownloadResult, error) {
	if videoInfo.HlsManifestURL == "" {
		return nil, fmt.Errorf("no HLS manifest URL available")
	}

	hlsURL := videoInfo.HlsManifestURL

	// Inject PO token if available
	if potProvider != nil {
		poToken, err := potProvider.GeneratePoTokenString(ctx, videoInfo.ChannelID, false)
		if err != nil {
			job.Logger.Warn("pot: failed to generate PO token for HLS", "err", err)
		} else if poToken != "" {
			hlsURL = strings.TrimRight(hlsURL, "/") + "/pot/" + poToken
			job.Logger.Debug("pot: added PO token to HLS manifest URL")
		}
	}

	// Step 1: Fetch the HLS master playlist
	job.Logger.Info("fetching HLS master playlist")
	manifestData, statusCode, err := fetchURL(ctx, hlsURL)
	if err != nil {
		return nil, fmt.Errorf("fetch HLS manifest: %w", err)
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch HLS manifest: HTTP %d", statusCode)
	}

	// Step 2: Parse the master playlist to extract variants
	parsed := engine.ParseHls(string(manifestData), hlsURL)
	if parsed == nil || !parsed.IsMaster || len(parsed.Variants) == 0 {
		return nil, fmt.Errorf("invalid HLS master playlist (no variants found)")
	}

	// Step 3: Select best variant respecting max_video_resolution
	maxRes := job.Config.MaxVideoResolution
	if maxRes <= 0 {
		maxRes = 9999
	}

	var bestVariant *engine.HlsVariant
	for i := range parsed.Variants {
		v := &parsed.Variants[i]
		varMaxDim := v.Width
		if v.Height > varMaxDim {
			varMaxDim = v.Height
		}
		if varMaxDim > maxRes {
			continue
		}
		if bestVariant == nil || v.Bandwidth > bestVariant.Bandwidth {
			bestVariant = v
		}
	}

	if bestVariant == nil {
		return nil, fmt.Errorf("no HLS variants found within resolution limit (%d)", maxRes)
	}

	job.Logger.Info("selected HLS variant",
		"width", bestVariant.Width, "height", bestVariant.Height,
		"bandwidth", bestVariant.Bandwidth)

	// Step 4: Store variant dimensions on the job
	if bestVariant.Width > 0 || bestVariant.Height > 0 {
		job.DB.UpdateJobFields(job.Job.ID, map[string]any{
			"video_width":  bestVariant.Width,
			"video_height": bestVariant.Height,
		})
	}

	// Step 5: Create downloader using the selected variant's playlist URL
	result := &DownloadResult{
		HasVideo:  true,
		IsHls:     true,
		VideoPath: filepath.Join(job.StagingDir, "video.ts"),
	}

	// Apply PO token to variant playlist URL (path mode) and pass for segment URLs (query mode)
	variantURL := bestVariant.URL
	var hlsPoToken string
	if potProvider != nil {
		poToken, potErr := potProvider.GeneratePoTokenString(ctx, videoInfo.ChannelID, false)
		if potErr != nil {
			job.Logger.Warn("[POT] failed to generate PO token for HLS variant", "err", potErr)
		} else if poToken != "" {
			variantURL = strings.TrimRight(variantURL, "/") + "/pot/" + poToken
			hlsPoToken = poToken
			job.Logger.Info("[POT] added PO token to HLS variant URL", "tokenLength", len(poToken))
		}
	}

	result.VideoDownloader = engine.NewSegmentDownloader(engine.DownloaderOptions{
		BaseURL:          variantURL,
		OutputFile:       result.VideoPath,
		StartSeq:         -1,
		IsHls:            true,
		PoToken:          hlsPoToken,
		RetryDelayCap:    job.Config.SegmentRetryDelayCap,
		LiveCheckRetries: job.Config.SegmentLiveCheckRetries,
		CheckStreamStatus: func(ctx context.Context) (bool, error) {
			info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
			if err != nil {
				return false, err
			}
			return info.StreamStatus != youtube.StreamLive, nil
		},
	})

	return result, nil
}

// downloadDirectURL downloads a complete file from a URL to a local path.
// Used for VOD format URLs that point to complete files.
func downloadDirectURL(ctx context.Context, url, outputPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d downloading %s", resp.StatusCode, url)
	}

	f, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}
