package twitch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

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

// GetLogin extracts the login (account name) from the cookie jar.
//
// The IRC handshake needs it: NICK identifies the session's user and Twitch
// binds the OAuth token to it, so an authenticated session must send the
// account's own nickname rather than the anonymous justinfan one.
//
// The cookie is preferred over the authoritative login in ValidateToken's
// response because it is local: the IRC connect path must work when the
// network is flaky, and caching a validated login would add a lifecycle to
// get wrong. Both values come from the same cookie file as the auth-token, so
// the pair belongs to one session unless the file was hand-edited across
// accounts — in which case the login is simply rejected, visibly.
func (a *Auth) GetLogin() string {
	if a.cookieJar == nil {
		return ""
	}
	return a.cookieJar.GetTwitchLogin()
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return false, fmt.Errorf("validate unexpected status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response for login info (body is consumed by Decode, no drain needed)
	var result struct {
		Login    string `json:"login"`
		UserID   string `json:"user_id"`
		ClientID string `json:"client_id"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Errorf("parse validate response: %w", err)
	}

	a.logger.Debug("twitch auth validated", "login", result.Login, "userID", result.UserID)
	return true, nil
}

// Reload reloads the cookie jar to pick up new tokens.
func (a *Auth) Reload() error {
	if a.cookieJar != nil {
		return a.cookieJar.Reload()
	}
	return nil
}
