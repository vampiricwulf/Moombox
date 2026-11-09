package worker

import (
	"errors"
	"fmt"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobverbs"
)

// ErrNotAllowed: the job's status does not allow the verb (the predicates in
// internal/jobverbs). The REST routes check the same predicate on the row
// they read and answer 400; a worker verb refusing it means the row moved on
// between that read and the verb — a stale TUI row, a racing operator or
// monitor — and the routes answer 409 with the worker's text, which the TUI
// shows as its red feedback line. Always wrapped in a *NotAllowedError
// naming the verb and the status found.
var ErrNotAllowed = errors.New("not allowed in the job's current status")

// NotAllowedError is ErrNotAllowed for one verb and the status that refused
// it. errors.Is(err, ErrNotAllowed) matches it.
type NotAllowedError struct {
	// Verb is the verb's past participle, as the routes' messages use it:
	// "resumed", "reinitialized", "muxed".
	Verb string
	// Status is what the row read when the verb was refused; "" only when
	// Mux's re-read after a missed write failed (a gone row is
	// ErrJobNotFound, never this).
	Status database.JobStatus
}

func (e *NotAllowedError) Error() string {
	return fmt.Sprintf("Job cannot be %s in current state (%s)", e.Verb, e.Status)
}

func (e *NotAllowedError) Unwrap() error { return ErrNotAllowed }

// ErrAlreadyFinishing: a Cancel that reached a job whose finalize has already
// produced its archive and is recording it (owner ruling R-C1). The job ends
// Finished — the archive is complete, and Cancelling it now would leave a
// written archive on no row or turn a finished recording into a Cancelled
// one — and the Cancel answers this: 409 over REST, the same text in red in
// the TUI. CancelJob returns it.
//
// A type rather than errors.New only so its text can start as the routes'
// other cancel answers do ("Not cancelled — ...").
var ErrAlreadyFinishing error = alreadyFinishingError{}

type alreadyFinishingError struct{}

func (alreadyFinishingError) Error() string {
	return "Not cancelled — the job is already finishing: its archive is written"
}

// resumeGate, reinitGate are afterJobExit's acceptance checks for the reset
// verbs: the status predicate each verb's route and TUI filter offer it on.
func resumeGate(job *database.Job) error {
	if !jobverbs.ResumableStatus(job.Status, job.IncompleteTail) {
		return &NotAllowedError{Verb: "resumed", Status: job.Status}
	}
	return nil
}

func reinitGate(job *database.Job) error {
	if !jobverbs.ReinitializableStatus(job.Status) {
		return &NotAllowedError{Verb: "reinitialized", Status: job.Status}
	}
	return nil
}

// reinitResetFields is the row a Reinitialize — the operator's and the
// monitor's automatic one alike — writes: every column a run fills in back
// to its fresh value, the status Upcoming. autoRetryCount is the one column
// the two differ in: an operator's reset grants a fresh budget (0), the
// automatic one spends one of it (the count so far + 1). One map, so a column
// added to the reset cannot be missed by one of them
// (TestReinitResetFieldsAreTheSameForBothPaths).
func reinitResetFields(autoRetryCount int) map[string]any {
	return map[string]any{
		"status":              database.StatusUpcoming,
		"error":               "",
		"park_reason":         database.ParkReasonNone,
		"park_identity":       "",
		"announce_found":      false, // see ResumeJob
		"progress":            "",
		"percent":             0,
		"speed":               "",
		"eta":                 "",
		"last_video_seq":      nil,
		"last_audio_seq":      nil,
		"total_video_seq":     nil,
		"total_audio_seq":     nil,
		"chat_status":         "",
		"total_chat_messages": nil,
		"download_started_at": "",
		"stream_end_time":     "",
		"output_file":         "",
		"filename":            "",
		"file_size":           nil,
		"chat_file":           "",
		"chat_filename":       "",
		"description_file":    "",
		"thumbnail_file":      "",
		"video_width":         nil,
		"video_height":        nil,
		"video_fps":           nil,
		"length_seconds":      nil,
		"join_offset_seconds": nil,
		"live_edge_reason":    nil,
		"selected_video_itag": nil,
		"selected_audio_itag": nil,
		"auto_retry_count":    autoRetryCount,
		"incomplete_tail":     false,
	}
}
