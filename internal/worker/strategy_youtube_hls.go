package worker

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// DownloadHls sets up an HLS segment downloader.
// Fetches the HLS master playlist, selects the best variant based on
// max_video_resolution config, and passes the selected variant's playlist URL
// to the SegmentDownloader.
//
// routedSolver is the composite cipher.Solver (sidecar primary, goja fallback);
// cipherSolver is the legacy *GojaResolver, asked directly only when
// routedSolver is nil. They solve the `/n/`
// challenge in the master URL before fetching — yt-dlp does the
// same in extractor/youtube/_video.py:3684-3690. Without this, YouTube's CDN
// returns HTTP 403 on every segment for streams whose master URL ships with
// an encrypted n in its path (cb017549-family players).
//
// So a master URL whose n no solver answers is not fetched, as yt-dlp uses
// an HLS manifest only `if solved_n or not n_challenge` (_video.py:3692-3713,
// d20f58d72): it is passed over for the cookieless chain's master, which
// carries none (challengeFreeHlsInfo, audit G10), and fails the setup
// (cipher.ErrNUnsolved) only when there is none. It used to be fetched
// anyway, behind a Warn that downloads "may be throttled" (audit G9). An
// extraction that carries no master at all asks the same chain.
func DownloadHls(ctx context.Context, job *JobContext, videoInfo *youtube.VideoInfo, routedSolver cipher.Solver, cipherSolver *cipher.GojaResolver, potProvider *bgutils.PotProvider, isOnline func() bool) (*DownloadResult, error) {
	// The extraction's segment cadence, read before a cookieless chain's
	// extraction may replace it below: that one can carry no adaptive
	// formats (an HLS-only client), where this one does.
	extracted := videoInfo
	if videoInfo.HlsManifestURL == "" {
		// An extraction with no master is passed over for the cookieless
		// chain's, as a master whose n has no answer is below: the
		// challenge-free live fallback (setupWithFallback) records that
		// chain's master when the extraction it fell back from carried
		// none, and every later refresh of that capture lands here with a
		// fresh extraction that carries none either (refreshDownload routes
		// an HLS capture straight to this). Failing here made each stall,
		// quality or credential refresh of it fail where its setup had not.
		cf, cfErr := challengeFreeHlsInfo(ctx, job, videoInfo)
		if cfErr != nil {
			return nil, fmt.Errorf("no HLS manifest URL available (and no master that needs no player JS: %v)", cfErr)
		}
		videoInfo = cf
	}

	hlsURL := videoInfo.HlsManifestURL

	// Decrypt /n/<encrypted>/ in master URL before fetching. Mirrors
	// yt-dlp's HLS path: the master URL ships with an n value that the CDN
	// rejects on segments unless replaced with a player-JS-solved n. Master
	// playlist fetch may succeed with the encrypted n (YouTube only enforces
	// n on /videoplayback segment requests), but every segment returned by
	// the variant playlist will then 403. Asked whatever the player URL and
	// solvers: a master URL that carries no n (visionos's) comes back as it
	// is, and one with an n but no player or solver to answer it is as
	// unsolved as one the solvers failed. Only a player URL and a solver used
	// to let this run; without either, the n went out as sent.
	decrypted, err := cipher.RoutedDecryptNInURL(ctx, routedSolver, cipherSolver, videoInfo.PlayerURL, hlsURL)
	if err != nil {
		// A master whose n no solver answers is passed over for the next
		// client's, as yt-dlp passes it over (`if solved_n or not
		// n_challenge`): the cookieless chain's, visionos's first, whose
		// master carries none (challengeFreeHlsInfo, audit G10). The setup
		// fails as it did only when that has none either.
		cf, cfErr := challengeFreeHlsInfo(ctx, job, videoInfo)
		if cfErr != nil {
			return nil, fmt.Errorf("HLS: n challenge unsolved in the master playlist URL — its segments would be refused: %w (and no master that needs no player JS: %v)", err, cfErr)
		}
		videoInfo, hlsURL, decrypted = cf, cf.HlsManifestURL, cf.HlsManifestURL
	}
	if decrypted != hlsURL {
		hlsURL = decrypted
		job.Logger.Debug("[Cipher] decrypted n-param in HLS manifest URL")
	}

	// Generate PO token once and reuse for master URL, variant URL, and
	// segment URLs (audit reports/worker.md F30; same dedup as F29 for DASH).
	//
	// GVS PO token: bound per yt-dlp's get_webpo_content_binding rule,
	// resolved once at extraction (withAttestation) and carried on
	// videoInfo; minted via the cached /att/get minter (upstream provider
	// parity). See strategy_youtube_dash.go's mint block for the full
	// rationale and the 2026-08-16 activation note.
	//
	// Only a master playlist from a client whose URLs require the GVS token
	// takes one (youtube.GvsTokenRequired on the source the extraction
	// recorded) — the same rule as the DASH mint block. VISIONOS serves live
	// as HLS only, so its master is the common bare case here; a tv master
	// rides bare too, as upstream does, and so do the variant URL and the
	// downloader.
	//
	// HLS is never missing_pot: the web family's HLS policy is recommended,
	// not required (_base.py:85-88), so a master whose token none could be
	// minted for is fetched bare, as upstream fetches it — which is what
	// makes it the fallback a token-less manifest-free or DASH setup falls to
	// (missingPotFallback). A guessed binding counts as no token (D12): the
	// master goes bare rather than carry a token bound to a guess, as
	// upstream mints nothing for a signed-in session without a datasync ID.
	var hlsPoToken string
	hlsSource := videoInfo.HlsManifestSource
	if potProvider != nil && !youtube.GvsTokenRequired(hlsSource) {
		logGvsTokenSkipped(job, hlsSource)
	} else if potProvider != nil {
		bindingValue, bindingKind := gvsBinding(job, videoInfo)
		poToken, err := strategyGvsToken(ctx, potProvider, videoInfo, bindingValue, false)
		if err != nil {
			job.Logger.Warn("[POT] no GVS token — fetching the HLS manifest bare: its token is recommended, not required", "jobID", job.Job.ID,
				"binding", bindingKind, "source", hlsSource, "err", err)
		} else {
			hlsPoToken = poToken
			hlsURL = strings.TrimRight(hlsURL, "/") + "/pot/" + poToken
			job.Logger.Info("[POT] GVS mint", "jobID", job.Job.ID,
				"binding", bindingKind, "tokenLength", len(poToken), "itBytes", itBytesAttr(poToken, bindingValue), "source", hlsSource)
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
	qualityPref := job.Job.QualityPreference
	// HLS variants carry no itag, so a manual pin cannot be honoured here;
	// say so rather than record some other variant without a word.
	if pin := pinnedVideoItag(job); pin > 0 {
		job.Logger.Warn(fmt.Sprintf("[FormatSelector] Manual video itag %d cannot be honoured on an HLS stream (its variants carry no itag); selecting automatically", pin))
	}

	// Orchestrator-provided continuation position (restart discovery seeding
	// a fresh part dir). Without honoring it, a fresh dir + StartSeq -1
	// initializes at the playlist window start — which for DVR-enabled
	// YouTube live HLS reaches back to broadcast start, re-downloading hours
	// already archived in earlier parts. The DASH strategies honor the same
	// field; a resume sidecar (resuming into an unmuxed part) still takes
	// priority inside the engine. The audio rendition's downloader takes the
	// audio position the same way.
	startOf := func(seq int) (int, bool) {
		if seq > 0 {
			return seq, true
		}
		return -1, false
	}

	// newHlsDownloader builds the downloader of one media playlist of this
	// master — the picked variant, or the audio rendition beside it — with
	// the PO token, status check and 403 hook every one of them shares.
	// Reuses hlsPoToken from the master-URL step above; no need to mint
	// twice.
	newHlsDownloader := func(playlistURL, outputFile, stream string, startSeq int) *engine.SegmentDownloader {
		if hlsPoToken != "" {
			playlistURL = strings.TrimRight(playlistURL, "/") + "/pot/" + hlsPoToken
			what := "variant"
			if stream == "audio" {
				what = "audio rendition"
			}
			job.Logger.Info("[POT] added PO token to HLS "+what+" URL", "tokenLength", len(hlsPoToken))
		}
		seq, force := startOf(startSeq)
		d := newSegmentDownloader(engine.DownloaderOptions{
			BaseURL:             playlistURL,
			OutputFile:          outputFile,
			StartSeq:            seq,
			ForceStartSeq:       force,
			IsHls:               true,
			PoToken:             hlsPoToken,
			MaxTimeout:          time.Duration(job.Config.MaximumTimeout) * time.Second,
			SegmentWorkers:      job.Config.SegmentWorkers,
			InterruptionTimeout: engineInterruptionTimeout(job.Config.InterruptionTimeout),
			EnforceMaxTimeout:   true, // YouTube status can stick; Twitch HLS opts out
			IsOnline:            isOnline,
			Logger:              newScopedLogger(job.Logger, "jobID", job.Job.ID, "stream", stream),
			CheckStreamStatus: func(ctx context.Context) (bool, error) {
				info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
				return observeYouTubeStatusProbe(job, info, err)
			},
		})
		// Wire OnCipherFailure for HLS so a 403 burst invalidates POT /
		// visitor data / cipher caches. The playlist URL has POT in its path
		// so we can't hot-swap it mid-loop; return "" and let the next
		// orchestrator-driven refresh rebuild the strategy with fresh values.
		// Installed whenever a solver is, whatever the extraction's
		// PlayerURL, as the DASH hooks are: the POT and visitor-data halves
		// need no player (audit G2).
		if routedSolver != nil || cipherSolver != nil {
			tag := "HLS"
			if stream == "audio" {
				tag = "HLS audio"
			}
			d.OnCipherFailure = func() string {
				invalidate403Caches(job, videoInfo.PlayerURL, cipherSolver, potProvider, tag)
				return ""
			}
		}
		return d
	}

	result := &DownloadResult{
		IsHls: true,
		// An HLS variant maps to no player-response format, so no
		// VideoFormat says how long a segment runs; the finalize's join
		// point (owner decision CJ-3) needs it for an MPEG-TS that joined
		// late.
		SegmentDurationSec: advertisedSegmentDuration(extracted, videoInfo),
	}

	// audio_only on a demuxed master records the audio rendition alone —
	// yt-dlp's best audio format there — where it used to pick the lowest
	// bandwidth variant, whose segments on such a master hold no audio.
	if qualityPref == "audio_only" {
		if rendition := selectHlsAudioOnly(parsed, videoInfo.AudioLanguages); rendition != nil {
			job.Logger.Info("selected HLS audio rendition for an audio-only recording",
				"group", rendition.GroupID, "name", rendition.Name, "language", rendition.Language,
				"audioTrack", rendition.AudioContentID)
			result.HasAudio = true
			result.AdvertisedAudio = true
			result.AudioItag = hlsItagOf(rendition.URI)
			result.AudioPath = filepath.Join(job.StagingDir, hlsAudioFile)
			result.AudioDownloader = newHlsDownloader(rendition.URI, result.AudioPath, "audio", job.AudioStartSeq)
			noteAdvertisedAudio(result)
			if err := waitForPlayback(ctx, job, videoInfo, videoInfo.HlsManifestAvailableAt, videoInfo.HlsManifestSource, "HLS"); err != nil {
				return nil, fmt.Errorf("HLS: waiting out the pre-roll ad: %w", err)
			}
			return result, nil
		}
		job.Logger.Warn("audio_only preference with HLS: this master has no audio-only rendition, selecting the lowest bandwidth variant")
	}
	bestVariant := selectHlsVariant(parsed.Variants, qualityPref, job.Config.MaxVideoResolution, job.Config.Prefer60fps, job.Config.PreferHDR, videoInfo.AudioLanguages)
	if bestVariant == nil {
		return nil, fmt.Errorf("invalid HLS master playlist (no variants found)")
	}

	// A video-only variant (its AUDIO group's rendition has a URI of its
	// own) is recorded with that rendition beside it: the variant's CODECS
	// still names the audio codec, but its segments carry none.
	var audioRendition *engine.HlsRendition
	if bestVariant.VideoOnly {
		audioRendition = selectHlsAudioRendition(parsed.AudioRenditions(bestVariant.AudioGroup), videoInfo.AudioLanguages)
	}
	audioWhere := "in the variant"
	switch {
	case audioRendition != nil:
		audioWhere = "separate rendition (group " + audioRendition.GroupID + ")"
	case bestVariant.VideoOnly:
		audioWhere = "none: the variant is video-only and its audio group has no rendition to record"
	case !engine.CodecsNameAudio(bestVariant.Codecs):
		audioWhere = "none advertised"
	}
	job.Logger.Info("selected HLS variant",
		"width", bestVariant.Width, "height", bestVariant.Height,
		"bandwidth", bestVariant.Bandwidth, "codecs", bestVariant.Codecs,
		"audio", audioWhere, "audioTrack", bestVariant.AudioContentID)
	if bestVariant.VideoOnly && audioRendition == nil {
		job.Logger.Warn("the selected HLS variant is video-only and the master names no audio rendition to record beside it; the archive will have no sound",
			"jobID", job.Job.ID, "audioGroup", bestVariant.AudioGroup)
	}

	// Step 4: Store variant dimensions on the job
	if bestVariant.Width > 0 || bestVariant.Height > 0 {
		job.DB.UpdateJobFields(job.Job.ID, map[string]any{
			"video_width":  bestVariant.Width,
			"video_height": bestVariant.Height,
		})
	}

	// Step 5: Create the downloaders: the selected variant's playlist into
	// video.ts and, for a video-only variant, its audio rendition's into
	// audio.ts, muxed together at finalize.
	result.HasVideo = true
	result.VideoPath = filepath.Join(job.StagingDir, "video.ts")
	result.VideoWidth = bestVariant.Width
	result.VideoHeight = bestVariant.Height
	result.VideoFps = bestVariant.FPS
	result.AdvertisedAudio = audioRendition != nil || engine.CodecsNameAudio(bestVariant.Codecs)
	videoStart, audioStart := job.VideoStartSeq, job.AudioStartSeq
	if audioRendition != nil {
		var err error
		videoStart, audioStart, err = pairLoneHlsHalf(job, result.VideoPath, filepath.Join(job.StagingDir, hlsAudioFile), videoStart, audioStart)
		if err != nil {
			return nil, err
		}
		videoStart, audioStart = levelFreshHlsPair(job, result.VideoPath, filepath.Join(job.StagingDir, hlsAudioFile), videoStart, audioStart)
	}
	result.VideoDownloader = newHlsDownloader(bestVariant.URL, result.VideoPath, "video", videoStart)
	if audioRendition != nil {
		result.HasAudio = true
		result.AudioItag = hlsItagOf(audioRendition.URI)
		result.AudioPath = filepath.Join(job.StagingDir, hlsAudioFile)
		result.AudioDownloader = newHlsDownloader(audioRendition.URI, result.AudioPath, "audio", audioStart)
	}

	// Kept beside the staged files for a finalize that has no result (a
	// restart mux, the Mux action): the missing-audio guard reads it.
	noteAdvertisedAudio(result)

	// The master playlist is fetched; the variant playlist and its segments
	// wait out the pre-roll ad of the response that served the master
	// (waitForPlayback) — a finished stream's only, a live one never waits.
	if err := waitForPlayback(ctx, job, videoInfo, videoInfo.HlsManifestAvailableAt, videoInfo.HlsManifestSource, "HLS"); err != nil {
		return nil, fmt.Errorf("HLS: waiting out the pre-roll ad: %w", err)
	}

	return result, nil
}

// pairLoneHlsHalf is where the two downloaders of a demuxed HLS capture
// start when the job's setup finds its part dir holding one half and not
// the other: a video.ts with no audio.ts beside it, or the reverse. Without
// it each half took its own position — the staged one its resume sidecar's,
// the missing one the playlist window's start, up to an hour back — and the
// archive's sound ran minutes to an hour out of step with its picture, with
// an audio stream the missing-audio guard could not fault.
//
// A dir holding both halves of which only one can resume is the same skew
// (a crash between a half's first write and its first saved position): both
// are set aside together and start at the resumable one's position.
//
// Ways there: a job in flight under v2.9.8 to v2.9.10, which recorded a
// demuxed master's video-only variant alone; a crash before the second half
// wrote its first segment; a staged capture of a muxed master or an
// audio-only recording that is resumed as a demuxed one. In every case the
// lone half cannot be joined to a new one — the copy cannot delay an MPEG-TS
// input by more than a few seconds (alignHlsPair), and an older version's
// file has no recorded date to place it by — so it is set aside with its
// sidecar, to be muxed into its own file beside the archive at finalize as
// every aside is, and both halves start together where it stopped: its
// sidecar's next sequence, else the setup's seed, else the playlist window.
//
// Only the job's setup pairs (JobContext.PairStagedHalves): a refresh into
// the running part (the live loop's look at the stream) leaves the part's
// files alone, and a refresh that changed the shape splits the part instead
// (streamIdentityChanged).
func pairLoneHlsHalf(job *JobContext, videoPath, audioPath string, videoStart, audioStart int) (int, int, error) {
	if !job.PairStagedHalves {
		return videoStart, audioStart, nil
	}
	hasVideo, hasAudio := nonEmptyFile(videoPath), nonEmptyFile(audioPath)
	lg := newScopedLogger(job.Logger, "jobID", job.Job.ID)
	if hasVideo && hasAudio {
		// Both staged, but only one can resume (a crash between a half's
		// first write and its first saved position): the other would be
		// started over at the playlist window by the engine while this one
		// resumed. Both are set aside together and start at the resumable
		// one's position.
		vNext, vok := engine.StagedResumeNextSeq(videoPath)
		aNext, aok := engine.StagedResumeNextSeq(audioPath)
		if vok == aok {
			return videoStart, audioStart, nil
		}
		next := max(vNext, aNext)
		asides, err := setAsideStagedMedia(filepath.Dir(videoPath), []string{filepath.Base(videoPath), filepath.Base(audioPath)})
		if err != nil {
			lg.Error("could not set aside a staged HLS pair only one half of which can resume; refusing to record a new pair beside it",
				"dir", filepath.Dir(videoPath), "err", err)
			return 0, 0, fmt.Errorf("staging holds an HLS pair that could not be set aside: %w", err)
		}
		lg.Warn("only one half of a staged HLS pair can resume; the pair was set aside, to be muxed into its own file beside the archive, and the new video and audio start together where the resumable half stopped",
			"asides", strings.Join(asides, " | "), "startSeq", next)
		return next, next, nil
	}
	if !hasVideo && !hasAudio {
		return videoStart, audioStart, nil
	}
	lone := videoPath
	if hasAudio {
		lone = audioPath
	}
	next, resumable := engine.StagedResumeNextSeq(lone)
	if !resumable {
		next = max(videoStart, audioStart, 0)
	}
	asides, err := setAsideStagedMedia(filepath.Dir(lone), []string{filepath.Base(lone)})
	if err != nil {
		lg.Error("could not set aside a staged half of an HLS capture that has no other half to pair with; refusing to record a new pair beside it",
			"staged", lone, "err", err)
		return 0, 0, fmt.Errorf("staging holds one half of an HLS capture that could not be set aside: %w", err)
	}
	lg.Warn("a staged half of an HLS capture had no other half beside it; it was set aside, to be muxed into its own file beside the archive, and the new video and audio start together where it stopped",
		"asides", strings.Join(asides, " | "), "startSeq", next)
	return next, next, nil
}

// levelFreshHlsPair gives the two downloaders of a demuxed HLS capture one
// start when they open a fresh part (neither half staged) at two different
// positions: both start at the earlier one. The two halves do not advance
// together while they catch up (audio segments are smaller: a live run had
// its audio 900 s in when its video was 605 s in), so a part split
// mid-catch-up (carriedStartSeqs carries each half's own position) or a
// restart into a fresh part dir (seeded from last_video_seq and
// last_audio_seq, which may differ by up to 50 segments) would otherwise
// open a part whose halves begin minutes apart, further than the copy can
// place them (hlsPairMaxSkew). The leading half re-fetches the difference,
// which overlaps the part before as part overlap already allows; the
// lagging half loses nothing.
//
// A part with staged halves is left alone: each continues its own file
// (its resume sidecar, or an append at its own forced position), and moving
// one back would append segments the file already holds.
func levelFreshHlsPair(job *JobContext, videoPath, audioPath string, videoStart, audioStart int) (int, int) {
	if videoStart <= 0 || audioStart <= 0 || videoStart == audioStart {
		return videoStart, audioStart
	}
	if nonEmptyFile(videoPath) || nonEmptyFile(audioPath) {
		return videoStart, audioStart
	}
	start := min(videoStart, audioStart)
	job.Logger.Info("the video and audio of an HLS capture were to start a part at different segments; both start at the earlier one",
		"jobID", job.Job.ID, "videoStartSeq", videoStart, "audioStartSeq", audioStart, "startSeq", start)
	return start, start
}

// hlsAudioFile is the staged name of an HLS capture's separate audio
// rendition, beside the variant's video.ts (a demuxed master's: see
// engine.HlsVariant.VideoOnly). A capture of a muxed master writes video.ts
// alone, as it always has.
const hlsAudioFile = "audio.ts"

// hlsItagOf is the itag in a YouTube HLS playlist URL (/itag/N), 0 when it
// names none. An audio rendition's says which of the master's audio groups a
// capture records, so a refresh that picks another group is a new rendition
// (streamIdentityChanged), as a DASH audio itag is.
func hlsItagOf(u string) int {
	m := hlsItagRegex.FindStringSubmatch(u)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// hlsRenditionBetter reports whether a ranks above b as yt-dlp's format sort
// would rank two audio renditions of one master: the language first (the
// `lang` field, process_manifest_format's ORIGINAL/DEFAULT values from the
// rendition's YT-EXT-AUDIO-CONTENT-ID or LANGUAGE), then an alternate
// (DEFAULT=NO or AUTOSELECT=NO) below the rest (source_preference -2), then
// the group's rank (groupRank, 0 when not ranked), then DEFAULT=YES. A tie
// keeps the earlier one in the master.
func hlsRenditionBetter(a, b *engine.HlsRendition, langs youtube.AudioLanguages, groupRank map[string]int) bool {
	track := func(r *engine.HlsRendition) int {
		if r.AudioContentID != "" {
			return youtube.LanguageTrackScore(r.AudioContentID, langs)
		}
		return youtube.LanguageTrackScore(r.Language, langs)
	}
	if ta, tb := track(a), track(b); ta != tb {
		return ta > tb
	}
	if a.Alternate != b.Alternate {
		return !a.Alternate
	}
	if ga, gb := groupRank[a.GroupID], groupRank[b.GroupID]; ga != gb {
		return ga > gb
	}
	return a.Default && !b.Default
}

// selectHlsAudioRendition picks the rendition to record beside a video-only
// variant from its AUDIO group's renditions (engine.HlsParseResult.
// AudioRenditions): the DEFAULT one, unless the group holds several, which
// rank as yt-dlp ranks them (hlsRenditionBetter). nil for an empty group.
func selectHlsAudioRendition(group []*engine.HlsRendition, langs youtube.AudioLanguages) *engine.HlsRendition {
	var best *engine.HlsRendition
	for _, r := range group {
		if best == nil || hlsRenditionBetter(r, best, langs, nil) {
			best = r
		}
	}
	return best
}

// selectHlsAudioOnly picks the audio rendition an audio_only recording takes
// from a demuxed master, as yt-dlp picks its best audio there: the renditions
// of every group a video-only variant names, the groups ranked by the video
// they serve (_parse_m3u8_formats_and_subtitles' audio_groups_by_quality:
// each group placed where it first appears among those variants sorted by
// bandwidth then width, the later the better). nil when the master has no
// video-only variant — a muxed master, where the old lowest-bandwidth pick
// stands.
func selectHlsAudioOnly(parsed *engine.HlsParseResult, langs youtube.AudioLanguages) *engine.HlsRendition {
	var serving []*engine.HlsVariant
	for i := range parsed.Variants {
		if v := &parsed.Variants[i]; v.VideoOnly {
			serving = append(serving, v)
		}
	}
	if len(serving) == 0 {
		return nil
	}
	sort.SliceStable(serving, func(i, j int) bool {
		if serving[i].Bandwidth != serving[j].Bandwidth {
			return serving[i].Bandwidth < serving[j].Bandwidth
		}
		return serving[i].Width < serving[j].Width
	})
	groupRank := map[string]int{}
	for _, v := range serving {
		if _, seen := groupRank[v.AudioGroup]; !seen {
			groupRank[v.AudioGroup] = len(groupRank)
		}
	}
	var best *engine.HlsRendition
	for i := range parsed.Renditions {
		r := &parsed.Renditions[i]
		if _, ranked := groupRank[r.GroupID]; !ranked || r.Type != "AUDIO" || r.URI == "" {
			continue
		}
		if best == nil || hlsRenditionBetter(r, best, langs, groupRank) {
			best = r
		}
	}
	return best
}

// selectHlsVariant picks the variant to record from a YouTube live master
// playlist, given the job's quality preference and downloader.max_video_resolution.
//
// Ruling R1: the cap compares the SHORTER frame dimension and resolves to one
// size — the largest at or below it, or the closest above it when nothing is at
// or below. `0` is unbounded. Before this arc the site compared max(width,
// height) and returned a HARD ERROR ("no HLS variants found within resolution
// limit") when the filter emptied the list, which failed the whole download
// over a preference; and it substituted 9999 for a non-positive cap.
//
// The quality preference is matched against everything at or below the chosen
// size (so a per-job 720p under a 2160 cap still resolves to 720p); the
// no-preference path ranks only the variants AT the chosen size.
//
// Within a size the variants rank as every YouTube selector ranks (owner
// decision D14): the frame rate, then the shared rank — dynamic range under
// preferHDR, Premium (itags 616 and 235, as yt-dlp's process_manifest_format
// marks them, _video.py:3666-3668), codec — then the audio track, then
// bandwidth. The track is yt-dlp's `lang` rung for a pre-merged variant
// (extractor/common.py:2390-2392 reads YT-EXT-AUDIO-CONTENT-ID, _video.py:
// 3655-3663 ranks the original language at 10 and the default at 5): a
// dubbed VOD's master lists one variant per audio track at each size, alike
// on every other rung, and bandwidth alone could hand the archive a dub. The
// languages are the extraction's (VideoInfo.AudioLanguages), from every
// format entry, URL-less ones included, so a SABR-forced VOD whose only
// fetchable media is this master still knows its original.
//
// Returns nil only for an empty variant list — DownloadHls rejects a master
// playlist with no variants before calling this.
func selectHlsVariant(variants []engine.HlsVariant, qualityPref string, maxRes int, prefer60fps, preferHDR bool, langs youtube.AudioLanguages) *engine.HlsVariant {
	if len(variants) == 0 {
		return nil
	}

	cands := make([]utils.Cand, len(variants))
	for i := range variants {
		cands[i] = utils.Cand{Width: variants[i].Width, Height: variants[i].Height}
	}
	capSize, haveCapSize := utils.SelectByCap(maxRes, cands)

	var capped []*engine.HlsVariant
	for i := range variants {
		if haveCapSize && utils.CapDimension(variants[i].Width, variants[i].Height) > capSize {
			continue
		}
		capped = append(capped, &variants[i])
	}

	if qualityPref == "audio_only" {
		// Reached only for a master with no audio rendition of its own (a
		// muxed one: DownloadHls records a demuxed master's rendition alone,
		// selectHlsAudioOnly), which has no audio-only variant — take the
		// lowest bandwidth to minimise wasted video data (audio quality is
		// the same across variants). DownloadHls logs the warning. The audio track comes
		// first, though: on a dubbed VOD's master each track is pre-merged
		// into its own variant at every size, and an audio-only archive is
		// the one where the track is the whole point — the lowest bandwidth
		// alone could be a dub's. So the variants of the best track (the
		// same LanguageTrackScore the other paths rank by) compete on
		// bandwidth; with no language map every track scores alike and this
		// is the plain lowest bandwidth it always was.
		best := capped[0]
		bestTrack := youtube.LanguageTrackScore(best.AudioContentID, langs)
		for _, v := range capped[1:] {
			track := youtube.LanguageTrackScore(v.AudioContentID, langs)
			if track > bestTrack || (track == bestTrack && v.Bandwidth < best.Bandwidth) {
				best, bestTrack = v, track
			}
		}
		return best
	}

	if qualityPref != "" && qualityPref != "best" {
		targetHeight, targetFPS := ParseQualityPreference(qualityPref)
		if targetHeight > 0 {
			prefer := rankRule{fps: fpsPreference(targetFPS, prefer60fps), preferHDR: preferHDR}
			if v := selectHlsByHeight(capped, targetHeight, prefer, langs); v != nil {
				return v
			}
			if v := selectNextLowerHls(capped, targetHeight, prefer, langs); v != nil {
				return v
			}
			// No lower heights — fall through to source/best
		}
	}

	atSize := capped
	if haveCapSize {
		var exact []*engine.HlsVariant
		for _, v := range capped {
			if utils.CapDimension(v.Width, v.Height) == capSize {
				exact = append(exact, v)
			}
		}
		if len(exact) > 0 {
			atSize = exact
		}
	}
	return atSize[rankRenditions(atSize, hlsFields(langs), rankRule{fps: fpsPreference(0, prefer60fps), preferHDR: preferHDR})]
}

// pinnedVideoItag is the manual video itag the job asks for, the per-job
// selection over the config default, or 0 for none (-1, "no video", is not a
// pin of a rendition).
func pinnedVideoItag(job *JobContext) int {
	pin := job.Config.VideoItag
	if job.Job.SelectedVideoItag != nil {
		pin = *job.Job.SelectedVideoItag
	}
	return max(pin, 0)
}

// hlsItagRegex is yt-dlp's `/itag/(\d+)` read of a YouTube HLS variant URL
// (_video.py:3722-3723).
var hlsItagRegex = regexp.MustCompile(`/itag/(\d+)`)

// hlsPremiumItag reports whether a variant URL is one of the HLS itags
// yt-dlp marks Premium (616, 235; _video.py:3666-3668).
func hlsPremiumItag(variantURL string) bool {
	m := hlsItagRegex.FindStringSubmatch(variantURL)
	return m != nil && (m[1] == "616" || m[1] == "235")
}

// hlsFields measures a variant by its frame's shorter edge — see
// dashFieldAccessor — and ranks it: its CODECS (the video one of a
// pre-merged list), its Premium itag, and its audio track against langs.
func hlsFields(langs youtube.AudioLanguages) func(v *engine.HlsVariant) renditionFields {
	return func(v *engine.HlsVariant) renditionFields {
		return renditionFields{
			size:      utils.CapDimension(v.Width, v.Height),
			fps:       v.FPS,
			rank:      youtube.VideoRankOf(v.Codecs, hlsPremiumItag(v.URL)),
			track:     youtube.LanguageTrackScore(v.AudioContentID, langs),
			bandwidth: v.Bandwidth,
		}
	}
}

// selectHlsByHeight finds an HLS variant matching the target height, optionally with FPS.
// Thin wrapper around the generic selectAtHeightIdx (audit reports/worker.md F35).
func selectHlsByHeight(variants []*engine.HlsVariant, targetHeight int, prefer rankRule, langs youtube.AudioLanguages) *engine.HlsVariant {
	idx := selectAtHeightIdx(variants, hlsFields(langs), targetHeight, prefer)
	if idx < 0 {
		return nil
	}
	return variants[idx]
}

// selectNextLowerHls finds the HLS variant a preference picks at the next
// lower size below the target height, ranked by the rule prefer (frame rate,
// the shared rank) and then bandwidth. Thin wrapper around selectNextLowerIdx (audit reports/worker.md
// F36).
func selectNextLowerHls(variants []*engine.HlsVariant, targetHeight int, prefer rankRule, langs youtube.AudioLanguages) *engine.HlsVariant {
	idx := selectNextLowerIdx(variants, hlsFields(langs), targetHeight, prefer)
	if idx < 0 {
		return nil
	}
	return variants[idx]
}
