// Package youtube provides YouTube Innertube API integration.
package youtube

import "strings"

// StreamStatus indicates the current state of a YouTube video.
type StreamStatus string

const (
	StreamLive       StreamStatus = "live"
	StreamUpcoming   StreamStatus = "upcoming"
	StreamVOD        StreamStatus = "vod"
	StreamPostLive   StreamStatus = "post_live"
	StreamNotAStream StreamStatus = "not_a_stream"
)

// PlayabilityError types from YouTube API.
type PlayabilityError string

const (
	PlayabilityOK            PlayabilityError = "ok"
	PlayabilityMembersOnly   PlayabilityError = "members_only"
	PlayabilityLoginRequired PlayabilityError = "login_required"
	PlayabilityAgeRestricted PlayabilityError = "age_restricted"
	PlayabilityUnavailable   PlayabilityError = "unavailable"
	PlayabilityPrivate       PlayabilityError = "private"
	PlayabilityRegionBlocked PlayabilityError = "region_blocked"
	PlayabilityUnknown       PlayabilityError = "unknown"
)

// VideoInfo contains complete video information from YouTube.
type VideoInfo struct {
	Title              string           `json:"title"`
	ChannelName        string           `json:"channelName"`
	ChannelID          string           `json:"channelId"`
	Description        string           `json:"description"`
	ThumbnailURL       string           `json:"thumbnailUrl,omitempty"`
	Formats            []Format         `json:"formats"`
	PlayerURL          string           `json:"playerUrl"`
	StreamStatus       StreamStatus     `json:"streamStatus"`
	IsLive             bool             `json:"isLive"`
	IsUpcoming         bool             `json:"isUpcoming"`
	IsPostLiveDVR      bool             `json:"isPostLiveDVR"`
	LengthSeconds      *int             `json:"lengthSeconds,omitempty"`
	EndTimestamp       string           `json:"endTimestamp,omitempty"`
	ScheduledStartTime string           `json:"scheduledStartTime,omitempty"`
	DashManifestURL    string           `json:"dashManifestUrl,omitempty"`
	HlsManifestURL     string           `json:"hlsManifestUrl,omitempty"`
	PlayabilityError   PlayabilityError `json:"playabilityError,omitempty"`
	PlayabilityReason  string           `json:"playabilityReason,omitempty"`

	// PublishedAt is the probe's authoritative publish date (RFC3339 for a
	// real broadcast start, YYYY-MM-DD for a microformat upload/publish
	// date), status-aware per spec §12 — see extractPublishedAt. Empty when
	// the status can't yield one (upcoming/live) or no source was present.
	PublishedAt string `json:"publishedAt,omitempty"`
	// PublishedPrecision describes what PublishedAt represents: "started"
	// (a real liveBroadcastDetails.startTimestamp, vod/post_live only) or
	// "day" (microformat uploadDate/publishDate fallback). Empty when
	// PublishedAt is empty.
	PublishedPrecision string `json:"publishedPrecision,omitempty"`
}

// Format contains video/audio format information from YouTube API.
//
// URL holds either a fully-resolved direct URL (when the YouTube response
// shipped one inline) OR the raw `url=` value parsed out of a
// signatureCipher entry — in the latter case EncryptedSig is set and the
// format is not fetchable until cipher.ResolveFormatURL has been called
// to append the decrypted signature and decrypt the n-param. Both URL
// shapes also carry an encrypted n-param that ResolveFormatURL must
// decrypt before the URL can be used.
//
// Stage 3 of the cipher pipeline rework defers cipher decryption from
// parse-time to post-selection so we don't pay for decrypting 7-26
// formats per stream when only 1-2 will actually be used. See
// docs/plans/cipher-pipeline-rework.md.
type Format struct {
	Itag            int    `json:"itag"`
	URL             string `json:"url,omitempty"`
	MimeType        string `json:"mimeType"`
	Bitrate         int    `json:"bitrate"`
	Width           *int   `json:"width,omitempty"`
	Height          *int   `json:"height,omitempty"`
	ContentLength   string `json:"contentLength,omitempty"`
	QualityLabel    string `json:"qualityLabel,omitempty"`
	AudioQuality    string `json:"audioQuality,omitempty"`
	AudioSampleRate string `json:"audioSampleRate,omitempty"`
	Fps             *int   `json:"fps,omitempty"`
	Source          string `json:"source,omitempty"`
	AuthLevel       *int   `json:"authLevel,omitempty"`

	// EncryptedSig is the `s` field from a signatureCipher entry, captured
	// at parse-time and decrypted on demand by cipher.ResolveFormatURL.
	// Empty when the format originated from a direct URL response (no sig
	// needed).
	EncryptedSig string `json:"encryptedSig,omitempty"`
	// SigKey is the `sp` field from a signatureCipher entry, defaulting to
	// "signature" when absent. Used as the URL parameter name when
	// appending the decrypted sig.
	SigKey string `json:"sigKey,omitempty"`
}

// IsVideo returns true if this format contains video.
func (f *Format) IsVideo() bool {
	return f.Width != nil || f.Height != nil
}

// IsAudio returns true if this format contains audio only. Uses mimeType as
// the primary signal because some Innertube responses (notably TV client
// adaptiveFormats) omit audioQuality entirely — relying on AudioQuality alone
// would misclassify those as non-audio and trip hasAdequateFormats into an
// unnecessary ANDROID_VR fallback.
func (f *Format) IsAudio() bool {
	return strings.Contains(f.MimeType, "audio") && f.Width == nil
}

// MaxDimension returns max(width, height) for resolution comparison.
func (f *Format) MaxDimension() int {
	w, h := 0, 0
	if f.Width != nil {
		w = *f.Width
	}
	if f.Height != nil {
		h = *f.Height
	}
	if w > h {
		return w
	}
	return h
}

// YtcfgData holds configuration extracted from YouTube watch page.
type YtcfgData struct {
	PlayerURL          string
	VisitorData        string
	SessionIndex       *int
	DelegatedSessionID string
	DataSyncID         string
	Title              string
	Author             string
	ChannelID          string
	Description        string
	ThumbnailURL       string
}

// AuthLevel constants for format deduplication. Lower = preferred during
// dedup tiebreaks (more direct / less rewrapped). Public watch-page formats
// rank ahead of authenticated watch-page formats so cookie-bearing fetches
// can win the dedup against a stray un-cookied parse (audit I5).
const (
	AuthLevelAndroidVR       = 0
	AuthLevelWatchPagePublic = 1
	AuthLevelWatchPageAuth   = 2
	AuthLevelTVPublic        = 3
	AuthLevelTVAuth          = 4
	AuthLevelWebSafari       = 5
	AuthLevelWeb             = 6
	AuthLevelWebEmbedded     = 7
	AuthLevelWebCreator      = 8
)

// Sentinel values written by parsePlayerResponse when metadata is missing.
// mergeWatchPageMetadata inspects these to decide whether to overwrite with
// a source value; they must stay in sync with the strings produced in
// parsePlayerResponse.
const (
	UnknownTitleSentinel   = "Unknown Title"
	UnknownChannelSentinel = "Unknown Channel"
)
