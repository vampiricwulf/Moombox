package worker

import (
	"context"
	"errors"
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
// An n that is not solved (cipher.ErrNUnsolved), or a URL that carries none
// and so comes back as it went in, is "" too: an unchanged URL repairs
// nothing. Both used to log "succeeded" and hand the engine the URL it had —
// the unsolved n, after a second pass through a legacy goja path that ran the
// very GetSolvers + DecryptN RoutedDecryptNInURL's own goja fallback had just
// run, and that swallowed its own failure to solve (audit G9).
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
	decrypted, err := cipher.RoutedDecryptNInURL(ctx, routedSolver, cipherSolver, playerURL, origURL)
	switch {
	case err != nil:
		logger.Warn("[Cipher] retry: n challenge unsolved — no fresh URL to swap in", "jobID", jobID, "err", err)
		return ""
	case decrypted == origURL:
		logger.Warn("[Cipher] retry: the stream URL carries no n challenge to re-solve — no fresh URL to swap in", "jobID", jobID)
		return ""
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
// is every n solve's goja fallback, and is what the OnCipherFailure retry path
// invalidates.
//
// A manifest whose n challenge no solver answers fails the setup
// (cipher.ErrNUnsolved): its segment URLs would be refused, every one. yt-dlp
// uses a DASH manifest only `if solved_n or not n_challenge`
// (_video.py:3730-3751). It used to download them anyway, behind a Warn that
// downloads "may be throttled" (audit G9). So does a web-family manifest whose
// GVS token is mandatory and could not be minted (errMissingPot, the mint
// block below), which a live capture answers from its HLS manifest
// (missingPotFallback) and a VOD or post-live stream from its other formats
// (vodMissingPotFallback).
func DownloadDash(ctx context.Context, job *JobContext, videoInfo *youtube.VideoInfo, routedSolver cipher.Solver, cipherSolver *cipher.GojaResolver, potProvider *bgutils.PotProvider, isOnline func() bool) (*DownloadResult, error) {
	if videoInfo.DashManifestURL == "" {
		return nil, fmt.Errorf("no DASH manifest URL available")
	}

	dashURL := videoInfo.DashManifestURL

	// Step 1: Decrypt the DASH manifest URL itself (signature cipher).
	// Route through the composite solver (sidecar primary; goja fallback)
	// so sig decryption works on cb017549-family players where the goja
	// extractor cannot produce a sig algorithm. Asked whatever the player URL
	// and solvers: a URL that needs neither comes back as it is, and one whose
	// query n has no player or solver to answer it is as unsolved as one the
	// solvers failed (cipher.ErrNUnsolved).
	//
	// The path n (/n/<challenge>/, the form a live manifest URL carries, and
	// the one yt-dlp solves at this point: _video.py:3730-3741) is left as
	// YouTube sent it, on purpose. The manifest is fetched with it, its
	// representations echo it, and Step 4 solves each echo from the original
	// BaseURL it snapshots (DECISIONS #7). Solved here, the representations
	// would echo the answer, and Step 4 would run the n function over that
	// answer into a value no CDN accepts. So a path n no solver answers fails
	// the setup at Step 4, after the token mint and the manifest fetch, where
	// yt-dlp skips the manifest before either; the job ends the same way.
	resolved, err := cipher.RoutedResolveURL(ctx, routedSolver, cipherSolver, cipher.ResolveURLRequest{
		StreamURL: dashURL,
		PlayerURL: videoInfo.PlayerURL,
	})
	switch {
	case errors.Is(err, cipher.ErrNUnsolved):
		return nil, fmt.Errorf("DASH: n challenge unsolved in the manifest URL — its segments would be refused: %w", err)
	case err != nil:
		job.Logger.Warn("[Cipher] failed to decrypt DASH manifest URL", "err", err)
	case resolved.URL != dashURL:
		dashURL = resolved.URL
		job.Logger.Debug("[Cipher] decrypted DASH manifest URL")
	}

	// Step 2: Generate PO token once, used for both manifest URL injection and segment URLs.
	// Per audit reports/worker.md F29: previously generated twice (steps 2 and 5),
	// which was a cache hit but redundant work and confusing duplicate logging.
	//
	// GVS PO token: bound per yt-dlp's get_webpo_content_binding rule —
	// experiment flag → videoID, authenticated → datasyncID, otherwise
	// visitorData — resolved once at extraction (withAttestation) and carried
	// on videoInfo, so every strategy mints under the same identity. Minted
	// via the cached /att/get minter, the same sourcing
	// bgutil-ytdlp-pot-provider uses; the challenge-sourced fresh-per-mint
	// minters exceed upstream and stay dormant (see GenerateGvsPoToken). If
	// premieres 403 despite the yt-dlp-parity bindings, those minters are the
	// next variable to trial, and the provenance log line should then also
	// say whether a watch-page ytAtN challenge was present ("page"/"none").
	// Activated 2026-08-16 after the 10c2efd revert's suspect — the bundled
	// token-policy change — was exonerated (the stall reproduced on baseline;
	// root cause was the ANDROID_VR client ranking, fixed in e9d1388).
	//
	// Only a manifest from a client whose URLs require the GVS token takes
	// one (youtube.GvsTokenRequired on the source the extraction recorded —
	// the same rule the VOD path uses): a WEB-family manifest is tokenised;
	// a tv, web_embedded, visionos or android_vr manifest rides bare, as
	// upstream does — a token on an android_vr URL is the 2026-08-15 403
	// cause the AuthLevel block in internal/youtube/types.go names. Both
	// downloaders below follow.
	//
	// missing_pot: a web-family manifest whose token none could be minted
	// for is skipped, not fetched bare — upstream's `if require_po_token and
	// not po_token` skip (_video.py:3746-3749), which a bare fetch earns as a
	// 403 on every segment and a refresh budget spent re-minting against the
	// same failure (audit G16). The setup fails with errMissingPot, and a live
	// capture falls to its HLS manifest (missingPotFallback), a VOD or
	// post-live stream to its other formats (vodMissingPotFallback). A Premium
	// session's manifest is only recommended a token (GvsTokenMandatory): it
	// is fetched bare when none can be had, as upstream fetches it. A guessed
	// binding counts as no token (D12), and a manifest has no token-free copy
	// of its own to prefer, so a mandatory one takes the token minted under
	// the guess — the last resort D12 leaves it; a recommended one goes bare.
	var dashPoToken string
	var guessedPot bool // dashPoToken was minted under a guessed binding
	dashSource := videoInfo.DashManifestSource
	if potProvider != nil && !youtube.GvsTokenRequired(dashSource) {
		logGvsTokenSkipped(job, dashSource)
	} else if potProvider != nil {
		bindingValue, bindingKind := gvsBinding(job, videoInfo)
		mandatory := youtube.GvsTokenMandatory(dashSource, videoInfo.IsPremium)
		poToken, err := strategyGvsToken(ctx, potProvider, videoInfo, bindingValue, false)
		if mandatory && errors.Is(err, errGuessedGvsBinding) {
			job.Logger.Warn("[POT] missing_pot: the GVS content binding is a guess, and a DASH manifest has no token-free copy — minting under it",
				"jobID", job.Job.ID, "binding", bindingKind, "source", dashSource)
			poToken, err = strategyGvsToken(ctx, potProvider, videoInfo, bindingValue, true)
			guessedPot = err == nil
		}
		switch {
		case err == nil:
			dashPoToken = poToken
			dashURL = strings.TrimRight(dashURL, "/") + "/pot/" + poToken
			job.Logger.Info("[POT] GVS mint", "jobID", job.Job.ID,
				"binding", bindingKind, "tokenLength", len(poToken), "source", dashSource)
		case mandatory:
			job.Logger.Warn("[POT] missing_pot: no GVS token — skipping the web-family DASH manifest",
				"jobID", job.Job.ID, "binding", bindingKind, "source", dashSource, "err", err)
			return nil, fmt.Errorf("DASH: %w: the %s manifest needs a GVS PO token, and none could be minted: %w", errMissingPot, dashSource, err)
		default:
			job.Logger.Warn("[POT] no GVS token — fetching the manifest bare: Premium makes it recommended, not required",
				"jobID", job.Job.ID, "binding", bindingKind, "source", dashSource, "err", err)
		}
	}

	// Step 3: Fetch and parse DASH manifest
	manifestData, status, err := fetchURL(ctx, dashURL)
	if err != nil {
		return nil, fmt.Errorf("fetch DASH manifest: %w", err)
	}
	// A manifest refused (403) the token minted under a guessed binding was
	// refused for that token, most likely: it is bound to an identity the
	// session's GVS token is not shown to carry (D12). So it is no token at
	// all, and the setup fails as a manifest none could be minted for does
	// (errMissingPot), for the fallbacks that answer that — a live capture's
	// HLS manifest, whose token is only recommended (missingPotFallback; owner
	// decision, wave FC: "if refused, the missing_pot fallback drops to HLS"),
	// a VOD's other formats (vodMissingPotFallback). A refusal of a token
	// that is no guess is left to fail as it always did, at the parse.
	if guessedPot && status == http.StatusForbidden {
		job.Logger.Warn("[POT] missing_pot: the DASH manifest refused the token minted under the guessed binding",
			"jobID", job.Job.ID, "source", dashSource, "status", status)
		return nil, fmt.Errorf("DASH: %w: the %s manifest refused the GVS PO token minted under the guessed binding (HTTP %d): %w", errMissingPot, dashSource, status, errGuessedGvsBinding)
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

	// Step 4: Decrypt n-parameter in each stream's BaseURL (a segment URL
	// whose n is left as YouTube sent it is refused, 403).
	//
	// Snapshot the ORIGINAL (pre-decryption) BaseURL per itag BEFORE the
	// loop overwrites it — the OnCipherFailure handler below uses these
	// originals to re-decrypt with a freshly-solved cipher when YouTube
	// rotates mid-stream. Without the snapshot, the closure would re-decrypt
	// an already-decrypted URL and produce garbage. DECISIONS #7.
	//
	// Every BaseURL is asked, whatever the player URL and solvers, and the
	// first whose n is unsolved fails the setup: the representations echo
	// the one n the manifest was fetched with, so an n one of them cannot
	// have answered none of them can — yt-dlp skips the manifest whole. A
	// BaseURL that carries no n comes back as it is. Only a player URL and a
	// solver used to let this run; without either, every n went out as sent.
	originalBaseURLs := make(map[int]string, len(streams))
	for i := range streams {
		if streams[i].BaseURL != "" {
			originalBaseURLs[streams[i].Itag] = streams[i].BaseURL
		}
	}
	decryptCount := 0
	for i := range streams {
		// A manifest Initialization is resolved against its representation's
		// BaseURL (engine.ParseDash), so it carries that n too, and the
		// downloader fetches it as is: no 403 hook re-solves it, and a part
		// whose init is refused does not start (ErrInitSegmentUnavailable).
		// It gets its n solved like the BaseURL, and fails the setup the
		// same way when that n is unsolved. It used to go out with its n as
		// YouTube sent it. YouTube's live manifests carry no Initialization
		// today (a Period-level SegmentList; moonarchive reads nothing else,
		// downloaders/youtube/dash_manifest.py), so this is for the one that
		// does.
		if streams[i].Initialization != "" {
			init, err := cipher.RoutedDecryptNInURL(ctx, routedSolver, cipherSolver, videoInfo.PlayerURL, streams[i].Initialization)
			if err != nil {
				return nil, fmt.Errorf("DASH: n challenge unsolved in the initialization URL for itag %d — its init segment would be refused: %w", streams[i].Itag, err)
			}
			streams[i].Initialization = init
		}
		if streams[i].BaseURL == "" {
			continue
		}
		decrypted, err := cipher.RoutedDecryptNInURL(ctx, routedSolver, cipherSolver, videoInfo.PlayerURL, streams[i].BaseURL)
		if err != nil {
			return nil, fmt.Errorf("DASH: n challenge unsolved for itag %d — the manifest's segments would be refused: %w", streams[i].Itag, err)
		}
		if decrypted != streams[i].BaseURL {
			streams[i].BaseURL = decrypted
			decryptCount++
		}
	}
	job.Logger.Debug("[Cipher] DASH n-param decryption complete", "decrypted", decryptCount, "total", len(streams))

	// dashPoToken from step 2 is reused for segment URLs via DownloaderOptions.

	// Convert to DashStreamInfo for selection
	streamInfos := manifestStreamInfos(streams, videoInfo.AudioLanguages, videoInfo.Formats)

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
		videoStream = SelectBestDashStream(streamInfos, videoItag, job.Config.MaxVideoResolution, true, job.Job.QualityPreference, job.Config.Prefer60fps, job.Config.PreferHDR)
	}
	if audioItag != -1 {
		audioStream = SelectBestDashStream(streamInfos, audioItag, 0, false, "", false, false)
	}
	warnUnhonouredPins(job, videoItag, videoStream, audioItag, audioStream)
	warnDamagedManifestPins(job, videoInfo, videoStream, audioStream)

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
		result.VideoItag = videoStream.Itag
	}

	// Get cookie header for authenticated downloads. Method value, not a
	// snapshot: a DASH capture runs for the length of the broadcast while the
	// in-process refresh rotates the jar every ~30 minutes, so a header read
	// here would still be presented to the last segment hours later.
	var dashCookieHeader func() string
	if job.YT != nil {
		dashCookieHeader = job.YT.GetCookieHeader
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
		result.VideoDownloader = newSegmentDownloader(engine.DownloaderOptions{
			BaseURL:             videoStream.BaseURL,
			OutputFile:          result.VideoPath,
			StartSeq:            videoStartSeq,
			ForceStartSeq:       forceVideoSeq,
			InitURL:             videoStream.Initialization,
			PoToken:             dashPoToken,
			CookieHeader:        dashCookieHeader,
			MaxTimeout:          time.Duration(job.Config.MaximumTimeout) * time.Second,
			SegmentWorkers:      job.Config.SegmentWorkers,
			InterruptionTimeout: engineInterruptionTimeout(job.Config.InterruptionTimeout),
			IsOnline:            isOnline,
			Logger:              newScopedLogger(job.Logger, "jobID", job.Job.ID, "stream", "video"),
			CheckStreamStatus: func(ctx context.Context) (bool, error) {
				info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
				return observeYouTubeStatusProbe(job, info, err)
			},
		})
		// Installed whenever a solver is, as refreshGvsCredentials' URL half
		// is gated — not on the extraction's PlayerURL (audit G2). Without
		// one there is no player to re-solve with and no fresh response here
		// to take one from, so the hook does the player-free half alone: the
		// POT and visitor data are dropped, and its "" sends the engine down
		// its refresh path, whose re-extraction resolves a player afresh
		// (resolvePlayerURL). Gated on the PlayerURL, such a job's first 403
		// invalidated nothing at all.
		if routedSolver != nil || cipherSolver != nil {
			origURL := originalBaseURLs[videoStream.Itag]
			result.VideoDownloader.OnCipherFailure = func() string {
				invalidate403Caches(job, videoInfo.PlayerURL, cipherSolver, potProvider, "DASH video")
				if videoInfo.PlayerURL == "" {
					return ""
				}
				return resolveFreshDashURL(ctx, job.Job.ID, routedSolver, cipherSolver, videoInfo.PlayerURL, origURL, job.Logger)
			}
		}
	}

	if audioStream != nil {
		result.HasAudio = true
		result.AudioItag = audioStream.Itag
		result.AudioPath = filepath.Join(job.StagingDir, "audio_stream")
		result.AudioDownloader = newSegmentDownloader(engine.DownloaderOptions{
			BaseURL:             audioStream.BaseURL,
			OutputFile:          result.AudioPath,
			StartSeq:            audioStartSeq,
			ForceStartSeq:       forceAudioSeq,
			InitURL:             audioStream.Initialization,
			PoToken:             dashPoToken,
			CookieHeader:        dashCookieHeader,
			MaxTimeout:          time.Duration(job.Config.MaximumTimeout) * time.Second,
			SegmentWorkers:      job.Config.SegmentWorkers,
			InterruptionTimeout: engineInterruptionTimeout(job.Config.InterruptionTimeout),
			IsOnline:            isOnline,
			Logger:              newScopedLogger(job.Logger, "jobID", job.Job.ID, "stream", "audio"),
			CheckStreamStatus: func(ctx context.Context) (bool, error) {
				info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
				return observeYouTubeStatusProbe(job, info, err)
			},
		})
		// Gated as the video hook is; see there.
		if routedSolver != nil || cipherSolver != nil {
			origURL := originalBaseURLs[audioStream.Itag]
			result.AudioDownloader.OnCipherFailure = func() string {
				invalidate403Caches(job, videoInfo.PlayerURL, cipherSolver, potProvider, "DASH audio")
				if videoInfo.PlayerURL == "" {
					return ""
				}
				return resolveFreshDashURL(ctx, job.Job.ID, routedSolver, cipherSolver, videoInfo.PlayerURL, origURL, job.Logger)
			}
		}
	}

	// The manifest is fetched; its segments wait out the pre-roll ad of the
	// response that served it (waitForPlayback) — a finished stream's only,
	// a live one never waits.
	if err := waitForPlayback(ctx, job, videoInfo, videoInfo.DashManifestAvailableAt, videoInfo.DashManifestSource, "DASH"); err != nil {
		return nil, fmt.Errorf("DASH: waiting out the pre-roll ad: %w", err)
	}

	return result, nil
}

// warnDamagedManifestPins is warnDamagedPins for the DASH-manifest path: one
// Warn for each manual itag the job pinned (Job.SelectedVideoItag,
// SelectedAudioItag — the operator's pin, not the config default) that the
// manifest honoured, video or audio — the selection's stream is that itag —
// and whose copy in videoInfo.Formats is damaged (youtube.Format.Damaged,
// under half the video's length): "Manual video itag N is possibly damaged
// (shorter than the video); recording it as pinned". The manifest's
// Representation carries no length to judge, so the verdict is read off the
// player response's own copy of the itag, of the pin's kind, which the
// extraction marked. The owner's decision on the picker's second follow-up
// was not limited to the whole-file path (PK2's leftover, the integrator's
// call), and this path honours a damaged pin as that one does; upstream has
// no pin to honour (warnDamagedPins). Logged once per job run (JobContext.Run):
// every mid-stream refresh sets the strategy up again (refreshDownload), and
// a line per refresh would repeat what the first one said.
func warnDamagedManifestPins(job *JobContext, videoInfo *youtube.VideoInfo, video, audio *DashStreamInfo) {
	for _, pin := range []struct {
		kind   string
		itag   *int
		stream *DashStreamInfo
	}{{"video", job.Job.SelectedVideoItag, video}, {"audio", job.Job.SelectedAudioItag, audio}} {
		if pin.itag == nil || *pin.itag <= 0 || pin.stream == nil || pin.stream.Itag != *pin.itag {
			continue
		}
		for i := range videoInfo.Formats {
			f := &videoInfo.Formats[i]
			if f.Itag != *pin.itag || !strings.Contains(f.MimeType, pin.kind) {
				continue
			}
			if f.Damaged && job.Run.first(fmt.Sprintf("damaged-pin:%s:%d", pin.kind, *pin.itag)) {
				job.Logger.Warn(fmt.Sprintf("[FormatSelector] Manual %s itag %d is possibly damaged (shorter than the video); recording it as pinned", pin.kind, *pin.itag))
			}
			break
		}
	}
}

// manifestStreamInfos converts a parsed DASH manifest's streams into the
// DashStreamInfo shape SelectBestDashStream ranks — for the DASH-manifest
// download (DownloadDash) and the quality probe's manifest branch alike, so
// the probe ranks exactly what the download would. The conversion used to
// drop the Representation's codecs, leaving the selector nothing to rank but
// bandwidth (G37); it carries them now, and an audio stream's rank: its
// codec score, and its track — the Representation's lang against the
// extraction's original and default languages (youtube.LanguageTrackScore),
// yt-dlp's language_preference for a DASH-manifest format (_video.py:
// 3655-3663).
//
// A manifest states no audioQuality and no channel count, so both come from
// the extraction's own copy of the same itag (formats, VideoInfo.Formats):
// the quality as yt-dlp's process_manifest_format fills a manifest format's
// from the https formats of its itag (`f['quality'] = q(itag_qualities…)`,
// _video.py:3670, filled at 3409-3411), and the channels likewise — there
// Moombox goes beyond upstream, whose MPD parse reads no
// AudioChannelConfiguration and so ties every manifest track on channels:
// owner decision D14 ranks this path like VOD, and the VOD path ranks a 5.1
// rendition above stereo at one quality (D15), which a tie would hand back
// to the codec rung (stereo Opus over E-AC-3 5.1). An itag's channel count
// is a property of the itag, so the https copy states the manifest's. With
// no copy of the itag the rungs stay 0 and tie, as upstream's do.
func manifestStreamInfos(streams []engine.DashStream, langs youtube.AudioLanguages, formats []youtube.Format) []DashStreamInfo {
	type facts struct{ quality, channels int }
	byItag := make(map[int]facts)
	for i := range formats {
		f := &formats[i]
		if !strings.Contains(f.MimeType, "audio") {
			continue
		}
		have := byItag[f.Itag]
		if have.quality == 0 {
			have.quality = youtube.AudioQualityRung(f.AudioQuality)
		}
		if have.channels == 0 {
			have.channels = f.AudioChannels
		}
		byItag[f.Itag] = have
	}
	infos := make([]DashStreamInfo, 0, len(streams))
	for _, s := range streams {
		info := DashStreamInfo{
			Itag:           s.Itag,
			MimeType:       s.MimeType,
			Width:          s.Width,
			Height:         s.Height,
			FPS:            s.FPS,
			Bandwidth:      s.Bandwidth,
			BaseURL:        s.BaseURL,
			Initialization: s.Initialization,
			Codecs:         s.Codecs,
			Lang:           s.Lang,
		}
		if strings.Contains(s.MimeType, "audio") {
			fill := byItag[s.Itag]
			info.Audio = youtube.AudioRank{
				Track:    youtube.LanguageTrackScore(s.Lang, langs),
				Quality:  fill.quality,
				Channels: fill.channels,
				Codec:    youtube.ScoreAudioCodec(s.Codecs),
			}
		}
		infos = append(infos, info)
	}
	return infos
}
