package worker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// errEmptyGvsToken is the missing_pot cause when the generator answers
// without an error but with no token.
var errEmptyGvsToken = errors.New("PO token generator returned an empty token")

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
	// pool is the format list selection, cipher re-selection and the
	// alternate picker all draw from. It is the caller's slice until a
	// missing_pot degrade swaps in a filtered COPY — never filtered in place.
	pool := videoInfo.Formats
	selected, result := selectVodFormats(job, pool, nil)
	if !result.HasVideo && !result.HasAudio {
		return nil, fmt.Errorf("no suitable formats found for VOD download")
	}
	videoResolved, audioResolved, err := resolveVodURLs(ctx, job, result, pool, routedSolver, cipherSolver, videoInfo.PlayerURL)
	if err != nil {
		return nil, err
	}

	// GVS PO token for the direct format URLs. yt-dlp's GVS_PO_TOKEN_POLICY
	// marks WEB-family HTTPS/DASH URLs required (not_required_for_premium,
	// and Moombox has no Premium detection), so a web_creator / web /
	// web_safari / watch_page URL answers the 1-byte Range probe 206 and then
	// 403s its first 5 MB chunk without one (the VOD 403 of 2026-09-29).
	// tv, web_embedded, visionos and android_vr carry no requirement, so
	// those ride bare — see youtube.GvsTokenRequired.
	// Video and audio can come from different clients, so the token is
	// minted at most once and passed per stream, only where that stream's
	// own Source requires it. Bound the same way as the DASH mint (see
	// strategy_youtube_dash.go's mint block for the binding rationale).
	//
	// missing_pot (yt-dlp's name for it): when a token is required but the
	// mint fails or comes back empty, sending the URL bare is a certain 403,
	// so the WEB-family formats leave the pool and the selection re-runs on
	// what is left (tv / web_embedded / visionos / android_vr — none of which
	// needs a token). A dropped format that carries a TokenFreeAlternate (the
	// dedup shadow) is replaced by it — same itag, bare URL — rather than
	// lost. A chosen stream that WAS lost triggers one cookieless re-extract
	// (see the block below). Nothing selectable left is an error naming the
	// cause.
	videoNeedsPot := result.VideoFormat != nil && youtube.GvsTokenRequired(result.VideoFormat.Source)
	audioNeedsPot := result.AudioFormat != nil && youtube.GvsTokenRequired(result.AudioFormat.Source)
	var vodPoToken string
	if potProvider != nil && (videoNeedsPot || audioNeedsPot) {
		bindingValue, bindingKind := gvsBinding(job, videoInfo)
		tok, mintErr := mintGvsPoToken(ctx, potProvider, bindingValue)
		if mintErr == nil && tok == "" {
			mintErr = errEmptyGvsToken
		}
		if mintErr != nil {
			var dropped []youtube.Format
			var swapped int
			pool, dropped, swapped = withoutGvsRequiredFormats(pool)
			job.Logger.Warn("[POT] missing_pot: no GVS token — dropping web-family formats",
				"jobID", job.Job.ID, "binding", bindingKind, "err", mintErr, "dropped", len(dropped), "swapped", swapped)
			// A chosen stream no shadow covered is gone from the degraded pool.
			// The cascade runs the cookieless chain only when web_creator was
			// inadequate, so an adequate web_creator pool holds no token-free
			// copy at all: fetch one, once, from visionos / android_vr, merge it
			// into a copy of the extraction's pool (the re-dedup attaches it as
			// the shadow) and degrade again. A failed fetch falls through to
			// the degraded pool. An already-cancelled job skips the fetch.
			lostVideo := result.VideoFormat != nil && !poolHasStream(pool, result.VideoFormat)
			lostAudio := result.AudioFormat != nil && !poolHasStream(pool, result.AudioFormat)
			if (lostVideo || lostAudio) && job.YT != nil && ctx.Err() == nil {
				job.Logger.Info("[POT] missing_pot: re-extracting with the cookieless clients",
					"jobID", job.Job.ID, "lostVideo", lostVideo, "lostAudio", lostAudio)
				rxCtx, cancel := context.WithTimeout(ctx, credentialRefreshTimeoutFor(job.Config))
				extra, rxErr := fetchCookielessFormats(job.YT, rxCtx, job.Job.VideoID)
				cancel()
				if rxErr != nil {
					job.Logger.Warn("[POT] missing_pot: cookieless re-extract failed — continuing with the degraded pool",
						"jobID", job.Job.ID, "err", rxErr)
				} else {
					merged := youtube.MergeFormatPool(ctx, videoInfo.Formats, extra)
					pool, dropped, swapped = withoutGvsRequiredFormats(merged)
					job.Logger.Info("[POT] missing_pot: cookieless re-extract merged",
						"jobID", job.Job.ID, "added", len(extra), "dropped", len(dropped), "swapped", swapped)
				}
			}
			selected, result = selectVodFormats(job, pool, dropped)
			if !result.HasVideo && !result.HasAudio {
				return nil, fmt.Errorf("VOD: no formats usable without a GVS PO token (mint failed: %w)", mintErr)
			}
			videoResolved, audioResolved, err = resolveVodURLs(ctx, job, result, pool, routedSolver, cipherSolver, videoInfo.PlayerURL)
			if err != nil {
				return nil, err
			}
			// The degraded pool holds no token-requiring format.
			videoNeedsPot, audioNeedsPot = false, false
			videoSource, audioSource := "", ""
			if result.VideoFormat != nil {
				videoSource = result.VideoFormat.Source
			}
			if result.AudioFormat != nil {
				audioSource = result.AudioFormat.Source
			}
			job.Logger.Info("[POT] missing_pot: serving token-free formats",
				"jobID", job.Job.ID, "videoSource", videoSource, "audioSource", audioSource)
		} else {
			vodPoToken = tok
			videoSource, audioSource := "", ""
			if result.VideoFormat != nil {
				videoSource = result.VideoFormat.Source
			}
			if result.AudioFormat != nil {
				audioSource = result.AudioFormat.Source
			}
			job.Logger.Info("[POT] GVS mint", "jobID", job.Job.ID,
				"binding", bindingKind, "tokenLength", len(tok),
				"videoSource", videoSource, "audioSource", audioSource)
		}
	}
	videoPoToken, audioPoToken := "", ""
	if videoNeedsPot {
		videoPoToken = vodPoToken
	}
	if audioNeedsPot {
		audioPoToken = vodPoToken
	}

	if err := setAsideLiveShapesForVod(job); err != nil { // staging_shapes.go
		return nil, err
	}

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
			BaseURL:        videoResolved,
			OutputFile:     result.VideoPath,
			StartSeq:       0,
			EndSeq:         0, // Single file download
			IsDirectURL:    true,
			StreamID:       vodStreamID(job.Job.VideoID, result.VideoFormat),
			SegmentWorkers: job.Config.SegmentWorkers,
			PoToken:        videoPoToken,
			Logger:         newScopedLogger(job.Logger, "jobID", job.Job.ID, "stream", "video"),
		})
	}

	if result.HasAudio && result.AudioPath != "" && audioResolved != "" {
		result.AudioDownloader = engine.NewSegmentDownloader(engine.DownloaderOptions{
			BaseURL:        audioResolved,
			OutputFile:     result.AudioPath,
			StartSeq:       0,
			EndSeq:         0,
			IsDirectURL:    true,
			StreamID:       vodStreamID(job.Job.VideoID, result.AudioFormat),
			SegmentWorkers: job.Config.SegmentWorkers,
			PoToken:        audioPoToken,
			Logger:         newScopedLogger(job.Logger, "jobID", job.Job.ID, "stream", "audio"),
		})
	}

	return result, nil
}

// vodSelectionBounds folds the job's quality_preference into the whole-file
// selector's two knobs. A preferred size below the cap becomes the cap — the
// selector then takes that size, or the largest one below it, which is what
// the live paths' preference matching does — and an explicit "…p60" asks for
// 60 fps whatever prefer_60fps says; a suffix-less preference leaves the
// setting in charge, as on the live paths. The VOD path read the preference
// only for audio_only, so a "720p" channel's uploads and finished VODs
// downloaded at the global 2160 cap.
func vodSelectionBounds(job *JobContext) (maxRes int, prefer60fps bool) {
	maxRes, prefer60fps = job.Config.MaxVideoResolution, job.Config.Prefer60fps
	height, fps := ParseQualityPreference(job.Job.QualityPreference)
	if height > 0 && (maxRes <= 0 || height < maxRes) {
		maxRes = height
	}
	if fps > 0 {
		prefer60fps = fps >= 50
	}
	return maxRes, prefer60fps
}

// selectVodFormats runs the VOD format selection over formats: the automatic
// pick, the per-job manual itag overrides, the audio_only preference, and the
// DownloadResult fields derived from them (which streams exist, their
// formats and staging paths). The returned formats point into formats.
//
// dropped is nil on the first run. On a missing_pot re-run it holds the
// WEB-family formats removed from the pool, so a manual itag that pointed at
// one of them says why it is being ignored instead of "not found" — or, when
// the drop swapped that format for its token-free shadow (the itag is still
// in formats), says which client now serves it.
func selectVodFormats(job *JobContext, formats, dropped []youtube.Format) (youtube.SelectedFormats, *DownloadResult) {
	maxRes, prefer60fps := vodSelectionBounds(job)
	selected := youtube.SelectBestFormatsWithLogger(formats, maxRes, prefer60fps, job.Logger)

	// Per-job itag overrides (from manual format selection in the UI).
	// A value of -1 means "explicitly no video/audio" (skip that track).
	if job.Job.SelectedVideoItag != nil {
		itag := *job.Job.SelectedVideoItag
		if itag == -1 {
			selected.Video = nil
		} else if itag > 0 {
			found := false
			for i := range formats {
				f := &formats[i]
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
				if droppedHasItag(dropped, itag, "video") {
					job.Logger.Info(fmt.Sprintf("[FormatSelector] Manual video itag %d served from %s after missing_pot", itag, selected.Video.Source))
				}
			} else if droppedHasItag(dropped, itag, "video") {
				job.Logger.Warn(fmt.Sprintf("[FormatSelector] Manual video itag %d requires a GVS token none could be minted; falling back to auto", itag))
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
			for i := range formats {
				f := &formats[i]
				if f.Itag == itag && strings.Contains(f.MimeType, "audio") && f.URL != "" {
					selected.Audio = f
					found = true
					break
				}
			}
			if found {
				job.Logger.Debug(fmt.Sprintf("[FormatSelector] Manual audio selection: itag %d %dbps", itag, selected.Audio.Bitrate))
				if droppedHasItag(dropped, itag, "audio") {
					job.Logger.Info(fmt.Sprintf("[FormatSelector] Manual audio itag %d served from %s after missing_pot", itag, selected.Audio.Source))
				}
			} else if droppedHasItag(dropped, itag, "audio") {
				job.Logger.Warn(fmt.Sprintf("[FormatSelector] Manual audio itag %d requires a GVS token none could be minted; falling back to auto", itag))
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

	return selected, result
}

// resolveVodURLs resolves sig + n on the chosen video / audio formats so the
// engine is handed fetchable URLs. Stage 3 of the cipher pipeline rework:
// parseFormats leaves URLs raw (with sigCipher entries carrying EncryptedSig
// + the bare `url=` value). Bound to two attempts via re-selection — the
// alternate is drawn from pool, the same list the selection ran over — so a
// fully broken cipher state surfaces cleanly. A re-selection rewrites
// result.VideoFormat / result.AudioFormat.
func resolveVodURLs(ctx context.Context, job *JobContext, result *DownloadResult, pool []youtube.Format, routedSolver cipher.Solver, cipherSolver *cipher.GojaResolver, playerURL string) (videoResolved, audioResolved string, err error) {
	if result.HasVideo && result.VideoFormat != nil {
		resolvedURL, err := resolveFormatURL(ctx, result.VideoFormat, routedSolver, cipherSolver, playerURL, job.Logger)
		if err != nil {
			job.Logger.Warn("[Cipher] VOD video resolve failed; trying re-selection",
				"itag", result.VideoFormat.Itag, "err", err)
			alt := reselectVodWithout(job, pool, result.VideoFormat.Itag)
			if !alt.HasVideo || alt.VideoFormat == nil {
				return "", "", fmt.Errorf("VOD: video URL resolve failed and no alternate format: %w", err)
			}
			resolvedURL, err = resolveFormatURL(ctx, alt.VideoFormat, routedSolver, cipherSolver, playerURL, job.Logger)
			if err != nil {
				return "", "", fmt.Errorf("VOD: video URL resolve failed for primary and alternate: %w", err)
			}
			// The re-selection's whole stream shape, not just its video: a
			// progressive primary replaced by a video-only alternate needs the
			// audio the selection paired with it (keeping the old shape made
			// a silent file), and an adaptive one replaced by a progressive
			// alternate needs no separate audio. The audio block below then
			// resolves whatever audio this adopted.
			result.HasVideo, result.VideoFormat, result.VideoPath = alt.HasVideo, alt.VideoFormat, alt.VideoPath
			result.HasAudio, result.AudioFormat, result.AudioPath = alt.HasAudio, alt.AudioFormat, alt.AudioPath
			job.Logger.Info("[Cipher] VOD video re-selection succeeded", "newItag", alt.VideoFormat.Itag)
		}
		videoResolved = resolvedURL
	}
	if result.HasAudio && result.AudioFormat != nil {
		resolvedURL, err := resolveFormatURL(ctx, result.AudioFormat, routedSolver, cipherSolver, playerURL, job.Logger)
		if err != nil {
			job.Logger.Warn("[Cipher] VOD audio resolve failed; trying re-selection",
				"itag", result.AudioFormat.Itag, "err", err)
			retry := reselectVodWithout(job, pool, result.AudioFormat.Itag).AudioFormat
			if retry == nil {
				return "", "", fmt.Errorf("VOD: audio URL resolve failed and no alternate format: %w", err)
			}
			resolvedURL, err = resolveFormatURL(ctx, retry, routedSolver, cipherSolver, playerURL, job.Logger)
			if err != nil {
				return "", "", fmt.Errorf("VOD: audio URL resolve failed for primary and alternate: %w", err)
			}
			result.AudioFormat = retry
			job.Logger.Info("[Cipher] VOD audio re-selection succeeded", "newItag", retry.Itag)
		}
		audioResolved = resolvedURL
	}
	return videoResolved, audioResolved, nil
}

// reselectVodWithout re-runs the job's own VOD selection over pool minus the
// format whose URL would not resolve. The alternate it used to take was
// simply the highest-bitrate format of the same kind, which ignored the
// resolution cap, the quality preference, prefer_60fps and audio_only alike:
// a 720p-capped job whose 720p format failed its signature downloaded the
// 2160p one, and an audio-only job whose audio failed downloaded video.
func reselectVodWithout(job *JobContext, pool []youtube.Format, failedItag int) *DownloadResult {
	rest := make([]youtube.Format, 0, len(pool))
	for _, f := range pool {
		if f.Itag != failedItag {
			rest = append(rest, f)
		}
	}
	_, alt := selectVodFormats(job, rest, nil)
	return alt
}

// withoutGvsRequiredFormats splits formats into the ones usable without a GVS
// PO token and the WEB-family ones that require it. A dropped format carrying
// a TokenFreeAlternate (the dedup shadow) contributes that alternate to kept
// instead — same itag, a URL that needs no token — and counts in swapped; it
// is still reported in dropped. Both slices are fresh: the caller's list is
// never filtered in place, and the alternate is copied out of its pointer.
func withoutGvsRequiredFormats(formats []youtube.Format) (kept, dropped []youtube.Format, swapped int) {
	kept = make([]youtube.Format, 0, len(formats))
	for _, f := range formats {
		if !youtube.GvsTokenRequired(f.Source) {
			kept = append(kept, f)
			continue
		}
		dropped = append(dropped, f)
		if alt := f.TokenFreeAlternate; alt != nil && !youtube.GvsTokenRequired(alt.Source) {
			a := *alt
			a.TokenFreeAlternate = nil
			kept = append(kept, a)
			swapped++
		}
	}
	return kept, dropped, swapped
}

// droppedHasItag reports whether a missing_pot drop removed a format with
// this itag of the given kind ("video" / "audio"), matched the way the manual
// override matches.
func droppedHasItag(dropped []youtube.Format, itag int, kind string) bool {
	for i := range dropped {
		f := &dropped[i]
		if f.Itag == itag && strings.Contains(f.MimeType, kind) && f.URL != "" {
			return true
		}
	}
	return false
}

// poolHasStream reports whether pool still holds f's stream — same itag and
// audio track, a URL to fetch — the test for a chosen format a missing_pot
// degrade dropped without a shadow to swap in.
func poolHasStream(pool []youtube.Format, f *youtube.Format) bool {
	for i := range pool {
		if pool[i].Itag == f.Itag && pool[i].AudioTrackID == f.AudioTrackID && pool[i].URL != "" {
			return true
		}
	}
	return false
}
