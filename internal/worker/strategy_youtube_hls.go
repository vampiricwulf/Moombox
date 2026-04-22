package worker

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// DownloadHls sets up an HLS segment downloader.
// Fetches the HLS master playlist, selects the best variant based on
// max_video_resolution config, and passes the selected variant's playlist URL
// to the SegmentDownloader.
func DownloadHls(ctx context.Context, job *JobContext, videoInfo *youtube.VideoInfo, potProvider *bgutils.PotProvider, isOnline func() bool) (*DownloadResult, error) {
	if videoInfo.HlsManifestURL == "" {
		return nil, fmt.Errorf("no HLS manifest URL available")
	}

	hlsURL := videoInfo.HlsManifestURL

	// Inject PO token if available
	if potProvider != nil {
		poToken, err := potProvider.GeneratePoTokenString(ctx, poTokenBinding(job, videoInfo), false)
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

	// Step 3: Select best variant respecting max_video_resolution and quality preference
	maxRes := job.Config.MaxVideoResolution
	if maxRes <= 0 {
		maxRes = 9999
	}

	// Filter by maxRes cap
	var filtered []*engine.HlsVariant
	for i := range parsed.Variants {
		v := &parsed.Variants[i]
		varMaxDim := max(v.Height, v.Width)
		if varMaxDim <= maxRes {
			filtered = append(filtered, v)
		}
	}

	if len(filtered) == 0 {
		return nil, fmt.Errorf("no HLS variants found within resolution limit (%d)", maxRes)
	}

	// Apply quality preference targeting
	var bestVariant *engine.HlsVariant
	qualityPref := job.Job.QualityPreference
	if qualityPref == "audio_only" {
		// YouTube HLS doesn't have audio-only variants — select lowest bandwidth
		// to minimize wasted video data (audio quality is the same across variants)
		job.Logger.Warn("audio_only preference with HLS: YouTube HLS has no audio-only variants, selecting lowest bandwidth")
		bestVariant = filtered[0]
		for _, v := range filtered[1:] {
			if v.Bandwidth < bestVariant.Bandwidth {
				bestVariant = v
			}
		}
	} else if qualityPref != "" && qualityPref != "best" {
		targetHeight, targetFPS := ParseQualityPreference(qualityPref)
		if targetHeight > 0 {
			// Try exact height match
			bestVariant = selectHlsByHeight(filtered, targetHeight, targetFPS)
			// Descend through lower heights
			if bestVariant == nil {
				bestVariant = selectNextLowerHls(filtered, targetHeight)
			}
			// No lower heights — fall through to source/best
		}
	}

	// Fallback: highest bandwidth among all filtered candidates (source/best)
	if bestVariant == nil {
		bestVariant = filtered[0]
		for _, v := range filtered[1:] {
			if v.Bandwidth > bestVariant.Bandwidth {
				bestVariant = v
			}
		}
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
		HasVideo:    true,
		IsHls:       true,
		VideoPath:   filepath.Join(job.StagingDir, "video.ts"),
		VideoWidth:  bestVariant.Width,
		VideoHeight: bestVariant.Height,
		VideoFps:    bestVariant.FPS,
	}

	// Apply PO token to variant playlist URL (path mode) and pass for segment URLs (query mode)
	variantURL := bestVariant.URL
	var hlsPoToken string
	if potProvider != nil {
		poToken, potErr := potProvider.GeneratePoTokenString(ctx, poTokenBinding(job, videoInfo), false)
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

	return result, nil
}

// selectHlsByHeight finds an HLS variant matching the target height, optionally with FPS.
func selectHlsByHeight(variants []*engine.HlsVariant, targetHeight, targetFPS int) *engine.HlsVariant {
	var heightMatches []*engine.HlsVariant
	for _, v := range variants {
		if v.Height == targetHeight {
			heightMatches = append(heightMatches, v)
		}
	}
	if len(heightMatches) == 0 {
		return nil
	}
	// If FPS-specific, prefer highest bandwidth among FPS matches
	if targetFPS > 0 {
		var bestFPS *engine.HlsVariant
		for _, v := range heightMatches {
			if v.FPS >= targetFPS-1 {
				if bestFPS == nil || v.Bandwidth > bestFPS.Bandwidth {
					bestFPS = v
				}
			}
		}
		if bestFPS != nil {
			return bestFPS
		}
	}
	// Return highest bandwidth at target height
	best := heightMatches[0]
	for _, v := range heightMatches[1:] {
		if v.Bandwidth > best.Bandwidth {
			best = v
		}
	}
	return best
}

// selectNextLowerHls finds the best HLS variant below the target height,
// descending through available heights. Returns nil if no lower heights exist.
func selectNextLowerHls(variants []*engine.HlsVariant, targetHeight int) *engine.HlsVariant {
	bestHeight := 0
	for _, v := range variants {
		if v.Height < targetHeight && v.Height > bestHeight {
			bestHeight = v.Height
		}
	}
	if bestHeight == 0 {
		return nil
	}
	var best *engine.HlsVariant
	for _, v := range variants {
		if v.Height == bestHeight {
			if best == nil || v.Bandwidth > best.Bandwidth {
				best = v
			}
		}
	}
	return best
}
