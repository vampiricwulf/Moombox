// Package database provides SQLite-based persistence for Moombox.
package database

import "slices"

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

// ParkReason records WHY a job was parked at StatusCookies (ParkReasonAuth,
// ParkReasonMembership, or the legacy ParkReasonNone) — and, for three cases,
// why it stopped in StatusError: ParkReasonTwitchEndUnconfirmed,
// ParkReasonTwitchUnreachable and ParkReasonMuxAfterCancel are the only
// values an Error row carries, and no COOKIES? row carries them. For the
// COOKIES? values the status alone says "credentials are the fix", which is
// true for each of them — but it does not say WHICH credentials, and the
// automatic recovery sweeps need that distinction to avoid retrying a job that
// cannot possibly succeed against the credentials currently on disk.
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

	// ParkReasonTwitchEndUnconfirmed is the first of the three ERROR-row reasons
	// (owner decision D-T4): a live Twitch download failed while nothing said
	// its broadcast was over, so the job stopped in Error with its staging and
	// resume sidecar kept (ExecuteTwitch's unconfirmed-end latch). It is what
	// lets the Twitch monitor mux that staging automatically once it confirms
	// the broadcast is over — offline, or live as a different broadcast —
	// exactly as the Mux action would, and only for these rows: the marker is
	// written by the latch, never inferred from the error text, and the
	// automatic mux clears it before it starts, so it runs once per failure.
	// No COOKIES? sweep reads an Error row, so the credential sweeps never see
	// it.
	ParkReasonTwitchEndUnconfirmed ParkReason = "twitch_end_unconfirmed"

	// ParkReasonTwitchUnreachable is the second ERROR-row reason: a
	// Twitch live job's stream processing never reached Twitch — a transport
	// failure, on a network the connectivity monitor says is up (one it calls
	// an outage is waited out instead, and the job never stops). Like the
	// offline flap's TwitchOfflineErrMsg, it is what lets the Twitch monitor
	// re-initialise the job when it finds the same broadcast live again
	// (jobstate.TwitchAutoRetryPending → AutoReinitializeJob), within the same retry
	// budget and only while no segment was downloaded. Written by setJobError
	// from the error's type, never inferred from its text; the
	// re-initialisation clears it.
	ParkReasonTwitchUnreachable ParkReason = "twitch_unreachable"

	// ParkReasonMuxAfterCancel is the third ERROR-row reason: the
	// operator's Mux of a Cancelled row failed (MuxJob). The Cancel closed a
	// live Twitch capture's gap-fill tail (owner decision FQ-2), and CancelJob
	// writes nothing on the row but its status, which the failed Mux then
	// rewrites to Error — so without this marker the next Mux read the row as
	// a capture cut off mid-broadcast and filled the rest of the broadcast
	// the operator cancelled. Written by MuxJob's failure writes only, and
	// carried forward by them while the Mux keeps failing; a Retry or a
	// Reinitialize clears it with every other park_reason, which is what ends
	// the Cancel's close. Read by twitchMuxTailOpen alone.
	ParkReasonMuxAfterCancel ParkReason = "mux_after_cancel"
)

// ChatStatus is a job's chat_status: the lifecycle of its chat capture, the
// one closed vocabulary both UIs render verbatim (the dashboard's
// CHAT_STATUS_VARIANTS / chatStatusVariant in web/public/modules/utils.js
// mirrors the set, pinned by
// cmd/moombox TestChatStatusSetMatchesTheDashboard). The bytes on disk and on
// the wire are the constants' values.
//
// The order a run moves through is "" → pending (a downloader is set up) →
// downloading (it received its first batch) → one verdict: finished,
// incomplete (the capture stopped short) or unavailable (the video has no
// chat, or a capture that saw no messages). Reinitialize resets it to "".
// Two writes settle a capture its run left mid-flight, by one rule (worker
// stoppedChatStatus: downloading → incomplete, pending → ""): a user Cancel
// and a YouTube download Error (owner rulings R-E1, R-E1a,
// DownloadWorker.settleErrorChat), each conditional on the row.
type ChatStatus string

const (
	// ChatNone is the zero value: no chat downloader has run for this job,
	// or Reinitialize reset it.
	ChatNone ChatStatus = ""
	// ChatPending is a downloader that is set up and has not received a batch.
	ChatPending ChatStatus = "pending"
	// ChatDownloading is a downloader receiving messages.
	ChatDownloading ChatStatus = "downloading"
	// ChatUnavailable is a video with no chat, or a capture that ended with
	// no messages.
	ChatUnavailable ChatStatus = "unavailable"
	// ChatFinished is a capture that completed.
	ChatFinished ChatStatus = "finished"
	// ChatIncomplete is a capture that ENDED WITHOUT COMPLETING — the
	// downloader returned an error (a Twitch VOD paging stall, an IRC session
	// that exhausted its reconnect budget, a panic) instead of running out of
	// chat to fetch, a user Cancel cut it off, or a YouTube download Error
	// stopped it (R-E1) — so messages can be missing. Its resume sidecar is
	// kept in staging. The value is the label: both UIs render it verbatim
	// (warning colour), and "error" was not reused for it — nothing writes
	// that, and it reads as a hard failure rather than an archive that
	// stopped short.
	ChatIncomplete ChatStatus = "incomplete"
)

// ChatStatuses is every ChatStatus, in lifecycle order. The parity test reads
// it against the dashboard's map.
var ChatStatuses = []ChatStatus{ChatNone, ChatPending, ChatDownloading, ChatUnavailable, ChatFinished, ChatIncomplete}

// InFlight reports whether s is a capture that has not reached a verdict:
// pending or downloading. A run that ends without recording one leaves it on
// the row until something settles it: these are the values the Cancel and
// Error settles map (worker stoppedChatStatus), and settleErrorChat iterates
// ChatStatuses through this predicate for its from-set.
func (s ChatStatus) InFlight() bool {
	return s == ChatPending || s == ChatDownloading
}

// Job is the primary data model for a download job.
type Job struct {
	// Version is the write that produced this copy of the row: a number
	// drawn under db.mu by UpdateJobFields and AddJob, so a larger one is a
	// later state. Both notify AFTER releasing the lock, so two writers can
	// reach subscribers in the opposite order to their writes — a progress
	// tick read back before a Muxing write, delivered after it, put the row
	// back to Downloading on every dashboard. Subscribers that keep a copy
	// (the WebSocket hub, the TUI) drop a row older than one they hold.
	// Zero on rows read any other way (GetJob, GetAllJobs): no claim either
	// way. Not serialised.
	Version uint64 `json:"-"`

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
	ChatStatus        ChatStatus `json:"chatStatus,omitempty"`
	TotalChatMessages *int       `json:"totalChatMessages,omitempty"`
	ChatFilename      string     `json:"chatFilename,omitempty"`
	ChatFile          string     `json:"chatFile,omitempty"`
	// Saved asset files (absolute paths)
	ThumbnailFile   string `json:"thumbnailFile,omitempty"`
	DescriptionFile string `json:"descriptionFile,omitempty"`
	// Gaps
	Gaps []Gap `json:"gaps,omitempty"`
	// Twitch
	//
	// TwitchQuality is the variant the capture is actually recording, by its
	// playlist name ("chunked", "720p60"): written when the capture starts and
	// again whenever a split moves it to another variant. Both UIs show it as
	// "Quality". It is NOT the preference and nothing selects from it — it used
	// to be both, set to the preference at creation and read back as one by
	// the next selection after the stream start had overwritten it. The
	// preference is QualityPreference, on Twitch rows as on YouTube ones.
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
	// QualityPreference is the quality the job was created to record: the
	// channel's quality_preference, or the manual add's. Written at creation
	// and never overwritten. Every variant selection reads it — on Twitch the
	// capture start's and every re-selection during the capture, never
	// TwitchQuality. A Twitch row records "best" when nothing was named; an
	// empty value, which rows from before that rule can hold, selects as
	// "best" too.
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
	// ParkReason is why the job stopped at StatusCookies, or — for the three
	// Error-row values — in StatusError (see the ParkReason doc comment).
	// setJobError and the park paths write it (ParkReasonNone for a failure
	// with no reason to record), and the un-park and restart paths clear it.
	// Not every Error write touches it: the processJob panic recoveries write
	// only status and error, and a failed operator Mux of an Error row adds a
	// reason only for a Cancelled or mux-after-cancel prior, so either leaves the
	// reason the row already had (a twitch_unreachable or
	// twitch_end_unconfirmed among them).
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
	// AnnounceFound says this job's "Stream Found" is still owed. The YouTube
	// monitor's creator sets it in the INSERT that creates the job; the
	// worker's first metadata fetch takes it (TakeAnnounceFound) and sends the
	// embed with what that fetch learned; a cancel clears it. Living in the row
	// is what makes it exact: a second monitor's duplicate insert and a
	// monitor's per-cycle re-detection of a known job cannot touch it, and a
	// restart before the fetch keeps it owed.
	//
	// json:"-": it is the notifier's bookkeeping, of no use to any UI.
	AnnounceFound bool `json:"-"`
	// JoinOffsetSeconds is where in the broadcast a live YouTube capture's
	// recording began, in seconds of the broadcast's own timeline — the
	// video's, which a viewer's timestamp comment counts in (owner decision
	// CJ-3). 0 for a capture recorded from sq=0; the join point of one that
	// joined late. Written by the finalize from the staged recording
	// (joinOffsetSeconds, internal/worker/recording_start.go; a split
	// archive's is part 1's, splitJoinOffsetSeconds, owner decision CK-3);
	// nil while unknown — a VOD, a Twitch capture, a row from before the
	// column, a capture whose start was not recorded. The chapter import
	// pre-fills its offset with it negated (defaultChapterOffset).
	JoinOffsetSeconds *float64 `json:"joinOffsetSeconds,omitempty"`
	// LiveEdgeReason says why a live YouTube capture recorded from the live
	// edge instead of the broadcast's start (owner decision PR-R2): the
	// stream was older than YouTube keeps its segments, its start was gone,
	// every DASH format was DRM-protected, no GVS token could be minted, or
	// the player's JS challenges could not be solved with no split pair
	// left that needs none. Written once by the worker when the capture's
	// first setup falls to the live HLS playlist
	// (DownloadOrchestrator.recordLiveEdgeStart), cleared by a Reinitialize,
	// "" (NULL) for every capture that recorded from the start. Both UIs'
	// job details show it beside JoinOffsetSeconds as "Recorded from the
	// live edge: <reason>, joined H:MM:SS in" — YouTube has no gap-fill, so
	// the operator is the only one who can fetch the missing start.
	LiveEdgeReason string `json:"liveEdgeReason,omitempty"`
	// NotificationMsgs maps a notification target's stable key — the first 16
	// hex digits of SHA-256 over the RESOLVED webhook URL, the same key
	// notifications.buildTargets dedupes on — to the id of the one Discord
	// message that target rewrites in place for this job (per-target
	// `mode = "edit"`). Empty/absent for every job on a separate-mode install,
	// which is the default.
	//
	// Written ONCE per (job, target), on the first successful POST, through
	// UpdateNotificationMsgs: a silent single-column write that bumps no
	// updated_at and wakes no subscriber (owner ruling 2026-09-27). It is not
	// in fieldToColumn on purpose — UpdateJobFields must never be able to
	// write it, because that is the path that fans out to every UI.
	//
	// json:"-" on purpose. It pairs a fingerprint of a webhook URL (which IS
	// the credential) with a Discord message id, and no UI displays either.
	NotificationMsgs map[string]string `json:"-"`
	// IncompleteTail marks a Finished job whose recording is known to be missing
	// tail segments (finalized behind head after refresh attempts). Staging +
	// resume sidecar are preserved; Resume is allowed and clears the flag on a
	// complete re-run. Retry is refused for such a job (it would delete the
	// staging Resume needs — see the retry route in routes/jobs.go).
	IncompleteTail bool `json:"incompleteTail,omitempty"`
	// Trims (loaded via join)
	Trims []TrimRecord `json:"trims,omitempty"`
	// Segments (loaded via join, for multi-segment quality-split jobs)
	Segments []Segment `json:"segments,omitempty"`
}

// JobVersion reports the job's ID and write Version — the pair a subscriber
// that keeps a copy needs to drop a stale delivery (see Version).
func (j *Job) JobVersion() (string, uint64) { return j.ID, j.Version }

// terminalStatuses are the statuses a job's run ends in — its outcome.
// IsTerminal reads them, and UpdateJobFieldsUnlessTerminal will not write
// over them.
var terminalStatuses = []JobStatus{StatusFinished, StatusError, StatusCancelled}

// IsTerminal returns true if the job status is a terminal state.
func (j *Job) IsTerminal() bool {
	return slices.Contains(terminalStatuses, j.Status)
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
	TotalCount        int   `json:"totalCount"` // every job, whatever its status or platform
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
