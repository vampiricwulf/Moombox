// Package database provides SQLite-based persistence for Moombox.
package database

// JobStatus represents the status of a download job.
type JobStatus string

const (
	// StatusQueued is a resting state: the job waits for an archive slot and
	// is admitted by the scheduler, never by the worker's startup/heartbeat
	// self-scans (worker.ShouldProcess must stay false for it).
	StatusQueued      JobStatus = "Queued"
	StatusUpcoming    JobStatus = "Upcoming"
	StatusLive        JobStatus = "Live"
	StatusDownloading JobStatus = "Downloading"
	StatusMuxing      JobStatus = "Muxing"
	StatusFinished    JobStatus = "Finished"
	StatusError       JobStatus = "Error"
	StatusCancelled   JobStatus = "Cancelled"
	StatusCookies     JobStatus = "COOKIES?"
)

// ParkReason records WHY a job was parked at StatusCookies. The status alone
// says "credentials are the fix", which is true for every value here — but it
// does not say WHICH credentials, and the automatic recovery sweeps need that
// distinction to avoid retrying a job that cannot possibly succeed against the
// credentials currently on disk.
//
// It is deliberately a persisted column rather than a re-derivation from the
// job's error text: the sweep runs minutes, days, or restarts after the park,
// and the error string is user-facing prose that gets reworded (it already has
// been). Routing control flow through UI copy is the failure mode this codebase
// has been bitten by before — see classifyProbeErr's "gql auth failure (401)"
// substring in internal/worker/probe_classify.go.
type ParkReason string

const (
	// ParkReasonNone is the zero value: the job is not parked at
	// StatusCookies, or it parked before this column existed (pre-v18 rows).
	// Sweeps treat it exactly like ParkReasonAuth, which preserves the
	// behavior every legacy COOKIES? row already had.
	ParkReasonNone ParkReason = ""

	// ParkReasonAuth means the request was NOT signed in — the cookies are
	// missing, expired, or dead. Restoring authentication is the whole fix,
	// so the auth-recovered sweep resumes these.
	ParkReasonAuth ParkReason = "auth"

	// ParkReasonMembership means the request WAS signed in and YouTube still
	// refused: the account simply does not hold the channel's membership.
	// Credentials are still the fix, but only DIFFERENT credentials — an
	// account that holds the membership. Restoring or rotating the same
	// session cannot help, so the auth-recovered sweep skips these; only a
	// genuine change of account identity resumes them.
	ParkReasonMembership ParkReason = "membership"
)

// Job is the primary data model for a download job.
type Job struct {
	ID          string    `json:"id"`
	VideoID     string    `json:"videoId"`
	URL         string    `json:"url"`
	Title       string    `json:"title"`
	ChannelName string    `json:"channelName"`
	Platform    string    `json:"platform,omitempty"`
	Status      JobStatus `json:"status"`
	Progress    string    `json:"progress"`
	Percent     float64   `json:"percent"`
	ETA         string    `json:"eta"`
	Speed       string    `json:"speed"`
	CreatedAt   string    `json:"createdAt"`
	UpdatedAt   string    `json:"updatedAt"`
	Error       string    `json:"error,omitempty"`
	// Download state
	LastVideoSeq      *int   `json:"lastVideoSeq,omitempty"`
	LastAudioSeq      *int   `json:"lastAudioSeq,omitempty"`
	TotalVideoSeq     *int   `json:"totalVideoSeq,omitempty"`
	TotalAudioSeq     *int   `json:"totalAudioSeq,omitempty"`
	IsVod             bool   `json:"isVod,omitempty"`
	ManuallyAdded     bool   `json:"manuallyAdded,omitempty"`
	AllowNonStream    bool   `json:"allowNonStream,omitempty"`
	StreamStartTime   string `json:"streamStartTime,omitempty"`
	LengthSeconds     *int   `json:"lengthSeconds,omitempty"`
	StreamEndTime     string `json:"streamEndTime,omitempty"`
	DownloadStartedAt string `json:"downloadStartedAt,omitempty"`
	// Media
	ThumbnailURL    string `json:"thumbnailUrl,omitempty"`
	Description     string `json:"description,omitempty"`
	OutputFile      string `json:"outputFile,omitempty"`
	Filename        string `json:"filename,omitempty"`
	OutputDirectory string `json:"outputDirectory,omitempty"`
	VideoWidth      *int   `json:"videoWidth,omitempty"`
	VideoHeight     *int   `json:"videoHeight,omitempty"`
	VideoFps        *int   `json:"videoFps,omitempty"`
	FileSize        *int64 `json:"fileSize,omitempty"`
	// Chat
	ChatStatus        string `json:"chatStatus,omitempty"`
	TotalChatMessages *int   `json:"totalChatMessages,omitempty"`
	ChatFilename      string `json:"chatFilename,omitempty"`
	ChatFile          string `json:"chatFile,omitempty"`
	// Saved asset files (absolute paths)
	ThumbnailFile   string `json:"thumbnailFile,omitempty"`
	DescriptionFile string `json:"descriptionFile,omitempty"`
	// Gaps
	Gaps []Gap `json:"gaps,omitempty"`
	// Twitch
	TwitchQuality    string `json:"twitchQuality,omitempty"`
	TwitchCategory   string `json:"twitchCategory,omitempty"`
	ChannelAvatarURL string `json:"channelAvatarUrl,omitempty"`
	// Recheck tracking
	LastRecheckAt string `json:"lastRecheckAt,omitempty"`
	// Advanced options
	SelectedVideoItag *int     `json:"selectedVideoItag,omitempty"`
	SelectedAudioItag *int     `json:"selectedAudioItag,omitempty"`
	StartTime         *float64 `json:"startTime,omitempty"`
	EndTime           *float64 `json:"endTime,omitempty"`
	// Quality monitoring
	QualityPreference string `json:"qualityPreference,omitempty"`
	// Watch tracking / player state
	Watched        bool     `json:"watched"`
	ResumePosition *float64 `json:"resumePosition,omitempty"`
	ChatOffset     float64  `json:"chatOffset"`
	// Auto-recovery
	AutoRetryCount int `json:"autoRetryCount,omitempty"`
	// Archive-slots scheduling (spec §10). ChannelID is the monitored channel
	// that created this job — set only by the feed/DECAPI creation path; nil
	// (stored NULL) for Twitch/manual jobs, so an absent affiliation never
	// reads as "". QueuePriority is written EXPLICITLY by every creator
	// (broadcast/new-VOD 0, backlog 1) — the schema DEFAULT 1 exists only for
	// pre-v16 legacy rows and must never be relied on.
	ChannelID     *string `json:"channelId,omitempty"`
	QueuePriority int     `json:"queuePriority,omitempty"`
	// ParkReason is why the job stopped at StatusCookies (see the ParkReason
	// doc comment). Meaningful only while Status == StatusCookies; every path
	// that parks a job rewrites it, and every path that un-parks one clears it.
	ParkReason ParkReason `json:"parkReason,omitempty"`
	// ParkIdentity is an opaque fingerprint of WHICH platform account the job
	// was refused under, recorded when ParkReason is ParkReasonMembership (see
	// cookies.CookieJar.YouTubeIdentity). A membership park resumes when the
	// current account differs from this one — the durable comparison that
	// makes the resume decision independent of any in-process edge, so it
	// survives restarts and cannot be consumed by a missed transition.
	//
	// "" means "parked under an unknown account" (a pre-v19 row, or a park
	// where the identity could not be computed) and resolves permissively:
	// one retry on the next observation rather than a permanent strand.
	//
	// json:"-" on purpose. It is credential-derived and of no use to any UI;
	// the user-facing signal is ParkReason plus the job's error text.
	ParkIdentity string `json:"-"`
	// IncompleteTail marks a Finished job whose recording is known to be missing
	// tail segments (finalized behind head after refresh attempts). Staging +
	// resume sidecar are preserved; Retry/Resume are allowed and clear the flag
	// on a complete re-run.
	IncompleteTail bool `json:"incompleteTail,omitempty"`
	// Trims (loaded via join)
	Trims []TrimRecord `json:"trims,omitempty"`
	// Segments (loaded via join, for multi-segment quality-split jobs)
	Segments []Segment `json:"segments,omitempty"`
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

// JobStats holds aggregate statistics across all jobs.
type JobStats struct {
	FinishedCount     int   `json:"finishedCount"`
	ActiveCount       int   `json:"activeCount"`
	MuxingCount       int   `json:"muxingCount"`
	ErrorCount        int   `json:"errorCount"`
	CancelledCount    int   `json:"cancelledCount"`
	QueuedCount       int   `json:"queuedCount"`
	YouTubeCount      int   `json:"youtubeCount"`
	TwitchCount       int   `json:"twitchCount"`
	FinishedSize      int64 `json:"finishedSize"`
	ErrorSize         int64 `json:"errorSize"`
	CancelledSize     int64 `json:"cancelledSize"`
	YouTubeSize       int64 `json:"youtubeSize"`
	TwitchSize        int64 `json:"twitchSize"`
	TotalDuration     int64 `json:"totalDuration"`
	TotalChatMessages int64 `json:"totalChatMessages"`
}

// Segment represents one part of a multi-part download. Parts are produced
// when stream quality changes mid-download (quality split) or, for Twitch
// live, when segments expired unrecoverably from the CDN (gap split) — each
// captured span is muxed as its own internally-gapless file.
type Segment struct {
	ID              int     `json:"id,omitempty"`
	JobID           string  `json:"jobId,omitempty"`
	SegmentIndex    int     `json:"segmentIndex"`
	UnixStart       int64   `json:"unixStart"`
	UnixEnd         int64   `json:"unixEnd"`
	Quality         string  `json:"quality"`
	Filename        string  `json:"filename"`
	FilePath        string  `json:"filePath,omitempty"`
	FileSize        *int64  `json:"fileSize,omitempty"`
	VideoWidth      *int    `json:"videoWidth,omitempty"`
	VideoHeight     *int    `json:"videoHeight,omitempty"`
	VideoFps        *int    `json:"videoFps,omitempty"`
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
	// ChatFile is the absolute path of this part's chat JSON ("" when the
	// part has no chat — recorded before per-part chat existed, chat
	// disabled, or no messages during the part). Mirrors Job.ChatFile.
	ChatFile string `json:"chatFile,omitempty"`
}

// ClientToken represents a persistent client token for remote auth across restarts.
type ClientToken struct {
	ID          string `json:"id"`
	TokenPrefix string `json:"tokenPrefix"`
	TokenHash   string `json:"-"`
	Label       string `json:"label"`
	CreatedAt   string `json:"createdAt"`
	LastUsedAt  string `json:"lastUsedAt"`
	LastIP      string `json:"lastIp"`
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
