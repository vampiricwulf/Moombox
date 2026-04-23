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

	// Apply n-parameter decryption — replace in-place to preserve param order.
	// Extract the raw query directly (split at the first '?') instead of via
	// url.Parse. url.Parse applies strict validation and has stumbled in the
	// past on decrypted signatures with reserved chars; we only need the raw
	// query for string matching, not full URL structure.
	rawQuery := ""
	if q := strings.Index(result, "?"); q >= 0 {
		rawQuery = result[q+1:]
	}
	// The raw (percent-encoded) form is what we must target for in-place
	// replacement. Query().Get() returns the decoded value which won't match
	// the raw URL when the value contains percent-encoded characters (e.g.,
	// %2F, %3D).
	rawN, decodedN := RawQueryParam(rawQuery, "n")
	nParam := req.NParam
	if nParam != "" {
		// Caller provided a pre-decoded n-param value for the solver. We still
		// use the raw form extracted from the URL for string matching, so the
		// replacement targets the correct (possibly percent-encoded) substring.
		decodedN = nParam
		if rawN == "" {
			rawN = url.QueryEscape(nParam)
		}
	} else {
		nParam = decodedN
	}
	if nParam != "" && rawN != "" && solvers.N != nil {
		decryptedN, err := solvers.DecryptN(nParam)
		if err != nil {
			return nil, fmt.Errorf("decrypt n-parameter: %w", err)
		}
		// Replace the first occurrence of n=<value> that is a proper query parameter
		for _, prefix := range []string{"?", "&"} {
			old := prefix + "n=" + rawN
			if strings.Contains(result, old) {
				result = strings.Replace(result, old, prefix+"n="+url.QueryEscape(decryptedN), 1)
				break
			}
		}
	}

	return &ResolveURLResponse{URL: result}, nil
}

// RawQueryParam extracts a query parameter's raw (percent-encoded) and decoded
// values from a raw query string. This is needed for accurate string replacement
// in URLs — url.Values.Get() returns only the decoded value, which may not match
// the raw URL when the value contains percent-encoded characters.
func RawQueryParam(rawQuery, key string) (raw, decoded string) {
	for part := range strings.SplitSeq(rawQuery, "&") {
		k, v, ok := strings.Cut(part, "=")
		if ok && k == key {
			decoded, _ = url.QueryUnescape(v)
			return v, decoded
		}
	}
	return "", ""
}
