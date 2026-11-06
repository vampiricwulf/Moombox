package twitch

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// Twitch clips (owner decision D26): one clip is a whole MP4 Twitch renders
// once and serves signed, read through the ShareClipRenderStatus persisted
// query, and a channel's clips are listed through ClipsCards__User. Both are
// ported from yt-dlp's extractors at b77c9304e — TwitchClipsIE
// (yt_dlp/extractor/twitch.py 1129-1336) and TwitchVideosClipsIE (823-880,
// paged by TwitchPlaylistBaseIE._entries, 649-697) — and their hashes live
// beside the other persisted-query hashes in constants.TwitchGQLHashes.

// ClipJobIDPrefix starts the job ID (and video ID) of a clip job, the
// clip's slug after it: "tw_clip_FaintLightGullWholeWheat". It is the clip
// kind's mark, as "tw_v" is a VOD's, and the archive's ${id} — the default
// output template's "[tw_clip_<slug>]" tells a clip's file from a VOD's
// ("[tw_v<id>]") and a live capture's ("[tw_<stream id>]") without a token of
// its own. Distinct from "tw_v" on purpose: a slug may start with a "v"
// ("VibrantCat…"), and "tw_" + that slug would read as a VOD.
const ClipJobIDPrefix = "tw_clip_"

// ClipJobID is the job ID of the clip with this slug. One slug, one ID: the
// dedupe that archives a clip once (AddJob's primary key, the processing
// history) is keyed on it, whichever add path or monitor sweep found it.
func ClipJobID(slug string) string { return ClipJobIDPrefix + slug }

// ClipSlugFromJobID is the slug a clip job's ID carries, and false for any
// other ID.
func ClipSlugFromJobID(id string) (string, bool) {
	slug, ok := strings.CutPrefix(id, ClipJobIDPrefix)
	return slug, ok && slug != ""
}

// ClipAPIChangedError is a clip operation Twitch no longer knows by the hash
// Moombox sends: PersistedQueryNotFound (ErrPersistedQueryNotFound). yt-dlp
// re-hashed ShareClipRenderStatus three times in 14 months, and every clip
// fails the same way until the hash is bumped, so the job says what to do
// (owner decision D26) rather than repeat Twitch's one-word refusal. It
// unwraps to ErrPersistedQueryNotFound.
type ClipAPIChangedError struct {
	// Op is the GQL operation Twitch refused: ShareClipRenderStatus or
	// ClipsCards__User.
	Op string
}

func (e *ClipAPIChangedError) Error() string {
	return fmt.Sprintf("Twitch changed its clip API (%s); update Moombox", e.Op)
}

func (e *ClipAPIChangedError) Unwrap() error { return ErrPersistedQueryNotFound }

// clipOpError turns a refused hash into ClipAPIChangedError and leaves every
// other error as it is.
func clipOpError(op string, err error) error {
	if errors.Is(err, ErrPersistedQueryNotFound) {
		return &ClipAPIChangedError{Op: op}
	}
	return err
}

// ErrClipUnavailable is the answer for a slug Twitch returns no clip for
// (`data.clip` null): deleted, or never existed. yt-dlp's wording
// (TwitchClipsIE._real_extract: "This clip is no longer available").
var ErrClipUnavailable = errors.New("this clip is no longer available")

// TwitchClipQuality is one rendition of a clip: a whole MP4, its URL already
// signed with the clip's playback access token.
type TwitchClipQuality struct {
	URL string `json:"url"`
	// Quality is Twitch's label for it ("1080", "720", …), which yt-dlp takes
	// for both the format ID and the height.
	Quality   string  `json:"quality"`
	Height    int     `json:"height,omitempty"`
	FrameRate float64 `json:"frameRate,omitempty"`
	// Portrait marks a rendition of the clip's portrait asset (assets[1]),
	// the vertical crop Twitch makes for mobile. yt-dlp ranks those below
	// every landscape one (quality -2); BestClipQuality does the same.
	Portrait bool `json:"portrait,omitempty"`
}

// TwitchClipInfo is one clip as ShareClipRenderStatus describes it — the
// fields yt-dlp's TwitchClipsIE reads.
type TwitchClipInfo struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Title string `json:"title"`
	// BroadcasterLogin is the channel's login when the answer carries one
	// (yt-dlp reads only the display name and ID); "" otherwise.
	BroadcasterLogin       string `json:"broadcasterLogin,omitempty"`
	BroadcasterDisplayName string `json:"broadcasterDisplayName"`
	BroadcasterID          string `json:"broadcasterId,omitempty"`
	// Curator is who made the clip (yt-dlp's uploader: curator.displayName,
	// and uploader_id the curator's id). The login is kept beside the display
	// name, which a clip job's description names it by and falls back from
	// (owner decision FE-32).
	CuratorDisplayName string `json:"curatorDisplayName,omitempty"`
	CuratorLogin       string `json:"curatorLogin,omitempty"`
	// Description is the clip's own description when the answer carries one.
	// yt-dlp reads none off ShareClipRenderStatus, whose selection set is
	// Twitch's: "" whenever the node has no description field.
	Description     string `json:"description,omitempty"`
	GameCategory    string `json:"gameCategory,omitempty"`
	CreatedAt       string `json:"createdAt,omitempty"`
	DurationSeconds int    `json:"durationSeconds,omitempty"`
	ViewCount       int    `json:"viewCount,omitempty"`
	// VideoID is the source VOD the clip was cut from, and VideoOffsetSeconds
	// where in it the clip starts (nil when the answer gives no offset), for
	// the clip job's link back to it (owner decision FQ-3). BEYOND UPSTREAM:
	// yt-dlp's TwitchClipsIE reads neither, but its own ShareClipRenderStatus
	// answer carries both — Twitch's selection set for that persisted query
	// (hash 2db6a3b2…, the one yt-dlp sends at b77c9304e) holds
	// `video { id broadcastType title }` and `videoOffsetSeconds`, null when
	// the VOD is gone or was never saved, as a live probe of yt-dlp's own
	// test clips showed on 2026-11-06. No second query is sent for them.
	VideoID            string `json:"videoId,omitempty"`
	VideoOffsetSeconds *int   `json:"videoOffsetSeconds,omitempty"`
	// ThumbnailURL is the best thumbnail: the default asset's, else the
	// clip's own small one, else the portrait asset's. Unlike upstream,
	// which ranks the portrait one (-1) above the small one (-2); see info.
	ThumbnailURL string              `json:"thumbnailUrl,omitempty"`
	Qualities    []TwitchClipQuality `json:"qualities,omitempty"`
}

// TwitchClipCard is one entry of a channel's clip list (ClipsCards__User),
// the fields yt-dlp's TwitchVideosClipsIE._extract_entry reads plus the slug
// its URL names.
type TwitchClipCard struct {
	ID              string `json:"id"`
	Slug            string `json:"slug"`
	URL             string `json:"url"`
	Title           string `json:"title"`
	ThumbnailURL    string `json:"thumbnailUrl,omitempty"`
	DurationSeconds int    `json:"durationSeconds,omitempty"`
	CreatedAt       string `json:"createdAt,omitempty"`
	ViewCount       int    `json:"viewCount,omitempty"`
}

// ClipFilter is ClipsCards__User's time filter (criteria.filter): the clips
// created within the last day, week, month, or ever. yt-dlp's _RANGE maps
// the clip page's ?range= to these (twitch.py 840-846).
type ClipFilter string

const (
	ClipFilterLastDay   ClipFilter = "LAST_DAY"
	ClipFilterLastWeek  ClipFilter = "LAST_WEEK"
	ClipFilterLastMonth ClipFilter = "LAST_MONTH"
	ClipFilterAllTime   ClipFilter = "ALL_TIME"
)

// clipsPageLimit is ClipsCards__User's page size. yt-dlp: "NB: values other
// than 20 result in skipped videos" (TwitchVideosClipsIE._PAGE_LIMIT).
const clipsPageLimit = 20

// clipsMaxPages bounds one listing. BEYOND UPSTREAM: yt-dlp pages a playlist
// until the cursor runs out, which is right for a one-shot command; the
// monitor's clip sweep runs every hour, and a channel whose day of clips ran
// to thousands would page for minutes each time. Fifty pages are 1000 clips,
// far past a day's or a week's on any channel the sweep's filters ask about.
const clipsMaxPages = 50

// GetClip reads one clip through ShareClipRenderStatus — yt-dlp's
// TwitchClipsIE._real_extract. Each rendition's sourceURL is signed with the
// clip's playbackAccessToken, as `sig` and `token` query parameters
// (update_url_query(query=access_query)): unsigned, the CDN refuses it.
//
// A slug Twitch has no clip for is ErrClipUnavailable; a refused hash is
// ClipAPIChangedError.
func (a *API) GetClip(ctx context.Context, slug, authToken string) (*TwitchClipInfo, error) {
	const op = "ShareClipRenderStatus"
	query := newPersistedQuery(op, constants.TwitchGQLHashes.ShareClipRenderStatus, map[string]any{"slug": slug})
	respData, err := a.gqlRequest(ctx, op, query, authToken)
	if err != nil {
		return nil, clipOpError(op, err)
	}
	var resp struct {
		Data struct {
			Clip *clipNode `json:"clip"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("parse clip (%s): %w", op, err)
	}
	if resp.Data.Clip == nil {
		return nil, ErrClipUnavailable
	}
	return resp.Data.Clip.info(slug)
}

// clipNode is ShareClipRenderStatus's `data.clip`.
type clipNode struct {
	ID              string        `json:"id"`
	Slug            string        `json:"slug"`
	Title           string        `json:"title"`
	Description     string        `json:"description"`
	DurationSeconds flexInt       `json:"durationSeconds"`
	ViewCount       flexInt       `json:"viewCount"`
	CreatedAt       string        `json:"createdAt"`
	ThumbnailURL    string        `json:"thumbnailURL"`
	Broadcaster     *clipUser     `json:"broadcaster"`
	Curator         *clipUser     `json:"curator"`
	Game            *clipGame     `json:"game"`
	Video           *clipVideo    `json:"video"`
	VideoOffset     *flexInt      `json:"videoOffsetSeconds"`
	AccessToken     *clipToken    `json:"playbackAccessToken"`
	Assets          []clipAssetRW `json:"assets"`
}

type clipUser struct {
	ID          string `json:"id"`
	Login       string `json:"login"`
	DisplayName string `json:"displayName"`
}

// clipVideo is the clip's source VOD (`data.clip.video`): null when the VOD
// was deleted or never saved.
type clipVideo struct {
	ID string `json:"id"`
}

type clipGame struct {
	DisplayName string `json:"displayName"`
	Name        string `json:"name"`
}

type clipToken struct {
	Signature string `json:"signature"`
	Value     string `json:"value"`
}

type clipAssetRW struct {
	ThumbnailURL   string `json:"thumbnailURL"`
	VideoQualities []struct {
		SourceURL string    `json:"sourceURL"`
		Quality   string    `json:"quality"`
		FrameRate flexFloat `json:"frameRate"`
	} `json:"videoQualities"`
}

// info maps the node to a TwitchClipInfo, signing every rendition. The
// default asset (assets[0]) is the landscape clip and the second, when there
// is one, its portrait crop — the order yt-dlp reads them in.
func (c *clipNode) info(slug string) (*TwitchClipInfo, error) {
	if c.AccessToken == nil || c.AccessToken.Signature == "" || c.AccessToken.Value == "" {
		// yt-dlp indexes clip['playbackAccessToken'] unguarded and fails the
		// extraction without one; an unsigned URL is refused by the CDN, so
		// the clip cannot be fetched either way.
		return nil, errors.New("clip has no playback access token")
	}
	out := &TwitchClipInfo{
		ID:              cmp.Or(c.ID, slug),
		Slug:            cmp.Or(c.Slug, slug),
		Title:           c.Title,
		Description:     strings.TrimSpace(c.Description),
		CreatedAt:       c.CreatedAt,
		DurationSeconds: int(c.DurationSeconds),
		ViewCount:       int(c.ViewCount),
	}
	if b := c.Broadcaster; b != nil {
		out.BroadcasterLogin = strings.ToLower(b.Login)
		out.BroadcasterDisplayName = b.DisplayName
		out.BroadcasterID = b.ID
	}
	if c.Curator != nil {
		out.CuratorDisplayName = c.Curator.DisplayName
		out.CuratorLogin = strings.ToLower(c.Curator.Login)
	}
	if c.Game != nil {
		out.GameCategory = cmp.Or(c.Game.DisplayName, c.Game.Name)
	}
	if c.Video != nil {
		out.VideoID = strings.TrimSpace(c.Video.ID)
	}
	if c.VideoOffset != nil {
		offset := int(*c.VideoOffset)
		out.VideoOffsetSeconds = &offset
	}
	for i, asset := range c.Assets {
		if i > 1 {
			break // yt-dlp reads assets 0 (default) and 1 (portrait) only
		}
		for _, q := range asset.VideoQualities {
			signed, ok := signClipURL(q.SourceURL, c.AccessToken)
			if !ok {
				continue // url_or_none: a quality with no usable URL is skipped
			}
			height, _ := strconv.Atoi(q.Quality)
			out.Qualities = append(out.Qualities, TwitchClipQuality{
				URL: signed, Quality: q.Quality, Height: height,
				FrameRate: float64(q.FrameRate), Portrait: i == 1,
			})
		}
	}
	// Thumbnails, best first: the default asset's ("default", preference 0),
	// then the clip's own ("small", -2). DIFFERENT FROM UPSTREAM: yt-dlp
	// ranks the portrait asset's (-1) above the small one (twitch.py
	// b77c9304e 1246-1266); Moombox takes one thumbnail, embedded in a
	// landscape archive, and the portrait one is a vertical crop of the same
	// frame, so it only stands in when it is all there is.
	var portraitThumb string
	if len(c.Assets) > 1 {
		portraitThumb = c.Assets[1].ThumbnailURL
	}
	var defaultThumb string
	if len(c.Assets) > 0 {
		defaultThumb = c.Assets[0].ThumbnailURL
	}
	out.ThumbnailURL = cmp.Or(httpURL(defaultThumb), httpURL(c.ThumbnailURL), httpURL(portraitThumb))
	return out, nil
}

// signClipURL appends the clip's access token to a rendition's URL as
// yt-dlp's update_url_query does: `sig` the signature, `token` the value.
// false for a URL that is not http(s) (url_or_none).
func signClipURL(raw string, tok *clipToken) (string, bool) {
	if httpURL(raw) == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	q := u.Query()
	q.Set("sig", tok.Signature)
	q.Set("token", tok.Value)
	u.RawQuery = q.Encode()
	return u.String(), true
}

// httpURL is raw when it is an absolute http(s) URL, else "" — yt-dlp's
// url_or_none for the fields it guards with it.
func httpURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return strings.TrimSpace(raw)
}

// BestClipQuality is a clip's best rendition — the one SelectClipQuality
// falls back to when nothing fits the job's cap and preference: the best
// landscape one — tallest, then highest frame rate — and a portrait one only when the
// clip has no landscape rendition at all. That is yt-dlp's default ranking of
// TwitchClipsIE's formats: the portrait ones carry quality -2, below every
// landscape one, and within each the height then the fps decide. nil when
// there is none.
func BestClipQuality(qs []TwitchClipQuality) *TwitchClipQuality {
	var best *TwitchClipQuality
	for i := range qs {
		q := &qs[i]
		if best == nil || clipQualityBetter(q, best) {
			best = q
		}
	}
	return best
}

// SelectClipQuality is the rendition a clip job downloads under the job's
// quality_preference and the downloader's max_video_resolution and
// prefer_60fps (owner decision FE-29): the rendition a VOD's selection would
// take from the same ladder — SelectBestVariant, the rule every Twitch
// capture starts on (selectTwitchVariant) — over the clip's landscape
// renditions, or over its portrait ones only when it has no landscape one, so
// a landscape rendition still ranks above every portrait one whatever the
// cap and the preference say (yt-dlp's quality -2, as BestClipQuality).
//
// When nothing fits — no rendition at or under a set cap — it is
// BestClipQuality, the best rendition, where a VOD's cap would take the
// smallest size above it (utils.SelectByCap): the owner's rule for clips.
// "audio_only" is SmallestClipQuality, whatever the cap: a clip has no
// audio-only rendition, and the job keeps only the audio of the one it
// downloads (owner decision FQ-4), so the fewest video bytes are the best.
// A preference with no rendition of its size descends as a VOD's does, to the
// next lower size and then the best under the cap. nil when qs is empty.
//
// yt-dlp has no cap or preference of its own (its -f/-S are the user's);
// this is Moombox's setting applied to the formats TwitchClipsIE lists.
func SelectClipQuality(qs []TwitchClipQuality, qualityPref string, maxResolution int, prefer60fps bool) *TwitchClipQuality {
	pool := make([]int, 0, len(qs))
	for i := range qs {
		if !qs[i].Portrait {
			pool = append(pool, i)
		}
	}
	if len(pool) == 0 {
		for i := range qs {
			pool = append(pool, i)
		}
	}
	if len(pool) == 0 {
		return nil
	}
	fits := maxResolution <= 0
	variants := make([]TwitchHLSVariant, len(pool))
	for n, i := range pool {
		q := &qs[i]
		// The clip's label is its height (yt-dlp takes it for both); a
		// variant without a width measures by it (utils.CapDimension). The
		// URL carries the index back, since the pick may point into
		// SelectBestVariant's own filtered copy.
		variants[n] = TwitchHLSVariant{URL: strconv.Itoa(i), Name: q.Quality + "p", Height: q.Height, FPS: q.FrameRate}
		if q.Height > 0 && q.Height <= maxResolution {
			fits = true
		}
	}
	if strings.EqualFold(qualityPref, "audio_only") {
		return SmallestClipQuality(qs)
	}
	if !fits {
		return BestClipQuality(qs)
	}
	pick := SelectBestVariant(variants, qualityPref, maxResolution, prefer60fps)
	if pick == nil {
		return BestClipQuality(qs)
	}
	i, err := strconv.Atoi(pick.URL)
	if err != nil || i < 0 || i >= len(qs) {
		return BestClipQuality(qs)
	}
	return &qs[i]
}

// SmallestClipQuality is the rendition an audio_only clip job downloads to
// keep its audio alone (owner decision FQ-4): the shortest landscape one —
// lowest height, then lowest frame rate — and a portrait one only when the
// clip has no landscape rendition, as SelectClipQuality's pool is drawn. A
// clip's renditions are encodes of one source whose audio rides beside
// every one of them, so the smallest video is the fewest bytes for the same
// track. Twitch lists none smaller than the clip's own (BEYOND UPSTREAM:
// yt-dlp's clip extractor offers no audio-only format, and its `-x` would
// take bestaudio — the best rendition, video and all — before extracting).
// nil when qs is empty.
func SmallestClipQuality(qs []TwitchClipQuality) *TwitchClipQuality {
	var landscape bool
	for i := range qs {
		if !qs[i].Portrait {
			landscape = true
			break
		}
	}
	var small *TwitchClipQuality
	for i := range qs {
		q := &qs[i]
		if landscape && q.Portrait {
			continue
		}
		if small == nil || q.Height < small.Height || (q.Height == small.Height && q.FrameRate < small.FrameRate) {
			small = q
		}
	}
	return small
}

func clipQualityBetter(a, b *TwitchClipQuality) bool {
	if a.Portrait != b.Portrait {
		return !a.Portrait
	}
	if a.Height != b.Height {
		return a.Height > b.Height
	}
	return a.FrameRate > b.FrameRate
}

// ListClips pages a channel's clips created within filter through
// ClipsCards__User — yt-dlp's TwitchVideosClipsIE over
// TwitchPlaylistBaseIE._entries: 20 to a page, each page asked for with the
// cursor of the last entry the previous one yielded, until a page has no
// edges or no cursor. Edges and nodes of another __typename are skipped, and
// so is a node with no clip URL. A login Twitch answers with an empty user
// ID (the channel does not exist — yt-dlp #15450) is ErrChannelNotFound; a
// refused hash is ClipAPIChangedError. At most clipsMaxPages pages are read.
func (a *API) ListClips(ctx context.Context, login string, filter ClipFilter, authToken string) ([]TwitchClipCard, error) {
	const op = "ClipsCards__User"
	var out []TwitchClipCard
	cursor := ""
	for page := 0; page < clipsMaxPages; page++ {
		vars := map[string]any{
			"login":    strings.ToLower(login),
			"criteria": map[string]any{"filter": string(filter)},
			"limit":    clipsPageLimit,
		}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		respData, err := a.gqlRequest(ctx, op, newPersistedQuery(op, constants.TwitchGQLHashes.ClipsCardsUser, vars), authToken)
		if err != nil {
			return out, clipOpError(op, err)
		}
		var resp struct {
			Data struct {
				User *struct {
					ID    *string `json:"id"`
					Clips *struct {
						Edges []struct {
							TypeName string          `json:"__typename"`
							Cursor   json.RawMessage `json:"cursor"`
							Node     *clipCardNode   `json:"node"`
						} `json:"edges"`
					} `json:"clips"`
				} `json:"user"`
			} `json:"data"`
		}
		if err := json.Unmarshal(respData, &resp); err != nil {
			return out, fmt.Errorf("parse clip list (%s): %w", op, err)
		}
		user := resp.Data.User
		if user != nil && user.ID != nil && *user.ID == "" {
			return out, ErrChannelNotFound
		}
		if user == nil || user.Clips == nil || len(user.Clips.Edges) == 0 {
			return out, nil
		}
		cursor = ""
		for _, e := range user.Clips.Edges {
			if e.TypeName != "ClipEdge" || e.Node == nil || e.Node.TypeName != "Clip" {
				continue
			}
			card, ok := e.Node.card()
			if !ok {
				continue
			}
			// The cursor of the last entry yielded, as _entries keeps it; a
			// non-string cursor ends the listing there. DIFFERENT FROM
			// UPSTREAM: the cursor is reset at the top of each page (above),
			// so a page that yields no clip ends the listing; _entries keeps
			// the previous page's cursor across it and would ask for the
			// same page again, without end if Twitch kept answering it so.
			var c string
			if json.Unmarshal(e.Cursor, &c) == nil {
				cursor = c
			} else {
				cursor = ""
			}
			out = append(out, card)
		}
		if cursor == "" {
			return out, nil
		}
	}
	if a.logger != nil {
		a.logger.Warn("twitch clip list stopped at its page cap", "channel", login, "filter", string(filter), "pages", clipsMaxPages, "clips", len(out))
	}
	return out, nil
}

// clipCardNode is one ClipsCards__User node.
type clipCardNode struct {
	TypeName        string    `json:"__typename"`
	ID              string    `json:"id"`
	Slug            string    `json:"slug"`
	URL             string    `json:"url"`
	Title           string    `json:"title"`
	ThumbnailURL    string    `json:"thumbnailURL"`
	DurationSeconds flexFloat `json:"durationSeconds"`
	CreatedAt       string    `json:"createdAt"`
	ViewCount       flexInt   `json:"viewCount"`
}

// card maps the node to a TwitchClipCard; false when it names no clip
// (yt-dlp skips a node whose url is not a URL). The slug is the one its URL
// names, through the URL table every add path reads, else the node's own.
func (n *clipCardNode) card() (TwitchClipCard, bool) {
	clipURL := httpURL(n.URL)
	if clipURL == "" {
		return TwitchClipCard{}, false
	}
	slug := n.Slug
	if t := utils.ExtractTwitchTarget(clipURL); t != nil && t.Type == utils.TwitchClip {
		slug = t.Value
	}
	if slug == "" {
		return TwitchClipCard{}, false
	}
	return TwitchClipCard{
		ID: n.ID, Slug: slug, URL: clipURL, Title: n.Title,
		ThumbnailURL:    httpURL(n.ThumbnailURL),
		DurationSeconds: int(n.DurationSeconds),
		CreatedAt:       n.CreatedAt,
		ViewCount:       int(n.ViewCount),
	}, true
}

// flexInt and flexFloat decode a number Twitch sends as a JSON number, a
// numeric string or null — yt-dlp's int_or_none / float_or_none — and leave
// zero for anything else, so one odd field never fails the clip.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	var v flexFloat
	_ = v.UnmarshalJSON(b)
	*f = flexInt(v)
	return nil
}

type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	var n json.Number
	if json.Unmarshal(b, &n) != nil {
		var s string
		if json.Unmarshal(b, &s) != nil {
			*f = 0
			return nil
		}
		n = json.Number(s)
	}
	v, err := n.Float64()
	if err != nil {
		v = 0
	}
	*f = flexFloat(v)
	return nil
}

// GetClip reads one clip (API.GetClip) with the jar's auth-token, as yt-dlp's
// _download_base_gql sends the auth-token cookie whenever there is one.
func (s *Service) GetClip(ctx context.Context, slug string) (*TwitchClipInfo, error) {
	return s.API.GetClip(ctx, slug, s.Auth.GetAuthToken())
}

// ListClips lists a channel's clips created within filter (API.ListClips).
func (s *Service) ListClips(ctx context.Context, login string, filter ClipFilter) ([]TwitchClipCard, error) {
	return s.API.ListClips(ctx, login, filter, s.Auth.GetAuthToken())
}
