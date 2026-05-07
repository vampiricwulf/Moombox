package worker

import (
	"context"
	"net/url"
	"regexp"
	"strings"

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

// vodStrategyT / dashStrategyT / hlsStrategyT — stateless adapters
// that delegate to the existing Download* functions. Kept stateless
// (zero-size) so the package-level singletons below cost nothing at
// runtime.
type vodStrategyT struct{}
type dashStrategyT struct{}
type manifestlessDashStrategyT struct{}
type hlsStrategyT struct{}

func (vodStrategyT) Kind() string                { return "vod" }
func (dashStrategyT) Kind() string               { return "dash" }
func (manifestlessDashStrategyT) Kind() string   { return "manifestless_dash" }
func (hlsStrategyT) Kind() string                { return "hls" }

func (vodStrategyT) Download(ctx context.Context, job *JobContext, info *youtube.VideoInfo, deps *StrategyDeps) (*DownloadResult, error) {
	return DownloadVod(ctx, job, info, deps.CipherSolver, deps.PotProvider)
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
	VodStrategy               DownloadStrategy = vodStrategyT{}
	DashStrategy              DownloadStrategy = dashStrategyT{}
	ManifestlessDashStrategy  DownloadStrategy = manifestlessDashStrategyT{}
	HlsStrategy               DownloadStrategy = hlsStrategyT{}
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
	job.Logger.Warn("[Cipher] "+tag+" 403 signal — invalidating solver and POT", "playerURL", playerURL)
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

// poTokenBinding returns the content binding value to pass to the PO token provider.
// Prefers visitorData (matches yt-dlp and bgutil-ytdlp-pot-provider upstream), falling
// back to ChannelID if visitor data extraction failed. Logs a warning on fallback so
// the underlying extraction issue is surfaced rather than silently degrading caching.
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
