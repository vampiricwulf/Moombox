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
	"time"
	"github.com/vampiricwulf/Moombox/internal/constants"
)

// ErrTwitchAuthExpired signals that Twitch rejected the provided auth-token
// (401 or 403 on a GQL call). Callers can wrap and detect this via errors.Is
// to surface a re-auth prompt to the user rather than treating it as a
// generic HTTP failure. Without this signal, token rotation in the user's
// browser produced indistinguishable errors from transient network trouble.
var ErrTwitchAuthExpired = errors.New("twitch auth token expired or invalid")

// twitchHTTPClient is a shared HTTP client with a timeout for all Twitch API requests.
var twitchHTTPClient = &http.Client{Timeout: 30 * time.Second}

var (
	safeLoginRe   = regexp.MustCompile(`[^a-zA-Z0-9_]`)
	safeVodIDRe   = regexp.MustCompile(`[^0-9]`)
	thumbWidthRe  = regexp.MustCompile(`[%{]*width[}]*`)
	thumbHeightRe = regexp.MustCompile(`[%{]*height[}]*`)
	thumbDimRe    = regexp.MustCompile(`[-_]\d+x\d+\.`)
)

// API provides low-level Twitch GQL and Usher access.
type API struct {
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

// opLabel returns the operation name or a placeholder when the caller passed
// an empty string (raw query). Keeps error messages parseable when grepping.
func opLabel(opName string) string {
	if opName == "" {
		return "raw"
	}
	return opName
}

// gqlRequest sends a GQL request and returns the raw JSON response.
// opName is used only to annotate error messages so log correlation is
// possible when multiple GQL operations share a pipeline — pass an empty
// string for ad-hoc raw queries.
func (a *API) gqlRequest(ctx context.Context, opName string, body any, authToken string) (json.RawMessage, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal gql body (%s): %w", opLabel(opName), err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, constants.TwitchURLs.GQL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Client-ID", constants.TwitchGQLClientID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", constants.UserAgents.Web)

	if authToken != "" {
		req.Header.Set("Authorization", "OAuth "+authToken)
	}

	resp, err := twitchHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gql request (%s): %w", opLabel(opName), err)
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB limit
	if err != nil {
		return nil, fmt.Errorf("read gql response (%s): %w", opLabel(opName), err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("gql rate limited (429) (%s): %s", opLabel(opName), string(respData))
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Wrap the sentinel so callers can detect auth-expiry via
		// errors.Is(err, ErrTwitchAuthExpired) without parsing strings.
		// Only surface the sentinel when the caller actually supplied a
		// token — anon GQL requests legitimately get 401 on some paths
		// (e.g. mature-gated streams) and treating that as "re-auth
		// needed" would loop the user through a login flow pointlessly.
		if authToken != "" {
			return nil, fmt.Errorf("gql auth failure (%d) (%s): %s: %w", resp.StatusCode, opLabel(opName), string(respData), ErrTwitchAuthExpired)
		}
		return nil, fmt.Errorf("gql auth failure (%d) (%s): %s", resp.StatusCode, opLabel(opName), string(respData))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gql http %d (%s): %s", resp.StatusCode, opLabel(opName), string(respData))
	}

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

	// Profile image URL fallback: if not in StreamMetadata, try direct GQL query
	if info.ProfileImageURL == "" {
		safeLogin := safeLoginRe.ReplaceAllString(channelLogin, "")
		var userResp struct {
			Data struct {
				User *struct {
					ProfileImageURL string `json:"profileImageURL"`
				} `json:"user"`
			} `json:"data"`
		}
		userQuery := gqlRawQuery{
			Query: fmt.Sprintf(`{ user(login: "%s") { profileImageURL(width: 300) } }`, safeLogin),
		}
		rawResp, err := a.gqlRequest(ctx, "UserProfileImageFallback", userQuery, authToken)
		if err == nil {
			if json.Unmarshal(rawResp, &userResp) == nil && userResp.Data.User != nil {
				info.ProfileImageURL = userResp.Data.User.ProfileImageURL
			}
		}
	}

	// Normalize stream type
	if info.StreamType != "rerun" {
		info.StreamType = "live"
	}

	return info, nil
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
