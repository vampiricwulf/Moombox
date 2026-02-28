// Package youtube provides YouTube Innertube API integration.
package youtube

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
	PlayabilityOK             PlayabilityError = "ok"
	PlayabilityMembersOnly    PlayabilityError = "members_only"
	PlayabilityLoginRequired  PlayabilityError = "login_required"
	PlayabilityAgeRestricted  PlayabilityError = "age_restricted"
	PlayabilityUnavailable    PlayabilityError = "unavailable"
	PlayabilityPrivate        PlayabilityError = "private"
	PlayabilityRegionBlocked  PlayabilityError = "region_blocked"
	PlayabilityUnknown        PlayabilityError = "unknown"
)

// VideoInfo contains complete video information from YouTube.
type VideoInfo struct {
	Title              string       `json:"title"`
	ChannelName        string       `json:"channelName"`
	ChannelID          string       `json:"channelId"`
	Description        string       `json:"description"`
	ThumbnailURL       string       `json:"thumbnailUrl,omitempty"`
	Formats            []Format     `json:"formats"`
	PlayerURL          string       `json:"playerUrl"`
	StreamStatus       StreamStatus `json:"streamStatus"`
	IsLive             bool         `json:"isLive"`
	IsUpcoming         bool         `json:"isUpcoming"`
	IsPostLiveDVR      bool         `json:"isPostLiveDVR"`
	LengthSeconds      *int         `json:"lengthSeconds,omitempty"`
	EndTimestamp       string       `json:"endTimestamp,omitempty"`
	ScheduledStartTime string       `json:"scheduledStartTime,omitempty"`
	DashManifestURL    string       `json:"dashManifestUrl,omitempty"`
	HlsManifestURL     string       `json:"hlsManifestUrl,omitempty"`
	PlayabilityError   PlayabilityError `json:"playabilityError,omitempty"`
	PlayabilityReason  string       `json:"playabilityReason,omitempty"`
}

// Format contains video/audio format information from YouTube API.
type Format struct {
	Itag           int    `json:"itag"`
	URL            string `json:"url,omitempty"`
	MimeType       string `json:"mimeType"`
	Bitrate        int    `json:"bitrate"`
	Width          *int   `json:"width,omitempty"`
	Height         *int   `json:"height,omitempty"`
	ContentLength  string `json:"contentLength,omitempty"`
	QualityLabel   string `json:"qualityLabel,omitempty"`
	AudioQuality   string `json:"audioQuality,omitempty"`
	AudioSampleRate string `json:"audioSampleRate,omitempty"`
	Fps            *int   `json:"fps,omitempty"`
	Source         string `json:"source,omitempty"`
	AuthLevel      *int   `json:"authLevel,omitempty"`
}

// IsVideo returns true if this format contains video.
func (f *Format) IsVideo() bool {
	return f.Width != nil || f.Height != nil
}

// IsAudio returns true if this format contains audio only.
func (f *Format) IsAudio() bool {
	return f.AudioQuality != "" && f.Width == nil
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

// AuthLevel constants for format deduplication.
const (
	AuthLevelAndroidVR  = 0
	AuthLevelWatchPage  = 1
	AuthLevelTVPublic   = 2
	AuthLevelTVAuth     = 3
	AuthLevelWeb        = 4
	AuthLevelWebCreator = 5
)

// CreateEmptyVideoInfo creates a minimal valid VideoInfo for error paths.
func CreateEmptyVideoInfo() *VideoInfo {
	return &VideoInfo{
		Title:        "Unknown",
		ChannelName:  "Unknown",
		ChannelID:    "",
		Description:  "",
		Formats:      []Format{},
		PlayerURL:    "",
		StreamStatus: StreamNotAStream,
		IsLive:       false,
		IsUpcoming:   false,
		IsPostLiveDVR: false,
	}
}
