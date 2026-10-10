// Package twitch provides Twitch GQL API, chat, and HLS integration.
package twitch

// TwitchStreamInfo contains live stream information from Twitch GQL.
type TwitchStreamInfo struct {
	StreamID           string `json:"streamId"`
	ChannelLogin       string `json:"channelLogin"`
	ChannelDisplayName string `json:"channelDisplayName"`
	ChannelID          string `json:"channelId"`
	Title              string `json:"title"`
	GameCategory       string `json:"gameCategory,omitempty"`
	ThumbnailURL       string `json:"thumbnailUrl,omitempty"`
	ViewerCount        int    `json:"viewerCount,omitempty"`
	StartedAt          string `json:"startedAt,omitempty"`
	ProfileImageURL    string `json:"profileImageUrl,omitempty"`
	IsLive             bool   `json:"isLive"`
	StreamType         string `json:"streamType,omitempty"`    // Normalized: "live" or "rerun"
	RawStreamType      string `json:"rawStreamType,omitempty"` // Original from Twitch (e.g. "watchparty", "premiere") — preserved for diagnostics. Audit-finding #29.
}

// TwitchAccessToken holds HLS access credentials.
type TwitchAccessToken struct {
	Value     string `json:"value"`
	Signature string `json:"signature"`
}

// TwitchHLSVariant represents an HLS quality variant from the master playlist.
type TwitchHLSVariant struct {
	URL        string  `json:"url"`
	Name       string  `json:"name"`
	Bandwidth  int     `json:"bandwidth"`
	Width      int     `json:"width,omitempty"`
	Height     int     `json:"height,omitempty"`
	FPS        float64 `json:"fps,omitempty"`
	VideoGroup string  `json:"videoGroup,omitempty"`
	// Codecs is the raw CODECS attribute (RFC 6381 ids, comma-separated) and
	// VideoCodec the normalized family derived from it: "av01", "hevc",
	// "avc1", or "" when the playlist carries no CODECS or the variant has no
	// video track. A pre-enhanced playlist lists only H.264 renditions, so
	// every source in one reports "avc1"; the enhanced-broadcast opt-in (see
	// BuildUsherLiveURL) is what can add an "hevc" or "av01" source beside it.
	Codecs     string `json:"codecs,omitempty"`
	VideoCodec string `json:"videoCodec,omitempty"`
	IsSource   bool   `json:"isSource"`
}

// TwitchVodInfo contains VOD metadata from Twitch GQL.
type TwitchVodInfo struct {
	VodID              string `json:"vodId"`
	Title              string `json:"title"`
	ChannelLogin       string `json:"channelLogin"`
	ChannelDisplayName string `json:"channelDisplayName"`
	ChannelID          string `json:"channelId,omitempty"`
	Duration           int    `json:"duration"` // seconds
	ThumbnailURL       string `json:"thumbnailUrl,omitempty"`
	CreatedAt          string `json:"createdAt,omitempty"`
	ViewCount          int    `json:"viewCount,omitempty"`
	GameCategory       string `json:"gameCategory,omitempty"`
}

// TwitchChatMessage represents a single Twitch chat message.
type TwitchChatMessage struct {
	ID          string `json:"id"`
	TimestampMs int64  `json:"timestampMs"`
	// OffsetMs is the SIGNED ms offset from the part's recording start (negative = before recording began).
	OffsetMs     int64            `json:"offsetMs"`
	AuthorName   string           `json:"authorName"`
	AuthorID     string           `json:"authorId"`
	AuthorBadges []string         `json:"authorBadges,omitempty"`
	AuthorColor  string           `json:"authorColor,omitempty"`
	Message      string           `json:"message"`
	Emotes       []TwitchEmoteRef `json:"emotes,omitempty"`
	Bits         int              `json:"bits,omitempty"`
	MessageType  string           `json:"messageType"` // "chat", "sub", "resub", "subgift", "raid", "announcement", "bits", "system"
	// IsAction marks a /me message. Twitch sends those as the CTCP form
	// \x01ACTION <text>\x01; parsePrivmsg unwraps them so Message holds only
	// the text and the emote offsets (which index the UNWRAPPED text — see
	// parseEmoteTags) line up. Raw keeps the verbatim wire line.
	IsAction          bool   `json:"isAction,omitempty"`
	SystemMsg         string `json:"systemMsg,omitempty"`
	SubPlan           string `json:"subPlan,omitempty"`           // C1: "1000", "2000", "3000", "Prime"
	GiftRecipient     string `json:"giftRecipient,omitempty"`     // C1: msg-param-recipient-display-name
	ViewerCount       int    `json:"viewerCount,omitempty"`       // C1: msg-param-viewerCount (raids)
	AnnouncementColor string `json:"announcementColor,omitempty"` // msg-param-color for announcements: "primary"|"blue"|"green"|"orange"|"purple"
	Raw               string `json:"raw,omitempty"`               // Lossless raw IRC line
}

// TwitchEmoteRef references an emote within a message.
type TwitchEmoteRef struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// TwitchChatData is the complete chat data structure for a Twitch stream.
type TwitchChatData struct {
	Platform           string `json:"platform"` // "twitch"
	ChannelLogin       string `json:"channelLogin"`
	ChannelDisplayName string `json:"channelDisplayName"`
	StreamID           string `json:"streamId"`
	StreamStartTime    string `json:"streamStartTime,omitempty"`
	RecordingStartTime string `json:"recordingStartTime,omitempty"`
	DownloadedAt       string `json:"downloadedAt"`
	MessageCount       int    `json:"messageCount"`
	// EmoteOffsets names the index space TwitchEmoteRef.Start/End count in.
	// "utf16" is the only value any writer here produces, and its ABSENCE is
	// what the player reads as "written before 2026-09-15, when the live IRC
	// path mistook Twitch's code-point offsets for UTF-16 units" — an unmarked
	// file's IRC messages are corrected at load (correctLegacyTwitchEmotes,
	// web/public/modules/chat-timeline.js).
	//
	// It is a HEADER SCALAR and must stay one, before "emotes"/"messages":
	// chatFileRecordingBaseMs stops its scan at the first composite value, and
	// AppendChatMessages splices at the file's last ']'.
	//
	// KNOWN WINDOW: only the two full-file writers set it, so a part file
	// created before this change and APPENDED to after it keeps an unmarked
	// header while its new messages already carry UTF-16 offsets. Those few
	// messages are over-shifted at replay. Rewriting a marathon part's whole
	// file on resume, or carrying a second "keep emitting legacy offsets"
	// parser mode, both cost more than the defect; the window closes at the
	// next part roll or at job end.
	EmoteOffsets string `json:"emoteOffsets,omitempty"`
	// Emotes must serialize BEFORE Messages: AppendChatMessages locates the
	// messages array via "last ] in the file tail", so the messages array has
	// to stay the final field. With emotes trailing, any append after emote
	// enrichment would splice chat into the emotes block and corrupt the JSON.
	Emotes   *TwitchEmoteData    `json:"emotes,omitempty"`
	Messages []TwitchChatMessage `json:"messages"`
}

// TwitchEmoteData holds resolved third-party emotes.
type TwitchEmoteData struct {
	BTTV    []EmoteInfo `json:"bttv,omitempty"`
	FFZ     []EmoteInfo `json:"ffz,omitempty"`
	SevenTV []EmoteInfo `json:"seventv,omitempty"`
}

// EmoteInfo holds emote metadata from third-party services.
type EmoteInfo struct {
	ID   string `json:"id"`
	Code string `json:"code"`
	URL  string `json:"url"`
}

// VodCommentEdge represents a single VOD comment from GQL pagination.
type VodCommentEdge struct {
	ID string
	// Cursor is the Relay edge cursor Twitch sends beside the node. It is the
	// ONLY reliable way to page a VOD: contentOffsetSeconds is an integer
	// second, and a second of a busy VOD holds more comments than one page, so
	// an offset-based next-page request asks for the page it just read.
	Cursor               string
	ContentOffsetSeconds float64
	CommenterDisplayName string
	CommenterID          string
	CommenterLogin       string
	MessageText          string
	Emotes               []TwitchEmoteRef
	UserBadges           []string
	UserColor            string
}

// ChatResumeState holds Twitch chat resume information.
//
// MessageCount counts messages in the CURRENT chat file. TotalCount is the
// cumulative count across all part files of the job (equal to MessageCount
// until the first RollFile). States written before part-splitting existed
// lack TotalCount — readers fall back to MessageCount, which was cumulative
// by definition when there was only ever one file.
//
// Timestamp is epoch MILLISECONDS on BOTH paths. It exists only so a human
// reading a sidecar can see when it was written — nothing loads it — and until
// sweep 2 the IRC writer used milliseconds while the VOD writer used seconds,
// so two files in the same staging tree disagreed about the unit by a factor
// of a thousand (TWITCH-8).
type ChatResumeState struct {
	MessageCount      int      `json:"messageCount"`
	TotalCount        int      `json:"totalCount,omitempty"`
	RollUnwritten     int      `json:"rollUnwritten,omitempty"` // IRC only: ChatDownloader.rollUnwritten
	LastTimestampMs   int64    `json:"lastTimestampMs"`
	LastOffsetSeconds float64  `json:"lastOffsetSeconds"`
	Timestamp         int64    `json:"timestamp"`
	StreamID          string   `json:"streamId"`
	RecentIDs         []string `json:"recentIds"`
}
