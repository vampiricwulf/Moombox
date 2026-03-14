package cipher

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// ResolveURL applies signature and n-parameter decryption to a stream URL.
// Uses string manipulation to preserve original URL parameter order —
// Go's url.Values.Encode() sorts parameters alphabetically, which breaks
// YouTube's URL signature verification and causes HTTP 403.
func (s *Solver) ResolveURL(ctx context.Context, req ResolveURLRequest) (*ResolveURLResponse, error) {
	if req.StreamURL == "" {
		return nil, fmt.Errorf("stream URL is required")
	}
	if req.PlayerURL == "" {
		return nil, fmt.Errorf("player URL is required")
	}

	solvers, err := s.GetSolvers(ctx, req.PlayerURL)
	if err != nil {
		return nil, fmt.Errorf("get solvers: %w", err)
	}

	result := req.StreamURL

	// Apply signature decryption — append decrypted sig as new query param
	if req.EncryptedSignature != "" && solvers.Sig != nil {
		decryptedSig, err := solvers.DecryptSig(req.EncryptedSignature)
		if err != nil {
			return nil, fmt.Errorf("decrypt signature: %w", err)
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

	// Apply n-parameter decryption — replace in-place to preserve param order
	parsed, err := url.Parse(result)
	if err != nil {
		return nil, fmt.Errorf("parse stream URL: %w", err)
	}
	nParam := req.NParam
	if nParam == "" {
		nParam = parsed.Query().Get("n")
	}
	if nParam != "" && solvers.N != nil {
		decryptedN, err := solvers.DecryptN(nParam)
		if err != nil {
			return nil, fmt.Errorf("decrypt n-parameter: %w", err)
		}
		// Replace the first occurrence of n=<value> that is a proper query parameter
		for _, prefix := range []string{"?", "&"} {
			old := prefix + "n=" + nParam
			if strings.Contains(result, old) {
				result = strings.Replace(result, old, prefix+"n="+decryptedN, 1)
				break
			}
		}
	}

	return &ResolveURLResponse{URL: result}, nil
}
