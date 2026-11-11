package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/httpx"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// ErrTwitchAuthExpired signals that Twitch rejected the provided auth-token
// (401 or 403 on a GQL call). Callers can wrap and detect this via errors.Is
// to surface a re-auth prompt to the user rather than treating it as a
// generic HTTP failure. Without this signal, token rotation in the user's
// browser produced indistinguishable errors from transient network trouble.
var ErrTwitchAuthExpired = errors.New("twitch auth token expired or invalid")

// ErrSubscriberOnly signals an usher 403 whose body carries error_code
// vod_manifest_restricted or unauthorized_entitlements — subscriber-only
// content (yt-dlp twitch.py parity) — refused to a session that was NOT
// shown to be signed in: no auth-token was sent, the playback token Twitch
// minted for the request belongs to nobody, or its document could not be
// read (see subscriberOnlyRefusal). Distinct from ErrTwitchAuthExpired: the
// GQL call took the token, Usher just will not serve this session. The
// worker routes it to StatusCookies since logging into an account that has
// access is the fix — the same call yt-dlp makes with raise_login_required.
var ErrSubscriberOnly = errors.New("subscriber-only content: you must be logged into an account that has access")

// ErrSubscriberOnlyNotEntitled is the same Usher refusal made to a session
// Twitch itself names as signed in: an auth-token was sent and the playback
// token minted for it carries a user_id (PlaybackTokenSession, conclusive and
// signed in). The account is alive and simply has no subscription, so no
// cookie — fresh, refreshed or restored — fixes it, and the worker parks the
// job in Error with no automatic cookie refresh (owner decision D20).
//
// Ported from yt-dlp's split in _extract_twitch_m3u8_formats (twitch.py,
// b77c9304e): with an auth-token cookie it raises "Your account does not have
// access to this subscriber-only content", expected=True, and only without
// one does it raise login-required. The wording is yt-dlp's, lowercased for a
// Go error string. Moombox's test is stronger than upstream's cookie-presence
// check, deliberately: a dead auth-token is still a cookie that is present,
// and yt-dlp would tell that session it is signed in without access, where
// the playback token's own user_id says it is anonymous — which stays
// ErrSubscriberOnly here, and so stays a credential problem.
//
// Deliberately NOT a wrap of ErrSubscriberOnly: anything that matches that
// one (cookiesStatusError, twitchAuthSentinel) would route this refusal back
// to COOKIES? and a pointless browser refresh.
var ErrSubscriberOnlyNotEntitled = errors.New("your account does not have access to this subscriber-only content")

// ErrChannelNotFound signals a login that doesn't resolve to a Twitch user
// (renamed, banned, deleted, or a typo) — GQL returns user:null, which is
// indistinguishable from "offline" without this. Surfaced by
// GetStreamInfoBatch (the monitor's health tracker uses it) and by
// LookupStreamInfo, the not-found-aware single lookup the manual add and the
// manual wait use (G53); GetStreamInfo keeps its historical (nil, nil) for
// the callers that treat not-yet-live and not-found identically.
var ErrChannelNotFound = errors.New("twitch channel not found (renamed, banned, or invalid login)")

// ErrPersistedQueryNotFound is what a GQL call returns (wrapped) when Twitch
// answers `{"errors":[{"message":"PersistedQueryNotFound"}]}`: the sha256Hash
// it was sent (constants.TwitchGQLHashes) names no query Twitch still holds —
// Twitch re-hashed the operation, and every call of it fails the same way
// until the hash is bumped. The text stays the one Twitch sent, so a log line
// reads as before; what it adds is an identity callers can tell apart from a
// query's other errors without matching on it (ClipAPIChangedError).
var ErrPersistedQueryNotFound = errors.New(persistedQueryNotFoundMsg)

const persistedQueryNotFoundMsg = "PersistedQueryNotFound"

// errStreamSlotUnavailable is a per-channel TRANSIENT failure inside an
// otherwise-successful batch (that channel's StreamMetadata element carried
// a GQL error or null data). Distinct from ErrChannelNotFound so the
// monitor's health tracker counts it as an ordinary recoverable streak
// error, not a definitive renamed/banned channel. The single GetStreamInfo
// now PROPAGATES it (owner decision O-C): the worker confirms an end from two
// samples, so a transient slot failure must reach it as an error rather than
// as "offline" — collapsing it to (nil, nil) is what let one flap finalize a
// live recording mid-broadcast. See collapseStreamInfoError.
var errStreamSlotUnavailable = errors.New("twitch stream metadata slot unavailable")

// twitchHTTPClient is a shared HTTP client for all Twitch GQL + Helix
// requests. Backed by the shared httpx transport for keep-alive
// amortisation across the 6-8 GQL calls per monitor cycle.
var twitchHTTPClient = httpx.Client(30 * time.Second)

// clientIDOverride is the `[twitch] client_id` an install configured, or nil
// for the built-in constants.TwitchGQLClientID (G70). yt-dlp makes its own
// Client-ID an extractor argument for the same reason
// (`--extractor-args twitch:client_id=…`, TwitchBaseIE._CLIENT_ID, twitch.py
// b77c9304e): it is the first thing Twitch's integrity checks rotate out from
// under a client, and a hard-coded one can then only be fixed by a release.
// Process-wide, like the ID it replaces: every GQL call and every Usher
// playlist fetch sends it. Set once at boot (the setting is restart-required),
// atomic so a test that sets it races nothing.
var clientIDOverride atomic.Pointer[string]

// SetClientID installs the configured Client-ID override. An empty id
// restores the built-in one.
func SetClientID(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		clientIDOverride.Store(nil)
		return
	}
	clientIDOverride.Store(&id)
}

// ClientID is the Client-ID every Twitch request sends: the configured
// override, else constants.TwitchGQLClientID.
func ClientID() string {
	if id := clientIDOverride.Load(); id != nil {
		return *id
	}
	return constants.TwitchGQLClientID
}

var (
	safeLoginRe   = regexp.MustCompile(`[^a-zA-Z0-9_]`)
	safeVodIDRe   = regexp.MustCompile(`[^0-9]`)
	thumbWidthRe  = regexp.MustCompile(`[%{]*width[}]*`)
	thumbHeightRe = regexp.MustCompile(`[%{]*height[}]*`)
	thumbDimRe    = regexp.MustCompile(`[-_]\d+x\d+\.`)
	// vodProcessingThumbRe is Twitch's "still processing" placeholder
	// thumbnail, yt-dlp's `/404_processing_[^.?#]+\.png` (twitch.py 516).
	vodProcessingThumbRe = regexp.MustCompile(`/404_processing_[^.?#]+\.png`)
)

// twitchGQLRatePerSec is the steady-state ceiling for outbound GQL
// requests. Twitch's documented public limit is generous for our flows
// (a few req/sec average across all channels), but bursting tens of
// req/sec while bringing up many channels at startup tripped reactive
// 429s; a proactive token bucket flattens that. Audit reports/twitch.md
// #40.
const twitchGQLRatePerSec = 10

// twitchGQLTokenInterval is how long the bucket takes to earn one token,
// and twitchGQLBurst the most it holds: an idle period never hands out
// more than twitchGQLRatePerSec requests at once.
const (
	twitchGQLTokenInterval = time.Second / twitchGQLRatePerSec
	twitchGQLBurst         = twitchGQLRatePerSec * twitchGQLTokenInterval
)

// API provides low-level Twitch GQL and Usher access.
type API struct {
	// The GQL rate limiter: one process-wide token bucket. Per-channel was
	// considered, but the flows already fan out concurrently across
	// channels, so a single shared bucket is the right place to enforce
	// the ceiling. It is refilled lazily from elapsed time on each
	// acquireToken call, so an idle process runs no timer and no goroutine
	// for it.
	//
	// rateCredit is the bucket's content as time: one token is worth
	// twitchGQLTokenInterval, a full bucket twitchGQLBurst. It goes
	// negative while callers wait: each has reserved a token the bucket
	// has still to earn. rateLast is when rateCredit was last brought up
	// to date; zero means never used, which reads as a full bucket.
	rateMu     sync.Mutex
	rateCredit time.Duration
	rateLast   time.Time

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

// reserveToken takes one token from the bucket and returns how long the
// caller must wait before using it: zero when the bucket held one, and
// otherwise the time the bucket needs to earn it, behind every caller
// already waiting. Audit reports/twitch.md #40.
func (a *API) reserveToken() time.Duration {
	a.rateMu.Lock()
	defer a.rateMu.Unlock()
	now := time.Now()
	if a.rateLast.IsZero() {
		// First use: a fresh process may issue the whole burst at once.
		a.rateCredit = twitchGQLBurst
	} else if elapsed := now.Sub(a.rateLast); elapsed > 0 {
		// min before the add: elapsed can be days, the credit never more
		// than the burst.
		a.rateCredit += min(elapsed, twitchGQLBurst-a.rateCredit)
	}
	a.rateLast = now
	a.rateCredit -= twitchGQLTokenInterval
	if a.rateCredit >= 0 {
		return 0
	}
	return -a.rateCredit
}

// returnToken gives back a token reserved by a caller that gave up
// waiting, so its place is not charged to the callers after it.
func (a *API) returnToken() {
	a.rateMu.Lock()
	defer a.rateMu.Unlock()
	a.rateCredit = min(a.rateCredit+twitchGQLTokenInterval, twitchGQLBurst)
}

// acquireToken blocks until a rate-limit token is available or ctx is
// cancelled. Returns ctx.Err() on cancellation; a context that is already
// done takes no token.
func (a *API) acquireToken(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	wait := a.reserveToken()
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		a.returnToken()
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
// (5xx, 429, network errors). 3 retries on the backoff schedule below wait
// 7s in all before giving up; a 429's Retry-After (at most gqlMaxRetryDelay)
// replaces a retry's backoff, so throttled retries can wait up to
// 3 × gqlMaxRetryDelay.
const gqlMaxRetries = 3

// gqlBaseRetryDelay is the first-retry wait. Doubles each subsequent
// retry up to gqlMaxRetryDelay. Audit reports/twitch.md #11.
//
// A var, not a const, for exactly one reader: api_gql_log_hygiene_test.go
// shrinks it to a millisecond so the retried arms can be driven to
// exhaustion. Production never assigns it.
var gqlBaseRetryDelay = 1 * time.Second

// gqlMaxRetryDelay caps the exponential backoff so a long Retry-After
// or repeated 429s can't pin a monitor cycle for minutes.
const gqlMaxRetryDelay = 30 * time.Second

// gqlWait is every wait gqlRequest makes between attempts — the backoff and
// a 429's Retry-After alike: d, or until ctx ends, whose error it returns. A
// var for the tests, which record the waits instead of serving them: a
// Retry-After is whole seconds, and gqlBaseRetryDelay cannot shrink it.
// Production never assigns it.
var gqlWait = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// gqlBodySize renders a GQL response body as its SIZE, never its bytes.
//
// Every gqlRequest error used to interpolate string(respData). An
// intermediary's error page can echo the request headers -- including
// Authorization: OAuth <token> -- and those errors reach both the retry log
// line and every caller that logs a failure, which Moombox fans out over the
// WebSocket to the dashboard and the TUI. The size is what an operator
// actually needs from a body they must not see: it separates "empty 503" from
// "an HTML error page" without carrying either.
func gqlBodySize(respData []byte) string {
	return fmt.Sprintf("%d-byte body", len(respData))
}

// gqlRequest sends a GQL request and returns the raw JSON response.
// opName is used only to annotate error messages so log correlation is
// possible when multiple GQL operations share a pipeline — pass an empty
// string for ad-hoc raw queries.
//
// Transient failures (5xx, 429, transport errors) are retried with
// exponential backoff (1s → 2s → 4s) up to gqlMaxRetries. A 429 honors
// `Retry-After` when it is within gqlMaxRetryDelay (that wait replaces the
// retry's own backoff); a `Retry-After` LONGER than the cap returns the 429 at
// once, so the caller's cycle cadence is the backoff rather than three quick
// retries into a throttle Twitch asked us to respect. Auth failures (401/403)
// and other 4xx responses are not retried — they need a different recovery
// path (re-login, fix caller).
func (a *API) gqlRequest(ctx context.Context, opName string, body any, authToken string) (json.RawMessage, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal gql body (%s): %w", opLabel(opName), err)
	}

	var lastErr error
	// lastStatus is what the retry line reports instead of the previous
	// error: 0 for a transport-level failure, otherwise the HTTP status that
	// caused the retry. The error itself is deliberately NOT logged -- a
	// transport error wraps whatever the proxy said, and that is the same
	// class of untrusted upstream text gqlBodySize exists to keep out.
	lastStatus := 0
	skipBackoff := false
	for attempt := 0; attempt <= gqlMaxRetries; attempt++ {
		if attempt > 0 && !skipBackoff {
			// Compute backoff for this retry — 1s, 2s, 4s, … capped at
			// gqlMaxRetryDelay. retryAfter takes precedence when the
			// previous response was a 429 with a usable header (the 429
			// branch below sleeps Retry-After itself and sets skipBackoff
			// so the two delays don't stack).
			delay := min(gqlBaseRetryDelay<<(attempt-1), gqlMaxRetryDelay)
			if a.logger != nil {
				a.logger.Debug("twitch gql retry", "op", opLabel(opName), "attempt", attempt, "delay", delay.String(), "prev_status", lastStatus)
			}
			if err := gqlWait(ctx, delay); err != nil {
				return nil, err
			}
		}
		skipBackoff = false

		respData, statusCode, hdrRetryAfter, doErr := a.doGQLOnce(ctx, data, authToken)
		if doErr != nil {
			// Transport-level error — retry unless ctx is done.
			lastErr = fmt.Errorf("gql request (%s): %w", opLabel(opName), doErr)
			lastStatus = 0
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}

		// 429: respect Retry-After. Inside gqlMaxRetryDelay it becomes THIS
		// retry's delay; beyond it, retrying is the wrong answer altogether —
		// three requests on the 1s/2s/4s schedule walk straight into a
		// throttle Twitch just asked us to sit out, and the caller's own cycle
		// (the monitor's 15 s, a quality probe's 30 s) is already a better
		// backoff than anything this loop can offer (TWITCH-9).
		if statusCode == http.StatusTooManyRequests {
			ra := parseRetryAfter(hdrRetryAfter)
			lastErr = fmt.Errorf("gql rate limited (429) (%s): %s", opLabel(opName), gqlBodySize(respData))
			lastStatus = statusCode
			if ra > gqlMaxRetryDelay {
				if a.logger != nil {
					a.logger.Warn("twitch gql 429 asked for longer than we retry; deferring to the caller's cadence",
						"op", opLabel(opName), "retry_after", ra.String(), "cap", gqlMaxRetryDelay.String())
				}
				return nil, lastErr
			}
			// No retry is left after the final attempt, so there is nothing to
			// wait FOR: sleeping out the window there only delayed the same
			// exhausted-retries error by up to gqlMaxRetryDelay.
			if ra > 0 && attempt < gqlMaxRetries {
				if a.logger != nil {
					a.logger.Debug("twitch gql 429 honoring Retry-After", "op", opLabel(opName), "wait", ra.String())
				}
				if err := gqlWait(ctx, ra); err != nil {
					return nil, err
				}
				// Retry-After already served as this retry's delay — don't
				// stack the exponential backoff on top of it.
				skipBackoff = true
			}
			continue
		}

		// 5xx: retry. Other server-side failures often clear within seconds.
		if statusCode >= 500 && statusCode < 600 {
			lastErr = fmt.Errorf("gql http %d (%s): %s", statusCode, opLabel(opName), gqlBodySize(respData))
			lastStatus = statusCode
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
				return nil, fmt.Errorf("gql auth failure (%d) (%s): %s: %w", statusCode, opLabel(opName), gqlBodySize(respData), ErrTwitchAuthExpired)
			}
			return nil, fmt.Errorf("gql auth failure (%d) (%s): %s", statusCode, opLabel(opName), gqlBodySize(respData))
		}

		// Other 4xx: caller-facing failure (bad query, missing field, …);
		// retrying won't help.
		if statusCode >= 400 && statusCode < 500 {
			return nil, fmt.Errorf("gql http %d (%s): %s", statusCode, opLabel(opName), gqlBodySize(respData))
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

	req.Header.Set("Client-ID", ClientID())
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
			if msg == persistedQueryNotFoundMsg {
				return nil, fmt.Errorf("gql error (%s): %w", opLabel(opName), ErrPersistedQueryNotFound)
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
			if firstErrMsg == persistedQueryNotFoundMsg {
				return nil, fmt.Errorf("gql batch error (%s): %w", opLabel(opName), ErrPersistedQueryNotFound)
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

// The labels gqlRequest's errors and retry lines carry for the one-channel and
// the many-channel shape of the same StreamMetadata + ComscoreStreamingQuery
// batch (streamInfo sends both).
const (
	streamInfoOp      = "StreamMetadata+ComscoreStreamingQuery"
	streamInfoBatchOp = "StreamMetadata+ComscoreStreamingQuery(batch)"
)

// GetStreamInfo fetches stream info for a channel (batched StreamMetadata + ComscoreStreamingQuery).
// channelLogin is lowercased before the GQL call — Twitch channel names are
// case-insensitive on lookup, but some response paths (and downstream helpers
// like the thumbnail CDN URL built below) only produce correct values when
// the login is the canonical lower-case form. Mixed-case logins entered via
// config could otherwise silently return an empty stream.
//
// A login that does not resolve reads as offline here (collapseStreamInfoError);
// LookupStreamInfo is the same request without that collapse.
func (a *API) GetStreamInfo(ctx context.Context, channelLogin, authToken string) (*TwitchStreamInfo, error) {
	return collapseStreamInfoError(a.LookupStreamInfo(ctx, channelLogin, authToken))
}

// LookupStreamInfo is GetStreamInfo's request with the not-found answer kept:
// (info, nil) live, (nil, nil) offline, (nil, ErrChannelNotFound) for a login
// that does not resolve — GQL's user:null, yt-dlp's "<channel> does not exist"
// (TwitchStreamIE._real_extract, twitch.py b77c9304e), which upstream raises
// as an expected error at once rather than wait out as a UserNotLive. A
// one-channel batch through the same streamInfo the monitor's batch uses, so
// the two shapes cannot drift apart.
func (a *API) LookupStreamInfo(ctx context.Context, channelLogin, authToken string) (*TwitchStreamInfo, error) {
	infos, errs, err := a.streamInfo(ctx, streamInfoOp, []string{channelLogin}, authToken)
	if err != nil {
		return nil, err
	}
	return infos[0], errs[0]
}

// collapseStreamInfoError applies the single-call contract to parseStreamInfo's
// result. ErrChannelNotFound still reads as offline for GetStreamInfo's callers
// (processTwitchLive's first look, the capture's end checks), and a login that
// does not resolve IS a settled answer there. The two callers for which it is
// NOT the same answer as offline — the manual-add route, which used to create
// a job for a typo, and waitForTwitchLive, which then polled it forever (G53)
// — call LookupStreamInfo, which skips this collapse.
//
// errStreamSlotUnavailable no longer does (owner decision O-C, sweep-2
// ENGINE-3/TWITCH-2). A per-element GQL error or null data is a TRANSIENT
// partial-batch failure, and collapsing it into "offline" is what let ONE
// sample finalize a live recording as Finished mid-broadcast: the worker's
// CheckStreamFn read (nil, nil) as "the stream ended". Propagated as an
// error it becomes a deferred verdict at every consult site instead.
func collapseStreamInfoError(info *TwitchStreamInfo, err error) (*TwitchStreamInfo, error) {
	if errors.Is(err, ErrChannelNotFound) {
		return nil, nil
	}
	return info, err
}

// streamInfoOps builds the StreamMetadata + ComscoreStreamingQuery persisted
// queries for one channel. Shared by the single and batched paths so the
// two can never drift.
func streamInfoOps(channelLogin string) []gqlPersistedQuery {
	return []gqlPersistedQuery{
		newPersistedQuery("StreamMetadata", constants.TwitchGQLHashes.StreamMetadata, map[string]any{
			"channelLogin": channelLogin,
			"includeIsDJ":  true,
		}),
		newPersistedQuery("ComscoreStreamingQuery", constants.TwitchGQLHashes.ComscoreStreamingQuery, map[string]any{
			"channel":           channelLogin,
			"clipSlug":          "",
			"isClip":            false,
			"isLive":            true,
			"isVodOrCollection": false,
			"vodID":             "",
		}),
	}
}

// GetStreamInfoBatch fetches stream info for MANY channels in a SINGLE GQL
// request (2 ops per channel), collapsing an N-channel monitor cycle from N
// serialized round-trips to one.
//
// Returns a result per input login in the SAME order plus a SEPARATE
// whole-request error:
//   - wholeErr != nil → the entire request failed (transport, auth,
//     malformed batch). infos/errs are unusable; the caller must NOT
//     attribute this to any channel (it's not per-channel — a shared
//     outage would otherwise mark every channel unhealthy at once).
//   - wholeErr == nil → per-channel: infos[i]/errs[i] where (nil,nil) is
//     offline, (nil, ErrChannelNotFound) is an unresolved login, and
//     (nil, parseErr) is a malformed slot for THAT channel only
//     (index-mapped isolation — one bad channel never taints its
//     neighbors).
func (a *API) GetStreamInfoBatch(ctx context.Context, channelLogins []string, authToken string) (infos []*TwitchStreamInfo, errs []error, wholeErr error) {
	return a.streamInfo(ctx, streamInfoBatchOp, channelLogins, authToken)
}

// streamInfo is the one request behind GetStreamInfoBatch and LookupStreamInfo:
// channelLogins' StreamMetadata + Comscore pairs in one GQL batch, sent once
// with authToken, labelled opName. GetStreamInfoBatch's doc is its contract.
func (a *API) streamInfo(ctx context.Context, opName string, channelLogins []string, authToken string) (infos []*TwitchStreamInfo, errs []error, wholeErr error) {
	infos = make([]*TwitchStreamInfo, len(channelLogins))
	errs = make([]error, len(channelLogins))
	if len(channelLogins) == 0 {
		return infos, errs, nil
	}

	ops := make([]gqlPersistedQuery, 0, len(channelLogins)*2)
	lowered := make([]string, len(channelLogins))
	for i, login := range channelLogins {
		lowered[i] = strings.ToLower(login)
		ops = append(ops, streamInfoOps(lowered[i])...)
	}

	respData, err := a.gqlRequest(ctx, opName, ops, authToken)
	if err != nil {
		return infos, errs, err
	}

	var results []json.RawMessage
	if err := json.Unmarshal(respData, &results); err != nil {
		return infos, errs, fmt.Errorf("parse batch response: %w", err)
	}
	if len(results) < len(channelLogins)*2 {
		// A truncated batch is a whole-request anomaly, not a per-channel
		// fault — don't index into a short slice or blame channels.
		return infos, errs, fmt.Errorf("batch response has %d results, expected %d", len(results), len(channelLogins)*2)
	}

	// Results are index-aligned with ops: channel i occupies [2i, 2i+1].
	for i := range channelLogins {
		infos[i], errs[i] = a.parseStreamInfo(lowered[i], results[2*i], results[2*i+1])
	}
	return infos, errs, nil
}

// parseStreamInfo turns one channel's StreamMetadata + Comscore result pair
// into a TwitchStreamInfo. Returns (nil, nil) when the channel is offline,
// (nil, ErrChannelNotFound) when the login doesn't resolve (renamed/banned/
// typo'd — GQL returns user:null), and (nil, err) on a genuine parse error.
func (a *API) parseStreamInfo(channelLogin string, smRaw, csRaw json.RawMessage) (*TwitchStreamInfo, error) {
	// Parse StreamMetadata. Data is a POINTER and Errors is captured so we
	// can tell a genuine user:null (data present, user null → not-found)
	// from a per-element GQL error / null-data slot in a PARTIAL batch
	// (transient) — otherwise a flaky slot for a live channel would be
	// mislabeled as renamed/banned and wrongly bump its health streak.
	var smResp struct {
		Errors []json.RawMessage `json:"errors"`
		Data   *struct {
			User *struct {
				ID              string `json:"id"`
				DisplayName     string `json:"displayName"`
				Login           string `json:"login"`
				ProfileImageURL string `json:"profileImageURL"`
				// LastBroadcast is where streamlink reads a live channel's
				// title (plugins/twitch.py metadata_channel), one of the
				// title fallbacks below. Speculative here: streamlink sends
				// a different StreamMetadata persisted query (b57f9b91…)
				// from the one Moombox sends (yt-dlp's ad022ca3…), and a
				// persisted query fixes its selection set, so whether this
				// reply carries it at all is unverified. Absent, it is "".
				LastBroadcast *struct {
					Title string `json:"title"`
				} `json:"lastBroadcast"`
				Stream *struct {
					ID           string           `json:"id"`
					Title        string           `json:"title"`
					Type         string           `json:"type"`
					ViewersCount int              `json:"viewersCount"`
					CreatedAt    string           `json:"createdAt"`
					Game         *twitchGameField `json:"game"`
				} `json:"stream"`
			} `json:"user"`
		} `json:"data"`
	}

	if err := json.Unmarshal(smRaw, &smResp); err != nil {
		return nil, fmt.Errorf("parse StreamMetadata: %w", err)
	}

	// Per-element error or absent data = a transient partial-batch failure,
	// NOT a channel that doesn't exist. Return a plain error so the health
	// tracker counts it as an ordinary streak error (recoverable) rather
	// than a definitive not-found.
	if len(smResp.Errors) > 0 || smResp.Data == nil {
		return nil, errStreamSlotUnavailable
	}

	// data.user == null → the login doesn't resolve (renamed/banned/typo).
	if smResp.Data.User == nil {
		return nil, ErrChannelNotFound
	}

	user := *smResp.Data.User
	// A resolved user with no stream is a real offline channel — success,
	// not an error (the user:null not-found case is handled above).
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

	info.GameCategory = user.Stream.Game.name()

	// Build thumbnail URL (only if we have a login)
	if login != "" {
		info.ThumbnailURL = fmt.Sprintf("%s/live_user_%s-640x360.jpg",
			constants.TwitchURLs.PreviewCDN, strings.ToLower(login))
	}

	// ComscoreStreamingQuery, for the title and category fallbacks. Two
	// shapes are read: yt-dlp's, user.broadcastSettings (TwitchStreamIE
	// _real_extract, twitch.py b77c9304e, which takes its live description
	// from user.broadcastSettings.title), and the user.stream.broadcaster
	// path this parser read alone until G69, which no reference uses.
	var csResp struct {
		Data struct {
			User struct {
				BroadcastSettings *twitchBroadcastSettings `json:"broadcastSettings"`
				Stream            *struct {
					Broadcaster struct {
						BroadcastSettings twitchBroadcastSettings `json:"broadcastSettings"`
					} `json:"broadcaster"`
				} `json:"stream"`
			} `json:"user"`
		} `json:"data"`
	}
	var ytdlpSettings, legacySettings twitchBroadcastSettings
	if err := json.Unmarshal(csRaw, &csResp); err == nil {
		if csResp.Data.User.BroadcastSettings != nil {
			ytdlpSettings = *csResp.Data.User.BroadcastSettings
		}
		if csResp.Data.User.Stream != nil {
			legacySettings = csResp.Data.User.Stream.Broadcaster.BroadcastSettings
		}
	}
	var lastBroadcastTitle string
	if user.LastBroadcast != nil {
		lastBroadcastTitle = user.LastBroadcast.Title
	}

	// THE TITLE, first non-empty of (G69): StreamMetadata's own
	// user.stream.title; Comscore user.broadcastSettings.title (yt-dlp);
	// StreamMetadata user.lastBroadcast.title (streamlink's path, but
	// streamlink validates it under its own StreamMetadata hash, b57f9b91…,
	// not the yt-dlp one Moombox sends, so whether this reply carries it is
	// unverified — harmless when absent); and last the Comscore path read
	// before. The Comscore path is sound: its hash is yt-dlp's own. The order cannot be checked
	// against a capture from here — MOOMBOX_LIVE_TWITCH_TEST needs a live
	// channel — so it is built from the two references' field paths: the
	// primary path stays first because production titles show today.
	if info.Title == "" {
		info.Title = firstNonEmpty(ytdlpSettings.Title, lastBroadcastTitle, legacySettings.Title)
	}
	// The category, the same way: stream.game (displayName, then the
	// game.name streamlink reads), then each Comscore shape's game.
	if info.GameCategory == "" {
		info.GameCategory = firstNonEmpty(ytdlpSettings.Game.name(), legacySettings.Game.name())
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

// twitchGameField is a GQL game: displayName where Moombox has always read
// it, name where streamlink reads it (StreamMetadata user.stream.game.name —
// under streamlink's own StreamMetadata hash, so whether Moombox's reply
// carries it is unverified; absent, it is "").
type twitchGameField struct {
	DisplayName string `json:"displayName"`
	Name        string `json:"name"`
}

// name is the game's display name, else its name; "" for no game.
func (g *twitchGameField) name() string {
	if g == nil {
		return ""
	}
	return firstNonEmpty(g.DisplayName, g.Name)
}

// twitchBroadcastSettings is a ComscoreStreamingQuery broadcastSettings
// object, at either of the paths parseStreamInfo reads.
type twitchBroadcastSettings struct {
	Title string           `json:"title"`
	Game  *twitchGameField `json:"game"`
}

// firstNonEmpty is the first of vals that is not "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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

// GetVodInfo fetches VOD metadata via GQL: VideoMetadata, batched with
// VideoPlayer_ChapterSelectButtonVideo for the VOD's chapters, as yt-dlp's
// TwitchVodIE._download_info batches them (twitch.py b77c9304e; it also sends
// VideoPlayer_VODSeekbarPreviewVideo, for the storyboard, which Moombox has no
// use for). The description is VideoMetadata's own field, as upstream reads it
// (_extract_info_gql), G68.
//
// The chapter half is best effort: the batch answers per element, and a
// chapter op Twitch errors on (a rotated hash) still returns the metadata — the
// VOD downloads without chapters, never fails over them (parseGQLBody fails a
// batch only when EVERY element errored). A reply that is a single object
// rather than an array is read as VideoMetadata alone.
func (a *API) GetVodInfo(ctx context.Context, vodID, authToken string) (*TwitchVodInfo, error) {
	ops := []gqlPersistedQuery{
		newPersistedQuery("VideoMetadata", constants.TwitchGQLHashes.VideoMetadata, map[string]any{
			"channelLogin": "",
			"videoID":      vodID,
		}),
		newPersistedQuery("VideoPlayer_ChapterSelectButtonVideo", constants.TwitchGQLHashes.VideoPlayerChapterSelectButtonVideo, map[string]any{
			"includePrivate": false,
			"videoID":        vodID,
		}),
	}

	respData, err := a.gqlRequest(ctx, "VideoMetadata+VideoPlayer_ChapterSelectButtonVideo", ops, authToken)
	if err != nil {
		return nil, err
	}

	var results []json.RawMessage
	if len(respData) > 0 && respData[0] == '[' {
		if err := json.Unmarshal(respData, &results); err != nil {
			return nil, fmt.Errorf("parse vod info: %w", err)
		}
	} else {
		results = []json.RawMessage{respData}
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("parse vod info: empty batch reply")
	}

	var resp struct {
		Data struct {
			Video *struct {
				ID            string `json:"id"`
				Title         string `json:"title"`
				Description   string `json:"description"`
				LengthSeconds int    `json:"lengthSeconds"`
				PreviewURL    string `json:"previewThumbnailURL"`
				BroadcastType string `json:"broadcastType"`
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

	if err := json.Unmarshal(results[0], &resp); err != nil {
		return nil, fmt.Errorf("parse vod info: %w", err)
	}

	if resp.Data.Video == nil {
		return nil, fmt.Errorf("vod not found: %s", vodID)
	}

	v := resp.Data.Video
	// yt-dlp's `info.get('title') or 'Untitled Broadcast'` (twitch.py 526):
	// every title written from this one — the stream processor's
	// "<Channel> — <title>" and the manual-add route's — then names the
	// broadcast instead of ending on a dangling separator, and so does the
	// filename built from it. Beyond yt-dlp, a whitespace-only title counts as
	// empty too (it would dangle the separator just the same); a title with
	// text is kept exactly as Twitch sent it, surrounding spaces included, as
	// yt-dlp keeps it.
	title := v.Title
	if strings.TrimSpace(title) == "" {
		title = "Untitled Broadcast"
	}
	info := &TwitchVodInfo{
		VodID:              v.ID,
		Title:              title,
		Description:        v.Description,
		ChannelLogin:       v.Owner.Login,
		ChannelDisplayName: v.Owner.DisplayName,
		ChannelID:          v.Owner.ID,
		Duration:           v.LengthSeconds,
		CreatedAt:          v.CreatedAt,
		ViewCount:          v.ViewCount,
		BroadcastType:      v.BroadcastType,
	}

	// The processing placeholder is no thumbnail of the broadcast: yt-dlp
	// drops it (thumbnail = None) whatever the broadcast type, and only an
	// ARCHIVE behind it is still recording. Stored, it was rewritten by
	// thumbDimRe into a 404_processing-640x360.png that exists nowhere, which
	// the dashboard, notifications and the staging thumbnail fetch all read.
	if vodProcessingThumbRe.MatchString(v.PreviewURL) {
		info.IsRecording = v.BroadcastType == "ARCHIVE"
	} else if v.PreviewURL != "" {
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

	var moments []vodMoment
	if len(results) > 1 {
		moments = parseVodMoments(results[1])
	}
	info.Chapters = vodChapters(moments, info.GameCategory)

	return info, nil
}

// vodMoment is one node of VideoPlayer_ChapterSelectButtonVideo's
// data.video.moments.edges: a game segment of the VOD.
type vodMoment struct {
	PositionMilliseconds *int64 `json:"positionMilliseconds"`
	DurationMilliseconds *int64 `json:"durationMilliseconds"`
	Description          string `json:"description"`
}

// parseVodMoments reads the moments out of the chapter op's reply; a reply
// that is not that shape, or an errored element, has none.
func parseVodMoments(raw json.RawMessage) []vodMoment {
	var resp struct {
		Data struct {
			Video *struct {
				Moments struct {
					Edges []struct {
						Node *vodMoment `json:"node"`
					} `json:"edges"`
				} `json:"moments"`
			} `json:"video"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &resp) != nil || resp.Data.Video == nil {
		return nil
	}
	var out []vodMoment
	for _, e := range resp.Data.Video.Moments.Edges {
		if e.Node != nil {
			out = append(out, *e.Node)
		}
	}
	return out
}

// vodChapters is yt-dlp's TwitchVodIE._extract_chapters (twitch.py
// b77c9304e): one chapter per moment, from positionMilliseconds for
// durationMilliseconds and titled with its description, a moment missing
// either time skipped (upstream warns "Important chapter information
// missing"); and with no moments at all, the whole VOD as one chapter named
// after its game, when it has one. That single chapter is open-ended, as
// upstream yields it ({'title': game}, ended by process_info's fill-in at the
// video's duration): utils.SanitizeChapters ends it at the archive's probed
// span at mux. Ending it at lengthSeconds cut it short for a VOD still being
// recorded, whose length at extraction is only the length so far.
func vodChapters(moments []vodMoment, game string) []utils.Chapter {
	if len(moments) == 0 {
		if game == "" {
			return nil
		}
		return []utils.Chapter{{StartMs: 0, Title: game}}
	}
	var out []utils.Chapter
	for _, m := range moments {
		if m.PositionMilliseconds == nil || m.DurationMilliseconds == nil {
			continue
		}
		out = append(out, utils.Chapter{
			StartMs: *m.PositionMilliseconds,
			EndMs:   *m.PositionMilliseconds + *m.DurationMilliseconds,
			Title:   m.Description,
		})
	}
	return out
}

// thumbOriginalSizeRe is yt-dlp's _get_thumbnails rewrite
// (`\d+x\d+(\.\w+)($|(?=[?#]))` → `0x0\1`, twitch.py b77c9304e), which asks
// the CDN for the image at its original size. RE2 has no lookahead, so the
// "?" or "#" that ends the match is captured and put back.
var thumbOriginalSizeRe = regexp.MustCompile(`\d+x\d+(\.\w+)($|[?#])`)

// OriginalSizeThumbnailURL is a Twitch thumbnail URL rewritten to its
// original size (0x0), the form yt-dlp ranks first (preference 1) with the
// sized URL as its fallback (G45). "" when the URL carries no WxH size to
// rewrite.
func OriginalSizeThumbnailURL(u string) string {
	if !thumbOriginalSizeRe.MatchString(u) {
		return ""
	}
	out := thumbOriginalSizeRe.ReplaceAllString(u, "0x0${1}${2}")
	if out == u {
		return ""
	}
	return out
}

// BuildUsherLiveURL constructs the Usher HLS master playlist URL for a live channel.
//
// platform=web and supported_codecs=av1,h265,h264 are the ENHANCED-BROADCAST
// opt-in, byte-for-byte what yt-dlp sends (references/yt-dlp
// yt_dlp/extractor/twitch.py, _extract_twitch_m3u8_formats). Without them
// Twitch never offers the HEVC/AV1 1440p/4K source of a channel that
// multi-encodes, and the capture takes the H.264 transcode. The short names
// here are the request spelling; the playlist answers in RFC 6381 codec ids
// (av01…/hev1…/hvc1…) — see videoCodecFamily in hls.go.
func BuildUsherLiveURL(channelLogin string, token *TwitchAccessToken) string {
	params := url.Values{
		"allow_source":               {"true"},
		"allow_audio_only":           {"true"},
		"allow_spectre":              {"true"},
		"fast_bread":                 {"true"},
		"p":                          {strconv.Itoa(rand.IntN(10_000_000))},
		"platform":                   {"web"},
		"player":                     {"twitchweb"},
		"playlist_include_framerate": {"true"},
		"sig":                        {token.Signature},
		"supported_codecs":           {"av1,h265,h264"},
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
// platform / supported_codecs: see BuildUsherLiveURL — yt-dlp sends the same
// pair for both paths, and enhanced-broadcast VODs exist.
func BuildUsherVodURL(vodID string, token *TwitchAccessToken) string {
	params := url.Values{
		"allow_source":               {"true"},
		"allow_audio_only":           {"true"},
		"allow_spectre":              {"true"},
		"p":                          {strconv.Itoa(rand.IntN(10_000_000))},
		"platform":                   {"web"},
		"player":                     {"twitchweb"},
		"playlist_include_framerate": {"true"},
		"sig":                        {token.Signature},
		"supported_codecs":           {"av1,h265,h264"},
		"token":                      {token.Value},
		"type":                       {"any"},
	}
	return fmt.Sprintf("%s/%s.m3u8?%s",
		constants.TwitchURLs.UsherVOD,
		vodID,
		params.Encode(),
	)
}

// GetVodComments fetches one page of VOD chat comments.
//
// The operation is VideoCommentsByOffsetOrCursor and the OR is the point: a
// non-empty cursor selects the page AFTER the edge it came from, and
// contentOffsetSeconds selects the page containing a moment. They are mutually
// exclusive — sending both leaves it to Twitch's resolver which one wins — so
// the cursor takes precedence here and the offset is used only to ENTER a VOD
// (a fresh start, or a resume from the sidecar's LastOffsetSeconds).
//
// The persisted-query hash is unchanged: both forms are the same operation.
func (a *API) GetVodComments(ctx context.Context, vodID string, contentOffsetSeconds float64, cursor, authToken string) ([]VodCommentEdge, bool, error) {
	vars := map[string]any{"videoID": vodID}
	if cursor != "" {
		vars["cursor"] = cursor
	} else {
		vars["contentOffsetSeconds"] = contentOffsetSeconds
	}
	query := newPersistedQuery("VideoCommentsByOffsetOrCursor", constants.TwitchGQLHashes.VideoCommentsByOffsetOrCursor, vars)

	respData, err := a.gqlRequest(ctx, "VideoCommentsByOffsetOrCursor", query, authToken)
	if err != nil {
		return nil, false, err
	}

	var resp struct {
		Data struct {
			Video struct {
				Comments struct {
					Edges []struct {
						Cursor string `json:"cursor"`
						Node   struct {
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
			Cursor:               e.Cursor,
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
