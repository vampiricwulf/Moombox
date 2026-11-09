// Package jobverbs holds the one status predicate for each job verb: which
// statuses Cancel, Resume, Reinitialize, Mux, Delete, Toggle Watched, Trim
// and the chapter import are offered and accepted from. The REST routes, the
// TUI's menu filters and batch loops, and the worker's verbs themselves all
// call these; the dashboard mirrors them in web/public/modules/utils.js, and
// cmd/moombox's verb parity test compares the two over every declared
// database.JobStatus.
//
// A leaf package (it imports only internal/database) so internal/tui, which
// does not import internal/worker, calls the same functions rather than a
// re-declared twin.
//
// Status only. A verb's disk facts — staged media for Mux, staging to resume,
// an unmuxed part of a Finished job — are probed by their own functions in
// internal/worker, and only where a verb is chosen, never where a menu opens
// (CORE-9). Each predicate here is the cheap half its probe narrows.
package jobverbs

import (
	"slices"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// Cancellable reports whether a job in status s can be cancelled: every
// status a job is still on its way through — waiting (Queued, Upcoming),
// recording (Live, Downloading), muxing, or parked for credentials. Never an
// outcome (Finished, Error, Cancelled). A positive list, so a status added
// later is not cancellable until it is named here (utils.js CANCEL_STATUSES).
func Cancellable(s database.JobStatus) bool { return slices.Contains(cancellable, s) }

var cancellable = []database.JobStatus{
	database.StatusDownloading, database.StatusLive, database.StatusUpcoming,
	database.StatusQueued, database.StatusMuxing, database.StatusCookies,
}

// CancellableStatuses is Cancellable as a list, for a conditional write
// (database.UpdateJobFieldsIfIn) that must apply only while the job is still
// cancellable. A fresh copy.
func CancellableStatuses() []database.JobStatus { return slices.Clone(cancellable) }

// ResumableStatus reports whether a job in status s can be resumed — its
// recording continued from its staging: one that failed, was cancelled or was
// parked for credentials, and a Finished one whose tail is known to be missing
// (incompleteTail), which its preserved staging and resume sidecar recover.
// Resume also needs a YouTube job (ResumeSupported) and staging to resume
// from (worker.HasStagingFiles). utils.js: RESUMABLE_STATUSES and
// canResumeJob's Finished arm.
//
// Equal to ReinitializableStatus on the first three by coincidence, not by
// contract: a resume keeps staging, a reinitialize deletes it.
func ResumableStatus(s database.JobStatus, incompleteTail bool) bool {
	switch s {
	case database.StatusError, database.StatusCancelled, database.StatusCookies:
		return true
	case database.StatusFinished:
		return incompleteTail
	}
	return false
}

// ResumeSupported reports whether a job of platform can be resumed at all: a
// YouTube capture only. A Twitch job starts over (Reinitialize). utils.js
// RESUME_PLATFORMS.
func ResumeSupported(platform string) bool { return platform == "youtube" }

// ReinitializableStatus reports whether a job in status s can be reset and
// started over: one that failed, was cancelled or was parked. Never a job
// that is running, queued or Finished — not reset under its run or over its
// archive. utils.js REINIT_STATUSES.
func ReinitializableStatus(s database.JobStatus) bool { return slices.Contains(reinitializable, s) }

var reinitializable = []database.JobStatus{database.StatusError, database.StatusCancelled, database.StatusCookies}

// MuxableStatus reports whether the Mux action — mux what the job's staging
// holds — is offered and accepted for a job in status s: a failed or
// cancelled one (with staged media, worker.HasSegmentFiles), and a Finished
// one whose finalize could not mux a part (MuxNeedsUnmuxedParts). utils.js
// MUX_STATUSES holds the first two; job-details.js names Finished beside it
// with the unmuxed-parts fact.
func MuxableStatus(s database.JobStatus) bool { return slices.Contains(muxable, s) }

var muxable = []database.JobStatus{database.StatusError, database.StatusCancelled, database.StatusFinished}

// MuxableStatuses is MuxableStatus as a list, for the Mux's conditional
// Muxing write. A fresh copy.
func MuxableStatuses() []database.JobStatus { return slices.Clone(muxable) }

// MuxNeedsUnmuxedParts reports whether a Mux of a job in status s needs the
// unmuxed-part fact (worker.HasUnmuxedParts) besides staged media: a Finished
// job is muxed only for a part its finalize left in staging.
func MuxNeedsUnmuxedParts(s database.JobStatus) bool { return s == database.StatusFinished }

// DeletableStatus reports whether both UIs offer Delete for a job in status
// s: one that has ended or is parked. The delete routes themselves accept any
// status and cancel an active job first (worker.IsActiveJobStatus); this is
// what the menus and the batch bar offer. utils.js DELETE_STATUSES.
func DeletableStatus(s database.JobStatus) bool {
	switch s {
	case database.StatusFinished, database.StatusError, database.StatusCancelled, database.StatusCookies:
		return true
	}
	return false
}

// WatchableStatus reports whether a job in status s can be marked watched or
// unwatched: a Finished one, the only kind with an archive to have watched.
// utils.js WATCHED_STATUSES.
func WatchableStatus(s database.JobStatus) bool { return s == database.StatusFinished }

// TrimmableStatus reports whether a job in status s can be trimmed: a
// Finished one (with an archive file, a row fact its callers check).
// utils.js TRIM_STATUSES.
func TrimmableStatus(s database.JobStatus) bool { return s == database.StatusFinished }

// ChapterImportableStatus reports whether a job in status s can have its
// chapters imported or restored: a complete Finished job only (owner
// decision 2026-11-07, narrowing CI-1). An Error, Cancelled or
// incomplete-tail row can still be resumed, and a resumed run rewrites the
// archive an import would be rewriting. chapter-import.js chapterImportable.
func ChapterImportableStatus(s database.JobStatus, incompleteTail bool) bool {
	return s == database.StatusFinished && !incompleteTail
}
