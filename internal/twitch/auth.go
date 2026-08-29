package twitch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// Auth manages Twitch OAuth token extraction and validation.
type Auth struct {
	cookieJar *cookies.CookieJar
	logger    interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NewAuth creates a new Twitch auth manager.
func NewAuth(jar *cookies.CookieJar, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *Auth {
	return &Auth{cookieJar: jar, logger: logger}
}

// GetAuthToken extracts the auth-token from the cookie jar.
func (a *Auth) GetAuthToken() string {
	if a.cookieJar == nil {
		return ""
	}
	return a.cookieJar.GetCookie("auth-token")
}

// GetCredentials extracts the auth-token and the account name it belongs to,
// as one atomic pair.
//
// The IRC handshake needs both: NICK identifies the session's user and Twitch
// binds the OAuth token to it, so an authenticated session must send the
// account's own nickname rather than the anonymous justinfan one. Reading them
// through two accessors would let a concurrent jar Reload pair one session's
// token with another's login — see CookieJar.GetTwitchCredentials, which does
// the reading under a single RLock so this cannot happen.
//
// The login cookie is preferred over the authoritative login Twitch's
// oauth2/validate response carries because it is local: the IRC connect path
// must work when the network is flaky, and caching a validated login would add
// a lifecycle to get wrong. Both values come from the same cookie file as each
// other, so the pair belongs to one session unless the file was hand-edited
// across accounts. ValidateToken consequently does not decode that response
// field at all.
func (a *Auth) GetCredentials() (token, login string) {
	if a.cookieJar == nil {
		return "", ""
	}
	return a.cookieJar.GetTwitchCredentials()
}

// HasAuthToken returns true if a Twitch auth token is available.
func (a *Auth) HasAuthToken() bool {
	return a.GetAuthToken() != ""
}

// ValidateToken checks if the auth token is still valid via Twitch OAuth endpoint.
func (a *Auth) ValidateToken(ctx context.Context) (bool, error) {
	token := a.GetAuthToken()
	if token == "" {
		return false, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, constants.TwitchURLs.OAuthValidate, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "OAuth "+token)

	resp, err := twitchHTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("validate token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, resp.Body)
		return false, nil
	}

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("validate unexpected status %d%s", resp.StatusCode, validateErrorDetail(resp))
	}

	// Parse response for the user id (body is consumed by Decode, no drain
	// needed). The response also carries `login` and `client_id`; neither is
	// decoded. `login` deliberately so — it is the IRC NICK half of this
	// install's credentials (see GetCredentials), nothing here needs it, and a
	// field that does not exist cannot be added to a log line by accident.
	var result struct {
		UserID string `json:"user_id"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Errorf("parse validate response: %w", err)
	}

	// userID only. The login used to ride along here and must not come back:
	// it is a credential half, every line of the IRC handshake work was careful
	// never to log it, and this line runs on the ordinary success path where a
	// debug-level install would write it to disk on every check. An opaque
	// numeric user id is not a credential and is what support actually needs.
	a.logger.Debug("twitch auth validated", "userID", result.UserID)
	return true, nil
}

// validateErrorBodyPrefix bounds how much of an unexpected response body may
// reach the error string, and validateErrorTypeMax bounds the reported media
// type. Both values are remote input; neither is trusted for length.
const (
	validateErrorBodyPrefix = 200
	validateErrorTypeMax    = 64
)

// validateErrorDetail renders what an unexpected oauth2/validate status may
// safely say about the response it came with.
//
// This body used to be read to 1 MB and interpolated whole into the returned
// error, and every caller that logs that error logged the body with it. The
// two answers that actually show up on this path are the ones that must not:
// an intermediary's HTML sign-in or block page, and a service error page that
// echoes the request — including, in the echo case, the Authorization header
// carrying this install's bearer token. A non-200 needs none of that to be
// diagnosable, so what survives is the status (rendered by the caller), the
// media type, and at most a short prefix of the two types that cannot carry
// markup.
//
// The media type is reported PARSED and clamped, never as the raw header: the
// raw value is remote input of unbounded length, so %q on it would put
// whatever an intermediary chose to send into the log verbatim. The body
// prefix is %q-rendered for the same reason — a text/plain body may hold
// newlines, and a log line must stay one line.
//
// The remainder of the body is drained, bounded, so a small error response
// still leaves the connection reusable. That is what the old 1 MB read did as
// a side effect of its hazard.
func validateErrorDetail(resp *http.Response) string {
	defer io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		mediaType = ""
	}
	mediaType = strings.ToLower(mediaType)
	if len(mediaType) > validateErrorTypeMax {
		mediaType = mediaType[:validateErrorTypeMax]
	}

	switch mediaType {
	case "text/plain", "application/json":
		body, _ := io.ReadAll(io.LimitReader(resp.Body, validateErrorBodyPrefix))
		return fmt.Sprintf(" (content-type %s, body %q)", mediaType, body)
	case "":
		return " (no parseable content-type; body omitted)"
	default:
		return fmt.Sprintf(" (content-type %s; body omitted)", mediaType)
	}
}

// Reload reloads the cookie jar to pick up new tokens.
func (a *Auth) Reload() error {
	if a.cookieJar != nil {
		return a.cookieJar.Reload()
	}
	return nil
}
