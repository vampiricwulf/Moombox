package cipher

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// TVPlayerSource is the optional half of a PlayerSource that can also yield
// the tv body of a player (tvVariantPath), for the sidecar solver's retry
// when ejs cannot solve with the main one. A source without it gets no tv
// retry. *GojaResolver is the production one.
type TVPlayerSource interface {
	TVPlayerJS(ctx context.Context, playerID string) (string, error)
}

// TVPlayerJS satisfies TVPlayerSource. It does not go through the resolver's
// player cache: see fetchTVPlayerJS.
func (s *GojaResolver) TVPlayerJS(ctx context.Context, playerID string) (string, error) {
	return fetchTVPlayerJS(ctx, playerID)
}

// fetchTVPlayerJS downloads the tv body of a player and hands it back. It
// keeps nothing: not in memory, and not in PlayerCache, which this function
// never touches. That cache keys a player by its id alone and replays the
// stored Last-Modified, and the main and tv files of one player carry the
// same Last-Modified and no ETag — a tv fetch through it would be answered
// 304 and handed the cached main body, or would overwrite it (audit G50
// again). The sidecar keeps the prepared tv copy under its own key
// (tvKeySuffix), in memory and in its store, so this runs about once per
// player that needs it.
//
// One GET with no validators, bounded by ctx and the client's 30s. A failure
// wraps ErrPlayerJSFetch, with the HTTP status when there was an answer.
func fetchTVPlayerJS(ctx context.Context, playerID string) (string, error) {
	tvURL := playerVariantURL(playerID, tvVariantPath)
	// A player id that is not one (PlayerIDFromURL's hash of a URL it could
	// not read) names no tv file.
	if PlayerIDOf(tvURL) != playerID {
		return "", fmt.Errorf("%w: %q is not a player id, so it has no tv variant", ErrPlayerJSFetch, playerID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tvURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrPlayerJSFetch, err)
	}
	req.Header.Set("User-Agent", playerFetchUserAgent)
	resp, err := playerHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrPlayerJSFetch, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: %w", ErrPlayerJSFetch, playerFetchStatus(resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, playerJSMaxBytes))
	if err != nil {
		return "", fmt.Errorf("%w: read body: %v", ErrPlayerJSFetch, err)
	}
	if len(body) == 0 {
		return "", fmt.Errorf("%w: empty body", ErrPlayerJSFetch)
	}
	return string(body), nil
}

// Compile-time check: GojaResolver can yield the tv body.
var _ TVPlayerSource = (*GojaResolver)(nil)
