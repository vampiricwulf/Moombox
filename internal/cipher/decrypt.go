package cipher

import (
	"context"
	"fmt"
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
// This is the worker-side equivalent of PlayerAPI.decryptSig / decryptN:
// it keeps the worker's URL-resolution call sites routing through the sidecar
// on cb017549-family players where the goja extractor cannot produce a sig.
//
// Pass a nil routedSolver to skip the routed path and use gojaResolver directly
// (matches prior behaviour on startup before the sidecar is healthy).
func RoutedResolveURL(ctx context.Context, routedSolver Solver, gojaResolver *GojaResolver, req ResolveURLRequest) (*ResolveURLResponse, error) {
	if routedSolver == nil || req.PlayerURL == "" {
		if gojaResolver == nil {
			return nil, fmt.Errorf("cipher: no solver available")
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
			return gojaResolver.ResolveURL(ctx, req)
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
		decryptedN, err := routedSolver.N(ctx, playerID, nParam)
		if err != nil {
			// Routed N failed — try goja legacy for this param only.
			if gojaResolver != nil {
				solvers, solverErr := gojaResolver.GetSolvers(ctx, req.PlayerURL)
				if solverErr == nil && solvers.HasN() {
					decryptedN, err = solvers.DecryptN(nParam)
				}
			}
		}
		if err == nil && decryptedN != nParam {
			for _, prefix := range []string{"?", "&"} {
				old := prefix + "n=" + rawN
				if strings.Contains(result, old) {
					result = strings.Replace(result, old, prefix+"n="+url.QueryEscape(decryptedN), 1)
					break
				}
			}
		}
	}

	return &ResolveURLResponse{URL: result}, nil
}

// RoutedDecryptNInURL decrypts the n-parameter (query-string and path forms)
// in rawURL, routing through the composite Solver first, falling back to the
// goja resolver on error.  Returns the mutated URL; on a total failure the
// original rawURL is returned unchanged (matching the pre-existing silent-fail
// behaviour of decryptNParamInURL in the worker).
//
// playerURL is the player.js URL (used for PlayerIDFromURL and the goja
// GetSolvers cache key).  Pass a nil routedSolver to use goja directly.
func RoutedDecryptNInURL(ctx context.Context, routedSolver Solver, gojaResolver *GojaResolver, playerURL, rawURL string) string {
	if rawURL == "" {
		return rawURL
	}
	playerID := PlayerIDFromURL(playerURL)

	// decryptN attempts to decrypt with routed solver then goja fallback.
	decryptFn := func(encrypted string) (string, error) {
		if routedSolver != nil {
			if dec, err := routedSolver.N(ctx, playerID, encrypted); err == nil {
				return dec, nil
			}
		}
		if gojaResolver != nil {
			solvers, err := gojaResolver.GetSolvers(ctx, playerURL)
			if err == nil && solvers.HasN() {
				return solvers.DecryptN(encrypted)
			}
		}
		return encrypted, fmt.Errorf("cipher: n decrypt unavailable for player %s", playerID)
	}

	result := rawURL

	// Path-encoded n: /n/{value}/
	const pathPrefix = "/n/"
	if idx := strings.Index(result, pathPrefix); idx >= 0 {
		rest := result[idx+len(pathPrefix):]
		if end := strings.Index(rest, "/"); end >= 0 {
			encryptedN := rest[:end]
			if len(encryptedN) >= 10 {
				if dec, err := decryptFn(encryptedN); err == nil && dec != encryptedN {
					result = strings.Replace(result, pathPrefix+encryptedN+"/", pathPrefix+dec+"/", 1)
				}
			}
		}
	}

	// Query-string n param.
	rawQuery := ""
	if q := strings.Index(result, "?"); q >= 0 {
		rawQuery = result[q+1:]
	}
	rawN, decodedN := RawQueryParam(rawQuery, "n")
	if rawN != "" && decodedN != "" {
		if dec, err := decryptFn(decodedN); err == nil && dec != decodedN {
			for _, prefix := range []string{"?", "&"} {
				old := prefix + "n=" + rawN
				if strings.Contains(result, old) {
					result = strings.Replace(result, old, prefix+"n="+url.QueryEscape(dec), 1)
					break
				}
			}
		}
	}

	return result
}

// GetSts returns the signatureTimestamp for a player URL.
func (s *GojaResolver) GetSts(ctx context.Context, playerURL string) (string, error) {
	if s.stsCache == nil {
		s.stsCache = NewStsCache()
	}
	return s.stsCache.GetSts(ctx, s.playerCache, playerURL)
}
