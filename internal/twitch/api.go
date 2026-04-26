package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/httpx"
)

// ErrTwitchAuthExpired signals that Twitch rejected the provided auth-token
// (401 or 403 on a GQL call). Callers can wrap and detect this via errors.Is
// to surface a re-auth prompt to the user rather than treating it as a
// generic HTTP failure. Without this signal, token rotation in the user's
// browser produced indistinguishable errors from transient network trouble.
var ErrTwitchAuthExpired = errors.New("twitch auth token expired or invalid")

// twitchHTTPClient is a shared HTTP client for all Twitch GQL + Helix
// requests. Backed by the shared httpx transport for keep-alive
// amortisation across the 6-8 GQL calls per monitor cycle.
var twitchHTTPClient = httpx.Client(30 * time.Second)

var (
	safeLoginRe   = regexp.MustCompile(`[^a-zA-Z0-9_]`)
	safeVodIDRe   = regexp.MustCompile(`[^0-9]`)
	thumbWidthRe  = regexp.MustCompile(`[%{]*width[}]*`)
	thumbHeightRe = regexp.MustCompile(`[%{]*height[}]*`)
	thumbDimRe    = regexp.MustCompile(`[-_]\d+x\d+\.`)
)

// twitchGQLRatePerSec is the steady-state ceiling for outbound GQL
// requests. Twitch's documented public limit is generous for our flows
// (a few req/sec average across all channels), but bursting tens of
// req/sec while bringing up many channels at startup tripped reactive
// 429s; a proactive token bucket flattens that. Audit reports/twitch.md
// #40.
const twitchGQLRatePerSec = 10

// API provides low-level Twitch GQL and Usher access.
type API struct {
	// rateLimiter throttles outbound GQL requests. Process-wide bucket;
	// per-channel was considered but the existing flows already fan out
	// concurrently across channels, so a single shared bucket is the
	// right place to enforce the ceiling. nil disables rate limiting
	// (used in tests).
	rateLimiter *time.Ticker
	rateTokens  chan struct{}
	rateOnce    sync.Once

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NewAPI creates a new Twitch API client.
func NewAPI(logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *API {
	return &API{logger: logger}
}

// initRateLimiter lazily starts the GQL rate limiter on first use. Tokens
// are produced at twitchGQLRatePerSec; the channel buffer caps the burst
// to twitchGQLRatePerSec (so an idle period doesn't hand out a year's
// worth of tokens at once). Audit reports/twitch.md #40.
func (a *API) initRateLimiter() {
	a.rateOnce.Do(func() {
		a.rateLimiter = time.NewTicker(time.Second / time.Duration(twitchGQLRatePerSec))
		a.rateTokens = make(chan struct{}, twitchGQLRatePerSec)
		// Pre-fill the burst capacity so a fresh process can issue up to
		// twitchGQLRatePerSec without waiting for the first tick.
		for i := 0; i < twitchGQLRatePerSec; i++ {
			a.rateTokens <- struct{}{}
		}
		go func() {
			defer func() {
				if r := recover(); r != nil && a.logger != nil {
					a.logger.Error("twitch rate limiter goroutine panic", "panic", r)
				}
			}()
			for range a.rateLimiter.C {
				select {
				case a.rateTokens <- struct{}{}:
				default:
					// Bucket full — drop the token. Burst-cap behaviour.
				}
			}
		}()
	})
}

// acquireToken blocks until a rate-limit token is available or ctx is
// cancelled. Returns ctx.Err() on cancellation.
func (a *API) acquireToken(ctx context.Context) error {
	a.initRateLimiter()
	select {
	case <-a.rateTokens:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// opLabel returns the operation name or a placeholder when the caller passed
// an empty string (raw query). Keeps error messages parseable when grepping.
func opLabel(opName string) string {
	if opName == "" {
		return "raw"
	}
	return opName
}

// gqlMaxRetries caps the number of retry attempts on transient failures
// (5xx, 429, network errors). 3 retries with the backoff schedule below
// give a worst-case total wait of 7s before giving up.
const gqlMaxRetries = 3

// gqlBaseRetryDelay is the first-retry wait. Doubles each subsequent
// retry up to gqlMaxRetryDelay. Audit reports/twitch.md #11.
const gqlBaseRetryDelay = 1 * time.Second

// gqlMaxRetryDelay caps the exponential backoff so a long Retry-After
// or repeated 429s can't pin a monitor cycle for minutes.
const gqlMaxRetryDelay = 30 * time.Second

// gqlRequest sends a GQL request and returns the raw JSON response.
// opName is used only to annotate error messages so log correlation is
// possible when multiple GQL operations share a pipeline — pass an empty
// string for ad-hoc raw queries.
//
// Transient failures (5xx, 429, transport errors) are retried with
// exponential backoff (1s → 2s → 4s) up to gqlMaxRetries. 429 honors
// `Retry-After` if present and within gqlMaxRetryDelay; otherwise the
// backoff schedule wins. Auth failures (401/403) and 4xx responses are
// not retried — they need a different recovery path (re-login, fix
// caller).
func (a *API) gqlRequest(ctx context.Context, opName string, body any, authToken string) (json.RawMessage, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal gql body (%s): %w", opLabel(opName), err)
	}

	var lastErr error
	for attempt := 0; attempt <= gqlMaxRetries; attempt++ {
		if attempt > 0 {
			// Compute backoff for this retry. retryAfter takes precedence
			// when the previous response was a 429 with a usable header.
			delay := gqlBaseRetryDelay << (attempt - 1) // 1s, 2s, 4s, …
			if delay > gqlMaxRetryDelay {
				delay = gqlMaxRetryDelay
			}
			if a.logger != nil {
				a.logger.Debug("twitch gql retry", "op", opLabel(opName), "attempt", attempt, "delay", delay.String(), "prev_err", lastErr)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		respData, statusCode, hdrRetryAfter, doErr := a.doGQLOnce(ctx, data, authToken)
		if doErr != nil {
			// Transport-level error — retry unless ctx is done.
			lastErr = fmt.Errorf("gql request (%s): %w", opLabel(opName), doErr)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}

		// 429: respect Retry-After when reasonable, else fall through to
		// the standard backoff on the next iteration.
		if statusCode == http.StatusTooManyRequests {
			if ra := parseRetryAfter(hdrRetryAfter); ra > 0 && ra <= gqlMaxRetryDelay {
				if a.logger != nil {
					a.logger.Debug("twitch gql 429 honoring Retry-After", "op", opLabel(opName), "wait", ra.String())
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(ra):
				}
			}
			lastErr = fmt.Errorf("gql rate limited (429) (%s): %s", opLabel(opName), string(respData))
			continue
		}

		// 5xx: retry. Other server-side failures often clear within seconds.
		if statusCode >= 500 && statusCode < 600 {
			lastErr = fmt.Errorf("gql http %d (%s): %s", statusCode, opLabel(opName), string(respData))
			continue
		}

		// 401/403: auth-related — fast-fail without retry.
		if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
			// Wrap the sentinel so callers can detect auth-expiry via
			// errors.Is(err, ErrTwitchAuthExpired) without parsing strings.
			// Only surface the sentinel when the caller actually supplied a
			// token — anon GQL requests legitimately get 401 on some paths
			// (e.g. mature-gated streams) and treating that as "re-auth
			// needed" would loop the user through a login flow pointlessly.
			if authToken != "" {
				return nil, fmt.Errorf("gql auth failure (%d) (%s): %s: %w", statusCode, opLabel(opName), string(respData), ErrTwitchAuthExpired)
			}
			return nil, fmt.Errorf("gql auth failure (%d) (%s): %s", statusCode, opLabel(opName), string(respData))
		}

		// Other 4xx: caller-facing failure (bad query, missing field, …);
		// retrying won't help.
		if statusCode >= 400 && statusCode < 500 {
			return nil, fmt.Errorf("gql http %d (%s): %s", statusCode, opLabel(opName), string(respData))
		}

		// 200 path: continue with response-body validation below.
		return a.parseGQLBody(opName, respData)
	}

	return nil, fmt.Errorf("gql exhausted %d retries: %w", gqlMaxRetries, lastErr)
}

// doGQLOnce performs a single HTTP POST against Twitch GQL. Returns the
// response body, status code, Retry-After header value, and a transport
// error if the request couldn't reach the server.
//
// Audit reports/twitch.md #40 — acquires a rate-limit token before
// sending so a multi-channel startup burst doesn't trip Twitch's public
// 429 ceiling.
func (a *API) doGQLOnce(ctx context.Context, data []byte, authToken string) ([]byte, int, string, error) {
	if err := a.acquireToken(ctx); err != nil {
		return nil, 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, constants.TwitchURLs.GQL, bytes.NewReader(data))
	if err != nil {
		return nil, 0, "", err
	}

	req.Header.Set("Client-ID", constants.TwitchGQLClientID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", constants.UserAgents.Web)
	if authToken != "" {
		req.Header.Set("Authorization", "OAuth "+authToken)
	}

	resp, err := twitchHTTPClient.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB limit
	if err != nil {
		return nil, resp.StatusCode, resp.Header.Get("Retry-After"), err
	}
	return respData, resp.StatusCode, resp.Header.Get("Retry-After"), nil
}

// parseRetryAfter returns the parsed Retry-After header as a Duration.
// Honors both the seconds form (RFC 7231 §7.1.3) and the HTTP-date form.
// Returns 0 for empty / unparseable values so callers can fall back to
// their own backoff schedule.
func parseRetryAfter(hdr string) time.Duration {
	hdr = strings.TrimSpace(hdr)
	if hdr == "" {
		return 0
	}
	// Seconds form
	if secs, err := strconv.Atoi(hdr); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	// HTTP-date form
	if t, err := http.ParseTime(hdr); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// parseGQLBody validates a successful (HTTP 200) GQL response body and
// returns it unchanged. Twitch returns 200 even on GQL-level errors, so
// we still have to look inside.
func (a *API) parseGQLBody(opName string, respData []byte) (json.RawMessage, error) {

	// Twitch GQL returns 200 even on errors — check for errors in response.
	// Response can be a single object or an array (batch requests).
	if len(respData) > 0 && respData[0] == '{' {
		var errCheck struct {
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if json.Unmarshal(respData, &errCheck) == nil && len(errCheck.Errors) > 0 {
			msg := errCheck.Errors[0].Message
			if msg == "" {
				msg = "unknown error"
			}
			return nil, fmt.Errorf("gql error (%s): %s", opLabel(opName), msg)
		}
	} else if len(respData) > 0 && respData[0] == '[' {
		// Batch response — fail only when EVERY element errored. Twitch
		// sometimes returns partial failures (e.g. ComscoreStreamingQuery
		// erroring while StreamMetadata succeeds). The pre-existing
		// "return on first error" behaviour made a single flaky element
		// fail the whole monitor cycle, producing noisy warnings while
		// the useful data was sitting right there in the response.
		// Callers already re-parse elements individually and tolerate
		// missing fields; logging at Warn is enough for observability.
		var batchResults []json.RawMessage
		if err := json.Unmarshal(respData, &batchResults); err != nil {
			return nil, fmt.Errorf("parse batch response (%s): %w", opLabel(opName), err)
		}
		allErrored := len(batchResults) > 0
		firstErrMsg := ""
		for _, item := range batchResults {
			var errCheck struct {
				Errors []struct {
					Message string `json:"message"`
				} `json:"errors"`
			}
			if err := json.Unmarshal(item, &errCheck); err != nil || len(errCheck.Errors) == 0 {
				allErrored = false
				continue
			}
			if firstErrMsg == "" {
				firstErrMsg = errCheck.Errors[0].Message
			}
		}
		if allErrored {
			if firstErrMsg == "" {
				firstErrMsg = "all batch elements returned errors"
			}
			return nil, fmt.Errorf("gql batch error (%s): %s", opLabel(opName), firstErrMsg)
		}
		if firstErrMsg != "" && a.logger != nil {
			a.logger.Warn("twitch gql batch: partial failure", "op", opLabel(opName), "err", firstErrMsg)
		}
	}

	return respData, nil
}

type gqlPersistedQuery struct {
	OperationName string `json:"operationName"`
	Variables     any    `json:"variables"`
	Extensions    struct {
		PersistedQuery struct {
			Version int    `json:"version"`
			Hash    string `json:"sha256Hash"`
		} `json:"persistedQuery"`
	} `json:"extensions"`
}

func newPersistedQuery(opName, hash string, vars any) gqlPersistedQuery {
	q := gqlPersistedQuery{
		OperationName: opName,
		Variables:     vars,
	}
	q.Extensions.PersistedQuery.Version = 1
	q.Extensions.PersistedQuery.Hash = hash
	return q
}

type gqlRawQuery struct {
	Query     string `json:"query"`
	Variables any    `json:"variables,omitempty"`
}

// GetStreamInfo fetches stream info for a channel (batched StreamMetadata + ComscoreStreamingQuery).
// channelLogin is lowercased before the GQL call — Twitch channel names are
// case-insensitive on lookup, but some response paths (and downstream helpers
// like the thumbnail CDN URL built below) only produce correct values when
// the login is the canonical lower-case form. Mixed-case logins entered via
// config could otherwise silently return an empty stream.
func (a *API) GetStreamInfo(ctx context.Context, channelLogin, authToken string) (*TwitchStreamInfo, error) {
	channelLogin = strings.ToLower(channelLogin)

	batch := []gqlPersistedQuery{
		newPersistedQuery("StreamMetadata", constants.TwitchGQLHashes.StreamMetadata, map[string]any{
			"channelLogin": channelLogin,
			"includeIsDJ":  true,
		}),
		newPersistedQuery("ComscoreStreamingQuery", constants.TwitchGQLHashes.ComscoreStreamingQuery, map[string]any{
			"channel":            channelLogin,
			"clipSlug":           "",
			"isClip":             false,
			"isLive":             true,
			"isVodOrCollection":  false,
			"vodID":              "",
		}),
	}

	respData, err := a.gqlRequest(ctx, "StreamMetadata+ComscoreStreamingQuery", batch, authToken)
	if err != nil {
		return nil, err
	}

	var results []json.RawMessage
	if err := json.Unmarshal(respData, &results); err != nil {
		return nil, fmt.Errorf("parse batch response: %w", err)
	}

	if len(results) < 2 {
		return nil, fmt.Errorf("unexpected batch response length: %d", len(results))
	}

	// Parse StreamMetadata
	var smResp struct {
		Data struct {
			User struct {
				ID              string `json:"id"`
				DisplayName     string `json:"displayName"`
				Login           string `json:"login"`
				ProfileImageURL string `json:"profileImageURL"`
				Stream          *struct {
					ID         string `json:"id"`
					Title      string `json:"title"`
					Type       string `json:"type"`
					ViewersCount int  `json:"viewersCount"`
					CreatedAt  string `json:"createdAt"`
					Game       *struct {
						DisplayName string `json:"displayName"`
					} `json:"game"`
				} `json:"stream"`
			} `json:"user"`
		} `json:"data"`
	}

	if err := json.Unmarshal(results[0], &smResp); err != nil {
		return nil, fmt.Errorf("parse StreamMetadata: %w", err)
	}

	user := smResp.Data.User
	if user.Stream == nil {
		return nil, nil // Channel offline
	}

	login := user.Login
	if login == "" {
		login = channelLogin
	}
	displayName := user.DisplayName
	if displayName == "" {
		displayName = channelLogin
	}

	info := &TwitchStreamInfo{
		StreamID:           user.Stream.ID,
		ChannelLogin:       login,
		ChannelDisplayName: displayName,
		ChannelID:          user.ID,
		Title:              user.Stream.Title,
		ViewerCount:        user.Stream.ViewersCount,
		StartedAt:          user.Stream.CreatedAt,
		ProfileImageURL:    user.ProfileImageURL,
		IsLive:             true,
		StreamType:         user.Stream.Type,
		RawStreamType:      user.Stream.Type, // preserved before normalization (audit twitch.md #29)
	}

	if user.Stream.Game != nil {
		info.GameCategory = user.Stream.Game.DisplayName
	}

	// Build thumbnail URL (only if we have a login)
	if login != "" {
		info.ThumbnailURL = fmt.Sprintf("%s/live_user_%s-640x360.jpg",
			constants.TwitchURLs.PreviewCDN, strings.ToLower(login))
	}

	// Parse ComscoreStreamingQuery for title fallback
	var csResp struct {
		Data struct {
			User struct {
				Stream *struct {
					Broadcaster struct {
						BroadcastSettings struct {
							Title string `json:"title"`
							Game  *struct {
								DisplayName string `json:"displayName"`
							} `json:"game"`
						} `json:"broadcastSettings"`
					} `json:"broadcaster"`
				} `json:"stream"`
			} `json:"user"`
		} `json:"data"`
	}

	if err := json.Unmarshal(results[1], &csResp); err == nil && csResp.Data.User.Stream != nil {
		bs := csResp.Data.User.Stream.Broadcaster.BroadcastSettings
		// Comscore title is only a fallback — stream title takes priority
		if info.Title == "" && bs.Title != "" {
			info.Title = bs.Title
		}
		if bs.Game != nil && bs.Game.DisplayName != "" && info.GameCategory == "" {
			info.GameCategory = bs.Game.DisplayName
		}
	}

	// Profile image URL is already part of the StreamMetadata response above
	// — the previous unbatched fallback GQL call was vestigial and tripled
	// monitor-cycle latency for any rare miss. Audit-finding twitch.md #10.

	// Normalize stream type. Twitch documents "live" and "rerun" but new
	// values may appear (e.g. "watch_party" historically). Preserve the
	// original via RawStreamType (already captured above) and warn-once
	// on truly unknown values so a future stream type doesn't get
	// silently swallowed as "live". Audit twitch.md #29.
	switch info.StreamType {
	case "live", "rerun":
		// known shapes — no-op
	case "":
		info.StreamType = "live" // empty defaults to live
	default:
		warnUnknownStreamType(info.StreamType, a.logger)
		// Pass through verbatim instead of overwriting as "live" so the
		// downstream classifier can decide.
	}

	return info, nil
}

// unknownStreamTypesSeen tracks Twitch StreamType values we've already
// warn-logged so the per-tick monitor doesn't spam the same value.
var (
	unknownStreamTypesMu   sync.Mutex
	unknownStreamTypesSeen = map[string]struct{}{}
)

func warnUnknownStreamType(t string, log interface {
	Warn(msg string, args ...any)
}) {
	unknownStreamTypesMu.Lock()
	_, seen := unknownStreamTypesSeen[t]
	if !seen {
		unknownStreamTypesSeen[t] = struct{}{}
	}
	unknownStreamTypesMu.Unlock()
	if !seen && log != nil {
		log.Warn("twitch: unknown stream type — passing through verbatim", "type", t)
	}
}

// GetStreamAccessToken fetches an HLS access token for a live channel.
// channelLogin is lowercased before the Usher call — see GetStreamInfo.
func (a *API) GetStreamAccessToken(ctx context.Context, channelLogin, authToken string) (*TwitchAccessToken, error) {
	safeLogin := safeLoginRe.ReplaceAllString(strings.ToLower(channelLogin), "")

	query := gqlRawQuery{
		Query: fmt.Sprintf(`{
			streamPlaybackAccessToken(
				channelName: "%s",
				params: {
					platform: "web",
					playerBackend: "mediaplayer",
					playerType: "site"
				}
			) {
				value
				signature
			}
		}`, safeLogin),
	}

	respData, err := a.gqlRequest(ctx, "StreamPlaybackAccessToken", query, authToken)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Data struct {
			StreamPlaybackAccessToken *TwitchAccessToken `json:"streamPlaybackAccessToken"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("parse access token: %w", err)
	}

	if resp.Data.StreamPlaybackAccessToken == nil {
		return nil, fmt.Errorf("no access token returned")
	}

	return resp.Data.StreamPlaybackAccessToken, nil
}

// GetVodAccessToken fetches an HLS access token for a VOD.
func (a *API) GetVodAccessToken(ctx context.Context, vodID, authToken string) (*TwitchAccessToken, error) {
	safeID := safeVodIDRe.ReplaceAllString(vodID, "")

	query := gqlRawQuery{
		Query: fmt.Sprintf(`{
			videoPlaybackAccessToken(
				id: "%s",
				params: {
					platform: "web",
					playerBackend: "mediaplayer",
					playerType: "site"
				}
			) {
				value
				signature
			}
		}`, safeID),
	}

	respData, err := a.gqlRequest(ctx, "VideoPlaybackAccessToken", query, authToken)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Data struct {
			VideoPlaybackAccessToken *TwitchAccessToken `json:"videoPlaybackAccessToken"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("parse vod access token: %w", err)
	}

	if resp.Data.VideoPlaybackAccessToken == nil {
		return nil, fmt.Errorf("no vod access token returned")
	}

	return resp.Data.VideoPlaybackAccessToken, nil
}

// GetVodInfo fetches VOD metadata via GQL.
func (a *API) GetVodInfo(ctx context.Context, vodID, authToken string) (*TwitchVodInfo, error) {
	query := newPersistedQuery("VideoMetadata", constants.TwitchGQLHashes.VideoMetadata, map[string]any{
		"channelLogin": "",
		"videoID":      vodID,
	})

	respData, err := a.gqlRequest(ctx, "VideoMetadata", query, authToken)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Data struct {
			Video *struct {
				ID            string `json:"id"`
				Title         string `json:"title"`
				LengthSeconds int    `json:"lengthSeconds"`
				PreviewURL    string `json:"previewThumbnailURL"`
				CreatedAt     string `json:"createdAt"`
				ViewCount     int    `json:"viewCount"`
				Owner         struct {
					ID          string `json:"id"`
					Login       string `json:"login"`
					DisplayName string `json:"displayName"`
				} `json:"owner"`
				Game *struct {
					DisplayName string `json:"displayName"`
				} `json:"game"`
			} `json:"video"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("parse vod info: %w", err)
	}

	if resp.Data.Video == nil {
		return nil, fmt.Errorf("vod not found: %s", vodID)
	}

	v := resp.Data.Video
	info := &TwitchVodInfo{
		VodID:        v.ID,
		Title:        v.Title,
		ChannelLogin: v.Owner.Login,
		ChannelDisplayName: v.Owner.DisplayName,
		ChannelID:    v.Owner.ID,
		Duration:     v.LengthSeconds,
		CreatedAt:    v.CreatedAt,
		ViewCount:    v.ViewCount,
	}

	if v.PreviewURL != "" {
		hadTemplate := thumbWidthRe.MatchString(v.PreviewURL) || thumbHeightRe.MatchString(v.PreviewURL)
		thumbURL := thumbWidthRe.ReplaceAllString(v.PreviewURL, "640")
		thumbURL = thumbHeightRe.ReplaceAllString(thumbURL, "360")
		// Only apply the hard-coded-dimension fallback when the URL had
		// no {width}/{height} template. The template path already
		// produces a canonical 640x360 URL; running thumbDimRe on it
		// would rewrite dash vs underscore separators and mangle
		// legacy URLs that already embedded -640x360. with a literal.
		if !hadTemplate {
			info.ThumbnailURL = thumbDimRe.ReplaceAllString(thumbURL, "-640x360.")
		} else {
			info.ThumbnailURL = thumbURL
		}
	}
	if v.Game != nil {
		info.GameCategory = v.Game.DisplayName
	}

	return info, nil
}

// BuildUsherLiveURL constructs the Usher HLS master playlist URL for a live channel.
func BuildUsherLiveURL(channelLogin string, token *TwitchAccessToken) string {
	params := url.Values{
		"allow_source":               {"true"},
		"allow_audio_only":           {"true"},
		"allow_spectre":              {"true"},
		"fast_bread":                 {"true"},
		"p":                          {strconv.Itoa(rand.Intn(10_000_000))},
		"player":                     {"twitchweb"},
		"playlist_include_framerate": {"true"},
		"sig":                        {token.Signature},
		"token":                      {token.Value},
		"type":                       {"any"},
	}
	return fmt.Sprintf("%s/%s.m3u8?%s",
		constants.TwitchURLs.UsherLive,
		strings.ToLower(channelLogin),
		params.Encode(),
	)
}

// BuildUsherVodURL constructs the Usher HLS master playlist URL for a VOD.
func BuildUsherVodURL(vodID string, token *TwitchAccessToken) string {
	params := url.Values{
		"allow_source":               {"true"},
		"allow_audio_only":           {"true"},
		"allow_spectre":              {"true"},
		"p":                          {strconv.Itoa(rand.Intn(10_000_000))},
		"player":                     {"twitchweb"},
		"playlist_include_framerate": {"true"},
		"sig":                        {token.Signature},
		"token":                      {token.Value},
		"type":                       {"any"},
	}
	return fmt.Sprintf("%s/%s.m3u8?%s",
		constants.TwitchURLs.UsherVOD,
		vodID,
		params.Encode(),
	)
}

// GetVodComments fetches VOD chat comments at a given offset.
func (a *API) GetVodComments(ctx context.Context, vodID string, contentOffsetSeconds float64, authToken string) ([]VodCommentEdge, bool, error) {
	query := newPersistedQuery("VideoCommentsByOffsetOrCursor", constants.TwitchGQLHashes.VideoCommentsByOffsetOrCursor, map[string]any{
		"videoID":              vodID,
		"contentOffsetSeconds": contentOffsetSeconds,
	})

	respData, err := a.gqlRequest(ctx, "VideoCommentsByOffsetOrCursor", query, authToken)
	if err != nil {
		return nil, false, err
	}

	var resp struct {
		Data struct {
			Video struct {
				Comments struct {
					Edges []struct {
						Node struct {
							ID                   string  `json:"id"`
							ContentOffsetSeconds float64 `json:"contentOffsetSeconds"`
							Commenter            *struct {
								DisplayName string `json:"displayName"`
								ID          string `json:"id"`
								Login       string `json:"login"`
							} `json:"commenter"`
							Message struct {
								Fragments []struct {
									Text  string `json:"text"`
									Emote *struct {
										EmoteID string `json:"emoteID"`
									} `json:"emote"`
								} `json:"fragments"`
								UserBadges []struct {
									SetID   string `json:"setID"`
									Version string `json:"version"`
								} `json:"userBadges"`
								UserColor *string `json:"userColor"`
							} `json:"message"`
						} `json:"node"`
					} `json:"edges"`
					PageInfo struct {
						HasNextPage bool `json:"hasNextPage"`
					} `json:"pageInfo"`
				} `json:"comments"`
			} `json:"video"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, false, fmt.Errorf("parse vod comments: %w", err)
	}

	edges := resp.Data.Video.Comments.Edges
	hasNext := resp.Data.Video.Comments.PageInfo.HasNextPage

	var result []VodCommentEdge
	for _, e := range edges {
		node := e.Node

		// Build message text and extract emotes from fragments
		var msgParts []string
		var emotes []TwitchEmoteRef
		charOffset := 0
		for _, f := range node.Message.Fragments {
			// Use UTF-16 code unit length (not rune count) because the frontend's
			// JS substring() operates on UTF-16. Non-BMP characters (U+10000+) are
			// 2 UTF-16 code units but only 1 rune.
			runeLen := utf16Len(f.Text)
			if f.Emote != nil && f.Emote.EmoteID != "" {
				emotes = append(emotes, TwitchEmoteRef{
					ID:    f.Emote.EmoteID,
					Name:  f.Text,
					Start: charOffset,
					End:   charOffset + runeLen - 1,
				})
			}
			msgParts = append(msgParts, f.Text)
			charOffset += runeLen
		}

		// Build badge strings
		var badges []string
		for _, b := range node.Message.UserBadges {
			badges = append(badges, b.SetID+"/"+b.Version)
		}

		edge := VodCommentEdge{
			ID:                   node.ID,
			ContentOffsetSeconds: node.ContentOffsetSeconds,
			MessageText:          strings.Join(msgParts, ""),
			Emotes:               emotes,
		}

		if node.Commenter != nil {
			edge.CommenterDisplayName = node.Commenter.DisplayName
			edge.CommenterID = node.Commenter.ID
			edge.CommenterLogin = node.Commenter.Login
		}

		edge.UserBadges = badges
		if node.Message.UserColor != nil {
			edge.UserColor = *node.Message.UserColor
		}

		result = append(result, edge)
	}

	return result, hasNext, nil
}

// utf16Len returns the number of UTF-16 code units needed to represent s.
// This matches JavaScript's string indexing (String.prototype.substring)
// where characters outside the BMP (U+10000+) count as 2 code units.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2 // surrogate pair
		} else {
			n++
		}
	}
	return n
}
