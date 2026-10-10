package worker

import (
	"slices"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// Removing a channel asks what to do with its jobs (W25-09, owner decision):
// keep them all (the default), or delete the pending ones. Both UIs show the
// same counts before the operator chooses — the dashboard through
// GET /api/config/channels/{id}/removal, the TUI's Settings → Channels
// through its OnChannelRemovalSummary callback — and both run the delete
// through DeletePendingChannelJobs. A channel that leaves the config any
// other way (PUT /api/config, a hand-edited config.toml, a TUI save of a
// channel removed with "keep") keeps its jobs: the departure prune deletes
// only the channel's feed history (monitor.BackfillWorker.CancelAndPrune).

// pendingRemovalStatuses are the rows a removal's "delete its pending jobs"
// choice deletes: the ones nothing has started downloading yet — or, for
// COOKIES?, that are waiting on credentials. A row among them whose staging
// holds anything is kept all the same (ChannelRemoval.Footage).
var pendingRemovalStatuses = []database.JobStatus{
	database.StatusQueued, database.StatusUpcoming, database.StatusCookies,
}

// ChannelJobRef names one job in a removal summary.
type ChannelJobRef struct {
	ID     string             `json:"id"`
	Title  string             `json:"title"`
	Status database.JobStatus `json:"status"`
}

// ChannelRemoval is what removing a channel would do to its jobs.
type ChannelRemoval struct {
	// Total is every job carrying the channel's ID, whatever its status —
	// what "Remove channel, keep its N jobs" keeps.
	Total int `json:"total"`
	// Pending is the rows "delete its pending jobs" deletes: Queued,
	// Upcoming and COOKIES?, less Footage.
	Pending int `json:"pending"`
	// Footage is the pending-status rows whose staging directory is not
	// empty, which neither choice deletes. A COOKIES? row can be a live
	// capture whose cookies died mid-stream, parked with its segments
	// staged for Resume; a Queued one can be a backlog VOD requeued with its
	// resume checkpoint. Deleting the row left the footage with no job to
	// resume or mux it from, and offered it as a deletable orphan.
	Footage []ChannelJobRef `json:"footage"`
	// Active is the Live, Downloading and Muxing rows — never touched.
	Active int `json:"active"`
}

// SummarizeChannelRemoval reads channelID's jobs and sorts them as the
// removal confirmation counts them. stagingBase is the effective staging
// directory (config.PathsConfig.EffectiveStagingDir).
func SummarizeChannelRemoval(db *database.Database, stagingBase, channelID string) (ChannelRemoval, error) {
	jobs, err := db.ListChannelJobs(channelID)
	if err != nil {
		return ChannelRemoval{}, err
	}
	sum := ChannelRemoval{Total: len(jobs), Footage: []ChannelJobRef{}}
	for _, j := range jobs {
		switch {
		case j.Status == database.StatusLive || j.Status == database.StatusDownloading || j.Status == database.StatusMuxing:
			sum.Active++
		case slices.Contains(pendingRemovalStatuses, j.Status):
			if HasStagingFiles(stagingBase, j.ID) {
				sum.Footage = append(sum.Footage, ChannelJobRef{ID: j.ID, Title: j.Title, Status: j.Status})
			} else {
				sum.Pending++
			}
		}
	}
	return sum, nil
}

// DeletePendingChannelJobs is a removal's "delete its pending jobs" choice:
// it deletes channelID's Queued, Upcoming and COOKIES? rows, with their
// history, except those whose staging holds footage, and returns how many it
// deleted and the footage rows it kept. The summary is taken afresh rather
// than trusted from the confirmation, and the delete re-applies the status
// filter, so a row that started downloading in between is left alone.
//
// The caller removes the channel from the config FIRST: the monitors ask the
// live config before they create a job (createYouTubeJob), so a cycle that
// still holds the channel in its snapshot cannot recreate a row deleted here.
func DeletePendingChannelJobs(db *database.Database, stagingBase, channelID string) (int, []ChannelJobRef, error) {
	sum, err := SummarizeChannelRemoval(db, stagingBase, channelID)
	if err != nil {
		return 0, nil, err
	}
	keep := make([]string, len(sum.Footage))
	for i, f := range sum.Footage {
		keep[i] = f.ID
	}
	n, err := db.DeleteJobsAndHistoryForChannel(channelID, pendingRemovalStatuses, keep)
	if err != nil {
		return 0, nil, err
	}
	return n, sum.Footage, nil
}
