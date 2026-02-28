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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("validate token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, resp.Body)
		return false, nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
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
