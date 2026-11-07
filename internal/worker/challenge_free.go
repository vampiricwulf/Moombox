package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// errChallengesUnsolvable marks a format URL resolve that failed for a reason
// every format needing the player JS shares (systemicResolveFailure), and so
// a setup that can go on only with formats that need none. The VOD path
// drops to those (DownloadVod), and a live capture records the live HLS
// playlist instead (setupWithFallback). It wraps the resolve's
// own error, which says which reason it was.
var errChallengesUnsolvable = errors.New("the player's JS challenges cannot be solved")

// systemicResolveFailure reports whether err, the failure to resolve f's URL
// under playerURL, is one every format needing the player JS shares — the
// failure audit G10 found Moombox answering one itag at a time:
//
//   - cipher.ErrSidecarUnavailable: no sidecar configured, or the one
//     configured is down. Signatures are sidecar-only, so no signatureCipher
//     format resolves until it is back. A child that went down is waited for
//     first, within a minute of the outage's start (the composite solver's
//     sidecarReadyGrace, owner decision PR-R3), so a supervisor restart does
//     not reach here.
//   - cipher.ErrPlayerJSStale: ejs could not solve the player even after a
//     fresh fetch of it — the case of a player rotation ejs does not handle
//     yet.
//   - cipher.ErrPlayerJSFetch: the player could not be fetched, with no
//     cached copy to serve instead, in three tries (PlayerCache.Fetch,
//     playerFetchAttempts, owner decision PR-R3). Nothing solves a
//     challenge against a player it does not have; yt-dlp's challenge
//     director catches a provider that fails on a player it cannot load and
//     leaves its challenges unanswered (jsc/_director.py:95-140), so every
//     format needing one is skipped behind "Some formats may be missing"
//     (_video.py:3303-3359, 3557-3580) and visionos's copy is the one it
//     keeps. An n surfaces this as cipher.ErrNUnsolved already; a signature
//     surfaces it as itself (the sidecar's "player JS for <id>").
//   - cipher.ErrNUnsolved: no solver answered the n challenge. Every format
//     of a player shares its n function.
//   - no player URL: there is nothing to solve any challenge with.
//
// A format that needs no player JS (youtube.Format.NeedsPlayerJS false)
// cannot fail on any of these, so its failure is never systemic. yt-dlp
// skips each format whose challenge has no answer before it records the
// stream (_video.py:3509-3511, 3557-3580, 3587-3589), so the next client's
// copy wins — visionos's, which needs no JS. Moombox dedups first, so the
// strategies run that skip themselves once the selection's own format has
// failed this way.
func systemicResolveFailure(err error, f *youtube.Format, playerURL string) bool {
	if err == nil || f == nil || !f.NeedsPlayerJS() {
		return false
	}
	return playerURL == "" ||
		errors.Is(err, cipher.ErrSidecarUnavailable) ||
		errors.Is(err, cipher.ErrPlayerJSStale) ||
		errors.Is(err, cipher.ErrPlayerJSFetch) ||
		errors.Is(err, cipher.ErrNUnsolved)
}

// markChallengeFailure wraps err in errChallengesUnsolvable when it is a
// systemic resolve failure of f (systemicResolveFailure), once, and hands it
// back as it came otherwise.
func markChallengeFailure(err error, f *youtube.Format, playerURL string) error {
	if !systemicResolveFailure(err, f, playerURL) || errors.Is(err, errChallengesUnsolvable) {
		return err
	}
	return fmt.Errorf("%w: %w", errChallengesUnsolvable, err)
}

// setupFailedOnChallenges reports whether a strategy's setup error is a
// systemic failure to solve the player's JS challenges: a format resolve the
// strategy marked (errChallengesUnsolvable), or a manifest whose n no solver
// answered — DownloadDash and DownloadHls fail their setup on that
// (cipher.ErrNUnsolved) rather than fetch segments the CDN refuses — or one
// of the cipher's other systemic sentinels however it surfaced. What the
// orchestrator's challenge-free fallback acts on.
func setupFailedOnChallenges(err error) bool {
	return errors.Is(err, errChallengesUnsolvable) ||
		errors.Is(err, cipher.ErrNUnsolved) ||
		errors.Is(err, cipher.ErrSidecarUnavailable) ||
		errors.Is(err, cipher.ErrPlayerJSStale) ||
		errors.Is(err, cipher.ErrPlayerJSFetch)
}

// withoutChallengeFormats returns the copies in formats whose URL needs no
// player JS (youtube.Format.NeedsPlayerJS false), each read with its
// shadows: a winner that needs JS can carry a token-class shadow that does
// not — a web-family winner's visionos TokenFreeAlternate — and that copy is
// kept in its place, as the missing_pot drop swaps one in. Every copy kept
// is copied out with no shadow of its own (the caller re-dedups, which
// computes them afresh), and dropped counts the entries that needed JS. A
// fresh slice: formats is never filtered in place.
func withoutChallengeFormats(formats []youtube.Format) (kept []youtube.Format, dropped int) {
	kept = make([]youtube.Format, 0, len(formats))
	for i := range formats {
		if formats[i].NeedsPlayerJS() {
			dropped++
		}
		for _, c := range []*youtube.Format{&formats[i], formats[i].TokenFreeAlternate, formats[i].TokenAlternate} {
			if c == nil || c.URL == "" || c.NeedsPlayerJS() {
				continue
			}
			k := *c
			k.TokenFreeAlternate, k.TokenAlternate = nil, nil
			kept = append(kept, k)
		}
	}
	return kept, dropped
}

// cookielessReExtract is one strategy setup's cookieless re-extract
// (fetchCookielessFormats, visionos then android_vr), fetched at most once
// and shared by the setup's degrades: the missing_pot one and the
// challenge-free one each want the cookieless chain's copies, and a setup
// that runs both asks YouTube once. Bounded like the missing_pot fetch always
// was (credentialRefreshTimeoutFor). Not safe for concurrent use: a setup
// runs on one goroutine.
type cookielessReExtract struct {
	done    bool
	formats []youtube.Format
	err     error
}

// fetch runs the re-extract the first time it is asked and hands back that
// answer every time.
func (rx *cookielessReExtract) fetch(ctx context.Context, job *JobContext) ([]youtube.Format, error) {
	if !rx.done {
		rx.done = true
		rxCtx, cancel := context.WithTimeout(ctx, credentialRefreshTimeoutFor(job.Config))
		rx.formats, rx.err = fetchCookielessFormats(job.YT, rxCtx, job.Job.VideoID)
		cancel()
	}
	return rx.formats, rx.err
}

// challengeFreeFormats is the format list a setup goes on with once its
// selection's own format has failed systemically (systemicResolveFailure;
// cause is that failure): the extraction's formats with every copy that
// needs the player JS dropped (withoutChallengeFormats), plus the cookieless
// chain's copies that need none (rx), merged and deduplicated
// (youtube.MergeFormatPool). Owner decision 3 of the yt-dlp parity plan:
// re-extract and re-select, as the missing_pot degrade does, rather than a
// third dedup shadow.
//
// The re-extract runs whenever it can — a YouTube service to ask, a job not
// already cancelled — not only for a stream the drop lost, as the missing_pot
// one does: the dedup keeps a copy of each stream from the other GVS token
// class only, so a tv or web_embedded winner, the usual signatureCipher and
// n carrier, keeps no visionos copy, and the extraction's pool holds a copy
// that needs no JS only of a stream a cookieless client won — and the
// public cascade asks those clients only when TV was inadequate, the
// authenticated one only when web_creator was. A failed re-extract leaves
// what the drop kept, with a Warn. tag names the strategy in the log.
func challengeFreeFormats(ctx context.Context, job *JobContext, formats []youtube.Format, rx *cookielessReExtract, cause error, tag string) []youtube.Format {
	kept, dropped := withoutChallengeFormats(formats)
	job.Logger.Warn("[Cipher] "+tag+": the player's JS challenges cannot be solved — dropping every format that needs them",
		"jobID", job.Job.ID, "err", cause, "dropped", dropped, "kept", len(kept))
	if job.YT != nil && ctx.Err() == nil {
		job.Logger.Info("[Cipher] "+tag+": re-extracting with the cookieless clients for copies that need no player JS",
			"jobID", job.Job.ID)
		extra, rxErr := rx.fetch(ctx, job)
		if rxErr != nil {
			job.Logger.Warn("[Cipher] "+tag+": cookieless re-extract failed — continuing with the formats left",
				"jobID", job.Job.ID, "err", rxErr)
		} else {
			free, _ := withoutChallengeFormats(extra)
			kept = append(kept, free...)
			job.Logger.Info("[Cipher] "+tag+": cookieless re-extract merged",
				"jobID", job.Job.ID, "added", len(free))
		}
	}
	return youtube.MergeFormatPool(ctx, nil, kept)
}

// fetchCookielessHls is the challenge-free live fallback's fetch of an HLS
// master that needs no player JS ((*youtube.Service).CookielessHlsManifest)
// behind a package var for the reason fetchCookielessFormats is: job.YT is
// the concrete *youtube.Service, with no other seam. Production never writes
// it.
var fetchCookielessHls = (*youtube.Service).CookielessHlsManifest

// challengeFreeHlsInfo is info with its HLS master replaced by one that needs
// no player JS — the cookieless chain's (fetchCookielessHls), visionos's
// first — or an error saying why there is none: no YouTube service to ask, a
// job already cancelled, no cookieless client serving one. A copy: info is
// never written. Bounded like the cookieless re-extract
// (credentialRefreshTimeoutFor).
func challengeFreeHlsInfo(ctx context.Context, job *JobContext, info *youtube.VideoInfo) (*youtube.VideoInfo, error) {
	if job.YT == nil {
		return nil, errors.New("no YouTube service to ask the cookieless clients with")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rxCtx, cancel := context.WithTimeout(ctx, credentialRefreshTimeoutFor(job.Config))
	defer cancel()
	manifestURL, source, err := fetchCookielessHls(job.YT, rxCtx, job.Job.VideoID)
	if err != nil {
		return nil, err
	}
	cf := *info
	cf.HlsManifestURL, cf.HlsManifestSource = manifestURL, source
	job.Logger.Info("[Cipher] HLS: using the master playlist that needs no player JS",
		"jobID", job.Job.ID, "source", source)
	return &cf, nil
}

// setupWithFallback runs strategy's setup for info and, when it fails for
// want of a GVS token or on the player's JS challenges, the setup that needs
// neither — the one fallback site ExecuteWithChat has:
//
//   - A live capture on a from-start path — manifest-free DASH or a DASH
//     manifest — whose web-family streams need a GVS token none could be
//     minted for, with nothing token-free left to record (errMissingPot),
//     records its HLS manifest instead (missingPotFallback: a stream still
//     StreamLive, its extraction carrying a master). The web family's HLS
//     token is recommended only (_base.py:85-88), so upstream records that
//     HLS bare, and so does DownloadHls (audit G16).
//   - A live capture on a from-start path whose setup failed on the
//     player's JS challenges (setupFailedOnChallenges, audit G10) first
//     records from the start without JS (noJSFromStart, the owner's
//     decision on W8's first follow-up): the formats that need no player JS
//     — the extraction's, plus one cookieless re-extract's, visionos's split
//     live adaptive formats first (challengeFreeFormats) — on the
//     manifest-free path, from the start, as the post-live arm below
//     already does. That is yt-dlp's --live-from-start: it keeps the live
//     adaptive formats of every client (_video.py:3492-3501,
//     _needs_live_processing) and skips each whose n has no answer
//     (3573-3581, "if not n_result: continue"), so a client whose formats
//     carry no n challenge — visionos, the JS-less default client
//     (_DEFAULT_JSLESS_CLIENTS, 144) — is the one that records. Only when
//     that leaves no split video and audio pair, or its setup fails too,
//     does the capture record the live HLS playlist, the job log saying
//     which path ran and why (owner decision 7). That is yt-dlp's own
//     default for a live stream: it skips the live adaptive formats unless
//     --live-from-start asks for them (3492-3501, 3616-3619) and records
//     HLS, and a master whose n has no answer it passes over for the next
//     client's (3692-3713) — DownloadHls does the same through
//     challengeFreeHlsInfo, and asks it too when the extraction carried no
//     master at all. visionos serves a live stream no DASH manifest, so a
//     DASH-manifest capture's no-JS path is the manifest-free one too.
//   - A VOD or post-live DASH-manifest setup that failed with errMissingPot
//     downloads the stream's other formats instead (vodMissingPotFallback,
//     owner decision, wave FC): the manifest-free path for a post-live
//     stream with a manifest-free split, else the whole-file path for one
//     whose pool holds a format a whole-file GET can fetch. Either runs its
//     own missing_pot degrade over the token-free copies, and fails with the
//     missing_pot cause, joined to the DASH setup's, only when it has
//     nothing either. Upstream skips only the manifest (_video.py:3746-3749,
//     yt-dlp 5b57b72c1, #13640) and keeps the formats beside it.
//   - A post-live manifest-free capture has no live edge to fall to. One
//     that failed on the challenges runs again on the formats that need no
//     player JS (challengeFreeFormats: the drop, one cookieless re-extract,
//     the merge), from the start, as the VOD path does; yt-dlp keeps
//     post_live's live adaptive formats from every client
//     (_needs_live_processing), visionos's among them.
//
// Either live arm records as any HLS capture does, from the oldest segment
// the playlist's window still holds (StartSeq -1) — not from the edge alone
// — or from a restart's seed in a fresh part: the failed setup built no
// downloader, so the restart seeds are still unused. Each arm reads a
// capture still on air as it always has: the missing_pot arm a stream
// StreamLive whose extraction carries a master, the challenge arm a capture
// that is not a VOD run, whose DownloadHls asks the cookieless chain for a
// master when the extraction carries none. A setup error both arms could
// take falls to HLS once, under the missing_pot Warn.
//
// Any other setup error — or one of these with the job already cancelled,
// whose mint the cancel may be the failure of — is returned as it came, and
// nothing more is set up. A fallback that fails too names both failures. So
// is a VOD's or post-live stream's DASH-manifest setup (DashStrategy: an OTF
// not_a_stream, a post-live stream with a manifest and no split pair) whose
// manifest n has no answer: yt-dlp skips that manifest and keeps the other
// clients' copies (_video.py:3730-3751), but the whole-file path cannot
// fetch an OTF format, and re-routing a post-live manifest stream onto a
// re-extracted split pair is beyond what the plan asked of this. This site
// is the first setup's; a mid-stream refresh that fails the same way
// switches the capture's path in a new part instead (switchLivePath,
// runLiveStreamDownload's switchPart), and a capture this fallback put on
// the no-JS path refreshes on it (refreshDownload).
//
// A setup that built its downloaders, the first one or a fallback's, then
// clears its part of the other shape's staged media before they write
// (setAsideOtherLiveShape): a job resumed into a part that holds an earlier
// session's from-start capture records HLS beside it here, whichever arm
// sent it there, and one resumed from either fallback once a token can be
// minted or the player solved again records DASH beside its video.ts.
//
// A live capture of a broadcast older than YouTube's ~120 h window
// (marathonLiveEdge, audit G11, owner decision 10) never tries from the
// start: its sq=0 is gone. It records the live HLS playlist first, and the
// from-start setup above runs only when that fails (setupMarathonOr).
func setupWithFallback(ctx context.Context, job *JobContext, strategy DownloadStrategy, isVod bool, info *youtube.VideoInfo, deps *StrategyDeps) (*DownloadResult, error) {
	result, err := setupMarathonOr(ctx, job, strategy, isVod, info, deps)
	if err != nil {
		return result, err
	}
	if err := setAsideOtherLiveShape(job, result); err != nil {
		return nil, err
	}
	return result, nil
}

// setupOrFallback is setupWithFallback's choice of setup: the strategy's
// own, and the fallback's when that fails for want of a token or on the
// challenges.
func setupOrFallback(ctx context.Context, job *JobContext, strategy DownloadStrategy, isVod bool, info *youtube.VideoInfo, deps *StrategyDeps) (*DownloadResult, error) {
	result, err := strategy.Download(ctx, job, info, deps)
	if err == nil || ctx.Err() != nil {
		return result, err
	}
	kind := strategy.Kind()
	if hls := drmHlsFallback(strategy, info, err); hls != nil {
		job.Logger.Warn("[DRM] every DASH Representation of a kind the job needs is DRM-protected — recording the HLS manifest instead",
			"jobID", job.Job.ID, "err", err)
		result, hlsErr := hls.Download(ctx, job, info, deps)
		switch {
		case hlsErr == nil:
			if reason := drmLiveEdgeReason(isVod, info); reason != "" {
				result = withLiveEdge(result, reason)
			}
			return result, nil
		case errors.Is(hlsErr, engine.ErrDRMProtected):
			return nil, drmUnrecordable(fmt.Errorf("%w; the HLS fallback is DRM-protected too: %w", err, hlsErr))
		default:
			return nil, fmt.Errorf("%w; the HLS fallback failed too: %w", err, hlsErr)
		}
	}
	if errors.Is(err, engine.ErrDRMProtected) {
		return nil, drmUnrecordable(err)
	}
	if hls := missingPotFallback(strategy, info, err); hls != nil {
		job.Logger.Warn("[POT] missing_pot: no GVS token for the "+kind+" streams — recording the live HLS playlist instead, from the oldest segment its window holds",
			"jobID", job.Job.ID, "err", err)
		return withLiveEdgeErr(liveEdgeMissingPot)(liveHlsInstead(ctx, job, hls, info, deps, err))
	}
	if alt := vodMissingPotFallback(strategy, isVod, info, err); alt != nil {
		job.Logger.Warn("[POT] missing_pot: no GVS token for the DASH manifest — downloading the "+alt.Kind()+" formats instead",
			"jobID", job.Job.ID, "err", err)
		altResult, altErr := alt.Download(ctx, job, info, deps)
		if altErr != nil {
			return nil, fmt.Errorf("%w; the %s fallback failed too: %w", err, alt.Kind(), altErr)
		}
		return altResult, nil
	}
	if !setupFailedOnChallenges(err) {
		return result, err
	}
	switch {
	case !isVod && (kind == ManifestlessDashStrategy.Kind() || kind == DashStrategy.Kind()):
		return noJSFromStartOrHls(ctx, job, kind, info, deps, &cookielessReExtract{}, err)
	case isVod && kind == ManifestlessDashStrategy.Kind():
		cf := *info
		cf.Formats = challengeFreeFormats(ctx, job, info.Formats, &cookielessReExtract{}, err, "post-live manifest-free DASH")
		if !HasManifestlessDashFormats(cf.Formats) {
			return nil, fmt.Errorf("%w; no split video and audio pair that needs no player JS is left", err)
		}
		result, err := strategy.Download(ctx, job, &cf, deps)
		if err == nil {
			// Its refreshes — the tail an expired URL leaves
			// (runVodDownloadWithRefresh) — stay on the formats that need
			// no player JS too (refreshDownload).
			job.Run.markNoJS()
		}
		return result, err
	}
	return result, err
}

// noJSFromStartOrHls is the challenge arm's live path, for a capture on the
// from-start path kind whose setup — or, mid-stream, whose refresh
// (switchLivePath) — failed on the player's JS challenges with cause: from
// the start without JS (noJSFromStart) when the formats that need none hold a
// split video and audio pair, and the live HLS playlist when they do not, or
// when that setup fails too. Each says, at Warn, which path ran and why. rx
// is the setup's one cookieless re-extract.
func noJSFromStartOrHls(ctx context.Context, job *JobContext, kind string, info *youtube.VideoInfo, deps *StrategyDeps, rx *cookielessReExtract, cause error) (*DownloadResult, error) {
	result, err := noJSFromStart(ctx, job, info, deps, rx, cause)
	if err == nil {
		job.Logger.Warn("[Cipher] live: the player's JS challenges cannot be solved — recording from the start on the split live formats that need no player JS (manifest-free DASH), not the live HLS playlist",
			"jobID", job.Job.ID, "strategy", kind, "videoItag", result.VideoItag, "audioItag", result.AudioItag, "err", cause)
		return result, nil
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%w; %w", cause, err)
	}
	job.Logger.Warn("[Cipher] live: recording from the start cannot run — the player's JS challenges cannot be solved and "+err.Error()+"; recording the live HLS playlist instead, from the oldest segment its window holds",
		"jobID", job.Job.ID, "strategy", kind, "err", cause)
	return withLiveEdgeErr(liveEdgeNoJS)(liveHlsInstead(ctx, job, HlsStrategy, info, deps, fmt.Errorf("%w; %w", cause, err)))
}

// errNoChallengeFreePair is noJSFromStart's answer when the formats that need
// no player JS hold no split video and audio pair to record from the start.
var errNoChallengeFreePair = errors.New("no split video and audio pair that needs no player JS is left")

// noJSFromStart is a live capture's from-start setup on the formats that
// need no player JS (challengeFreeFormats: the extraction's own, and the
// cookieless chain's, visionos first) on the manifest-free path, for a
// capture whose from-start setup failed on the challenges with cause. A copy
// of info carries them: info is never written. It fails with
// errNoChallengeFreePair when they hold no split pair, and with the setup's
// own error when that fails.
//
// The capture it builds is DASH-shaped (video_stream, audio_stream) and may
// be another client's rendition of the stream than the one the part dir
// already holds — a tv or web DASH capture an earlier session of the job, or
// this one, recorded before the challenges stopped being solvable. Recorded
// into the same names it would resume through their sidecar, appending one
// rendition's fragments under the other's init segment; so any DASH capture
// in the part dir is set aside first, as the shape set-aside sets one aside
// (setAsideStagedMedia), muxed into its own file beside the archive at
// finalize and never written over. The downloaders read their resume state
// when they start, not when they are built, so the set-aside after the
// setup is ahead of it. A dir that cannot be cleared fails the setup.
//
// The run then records on the no-JS path (runRecord.markNoJS), so every
// refresh of the capture stays on it (refreshDownload, noJSSetup).
func noJSFromStart(ctx context.Context, job *JobContext, info *youtube.VideoInfo, deps *StrategyDeps, rx *cookielessReExtract, cause error) (*DownloadResult, error) {
	result, err := noJSSetup(ctx, job, info, deps, rx, cause, "live from-start DASH")
	if err != nil {
		return nil, err
	}
	asides, err := setAsideStagedMedia(job.StagingDir, []string{"video_stream", "audio_stream"})
	if len(asides) > 0 {
		job.Logger.Warn("an earlier from-start capture of this job was set aside before the no-JS from-start capture records into its part; it is muxed to its own file beside the archive, never into it",
			"jobID", job.Job.ID, "asides", strings.Join(asides, " | "))
	}
	if err != nil {
		return nil, fmt.Errorf("staging holds an earlier from-start capture that could not be set aside: %w", err)
	}
	job.Run.markNoJS()
	return result, nil
}

// errNoJSReExtractFailed is noJSSetup's answer when no split pair is left
// and the cookieless re-extract that should have added one failed — one
// network error, a brief outage — so nothing says the chain has no pair to
// give. Not errNoChallengeFreePair: a capture on the no-JS path that read
// this as "no pair left" spent its one HLS switch, and left a healthy no-JS
// path for good, over a fetch the live loop's still-live retries
// (refreshWhileLive) would have got past. liveSwitchWanted refuses it.
var errNoJSReExtractFailed = errors.New("no split pair that needs no player JS is in the extraction, and the cookieless re-extract for one failed")

// noJSSetup is the manifest-free setup on the formats in info that need no
// player JS, and the cookieless chain's (challengeFreeFormats over rx), on a
// copy of info: noJSFromStart's, and every refresh's of a capture already on
// that path (refreshDownload) — a fresh extraction ranks the copies that need
// the JS first, and a refresh of the whole pool picked one, failed on it, and
// sent the capture through a path switch it did not need. errNoChallengeFreePair
// when no split pair is left; errNoJSReExtractFailed, wrapping the fetch's
// error, when none is left because the re-extract failed; the setup's own
// error otherwise. cause is why the formats that need the JS are dropped, for
// the log; tag names the setup.
//
// yt-dlp's live-from-start refresh (`_prepare_live_from_start_formats`'s
// url_feed, _video.py:1948-1984) re-extracts and keeps the format with the
// same `_itag` and the same `_client`; this re-selects among the no-JS
// copies instead, so it may pick another itag or client than the one
// recording, and streamIdentityChanged then splits the part rather than
// append one rendition's fragments under the other's init segment.
func noJSSetup(ctx context.Context, job *JobContext, info *youtube.VideoInfo, deps *StrategyDeps, rx *cookielessReExtract, cause error, tag string) (*DownloadResult, error) {
	cf := *info
	cf.Formats = challengeFreeFormats(ctx, job, info.Formats, rx, cause, tag)
	if !HasManifestlessDashFormats(cf.Formats) {
		if rx.done && rx.err != nil {
			return nil, fmt.Errorf("%w: %w", errNoJSReExtractFailed, rx.err)
		}
		return nil, errNoChallengeFreePair
	}
	result, err := ManifestlessDashStrategy.Download(ctx, job, &cf, deps)
	if err != nil {
		return nil, fmt.Errorf("the no-JS from-start setup failed too: %w", err)
	}
	return result, nil
}

// errNoJSRefresh is the cause a refresh of a capture on the no-JS path names
// for the formats it drops: the path itself, chosen when the challenges could
// not be solved.
var errNoJSRefresh = errors.New("the capture records on the formats that need no player JS")

// errNoLivePathLeft is switchLivePath's answer when the run has already
// switched to every path it could switch to.
var errNoLivePathLeft = errors.New("no live path is left to switch to this run")

// switchLivePath builds the downloaders a live capture switches to once a
// mid-stream refresh of its from-start path — manifest-free DASH or a DASH
// manifest, not HLS — failed on the player's JS challenges with cause, or,
// on the no-JS path, found no split pair left (the owner's decision on W8's
// second follow-up). job is the NEW part's context (runLiveStreamDownload
// opens it; the current part is kept and muxed as its own file, never
// written over), info the fresh extraction. First the no-JS from-start path
// (noJSFromStart, decision 1), then the live HLS playlist, each at most once
// a run (runRecord.first): a capture already on the no-JS path goes to HLS,
// and one that has switched to both has nowhere left (errNoLivePathLeft,
// cause wrapped) — the loop then handles the failure as it always has. Never
// a loop: a spent switch is not given back. The way back is the live loop's
// own, once a run (switchBack, owner decision PR-R3): a capture on either
// fallback returns to the from-start path that needs the JS, in a new part,
// once that path can be set up again — and a later run (a restart, a Resume)
// starts on the path its setup chooses, the JS path once solving works.
// path names the one switched to, for the caller's log.
func switchLivePath(ctx context.Context, job *JobContext, info *youtube.VideoInfo, deps *StrategyDeps, cause error) (result *DownloadResult, path string, err error) {
	if !job.Run.noJS() && job.Run.first("live-switch:nojs") {
		result, err := noJSFromStart(ctx, job, info, deps, &cookielessReExtract{}, cause)
		if err == nil {
			job.Logger.Warn("[Cipher] live: a refresh failed on the player's JS challenges — switching, in a new part, to recording on the split live formats that need no player JS (manifest-free DASH); the part so far is kept as its own file",
				"jobID", job.Job.ID, "videoItag", result.VideoItag, "audioItag", result.AudioItag, "err", cause)
			return result, "no-JS DASH", nil
		}
		if ctx.Err() != nil {
			return nil, "", fmt.Errorf("%w; %w", cause, err)
		}
		cause = fmt.Errorf("%w; %w", cause, err)
	}
	if job.Run.first("live-switch:hls") {
		job.Logger.Warn("[Cipher] live: a refresh failed on the player's JS challenges and no split pair that needs no player JS is left — switching, in a new part, to the live HLS playlist; the part so far is kept as its own file",
			"jobID", job.Job.ID, "err", cause)
		result, err := liveHlsInstead(ctx, job, HlsStrategy, info, deps, cause)
		if err != nil {
			return nil, "", err
		}
		return result, "HLS", nil
	}
	return nil, "", fmt.Errorf("%w: %w", errNoLivePathLeft, cause)
}

// liveSwitchWanted reports whether a mid-stream refresh over info that failed
// with err is one a path switch answers (switchLivePath): a systemic failure
// on the player's JS challenges (setupFailedOnChallenges), or a no-JS
// refresh with no split pair left (errNoChallengeFreePair). Only for a
// capture that is not HLS: an HLS capture's master is answered by the
// cookieless chain already (challengeFreeHlsInfo), and there is no path
// after it.
//
// Never for a refresh that failed for a reason a switch does not answer,
// though its error may read like one, since each switch is spent once a run
// and a switched capture never goes back: an extraction with no formats at
// all — the interruption signature (live, zero formats; interruptionSignal),
// which no path can record from and the resume wait is for, though a no-JS
// refresh of it finds "no pair" — and a no-JS refresh whose cookieless
// re-extract failed (errNoJSReExtractFailed), which the still-live retries
// get past. Both are handled as any other failed refresh.
func liveSwitchWanted(info *youtube.VideoInfo, err error, isHls bool) bool {
	if isHls || info == nil || len(info.Formats) == 0 || errors.Is(err, errNoJSReExtractFailed) {
		return false
	}
	return setupFailedOnChallenges(err) || errors.Is(err, errNoChallengeFreePair)
}

// liveHlsInstead is either live arm of setupOrFallback: hls's setup for the
// capture whose from-start setup failed with setupErr, and both causes when
// it fails too — why HLS ran, and why it could not either.
func liveHlsInstead(ctx context.Context, job *JobContext, hls DownloadStrategy, info *youtube.VideoInfo, deps *StrategyDeps, setupErr error) (*DownloadResult, error) {
	result, err := hls.Download(ctx, job, info, deps)
	if err != nil {
		return nil, fmt.Errorf("%w; the live HLS fallback failed too: %w", setupErr, err)
	}
	// What the live loop's switch back reads (runRecord.canSwitchBack).
	job.Run.markHlsFallback()
	return result, nil
}
