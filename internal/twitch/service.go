package twitch

import (
	"context"
	"fmt"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// Service is the Twitch service facade wrapping API, auth, chat, HLS, and emotes.
type Service struct {
	API    *API
	Auth   *Auth
	Emotes *EmoteResolver
	logger interface {
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

// GetStreamInfoBatch fetches live stream info for many channels in one GQL
// request. Returns per-channel (info, error) pairs in input order plus a
// separate whole-request error; when wholeErr is non-nil the per-channel
// slices are unusable (see the API method's contract).
func (s *Service) GetStreamInfoBatch(ctx context.Context, channelLogins []string) (infos []*TwitchStreamInfo, errs []error, wholeErr error) {
	return s.API.GetStreamInfoBatch(ctx, channelLogins, s.Auth.GetAuthToken())
}

// GetVodInfo fetches VOD metadata.
func (s *Service) GetVodInfo(ctx context.Context, vodID string) (*TwitchVodInfo, error) {
	return s.API.GetVodInfo(ctx, vodID, s.Auth.GetAuthToken())
}

// GetVodComments fetches a page of VOD comments.
func (s *Service) GetVodComments(ctx context.Context, vodID string, contentOffsetSeconds float64) ([]VodCommentEdge, bool, error) {
	return s.API.GetVodComments(ctx, vodID, contentOffsetSeconds, s.Auth.GetAuthToken())
}

// GetHLSMasterPlaylist fetches and parses the HLS master playlist for a live
// channel, and reports whether Twitch IGNORED the credentials it was sent.
//
// anonymousPlayback is Arc 10 R6: the jar held a Twitch auth-token, the GQL
// call carried it, and the playback access token came back minted for nobody.
// It is the only dead-credential detector a job with chat capture switched off
// ever gets — every other route runs on the IRC handshake — and the caller
// routes it to the same platform mark the chat downgrade uses. See
// playbackTokenReportsAnonymous for the three conditions behind it.
//
// It is returned rather than delivered through a hook on Service so it reaches
// the caller that knows WHICH capture is starting, and so the mid-stream
// quality re-probe (worker's FetchVariantsFn, which calls this method again on
// every format change) can discard it instead of re-marking the platform on a
// loop.
//
// The verdict is returned even when the playlist fetch that follows FAILS: the
// credential is dead either way, and a capture that also fails to start is
// exactly when the operator most needs to know which of the two it is.
func (s *Service) GetHLSMasterPlaylist(ctx context.Context, channelLogin string) (variants []TwitchHLSVariant, anonymousPlayback bool, err error) {
	// Read ONCE and reuse: the same value must decide the request and the
	// verdict below, or a jar reload between two reads could report a
	// cookieless install as one whose credentials failed.
	authToken := s.Auth.GetAuthToken()
	token, err := s.API.GetStreamAccessToken(ctx, channelLogin, authToken)
	if err != nil {
		return nil, false, fmt.Errorf("get access token: %w", err)
	}

	// The dead-credential check, and the only one a chat-disabled job gets.
	// The decision itself is playbackTokenReportsAnonymous, which is where the
	// three conditions and their reasons live — and where they are testable.
	if playbackTokenReportsAnonymous(authToken, token.Value) {
		anonymousPlayback = true
		// Names the channel and the consequence. Nothing from the token
		// document reaches this line.
		s.logger.Warn("twitch issued an ANONYMOUS playback token although credentials were sent; "+
			"this capture will be served stitched ads and cannot fetch subscriber-only content",
			"channel", channelLogin)
	}

	url := BuildUsherLiveURL(channelLogin, token)
	variants, err = FetchHLSMasterPlaylist(ctx, url)
	return variants, anonymousPlayback, err
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
