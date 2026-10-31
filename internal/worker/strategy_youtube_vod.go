package worker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
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
// isOnline is the connectivity check the downloaders wait an outage out on,
// as on the live strategies; nil disables the wait.
func DownloadVod(ctx context.Context, job *JobContext, videoInfo *youtube.VideoInfo, routedSolver cipher.Solver, cipherSolver *cipher.GojaResolver, potProvider *bgutils.PotProvider, isOnline func() bool) (*DownloadResult, error) {
	// A run (downloadVod) that cannot record what it selected fails, before
	// anything is built or written, with one of two causes that a run over
	// other formats or another binding answers — each at most once, in
	// whichever order the runs meet them, so a setup makes three runs at
	// most. Every run reads the setup's degrades (vodDegrades): one
	// cookieless re-extract both degrades share, the challenge-free formats
	// once that degrade has run, and the mint.
	st := &vodDegrades{}
	info, trusted := videoInfo, false
	for {
		res, err := downloadVod(ctx, job, info, st, routedSolver, cipherSolver, potProvider, isOnline)
		if err == nil || ctx.Err() != nil {
			return res, err
		}
		switch {
		case st.challengeFree == nil && errors.Is(err, errChallengesUnsolvable):
			// The challenge-free degrade (audit G10). A resolve failed for a
			// reason every format needing the player JS shares — the sidecar
			// down or disabled, a player ejs cannot solve or none could
			// fetch, an n no solver answered, no player URL — the first
			// selection's, or the one a missing_pot degrade re-ran. That
			// leaves the job only the formats that need no JS: the
			// extraction's, every challenge-bearing copy dropped, plus the
			// cookieless chain's (challengeFreeFormats, over the setup's one
			// re-extract), from which every later run selects under the
			// first one's rules — a missing_pot degrade among them, and the
			// guessed binding's trusted run, which therefore keeps this
			// degrade rather than undoing it. yt-dlp gets there by skipping
			// each format whose challenge fails before it records the stream
			// (_video.py:3509-3511, 3557-3580, 3587-3589), so visionos's copy
			// is the one it keeps.
			st.challengeFree = err
			st.formats = challengeFreeFormats(ctx, job, videoInfo.Formats, &st.cookieless, err, "VOD")
		case !trusted && errors.Is(err, errGuessedGvsBinding):
			// D12's last resort. The binding is a guess, so the run counted
			// the token as missing and degraded — and the degrade left a
			// chosen stream with no token-free copy: nothing selectable at
			// all, a stream whose only copies left are segments, a video or
			// audio stream gone from the selection, or a pinned rendition
			// gone from the pool (only those failures wrap the
			// guessed-binding cause, each before anything is built or
			// written). The run is made once more with the guess trusted:
			// its selection is the first one again — over the challenge-free
			// formats when that degrade has run — and the token minted under
			// the guess rides it.
			job.Logger.Warn("[POT] missing_pot: a chosen stream has no token-free copy — minting under the guessed binding for the web-family formats",
				"jobID", job.Job.ID, "binding", info.GvsBindingKind, "err", err)
			// A mint that fails there too degrades again, on the mint's own
			// failure, as any failed mint's does — a lost pin falls to the
			// automatic pick, a lost stream is left out, nothing selectable
			// is that run's error — and that run's outcome is the job's,
			// save a degraded selection that fails on the challenges, which
			// the challenge-free degrade answers as it answers any run's.
			guessTrusted := *info
			guessTrusted.GvsBindingGuessed = false
			info, trusted = &guessTrusted, true
		default:
			return res, err
		}
	}
}

// vodDegrades is one VOD setup's state across DownloadVod's runs of
// downloadVod. Not safe for concurrent use: a setup runs on one goroutine.
type vodDegrades struct {
	// cookieless is the setup's cookieless re-extract, fetched at most once
	// whichever degrade asks first and in whichever run: the missing_pot one
	// and the challenge-free one each want the cookieless chain's copies.
	cookieless cookielessReExtract
	// challengeFree is the systemic resolve failure the challenge-free
	// degrade answered, nil until it has; formats is that degrade's list
	// (challengeFreeFormats), which every later run selects from in place of
	// the extraction's.
	challengeFree error
	formats       []youtube.Format
	// mint is the setup's GVS mint, made at most once for each reading of
	// the binding (vodDegrades.gvsToken).
	mint *vodMint
}

// vodMint is one GVS mint's answer, and whether the binding it was asked
// under was distrusted as a guess (youtube.VideoInfo.GvsBindingGuessed).
type vodMint struct {
	guessed bool
	tok     string
	err     error
}

// gvsToken is a run's GVS token (strategyGvsToken, the guess distrusted):
// minted the first time a run asks, and the same answer for a later run
// that reads the binding as that one did — a run the challenge-free degrade
// made after a missing_pot degrade's run failed on the challenges does not
// mint again against the failure it degraded on. The guessed binding's
// trusted run reads it otherwise, and mints.
func (st *vodDegrades) gvsToken(ctx context.Context, p *bgutils.PotProvider, info *youtube.VideoInfo, binding string) (string, error) {
	if st.mint == nil || st.mint.guessed != info.GvsBindingGuessed {
		tok, err := strategyGvsToken(ctx, p, info, binding, false)
		st.mint = &vodMint{guessed: info.GvsBindingGuessed, tok: tok, err: err}
	}
	return st.mint.tok, st.mint.err
}

// downloadVod is DownloadVod's one run over videoInfo, under the setup's
// degrades so far (st); see DownloadVod. A resolve that fails systemically
// (errChallengesUnsolvable) — the first selection's, or a missing_pot
// degrade's — ends the run, for DownloadVod's challenge-free degrade to
// answer.
func downloadVod(ctx context.Context, job *JobContext, videoInfo *youtube.VideoInfo, st *vodDegrades, routedSolver cipher.Solver, cipherSolver *cipher.GojaResolver, potProvider *bgutils.PotProvider, isOnline func() bool) (*DownloadResult, error) {
	// formats is the list this run selects from: the extraction's, or the
	// challenge-free degrade's once a run has failed on the player's JS
	// challenges (st.formats) — the list a missing_pot degrade then merges
	// into too. pool is the format list selection, cipher re-selection and
	// the alternate picker all draw from: formats without the ones a
	// whole-file GET cannot fetch (wholeFileVodPool) — a filtered COPY, as a
	// missing_pot degrade's is, never the caller's slice filtered in place.
	formats, cause := videoInfo.Formats, st.challengeFree
	if cause != nil {
		formats = st.formats
	}
	pool, segmented := wholeFileVodPool(formats)
	selected, result := selectVodFormats(job, pool, nil, segmented)
	if err := segmentedStreamLost(job, selected, segmented); err != nil {
		if cause != nil {
			return nil, fmt.Errorf("%w; its only copies that need no player JS are those (%w)", err, cause)
		}
		// selectDownloadStrategy sends a pool that holds a segmented format
		// to DASH whenever a manifest came with it (its OTF arm), so this
		// one had none.
		return nil, fmt.Errorf("%w, and no DASH manifest came with them", err)
	}
	if !result.HasVideo && !result.HasAudio {
		if cause != nil {
			return nil, fmt.Errorf("VOD: no format is left that can be fetched without solving the player JS (%w)", cause)
		}
		return nil, fmt.Errorf("no suitable formats found for VOD download")
	}
	videoResolved, audioResolved, err := resolveVodURLs(ctx, job, result, pool, nil, segmented, routedSolver, cipherSolver, videoInfo.PlayerURL)
	if err != nil {
		return nil, err
	}
	if cause != nil {
		videoSource, audioSource := "", ""
		if result.VideoFormat != nil {
			videoSource = result.VideoFormat.Source
		}
		if result.AudioFormat != nil {
			audioSource = result.AudioFormat.Source
		}
		job.Logger.Info("[Cipher] VOD: serving formats that need no player JS",
			"jobID", job.Job.ID, "videoSource", videoSource, "audioSource", audioSource)
	}

	// GVS PO token for the direct format URLs. yt-dlp's GVS_PO_TOKEN_POLICY
	// marks WEB-family HTTPS/DASH URLs required (not_required_for_premium),
	// so a web_creator / web / web_safari / watch_page URL answers the 1-byte
	// Range probe 206 and then 403s its first 5 MB chunk without one (the VOD
	// 403 of 2026-09-29).
	// tv, web_embedded, visionos and android_vr carry no requirement, so
	// those ride bare — see youtube.GvsTokenRequired — until a refusal long
	// before a URL's expiry says otherwise (the whole-file refresh's token
	// fallback, refreshVodURL).
	// Video and audio can come from different clients, so the token is
	// minted at most once and passed per stream, only where that stream's
	// own Source requires it. Bound the same way as the DASH mint (see
	// strategy_youtube_dash.go's mint block for the binding rationale).
	//
	// missing_pot (yt-dlp's name for it): when a chosen stream's token is
	// mandatory (youtube.FormatGvsTokenMandatory) but the mint fails, comes
	// back empty or would be bound to a guess (D12, errGuessedGvsBinding),
	// sending the URL bare is a certain 403, so the formats whose token is
	// mandatory leave the pool and the selection re-runs on what is left (tv
	// / web_embedded / visionos / android_vr — none of which needs a token —
	// and the web family's itag 18, which upstream exempts). A dropped format
	// that carries a TokenFreeAlternate (the dedup shadow) is replaced by it —
	// same itag, bare URL — rather than lost. A chosen stream that WAS lost
	// triggers one cookieless re-extract (see the block below). Nothing
	// selectable left is an error naming the cause — which, for a guessed
	// binding, DownloadVod answers by running once more with the guess
	// trusted, the one exit D12 leaves it. A chosen stream whose token is only
	// recommended — itag 18, or a Premium session's web family — rides bare
	// instead, as upstream saves it: its token is attached when minted, never
	// required.
	videoNeedsPot := result.VideoFormat != nil && youtube.GvsTokenRequired(result.VideoFormat.Source)
	audioNeedsPot := result.AudioFormat != nil && youtube.GvsTokenRequired(result.AudioFormat.Source)
	var vodPoToken string
	if potProvider != nil && (videoNeedsPot || audioNeedsPot) {
		bindingValue, bindingKind := gvsBinding(job, videoInfo)
		tok, mintErr := st.gvsToken(ctx, potProvider, videoInfo, bindingValue)
		mandatory := youtube.FormatGvsTokenMandatory(result.VideoFormat, videoInfo.IsPremium) ||
			youtube.FormatGvsTokenMandatory(result.AudioFormat, videoInfo.IsPremium)
		if mintErr != nil && !mandatory {
			job.Logger.Warn("[POT] no GVS token — the chosen web-family formats ride bare: their token is recommended, not required",
				"jobID", job.Job.ID, "binding", bindingKind, "err", mintErr, "premium", videoInfo.IsPremium,
				"videoItag", formatItag(result.VideoFormat), "audioItag", formatItag(result.AudioFormat))
			videoNeedsPot, audioNeedsPot = false, false
		} else if mintErr != nil {
			var dropped, removed []youtube.Format
			var swapped int
			pool, dropped, swapped = withoutGvsRequiredFormats(pool, videoInfo.IsPremium)
			// A shadow swapped in is another response's copy, which can be one
			// a whole-file GET cannot fetch: it leaves the pool as the
			// extraction's own did, and its stream counts as lost below. What
			// leaves joins segmented, the list the degraded selection is held
			// to once it has run.
			pool, removed = wholeFileVodPool(pool)
			segmented = append(segmented, removed...)
			job.Logger.Warn("[POT] missing_pot: no GVS token — dropping web-family formats",
				"jobID", job.Job.ID, "binding", bindingKind, "err", mintErr, "dropped", len(dropped), "swapped", swapped)
			// A chosen stream no shadow covered is gone from the degraded pool.
			// The cascade runs the cookieless chain only when web_creator was
			// inadequate, so an adequate web_creator pool holds no token-free
			// copy at all: fetch one, once, from visionos / android_vr, merge it
			// into a copy of formats — the extraction's, or the challenge-free
			// degrade's, which takes only the fetch's copies that need no
			// player JS — (the re-dedup attaches it as the shadow) and degrade
			// again. The fetch is the setup's one (st.cookieless): a
			// challenge-free degrade that ran first made it already. A failed
			// fetch falls through to the degraded pool. An already-cancelled
			// job skips the fetch.
			lostVideo := result.VideoFormat != nil && !poolHasStream(pool, result.VideoFormat)
			lostAudio := result.AudioFormat != nil && !poolHasStream(pool, result.AudioFormat)
			if (lostVideo || lostAudio) && job.YT != nil && ctx.Err() == nil {
				job.Logger.Info("[POT] missing_pot: re-extracting with the cookieless clients",
					"jobID", job.Job.ID, "lostVideo", lostVideo, "lostAudio", lostAudio)
				extra, rxErr := st.cookieless.fetch(ctx, job)
				if rxErr != nil {
					job.Logger.Warn("[POT] missing_pot: cookieless re-extract failed — continuing with the degraded pool",
						"jobID", job.Job.ID, "err", rxErr)
				} else {
					if cause != nil {
						// Only the copies the challenge-free degrade can
						// fetch: formats holds no other.
						extra, _ = withoutChallengeFormats(extra)
					}
					merged := youtube.MergeFormatPool(ctx, formats, extra)
					pool, dropped, swapped = withoutGvsRequiredFormats(merged, videoInfo.IsPremium)
					pool, removed = wholeFileVodPool(pool)
					segmented = append(segmented, removed...)
					job.Logger.Info("[POT] missing_pot: cookieless re-extract merged",
						"jobID", job.Job.ID, "added", len(extra), "dropped", len(dropped), "swapped", swapped)
				}
			}
			// D12 prefers a token-free copy of what the job would record and
			// lets the guessed token ride only where there is none. A pinned
			// rendition is what the job would record: one the degrade dropped
			// with no token-free copy of its own (no shadow, nothing from the
			// cookieless chain) is not stood in for by another itag, as it is
			// when no token can be had at all — under the guess one can. So
			// the selection is not re-run (its pin arms would say the pin
			// needs a token none could be minted, where none was asked for):
			// the failure wraps the guessed-binding cause, and DownloadVod's
			// trusted run honours the pin with the guessed token.
			guessed := errors.Is(mintErr, errGuessedGvsBinding)
			if lost := vodPinsLost(job, result, pool); guessed && len(lost) > 0 {
				return nil, fmt.Errorf("VOD: the %s has no token-free copy: %w", strings.Join(lost, " and "), mintErr)
			}
			first := result
			selected, result = selectVodFormats(job, pool, dropped, segmented)
			if !result.HasVideo && !result.HasAudio {
				if cause != nil {
					return nil, fmt.Errorf("VOD: no formats usable without a GVS PO token or the player JS (mint failed: %w; %w)", mintErr, cause)
				}
				return nil, fmt.Errorf("VOD: no formats usable without a GVS PO token (mint failed: %w)", mintErr)
			}
			// The degraded selection is held to the first one's rule. The
			// dedup lets a stream's segmented tv copy lose to a web-family
			// file and rides it as that file's TokenFreeAlternate, so the swap
			// can land on the one copy the re-filter leaves out: a video whose
			// whole file needs the token no mint produced has nothing left but
			// segments. Without this the job archived its audio alone, with no
			// error — where, before the whole-file pool, it fetched the shadow
			// and failed as a fragment.
			if err := segmentedStreamLost(job, selected, segmented); err != nil {
				return nil, fmt.Errorf("%w; its whole-file copies need a GVS PO token, and none could be minted (mint failed: %w)", err, mintErr)
			}
			// The same rule for a whole stream: a degrade that kept the audio
			// and lost the video (or the reverse) would archive the one stream
			// alone — an audio-only file for a video job — where a token
			// minted under the guess fetches both. A lesser rendition of the
			// same kind is a token-free copy of the stream, and is kept.
			if lost := vodStreamsLost(first, result); guessed && len(lost) > 0 {
				return nil, fmt.Errorf("VOD: the %s stream has no token-free copy: %w", strings.Join(lost, " and "), mintErr)
			}
			// The degraded selection's resolve can fail where the first one's
			// did not — a tv copy's signature where the web family's n was
			// solved — and a systemic failure here ends the run as the first
			// selection's does: DownloadVod's challenge-free degrade answers
			// it, and the run it makes degrades for the token again over the
			// formats that need no player JS, on this run's mint and
			// re-extract.
			videoResolved, audioResolved, err = resolveVodURLs(ctx, job, result, pool, dropped, segmented, routedSolver, cipherSolver, videoInfo.PlayerURL)
			if err != nil {
				return nil, err
			}
			// The degraded pool holds no format a token is mandatory for,
			// and none was minted: nothing rides one.
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

	// The formats are settled: every selection, re-selection and degrade
	// has run. A damaged rendition the operator pinned is recorded as asked,
	// and says so once (warnDamagedPins).
	warnDamagedPins(job, result)

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
		result.VideoDownloader = newSegmentDownloader(engine.DownloaderOptions{
			BaseURL:             videoResolved,
			OutputFile:          result.VideoPath,
			StartSeq:            0,
			EndSeq:              0, // Single file download
			IsDirectURL:         true,
			StreamID:            vodStreamID(job.Job.VideoID, result.VideoFormat),
			SegmentWorkers:      job.Config.SegmentWorkers,
			PoToken:             videoPoToken,
			IsOnline:            isOnline,
			OnCredentialRefresh: vodURLRefresh(ctx, job, videoInfo, *result.VideoFormat, videoResolved, routedSolver, cipherSolver, vodRefreshMinter(potProvider), "VOD video"),
			Logger:              newScopedLogger(job.Logger, "jobID", job.Job.ID, "stream", "video"),
		})
	}

	if result.HasAudio && result.AudioPath != "" && audioResolved != "" {
		result.AudioDownloader = newSegmentDownloader(engine.DownloaderOptions{
			BaseURL:             audioResolved,
			OutputFile:          result.AudioPath,
			StartSeq:            0,
			EndSeq:              0,
			IsDirectURL:         true,
			StreamID:            vodStreamID(job.Job.VideoID, result.AudioFormat),
			SegmentWorkers:      job.Config.SegmentWorkers,
			PoToken:             audioPoToken,
			IsOnline:            isOnline,
			OnCredentialRefresh: vodURLRefresh(ctx, job, videoInfo, *result.AudioFormat, audioResolved, routedSolver, cipherSolver, vodRefreshMinter(potProvider), "VOD audio"),
			Logger:              newScopedLogger(job.Logger, "jobID", job.Job.ID, "stream", "audio"),
		})
	}

	// The downloaders' first request, the size probe, waits out the pre-roll
	// ad of the response each chosen format came from — the extraction's, or
	// the missing_pot re-extract's — or it is refused with a 403 like every
	// URL of that response (waitForPlayback).
	at, source := latestPlayback(result.VideoFormat, result.AudioFormat)
	if err := waitForPlayback(ctx, job, videoInfo, at, source, "VOD"); err != nil {
		return nil, fmt.Errorf("VOD: waiting out the pre-roll ad: %w", err)
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
//
// segmented is what the whole-file pool left out (wholeFileVodPool), for the
// same reason: a pin of one says it is segmented, not available as a whole
// file, rather than "not found" — the format picker offers it, marked
// segmented, because the manifest-free live and post-live paths fetch it as
// fragments. A token drop is named first: where the dedup's shadow swap left
// only a segmented copy of a pinned stream, its whole file is the one the
// token was needed for. A damaged pin has no arm here: damage only ranks a
// rendition lower, it never takes one out of the pool, so a damaged pin is
// found and honoured — and DownloadVod says so once the formats are settled
// (warnDamagedPins), not here, where the cipher re-selection would say it
// again.
func selectVodFormats(job *JobContext, formats, dropped, segmented []youtube.Format) (youtube.SelectedFormats, *DownloadResult) {
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
			} else if droppedHasItag(segmented, itag, "video") {
				job.Logger.Warn(fmt.Sprintf("[FormatSelector] Manual video itag %d is segmented, not available as a whole file; falling back to auto", itag))
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
			} else if droppedHasItag(segmented, itag, "audio") {
				job.Logger.Warn(fmt.Sprintf("[FormatSelector] Manual audio itag %d is segmented, not available as a whole file; falling back to auto", itag))
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
// alternate is drawn from pool, the same list the selection ran over, and
// dropped and segmented are what that selection was handed beside it
// (selectVodFormats) — so a fully broken cipher state surfaces cleanly. A
// re-selection rewrites result.VideoFormat / result.AudioFormat.
//
// A failure every format needing the player JS shares (systemicResolveFailure:
// the sidecar down or not configured, a player ejs cannot solve, an n no
// solver answered, no player URL) is not re-selected around one itag at a
// time — the alternate would fail alike — and comes back at once, marked
// errChallengesUnsolvable, for DownloadVod's challenge-free fallback.
func resolveVodURLs(ctx context.Context, job *JobContext, result *DownloadResult, pool, dropped, segmented []youtube.Format, routedSolver cipher.Solver, cipherSolver *cipher.GojaResolver, playerURL string) (videoResolved, audioResolved string, err error) {
	if result.HasVideo && result.VideoFormat != nil {
		resolvedURL, err := resolveFormatURL(ctx, result.VideoFormat, routedSolver, cipherSolver, playerURL, job.Logger)
		if systemicResolveFailure(err, result.VideoFormat, playerURL) {
			return "", "", fmt.Errorf("VOD: video URL resolve failed: %w", markChallengeFailure(err, result.VideoFormat, playerURL))
		}
		if err != nil {
			job.Logger.Warn("[Cipher] VOD video resolve failed; trying re-selection",
				"itag", result.VideoFormat.Itag, "err", err)
			alt := reselectVodWithout(job, pool, dropped, segmented, result.VideoFormat.Itag)
			if !alt.HasVideo || alt.VideoFormat == nil {
				return "", "", fmt.Errorf("VOD: video URL resolve failed and no alternate format: %w", err)
			}
			resolvedURL, err = resolveFormatURL(ctx, alt.VideoFormat, routedSolver, cipherSolver, playerURL, job.Logger)
			if err != nil {
				return "", "", fmt.Errorf("VOD: video URL resolve failed for primary and alternate: %w", markChallengeFailure(err, alt.VideoFormat, playerURL))
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
		if systemicResolveFailure(err, result.AudioFormat, playerURL) {
			return "", "", fmt.Errorf("VOD: audio URL resolve failed: %w", markChallengeFailure(err, result.AudioFormat, playerURL))
		}
		if err != nil {
			job.Logger.Warn("[Cipher] VOD audio resolve failed; trying re-selection",
				"itag", result.AudioFormat.Itag, "err", err)
			retry := reselectVodWithout(job, pool, dropped, segmented, result.AudioFormat.Itag).AudioFormat
			if retry == nil {
				return "", "", fmt.Errorf("VOD: audio URL resolve failed and no alternate format: %w", err)
			}
			resolvedURL, err = resolveFormatURL(ctx, retry, routedSolver, cipherSolver, playerURL, job.Logger)
			if err != nil {
				return "", "", fmt.Errorf("VOD: audio URL resolve failed for primary and alternate: %w", markChallengeFailure(err, retry, playerURL))
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
// 2160p one, and an audio-only job whose audio failed downloaded video. It is
// handed the dropped and segmented lists the selection it replaces was
// handed, so a pin that selection gave up on is reported for the same reason
// again; without them the pin loop warned "not found" of an itag the line
// before had just called segmented, or token-bound.
func reselectVodWithout(job *JobContext, pool, dropped, segmented []youtube.Format, failedItag int) *DownloadResult {
	rest := make([]youtube.Format, 0, len(pool))
	for _, f := range pool {
		if f.Itag != failedItag {
			rest = append(rest, f)
		}
	}
	_, alt := selectVodFormats(job, rest, dropped, segmented)
	return alt
}

// warnDamagedPins logs one Warn for each manual itag the job pinned that
// result records and that is damaged (youtube.Format.Damaged, under half the
// video's length): "Manual video itag N is possibly damaged (shorter than
// the video); recording it as pinned", or audio — the owner's decision on
// the picker's second follow-up. The picker marks such a rendition damaged
// and badges an intact one, but a pin of it is honoured, as every pin is:
// damage only ranks a rendition lower, the selector's rung, and a pin is the
// operator overriding the ranking (selectVodFormats). Upstream has no pin to
// honour — a -f of a damaged format downloads it, and the extraction's "some
// formats are possibly damaged" warning (_video.py:3436-3438, which the
// extraction's reportDamagedFormats ports) is the only word of it — so the
// line is Moombox's own, and a Warn for that warning's reason: the archive's
// track will stop partway, and nothing after the download says why.
//
// It reads the settled result, not a selection, which is what "recording it
// as pinned" can be said of: a selection's pick can still be replaced — the
// cipher re-selection (reselectVodWithout) re-runs the job's selection, pins
// and all, when a pick's signature fails, and a missing_pot degrade re-runs
// it over another pool — so a line logged per selection would be logged
// again by every re-run that found the same pin, and of a pick no download
// used. A pin is recorded when result's video or audio format is that itag
// of its kind: a pin the selection could not find (a damaged pin is always
// found, but a cipher failure can take it out of the re-selection's pool)
// is never the pick, since nothing else of that itag is in the pool; an
// audio pin with no video is recorded in the video's place; and an audio
// pin beside a muxed video is not recorded at all, its audio riding inside
// the video. The format read is the recorded copy, so the verdict is the
// copy's own, a missing_pot degrade's token-free shadow's included.
func warnDamagedPins(job *JobContext, result *DownloadResult) {
	for _, pin := range []struct {
		kind string
		itag *int
	}{{"video", job.Job.SelectedVideoItag}, {"audio", job.Job.SelectedAudioItag}} {
		if pin.itag == nil || *pin.itag <= 0 {
			continue
		}
		for _, f := range []*youtube.Format{result.VideoFormat, result.AudioFormat} {
			if f != nil && f.Itag == *pin.itag && strings.Contains(f.MimeType, pin.kind) {
				if f.Damaged {
					job.Logger.Warn(fmt.Sprintf("[FormatSelector] Manual %s itag %d is possibly damaged (shorter than the video); recording it as pinned", pin.kind, *pin.itag))
				}
				break
			}
		}
	}
}

// wholeFileVodPool splits formats into the ones a whole-file GET can fetch
// and the ones it cannot (youtube.Format.Segmented: an OTF or live adaptive
// format with no contentLength), whose URL answers with one fragment — the
// "bare media fragment" the whole-file path rejects, or, when that fragment
// is an sq=0-like init that starts with ftyp, a job that finishes over a
// nearly empty archive. yt-dlp's https pass skips both kinds
// (_video.py:3492-3507), and so does this pool. Where another client listed
// the same stream as a file, the extraction's dedup has already kept that
// copy instead (youtube's deduplicateFormats, the same skip); what is left
// here is a stream YouTube served segmented alone. Both slices are fresh.
func wholeFileVodPool(formats []youtube.Format) (pool, segmented []youtube.Format) {
	pool = make([]youtube.Format, 0, len(formats))
	for _, f := range formats {
		if f.URL != "" && f.Segmented() {
			segmented = append(segmented, f)
			continue
		}
		pool = append(pool, f)
	}
	return pool, segmented
}

// segmentedStreamLost is the clear failure for a VOD whose wanted video or
// audio YouTube served only segmented: the whole-file pool (wholeFileVodPool)
// has none of it, and selected — the job's own selection over that pool —
// came back without it. Leaving the formats out without this check would let
// a video job archive its audio alone, silently, where its OTF video used to
// fail as a fragment. DownloadVod holds its first selection to this, and a
// missing_pot degrade's too (segmented then also holds what the degrade's
// re-filters left out), and each call adds its own why: no DASH manifest came
// with the formats, or their whole-file copies need a token. A stream the
// job does not want — an audio_only job's video, a -1 manual itag — is never
// lost, and neither is audio a muxed (progressive) pick already carries. A
// loss-free pool that still left formats out says which, at Info. Each itag
// is named once: a degrade's re-filter can leave one out again.
func segmentedStreamLost(job *JobContext, selected youtube.SelectedFormats, segmented []youtube.Format) error {
	if len(segmented) == 0 {
		return nil
	}
	wantsVideo := job.Job.QualityPreference != "audio_only"
	if v := job.Job.SelectedVideoItag; v != nil {
		wantsVideo = *v != -1
	}
	wantsAudio := job.Job.SelectedAudioItag == nil || *job.Job.SelectedAudioItag != -1
	var lost []string
	if selected.Video == nil && wantsVideo && slices.ContainsFunc(segmented, func(f youtube.Format) bool { return strings.Contains(f.MimeType, "video") }) {
		lost = append(lost, "video")
	}
	if selected.Audio == nil && !IsProgressiveFormat(selected.Video) && wantsAudio && slices.ContainsFunc(segmented, func(f youtube.Format) bool { return f.IsAudio() }) {
		lost = append(lost, "audio")
	}
	itags := make([]string, 0, len(segmented))
	for _, f := range segmented {
		if s := strconv.Itoa(f.Itag); !slices.Contains(itags, s) {
			itags = append(itags, s)
		}
	}
	if len(lost) > 0 {
		return fmt.Errorf("VOD: YouTube serves this video's %s only as segments (itags %s: OTF or live adaptive, no contentLength), which a whole-file download cannot fetch",
			strings.Join(lost, " and "), strings.Join(itags, ", "))
	}
	job.Logger.Info("VOD: left out formats YouTube serves only as segments, which a whole-file download cannot fetch",
		"jobID", job.Job.ID, "itags", strings.Join(itags, ","))
	return nil
}

// withoutGvsRequiredFormats splits formats into the ones usable without a GVS
// PO token and the WEB-family ones whose token is mandatory
// (youtube.FormatGvsTokenMandatory, for a session isPremium says is or is not
// a Premium subscriber). A dropped format carrying a TokenFreeAlternate (the
// dedup shadow) contributes that alternate to kept instead — same itag, a URL
// that needs no token — and counts in swapped; it is still reported in
// dropped. A web-family format whose token is only recommended stays in kept
// as it is: itag 18, which upstream exempts from the requirement on every
// client (_video.py:3516-3518, audit G31) and saves bare when no token can be
// had, and every web-family format of a Premium session. Both slices are
// fresh: the caller's list is never filtered in place, and the alternate is
// copied out of its pointer.
func withoutGvsRequiredFormats(formats []youtube.Format, isPremium bool) (kept, dropped []youtube.Format, swapped int) {
	kept = make([]youtube.Format, 0, len(formats))
	for _, f := range formats {
		if !youtube.FormatGvsTokenMandatory(&f, isPremium) {
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

// formatItag is f's itag for a log line, or 0 for none.
func formatItag(f *youtube.Format) int {
	if f == nil {
		return 0
	}
	return f.Itag
}

// droppedHasItag reports whether a missing_pot drop removed a format with
// this itag of the given kind ("video" / "audio"), matched the way the manual
// override matches. Asked of the whole-file pool's segmented formats too.
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

// vodPinsLost names each manual itag the job pinned that first, a
// selection's result, records and pool no longer holds a copy of
// (poolHasStream) — "pinned video itag N", or audio — for a guessed
// binding's degrade, where such a pin has no token-free copy of its own and
// the guessed token is the way to record it (D12). A pin first did not
// record (not found, or an audio pin beside a muxed video) is not lost here:
// the degrade cannot lose what the selection never took.
func vodPinsLost(job *JobContext, first *DownloadResult, pool []youtube.Format) []string {
	var lost []string
	for _, pin := range []struct {
		kind string
		itag *int
	}{{"video", job.Job.SelectedVideoItag}, {"audio", job.Job.SelectedAudioItag}} {
		if pin.itag == nil || *pin.itag <= 0 {
			continue
		}
		for _, f := range []*youtube.Format{first.VideoFormat, first.AudioFormat} {
			if f != nil && f.Itag == *pin.itag && strings.Contains(f.MimeType, pin.kind) {
				if !poolHasStream(pool, f) {
					lost = append(lost, fmt.Sprintf("pinned %s itag %d", pin.kind, *pin.itag))
				}
				break
			}
		}
	}
	return lost
}

// vodStreamsLost names the streams — "video", "audio" — that first, a
// selection's result, records and degraded, the selection a missing_pot
// degrade re-ran, does not. A result records video when its video input is a
// video format, and audio when it is an audio-only primary (selectVodFormats
// files a lone audio stream as the video input), a progressive format's own
// track, or a separate audio format.
func vodStreamsLost(first, degraded *DownloadResult) []string {
	fv, fa := vodStreamKinds(first)
	dv, da := vodStreamKinds(degraded)
	var lost []string
	if fv && !dv {
		lost = append(lost, "video")
	}
	if fa && !da {
		lost = append(lost, "audio")
	}
	return lost
}

func vodStreamKinds(r *DownloadResult) (video, audio bool) {
	if f := r.VideoFormat; f != nil {
		if strings.Contains(f.MimeType, "video") {
			video, audio = true, IsProgressiveFormat(f)
		} else {
			audio = f.IsAudio()
		}
	}
	return video, audio || r.AudioFormat != nil
}
