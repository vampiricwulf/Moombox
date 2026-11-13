package jobstate

import (
	"fmt"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// HoldReason names why the archive-slots scheduler is not admitting a Queued
// backlog row (owner ruling R-D3). The wire values are what utils.js
// queuedHold answers too.
type HoldReason string

const (
	// HoldNone: nothing holds the row; it waits its turn for an archive slot.
	HoldNone HoldReason = ""
	// HoldOffline: the connectivity monitor reports offline, and the
	// scheduler admits nothing until it is back.
	HoldOffline HoldReason = "offline"
	// HoldDisk: the output volume reached disk_critical_percent and is not yet
	// config.DiskRecoveryMargin points below it (the disk gate).
	HoldDisk HoldReason = "disk"
	// HoldRateLimit: YouTube rate-limited the session; no YouTube backlog is
	// admitted until the hold runs out. Also a backlog VOD the limit itself
	// sent back to Queued, held for its own hour (not counted as a retry).
	HoldRateLimit HoldReason = "rate_limit"
	// HoldRetry: a transient failure or a full disk sent the backlog VOD back
	// to Queued, held for its backoff (attempt n of the retry budget).
	HoldRetry HoldReason = "retry"
)

// HoldPrecedence is the order a Queued row's hold reason is chosen in, which
// is the order the scheduler's admission sweep returns early in: offline
// first (sweep reads nothing else), then the disk gate, then the YouTube
// rate-limit hold (Twitch channels are admitted through it), then the row's
// own backoff (sweep's per-row held check). Written once, here; For follows
// it, and utils.js HOLD_PRECEDENCE is compared with it by cmd/moombox's
// job-state parity test.
var HoldPrecedence = []HoldReason{HoldOffline, HoldDisk, HoldRateLimit, HoldRetry}

// BacklogHold is one Queued backlog row's own hold: the backoff a requeue
// placed on it (worker backToQueued), with the retry streak's count when a
// counted failure placed it.
type BacklogHold struct {
	// Attempt is the requeue's place in the retry streak (1..Of); 0 for a
	// rate-limit requeue, which the streak does not count.
	Attempt int `json:"attempt"`
	// Of is the streak's budget (worker backlogRetryLimit).
	Of int `json:"of"`
	// Until is when the scheduler may admit the row again.
	Until time.Time `json:"until"`
}

// AdmissionHolds is a read-only snapshot of what holds backlog admission
// (worker.DownloadWorker.AdmissionHolds): the scheduler's gate-wide holds and
// each held row's own. It is the additive admissionHolds field of
// GET /api/status and the WebSocket's admission_holds frame, and what the
// TUI's Queued rows read. Derived from the scheduler's memory; nothing new is
// persisted.
type AdmissionHolds struct {
	Offline  bool `json:"offline"`
	DiskHeld bool `json:"diskHeld"`
	// RateLimitedUntil is when the YouTube rate-limit hold runs out; zero
	// (absent on the wire) when none stands.
	RateLimitedUntil time.Time              `json:"rateLimitedUntil,omitzero"`
	Jobs             map[string]BacklogHold `json:"jobs,omitempty"`
	// CookieRetries is each job's automatic cookie retries (owner ruling
	// R2-C1), by job id: the worker's in-memory count, carried here because
	// this snapshot is what already reaches both UIs. Read through
	// CookieRetryFor. It holds no admission.
	CookieRetries map[string]CookieRetry `json:"cookieRetries,omitempty"`
}

// QueuedHold is why one Queued row is not being admitted at a moment.
type QueuedHold struct {
	Reason HoldReason
	// Until is when the hold runs out (rate limit, retry); zero otherwise.
	Until time.Time
	// Attempt and Of are the retry streak's, for HoldRetry.
	Attempt, Of int
}

// For is job's hold at now, by HoldPrecedence. A row that is not Queued holds
// nothing. A hold whose time has passed holds nothing either, though the
// scheduler forgets it only at its next sweep.
func (h AdmissionHolds) For(job *database.Job, now time.Time) QueuedHold {
	if job == nil || job.Status != database.StatusQueued {
		return QueuedHold{}
	}
	own, hasOwn := h.Jobs[job.ID]
	hasOwn = hasOwn && now.Before(own.Until)
	for _, r := range HoldPrecedence {
		switch r {
		case HoldOffline:
			if h.Offline {
				return QueuedHold{Reason: HoldOffline}
			}
		case HoldDisk:
			if h.DiskHeld {
				return QueuedHold{Reason: HoldDisk}
			}
		case HoldRateLimit:
			if job.Platform != "twitch" && now.Before(h.RateLimitedUntil) {
				return QueuedHold{Reason: HoldRateLimit, Until: h.RateLimitedUntil}
			}
			if hasOwn && own.Attempt == 0 {
				return QueuedHold{Reason: HoldRateLimit, Until: own.Until}
			}
		case HoldRetry:
			if hasOwn && own.Attempt > 0 {
				return QueuedHold{Reason: HoldRetry, Until: own.Until, Attempt: own.Attempt, Of: own.Of}
			}
		}
	}
	return QueuedHold{}
}

// Text is the Queued row's line in both UIs (the dashboard's twin is
// utils.js queuedHoldText): the hold, or "Waiting in queue..." when nothing
// but the archive slots holds it. Times are the local wall clock, HH:MM.
func (q QueuedHold) Text() string {
	switch q.Reason {
	case HoldOffline:
		return "Held: offline"
	case HoldDisk:
		return "Held: disk full"
	case HoldRateLimit:
		return "Held: YouTube rate limit until " + q.Until.Local().Format("15:04")
	case HoldRetry:
		return fmt.Sprintf("Retry %d of %d at %s", q.Attempt, q.Of, q.Until.Local().Format("15:04"))
	}
	return "Waiting in queue..."
}
