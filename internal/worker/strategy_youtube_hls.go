package worker

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// DownloadHls sets up an HLS segment downloader.
// Fetches the HLS master playlist, selects the best variant based on
// max_video_resolution config, and passes the selected variant's playlist URL
// to the SegmentDownloader.
//
// routedSolver is the composite cipher.Solver (sidecar primary, goja fallback);
// cipherSolver is the legacy *GojaResolver. Both are used to decrypt the `/n/`
// throttling parameter in the master URL before fetching — yt-dlp does the
// same in extractor/youtube/_video.py:3684-3690. Without this, YouTube's CDN
// returns HTTP 403 on every segment for streams whose master URL ships with
// an encrypted n in its path (cb017549-family players).
func DownloadHls(ctx context.Context, job *JobContext, videoInfo *youtube.VideoInfo, routedSolver cipher.Solver, cipherSolver *cipher.GojaResolver, potProvider *bgutils.PotProvider, isOnline func() bool) (*DownloadResult, error) {
	if videoInfo.HlsManifestURL == "" {
		return nil, fmt.Errorf("no HLS manifest URL available")
	}

	hlsURL := videoInfo.HlsManifestURL

	// Decrypt /n/<encrypted>/ in master URL before fetching. Mirrors
	// yt-dlp's HLS path: the master URL ships with a throttled n value
	// that the CDN rejects on segments unless replaced with a player-JS-
	// solved n. Master playlist fetch may succeed with the encrypted n
	// (YouTube only enforces n on /videoplayback segment requests), but
	// every segment returned by the variant playlist will then 403.
	if videoInfo.PlayerURL != "" && (routedSolver != nil || cipherSolver != nil) {
		decrypted := cipher.RoutedDecryptNInURL(ctx, routedSolver, cipherSolver, videoInfo.PlayerURL, hlsURL)
		if decrypted != hlsURL {
			hlsURL = decrypted
			job.Logger.Debug("[Cipher] decrypted n-param in HLS manifest URL")
		}
	}

	// Generate PO token once and reuse for master URL, variant URL, and
	// segment URLs (audit reports/worker.md F30; same dedup as F29 for DASH).
	//
	// GVS PO token: unconditional videoID binding (moonarchive parity),
	// minted from the watch-page challenge riding on videoInfo when present.
	// Supersedes the former poTokenBinding visitorData/channelID scheme —
	// see spec 2026-08-14 attestation POT coherence §3.
	var hlsPoToken string
	if potProvider != nil {
		bindingValue, bindingKind := gvsBinding(job, videoInfo)
		mint, err := potProvider.GenerateGvsPoToken(ctx, bindingValue, videoInfo.AttestationChallenge)
		if err != nil {
			job.Logger.Warn("[POT] GVS mint failed", "jobID", job.Job.ID,
				"binding", bindingKind, "challenge", challengeLabel(videoInfo.AttestationChallenge), "err", err)
		} else if mint.PoToken != "" {
			hlsPoToken = mint.PoToken
			hlsURL = strings.TrimRight(hlsURL, "/") + "/pot/" + mint.PoToken
			job.Logger.Info("[POT] GVS mint", "jobID", job.Job.ID,
				"binding", bindingKind, "challenge", challengeLabel(videoInfo.AttestationChallenge),
				"minterSource", mint.MinterSource, "minterFresh", mint.MinterFresh,
				"sidecar", mint.ViaSidecar, "tokenLength", len(mint.PoToken))
		} else {
			job.Logger.Warn("[POT] generator returned empty token", "jobID", job.Job.ID)
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

	// Reuse hlsPoToken from the master-URL step above; no need to mint twice.
	variantURL := bestVariant.URL
	if hlsPoToken != "" {
		variantURL = strings.TrimRight(variantURL, "/") + "/pot/" + hlsPoToken
		job.Logger.Info("[POT] added PO token to HLS variant URL", "tokenLength", len(hlsPoToken))
	}

	// Orchestrator-provided continuation position (restart discovery seeding
	// a fresh part dir). Without honoring it, a fresh dir + StartSeq -1
	// initializes at the playlist window start — which for DVR-enabled
	// YouTube live HLS reaches back to broadcast start, re-downloading hours
	// already archived in earlier parts. The DASH strategies honor the same
	// field; a resume sidecar (resuming into an unmuxed part) still takes
	// priority inside the engine.
	hlsStartSeq := -1
	hlsForceSeq := false
	if job.VideoStartSeq > 0 {
		hlsStartSeq = job.VideoStartSeq
		hlsForceSeq = true
	}

	result.VideoDownloader = engine.NewSegmentDownloader(engine.DownloaderOptions{
		BaseURL:           variantURL,
		OutputFile:        result.VideoPath,
		StartSeq:          hlsStartSeq,
		ForceStartSeq:     hlsForceSeq,
		IsHls:             true,
		PoToken:           hlsPoToken,
		MaxTimeout:        time.Duration(job.Config.MaximumTimeout) * time.Second,
		EnforceMaxTimeout: true, // YouTube status can stick; Twitch HLS opts out
		IsOnline:          isOnline,
		Logger:            newScopedLogger(job.Logger, "jobID", job.Job.ID, "stream", "video"),
		CheckStreamStatus: func(ctx context.Context) (bool, error) {
			info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
			if err != nil {
				return false, err
			}
			return info.StreamStatus != youtube.StreamLive, nil
		},
	})

	// Wire OnCipherFailure for HLS so a 403 burst invalidates POT / visitor
	// data / cipher caches. The variant URL has POT in its path so we can't
	// hot-swap it mid-loop; return "" and let the next orchestrator-driven
	// refresh rebuild the strategy with fresh values.
	if videoInfo.PlayerURL != "" && (routedSolver != nil || cipherSolver != nil) {
		result.VideoDownloader.OnCipherFailure = func() string {
			invalidate403Caches(job, videoInfo.PlayerURL, cipherSolver, potProvider, "HLS")
			return ""
		}
	}

	return result, nil
}

// selectHlsByHeight finds an HLS variant matching the target height, optionally with FPS.
// Thin wrapper around the generic selectAtHeightIdx (audit reports/worker.md F35).
func selectHlsByHeight(variants []*engine.HlsVariant, targetHeight, targetFPS int) *engine.HlsVariant {
	idx := selectAtHeightIdx(variants, func(v *engine.HlsVariant) (int, int, int) {
		return v.Height, v.FPS, v.Bandwidth
	}, targetHeight, targetFPS)
	if idx < 0 {
		return nil
	}
	return variants[idx]
}

// selectNextLowerHls finds the best HLS variant below the target height,
// descending through available heights. Thin wrapper around selectNextLowerIdx
// (audit reports/worker.md F36).
func selectNextLowerHls(variants []*engine.HlsVariant, targetHeight int) *engine.HlsVariant {
	idx := selectNextLowerIdx(variants, func(v *engine.HlsVariant) (int, int) {
		return v.Height, v.Bandwidth
	}, targetHeight)
	if idx < 0 {
		return nil
	}
	return variants[idx]
}
