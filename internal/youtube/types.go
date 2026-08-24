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

	// AttestationChallenge is the session's watch-page BotGuard challenge
	// (see WatchPageResult.AttestationChallenge). Rides on VideoInfo so
	// download strategies can mint session-coherent GVS PO tokens.
	AttestationChallenge string `json:"-"`

	// GvsBinding is the content binding GVS (segment-URL) PO tokens for this
	// video must carry, and GvsBindingKind names the rule that produced it
	// ("videoID", "datasyncID", "visitorData", "channelID"). Resolved once in
	// withAttestation per yt-dlp's get_webpo_content_binding — see
	// GvsContentBinding — so download strategies never re-derive it.
	GvsBinding     string `json:"-"`
	GvsBindingKind string `json:"-"`

	// PublishedAt is the probe's authoritative publish date, status-aware
	// per spec §12 — see extractPublishedAt. RFC3339: a real broadcast
	// start carries its own time, and a bare-date microformat value is
	// normalized to <date>T23:59:59Z (the newest instant consistent with
	// the imprecise value, per the §12 skew-new rule). Empty when the
	// status can't yield one (upcoming/live) or no source was present.
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

	// TargetDurationSec is the per-segment duration (seconds) YouTube
	// advertises for an OTF/live adaptive format — the same value that
	// yt-dlp reads to compute a manifest-free DASH format's segment count.
	// Used by the worker's eviction diagnosis (Task 9) to convert a
	// bisected evicted-segment count into an approximate evicted duration.
	// Zero when the source format omitted it (a whole-file / non-OTF
	// format never carries this field).
	TargetDurationSec int `json:"targetDurationSec,omitempty"`
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
	// GvsBindToVideoID mirrors yt-dlp's gvs_bind_to_video_id: true when the
	// page's player configs carry html5_generate_content_po_token=true, the
	// experiment under which GVS PO tokens bind to the video ID instead of
	// the datasync ID / visitor data. See GvsContentBinding.
	GvsBindToVideoID bool
	Title            string
	Author           string
	ChannelID        string
	Description      string
	ThumbnailURL     string
}

// AuthLevel constants for format deduplication. Lower = preferred when two
// clients return the same itag.
//
// The tier order mirrors yt-dlp's client priority
// (references/yt-dlp/yt_dlp/extractor/youtube/_base.py, build_innertube_clients):
//
//	BASE_CLIENTS = ('tv', 'web', 'mweb', 'android', 'ios')  # highest→lowest
//	priority = 10 * index  =>  tv 40, web 30, mweb 20, android 10, ios 0
//
// android_vr's base client is `android`, so upstream ranks it near the
// BOTTOM. Moombox ranked it at the TOP until 2026-08-15, which meant every
// same-itag tie went to android_vr and live segment downloads ran off
// ANDROID_VR URLs.
//
// That is not a cosmetic difference. ANDROID_VR is absent from yt-dlp's
// WEBPO_CLIENTS, so get_webpo_content_binding produces no WebPO for it at
// all — upstream never attaches one, relying instead on that client's
// not_required_with_player_token policy. Moombox was attaching a
// visitorData-bound WebPO to android_vr URLs, a token type that does not
// apply to them, against a client yt-dlp documents as having "intermittent,
// selective POT enforcement" since 2026.07. Field result: a 403 every ~20s
// on a live archive, each one costing a credential refresh, holding catch-up
// to a quarter of its healthy rate. The same mismatch is the leading
// explanation for the 2026-08-14 premiere that 403'd every segment.
//
// The ANDROID_VR fallback still earns its place — it is the only source of a
// dashManifestUrl when the account experiment withholds one, and the only
// source of formats at all for some restricted videos — it simply must not
// displace a WEB/TV format that carries the same itag.
//
// VISIONOS (added 2026-08-24, yt-dlp 2026.08.19 parity) computes to upstream
// priority -10 — its base client is not in BASE_CLIENTS at all, so
// qualities() returns -1 — placing it below even android/ios. Moombox ranks
// it one notch ABOVE android_vr instead: upstream deleted android_vr from
// its defaults entirely (all-formats 403 enforcement since 2026-08-17), so
// its relative ranking there is vestigial, while Moombox retains android_vr
// as an extra fallback tier behind visionos. Under selective enforcement a
// 403-dead android_vr URL must never win a same-itag tie against a working
// visionos one.
//
// Within a tier the previous relative order is preserved: public watch-page
// formats still rank ahead of authenticated ones (audit I5).
const (
	// TV tier — upstream priority 40. TVHTML5 is a WEBPO client, so its URLs
	// and our WebPO tokens are the matched pair.
	AuthLevelTVPublic = 0
	AuthLevelTVAuth   = 1
	// WEB tier — upstream priority 30. The watch page IS the web client.
	AuthLevelWatchPagePublic = 2
	AuthLevelWatchPageAuth   = 3
	AuthLevelWebSafari       = 4
	AuthLevelWeb             = 5
	AuthLevelWebEmbedded     = 6
	AuthLevelWebCreator      = 7
	// Last-resort tier: cookieless non-WebPO clients. VISIONOS ahead of
	// ANDROID_VR — see the block comment above.
	AuthLevelVisionOS  = 8
	AuthLevelAndroidVR = 9
)

// Sentinel values written by parsePlayerResponse when metadata is missing.
// mergeWatchPageMetadata inspects these to decide whether to overwrite with
// a source value; they must stay in sync with the strings produced in
// parsePlayerResponse.
const (
	UnknownTitleSentinel   = "Unknown Title"
	UnknownChannelSentinel = "Unknown Channel"
)
