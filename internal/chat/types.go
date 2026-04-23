package chat

// ChatMessage represents a single chat message.
//
// OffsetMs is the signed millisecond offset relative to stream start. Negative
// values are legitimate for pre-stream "waiting room" chat. HasOffset
// distinguishes "offset actually 0ms" from "offset unknown"; callers should
// check HasOffset before treating OffsetMs as authoritative. Older chat files
// written before HasOffset was introduced deserialize with HasOffset=false,
// which matches the prior semantics (offsetMs=0 was the unset sentinel).
type ChatMessage struct {
	ID              string         `json:"id"`
	TimestampUsec   string         `json:"timestampUsec"`
	TimestampText   string         `json:"timestampText,omitempty"`
	OffsetMs        int64          `json:"offsetMs"`
	HasOffset       bool           `json:"hasOffset,omitempty"`
	AuthorName      string         `json:"authorName"`
	AuthorChannelID string         `json:"authorChannelId"`
	AuthorBadges    []string       `json:"authorBadges,omitempty"`
	Message         []MessagePart  `json:"message"`
	Superchat       *SuperchatInfo `json:"superchat,omitempty"`
	IsMembership    bool           `json:"isMembership,omitempty"`
}

// MessagePart represents a text or emoji segment in a chat message.
type MessagePart struct {
	Type          string `json:"type"`                    // "text" or "emoji"
	Text          string `json:"text,omitempty"`
	EmojiID       string `json:"emojiId,omitempty"`
	EmojiURL      string `json:"emojiUrl,omitempty"`
	IsCustomEmoji bool   `json:"isCustomEmoji,omitempty"`
}

// SuperchatInfo contains super chat/sticker payment details.
type SuperchatInfo struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
	Color    string `json:"color"`
	Tier     int    `json:"tier"`
}

// ChatData is the output format for chat files.
type ChatData struct {
	VideoID          string        `json:"videoId"`
	VideoTitle       string        `json:"videoTitle"`
	ChannelName      string        `json:"channelName"`
	StreamStartTime  string        `json:"streamStartTime,omitempty"`
	DownloadedAt     string        `json:"downloadedAt"`
	MessageCount     int           `json:"messageCount"`
	Messages         []ChatMessage `json:"messages"`
}

// ChatProgress holds progress information for event callbacks.
type ChatProgress struct {
	MessageCount  int
	LastTimestamp string
}

// ChatResumeState holds chat download progress for crash recovery.
type ChatResumeState struct {
	LastTimestampUsec string   `json:"lastTimestampUsec"`
	MessageCount      int      `json:"messageCount"`
	Continuation      string   `json:"continuation"`
	Timestamp         int64    `json:"timestamp"`
	VideoID           string   `json:"videoId"`
	RecentIDs         []string `json:"recentIds"`
}

// Superchat tier color mapping (YouTube's internal ARGB color codes).
// Keys are uint32 since the raw values are 32-bit unsigned ARGB ints.
var superchatTierColors = map[uint32]struct {
	tier  int
	color string
}{
	4280191205: {1, "blue"},
	4278248959: {2, "cyan"},
	4280150454: {3, "green"},
	4294953512: {4, "yellow"},
	4294278144: {5, "orange"},
	4293467747: {6, "magenta"},
	4293271831: {7, "red"},
}
