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
// going beyond upstream, because the blips this exists for (a 5xx, a reset at
// go-live) are the kind a second later outlasts. What keeps the waits, and
// the retries themselves, off the player calls is the retry window
// (pageRetryShare), not the backoff.
const pageRetryBackoffBase = time.Second

// pageRetryShare bounds the retry window: a caller with a deadline lets the
// retries spend at most a third of the budget it had left when the page fetch
// began, so at least two thirds are the player calls'. The page only leads
// into the extraction — every client call comes after it, and since
// fallbackYtcfg and resolvePlayerURL a page that cannot be had costs the
// extraction little — so a deadline-bound caller is the last place to spend
// the cascade's budget on it. That caller is mid-download 403 recovery
// (refreshGvsCredentials, refreshVodURL), whose whole extraction runs under
// min(45 s, MaxTimeout/3) after the GVS mint, 10 s at the floor: with the
// window only the deadline, a page answering 503 throughout held every
// player call back 7 s of those 10, and a hanging one spent all 45 s, so the
// refresh lost its URL half — the recovery it exists for. Under a third, the
// 10 s refresh gets one retry, the 45 s one the whole ladder.
const pageRetryShare = 3

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

// The advice names SOCS, the consent cookie YouTube reads since CONSENT
// stopped passing the wall (yt-dlp 378ae9f9f). A session that is not signed
// in already sends SOCS=CAI (Auth.PageCookieHeader), so the wall standing
// anyway is a signed-in session without a consent of its own, or a CAI
// YouTube no longer honours: either way a browser's own consent is the
// remedy.
func (e *consentWallError) Error() string {
	return fmt.Sprintf("%s redirected to consent wall (%s); accept YouTube's cookie consent in a browser and supply its cookies, SOCS included, via a cookie file", e.what, e.host)
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
// as an error at all (it sends SOCS up front, _initialize_consent), and the
// pages here send the same SOCS (Auth.PageCookieHeader); a wall that stands
// anyway is the cookies' answer, and the next attempt carries the same ones.
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
// The first attempt is the one the page always had, bounded by timeout and
// the caller's context alone. The retries live in a window that closes at
// the sooner of one attempt's timeout from the start and pageRetryShare of
// the caller's remaining budget: a retry whose wait would not leave
// pageRetryBackoffBase of the window for the attempt after it is not taken,
// and a retry that is taken runs only until the window closes. So a failing
// page costs a caller with no deadline (a job start, a go-live) at most one
// attempt's timeout, as the single attempt before these retries did — four
// timed-out attempts and the waits between them would hold a stream's start
// back two minutes — and a caller with a deadline at most a third of what it
// had left. yt-dlp retries a timed-out page as it retries any network error;
// a hung page here is not retried past the first attempt's timeout, short of
// upstream on purpose, because fallbackYtcfg and resolvePlayerURL carry the
// extraction on without it.
//
// A retry the window cuts short says nothing about the page, so the error
// returned is the one the page gave before it, as it is the last attempt's
// when the window or the ladder runs out. A cancelled context ends the loop
// with its own error (retryExitErr); one that lapsed keeps the page's. The
// final error says how many attempts it took when there was more than one.
func fetchYouTubePage(ctx context.Context, what, pageURL string, timeout time.Duration, headers map[string]string, maxBytes int64) ([]byte, error) {
	retryBy := time.Now().Add(timeout)
	if deadline, ok := ctx.Deadline(); ok {
		if share := time.Now().Add(time.Until(deadline) / pageRetryShare); share.Before(retryBy) {
			retryBy = share
		}
	}
	var lastErr error
	attempts := 0
	for attempt := range pageFetchAttempts {
		if err := ctx.Err(); err != nil {
			return nil, retryExitErr(err, lastErr)
		}
		attemptCtx, endAttempt := ctx, context.CancelFunc(func() {})
		if attempt > 0 {
			delay := pageRetryBackoffBase << (attempt - 1)
			if time.Until(retryBy) <= delay+pageRetryBackoffBase {
				break
			}
			if err := utils.Sleep(ctx, delay); err != nil {
				return nil, retryExitErr(err, lastErr)
			}
			attemptCtx, endAttempt = context.WithDeadline(ctx, retryBy)
		}
		attempts++
		body, err := fetchYouTubePageOnce(attemptCtx, what, pageURL, timeout, headers, maxBytes)
		cutShort := attempt > 0 && attemptCtx.Err() != nil && ctx.Err() == nil
		endAttempt()
		if err == nil {
			return body, nil
		}
		if cutShort {
			break
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
