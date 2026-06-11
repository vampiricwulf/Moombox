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
	IsSource   bool    `json:"isSource"`
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
	ID                string           `json:"id"`
	TimestampMs       int64            `json:"timestampMs"`
	OffsetMs          int64            `json:"offsetMs"`
	AuthorName        string           `json:"authorName"`
	AuthorID          string           `json:"authorId"`
	AuthorBadges      []string         `json:"authorBadges,omitempty"`
	AuthorColor       string           `json:"authorColor,omitempty"`
	Message           string           `json:"message"`
	Emotes            []TwitchEmoteRef `json:"emotes,omitempty"`
	Bits              int              `json:"bits,omitempty"`
	MessageType       string           `json:"messageType"` // "chat", "sub", "resub", "subgift", "raid", "announcement", "bits", "system"
	SystemMsg         string           `json:"systemMsg,omitempty"`
	SubPlan           string           `json:"subPlan,omitempty"`           // C1: "1000", "2000", "3000", "Prime"
	GiftRecipient     string           `json:"giftRecipient,omitempty"`     // C1: msg-param-recipient-display-name
	ViewerCount       int              `json:"viewerCount,omitempty"`       // C1: msg-param-viewerCount (raids)
	AnnouncementColor string           `json:"announcementColor,omitempty"` // msg-param-color for announcements: "primary"|"blue"|"green"|"orange"|"purple"
	Raw               string           `json:"raw,omitempty"`               // Lossless raw IRC line
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
	Platform           string              `json:"platform"` // "twitch"
	ChannelLogin       string              `json:"channelLogin"`
	ChannelDisplayName string              `json:"channelDisplayName"`
	StreamID           string              `json:"streamId"`
	StreamStartTime    string              `json:"streamStartTime,omitempty"`
	RecordingStartTime string              `json:"recordingStartTime,omitempty"`
	DownloadedAt       string              `json:"downloadedAt"`
	MessageCount       int                 `json:"messageCount"`
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
	ID                   string
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
type ChatResumeState struct {
	MessageCount      int      `json:"messageCount"`
	LastTimestampMs   int64    `json:"lastTimestampMs"`
	LastOffsetSeconds float64  `json:"lastOffsetSeconds"`
	Timestamp         int64    `json:"timestamp"`
	StreamID          string   `json:"streamId"`
	RecentIDs         []string `json:"recentIds"`
}
