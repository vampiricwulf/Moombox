package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/vampiricwulf/Moombox/internal/cipher"
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
//     format resolves until it is back.
//   - cipher.ErrPlayerJSStale: ejs could not solve the player even after a
//     fresh fetch of it — the case of a player rotation ejs does not handle
//     yet.
//   - cipher.ErrPlayerJSFetch: the player could not be fetched, with no
//     cached copy to serve instead (PlayerCache.Fetch). Nothing solves a
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
//     player's JS challenges (setupFailedOnChallenges, audit G10) records
//     the live HLS playlist instead, the job log saying why from-start was
//     dropped (owner decision 7). That is yt-dlp's own default for a live
//     stream: it skips the live adaptive formats unless --live-from-start
//     asks for them (_video.py:3492-3501, 3616-3619) and records HLS, and a
//     master whose n has no answer it passes over for the next client's
//     (3692-3713) — DownloadHls does the same through challengeFreeHlsInfo,
//     and asks it too when the extraction carried no master at all. The
//     from-start paths have no copy to fall to: visionos serves a live
//     stream no DASH manifest, and a challenge-free adaptive copy is not
//     looked for here (the plan's call), so the live playlist is what is
//     left.
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
// re-extracted split pair is beyond what the plan asked of this. Only the
// first setup falls back: a mid-stream refresh (refreshDownload) keeps the
// strategy the capture runs on.
//
// A setup that built its downloaders, the first one or a fallback's, then
// clears its part of the other shape's staged media before they write
// (setAsideOtherLiveShape): a job resumed into a part that holds an earlier
// session's from-start capture records HLS beside it here, whichever arm
// sent it there, and one resumed from either fallback once a token can be
// minted or the player solved again records DASH beside its video.ts.
func setupWithFallback(ctx context.Context, job *JobContext, strategy DownloadStrategy, isVod bool, info *youtube.VideoInfo, deps *StrategyDeps) (*DownloadResult, error) {
	result, err := setupOrFallback(ctx, job, strategy, isVod, info, deps)
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
	if hls := missingPotFallback(strategy, info, err); hls != nil {
		job.Logger.Warn("[POT] missing_pot: no GVS token for the "+kind+" streams — recording the live HLS playlist instead, from the oldest segment its window holds",
			"jobID", job.Job.ID, "err", err)
		return liveHlsInstead(ctx, job, hls, info, deps, err)
	}
	if !setupFailedOnChallenges(err) {
		return result, err
	}
	switch {
	case !isVod && (kind == ManifestlessDashStrategy.Kind() || kind == DashStrategy.Kind()):
		job.Logger.Warn("[Cipher] live: recording from the start cannot run — the player's JS challenges cannot be solved; recording the live HLS playlist instead, from the oldest segment its window holds",
			"jobID", job.Job.ID, "strategy", kind, "err", err)
		return liveHlsInstead(ctx, job, HlsStrategy, info, deps, err)
	case isVod && kind == ManifestlessDashStrategy.Kind():
		cf := *info
		cf.Formats = challengeFreeFormats(ctx, job, info.Formats, &cookielessReExtract{}, err, "post-live manifest-free DASH")
		if !HasManifestlessDashFormats(cf.Formats) {
			return nil, fmt.Errorf("%w; no split video and audio pair that needs no player JS is left", err)
		}
		return strategy.Download(ctx, job, &cf, deps)
	}
	return result, err
}

// liveHlsInstead is either live arm of setupOrFallback: hls's setup for the
// capture whose from-start setup failed with setupErr, and both causes when
// it fails too — why HLS ran, and why it could not either.
func liveHlsInstead(ctx context.Context, job *JobContext, hls DownloadStrategy, info *youtube.VideoInfo, deps *StrategyDeps, setupErr error) (*DownloadResult, error) {
	result, err := hls.Download(ctx, job, info, deps)
	if err != nil {
		return nil, fmt.Errorf("%w; the live HLS fallback failed too: %w", setupErr, err)
	}
	return result, nil
}
