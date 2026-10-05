package main

import (
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// cliAddedFacts describes a job added from the command line.
//
// `moombox add` runs no metadata fetch, so the row it writes carries the
// placeholders "Manual Add" and "Manual"; passing those through would make the
// embed read "Manually added: Manual Add" with a Channel field that names
// nothing. The facts are therefore built from what the COMMAND knows rather
// than from the row: an empty Title (the builder falls back to the id), the
// derived YouTube thumbnail, and — for a Twitch live add, where the target IS
// a channel login — the channel name and its page.
//
// channelLogin is empty for YouTube and for a Twitch VOD add, whose target is
// a video id.
func cliAddedFacts(platform, jobID, jobURL, channelLogin string) notifications.JobFacts {
	f := notifications.JobFacts{
		ID:       jobID,
		VideoID:  jobID,
		Platform: platform,
		URL:      jobURL,
	}
	switch {
	case platform == "twitch":
		if channelLogin != "" {
			f.Channel = channelLogin
			f.ChannelURL = "https://www.twitch.tv/" + channelLogin
		}
	default:
		f.ThumbnailURL = youtubeThumbnailURL(jobID)
	}
	return f
}

// notifyStreamFound is the one discovery embed, for both monitors.
//
// A package-level function rather than two inline sends inside
// wireMonitorCallbacks' closures: those closures need a runState, three live
// monitors and a database to reach, which is why neither discovery embed had a
// test before (audit §4, coverage root cause). Everything the two sites differ
// by is a parameter.
//
// channelURL is the channel's page when the caller knows one the job row
// cannot derive — a Twitch row's channel_id is always NULL, and its login is
// only on the monitor's stream info. category is the Twitch game, empty for
// YouTube.
func notifyStreamFound(n notifications.Sender, job *database.Job, channelURL, category string) {
	f := worker.NotifyFacts(job)
	if channelURL != "" {
		f.ChannelURL = channelURL
	}
	f.Category = category
	n.Send(notifications.StreamFound(f))
}

// cancelJobFromTUI is the TUI's cancel. A cancel that flags an actively
// processing run leaves "Job Cancelled" to that run's handleCancellation; a
// job no run holds — parked in COOKIES?, Queued for an archive slot, waiting
// to be picked up — has nobody to send it, and the TUI sent nothing where the
// Web's cancel route (internal/web/routes/jobs.go) sends it itself. In edit
// mode that also left the job's message short of its terminal state, and the
// tracker holding it.
func (s *runState) cancelJobFromTUI(jobID string) {
	job, _ := s.db.GetJob(jobID)
	if s.dlWorker.CancelJob(jobID) || job == nil || job.IsTerminal() {
		return
	}
	if s.notifyMgr != nil && s.notifyMgr.HasTargets() {
		s.notifyMgr.Send(notifications.JobCancelled(worker.NotifyFacts(job)))
	}
}
