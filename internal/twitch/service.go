package twitch

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

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

	// onAuthLoss reports a Twitch credential this install holds that Twitch
	// refused — cookies.RefreshService.NoteTwitchAuthLoss in production, wired
	// by cmd/moombox through the same goroutine-spawning hook the worker's
	// downgrade reports use (twitchAuthLossHook). nil is a valid install: the
	// refusal is still logged. The one reason sent from here is
	// AuthDowngradeStatusTokenRefused (see streamInfo).
	//
	// A func rather than the refresh service: internal/cookies is below this
	// package, and RefreshService is not something it should know about.
	onAuthLoss func(reason string)

	// statusTokenRefused is true from a stream check whose auth-token Twitch
	// refused until a stream check on which Twitch takes it again. It only
	// decides the log LEVEL of the next refusal (noteStatusTokenRefused): the
	// monitor asks every 15 s, and a Warn each time would bury the log. The
	// report itself is made on every refusal — NoteTwitchAuthLoss is
	// idempotent, and a mark that skipped repeats could miss the one that
	// follows a credential change.
	statusTokenRefused atomic.Bool
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

// SetOnAuthLoss wires the credential-loss report (onAuthLoss). Called during
// startup, before any monitor or job goroutine exists. fn runs on the
// caller's goroutine — the monitor's poll, a job's wait — and must not block.
func (s *Service) SetOnAuthLoss(fn func(reason string)) {
	s.onAuthLoss = fn
}

// GetStreamInfo fetches live stream info for a channel.
// Returns nil if the channel is offline, or if the login does not resolve —
// LookupStreamInfo tells those two apart.
func (s *Service) GetStreamInfo(ctx context.Context, channelLogin string) (*TwitchStreamInfo, error) {
	return collapseStreamInfoError(s.LookupStreamInfo(ctx, channelLogin))
}

// LookupStreamInfo is GetStreamInfo with the not-found answer kept: (nil,
// ErrChannelNotFound) for a login that does not resolve (API.LookupStreamInfo).
func (s *Service) LookupStreamInfo(ctx context.Context, channelLogin string) (*TwitchStreamInfo, error) {
	infos, errs, err := s.streamInfo(ctx, streamInfoOp, []string{channelLogin})
	if err != nil {
		return nil, err
	}
	return infos[0], errs[0]
}

// GetStreamInfoBatch fetches live stream info for many channels in one GQL
// request. Returns per-channel (info, error) pairs in input order plus a
// separate whole-request error; when wholeErr is non-nil the per-channel
// slices are unusable (see the API method's contract).
func (s *Service) GetStreamInfoBatch(ctx context.Context, channelLogins []string) (infos []*TwitchStreamInfo, errs []error, wholeErr error) {
	return s.streamInfo(ctx, streamInfoBatchOp, channelLogins)
}

// streamInfo sends one stream-status batch with the jar's auth-token and, when
// Twitch refuses that token, repeats it ONCE with none (G21, owner decision
// D19).
//
// WHY. gqlRequest turns a 401/403 to a request that carried a token into
// ErrTwitchAuthExpired, and for a status batch that is a whole-request
// failure: the monitor's checkChunk logs it and checks no channel at all, so a
// token invalidated by a password change, "log out everywhere" or plain expiry
// stopped ALL Twitch live detection until the operator replaced cookies.txt —
// with cookies.auto_enabled off, the default, for however many broadcasts went
// live in between. The batch needs no login: StreamMetadata and
// ComscoreStreamingQuery are public, so the same request without the
// Authorization header answers the question the dead token could not.
// Streamlink's fixture for a dead token is exactly that refusal, a hard 401
// `The "Authorization" token is invalid.` (tests/plugins/test_twitch.py),
// not a silently anonymous answer.
//
// BEYOND UPSTREAM, deliberately. yt-dlp's _download_base_gql sends the
// auth-token cookie whenever there is one and never retries without it
// (twitch.py b77c9304e): a one-shot CLI fails visibly and the user re-runs it.
// A monitor that fails that way misses broadcasts. The repeat is confined to
// this batch. A capture still mints its playback token with the login and
// parks in COOKIES? until the cookies are repaired (D19), so detection keeps
// running and the job it creates waits for the repair rather than record
// signed out.
//
// THE REPORT. A refusal the signed-out repeat then answers is proof that the
// token, and only the token, was refused, and it is reported through
// onAuthLoss so the platform still reads as needing a login: the repeat must
// not make a dead login invisible by making it harmless. A repeat that fails
// too proves nothing — a 403 refused for some other reason refuses the
// anonymous request as well — so the refusal is returned as it was,
// unreported, and every caller classifies it exactly as before.
//
// The authenticated request goes first on every call, so a repaired cookie
// file is used from the very next check, and a refusal costs one extra
// request per batch while it lasts.
func (s *Service) streamInfo(ctx context.Context, opName string, channelLogins []string) (infos []*TwitchStreamInfo, errs []error, wholeErr error) {
	// Read ONCE: the token that decided the request is the one a refusal can
	// be about.
	authToken := s.Auth.GetAuthToken()
	infos, errs, wholeErr = s.API.streamInfo(ctx, opName, channelLogins, authToken)
	// No token, or no channels — an empty batch sends nothing — and this
	// request said nothing about the saved token either way.
	if authToken == "" || len(channelLogins) == 0 {
		return infos, errs, wholeErr
	}
	if !errors.Is(wholeErr, ErrTwitchAuthExpired) {
		if wholeErr == nil && s.statusTokenRefused.Swap(false) {
			s.logger.Info("Twitch accepted the saved auth-token on a stream check again")
		}
		return infos, errs, wholeErr
	}
	anonInfos, anonErrs, anonErr := s.API.streamInfo(ctx, opName, channelLogins, "")
	if anonErr != nil {
		s.logger.Debug("twitch stream check: the auth-token was refused and the signed-out repeat failed too",
			"op", opName, "err", anonErr)
		return infos, errs, wholeErr
	}
	s.noteStatusTokenRefused(opName)
	return anonInfos, anonErrs, nil
}

// noteStatusTokenRefused logs and reports one stream check that Twitch answered
// only signed out. Warn on the first of a run of them, Debug after (see
// statusTokenRefused); the report on every one. Nothing read from the token,
// the jar or the response reaches either line: the op label and a fixed
// vocabulary token are all there is.
func (s *Service) noteStatusTokenRefused(opName string) {
	if !s.statusTokenRefused.Swap(true) {
		s.logger.Warn("Twitch refused the saved auth-token on a stream check and answered it signed out; "+
			"live detection keeps running, but captures need a working login until the cookies are fixed",
			"op", opName)
	} else {
		s.logger.Debug("twitch stream check answered signed out; the saved auth-token is still refused", "op", opName)
	}
	if s.onAuthLoss != nil {
		s.onAuthLoss(AuthDowngradeStatusTokenRefused)
	}
}

// GetVodInfo fetches VOD metadata.
func (s *Service) GetVodInfo(ctx context.Context, vodID string) (*TwitchVodInfo, error) {
	return s.API.GetVodInfo(ctx, vodID, s.Auth.GetAuthToken())
}

// GetVodComments fetches a page of VOD comments. A non-empty cursor selects the
// page after that edge; otherwise the offset selects the page containing it.
func (s *Service) GetVodComments(ctx context.Context, vodID string, contentOffsetSeconds float64, cursor string) ([]VodCommentEdge, bool, error) {
	return s.API.GetVodComments(ctx, vodID, contentOffsetSeconds, cursor, s.Auth.GetAuthToken())
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
func (s *Service) SelectBestVariant(variants []TwitchHLSVariant, qualityPref string, maxResolution int, prefer60fps bool) *TwitchHLSVariant {
	return SelectBestVariant(variants, qualityPref, maxResolution, prefer60fps)
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
