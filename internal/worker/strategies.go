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
	playerID := cipher.PlayerIDFromURL(playerURL)
	if cipherSolver != nil {
		cipherSolver.InvalidateSolver(playerURL)
	}
	if potProvider != nil {
		potProvider.InvalidateCaches()
	}
	if job.YT != nil {
		// Clear the sig-route log dedup so the "sig decrypted via …" Info
		// fires once on recovery rather than staying silent.
		job.YT.PlayerAPI.ClearLoggedRoutes(playerID)
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

// credentialRefreshTimeout bounds refreshGvsCredentials' entire round trip
// (cipher URL re-resolve + GVS token re-mint). The engine calls
// OnCredentialRefresh SYNCHRONOUSLY on the download-loop goroutine and does
// NOT reset lastSegTime around it (internal/engine/downloader.go
// refreshCredentials) — an unbounded refresh would silently spend the
// operator's MaxTimeout stall budget on network calls instead of segment
// retries, defeating the recovery it exists to provide.
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
// This is NOT free at config.MaximumTimeout's validated 30s floor — a
// single refresh attempt at that setting can consume the whole stall
// budget on its own. That floor already leaves little room for any
// recovery path (sidecar round trips, cipher re-resolve) to complete, so
// it is called out here as a known limitation rather than solved: raising
// this constant would not meaningfully help a 30s-budget job, and lowering
// it would start truncating legitimate cold-path mints for everyone else.
const credentialRefreshTimeout = 45 * time.Second

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
// player response rather than refreshing the URL alone, because the PO
// token is the half that actually went stale.
//
// The whole call is bounded by credentialRefreshTimeout (derived from ctx)
// so a slow mint can't silently eat the caller's MaxTimeout stall budget —
// see that constant's doc for the reasoning.
//
// The content binding is captured BEFORE invalidate403Caches runs, not
// after. invalidate403Caches unconditionally clears job.YT's visitor data
// as one of its steps, and nothing else in this synchronous call
// repopulates it (no watch-page fetch happens here — only cipher URL
// resolution and a direct POT mint). Reading poTokenBinding after
// invalidation would therefore always observe empty visitor data and
// silently fall back to the channelID/videoID binding — swapping the
// GVS binding SCHEME mid-download as a side effect of cache-clearing,
// not a deliberate choice. The proven-good binding for this job is
// whatever poTokenBinding already resolved to before the 403; refreshing
// the TOKEN under that same binding (via bypassCache) is what "refresh
// credentials" means here, not migrating to the degraded fallback.
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
	tag string,
) (baseURL string, poToken string) {
	refreshCtx, cancel := context.WithTimeout(ctx, credentialRefreshTimeout)
	defer cancel()

	binding := poTokenBinding(job, videoInfo)

	invalidate403Caches(job, videoInfo.PlayerURL, cipherSolver, asPotProvider(potProvider), tag)

	if fresh, err := resolveFormatURLByItag(refreshCtx, videoInfo.Formats, itag, routedSolver, cipherSolver, videoInfo.PlayerURL, job.Logger); err != nil {
		job.Logger.Warn("[POT] credential refresh: URL re-resolve failed",
			"jobID", job.Job.ID, "tag", tag, "err", err)
	} else {
		baseURL = fresh
	}

	if minterUsable(potProvider) {
		// bypassCache: the cached token is the credential that just 403'd —
		// handing it back unchanged would make this refresh a no-op.
		if token, err := potProvider.GeneratePoTokenString(refreshCtx, binding, true); err != nil {
			job.Logger.Warn("[POT] credential refresh: re-mint failed",
				"jobID", job.Job.ID, "tag", tag, "err", err)
		} else {
			poToken = token
		}
	}

	job.Logger.Info("[POT] credential refresh", "jobID", job.Job.ID, "tag", tag,
		"newURL", baseURL != "", "newToken", poToken != "")
	return baseURL, poToken
}

// poTokenBinding returns the GVS content binding: visitorData, falling back
// to the channel ID when visitor-data extraction failed.
//
// RESTORED 2026-08-15. This is the binding every live capture through
// 2026-08-13 used, with zero segment 403s recorded. It was replaced by
// gvsBinding (yt-dlp's experiment-aware videoID/datasyncID/visitorData rule)
// on 2026-08-15, and the very next live capture stalled at segment ~72 under
// a 403 storm. gvsBinding is retained below for the staged reintroduction —
// it is upstream-correct on paper — but nothing calls it until one live
// stream has proven each variable in isolation.
func poTokenBinding(job *JobContext, videoInfo *youtube.VideoInfo) string {
	if job != nil && job.YT != nil {
		if vd := job.YT.GetVisitorData(); vd != "" {
			return vd
		}
	}
	if job != nil && job.Logger != nil {
		job.Logger.Warn("[POT] visitor data unavailable; falling back to channelID for content binding")
	}
	return videoInfo.ChannelID
}

// gvsBinding returns the content binding a GVS mint must use for this job,
// plus the label the provenance log reports. The value is resolved once
// during extraction (youtube.GvsContentBinding, mirroring yt-dlp's
// get_webpo_content_binding) and carried on VideoInfo, so every strategy asks
// the same question and gets the same answer.
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

// challengeLabel compresses a challenge value to the label the provenance
// log line reports: "page" (watch-page ytAtN challenge present) or "none".
//
// GVS binding is videoID (moonarchive parity). If premieres 403 again
// despite challenge-sourced minters, see GenerateGvsPoToken's doc comment —
// datasync-ID binding is the next suspect.
func challengeLabel(challenge string) string {
	if challenge != "" {
		return "page"
	}
	return "none"
}
