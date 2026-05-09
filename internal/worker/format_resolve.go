package worker

import (
	"context"
	"fmt"
	"sync"

	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// cipherMissingWarnedURLs deduplicates the "cipher needed but missing"
// warning per playerURL, so a session that probes the same player many
// times only logs once. This is the relocated version of the
// parse-phase cipherNeededButMissing warning that lived in
// parseFormatsWithCipher pre-Stage-3 — we now warn at the moment a
// strategy actually needs to resolve a sigCipher format and finds no
// solver wired, which is the right semantic point.
var cipherMissingWarnedURLs sync.Map // map[playerURL]struct{}

// warnLog calls logger.Warn when logger is non-nil. Wraps every
// logger.Warn call in this file so a nil JobContext.Logger doesn't
// panic — the alternative (typed nil-guard at every call site) is
// noisier.
func warnLog(logger interface {
	Warn(msg string, args ...any)
}, msg string, args ...any) {
	if logger != nil {
		logger.Warn(msg, args...)
	}
}

func infoLog(logger interface {
	Info(msg string, args ...any)
}, msg string, args ...any) {
	if logger != nil {
		logger.Info(msg, args...)
	}
}

// resolveFormatURLByItag finds the Format with matching itag in the
// pool, then runs cipher.ResolveFormatURL to produce a fetchable URL.
// Returns the resolved URL or an error.
//
// Strategies that reach a chosen format via an intermediate type
// (manifestless DASH builds DashStreamInfo from Format) should call
// this helper rather than piping EncryptedSig/SigKey through the
// intermediate. Strategies that already hold the *youtube.Format can
// call cipher.ResolveFormatURL directly.
//
// When the format requires sig decryption but no cipher solver is
// wired, emits a one-shot per-playerURL Warn and returns an error so
// the strategy bubbles up cleanly.
func resolveFormatURLByItag(
	ctx context.Context,
	formats []youtube.Format,
	itag int,
	routed cipher.Solver,
	goja *cipher.GojaResolver,
	playerURL string,
	logger interface {
		Warn(msg string, args ...any)
	},
) (string, error) {
	for i := range formats {
		if formats[i].Itag != itag {
			continue
		}
		f := &formats[i]
		if f.EncryptedSig != "" && routed == nil && goja == nil {
			if _, loaded := cipherMissingWarnedURLs.LoadOrStore(playerURL, struct{}{}); !loaded {
				warnLog(logger, "[Cipher] format requires sig decryption but no cipher solver is wired",
					"playerURL", playerURL, "itag", itag)
			}
			return "", fmt.Errorf("cipher: format %d requires sig but no solver wired", itag)
		}
		return cipher.ResolveFormatURL(ctx, routed, goja, playerURL, cipher.ResolveFormatRequest{
			URL:          f.URL,
			EncryptedSig: f.EncryptedSig,
			SigKey:       f.SigKey,
		})
	}
	return "", fmt.Errorf("itag %d not found in format pool", itag)
}

// resolveManifestlessStream wraps resolveFormatURLByItag with bounded
// re-selection: on resolve failure for the chosen DashStreamInfo,
// excludes its itag and runs SelectBestDashStream once more before
// giving up. Returns (resolvedURL, replacementStream, err).
// replacementStream is non-nil only when the second selection picked a
// different stream than the original — caller substitutes it.
//
// In practice cipher failures are systemic (works for all formats from
// a player or none), so the re-selection rarely fires; this is a hedge
// against the rare case where one specific URL is broken while
// siblings work.
func resolveManifestlessStream(
	ctx context.Context,
	formatPool []youtube.Format,
	candidates []DashStreamInfo,
	chosen *DashStreamInfo,
	preferItag int,
	maxRes int,
	isVideo bool,
	qualityPref string,
	routed cipher.Solver,
	goja *cipher.GojaResolver,
	playerURL string,
	excluded map[int]bool,
	logger interface {
		Warn(msg string, args ...any)
		Info(msg string, args ...any)
	},
) (string, *DashStreamInfo, error) {
	resolved, err := resolveFormatURLByItag(ctx, formatPool, chosen.Itag, routed, goja, playerURL, logger)
	if err == nil {
		return resolved, nil, nil
	}

	warnLog(logger, "[Cipher] format resolve failed; attempting re-selection",
		"itag", chosen.Itag, "err", err)
	excluded[chosen.Itag] = true

	// Filter candidates excluding the broken itag + any prior failures.
	filtered := make([]DashStreamInfo, 0, len(candidates))
	for i := range candidates {
		if excluded[candidates[i].Itag] {
			continue
		}
		filtered = append(filtered, candidates[i])
	}
	if len(filtered) == 0 {
		return "", nil, fmt.Errorf("no remaining candidates after excluding itag %d", chosen.Itag)
	}

	retry := SelectBestDashStream(filtered, preferItag, maxRes, isVideo, qualityPref)
	if retry == nil {
		return "", nil, fmt.Errorf("re-selection produced no candidate after excluding itag %d", chosen.Itag)
	}

	resolved, err = resolveFormatURLByItag(ctx, formatPool, retry.Itag, routed, goja, playerURL, logger)
	if err != nil {
		return "", nil, fmt.Errorf("re-selection itag %d also failed to resolve: %w", retry.Itag, err)
	}
	infoLog(logger, "[Cipher] re-selection succeeded", "originalItag", chosen.Itag, "newItag", retry.Itag)
	return resolved, retry, nil
}

// pickAlternateVodFormat picks the next-best video or audio format from
// the pool, excluding the previously-chosen itag. Used as the
// re-selection hedge in DownloadVod when cipher resolution fails on
// the primary chosen format. Picks by bitrate as a simple "next-best"
// heuristic — ranking-aware re-selection would mean refactoring
// SelectBestFormats to accept an exclusion set, which isn't worth the
// surface area for what should be a cold path.
func pickAlternateVodFormat(pool []youtube.Format, isVideo bool, excludeItag int) *youtube.Format {
	var best *youtube.Format
	for i := range pool {
		f := &pool[i]
		if f.Itag == excludeItag || f.URL == "" {
			continue
		}
		if isVideo && !f.IsVideo() {
			continue
		}
		if !isVideo && !f.IsAudio() {
			continue
		}
		if best == nil || f.Bitrate > best.Bitrate {
			best = f
		}
	}
	return best
}

// resolveFormatURL is the non-itag-lookup variant for strategies that
// already hold the chosen *youtube.Format (e.g. VOD).
func resolveFormatURL(
	ctx context.Context,
	f *youtube.Format,
	routed cipher.Solver,
	goja *cipher.GojaResolver,
	playerURL string,
	logger interface {
		Warn(msg string, args ...any)
	},
) (string, error) {
	if f == nil {
		return "", fmt.Errorf("resolve format URL: nil format")
	}
	if f.EncryptedSig != "" && routed == nil && goja == nil {
		if _, loaded := cipherMissingWarnedURLs.LoadOrStore(playerURL, struct{}{}); !loaded {
			warnLog(logger, "[Cipher] format requires sig decryption but no cipher solver is wired",
				"playerURL", playerURL, "itag", f.Itag)
		}
		return "", fmt.Errorf("cipher: format %d requires sig but no solver wired", f.Itag)
	}
	return cipher.ResolveFormatURL(ctx, routed, goja, playerURL, cipher.ResolveFormatRequest{
		URL:          f.URL,
		EncryptedSig: f.EncryptedSig,
		SigKey:       f.SigKey,
	})
}
