package worker

import (
	"context"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// A YouTube discovery's "Stream Found" is sent here, by the worker, after the
// job's first metadata fetch — not by the monitor that created the job.
//
// The monitor knows a video id and a feed title and not much else. The first
// fetch, which starts the moment the job is enqueued, learns the real title,
// channel, thumbnail and — for an upcoming stream — the schedule. Announcing
// at creation sent "Stream Found" and then, seconds later, "YouTube Start
// Time Confirmed" for the same moment; announcing after the fetch sends one
// embed carrying both.
//
// What is owed is the row's announce_found flag (database.Job.AnnounceFound):
// set in the INSERT that creates the job, taken by Process once the fetch is
// over (Database.TakeAnnounceFound, which clears it in the same statement),
// cleared by a cancel (CancelJob, and the cancel route without a worker) and
// by every re-run (ResumeJob, ReinitializeJob, AutoReinitializeJob).

// sendStreamFound sends the deferred discovery embed for job. info is the
// first fetch's result, or nil when that fetch failed — the embed then says
// what the row already knew, rather than the discovery going unannounced.
func (sp *StreamProcessor) sendStreamFound(job *database.Job, info *youtube.VideoInfo) {
	if sp.notifier == nil {
		return
	}
	f := NotifyFacts(job)
	if info != nil {
		addFoundFacts(&f, job, info)
	}
	title, desc, ntype, fields, opts := notifications.StreamFound(f)
	if !f.StartsAt.IsZero() {
		// A target filtered to `scheduled` but not `found` heard about this
		// schedule before the two embeds merged; it still does, in the
		// confirmation's own words (Manager.Send routes it).
		ct, cd, cn, cf, co := startTimeConfirmed(job, job.StreamStartTime)
		opts.Fallback = &notifications.Fallback{Title: ct, Description: cd, Type: cn, Fields: cf, Event: co.Event}
	}
	sp.notifier.Send(title, desc, ntype, fields, opts)
}

// startTimeConfirmed is "YouTube Start Time Confirmed": an upcoming stream's
// schedule, when it appears after the stream was announced as found — or, as
// a Fallback, for a target that does not take "Stream Found" at all.
//
// The one row→facts mapper, exactly as the worker's "Authentication Required"
// send uses it (worker.go). `scheduled` is one of the lifecycle events, and
// Manager.planLifecycle (internal/notifications/lifecycle.go) keys on
// Opts.JobID: without it an edit-mode target could never fold this embed into
// the job's one message, and the dashboard deep link — which needs JobID and
// Author both — would never apply. NotifyFacts also supplies the YouTube
// watch-URL fallback for a row whose url is still blank, which a freshly
// discovered upcoming stream's often is.
//
// Title and channel are job-supplied text, escaped as the builders escape
// them; Starts At carries <t:…> markup on purpose and must NOT be (see
// EscapeMarkdown's doc). It is rendered as "Stream Found" renders the same
// field, countdown and all; a value that does not parse goes through raw, as
// it always has.
func startTimeConfirmed(job *database.Job, scheduledStart string) (string, string, notifications.NotificationType, []notifications.Field, notifications.SendOptions) {
	startsAt := scheduledStart
	if t := parseRFC3339(startsAt); !t.IsZero() {
		startsAt = notifications.StartsAtValue(t)
	}
	f := NotifyFacts(job)
	return "YouTube Start Time Confirmed",
		"Scheduled: " + notifications.EscapeMarkdown(job.Title),
		notifications.TypeInfo,
		[]notifications.Field{
			{Name: "Channel", Value: notifications.EscapeMarkdown(job.ChannelName), Inline: true},
			{Name: "Starts At", Value: startsAt, Inline: true},
		},
		notifications.SendOptions{
			URL:       f.URL,
			Thumbnail: f.ThumbnailURL,
			Event:     "scheduled",
			Author:    notifyAuthor(f),
			Platform:  f.Platform,
			JobID:     f.ID,
		}
}

// addFoundFacts fills the discovery half of f from the first fetch. job has
// already been updated from info, so the row's start time and description are
// the fetch's, sentinels and blanks already filtered out.
//
// Starts At follows exactly the condition the standalone "YouTube Start Time
// Confirmed" uses — upcoming and not yet live — and not Kind, because this
// embed replaces that one and must carry every schedule it would have: a
// stream already live is announced by "YouTube Download Starting", and a VOD
// has no start ahead of it.
//
// Length only where a video already exists: a live broadcast's is 0 (which
// the parse drops), and an upcoming one has none. Ended and Published only for
// a finished video, the only kind that has either.
func addFoundFacts(f *notifications.JobFacts, job *database.Job, info *youtube.VideoInfo) {
	f.Kind = foundKind(info)
	f.Description = job.Description
	f.Access = accessLabel(info.PlayabilityError)

	if info.IsUpcoming && !info.IsLive {
		f.StartsAt = parseRFC3339(job.StreamStartTime)
	}
	switch f.Kind {
	case notifications.FoundUpcomingPremiere, notifications.FoundLivePremiere:
		f.Length = lengthOf(info)
	case notifications.FoundEnded, notifications.FoundVideo:
		f.Length = lengthOf(info)
		f.EndedAt = parseRFC3339(info.EndTimestamp)
		f.PublishedAt = parseRFC3339(info.PublishedAt)
		// Day-only when the date really is a bare day: a "day"-precision value
		// that carried a time and an offset was converted to its exact instant,
		// which a <t:…> timestamp shows correctly and a plain date would not.
		f.PublishedDayOnly = info.PublishedPrecision == "day" && isBareDate(info.UploadDate)
	}
}

// lengthOf is the fetch's duration, or zero when it reported none.
func lengthOf(info *youtube.VideoInfo) time.Duration {
	if info.LengthSeconds == nil {
		return 0
	}
	return time.Duration(*info.LengthSeconds) * time.Second
}

// foundKind classifies a fetch for the discovery embed's description, or ""
// when it cannot say.
//
// A refused fetch cannot say. A members-only or age-gated response carries
// little more than the refusal, and classifyStream reads that as not_a_stream
// — "Video:" over an upcoming members-only stream. The embed then keeps its
// neutral wording, and Access names the refusal. Refused is what
// checkPlayability calls it — any verdict but OK — and the parse uses the same
// line to keep a refusal's upload date out of the start time.
func foundKind(info *youtube.VideoInfo) string {
	if info.PlayabilityError != "" && info.PlayabilityError != youtube.PlayabilityOK {
		return ""
	}
	switch {
	case info.IsUpcoming && !info.IsLive && info.IsPremiere:
		return notifications.FoundUpcomingPremiere
	case info.IsUpcoming && !info.IsLive:
		return notifications.FoundUpcoming
	case info.IsLive && info.IsPremiere:
		return notifications.FoundLivePremiere
	case info.IsLive:
		return notifications.FoundLive
	case info.StreamStatus == youtube.StreamPostLive:
		return notifications.FoundEnded
	case info.StreamStatus == youtube.StreamVOD, info.StreamStatus == youtube.StreamNotAStream:
		return notifications.FoundVideo
	default:
		return ""
	}
}

// accessLabel names a playability restriction for the discovery embed, or ""
// for a video anyone can watch — or one whose restriction is unknown, which
// is not worth a field. A members-only stream fetched with a member's cookies
// plays, so it reads as open: the field says what THIS install was told.
func accessLabel(p youtube.PlayabilityError) string {
	switch p {
	case youtube.PlayabilityMembersOnly:
		return "Members only"
	case youtube.PlayabilityAgeRestricted:
		return "Age-restricted"
	case youtube.PlayabilityLoginRequired:
		return "Sign-in required"
	case youtube.PlayabilityPrivate:
		return "Private"
	case youtube.PlayabilityRegionBlocked:
		return "Region-blocked"
	case youtube.PlayabilityUnavailable:
		return "Unavailable"
	default:
		return ""
	}
}

// takeAnnounceFound takes job's "Stream Found" flag for a run that is still
// live: false for a cancelled run (see Process), and false without touching
// the database for a row read with the flag already off — every restart,
// Retry and Resume of a job announced long ago — so only an owed job pays for
// the conditional UPDATE that decides.
func (sp *StreamProcessor) takeAnnounceFound(ctx context.Context, job *database.Job) bool {
	if ctx.Err() != nil || !job.AnnounceFound {
		return false
	}
	job.AnnounceFound = false
	return sp.db.TakeAnnounceFound(job.ID)
}

// isBareDate reports whether s is a YYYY-MM-DD date with no time — the shape
// the parse normalises to that day's last second UTC.
func isBareDate(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}
