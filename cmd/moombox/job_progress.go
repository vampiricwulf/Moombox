package main

import "github.com/vampiricwulf/Moombox/internal/database"

// jobProgressFrame is the job_progress payload (O-O).
//
// Every JSON key is spelled exactly as the same field on database.Job, because
// the client merges the frame onto the row it holds with an object spread
// ({...old, ...patch}); a renamed key would add a property rather than update
// one, and the card would freeze on its first value.
//
// What is NOT here is the point: description (YouTube's run to 5 KB),
// outputFile/filename/outputDirectory, thumbnailUrl, url, title, channelName
// and gaps are all immutable across a tick, and all of them were going out 60
// times a second per dashboard per active job. They still ride every
// job_update, which is every state transition — and gaps were never refreshed
// per tick anyway (renderJobDetails reads them on dialog open; updateJobDetails
// does not touch that row).
//
// status IS carried: it is what lets the client assert the server's promise
// that a progress frame never changes state, rather than assume it.
type jobProgressFrame struct {
	ID                string             `json:"id"`
	Status            database.JobStatus `json:"status"`
	Progress          string             `json:"progress"`
	Percent           float64            `json:"percent"`
	Speed             string             `json:"speed"`
	ETA               string             `json:"eta"`
	LastVideoSeq      *int               `json:"lastVideoSeq"`
	LastAudioSeq      *int               `json:"lastAudioSeq"`
	TotalVideoSeq     *int               `json:"totalVideoSeq"`
	TotalAudioSeq     *int               `json:"totalAudioSeq"`
	TotalChatMessages *int               `json:"totalChatMessages"`
	UpdatedAt         string             `json:"updatedAt"`

	// version is the write that produced the row (database.Job.Version),
	// for the hub's stale-frame check. Not part of the wire frame.
	version uint64
}

// JobVersion lets the WebSocket hub drop a frame older than one it has sent.
func (f jobProgressFrame) JobVersion() (string, uint64) { return f.ID, f.version }

// newJobProgressFrame projects the mutable fields out of a job row.
//
// No omitempty on the pointers: a nil one marshals as null, and the client's
// spread then writes null over the held value — which is what the DB actually
// says. Hiding the key would leave a stale number on screen for a job whose
// column was cleared.
func newJobProgressFrame(job *database.Job) jobProgressFrame {
	return jobProgressFrame{
		ID:                job.ID,
		Status:            job.Status,
		Progress:          job.Progress,
		Percent:           job.Percent,
		Speed:             job.Speed,
		ETA:               job.ETA,
		LastVideoSeq:      job.LastVideoSeq,
		LastAudioSeq:      job.LastAudioSeq,
		TotalVideoSeq:     job.TotalVideoSeq,
		TotalAudioSeq:     job.TotalAudioSeq,
		TotalChatMessages: job.TotalChatMessages,
		version:           job.Version,
		UpdatedAt:         job.UpdatedAt,
	}
}
