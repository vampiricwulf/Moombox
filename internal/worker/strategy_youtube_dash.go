package worker

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// resolveFreshDashURL re-decrypts the n-param on the original
// (pre-decryption) DASH stream URL using a freshly-resolved cipher.
// Called from the OnCipherFailure callback after InvalidateSolver —
// the next GetSolvers re-runs the player JS to pick up the rotated
// cipher. Returns the new decrypted URL or "" on failure (which
// falls through to the engine's legacy ErrQualityLost path so the
// orchestrator's higher-level retry logic refreshes the manifest).
//
// origURL must be the ORIGINAL stream BaseURL (before the initial
// n-param decryption); otherwise we'd re-decrypt an already-decrypted
// URL and produce garbage. DECISIONS #7.
//
// routedSolver is the composite cipher.Solver; it is tried first so
// the sidecar V8 ejs path handles n on cb017549-family players.
// Falls back to the goja resolver (cipherSolver) if nil or on error.
//
// jobID is included as the first key-value pair on every log line so a
// 403-triage grep by job stays complete even when the retry path is the
// only place that logged (audit: OnCipherFailure lines gain jobID).
func resolveFreshDashURL(ctx context.Context, jobID string, routedSolver cipher.Solver, cipherSolver *cipher.GojaResolver, playerURL, origURL string, logger interface {
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}) string {
	if origURL == "" {
		return ""
	}
	decrypted := cipher.RoutedDecryptNInURL(ctx, routedSolver, cipherSolver, playerURL, origURL)
	if decrypted == origURL {
		// No change means decryption either was a no-op (no n param) or
		// failed.  Fall back to the legacy GetSolvers path so we get the
		// same warning coverage as before.
		fresh, err := cipherSolver.GetSolvers(ctx, playerURL)
		if err != nil {
			logger.Warn("[Cipher] retry: GetSolvers failed", "jobID", jobID, "err", err)
			return ""
		}
		if fresh == nil || fresh.N == nil {
			logger.Warn("[Cipher] retry: solver missing N callable", "jobID", jobID)
			return ""
		}
		var decErr error
		decrypted, decErr = decryptNParamInURL(origURL, fresh.DecryptN)
		if decErr != nil {
			logger.Warn("[Cipher] retry: n-param decrypt failed", "jobID", jobID, "err", decErr)
			return ""
		}
	}
	logger.Info("[Cipher] retry: fresh n-param decryption succeeded; engine will swap BaseURL", "jobID", jobID)
	return decrypted
}

// DownloadDash sets up DASH segment downloaders for a live/post-live stream.
// Applies cipher decryption to manifest URL, n-parameter decryption to stream URLs,
// and injects PO token into the manifest URL path.
//
// routedSolver is the composite cipher.Solver (sidecar primary, goja fallback)
// and is used for all sig/n-param URL decryption.  cipherSolver (*GojaResolver)
// is kept for InvalidateSolver and GetSolvers in the OnCipherFailure retry path.
func DownloadDash(ctx context.Context, job *JobContext, videoInfo *youtube.VideoInfo, routedSolver cipher.Solver, cipherSolver *cipher.GojaResolver, potProvider *bgutils.PotProvider, isOnline func() bool) (*DownloadResult, error) {
	if videoInfo.DashManifestURL == "" {
		return nil, fmt.Errorf("no DASH manifest URL available")
	}

	dashURL := videoInfo.DashManifestURL

	// Step 1: Decrypt the DASH manifest URL itself (signature cipher).
	// Route through the composite solver (sidecar primary; goja fallback)
	// so sig decryption works on cb017549-family players where the goja
	// extractor cannot produce a sig algorithm.
	if videoInfo.PlayerURL != "" && (routedSolver != nil || cipherSolver != nil) {
		resolved, err := cipher.RoutedResolveURL(ctx, routedSolver, cipherSolver, cipher.ResolveURLRequest{
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

	// Step 2: Generate PO token once, used for both manifest URL injection and segment URLs.
	// Per audit reports/worker.md F29: previously generated twice (steps 2 and 5),
	// which was a cache hit but redundant work and confusing duplicate logging.
	//
	// GVS PO token: unconditional videoID binding (moonarchive parity),
	// minted from the watch-page challenge riding on videoInfo when present.
	// Supersedes the former poTokenBinding visitorData/channelID scheme —
	// see spec 2026-08-14 attestation POT coherence §3.
	var dashPoToken string
	if potProvider != nil {
		// REVERTED 2026-08-15: visitorData binding + cached /att/get minter,
		// the last GVS configuration observed downloading a live stream with
		// zero 403s. The videoID-bound, challenge-sourced, fresh-per-mint
		// policy stalled the first live capture at segment ~72 with a 403
		// storm. Reintroduce one variable at a time; live streams give
		// same-day feedback.
		bindingValue := poTokenBinding(job, videoInfo)
		poToken, err := potProvider.GeneratePoTokenString(ctx, bindingValue, false)
		if err != nil {
			job.Logger.Warn("[POT] GVS mint failed", "jobID", job.Job.ID,
				"binding", "visitorData", "err", err)
		} else if poToken != "" {
			dashPoToken = poToken
			dashURL = strings.TrimRight(dashURL, "/") + "/pot/" + poToken
			job.Logger.Info("[POT] GVS mint", "jobID", job.Job.ID,
				"binding", "visitorData", "tokenLength", len(poToken))
		} else {
			job.Logger.Warn("[POT] generator returned empty token", "jobID", job.Job.ID)
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
	// Distinguish "manifest had no parseable streams" from a downstream
	// "could not find suitable video stream" so debugging missing-formats
	// reports doesn't require re-running with packet capture (audit
	// reports/engine.md #9).
	if len(streams) == 0 {
		job.Logger.Debug("DASH manifest produced zero streams — likely empty AdaptationSets or all-non-numeric representation IDs",
			"manifestBytes", len(manifestData))
	}

	// Step 4: Decrypt n-parameter in each stream's BaseURL (prevents throttling/403).
	//
	// Snapshot the ORIGINAL (pre-decryption) BaseURL per itag BEFORE the
	// loop overwrites it — the OnCipherFailure handler below uses these
	// originals to re-decrypt with a freshly-solved cipher when YouTube
	// rotates mid-stream. Without the snapshot, the closure would re-decrypt
	// an already-decrypted URL and produce garbage. DECISIONS #7.
	originalBaseURLs := make(map[int]string, len(streams))
	for i := range streams {
		if streams[i].BaseURL != "" {
			originalBaseURLs[streams[i].Itag] = streams[i].BaseURL
		}
	}
	if videoInfo.PlayerURL != "" && (routedSolver != nil || cipherSolver != nil) {
		decryptCount := 0
		for i := range streams {
			if streams[i].BaseURL == "" {
				continue
			}
			decrypted := cipher.RoutedDecryptNInURL(ctx, routedSolver, cipherSolver, videoInfo.PlayerURL, streams[i].BaseURL)
			if decrypted != streams[i].BaseURL {
				streams[i].BaseURL = decrypted
				decryptCount++
			}
		}
		job.Logger.Debug("[Cipher] DASH n-param decryption complete", "decrypted", decryptCount, "total", len(streams))
	}

	// dashPoToken from step 2 is reused for segment URLs via DownloaderOptions.

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

	// Orchestrator-provided StartSeq takes priority (quality recovery/split).
	// Falls back to DB-stored last sequence for crash recovery.
	// The resume file (.resume.json) takes priority over both if it exists.
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
		result.VideoDownloader = engine.NewSegmentDownloader(engine.DownloaderOptions{
			BaseURL:       videoStream.BaseURL,
			OutputFile:    result.VideoPath,
			StartSeq:      videoStartSeq,
			ForceStartSeq: forceVideoSeq,
			InitURL:       videoStream.Initialization,
			PoToken:       dashPoToken,
			CookieHeader:  dashCookieHeader,
			MaxTimeout:    time.Duration(job.Config.MaximumTimeout) * time.Second,
			IsOnline:      isOnline,
			Logger:        newScopedLogger(job.Logger, "jobID", job.Job.ID, "stream", "video"),
			CheckStreamStatus: func(ctx context.Context) (bool, error) {
				info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
				if err != nil {
					return false, err
				}
				return info.StreamStatus != youtube.StreamLive, nil
			},
		})
		if videoInfo.PlayerURL != "" && (routedSolver != nil || cipherSolver != nil) {
			origURL := originalBaseURLs[videoStream.Itag]
			result.VideoDownloader.OnCipherFailure = func() string {
				invalidate403Caches(job, videoInfo.PlayerURL, cipherSolver, potProvider, "DASH video")
				return resolveFreshDashURL(ctx, job.Job.ID, routedSolver, cipherSolver, videoInfo.PlayerURL, origURL, job.Logger)
			}
		}
	}

	if audioStream != nil {
		result.HasAudio = true
		result.AudioPath = filepath.Join(job.StagingDir, "audio_stream")
		result.AudioDownloader = engine.NewSegmentDownloader(engine.DownloaderOptions{
			BaseURL:       audioStream.BaseURL,
			OutputFile:    result.AudioPath,
			StartSeq:      audioStartSeq,
			ForceStartSeq: forceAudioSeq,
			InitURL:       audioStream.Initialization,
			PoToken:       dashPoToken,
			CookieHeader:  dashCookieHeader,
			MaxTimeout:    time.Duration(job.Config.MaximumTimeout) * time.Second,
			IsOnline:      isOnline,
			Logger:        newScopedLogger(job.Logger, "jobID", job.Job.ID, "stream", "audio"),
			CheckStreamStatus: func(ctx context.Context) (bool, error) {
				info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
				if err != nil {
					return false, err
				}
				return info.StreamStatus != youtube.StreamLive, nil
			},
		})
		if videoInfo.PlayerURL != "" && (routedSolver != nil || cipherSolver != nil) {
			origURL := originalBaseURLs[audioStream.Itag]
			result.AudioDownloader.OnCipherFailure = func() string {
				invalidate403Caches(job, videoInfo.PlayerURL, cipherSolver, potProvider, "DASH audio")
				return resolveFreshDashURL(ctx, job.Job.ID, routedSolver, cipherSolver, videoInfo.PlayerURL, origURL, job.Logger)
			}
		}
	}

	return result, nil
}
