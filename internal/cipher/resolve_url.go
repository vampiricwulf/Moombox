package cipher

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// ResolveURL applies signature and n-parameter decryption to a stream URL.
// Uses string manipulation to preserve original URL parameter order —
// Go's url.Values.Encode() sorts parameters alphabetically, which breaks
// YouTube's URL signature verification and causes HTTP 403.
func (s *GojaResolver) ResolveURL(ctx context.Context, req ResolveURLRequest) (*ResolveURLResponse, error) {
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
	if req.EncryptedSignature != "" {
		if solvers.Sig == nil {
			// A format that ships an encrypted signature is unusable without
			// it — the CDN deterministically 403s an unsigned URL. Failing
			// here gives the caller an actionable error (invalidate / retry /
			// exclude format) instead of silently returning a URL that turns
			// into downstream 403 churn. Modern players routinely defeat the
			// goja sig extractor, so this fires whenever the sidecar route is
			// unavailable for a sig-bearing format.
			return nil, fmt.Errorf("decrypt signature: no sig solver available for player %s", PlayerIDFromURL(req.PlayerURL))
		}
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
	if nParam != "" && rawN != "" && solvers.N == nil {
		// Deliberate degrade, not a failure: unlike a missing sig solver
		// (hard error above — an unsigned URL 403s), an encrypted n still
		// serves; YouTube just throttles it. Surface the degrade, or the
		// only symptom is mysteriously slow downloads. Via slog.Default
		// (bridged into the full logger pipeline) — this path predates the
		// struct having any logger.
		slog.Warn("cipher: no n-param solver — leaving n encrypted, downloads may be throttled",
			"player", PlayerIDFromURL(req.PlayerURL))
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
