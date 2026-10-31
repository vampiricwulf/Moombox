package youtube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// pageGet is utils.FetchWithTimeout behind a package var so the retry tests
// can serve fetchYouTubePage's requests in-process (httpxtest), inside a
// synctest bubble whose fake clock runs the backoff. Production never writes
// it.
var pageGet = utils.FetchWithTimeout

// pageFetchAttempts is how many times fetchYouTubePage asks for a page: one
// try and three retries, yt-dlp's extractor_retries default (options.py:
// --extractor-retries default=3; RetryManager runs attempts 1..retries+1).
const pageFetchAttempts = 4

// pageRetryBackoffBase is the first wait between two attempts; attempt n
// waits base<<(n-1), 1 s, 2 s, 4 s — doRetryRequest's ladder, which the
// player calls behind the page already take. yt-dlp sleeps nothing between
// extractor retries unless --retry-sleep extractor: is given; Moombox waits,
// because the blips this exists for (a 5xx, a reset at go-live) are the kind
// a second later outlasts, and the deadline guard keeps the wait off any
// caller that cannot afford it.
const pageRetryBackoffBase = time.Second

// pageStatusError is a page fetch answered with a status outside 2xx. Its
// text is the one FetchWatchPage has always returned for it.
type pageStatusError struct {
	what   string // "watch page", "iframe API"
	status int
	url    string
}

func (e *pageStatusError) Error() string {
	return fmt.Sprintf("failed to fetch %s: HTTP %d from %s", e.what, e.status, e.url)
}

// consentWallError is a page fetch whose redirects ended on the EU consent
// interstitial (isConsentRedirect).
type consentWallError struct {
	what string
	host string
}

func (e *consentWallError) Error() string {
	return fmt.Sprintf("%s redirected to consent wall (%s); supply CONSENT cookies via a cookie file", e.what, e.host)
}

// pageRetryable is yt-dlp's _download_webpage_with_retries rule
// (_base.py:1001-1011, d48e61260 / #12339): every network error is retried
// except an HTTP 403 or 429. A transport error, a timeout, a body cut off
// mid-read and every other status (a 5xx, and a 404 too, as upstream) get
// another attempt; a 403 or 429 is YouTube refusing this session, which the
// same request a second later would only be refused again for — and a 429
// retried is more of what earned it.
//
// The consent interstitial is never retried either. Upstream never sees one
// as an error at all (it sends SOCS up front, _initialize_consent); here it
// is the cookie jar's answer, and the next attempt carries the same jar.
func pageRetryable(err error) bool {
	if _, ok := errors.AsType[*consentWallError](err); ok {
		return false
	}
	if se, ok := errors.AsType[*pageStatusError](err); ok {
		return se.status != http.StatusForbidden && se.status != http.StatusTooManyRequests
	}
	return true
}

// fetchYouTubePage GETs a YouTube page and returns its body, retrying what
// pageRetryable allows up to pageFetchAttempts times — yt-dlp's
// _download_webpage_with_retries, which serves the watch page
// (_video.py:3815-3819) and the iframe API (_video.py:2162-2165) alike.
// Each attempt has timeout to itself; body reads at most maxBytes, silently
// truncated past that as the watch page's read always was. what names the
// page in the errors.
//
// The backoff is bounded by the CALLER's deadline the way doRetryRequest's
// is, and for the same reason: mid-download 403 credential recovery runs a
// whole extraction under min(45 s, MaxTimeout/3), as little as 10 s, and a
// ladder that slept through it would turn the page's own error into
// context.DeadlineExceeded. A wait that would not leave pageRetryBackoffBase
// for the attempt after it is not taken, and the last attempt's error is
// returned. A cancelled context ends the loop with its own error
// (retryExitErr); one that lapsed keeps the page's. The final error says how
// many attempts it took when there was more than one.
func fetchYouTubePage(ctx context.Context, what, pageURL string, timeout time.Duration, headers map[string]string, maxBytes int64) ([]byte, error) {
	var lastErr error
	attempts := 0
	for attempt := range pageFetchAttempts {
		if err := ctx.Err(); err != nil {
			return nil, retryExitErr(err, lastErr)
		}
		if attempt > 0 {
			delay := pageRetryBackoffBase << (attempt - 1)
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay+pageRetryBackoffBase {
				break
			}
			if err := utils.Sleep(ctx, delay); err != nil {
				return nil, retryExitErr(err, lastErr)
			}
		}
		attempts++
		body, err := fetchYouTubePageOnce(ctx, what, pageURL, timeout, headers, maxBytes)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !pageRetryable(err) {
			return nil, err
		}
	}
	if attempts > 1 {
		return nil, fmt.Errorf("%w (%d attempts)", lastErr, attempts)
	}
	return nil, lastErr
}

// fetchYouTubePageOnce is one attempt of fetchYouTubePage.
//
// pageGet (utils.FetchWithTimeout, not FetchBody) so the FINAL post-redirect
// URL is inspectable — the consent interstitial answers 200, so status-code
// checks alone can never see it.
func fetchYouTubePageOnce(ctx context.Context, what, pageURL string, timeout time.Duration, headers map[string]string, maxBytes int64) ([]byte, error) {
	resp, cancel, err := pageGet(ctx, pageURL, timeout, headers)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch %s: %w", what, err)
	}
	defer cancel()
	defer resp.Body.Close()

	if isConsentRedirect(resp) {
		io.Copy(io.Discard, resp.Body)
		return nil, &consentWallError{what: what, host: resp.Request.URL.Host}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return nil, &pageStatusError{what: what, status: resp.StatusCode, url: pageURL}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch %s: %w", what, err)
	}
	return body, nil
}
