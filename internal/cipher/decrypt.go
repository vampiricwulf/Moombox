package cipher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// DecryptSignature decrypts the signature and n-parameter for a stream URL.
func (s *GojaResolver) DecryptSignature(ctx context.Context, req SignatureRequest) (*SignatureResponse, error) {
	if req.PlayerURL == "" {
		return nil, fmt.Errorf("player URL: %w", ErrInputRequired)
	}

	solvers, err := s.GetSolvers(ctx, req.PlayerURL)
	if err != nil {
		return nil, fmt.Errorf("get solvers: %w", err)
	}

	resp := &SignatureResponse{}

	// Decrypt signature
	if req.EncryptedSignature != "" && solvers.Sig != nil {
		decrypted, err := solvers.DecryptSig(req.EncryptedSignature)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSigDecrypt, err)
		}
		resp.DecryptedSignature = decrypted
	}

	// Decrypt n-parameter
	if req.NParam != "" && solvers.N != nil {
		decrypted, err := solvers.DecryptN(req.NParam)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNDecrypt, err)
		}
		resp.DecryptedNSig = decrypted
	}

	return resp, nil
}

// RoutedResolveURL applies sig and n-param decryption to a stream URL, routing
// through the composite Solver first (sidecar primary; goja n-fallback; sig is
// sidecar-only). Falls back to gojaResolver.ResolveURL on composite error so
// that the legacy goja path still handles n when the routed solver is
// unavailable.
//
// This is the worker-side equivalent of PlayerAPI.decryptN:
// it keeps the worker's URL-resolution call sites routing through the sidecar
// on cb017549-family players where the goja extractor cannot produce a sig.
//
// Pass a nil routedSolver to skip the routed path and use gojaResolver directly
// (matches prior behaviour on startup before the sidecar is healthy).
//
// An n in the query that no solver answers fails the URL with ErrNUnsolved,
// as a signature no solver answers always has; see solveN for what counts as
// an answer.
func RoutedResolveURL(ctx context.Context, routedSolver Solver, gojaResolver *GojaResolver, req ResolveURLRequest) (*ResolveURLResponse, error) {
	// A URL that asks nothing of a player resolves to itself, ahead of the
	// solver and player checks below — neither is needed for it, and the
	// fall-through used to fail it on an empty PlayerURL (needsNoPlayer).
	if req.StreamURL != "" && needsNoPlayer(req) {
		return &ResolveURLResponse{URL: req.StreamURL}, nil
	}
	if routedSolver == nil || req.PlayerURL == "" {
		if gojaResolver == nil {
			return nil, unsolvedIfNOnly(req, fmt.Errorf("cipher: no solver available"))
		}
		return gojaResolver.ResolveURL(ctx, req)
	}

	result := req.StreamURL
	playerID := PlayerIDFromURL(req.PlayerURL)

	// --- Sig decryption (sidecar-primary; no goja fallback for sig) ---
	if req.EncryptedSignature != "" {
		decryptedSig, err := routedSolver.Sig(ctx, playerID, req.EncryptedSignature)
		if err != nil {
			// Sig via routed solver failed — fall through to full legacy path
			// which also handles n-param so we don't partial-apply.
			if gojaResolver == nil {
				return nil, fmt.Errorf("cipher: routed sig failed and no goja fallback: %w", err)
			}
			resp, gojaErr := gojaResolver.ResolveURL(ctx, req)
			if gojaErr != nil {
				// Both paths failed. On its own the goja error reads the same
				// whether the sidecar is down, the player is stale or ejs
				// found no solution ("no sig solver available for player
				// …"), so the routed cause rides along — and stays the one
				// errors.Is sees (ErrPlayerJSStale, ErrSidecarUnavailable).
				return nil, fmt.Errorf("cipher: routed sig: %w (goja fallback: %v)", err, gojaErr)
			}
			return resp, nil
		}
		sigKey := req.SignatureKey
		if sigKey == "" {
			sigKey = "sig"
		}
		sep := "&"
		if !strings.Contains(result, "?") {
			sep = "?"
		}
		result = result + sep + url.QueryEscape(sigKey) + "=" + url.QueryEscape(decryptedSig)
	}

	// --- N-param decryption (sidecar-primary; goja fallback) ---
	rawQuery := ""
	if q := strings.Index(result, "?"); q >= 0 {
		rawQuery = result[q+1:]
	}
	rawN, decodedN := RawQueryParam(rawQuery, "n")
	nParam := req.NParam
	if nParam != "" {
		decodedN = nParam
		if rawN == "" {
			rawN = url.QueryEscape(nParam)
		}
	} else {
		nParam = decodedN
	}
	if nParam != "" && rawN != "" {
		decryptedN, err := solveN(ctx, routedSolver, gojaResolver, req.PlayerURL, playerID, nParam)
		if err != nil {
			// An n left as YouTube sent it is refused, not throttled: this
			// used to be a Warn and the URL as it came (ErrNUnsolved).
			return nil, fmt.Errorf("%w (player %s): %w", ErrNUnsolved, playerID, err)
		}
		var ok bool
		if result, ok = replaceQueryN(result, rawN, decryptedN); !ok {
			return nil, fmt.Errorf("%w (player %s): no n parameter in the URL to replace", ErrNUnsolved, playerID)
		}
	}

	return &ResolveURLResponse{URL: result}, nil
}

// solveN answers one n challenge for RoutedResolveURL and RoutedDecryptNInURL:
// the routed solver first, then the goja resolver's own compile of playerURL,
// the next tried whenever one fails or answers with what checkNResult says is
// no answer. yt-dlp's director moves to its next provider on an invalid
// response the same way and records nothing for it (jsc/_director.py:105-124,
// 6224a3898). Before, a routed answer was installed whatever it was, and the
// goja one too.
//
// The error names every path that failed: a dead sidecar, a stale player and
// ejs's "no solutions" otherwise read alike. The routed cause stays the
// wrapped one (ErrPlayerJSStale, ErrSidecarUnavailable). The callers wrap it
// in ErrNUnsolved.
func solveN(ctx context.Context, routedSolver Solver, gojaResolver *GojaResolver, playerURL, playerID, challenge string) (string, error) {
	var routedErr, gojaErr error
	if routedSolver != nil {
		dec, err := routedSolver.N(ctx, playerID, challenge)
		if err == nil {
			err = checkNResult(challenge, dec)
		}
		if err == nil {
			return dec, nil
		}
		routedErr = err
		slog.Debug("cipher: routed n-param decryption failed, trying goja",
			"player", playerID, "err", err)
	}
	if gojaResolver != nil {
		solvers, err := gojaResolver.GetSolvers(ctx, playerURL)
		switch {
		case err != nil:
			gojaErr = err
		case !solvers.HasN():
			gojaErr = fmt.Errorf("n unavailable for player %s", playerID)
		default:
			dec, err := solvers.DecryptN(challenge)
			if err == nil {
				err = checkNResult(challenge, dec)
			}
			if err == nil {
				return dec, nil
			}
			gojaErr = err
		}
	}
	switch {
	case routedErr != nil && gojaErr != nil:
		return "", fmt.Errorf("routed n: %w (goja fallback: %v)", routedErr, gojaErr)
	case routedErr != nil:
		return "", fmt.Errorf("routed n: %w (no goja fallback)", routedErr)
	case gojaErr != nil:
		return "", fmt.Errorf("goja n: %w", gojaErr)
	default:
		return "", errors.New("no solver to ask")
	}
}

// RoutedDecryptNInURL solves the n challenge of rawURL in both forms a
// googlevideo URL carries one, the path's /n/<value>/ (at least 10
// characters) and the query's n=, through solveN (the composite Solver
// first, the goja resolver after it), and returns the URL with each answer in
// its challenge's place. A URL that carries no n comes back unchanged with a
// nil error, whatever the player and solvers.
//
// An n it cannot answer fails the URL: the error wraps ErrNUnsolved and the
// URL is "". That is no player URL to solve with (no solver is asked), no
// solver, or every solver failing or answering with no answer (checkNResult).
// It used to hand the URL back unchanged with a Warn that downloads "may be
// throttled", and its callers downloaded segments that 403. yt-dlp uses a
// manifest only once its path n is solved and the answer is in the path
// (`solved_n = n_result in manifest_path`, _video.py:3699, 3737, d20f58d72);
// here both forms' answers land by construction, the challenge replaced
// being the one just read out of the URL.
//
// playerURL is the player.js URL (used for PlayerIDFromURL and the goja
// GetSolvers cache key).  Pass a nil routedSolver to use goja directly.
func RoutedDecryptNInURL(ctx context.Context, routedSolver Solver, gojaResolver *GojaResolver, playerURL, rawURL string) (string, error) {
	if rawURL == "" {
		return rawURL, nil
	}
	solve := func(challenge, form string) (string, error) {
		if playerURL == "" {
			return "", fmt.Errorf("%w (%s form): no player URL to solve it with", ErrNUnsolved, form)
		}
		// Not before the check above: PlayerIDFromURL warns of a URL it
		// cannot read an id from, "" included, and its callers now ask with
		// no player URL too (a URL with no n needs none).
		playerID := PlayerIDFromURL(playerURL)
		dec, err := solveN(ctx, routedSolver, gojaResolver, playerURL, playerID, challenge)
		if err != nil {
			return "", fmt.Errorf("%w (player %s, %s form): %w", ErrNUnsolved, playerID, form, err)
		}
		return dec, nil
	}

	result := rawURL

	// Path-encoded n: /n/{value}/
	const pathPrefix = "/n/"
	if idx := strings.Index(result, pathPrefix); idx >= 0 {
		rest := result[idx+len(pathPrefix):]
		if encryptedN, _, ok := strings.Cut(rest, "/"); ok && len(encryptedN) >= 10 {
			dec, err := solve(encryptedN, "path")
			if err != nil {
				return "", err
			}
			// Always lands: the segment replaced is the one just found.
			result = strings.Replace(result, pathPrefix+encryptedN+"/", pathPrefix+dec+"/", 1)
		}
	}

	// Query-string n param.
	rawQuery := ""
	if q := strings.Index(result, "?"); q >= 0 {
		rawQuery = result[q+1:]
	}
	rawN, decodedN := RawQueryParam(rawQuery, "n")
	if rawN != "" && decodedN != "" {
		dec, err := solve(decodedN, "query")
		if err != nil {
			return "", err
		}
		// Always lands too: rawN was read from this URL's own query, where
		// it follows a "?" or a "&". RoutedResolveURL's n can come from
		// elsewhere (NParam), and there a miss is checked.
		result, _ = replaceQueryN(result, rawN, dec)
	}

	return result, nil
}

// ResolveFormatRequest carries the parsed components of a YouTube Format
// that ResolveFormatURL needs to produce a fetchable URL. URL is the raw
// stream URL (either a direct URL from streamingData.formats[].url or
// the `url=` value parsed out of a signatureCipher entry).
// EncryptedSig + SigKey describe the sigCipher contribution; both are
// empty when the format shipped with a direct URL.
type ResolveFormatRequest struct {
	URL          string
	EncryptedSig string
	SigKey       string // defaults to "signature" when empty
}

// ResolveFormatURL turns a parsed YouTube Format into a fetchable URL by
// combining sig-decryption (when EncryptedSig is set) with n-param
// decryption (always, since YouTube format URLs always carry an
// encrypted n).
//
// This was previously done eagerly in parseFormatsWithCipher for every
// format in the response (7-26 calls per stream setup); deferring to
// post-selection means only the chosen format(s) pay the cost. See
// docs/plans/cipher-pipeline-rework.md Stage 3.
//
// On error the caller (worker strategy) should mark this format as
// broken in its in-memory exclusion set and re-run selection. This
// function is stateless — it has no knowledge of what alternatives
// exist or what "broken" means in the strategy context.
func ResolveFormatURL(ctx context.Context, routedSolver Solver, gojaResolver *GojaResolver, playerURL string, req ResolveFormatRequest) (string, error) {
	if req.URL == "" {
		return "", fmt.Errorf("cipher: resolve format: empty URL")
	}
	urlReq := ResolveURLRequest{
		StreamURL:          req.URL,
		PlayerURL:          playerURL,
		EncryptedSignature: req.EncryptedSig,
		SignatureKey:       req.SigKey,
	}
	if req.EncryptedSig != "" && urlReq.SignatureKey == "" {
		urlReq.SignatureKey = "signature"
	}
	resp, err := RoutedResolveURL(ctx, routedSolver, gojaResolver, urlReq)
	if err != nil {
		return "", err
	}
	return resp.URL, nil
}

// GetSts returns the signatureTimestamp for a player URL.
func (s *GojaResolver) GetSts(ctx context.Context, playerURL string) (string, error) {
	if s.stsCache == nil {
		s.stsCache = NewStsCache()
	}
	return s.stsCache.GetSts(ctx, s.playerCache, playerURL)
}
