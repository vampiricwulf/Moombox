package jobstate

import "github.com/vampiricwulf/Moombox/internal/database"

// KeepsHostAwake reports whether a job in this status is work that the host
// going to sleep would end: a capture that is starting (Live), one that is
// running (Downloading) or a mux (Muxing). While any job is in one of them,
// and downloader.keep_awake is on, Moombox asks Windows not to sleep on its
// idle timer (cmd/moombox, keepAwake; owner ruling R2-X5).
//
// Upcoming and Queued are not in it: a row can wait there for days, and
// holding the PC awake for as long as a channel is monitored is not what the
// setting promises.
func KeepsHostAwake(status database.JobStatus) bool {
	switch status {
	case database.StatusLive, database.StatusDownloading, database.StatusMuxing:
		return true
	}
	return false
}
