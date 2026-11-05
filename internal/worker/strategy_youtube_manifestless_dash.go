package worker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// HasManifestlessDashFormats reports whether videoInfo.Formats[] contains the
// split video + audio adaptive entries that make a manifest-free DASH
// (&sq=N-segmented) download viable. The signal is: at least one audio-only
// entry AND at least one video-only entry, both with non-empty URLs AND no
// contentLength. HLS variants are muxed (carry both video and audio in the same
// itag) so they don't pass; only true DASH-style adaptive splits do.
//
// The contentLength exclusion is critical. A format carrying a contentLength is
// a COMPLETE FILE (a regular VOD / premiere adaptive format) served whole over
// its URL — it is downloaded directly via byte-range, NEVER segment-addressed
// with &sq=N. Only open-ended live/OTF formats (no contentLength) are the
// manifest-free DASH segment case. This mirrors yt-dlp, which byte-range-chunks
// a contentLength-bearing format and only gives OTF/live formats &sq fragments.
// Feeding a whole-file URL to the &sq loop is catastrophic: YouTube ignores &sq
// and returns the ENTIRE file for every sequence number, so the segment loop
// re-downloads the whole file forever — no past-end 403 ever arrives to stop it
// (a premiere classified StreamPostLive filled the disk with hundreds of GB
// this way before this guard).
//
// Used by the orchestrator strategy switch as the primary-path routing
// signal: any live/post-live response shipping split adaptive URLs takes the
// manifest-free path regardless of whether dashManifestUrl is present
// (yt-dlp 8c1f07d81). Originally built to detect the narrower yt-dlp#15274
// experiment where YouTube withheld dashManifestUrl from cookied clients.
// The predicate itself lives in internal/youtube (HasSplitAdaptiveFormats)
// so the extraction cascade can consult it without importing this package;
// the rules above are the authoritative explanation of why it is shaped the
// way it is.
func HasManifestlessDashFormats(formats []youtube.Format) bool {
	return youtube.HasSplitAdaptiveFormats(formats)
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

// partitionManifestlessFormats splits adaptive formats into the video + audio
// pools SelectBestDashStream picks from. It excludes both URL-less formats and
// whole-file formats (ContentLength set) — the latter is critical: a complete
// file handed to the &sq segment loop returns the ENTIRE file for every
// sequence number, re-downloading it forever (the disk-filling runaway that
// HasManifestlessDashFormats guards against at the strategy gate). This MUST
// mirror that gate's exclusion: DashStreamInfo drops ContentLength, so
// SelectBestDashStream is contentLength-blind and would otherwise pick a
// higher-res whole-file itag as "best" from a MIXED pool (OTF video+audio that
// passes the gate PLUS a contentLength video) and feed its URL to the runaway.
// The video pool mirrors the gate's video test too (youtube.Format.IsVideoOnly):
// a muxed format (itag 18) listed without a contentLength is the progressive
// file, not a video half, and under a 360p cap it out-bandwidths itag 134.
func partitionManifestlessFormats(formats []youtube.Format) (video, audio []DashStreamInfo) {
	for i := range formats {
		f := &formats[i]
		if f.URL == "" || f.ContentLength != "" {
			continue
		}
		info := formatToDashStreamInfo(f)
		if f.IsAudio() {
			audio = append(audio, info)
		} else if f.IsVideoOnly() {
			video = append(video, info)
		}
	}
	return video, audio
}

// DownloadManifestlessDash sets up a manifest-free DASH download from the
// streamingData.adaptiveFormats[] direct URLs, fetched with `&sq=N`
// appended. The PRIMARY live/post-live path since yt-dlp 8c1f07d81 declared
// live DASH manifests "no longer properly supported" (2026-07); originally
// built for the yt-dlp#15274 experiment that stripped dashManifestUrl from
// cookied responses.
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
//  4. Mint a GVS POT bound per yt-dlp's get_webpo_content_binding rule
//     (gvsBinding, resolved at extraction and carried on videoInfo), via
//     the cached /att/get minter — only for a stream whose format came
//     from a client that requires the GVS token (youtube.GvsTokenRequired);
//     a tv, web_embedded, visionos or android_vr stream rides bare. A stream
//     whose token is mandatory and cannot be minted is never fetched bare:
//     the selection re-runs over the token-free pool, or the setup fails
//     with errMissingPot (manifestlessMissingPot; the mint block says how).
//  5. Build engine.SegmentDownloader instances. The downloader's
//     buildSegmentURL auto-detects query-style adaptive URLs and appends
//     `&sq=N`, while applyPoTokenQuery appends `&pot=POT` per fetch. No
//     separate init segment — sq=0 carries ftyp+moov inline, which is why a
//     start from sq=0 sets InitInlineAtStart and a forced later part sets
//     InitURL to sq=0 instead.
func DownloadManifestlessDash(
	ctx context.Context,
	job *JobContext,
	videoInfo *youtube.VideoInfo,
	routedSolver cipher.Solver,
	cipherSolver *cipher.GojaResolver,
	potProvider *bgutils.PotProvider,
	isOnline func() bool,
) (*DownloadResult, error) {
	// Partition videoInfo.Formats into video + audio adaptive pools (excludes
	// whole-file contentLength formats — see partitionManifestlessFormats).
	videoStreams, audioStreams := partitionManifestlessFormats(videoInfo.Formats)

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
		videoStream = SelectBestDashStream(videoStreams, videoItag, job.Config.MaxVideoResolution, true, job.Job.QualityPreference, job.Config.Prefer60fps)
	}
	if audioItag != -1 {
		audioStream = SelectBestDashStream(audioStreams, audioItag, 0, false, "", false)
	}
	warnUnhonouredPins(job, videoItag, videoStream, audioItag, audioStream)
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

	// Stage 3 of the cipher pipeline rework deferred sig+n decryption
	// from parse-time to here: parseFormats leaves Format.URL raw (with
	// EncryptedSig captured for sigCipher entries), and we resolve the
	// chosen video + audio URLs right before constructing the segment
	// downloader. Bound the re-selection to one fallback so a fully
	// broken cipher state surfaces as a clean setup error rather than
	// looping every itag.
	excludedVideoItags := map[int]bool{}
	excludedAudioItags := map[int]bool{}
	if videoStream != nil {
		resolved, retryStream, err := resolveManifestlessStream(ctx, videoInfo.Formats, videoStreams, videoStream, videoItag, job.Config.MaxVideoResolution, true, job.Job.QualityPreference, job.Config.Prefer60fps, routedSolver, cipherSolver, videoInfo.PlayerURL, excludedVideoItags, job.Logger)
		if err != nil {
			return nil, fmt.Errorf("manifestless DASH: resolve video URL: %w", err)
		}
		if retryStream != nil {
			videoStream = retryStream
		}
		videoStream.BaseURL = resolved
	}
	if audioStream != nil {
		resolved, retryStream, err := resolveManifestlessStream(ctx, videoInfo.Formats, audioStreams, audioStream, audioItag, 0, false, "", false, routedSolver, cipherSolver, videoInfo.PlayerURL, excludedAudioItags, job.Logger)
		if err != nil {
			return nil, fmt.Errorf("manifestless DASH: resolve audio URL: %w", err)
		}
		if retryStream != nil {
			audioStream = retryStream
		}
		audioStream.BaseURL = resolved
	}

	// GVS PO token: bound per yt-dlp's get_webpo_content_binding rule,
	// resolved once at extraction (withAttestation) and carried on
	// videoInfo; minted via the cached /att/get minter (upstream provider
	// parity). See strategy_youtube_dash.go's mint block for the full
	// rationale and the 2026-08-16 activation note.
	//
	// Resolved ONCE here — not re-derived per mint — so every subsequent
	// credential refresh (video, audio, 1st, 2nd, Nth) binds under the
	// same identity as this initial mint. gvsBinding reads the binding
	// stamped on videoInfo at extraction, which no cache invalidation
	// touches — but passing the resolved value keeps the refresh contract
	// (refreshGvsCredentials takes the caller's stable binding) uniform and
	// future-proof against a binding source that IS invalidated mid-job.
	//
	// No manifest here: each chosen format carries its own client, and video
	// and audio can come from different ones. Mint once if EITHER stream's
	// client requires the GVS token (youtube.GvsTokenRequired) and hand the
	// token only to the downloader whose own format does; a tv, web_embedded,
	// visionos or android_vr stream rides bare, as upstream does.
	// refreshGvsCredentials applies the same rule to the 403 re-mint, so a
	// bare stream stays bare.
	bindingValue, bindingKind := gvsBinding(job, videoInfo)
	var videoSource, audioSource string
	var videoNeedsPot, audioNeedsPot bool
	if videoStream != nil {
		videoSource = formatSourceByItag(videoInfo.Formats, videoStream.Itag)
		videoNeedsPot = youtube.GvsTokenRequired(videoSource)
	}
	if audioStream != nil {
		audioSource = formatSourceByItag(videoInfo.Formats, audioStream.Itag)
		audioNeedsPot = youtube.GvsTokenRequired(audioSource)
	}
	var pot string
	if potProvider != nil {
		if videoStream != nil && !videoNeedsPot {
			logGvsTokenSkipped(job, videoSource, "stream", "video")
		}
		if audioStream != nil && !audioNeedsPot {
			logGvsTokenSkipped(job, audioSource, "stream", "audio")
		}
	}
	//
	// missing_pot, as DownloadVod runs it (audit G16). A chosen stream whose
	// token is mandatory (youtube.FormatGvsTokenMandatory) and could not be
	// minted — the mint failed, came back empty, or the binding is a guess
	// (D12) — would 403 every segment bare, and spend the refresh budget
	// re-minting against the same failure. yt-dlp never records such a
	// format: it skips it before its stream id is recorded (_video.py:
	// 3440-3442, 3514-3555), so the next client's copy is the one it keeps.
	// Here the selection re-runs over the extraction's pool without the
	// formats that need the token, each replaced by its token-free shadow
	// where the dedup kept one (withoutGvsRequiredFormats) — tv's or a
	// cookieless copy of the same itag, or a lesser rendition of a token-free
	// client — and the capture records that (manifestlessMissingPot). With
	// nothing left to record the setup fails with errMissingPot, and a live
	// capture falls to its HLS manifest, whose token is only recommended
	// (missingPotFallback); a guessed binding instead rides its token on the
	// first selection, the one exit D12 leaves — there, and for a pinned
	// rendition the degrade dropped with no token-free copy of its own — and
	// only a mint that fails under the guess too degrades again, on that
	// failure, as any failed mint's does. A stream whose token is only
	// recommended — a Premium session's web family — rides bare, as upstream
	// fetches it.
	if potProvider != nil && (videoNeedsPot || audioNeedsPot) {
		poToken, err := strategyGvsToken(ctx, potProvider, videoInfo, bindingValue, false)
		mandatory := (videoStream != nil && youtube.FormatGvsTokenMandatory(formatByItag(videoInfo.Formats, videoStream.Itag), videoInfo.IsPremium)) ||
			(audioStream != nil && youtube.FormatGvsTokenMandatory(formatByItag(videoInfo.Formats, audioStream.Itag), videoInfo.IsPremium))
		switch {
		case err == nil:
			pot = poToken
			job.Logger.Info("[POT] GVS mint", "jobID", job.Job.ID,
				"binding", bindingKind, "tokenLength", len(poToken),
				"videoSource", videoSource, "audioSource", audioSource)
		case !mandatory:
			job.Logger.Warn("[POT] no GVS token — the web-family streams ride bare: Premium makes their token recommended, not required",
				"jobID", job.Job.ID, "binding", bindingKind, "err", err,
				"videoSource", videoSource, "audioSource", audioSource)
		default:
			deg, degErr := manifestlessMissingPot(ctx, job, videoInfo, videoItag, audioItag, routedSolver, cipherSolver, bindingKind, err)
			if degErr != nil && errors.Is(err, errGuessedGvsBinding) {
				// D12's last resort: a chosen stream — or a pinned rendition —
				// has no token-free copy to record, so the first selection
				// rides the token minted under the guessed binding.
				guessed, gErr := strategyGvsToken(ctx, potProvider, videoInfo, bindingValue, true)
				if gErr == nil {
					pot = guessed
					job.Logger.Warn("[POT] missing_pot: a chosen stream has no token-free copy — the token minted under the guessed binding rides the web-family streams",
						"jobID", job.Job.ID, "binding", bindingKind, "tokenLength", len(guessed),
						"videoSource", videoSource, "audioSource", audioSource, "err", degErr)
					break
				}
				// No token under the guess either: the degrade runs again on
				// the mint's own failure, as any failed mint's does — a dropped
				// pin falls to the token-free pick, and nothing left to record
				// is the missing_pot failure a live capture falls to HLS on.
				deg, degErr = manifestlessMissingPot(ctx, job, videoInfo, videoItag, audioItag, routedSolver, cipherSolver, bindingKind,
					fmt.Errorf("no token could be minted under the guessed binding either: %w", gErr))
			}
			if degErr != nil {
				return nil, degErr
			}
			videoInfo, videoStream, audioStream = deg.info, deg.video, deg.audio
			videoSource, audioSource = "", ""
			if videoStream != nil {
				videoSource = formatSourceByItag(videoInfo.Formats, videoStream.Itag)
			}
			if audioStream != nil {
				audioSource = formatSourceByItag(videoInfo.Formats, audioStream.Itag)
			}
			// The degraded pool holds no format a token is mandatory for,
			// and none was minted: nothing rides one.
			videoNeedsPot, audioNeedsPot = false, false
			job.Logger.Info("[POT] missing_pot: serving token-free formats",
				"jobID", job.Job.ID, "videoItag", itagOf(videoStream), "audioItag", itagOf(audioStream),
				"videoSource", videoSource, "audioSource", audioSource)
		}
	}
	videoPot, audioPot := "", ""
	if videoNeedsPot {
		videoPot = pot
	}
	if audioNeedsPot {
		audioPot = pot
	}

	result := &DownloadResult{}

	// Method value, not a snapshot — same reason as the DASH-manifest strategy:
	// the recording outlives the ~30-minute cookie rotation, and a header read
	// here would be sent unchanged to the final segment.
	var cookieHeader func() string
	if job.YT != nil {
		cookieHeader = job.YT.GetCookieHeader
	}

	// Orchestrator-provided start sequences (quality recovery / split path);
	// fall back to DB persisted last seq for crash recovery of a LIVE capture.
	// dbResumeSeq deliberately returns 0 for a finished post-live/VOD stream —
	// manifestless segments carry their ftyp+moov init only at sq=0, so a VOD
	// must start there; an interrupted VOD download resumes via the engine's
	// .resume.json sidecar, not the (possibly stale) job seq column.
	videoStartSeq := 0
	forceVideoSeq := false
	if job.VideoStartSeq > 0 {
		videoStartSeq = job.VideoStartSeq
		forceVideoSeq = true
	} else {
		videoStartSeq = dbResumeSeq(videoInfo.StreamStatus, job.Job.LastVideoSeq)
	}
	audioStartSeq := 0
	forceAudioSeq := false
	if job.AudioStartSeq > 0 {
		audioStartSeq = job.AudioStartSeq
		forceAudioSeq = true
	} else {
		audioStartSeq = dbResumeSeq(videoInfo.StreamStatus, job.Job.LastAudioSeq)
	}

	if videoStream != nil {
		result.HasVideo = true
		result.VideoPath = filepath.Join(job.StagingDir, "video_stream")
		result.VideoWidth = videoStream.Width
		result.VideoHeight = videoStream.Height
		result.VideoFps = videoStream.FPS
		result.VideoItag = videoStream.Itag
		// A part that force-starts mid-stream (quality split / restart) begins
		// at sq>0, but manifest-free DASH carries the ftyp+moov init only inline
		// at sq=0. Point InitURL at sq=0 so the engine prepends the extracted
		// init; otherwise the part file is a bare moof+mdat that won't mux.
		// Every other start begins at sq=0 itself (or appends to a file that
		// already holds it), and says so with InitInlineAtStart: the engine
		// then never lets an empty file begin past sq=0 without that init —
		// see engine.DownloaderOptions.InitInlineAtStart.
		videoInitURL := ""
		if forceVideoSeq && videoStartSeq > 0 {
			videoInitURL = manifestlessSq0URL(videoStream.BaseURL)
		}
		// Captured before NewSegmentDownloader — OnCredentialRefresh below is
		// built into the DownloaderOptions literal itself (unlike
		// OnCipherFailure, which is assigned after construction), so the
		// itag must be snapshotted here rather than at the later
		// OnCipherFailure site. Both closures share this one local so
		// neither can observe a later mutation of the videoStream pointer.
		videoItagChosen := videoStream.Itag
		result.VideoDownloader = newSegmentDownloader(engine.DownloaderOptions{
			BaseURL:             videoStream.BaseURL,
			OutputFile:          result.VideoPath,
			StartSeq:            videoStartSeq,
			ForceStartSeq:       forceVideoSeq,
			DiscardStaged:       manifestlessDiscardStaged(videoInfo.StreamStatus, forceVideoSeq),
			InitURL:             videoInitURL,
			InitFromSegment:     videoInitURL != "",
			InitInlineAtStart:   !forceVideoSeq,
			PoToken:             videoPot,
			CookieHeader:        cookieHeader,
			MaxTimeout:          time.Duration(job.Config.MaximumTimeout) * time.Second,
			SegmentWorkers:      job.Config.SegmentWorkers,
			InterruptionTimeout: engineInterruptionTimeout(job.Config.InterruptionTimeout),
			IsOnline:            isOnline,
			Logger:              newScopedLogger(job.Logger, "jobID", job.Job.ID, "stream", "video"),
			OnCredentialRefresh: func() (string, string) {
				return refreshGvsCredentials(ctx, job, videoInfo, videoItagChosen, routedSolver, cipherSolver, potProvider, bindingValue, "manifestless DASH video")
			},
			// Renewed before its URL expires while the stream is live
			// (liveURLRenewal, audit G48).
			URLExpiry: liveURLRenewal(videoInfo),
			OnProactiveRefresh: func() (string, string) {
				return renewGvsCredentials(ctx, job, videoInfo, videoItagChosen, routedSolver, cipherSolver, potProvider, bindingValue, "manifestless DASH video (renewal before expiry)", false)
			},
			CheckStreamStatus: func(ctx context.Context) (bool, error) {
				info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
				return observeYouTubeStatusProbe(job, info, err)
			},
		})
		// Same 403 invalidation chain as DASH-manifest path, plus a fresh-URL
		// re-resolve so the engine can swap BaseURL mid-download (mirrors
		// resolveFreshDashURL on the manifest path). The re-resolve runs the
		// chosen itag's ORIGINAL raw format URL through the freshly
		// invalidated solver — videoInfo.Formats keeps raw URLs; only the
		// DashStreamInfo copy was mutated with the resolved one.
		if videoInfo.PlayerURL != "" && (routedSolver != nil || cipherSolver != nil) {
			result.VideoDownloader.OnCipherFailure = func() string {
				invalidate403Caches(job, videoInfo.PlayerURL, cipherSolver, potProvider, "manifestless DASH video")
				fresh, err := resolveFormatURLByItag(ctx, videoInfo.Formats, videoItagChosen, routedSolver, cipherSolver, videoInfo.PlayerURL, job.Logger)
				if err != nil {
					job.Logger.Warn("manifestless DASH video: fresh URL re-resolve failed", "jobID", job.Job.ID, "err", err)
					return ""
				}
				job.Logger.Info("[Cipher] manifestless DASH video: fresh URL resolved; engine will swap BaseURL", "jobID", job.Job.ID)
				return fresh
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
		result.AudioItag = audioStream.Itag
		result.AudioPath = filepath.Join(job.StagingDir, "audio_stream")
		audioInitURL := ""
		if forceAudioSeq && audioStartSeq > 0 {
			audioInitURL = manifestlessSq0URL(audioStream.BaseURL)
		}
		// See the video block's identical comment: captured here (before the
		// DownloaderOptions literal) so OnCredentialRefresh and the later
		// OnCipherFailure share one immutable snapshot of the chosen itag.
		audioItagChosen := audioStream.Itag
		result.AudioDownloader = newSegmentDownloader(engine.DownloaderOptions{
			BaseURL:             audioStream.BaseURL,
			OutputFile:          result.AudioPath,
			StartSeq:            audioStartSeq,
			ForceStartSeq:       forceAudioSeq,
			DiscardStaged:       manifestlessDiscardStaged(videoInfo.StreamStatus, forceAudioSeq),
			InitURL:             audioInitURL,
			InitFromSegment:     audioInitURL != "",
			InitInlineAtStart:   !forceAudioSeq,
			PoToken:             audioPot,
			CookieHeader:        cookieHeader,
			MaxTimeout:          time.Duration(job.Config.MaximumTimeout) * time.Second,
			SegmentWorkers:      job.Config.SegmentWorkers,
			InterruptionTimeout: engineInterruptionTimeout(job.Config.InterruptionTimeout),
			IsOnline:            isOnline,
			Logger:              newScopedLogger(job.Logger, "jobID", job.Job.ID, "stream", "audio"),
			OnCredentialRefresh: func() (string, string) {
				return refreshGvsCredentials(ctx, job, videoInfo, audioItagChosen, routedSolver, cipherSolver, potProvider, bindingValue, "manifestless DASH audio")
			},
			// Renewed before its URL expires while the stream is live
			// (liveURLRenewal, audit G48).
			URLExpiry: liveURLRenewal(videoInfo),
			OnProactiveRefresh: func() (string, string) {
				return renewGvsCredentials(ctx, job, videoInfo, audioItagChosen, routedSolver, cipherSolver, potProvider, bindingValue, "manifestless DASH audio (renewal before expiry)", false)
			},
			CheckStreamStatus: func(ctx context.Context) (bool, error) {
				info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
				return observeYouTubeStatusProbe(job, info, err)
			},
		})
		if videoInfo.PlayerURL != "" && (routedSolver != nil || cipherSolver != nil) {
			result.AudioDownloader.OnCipherFailure = func() string {
				invalidate403Caches(job, videoInfo.PlayerURL, cipherSolver, potProvider, "manifestless DASH audio")
				fresh, err := resolveFormatURLByItag(ctx, videoInfo.Formats, audioItagChosen, routedSolver, cipherSolver, videoInfo.PlayerURL, job.Logger)
				if err != nil {
					job.Logger.Warn("manifestless DASH audio: fresh URL re-resolve failed", "jobID", job.Job.ID, "err", err)
					return ""
				}
				job.Logger.Info("[Cipher] manifestless DASH audio: fresh URL resolved; engine will swap BaseURL", "jobID", job.Job.ID)
				return fresh
			}
		}
	}

	// The first segment waits out the pre-roll ad of the response each chosen
	// format came from (waitForPlayback). The strategy serves live and
	// post-live streams, which never wait — upstream's is_live and post_live,
	// that is: a broadcast it calls was_live while its formats are still
	// segmented, as a refresh that finds it processed can hand one here,
	// would.
	var videoFormat, audioFormat *youtube.Format
	if videoStream != nil {
		videoFormat = formatByItag(videoInfo.Formats, videoStream.Itag)
	}
	if audioStream != nil {
		audioFormat = formatByItag(videoInfo.Formats, audioStream.Itag)
	}
	at, source := latestPlayback(videoFormat, audioFormat)
	if err := waitForPlayback(ctx, job, videoInfo, at, source, "manifestless DASH"); err != nil {
		return nil, fmt.Errorf("manifestless DASH: waiting out the pre-roll ad: %w", err)
	}

	return result, nil
}

// manifestlessDegrade is a manifest-free selection the missing_pot degrade
// re-ran: the streams it chose, with their URLs resolved, and info — a copy
// of the extraction's VideoInfo whose Formats is the degraded pool, so every
// later lookup by itag (the stream's source, its 403 refresh and cipher
// re-resolve, its pre-roll wait) reads the copy the stream records — a
// token-free shadow where one was swapped in — and never the winner it
// replaced.
type manifestlessDegrade struct {
	info         *youtube.VideoInfo
	video, audio *DashStreamInfo
}

// manifestlessMissingPot is DownloadManifestlessDash's missing_pot degrade:
// the selection over videoInfo's pool without the formats whose GVS token is
// mandatory (withoutGvsRequiredFormats, shadows swapped in), its URLs
// resolved as the first selection's were. A stream the job wants that the
// degraded pool cannot supply is an error wrapping errMissingPot and
// mintErr — the setup has nothing token-free to record. A pinned itag the
// degrade dropped says so, as the whole-file path's pins do — save under a
// guessed binding (mintErr wrapping errGuessedGvsBinding), where it is such
// an error too, for the caller to record the pin under the guessed token.
func manifestlessMissingPot(ctx context.Context, job *JobContext, videoInfo *youtube.VideoInfo, videoItag, audioItag int, routedSolver cipher.Solver, cipherSolver *cipher.GojaResolver, bindingKind string, mintErr error) (*manifestlessDegrade, error) {
	pool, dropped, swapped := withoutGvsRequiredFormats(videoInfo.Formats, videoInfo.IsPremium)
	job.Logger.Warn("[POT] missing_pot: no GVS token — dropping web-family formats",
		"jobID", job.Job.ID, "binding", bindingKind, "err", mintErr, "dropped", len(dropped), "swapped", swapped)
	videoStreams, audioStreams := partitionManifestlessFormats(pool)
	var video, audio *DashStreamInfo
	if videoItag != -1 {
		video = SelectBestDashStream(videoStreams, videoItag, job.Config.MaxVideoResolution, true, job.Job.QualityPreference, job.Config.Prefer60fps)
	}
	if audioItag != -1 {
		audio = SelectBestDashStream(audioStreams, audioItag, 0, false, "", false)
	}
	var lost []string
	if video == nil && videoItag != -1 {
		lost = append(lost, "video")
	}
	if audio == nil && audioItag != -1 {
		lost = append(lost, "audio")
	}
	if len(lost) > 0 {
		return nil, fmt.Errorf("manifestless DASH: %w: no %s adaptive format usable without a GVS PO token, and none could be minted: %w", errMissingPot, strings.Join(lost, " or "), mintErr)
	}
	// Under a guessed binding a pin the degrade dropped with no token-free
	// copy of its own is a stream nothing token-free stands in for: a token
	// can be had, under the guess, and D12 lets it ride exactly there — so
	// the caller's last resort records the pin with it, rather than another
	// itag with the Warn below, which is for a token none could be minted.
	if errors.Is(mintErr, errGuessedGvsBinding) {
		var pins []string
		if videoItag > 0 && video.Itag != videoItag && droppedHasItag(dropped, videoItag, "video") {
			pins = append(pins, fmt.Sprintf("pinned video itag %d", videoItag))
		}
		if audioItag > 0 && audio.Itag != audioItag && droppedHasItag(dropped, audioItag, "audio") {
			pins = append(pins, fmt.Sprintf("pinned audio itag %d", audioItag))
		}
		if len(pins) > 0 {
			return nil, fmt.Errorf("manifestless DASH: %w: the %s has no token-free copy: %w", errMissingPot, strings.Join(pins, " and "), mintErr)
		}
	}
	if videoItag > 0 && video.Itag != videoItag && droppedHasItag(dropped, videoItag, "video") {
		job.Logger.Warn(fmt.Sprintf("[FormatSelector] Manual video itag %d requires a GVS token none could be minted; recording itag %d instead", videoItag, video.Itag))
	} else {
		warnUnhonouredPins(job, videoItag, video, -1, nil)
	}
	if audioItag > 0 && audio.Itag != audioItag && droppedHasItag(dropped, audioItag, "audio") {
		job.Logger.Warn(fmt.Sprintf("[FormatSelector] Manual audio itag %d requires a GVS token none could be minted; recording itag %d instead", audioItag, audio.Itag))
	} else {
		warnUnhonouredPins(job, -1, nil, audioItag, audio)
	}
	if video != nil {
		resolved, retry, err := resolveManifestlessStream(ctx, pool, videoStreams, video, videoItag, job.Config.MaxVideoResolution, true, job.Job.QualityPreference, job.Config.Prefer60fps, routedSolver, cipherSolver, videoInfo.PlayerURL, map[int]bool{}, job.Logger)
		if err != nil {
			return nil, fmt.Errorf("manifestless DASH: resolve video URL: %w", err)
		}
		if retry != nil {
			video = retry
		}
		video.BaseURL = resolved
	}
	if audio != nil {
		resolved, retry, err := resolveManifestlessStream(ctx, pool, audioStreams, audio, audioItag, 0, false, "", false, routedSolver, cipherSolver, videoInfo.PlayerURL, map[int]bool{}, job.Logger)
		if err != nil {
			return nil, fmt.Errorf("manifestless DASH: resolve audio URL: %w", err)
		}
		if retry != nil {
			audio = retry
		}
		audio.BaseURL = resolved
	}
	info := *videoInfo
	info.Formats = pool
	return &manifestlessDegrade{info: &info, video: video, audio: audio}, nil
}

// dbResumeSeq returns the DB-persisted last sequence to resume a crashed LIVE
// manifestless capture from, or 0 when none applies. It returns 0 for any
// non-live stream (post-live / VOD): manifestless segments carry their
// ftyp+moov init only at sq=0, so a finished stream must start there rather
// than seed a stale live seq left over from an earlier live recording of the
// same job. Interrupted VOD downloads resume via the engine's .resume.json
// sidecar instead, which is keyed to the actual bytes on disk.
func dbResumeSeq(streamStatus youtube.StreamStatus, lastSeq *int) int {
	if streamStatus != youtube.StreamLive {
		return 0
	}
	if lastSeq != nil && *lastSeq > 0 {
		return *lastSeq
	}
	return 0
}

// manifestlessDiscardStaged reports whether this strategy's downloader needs a
// file that begins at the start of the stream. It is the mirror image of
// dbResumeSeq's rule: a non-live manifest-free capture deliberately begins
// again at sq=0 (segments carry their ftyp+moov init inline only there, so a
// partial file cannot be appended to), which means the engine's no-truncate
// guard must stand down for exactly that case. A LIVE capture resumes from
// the DB seq or the sidecar and must never discard; a part that force-starts
// at an orchestrator-provided seq owns its own fresh file.
//
// It is a pure predicate on purpose. Whether anything is actually thrown away
// is not a question this package can answer — only Start knows whether the
// resume sidecar is usable, and a usable one makes this flag irrelevant. The
// engine therefore owns the action: see engine.DownloaderOptions.DiscardStaged,
// which preserves a headed recording as <file>.restart-<ts> rather than
// truncating it (fix round 2, R1).
func manifestlessDiscardStaged(streamStatus youtube.StreamStatus, forcedStartSeq bool) bool {
	return streamStatus != youtube.StreamLive && !forcedStartSeq
}

// manifestlessSq0URL returns the sq=0 segment URL for a query-style manifest-
// free DASH base URL, used to fetch a part's inline ftyp+moov init. These
// adaptiveFormats URLs always carry query params, so &sq=0 is the correct
// separator (mirrors the engine's buildSegmentURL query-style form); the ?
// fallback is defensive.
func manifestlessSq0URL(base string) string {
	if strings.Contains(base, "?") {
		return base + "&sq=0"
	}
	return base + "?sq=0"
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
