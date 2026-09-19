package main

import "github.com/vampiricwulf/Moombox/internal/database"

// progressOnlyColumns are the schema columns a download's ~60 Hz progress tick
// writes (internal/worker/progress.go ProgressTracker.maybeUpdate and the two
// activity writers) and that nothing else writes alone. A JobChange whose every
// column is in this set carries no state transition, so the dashboard can be
// sent the slim job_progress frame instead of the whole row.
//
// total_video_seq / total_audio_seq are here although O-O's field list does not
// name them: maybeUpdate writes them on every tick that the stream has
// reported, so omitting them would classify every real tick as a transition and
// leave the whole change inert.
//
// "status" is deliberately ABSENT: a status change re-sorts the list and can
// cross the archive boundary, which is what job_update is for.
var progressOnlyColumns = map[string]bool{
	"progress":            true,
	"percent":             true,
	"eta":                 true,
	"speed":               true,
	"last_video_seq":      true,
	"total_video_seq":     true,
	"last_audio_seq":      true,
	"total_audio_seq":     true,
	"total_chat_messages": true,
}

// isProgressOnlyChange reports whether every column in changes is a progress
// column.
//
// An empty (or nil) set is NOT progress-only. UpdateJobFields never produces
// one for a real write — it refuses a call with no known field and strips only
// updated_at from the list — so an empty set means "a change we cannot
// classify", and the safe answer for that is the full row.
func isProgressOnlyChange(changes []string) bool {
	if len(changes) == 0 {
		return false
	}
	for _, col := range changes {
		if !progressOnlyColumns[col] {
			return false
		}
	}
	return true
}

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
}

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
		UpdatedAt:         job.UpdatedAt,
	}
}
