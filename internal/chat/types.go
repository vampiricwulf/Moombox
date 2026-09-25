package chat

// ChatMessage represents a single chat message.
//
// OffsetMs is the signed millisecond offset relative to stream start. Negative
// values are legitimate for pre-stream "waiting room" chat. For replay
// messages, YouTube reports videoOffsetTimeMsec = 0 for anything sent before
// the stream started; parseAction recovers the real negative offset from
// TimestampText in that case (parseNegativeTimestampText, N-F2). HasOffset
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

	// MembershipText is a liveChatMembershipItemRenderer's own header line —
	// "Member for 6 months" (headerPrimaryText) for a milestone, "Welcome to
	// Member!" (headerSubtext) for a new member — and the gift-purchase
	// announcement's primaryText ("Gifted 5 memberships"). It is the EVENT;
	// Message is only what the member typed, which a new member or a gift
	// purchase does not have.
	//
	// A separate field rather than a run prepended to Message: the sidebar's
	// member card shows the line beside the author and the message in its
	// body, and the video overlay renders Message alone, so folding it in
	// would start scrolling membership notices across the video (2026-09-25
	// ruling K4, "sidebar only"). Empty on every other kind of message and on
	// every file written before this field existed.
	MembershipText string `json:"membershipText,omitempty"`
}

// MessagePart represents a text or emoji segment in a chat message.
//
// On text segments, URL is the hyperlink target when a YouTube
// navigationEndpoint is attached to the run (audit chat.md E5). Bold and
// Italic reflect YouTube's `bold`/`italics` flags on text runs. Renderers
// that don't care about these fields can ignore them — omitempty keeps the
// JSON shape backward-compatible with pre-E5 chat files.
type MessagePart struct {
	Type          string `json:"type"` // "text" or "emoji"
	Text          string `json:"text,omitempty"`
	URL           string `json:"url,omitempty"`
	Bold          bool   `json:"bold,omitempty"`
	Italic        bool   `json:"italic,omitempty"`
	EmojiID       string `json:"emojiId,omitempty"`
	EmojiURL      string `json:"emojiUrl,omitempty"`
	IsCustomEmoji bool   `json:"isCustomEmoji,omitempty"`
}

// SuperchatInfo contains super chat/sticker payment details.
type SuperchatInfo struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
	// Color is the tier's palette name (blue, cyan, green, yellow, orange,
	// magenta, red), or "gray" when neither renderer color is in the table —
	// gray marks an unresolved tier and is never a resolved one. The actual
	// colors are always in HeaderColor / BodyColor.
	Color string `json:"color"`
	// Tier is YouTube's Super Chat tier, 1 (lowest) to 7; 0 means unknown
	// (owner ruling 2026-09-05: tier 0 IS the marker; no label field).
	Tier int `json:"tier"`
	// Kind names the renderer: "message" (liveChatPaidMessageRenderer) or
	// "sticker" (liveChatPaidStickerRenderer).
	Kind string `json:"kind,omitempty"`
	// HeaderColor and BodyColor are the raw renderer colors as #RRGGBB (alpha
	// dropped): headerBackgroundColor / bodyBackgroundColor for a message,
	// moneyChipBackgroundColor / backgroundColor for a sticker. Recorded
	// whenever present so an unmapped tier can be added to the table later
	// from the archive alone.
	HeaderColor string `json:"headerColor,omitempty"`
	BodyColor   string `json:"bodyColor,omitempty"`
}

// ChatData is the output format for chat files.
//
// DownloadedAt is RFC3339 UTC ("2006-01-02T15:04:05Z") — re-readers should
// parse it with time.RFC3339. Stored as a string rather than time.Time so
// the JSON form stays stable and human-readable. StreamStartTime uses the
// same format when present (audit chat.md U3).
type ChatData struct {
	VideoID         string        `json:"videoId"`
	VideoTitle      string        `json:"videoTitle"`
	ChannelName     string        `json:"channelName"`
	StreamStartTime string        `json:"streamStartTime,omitempty"`
	DownloadedAt    string        `json:"downloadedAt"`
	MessageCount    int           `json:"messageCount"`
	Messages        []ChatMessage `json:"messages"`
}

// ChatProgress holds progress information for event callbacks.
type ChatProgress struct {
	MessageCount  int
	LastTimestamp string
}

// ChatResumeState holds chat download progress for crash recovery.
//
// Older resume files may have a `lastTimestampUsec` field written — that
// field is ignored on load (encoding/json silently drops unknown fields)
// and is no longer written.
type ChatResumeState struct {
	MessageCount int      `json:"messageCount"`
	Continuation string   `json:"continuation"`
	Timestamp    int64    `json:"timestamp"`
	VideoID      string   `json:"videoId"`
	RecentIDs    []string `json:"recentIds"`
	// StreamStartMs is the epoch (ms) every offsetMs in the chat file was
	// computed against. A resumed or adopted run keeps it even when its own
	// options carry a newer start time — one file, one epoch.
	StreamStartMs int64 `json:"streamStartMs,omitempty"`
	// Mode records which kind of run wrote the sidecar: resumeModeLive (the
	// run was live/upcoming) or resumeModeReplay. A replay run must not adopt
	// a live run's sidecar — its count, continuation and dedup IDs describe
	// the live half of a mixed-mode file, and its epoch is the live run's.
	// Empty on sidecars written before this field existed, which keeps the
	// pre-existing behaviour so an upgrade never strands a mid-resume job.
	// Start's mode rule is the only reader.
	Mode string `json:"mode,omitempty"`
}

// ChatResumeState.Mode's values — the kind of run that wrote a sidecar.
const (
	resumeModeLive   = "live"
	resumeModeReplay = "replay"
)

// resumeModeFor names the kind of run writing a sidecar, from the same
// live/upcoming predicate Start's completion rule uses.
func resumeModeFor(liveOrUpcoming bool) string {
	if liveOrUpcoming {
		return resumeModeLive
	}
	return resumeModeReplay
}

// superchatTier is one row of YouTube's Super Chat palette.
type superchatTier struct {
	tier  int
	color string
}

// superchatTierColors maps every ARGB color YouTube paints a Super Chat with
// to its tier — two rows per tier: the HEADER color (headerBackgroundColor on
// a paid message; moneyChipBackgroundColor on a Super Sticker, inferred from
// the field name; endBackgroundColor on the ticker item) and the BODY color
// (bodyBackgroundColor; sticker backgroundColor; ticker startBackgroundColor).
// Every renderer color is looked up here regardless of which field carried
// it, so a swapped field still resolves. Keys are opaque ARGB uint32 exactly
// as the JSON delivers them; USD ranges per tier. The header rows are
// confirmed by a real sample and the live log.
//
// History: until 2026-09-05 the table held ONLY the body rows but was looked
// up with the header value, which can never equal one, so every archived
// Super Chat fell back to tier 1 blue and every sticker to tier 0.
var superchatTierColors = map[uint32]superchatTier{
	// tier 1 blue, $1-1.99
	0xFF1565C0: {1, "blue"}, // header
	0xFF1E88E5: {1, "blue"}, // body
	// tier 2 cyan, $2-4.99
	0xFF00B8D4: {2, "cyan"}, // header
	0xFF00E5FF: {2, "cyan"}, // body
	// tier 3 green, $5-9.99
	0xFF00BFA5: {3, "green"}, // header
	0xFF1DE9B6: {3, "green"}, // body
	// tier 4 yellow, $10-19.99
	0xFFFFB300: {4, "yellow"}, // header
	0xFFFFCA28: {4, "yellow"}, // body
	// tier 5 orange, $20-49.99
	0xFFE65100: {5, "orange"}, // header
	0xFFF57C00: {5, "orange"}, // body
	// tier 6 magenta, $50-99.99
	0xFFC2185B: {6, "magenta"}, // header
	0xFFE91E63: {6, "magenta"}, // body
	// tier 7 red, $100-500 (YouTube's cap; the Node table said $199.99)
	0xFFD00000: {7, "red"}, // header
	0xFFE62117: {7, "red"}, // body
}

// superchatUnknownColor is the Color recorded when neither renderer color is
// in the table (owner ruling 2026-09-05): no row uses gray, so it cannot be
// mistaken for a resolved tier; Tier 0 is the machine-readable marker.
const superchatUnknownColor = "gray"
