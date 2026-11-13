package worker

import (
	"strings"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// Which output directory a job writes to (owner ruling R2-X1).
//
// The monitors store a directory on every row they create: the channel's
// override, or the global paths.output_directory as it stood at detection.
// A row can then wait for days (a scheduled stream) or for the whole backlog
// ahead of it (a Queued VOD), and it went on naming that directory whatever
// the setting became: an operator who pointed Output directory at a new
// drive because the old one filled had every waiting row still mux into the
// full one, while the disk gauge and the backlog's disk gate read the new
// one. A manually added row stores no directory unless its request named
// one, so those already followed the setting.
//
// A monitor-detected row that has not started now follows the settings as
// they are when its download starts: the channel's override if the channel
// has one, else the global directory. buildJobContext settles it then and
// writes it to the row, so the row names the directory its files are in from
// the first file on, which is what every later reader of the column assumes
// (the mux, a Resume, the orphan sweep, the file routes). A row that has
// started, or has an archive or a part, keeps the directory it has.
//
// "Started" is download_started_at: a row that began downloading and was then
// resumed, retried or requeued is Upcoming or Queued again with its staging,
// and stays with the directory it started under, as its Mux action does. A
// Reinitialize clears the stamp with the staging, so a reset row follows the
// setting again.
//
// A row whose channel is no longer configured (a removed channel's kept
// backlog), or that names no channel to look up, keeps the directory it
// stores: that value is the only record of the override the channel had.

// followsOutputSetting reports whether job's output directory is still open:
// a monitor-detected row that is waiting (Upcoming, Queued, or Live with
// its download not yet claimed), has never started downloading
// (download_started_at) and has written nothing to an output directory.
// hasParts is whether the job has segment rows; a row with a part or an
// archive stays with them wherever the setting points now.
//
// A manually added row is never one: the directory its request named is its
// own, and one that named none stores none and reads the setting at every
// run, as it always has.
func followsOutputSetting(job *database.Job, hasParts bool) bool {
	if job == nil || job.ManuallyAdded || hasParts || job.OutputFile != "" || job.DownloadStartedAt != "" {
		return false
	}
	switch job.Status {
	case database.StatusUpcoming, database.StatusQueued, database.StatusLive:
		return true
	}
	return false
}

// channelOutputOverride returns the output_directory override of the
// configured channel job belongs to ("" when the channel has none), and
// whether that channel is configured at all. The channel is the one the row's
// channel_id names; a Twitch live row carries none, and is matched by the
// login its URL names. IDs compare case-insensitively, as everywhere else.
func channelOutputOverride(channels []config.ChannelConfig, job *database.Job) (override string, configured bool) {
	platform := job.Platform
	if platform == "" {
		platform = "youtube"
	}
	id := ""
	if job.ChannelID != nil {
		id = *job.ChannelID
	}
	if id == "" && platform == "twitch" {
		id = extractTwitchLoginFromJob(job)
	}
	if id == "" {
		return "", false
	}
	for i := range channels {
		if channels[i].GetPlatform() == platform && strings.EqualFold(channels[i].ID, id) {
			return channels[i].OutputDirectory, true
		}
	}
	return "", false
}

// settledOutputDirectory returns the output_directory a row whose directory
// is still open (followsOutputSetting) takes now: the channel's override,
// else the global directory. channelConfigured is whether the row's channel
// is still in the configuration (channelOutputOverride); a row whose channel
// is not keeps the directory it stores.
//
// A row that stores no directory (a Twitch backlog row of a channel with no
// override) takes the global one like any other, so it too is pinned once
// its download starts: left blank it read the setting at every run, and a
// later part or mux went to another directory than its first files.
func settledOutputDirectory(job *database.Job, globalDir, channelOverride string, channelConfigured bool) string {
	if channelOverride != "" {
		return channelOverride
	}
	// No global directory to name (config validation refuses an empty one,
	// so only a config built by hand has none): the row keeps what it has.
	if globalDir == "" {
		return job.OutputDirectory
	}
	if !channelConfigured && job.OutputDirectory != "" {
		return job.OutputDirectory
	}
	return globalDir
}

// settleOutputDirectory brings a row whose directory is still open up to the
// current settings, in the row and in job, before a JobContext is built from
// it. The write applies only while the row is still waiting: a refused one
// (the row moved on, or is gone) leaves job as it was read, so the context
// and the row never name different directories.
func (w *DownloadWorker) settleOutputDirectory(job *database.Job, hasParts bool, globalDir, channelOverride string, channelConfigured bool) {
	if w.db == nil || !followsOutputSetting(job, hasParts) {
		return
	}
	dir := settledOutputDirectory(job, globalDir, channelOverride, channelConfigured)
	if dir == job.OutputDirectory {
		return
	}
	waiting := []database.JobStatus{database.StatusUpcoming, database.StatusQueued, database.StatusLive}
	if !w.db.UpdateJobFieldsIfIn(job.ID, waiting, map[string]any{"output_directory": dir}) {
		return
	}
	w.logger.Info("output directory follows the current setting", "jobID", job.ID, "was", job.OutputDirectory, "now", dir)
	job.OutputDirectory = dir
}
