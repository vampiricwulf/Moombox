package worker

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// StrategyDeps bundles the per-strategy dependencies. Each strategy
// uses only the fields it needs (a strategy ignores irrelevant fields
// rather than rejecting nil), letting the orchestrator pass one
// shared deps struct instead of N per-strategy parameter lists.
//
// Audit reports/worker.md F49/Q9.
type StrategyDeps struct {
	// CipherSolver is the legacy *cipher.GojaResolver.  Kept for
	// GetSts, InvalidateSolver, and any goja-internal operations that
	// the cipher.Solver interface doesn't expose.  Sig/n-param URL
	// decryption should use RoutedCipherSolver instead.
	CipherSolver *cipher.GojaResolver

	// RoutedCipherSolver is the composite cipher.Solver (sidecar
	// primary, goja fallback).  DASH and HLS strategies call this for
	// sig and n-param decryption so the work flows through the V8 ejs
	// path on cb017549-family players.  nil falls back to CipherSolver
	// directly (pre-sidecar behaviour).
	RoutedCipherSolver cipher.Solver

	// PotProvider mints PO tokens for GVS (Google Video Server) requests.
	// All YouTube strategies use it when non-nil.
	PotProvider *bgutils.PotProvider

	// IsOnline is the connectivity check used by manifest-fetching
	// strategies (DASH, HLS) to bail out early during an offline blip.
	// VOD doesn't poll for connectivity.
	IsOnline func() bool
}

// DownloadStrategy is the unified entry-point for platform-specific
// download setup. Each implementation prepares segment downloaders,
// resolves manifest URLs, and returns a DownloadResult ready for the
// orchestrator's progress-tracking + run loop.
//
// Implementations are stateless (zero-sized adapter types) and live
// at the bottom of this file. The orchestrator selects one based on
// videoInfo shape (DASH manifest, HLS manifest, direct formats).
//
// Audit reports/worker.md F49/Q9 — was a chain of top-level functions
// with three different parameter lists; now mockable via the
// interface for table-driven tests.
type DownloadStrategy interface {
	Download(ctx context.Context, job *JobContext, info *youtube.VideoInfo, deps *StrategyDeps) (*DownloadResult, error)
	Kind() string
}

// selectDownloadStrategy picks the download strategy from the resolved stream
// shape. Pure (no I/O) so the routing rules stay unit-testable. Returns an
// error only when no strategy can handle the available formats.
func selectDownloadStrategy(isVod bool, info *youtube.VideoInfo) (DownloadStrategy, error) {
	// not_a_stream, or a VOD without a DASH manifest -> direct whole-file fetch.
	useDirectVod := isVod && (info.StreamStatus == youtube.StreamNotAStream || info.DashManifestURL == "")
	switch {
	// Post-live "manifestless DASH": a was-live stream that just ended is
	// classified StreamPostLive (and folded into isVod), but its open-ended
	// adaptive URLs are segment-addressed (&sq=N). A whole-file VodStrategy
	// GET of those returns a single moov-less fragment ("moov atom not
	// found"); the segment-addressed manifestless path — the same one the
	// live downloader uses — fetches sq=0's ftyp+moov init and terminates
	// once currentSeq passes the head harvested from X-Head-Seqnum.
	// Scoped to StreamPostLive specifically so a genuine finished StreamVOD —
	// whose adaptive URLs DO serve whole files — keeps using VodStrategy.
	// Preferred over DashStrategy even when a dashManifestUrl is present:
	// yt-dlp 8c1f07d81 migrated post-live off DASH manifests entirely
	// (post-live manifests only cover the trailing ~2h window).
	case isVod && info.StreamStatus == youtube.StreamPostLive &&
		HasManifestlessDashFormats(info.Formats):
		return ManifestlessDashStrategy, nil
	case useDirectVod && len(info.Formats) > 0:
		return VodStrategy, nil
	case !isVod && HasManifestlessDashFormats(info.Formats):
		// Manifest-free DASH — the PRIMARY live path since yt-dlp 8c1f07d81
		// declared live DASH manifests "no longer properly supported"
		// (2026-07): fetch each selected itag's adaptiveFormats[] URL with
		// `&sq=N` appended, discovering the live edge from the
		// X-Head-Seqnum response header instead of refetching an MPD.
		// Originally built for the yt-dlp#15274 experiment where YouTube
		// withheld dashManifestUrl from cookied clients; now routed ahead
		// of DashStrategy for every live stream that ships split
		// video+audio adaptive URLs. Preempts HLS because DASH gives
		// per-itag selection, separate audio (cleaner mux), and
		// live-from-start segment addressability that HLS can't do.
		return ManifestlessDashStrategy, nil
	case info.DashManifestURL != "":
		// Manifest-driven DASH survives as the fallback for responses whose
		// format pool lacks usable split adaptive URLs.
		return DashStrategy, nil
	case info.HlsManifestURL != "":
		return HlsStrategy, nil
	case len(info.Formats) > 0:
		// Fallback: no DASH/HLS manifest but formats exist — download directly.
		return VodStrategy, nil
	default:
		return nil, fmt.Errorf("no download strategy available")
	}
}

// vodStrategyT / dashStrategyT / hlsStrategyT — stateless adapters
// that delegate to the existing Download* functions. Kept stateless
// (zero-size) so the package-level singletons below cost nothing at
// runtime.
type vodStrategyT struct{}
type dashStrategyT struct{}
type manifestlessDashStrategyT struct{}
type hlsStrategyT struct{}

func (vodStrategyT) Kind() string              { return "vod" }
func (dashStrategyT) Kind() string             { return "dash" }
func (manifestlessDashStrategyT) Kind() string { return "manifestless_dash" }
func (hlsStrategyT) Kind() string              { return "hls" }

func (vodStrategyT) Download(ctx context.Context, job *JobContext, info *youtube.VideoInfo, deps *StrategyDeps) (*DownloadResult, error) {
	return DownloadVod(ctx, job, info, deps.RoutedCipherSolver, deps.CipherSolver, deps.PotProvider)
}

func (dashStrategyT) Download(ctx context.Context, job *JobContext, info *youtube.VideoInfo, deps *StrategyDeps) (*DownloadResult, error) {
	return DownloadDash(ctx, job, info, deps.RoutedCipherSolver, deps.CipherSolver, deps.PotProvider, deps.IsOnline)
}

func (manifestlessDashStrategyT) Download(ctx context.Context, job *JobContext, info *youtube.VideoInfo, deps *StrategyDeps) (*DownloadResult, error) {
	return DownloadManifestlessDash(ctx, job, info, deps.RoutedCipherSolver, deps.CipherSolver, deps.PotProvider, deps.IsOnline)
}

func (hlsStrategyT) Download(ctx context.Context, job *JobContext, info *youtube.VideoInfo, deps *StrategyDeps) (*DownloadResult, error) {
	return DownloadHls(ctx, job, info, deps.RoutedCipherSolver, deps.CipherSolver, deps.PotProvider, deps.IsOnline)
}

// VodStrategy / DashStrategy / ManifestlessDashStrategy / HlsStrategy
// are the package-level singletons used by the orchestrator's
// strategy-selection switch. Tests can substitute their own
// DownloadStrategy implementations.
var (
	VodStrategy              DownloadStrategy = vodStrategyT{}
	DashStrategy             DownloadStrategy = dashStrategyT{}
	ManifestlessDashStrategy DownloadStrategy = manifestlessDashStrategyT{}
	HlsStrategy              DownloadStrategy = hlsStrategyT{}
)

// nPathRe matches n-parameter encoded in URL path: /n/{encrypted_value}/
var nPathRe = regexp.MustCompile(`/n/([a-zA-Z0-9_-]{10,})/`)

// DownloadResult contains the result of a download strategy.
type DownloadResult struct {
	VideoDownloader *engine.SegmentDownloader
	AudioDownloader *engine.SegmentDownloader
	VideoPath       string
	AudioPath       string
	HasVideo        bool
	HasAudio        bool
	VideoFormat     *youtube.Format
	AudioFormat     *youtube.Format
	IsHls           bool // true if HLS strategy was used
	// ChatPath is the staged chat JSON belonging to this capture span
	// (Twitch per-part chat — set when the chat file was rolled at the
	// part boundary). muxSegment copies it next to the part's video and
	// records it on the segment row. Empty for platforms/jobs whose chat
	// is one file handled at finalize (YouTube, legacy staging layouts).
	ChatPath string
	// Stream dimensions (populated by all strategies for quality monitoring)
	VideoWidth  int
	VideoHeight int
	VideoFps    int
}

// decryptNParamInURL finds and decrypts the 'n' parameter in a URL.
// YouTube encodes n-params both in query strings (?n=value) and in URL paths (/n/{value}/).
// Both forms must be decrypted to avoid throttling/403 errors.
func decryptNParamInURL(rawURL string, nDecrypt func(string) (string, error)) (string, error) {
	result := rawURL

	// Check for n parameter in path: /n/{encrypted_value}/
	// Match values that look like encrypted n-params (10+ alphanumeric/special chars)
	if matches := nPathRe.FindStringSubmatch(result); len(matches) >= 2 {
		encryptedN := matches[1]
		decrypted, err := nDecrypt(encryptedN)
		if err == nil && decrypted != encryptedN {
			result = strings.Replace(result, "/n/"+encryptedN+"/", "/n/"+decrypted+"/", 1)
		}
	}

	// Also check query string n param.
	// Use string replacement to preserve original parameter order —
	// Go's url.Values.Encode() sorts parameters alphabetically, which breaks
	// YouTube's URL signature verification and causes HTTP 403.
	parsed, err := url.Parse(result)
	if err != nil {
		return result, err
	}
	// Extract raw (percent-encoded) n-param for accurate string matching.
	rawN, nParam := cipher.RawQueryParam(parsed.RawQuery, "n")
	if rawN != "" && nParam != "" {
		decrypted, err := nDecrypt(nParam)
		if err == nil && decrypted != nParam {
			for _, prefix := range []string{"?", "&"} {
				old := prefix + "n=" + rawN
				if strings.Contains(result, old) {
					result = strings.Replace(result, old, prefix+"n="+url.QueryEscape(decrypted), 1)
					break
				}
			}
		}
	}

	return result, nil
}

// invalidate403Caches clears cipher solver state, POT caches, and visitor data
// after a 403 burst. Cipher rotation and POT expiry both surface as 403 from
// segment fetches; without distinguishing signal, invalidate everything that
// could be stale and let the next manifest refresh repopulate.
//
// Order matters: POT caches wipe BEFORE visitor data invalidation. A
// concurrent goroutine that observed empty visitor data after invalidation
// would otherwise fetch a fresh watch page, install fresh visitor data, and
// mint a POT bound to that new value — which the subsequent
// PotProvider.InvalidateCaches() would then wipe. Empty cache → fresh visitor
// data → fresh mint is the cheaper sequence.
//
// `tag` is a short label included in the log line for triage (e.g. "DASH
// video", "DASH audio", "HLS").
func invalidate403Caches(job *JobContext, playerURL string, cipherSolver *cipher.GojaResolver, potProvider *bgutils.PotProvider, tag string) {
	job.Logger.Warn("[Cipher] "+tag+" 403 signal — invalidating solver and POT", "jobID", job.Job.ID, "playerURL", playerURL)
	if cipherSolver != nil {
		cipherSolver.InvalidateSolver(playerURL)
	}
	if potProvider != nil {
		potProvider.InvalidateCaches()
	}
	if job.YT != nil {
		job.YT.InvalidateVisitorData()
	}
}

// gvsTokenMinter is the slice of *bgutils.PotProvider that credential
// refresh needs to mint a fresh GVS token. Declared here (consumer side)
// so refreshGvsCredentials is testable without a real BotGuard sidecar;
// *bgutils.PotProvider satisfies it implicitly.
//
// invalidate403Caches deliberately keeps taking the concrete
// *bgutils.PotProvider rather than this interface: widening it would let a
// caller's typed-nil *bgutils.PotProvider (PotProvider disabled — every
// existing 403 call site nil-checks it, so this is a real, live case, not
// hypothetical) box into a NON-nil gvsTokenMinter value. Its own
// `if potProvider != nil` guard would then read true and dereference a nil
// receiver. refreshGvsCredentials bridges the two via asPotProvider /
// minterUsable below instead of touching invalidate403Caches's signature.
type gvsTokenMinter interface {
	GeneratePoTokenString(ctx context.Context, contentBinding string, bypassCache bool) (string, error)
}

// credentialRefreshTimeout is the CEILING refreshGvsCredentials' entire
// round trip (player-response re-fetch + cipher URL re-resolve + GVS token
// re-mint) is bounded by. The engine calls OnCredentialRefresh
// SYNCHRONOUSLY on the download-loop goroutine and does NOT reset
// lastSegTime around it (internal/engine/downloader.go refreshCredentials)
// — an unbounded refresh would silently spend the operator's MaxTimeout
// stall budget on network calls instead of segment retries, defeating the
// recovery it exists to provide.
//
// 45s sits comfortably above a normal mint: invalidate403Caches wipes the
// POT cache on every call, so the mint this function triggers is always at
// least a warm-cache miss and often the full cold path (challenge fetch +
// GenerateIT + a BotGuard interpreter pass) — the sidecar's own
// RequestTimeout budgets up to 90s for exactly that case, sized generously
// to also catch a genuinely wedged V8, not because a healthy cold mint
// needs the full 90s. 45s leaves a cold mint about half that ceiling while
// costing at most 7.5% of config.MaximumTimeout's 600s default.
//
// This ceiling is clamped per-job by credentialRefreshTimeoutFor against
// job.Config.MaximumTimeout, specifically so a job running near the 30s
// validated floor can't have one refresh attempt consume its ENTIRE stall
// budget — see that function's doc for why a flat 45s was unsafe there
// (behindHeadTailPending would read false right as working credentials
// arrived, and the very next 403 would force-finalize the recording).
const credentialRefreshTimeout = 45 * time.Second

// credentialRefreshTimeoutFor clamps credentialRefreshTimeout to at most
// MaxTimeout/3, keyed off job.Config.MaximumTimeout (the same value that
// seeds engine.DownloaderOptions.MaxTimeout for this job).
//
// At the operator-configured floor (config.MaximumTimeout validated to a
// 30s minimum), a flat 45s ceiling would let one refresh attempt consume
// the WHOLE stall budget by itself — worse, since the engine doesn't reset
// lastSegTime around the callback, behindHeadTailPending's
// `lastSegTime.Since() < MaxTimeout` check would read false the moment the
// refresh returns, so a subsequent 403 force-finalizes the recording at
// the exact instant working credentials arrived. MaxTimeout/3 leaves at
// least two more retry cycles' worth of budget after a refresh lands,
// whatever MaxTimeout is configured to. A missing/zero Config leaves the
// flat ceiling in place (tests that don't wire JobConfig get the documented
// 45s default rather than a divide-by-zero-shaped clamp to 0).
func credentialRefreshTimeoutFor(cfg *JobConfig) time.Duration {
	if cfg == nil || cfg.MaximumTimeout <= 0 {
		return credentialRefreshTimeout
	}
	if third := time.Duration(cfg.MaximumTimeout) * time.Second / 3; third < credentialRefreshTimeout {
		return third
	}
	return credentialRefreshTimeout
}

// asPotProvider extracts the concrete *bgutils.PotProvider from a
// gvsTokenMinter for invalidate403Caches, which needs the concrete type
// (it fans cache clearing out to the sidecar, which gvsTokenMinter doesn't
// expose). Returns nil for a test fake (or any other non-PotProvider
// implementation) and for a genuinely nil *bgutils.PotProvider — both read
// as "nothing to invalidate", which is exactly what invalidate403Caches's
// own nil check already expects.
func asPotProvider(m gvsTokenMinter) *bgutils.PotProvider {
	concrete, _ := m.(*bgutils.PotProvider)
	return concrete
}

// minterUsable reports whether m is safe to call GeneratePoTokenString on.
// A plain `m != nil` is not enough: a caller holding a nil
// *bgutils.PotProvider (PotProvider disabled) passes it here as a NON-nil
// gvsTokenMinter interface value wrapping a nil pointer — Go's classic
// typed-nil footgun — and `m != nil` would read true right before the call
// dereferences a nil receiver.
func minterUsable(m gvsTokenMinter) bool {
	if m == nil {
		return false
	}
	if concrete, ok := m.(*bgutils.PotProvider); ok {
		return concrete != nil
	}
	return true
}

// refreshGvsCredentials produces a fresh URL + PO token for a live segment
// downloader whose current credentials started earning 403s below the live
// head. It is the in-process equivalent of cancelling and resuming a job,
// which is what operators had to do manually before this existed.
//
// Both upstreams recover the same way — yt-dlp's url_feed and moonarchive's
// "stream access expired? retrieve a fresh manifest" both go back to the
// player response rather than refreshing the URL alone, because the URL's
// own expiry/session parameters (`expire`, `ei`, …) come from the player
// response, not from cipher decryption — re-resolving a cached, already-
// expired Format through a freshly invalidated cipher solver still
// reproduces the SAME expired URL byte-for-byte. So this function re-fetches
// the player response (job.YT.GetVideoInfo — the same call the job-start and
// quality-recovery paths use to obtain VideoInfo) and resolves the chosen
// itag from ITS formats, falling back to the caller's cached
// videoInfo.Formats/PlayerURL only if that fetch fails. GetVideoInfo itself
// does not decrypt sig/n — parsing leaves Format.URL raw — so
// resolveFormatURLByItag still runs cipher resolution afterward exactly as
// before, just against fresh formats instead of stale ones.
//
// The whole call (mint + re-fetch + resolve) is bounded by a per-job
// timeout derived from ctx — see credentialRefreshTimeoutFor. The mint runs
// FIRST, against that full remaining budget, and the URL half (re-fetch +
// resolve) spends whatever is left on the SAME shared deadline. This order
// is deliberate, not incidental: credentialRefreshTimeoutFor's clamp can
// take the whole budget as low as ~1/3 of a 30s-floor MaxTimeout (10s), and
// a cold BotGuard mint can genuinely need tens of seconds — the sidecar's
// own RequestTimeout budgets 90s for exactly that case. If the player-
// response re-fetch ran first on a tight budget, a slow-but-honest fetch
// could exhaust the whole window and hand GeneratePoTokenString an
// already-expired context, killing the re-mint — which is the half this
// entire recovery path exists for; a stale URL still fetches the same
// (still-valid-until-expiry) bytes, but a stale/cached token is the exact
// credential that just earned the 403. Minting first guarantees it gets a
// full shot at the budget regardless of how slow the URL half turns out to
// be; the URL half degrading to "" under a starved remainder is the
// acceptable half to lose, matching the manual cancel-and-resume workaround
// this callback replaces — a fresh mint alone is a genuine (if partial)
// recovery, a fresh URL alone with a stale token is not.
//
// binding is the GVS content binding to mint under, resolved ONCE by the
// caller (gvsBinding, at strategy setup) rather than recomputed here. The
// current binding source (VideoInfo.GvsBinding, stamped at extraction) is
// immune to cache invalidation, but the pass-the-stable-value contract
// stays: invalidate403Caches clears job.YT's visitor data mid-refresh, and
// any future binding source that reads live session state would silently
// drift between refreshes — or between the two downloaders sharing job.YT —
// if it were recomputed per call. One resolution at setup keeps every mint,
// across both downloaders and every retry, under the one binding scheme the
// job started with.
//
// The URL half (re-fetch + resolve) only runs when a cipher solver is
// actually wired and the job has a PlayerURL to route through — mirroring
// OnCipherFailure's install guard. Without a solver, resolution can only
// fail (RoutedResolveURL requires one even for solver-less passthrough),
// so skipping it avoids a wasted GetVideoInfo call and a Warn on every
// single attempt for work that cannot succeed; the token half still runs
// unconditionally.
//
// Never returns an error: a failed refresh yields empty strings, the engine
// keeps its existing credentials, and the download fails the same way it
// would have without this callback.
func refreshGvsCredentials(
	ctx context.Context,
	job *JobContext,
	videoInfo *youtube.VideoInfo,
	itag int,
	routedSolver cipher.Solver,
	cipherSolver *cipher.GojaResolver,
	potProvider gvsTokenMinter,
	binding string,
	tag string,
) (baseURL string, poToken string) {
	var cfg *JobConfig
	if job != nil {
		cfg = job.Config
	}
	refreshCtx, cancel := context.WithTimeout(ctx, credentialRefreshTimeoutFor(cfg))
	defer cancel()

	invalidate403Caches(job, videoInfo.PlayerURL, cipherSolver, asPotProvider(potProvider), tag)

	// Mint FIRST — before the URL half spends any of the shared refreshCtx
	// deadline. See the doc comment above for why: the clamp can leave as
	// little as ~10s, a cold mint can need tens of seconds, and the token is
	// the credential that actually went stale.
	//
	// Gated by the refreshed stream's own client, read from the same setup
	// format the strategy chose (formatSourceByItag), by the rule every
	// strategy uses (youtube.GvsTokenRequired): a tv, web_embedded, visionos
	// or android_vr stream was left bare at setup and must stay bare here, or
	// its first 403 would put a token on exactly the URL the setup gate kept
	// clean. The URL half below still runs for it.
	//
	// bypassCache=true on the re-mint: the cached token is the credential
	// that just 403'd — handing it back unchanged would make this refresh a
	// no-op.
	streamSource := formatSourceByItag(videoInfo.Formats, itag)
	if minterUsable(potProvider) {
		if !youtube.GvsTokenRequired(streamSource) {
			logGvsTokenSkipped(job, streamSource, "tag", tag)
		} else if token, err := potProvider.GeneratePoTokenString(refreshCtx, binding, true); err != nil {
			job.Logger.Warn("[POT] credential refresh: re-mint failed",
				"jobID", job.Job.ID, "tag", tag, "err", err)
		} else {
			poToken = token
		}
	}

	if videoInfo.PlayerURL != "" && (routedSolver != nil || cipherSolver != nil) {
		formats := videoInfo.Formats
		playerURL := videoInfo.PlayerURL
		if job.YT != nil {
			fresh, err := refreshVideoInfo(job.YT, refreshCtx, job.Job.VideoID)
			if err != nil {
				job.Logger.Warn("[POT] credential refresh: player response re-fetch failed; resolving against cached formats",
					"jobID", job.Job.ID, "tag", tag, "err", err)
			} else if fresh != nil {
				formats = fresh.Formats
				if fresh.PlayerURL != "" {
					playerURL = fresh.PlayerURL
				}
			}
			// Interruption spec Tier 1 evidence: this re-fetch runs on every
			// 403 recovery attempt (OnCredentialRefresh), a ~20-30s cadence
			// under a stall — success or formats-empty, either way the
			// signal should observe it so a fresh signature stays trusted
			// across the whole interruption, not just the moment it started.
			// AUTHENTICATED fetch — dead cookies mid-stream can produce the
			// same shape as a healthy auth-walled stream; observe() itself
			// guards against arming on that (I4 fix).
			job.Interruption.observe(fresh)
		}
		if formatBecameWholeFile(formats, itag) {
			// The itag that was segmented (OTF/live, no contentLength) at
			// strategy setup has since resolved to a complete-file format —
			// YouTube finished VOD processing mid-job, which happens exactly
			// during the post-live tail where behind-head 403 bursts (and
			// this refresh) occur. SetBaseURL would install it and
			// buildSegmentURL would append &sq=N to a byte-range URL:
			// YouTube ignores &sq on a whole-file URL and returns the ENTIRE
			// file for every sequence number — the disk-filling runaway
			// HasManifestlessDashFormats' doc comment describes (it guards
			// the same discriminator at strategy-selection time; this is the
			// same check applied mid-download, after the itag was already
			// chosen). OnCipherFailure cannot hit this because it resolves
			// against the cached live formats, never a fresh player-response
			// fetch. Skip the URL install; the token half above still ran.
			job.Logger.Warn("[POT] credential refresh: itag now resolves to a whole-file format (VOD processing finished mid-job) — skipping URL install to avoid feeding a non-segmented URL to the &sq loop",
				"jobID", job.Job.ID, "tag", tag, "itag", itag)
		} else if fresh, err := resolveFormatURLByItag(refreshCtx, formats, itag, routedSolver, cipherSolver, playerURL, job.Logger); err != nil {
			job.Logger.Warn("[POT] credential refresh: URL re-resolve failed",
				"jobID", job.Job.ID, "tag", tag, "err", err)
		} else if freshSource := formatSourceByItag(formats, itag); tokenClassChanged(streamSource, freshSource) {
			// The re-fetch ran the full cascade, and the itag's dedup winner
			// changed since setup to a client whose GVS token requirement
			// differs — the token half above followed the SETUP client, so
			// installing this URL would break the (URL, token) pair. Keep
			// the current URL, the same way the whole-file guard above does.
			reason := "fresh client requires the GVS token, setup client does not"
			if youtube.GvsTokenRequired(streamSource) {
				reason = "setup client requires the GVS token, fresh client does not"
			}
			job.Logger.Warn("[POT] credential refresh: itag now served by a client of a different token class — keeping the current URL",
				"jobID", job.Job.ID, "tag", tag, "itag", itag, "setupSource", streamSource, "freshSource", freshSource, "reason", reason)
		} else {
			baseURL = fresh
		}
	}

	job.Logger.Info("[POT] credential refresh", "jobID", job.Job.ID, "tag", tag,
		"newURL", baseURL != "", "newToken", poToken != "")
	return baseURL, poToken
}

// formatBecameWholeFile reports whether the format matching itag in formats
// now carries a non-empty ContentLength — the same discriminator
// HasManifestlessDashFormats (strategy_youtube_manifestless_dash.go) uses at
// strategy-selection time to identify a complete-file format that is served
// by byte-range and never &sq=N-addressable. A live itag can cross that
// boundary mid-job: YouTube can finish VOD processing while a post-live tail
// is still downloading, at which point the same itag's OTF/live entry is
// replaced by a whole-file one. Returns false (safe to resolve) when the
// itag isn't present in formats at all; resolveFormatURLByItag's own
// not-found error handles that case.
func formatBecameWholeFile(formats []youtube.Format, itag int) bool {
	for i := range formats {
		if formats[i].Itag == itag {
			return formats[i].ContentLength != ""
		}
	}
	return false
}

// gvsBinding returns the content binding a GVS mint must use for this job,
// plus the label the provenance log reports. The value is resolved once
// during extraction (youtube.GvsContentBinding, mirroring yt-dlp's
// get_webpo_content_binding — experiment flag → videoID, authenticated →
// datasyncID, otherwise visitorData) and carried on VideoInfo, so every
// strategy asks the same question and gets the same answer.
//
// ACTIVE since 2026-08-16 — yt-dlp's rule is the golden standard. The
// interim visitorData-always policy (poTokenBinding, removed; see 10c2efd
// and git history) existed because the rule's first live trial stalled, but
// that stall reproduced on baseline and traced to the ANDROID_VR client
// ranking (e9d1388), exonerating the binding. visitorData-always survived
// only while YouTube wasn't enforcing bindings on GVS: the
// html5_generate_content_po_token experiment (verified ON, 2026-08-15) is
// exactly how enforcement rolls out, and authenticated sessions upstream
// bind datasyncID, which visitorData-always never did.
//
// The videoID fallback covers VideoInfo values that never passed through a
// watch-page fetch (probe-only paths); it matches what the experiment branch
// would have chosen and is never empty.
func gvsBinding(job *JobContext, videoInfo *youtube.VideoInfo) (value, kind string) {
	if videoInfo != nil && videoInfo.GvsBinding != "" {
		return videoInfo.GvsBinding, videoInfo.GvsBindingKind
	}
	return job.Job.VideoID, youtube.BindingVideoID
}

// mintGvsPoToken is every strategy's first GVS PO token mint (VOD, DASH, HLS,
// manifestless), behind a package var so the strategy tests can swap in a
// fake without a real *bgutils.PotProvider. Production never writes it. The
// 403-recovery re-mint (refreshGvsCredentials) goes through its own
// gvsTokenMinter interface and is not routed here; it is gated by the same
// youtube.GvsTokenRequired policy as every other mint.
var mintGvsPoToken = func(ctx context.Context, p *bgutils.PotProvider, binding string) (string, error) {
	return p.GeneratePoTokenString(ctx, binding, false)
}

// tokenClassChanged reports whether a 403 refresh must NOT install the fresh
// URL for a stream whose setup format came from setupSource and whose fresh
// format now comes from freshSource: whenever the two clients' GVS token
// requirements (youtube.GvsTokenRequired) differ. Both directions break the
// (URL, token) pair the engine holds:
//
//   - a bare setup → a URL that REQUIRES the token (visionos or tv_auth →
//     web_creator): the stream was left bare at setup and the refresh does
//     not mint for it, so the URL 403s until the refresh attempts run out.
//   - a tokenised setup → a URL that needs none (web_creator → tv_auth or
//     visionos): the engine keeps the token it was given (the refresh
//     callback cannot clear one) on a URL upstream fetches bare. For a tv
//     URL that is probably harmless, but android_vr was not (the 2026-08-15
//     403s), and the rule stays symmetric rather than betting on which bare
//     clients tolerate a stray token.
//
// A move within one class (web_creator → web, visionos ↔ tv_auth) installs.
func tokenClassChanged(setupSource, freshSource string) bool {
	return youtube.GvsTokenRequired(setupSource) != youtube.GvsTokenRequired(freshSource)
}

// refreshVideoInfo is the 403 refresh's player-response re-fetch
// ((*youtube.Service).GetVideoInfo) behind a package var so the refresh
// tests can hand refreshGvsCredentials a changed fresh pool — job.YT is the
// concrete *youtube.Service, with no other seam. Production never writes it.
var refreshVideoInfo = (*youtube.Service).GetVideoInfo

// formatSourceByItag returns the Format.Source of the first format carrying
// itag — the same first match resolveFormatURLByItag resolves the URL from —
// or "" when the pool has none. It is how the manifest-free strategy and its
// 403 refresh learn which client served a chosen stream: DashStreamInfo, the
// shape selection works on, does not carry the source.
//
// It reads the SETUP pool: on the VOD path a missing_pot swap replaces an
// itag's winner with its token-free shadow in a filtered copy only, so this
// would still name the winner's client for a stream riding the shadow's URL.
// DownloadVod wires no OnCredentialRefresh, so no caller reaches that shape;
// a VOD refresh, if ever added, must be handed the served format's Source.
func formatSourceByItag(formats []youtube.Format, itag int) string {
	for i := range formats {
		if formats[i].Itag == itag {
			return formats[i].Source
		}
	}
	return ""
}

// logGvsTokenSkipped records that a live strategy (or its 403 refresh)
// deliberately minted and attached NO GVS PO token because the URL came from
// a client youtube.GvsTokenRequired answers false for: tv, web_embedded,
// visionos and android_vr, whose URLs upstream fetches without a GVS token.
// A URL with no recorded client is treated the same way and says so
// ("unknown"), because every extraction site stamps one and a missing label
// is a bug to see in the log rather than a token to guess at.
func logGvsTokenSkipped(job *JobContext, source string, args ...any) {
	label, reason := source, "no GVS token required for this client"
	if source == "" {
		label, reason = "unknown", "source not recorded"
	}
	job.Logger.Info("[POT] GVS token skipped",
		append([]any{"jobID", job.Job.ID, "source", label, "reason", reason}, args...)...)
}
