package twitch

import (
	"context"
	"fmt"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// Service is the Twitch service facade wrapping API, auth, chat, HLS, and emotes.
type Service struct {
	API           *API
	Auth          *Auth
	Emotes        *EmoteResolver
	logger        interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NewService creates a new Twitch service.
func NewService(jar *cookies.CookieJar, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *Service {
	return &Service{
		API:    NewAPI(logger),
		Auth:   NewAuth(jar, logger),
		Emotes: NewEmoteResolver(logger),
		logger: logger,
	}
}

// GetStreamInfo fetches live stream info for a channel.
// Returns nil if the channel is offline.
func (s *Service) GetStreamInfo(ctx context.Context, channelLogin string) (*TwitchStreamInfo, error) {
	return s.API.GetStreamInfo(ctx, channelLogin, s.Auth.GetAuthToken())
}

// GetVodInfo fetches VOD metadata.
func (s *Service) GetVodInfo(ctx context.Context, vodID string) (*TwitchVodInfo, error) {
	return s.API.GetVodInfo(ctx, vodID, s.Auth.GetAuthToken())
}

// GetVodComments fetches a page of VOD comments.
func (s *Service) GetVodComments(ctx context.Context, vodID string, contentOffsetSeconds float64) ([]VodCommentEdge, bool, error) {
	return s.API.GetVodComments(ctx, vodID, contentOffsetSeconds, s.Auth.GetAuthToken())
}

// GetHLSMasterPlaylist fetches and parses the HLS master playlist for a live channel.
func (s *Service) GetHLSMasterPlaylist(ctx context.Context, channelLogin string) ([]TwitchHLSVariant, error) {
	token, err := s.API.GetStreamAccessToken(ctx, channelLogin, s.Auth.GetAuthToken())
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}

	url := BuildUsherLiveURL(channelLogin, token)
	return FetchHLSMasterPlaylist(ctx, url)
}

// GetVodHLSPlaylist fetches and parses the HLS master playlist for a VOD.
func (s *Service) GetVodHLSPlaylist(ctx context.Context, vodID string) ([]TwitchHLSVariant, error) {
	token, err := s.API.GetVodAccessToken(ctx, vodID, s.Auth.GetAuthToken())
	if err != nil {
		return nil, fmt.Errorf("get vod access token: %w", err)
	}

	url := BuildUsherVodURL(vodID, token)
	return FetchHLSMasterPlaylist(ctx, url)
}

// SelectBestVariant selects the best HLS variant based on preferences.
func (s *Service) SelectBestVariant(variants []TwitchHLSVariant, qualityPref string, maxResolution int) *TwitchHLSVariant {
	return SelectBestVariant(variants, qualityPref, maxResolution)
}

// HasAuthToken returns true if a Twitch auth token is available.
func (s *Service) HasAuthToken() bool {
	return s.Auth.HasAuthToken()
}

// GetAuthToken returns the current auth token for authenticated requests.
func (s *Service) GetAuthToken() string {
	return s.Auth.GetAuthToken()
}

// GetAuthState returns the current authentication state.
func (s *Service) GetAuthState() map[string]any {
	return map[string]any{
		"isAuthenticated": s.Auth.HasAuthToken(),
	}
}

// ResolveEmotes fetches third-party emotes for a channel.
func (s *Service) ResolveEmotes(ctx context.Context, channelID string, channelLogin ...string) *TwitchEmoteData {
	return s.Emotes.Resolve(ctx, channelID, channelLogin...)
}

// ReloadAuth reloads auth cookies from disk.
func (s *Service) ReloadAuth() {
	if err := s.Auth.Reload(); err != nil {
		s.logger.Warn("failed to reload twitch auth", "err", err)
	}
}

// BuildJobID generates a job ID for a Twitch stream or VOD.
func BuildJobID(streamOrVodID string, isVod bool) string {
	if isVod {
		return "tw_v" + streamOrVodID
	}
	return "tw_" + streamOrVodID
}
