// Package database provides SQLite-based persistence for Moombox.
package database

// JobStatus represents the status of a download job.
type JobStatus string

const (
	StatusUpcoming    JobStatus = "Upcoming"
	StatusLive        JobStatus = "Live"
	StatusDownloading JobStatus = "Downloading"
	StatusMuxing      JobStatus = "Muxing"
	StatusFinished    JobStatus = "Finished"
	StatusError       JobStatus = "Error"
	StatusCancelled   JobStatus = "Cancelled"
	StatusCookies     JobStatus = "COOKIES?"
)

// Job is the primary data model for a download job.
type Job struct {
	ID                string     `json:"id"`
	VideoID           string     `json:"videoId"`
	URL               string     `json:"url"`
	Title             string     `json:"title"`
	ChannelName       string     `json:"channelName"`
	Platform          string     `json:"platform,omitempty"`
	Status            JobStatus  `json:"status"`
	Progress          string     `json:"progress"`
	Percent           float64    `json:"percent"`
	ETA               string     `json:"eta"`
	Speed             string     `json:"speed"`
	CreatedAt         string     `json:"createdAt"`
	UpdatedAt         string     `json:"updatedAt"`
	Error             string     `json:"error,omitempty"`
	// Download state
	LastVideoSeq      *int       `json:"lastVideoSeq,omitempty"`
	LastAudioSeq      *int       `json:"lastAudioSeq,omitempty"`
	TotalVideoSeq     *int       `json:"totalVideoSeq,omitempty"`
	TotalAudioSeq     *int       `json:"totalAudioSeq,omitempty"`
	IsVod             bool       `json:"isVod,omitempty"`
	ManuallyAdded     bool       `json:"manuallyAdded,omitempty"`
	AllowNonStream    bool       `json:"allowNonStream,omitempty"`
	StreamStartTime   string     `json:"streamStartTime,omitempty"`
	LengthSeconds     *int       `json:"lengthSeconds,omitempty"`
	StreamEndTime     string     `json:"streamEndTime,omitempty"`
	DownloadStartedAt string     `json:"downloadStartedAt,omitempty"`
	// Media
	ThumbnailURL      string     `json:"thumbnailUrl,omitempty"`
	Description       string     `json:"description,omitempty"`
	OutputFile        string     `json:"outputFile,omitempty"`
	Filename          string     `json:"filename,omitempty"`
	OutputDirectory   string     `json:"outputDirectory,omitempty"`
	VideoWidth        *int       `json:"videoWidth,omitempty"`
	VideoHeight       *int       `json:"videoHeight,omitempty"`
	VideoFps          *int       `json:"videoFps,omitempty"`
	FileSize          *int64     `json:"fileSize,omitempty"`
	// Chat
	ChatStatus        string     `json:"chatStatus,omitempty"`
	TotalChatMessages *int       `json:"totalChatMessages,omitempty"`
	ChatFilename      string     `json:"chatFilename,omitempty"`
	ChatFile          string     `json:"chatFile,omitempty"`
	// Saved asset files (absolute paths)
	ThumbnailFile     string     `json:"thumbnailFile,omitempty"`
	DescriptionFile   string     `json:"descriptionFile,omitempty"`
	// Gaps
	Gaps              []Gap      `json:"gaps,omitempty"`
	// Twitch
	TwitchQuality     string     `json:"twitchQuality,omitempty"`
	TwitchCategory    string     `json:"twitchCategory,omitempty"`
	ChannelAvatarURL  string     `json:"channelAvatarUrl,omitempty"`
	// Recheck tracking
	LastRecheckAt     string     `json:"lastRecheckAt,omitempty"`
	// Advanced options
	SelectedVideoItag *int       `json:"selectedVideoItag,omitempty"`
	SelectedAudioItag *int       `json:"selectedAudioItag,omitempty"`
	StartTime         *float64   `json:"startTime,omitempty"`
	EndTime           *float64   `json:"endTime,omitempty"`
	// Trims (loaded via join)
	Trims             []TrimRecord `json:"trims,omitempty"`
}

// IsTerminal returns true if the job status is a terminal state.
func (j *Job) IsTerminal() bool {
	return j.Status == StatusFinished || j.Status == StatusError || j.Status == StatusCancelled
}

// Gap represents a missing segment range in a download.
type Gap struct {
	ID     int    `json:"id,omitempty"`
	JobID  string `json:"jobId,omitempty"`
	From   int    `json:"from"`
	To     int    `json:"to"`
	Stream string `json:"stream"` // "video" or "audio"
}

// TrimRecord represents a trimmed clip created from a downloaded video.
type TrimRecord struct {
	ID        string  `json:"id"`
	JobID     string  `json:"jobId,omitempty"`
	StartTime float64 `json:"startTime"`
	EndTime   float64 `json:"endTime"`
	Filename  string  `json:"filename"`
	CreatedAt string  `json:"createdAt"`
	Duration  float64 `json:"duration"`
	FileSize  *int64  `json:"fileSize,omitempty"`
}
