package monitor

import (
	"context"
	"fmt"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// JobDisposition tells the host's OnVideoFound wiring HOW a job should be
// created — spec §10's creator table. Plan 4 implements the creation
// semantics (Queued vs admitted, queue_priority); until then the host maps
// every disposition to today's behavior (Upcoming + enqueue).
type JobDisposition int

const (
	// DispositionBroadcast is live/upcoming content: admitted immediately,
	// queue_priority 0 — never windowed, never M-counted (§10).
	DispositionBroadcast JobDisposition = iota
	// DispositionNewVOD is past content first inserted into the store THIS
	// cycle: admitted immediately, queue_priority 0 — new content skips the
	// queue (§10).
	DispositionNewVOD
	// DispositionBacklogVOD is past content already in the store before this
	// cycle: created Queued, queue_priority 1 — the only thing M paces (§10).
	DispositionBacklogVOD
)

// String labels dispositions for logs.
func (d JobDisposition) String() string {
	switch d {
	case DispositionBroadcast:
		return "broadcast"
	case DispositionNewVOD:
		return "new_vod"
	case DispositionBacklogVOD:
		return "backlog_vod"
	}
	return fmt.Sprintf("JobDisposition(%d)", int(d))
}

// passBudget bounds one WALK or ARCHIVE pass (spec §7). A fixed 60s budget
// silently truncates wide windows — the passes are serial, Q1 has no LIMIT,
// and archive_window_days validates to 3650 — so the budget scales with the
// scope actually read: floor 60s, one second per row, capped at 15m.
func passBudget(rows int) time.Duration {
	d := 60*time.Second + time.Duration(rows)*time.Second
	if d > 15*time.Minute {
		d = 15 * time.Minute
	}
	return d
}

// archive is checkChannel's step 4 (spec §10): decide, per scope row, whether
// to create a job — and with what disposition. scope is the RE-READ of
// FeedScope after the walk (the walk corrected dates and statuses, so a FRESH
// row's status here already reflects this cycle's probe). newIDs marks rows
// first inserted THIS cycle (new vs backlog); fresh carries the walk's
// successful probe results so nothing is probed twice in one cycle —
// probe_cooldown defaults to 0, so nothing else would suppress the repeat.
func (fm *FeedMonitor) archive(ctx context.Context, ch *config.ChannelConfig, chID, cutoff string, scope []database.FeedItem, newIDs map[string]bool, fresh map[string]ProbeClassifyResult) {
	// The established gate (§11) is per-channel: read once. Fail closed on a
	// read error — past content skipped this cycle is retried next cycle,
	// while archiving an unestablished channel repeats the headline bug.
	established, estErr := fm.db.GetChannelEstablished(chID)
	if estErr != nil {
		fm.logger.Warn("established-gate read failed; past content skipped this cycle",
			"channel", ch.Name, "err", estErr)
		established = false
	}

	// Per-pass tallies for the summary line below — same rationale as the
	// walk's: the pass's individual decisions are mostly silent by design,
	// and the summary is the heartbeat that proves the pass ran.
	var jobs, gated, history, retries, unknown int

	for _, row := range scope {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// The §10 decision table, in its order. The first two gates repeat
		// the walk's (the store and jobs table may have changed between the
		// passes, and archive must stand alone when the walk was truncated).
		if active, err := fm.db.HasActiveJob(row.VideoID); err != nil || active {
			gated++
			continue // DB read error ⇒ skip the item, continue the cycle (§7)
		}
		if !MatchesTerms(row.Title, ch) {
			gated++
			continue
		}

		switch row.Status {
		case "upcoming", "live":
			// Job iff FRESH this cycle — never windowed, never M-gated, and
			// NEVER HasProcessed-gated: history rows exist with no job row
			// (DECAPI give-up, legacy rows), and gating here would skip the
			// stream the moment its probe returned live (§10). A stale
			// upcoming/live row the walk could not probe this cycle
			// (cooldown, error, cookie gate) waits for the next one.
			if res, isFresh := fresh[row.VideoID]; isFresh {
				if fm.emitJob(ch, row, res, cutoff, newIDs) {
					jobs++
				}
			} else {
				retries++
			}

		case "vod", "not_a_stream":
			// Past content: the archival gates, in table order (§10).
			if done, err := fm.db.HasProcessed(row.VideoID); err != nil || done {
				history++
				continue // the history gate — vod/not_a_stream ONLY
			}
			if !ch.IncludeNonLiveContent {
				gated++
				continue // BEFORE probing: an unjobbable VOD is not worth a request
			}
			if !established {
				// §11: until an RSS success or a completed backfill proves we
				// can see the channel, membership items must not be treated
				// as the whole channel.
				gated++
				continue
			}
			if row.Source == "membership" && !fm.membershipActive() {
				gated++
				continue // the probe gate: skip a request we know is refused; scope is untouched (§9)
			}
			// Reuse the walk's FRESH result; refresh-probe otherwise. The
			// refresh probe exists to stop a job being built on STALE
			// metadata — metadata from earlier in this same cycle is not
			// stale (§10). probeRow, never a bare probeAndClassify, so the
			// members_only escalation (§9) applies here too.
			res, isFresh := fresh[row.VideoID]
			if !isFresh {
				var ok bool
				res, ok = fm.probeRowDated(ctx, ch, chID, row)
				if !ok {
					retries++ // date fetch failed — the errored-probe contract
					continue
				}
				if res.Outcome == OutcomeProbed {
					fm.applyProbe(chID, row.VideoID, row.DatePrecision, res)
				}
			}
			switch res.Outcome {
			case OutcomeDenied:
				// Denied FIRST (§10): it carries StreamStatus 'upcoming' by
				// definition, so a table that reaches the live/upcoming arm
				// before this one launders the 2.7.2 misfire into a job.
				// No job; retry next cycle.
				retries++
				continue
			case OutcomeErrored, OutcomeCooldown:
				retries++
				continue // the boundary was not learned — retry next cycle
			}
			if fm.emitJob(ch, row, res, cutoff, newIDs) {
				jobs++
			}

		default:
			// unknown → never a job. We do not know what it is; it stays in
			// scope (Q1, or Q2's unresolved arm) for the next walk (§10).
			unknown++
		}
	}

	fm.logger.Debug("archive done", "channel", chID, "scope", len(scope),
		"jobs", jobs, "history", history, "gated", gated, "retries", retries,
		"unknown", unknown)
}

// emitJob is the tail of the §10 decision table for one successfully-probed
// result — the caller has already routed denied/errored/cooldown away.
// Live/upcoming job as broadcasts; past content re-checks the window against
// the probe's date, then dispositions new-vs-backlog from newIDs.
func (fm *FeedMonitor) emitJob(ch *config.ChannelConfig, row database.FeedItem, res ProbeClassifyResult, cutoff string, newIDs map[string]bool) bool {
	d := DispositionBacklogVOD
	switch res.StreamStatus {
	case "live", "upcoming":
		// live/upcoming → job, never throttled and never windowed: a
		// broadcast probe supplies no date at all (§12), so the re-check
		// below can never apply to it — a date must never block the
		// never-miss-a-stream rule (§13 states the same law for DECAPI).
		d = DispositionBroadcast
	default:
		// vod / post_live / not_a_stream: RE-CHECK THE WINDOW against the
		// probe's date — the §10 crux. A coarse row was admitted on an upper
		// bound ("1 week ago" can be 13 days old); the probe's real date
		// decides, completed by the §9 date fetch when the status probe had
		// none. A result STILL dateless here falls back to the row's stored
		// date: reaching this arm requires stored status vod/not_a_stream,
		// which the §12 invariant only permits on rows whose date is coarse
		// or better — a rankable estimate, and Q1 admission already proved
		// it in-window. (An 'assumed' row can never arrive here dated-less:
		// dateless probes demote it to 'unknown', the default arm.)
		checkDate := res.PublishedAt
		if checkDate == "" {
			checkDate = row.Published
		}
		if checkDate == "" || checkDate < cutoff {
			fm.logger.Debug("archive: outside window on the probe's date; no job",
				"videoID", row.VideoID, "published", checkDate, "cutoff", cutoff)
			return false
		}
		if newIDs[row.VideoID] {
			d = DispositionNewVOD
		}
	}

	if fm.OnVideoFound == nil {
		return false
	}
	// Prefer the probe's title, but never let an API fallback placeholder
	// overwrite the listing title (same rule as the store's title arm, §6).
	title := row.Title
	if res.Title != "" && res.Title != "Unknown Title" {
		title = res.Title
	}
	fm.logger.Info("archive: creating job", "videoID", row.VideoID, "title", title,
		"status", res.StreamStatus, "disposition", d.String(), "channel", ch.Name)
	fm.OnVideoFound(row.VideoID, title,
		fmt.Sprintf("https://www.youtube.com/watch?v=%s", row.VideoID), ch, d)
	return true
}
