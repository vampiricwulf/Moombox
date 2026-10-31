package cipher

import (
	"context"
	"errors"
	"fmt"
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
	// Ahead of the player check, not behind it: see needsNoPlayer.
	if needsNoPlayer(req) {
		return &ResolveURLResponse{URL: req.StreamURL}, nil
	}
	if req.PlayerURL == "" {
		return nil, unsolvedIfNOnly(req, fmt.Errorf("player URL is required"))
	}

	solvers, err := s.GetSolvers(ctx, req.PlayerURL)
	if err != nil {
		return nil, unsolvedIfNOnly(req, fmt.Errorf("get solvers: %w", err))
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
	if nParam == "" && rawN != "" {
		return nil, fmt.Errorf("%w (player %s): %w", ErrNUnsolved, PlayerIDFromURL(req.PlayerURL), errNUndecodable)
	}
	if nParam != "" && rawN != "" {
		// A failure, like a missing sig solver above: an n left as YouTube
		// sent it is refused, not throttled (ErrNUnsolved says where that
		// premise went). It used to be a Warn and the URL as it came.
		playerID := PlayerIDFromURL(req.PlayerURL)
		if !solvers.HasN() {
			return nil, fmt.Errorf("%w (player %s): no n function in the player", ErrNUnsolved, playerID)
		}
		decryptedN, err := solvers.DecryptN(nParam)
		if err == nil {
			err = checkNResult(nParam, decryptedN)
		}
		if err != nil {
			return nil, fmt.Errorf("%w (player %s): decrypt n-parameter: %w", ErrNUnsolved, playerID, err)
		}
		var ok bool
		if result, ok = replaceQueryN(result, rawN, decryptedN); !ok {
			return nil, fmt.Errorf("%w (player %s): no n parameter in the URL to replace", ErrNUnsolved, playerID)
		}
	}

	return &ResolveURLResponse{URL: result}, nil
}

// checkNResult returns why out is no answer to the n challenge, or nil when
// it is one: an answer is not empty and does not end with the challenge —
// which also rules out the challenge itself, handed back unchanged. The
// suffix is the player's exception sentinel: an n function that throws
// returns from its catch block a string that ends with its own input
// ("enhanced_except_…_<challenge>"), which yt-dlp's
// validate_nsig_challenge_output rejects with "if they end with the input
// challenge then the js function returned with an exception"
// (jsc/_director.py:265-278, 6224a3898), and its bulk_solve records nothing
// for (_director.py:117-124). ejs itself returns whatever the function did
// (src/yt/solver/main.ts), and both of Moombox's solvers used to install it:
// the CDN refuses such an n as it refuses an unsolved one. Neither value is
// quoted: an n is one of the media-URL values redact.MediaURL cuts.
func checkNResult(challenge, out string) error {
	switch {
	case out == "":
		return errors.New("empty n result")
	case strings.HasSuffix(out, challenge):
		return errors.New("n result ends with its own challenge (exception sentinel, or the challenge unchanged)")
	}
	return nil
}

// errNUndecodable is why a query n whose raw value does not percent-decode
// is unsolved: RawQueryParam reads it as "", so there is no challenge to
// hand a solver, and the URL would go out with the n as YouTube sent it —
// refused, like any n left unsolved. All three entry points used to skip
// such an n and hand the URL back with a nil error. The value is not
// quoted: an n is one of the media-URL values redact.MediaURL cuts.
var errNUndecodable = errors.New("the n parameter does not percent-decode")

// replaceQueryN swaps the query's n=<raw> for n=<solved> in place, keeping
// the parameter order, and reports whether it did. yt-dlp checks the same of
// a manifest path (`solved_n = n_result in manifest_path`, _video.py:3699,
// 3737, d20f58d72): a solve whose answer never landed in the URL has solved
// nothing.
func replaceQueryN(rawURL, rawN, solved string) (string, bool) {
	for _, prefix := range []string{"?", "&"} {
		old := prefix + "n=" + rawN
		if strings.Contains(rawURL, old) {
			return strings.Replace(rawURL, old, prefix+"n="+url.QueryEscape(solved), 1), true
		}
	}
	return rawURL, false
}

// unsolvedIfNOnly marks err, which failed req before any solving began (no
// solver, no player URL, a player that would not compile), as ErrNUnsolved
// when an n is all req needed the player for: no encrypted signature rides it
// (needsNoPlayer has already handed back every URL that needs neither). A URL
// that also carries a signature fails over that first, and the error says so
// alone.
func unsolvedIfNOnly(req ResolveURLRequest, err error) error {
	if req.EncryptedSignature != "" {
		return err
	}
	return fmt.Errorf("%w: %w", ErrNUnsolved, err)
}

// needsNoPlayer reports whether req asks nothing of a player: no encrypted
// signature to append and no n to transform, neither handed in (NParam) nor
// in the URL's query, the one form both resolvers rewrite. Such a URL already
// is the fetchable one, and ResolveURL and RoutedResolveURL hand it back
// unchanged, whatever PlayerURL says and whether or not a solver is wired.
//
// yt-dlp asks no player about it either: _extract_formats solves a signature
// only `if encrypted_sig` and an n only `if query.get('n')`, so a direct URL
// with no n goes out as YouTube sent it (_video.py:3556-3579). Moombox failed
// it with "player URL is required" — RoutedResolveURL hands an empty
// PlayerURL straight to ResolveURL — so an extraction whose watch page took
// the player URL down with it failed every VOD and manifest-free resolve,
// cipher-free visionos and android_vr URLs included. With a player URL it
// compiled the player for nothing, and failed the URL when that compile did.
func needsNoPlayer(req ResolveURLRequest) bool {
	if req.EncryptedSignature != "" || req.NParam != "" {
		return false
	}
	rawQuery := ""
	if q := strings.Index(req.StreamURL, "?"); q >= 0 {
		rawQuery = req.StreamURL[q+1:]
	}
	rawN, _ := RawQueryParam(rawQuery, "n")
	return rawN == ""
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
