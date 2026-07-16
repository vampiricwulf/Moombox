package monitor

import (
	"context"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// walkState is the WALK step's per-pass, in-memory bookkeeping (spec §8).
// Built fresh at the start of every walk() call and never persisted — a
// crash or restart mid-cycle costs at most one over-probed cycle, never a
// correctness bug.
type walkState struct {
	exhausted map[string]bool      // per source: "everything behind here is older than the window"
	lastDate  map[string]time.Time // per source: the most recently probed date, for the ordering check
	noExit    map[string]bool      // per source: the ordering check tripped this cycle
}

// dateOrdered reports whether src's stored `published` reflects listing
// (recency) order, so the early-exit inference is safe to draw from it.
// `rss` is the one exception (spec §8): the store orders rss rows by
// <published> — the announcement time — while the probe returns the
// broadcast's actual start. For a scheduled stream those legitimately
// disagree, without bound, so rss never exhausts and never runs the
// ordering check. It needs neither: its rows are `exact` already, so there
// is no coarse bucket to bound.
func dateOrdered(source string) bool { return source != "rss" }

// walk is the WALK step (spec §8): a SERIAL probe pass over scope (Q1 ∪ Q2,
// in Q1's order — concurrent probes would already be in flight past a
// window boundary before it's known). For each row it applies the probe
// gates in order — HasActiveJob, term-match, the membership cookie gate,
// then the status rule (probe unknown/upcoming/live; vod only via the
// restart carve-out; not_a_stream never) — and, for date-ordered sources,
// infers when the rest of that source must be older than the window and
// stops probing it early.
//
// Returns the FRESH set — rows a probe actually ran against this pass —
// keyed by video ID, for the ARCHIVE step to disposition without
// re-probing.
func (fm *FeedMonitor) walk(ctx context.Context, ch *config.ChannelConfig, chID, cutoff string, scope []database.FeedItem) map[string]ProbeClassifyResult {
	fresh := map[string]ProbeClassifyResult{}
	st := walkState{exhausted: map[string]bool{}, lastDate: map[string]time.Time{}, noExit: map[string]bool{}}

	for _, row := range scope {
		// src is the source the row carried when the walk read it. A
		// members_only refusal can relabel a row mid-walk (§9); the relabel
		// takes effect next cycle — otherwise a row could exhaust a source it
		// was not in when the list was built.
		src := row.Source

		if st.exhausted[src] && row.Status != "upcoming" && row.Status != "live" && row.DatePrecision != "assumed" {
			// The exemptions are not optional: Q2 carries upcoming/live and
			// assumed rows unconditionally, and their published is an insert
			// or sighting instant, not a listing position — "everything
			// behind the boundary is older" says nothing about them.
			continue
		}

		if active, err := fm.db.HasActiveJob(row.VideoID); err != nil || active {
			continue // DB read error ⇒ skip the item, continue the cycle (§7)
		}

		// Title-only for store rows, like DECAPI — a store row carries no
		// description (see processCandidate's comment in feed.go).
		if !MatchesTerms(row.Title, ch) {
			continue
		}

		if src == "membership" && !fm.membershipActive() {
			continue
		}

		switch row.Status {
		case "unknown", "upcoming", "live":
			// probe below
		case "vod":
			hasJob, err := fm.db.HasAnyJob(row.VideoID)
			if err != nil || hasJob || row.DatePrecision != "started" {
				continue // restart carve-out only: vod + started + no job row (§8)
			}
		default:
			continue // not_a_stream
		}

		res := fm.probeRow(ctx, ch, chID, row) // Task 4: probe choice + escalation
		if res.Outcome == OutcomeProbed {
			fm.applyProbe(chID, row.VideoID, res) // normalization + terminal invariant, below
			fresh[row.VideoID] = res
			if res.PublishedAt != "" {
				d, _ := time.Parse(time.RFC3339, res.PublishedAt)
				if last, ok := st.lastDate[src]; ok && d.After(last) {
					// The ordering assumption is false for this source. The
					// IS-SET guard above is load-bearing: the zero value is
					// older than every real date, so without it this would
					// fire on the FIRST probe of every source, every cycle,
					// and early exit would never run.
					st.noExit[src] = true
					fm.logger.Warn("listing order violated; early exit disabled", "source", src, "channel", chID)
				}
				st.lastDate[src] = d
				// The !st.noExit[src] conjunct is NOT redundant with the
				// ordering check above — the two can trip on the SAME probe.
				// Reachable edge: an assumed-precision row of this source is
				// probed to a date outside the window WITHOUT exhausting (not
				// coarse), setting lastDate[src]; the next coarse row's probe
				// then returns a NEWER out-of-window date, tripping the
				// ordering violation AND satisfying every other exhaustion
				// conjunct in this same iteration. Without !noExit, a source
				// whose ordering was just disproven would still be retired,
				// skipping every row behind it. Do not "simplify" it away.
				if res.PublishedAt < cutoff && dateOrdered(src) && row.DatePrecision == "coarse" && !st.noExit[src] {
					// 'coarse' means the stored date came from a LISTING.
					// Exhaustion is an inference from listing order, so it
					// may only be drawn from a row whose position came from
					// a listing — an 'assumed' row's published=now is a
					// claim of ignorance, not a coordinate.
					st.exhausted[src] = true
				}
			}
		}
		// denied / errored / cooldown / probed-with-no-date: DO NOT exhaust.
		// The boundary was not learned. Retiring on these truncates a source
		// on a transient fault.
	}

	// PLAN3-TASK5 removes the double-probe: until the ARCHIVE step is wired
	// to consume this FRESH map instead of re-probing, checkChannel's legacy
	// candidate loop (mergeCandidates/processCandidate) may probe the same
	// videos again this cycle.
	return fresh
}

// probeRow probes a single feed-scope row and returns its classification.
// Simple form for Plan 3 Task 3: the probe choice is just row.Source — an
// authenticated probe for membership rows (an anonymous probe gets no
// formats on members-only content and the classifier misfires it as
// "upcoming"), anonymous otherwise. No escalation yet.
//
// chID is threaded through even though this form doesn't use it, so Task
// 4's escalation (which writes SetFeedItemSource(chID, ...) on a
// members_only refusal) doesn't need a signature change.
func (fm *FeedMonitor) probeRow(ctx context.Context, ch *config.ChannelConfig, chID string, row database.FeedItem) ProbeClassifyResult {
	probe := fm.ProbeVideo
	if row.Source == "membership" && fm.ProbeVideoAuth != nil {
		probe = fm.ProbeVideoAuth
	}
	res := probeAndClassify(ProbeClassifyParams{
		Ctx:        ctx,
		VideoID:    row.VideoID,
		Channel:    ch,
		ProbeVideo: probe,
		Tracker:    fm.MetadataTracker,
		Cooldown:   fm.ProbeCooldown,
		Logger:     fm.logger,
	})
	// PLAN3-TASK4 escalation lands here
	return res
}

// applyProbe writes a successful probe's classification to the store.
// post_live normalizes to vod on write (§12 — the store's status enum has
// no post_live; the DVR distinction is a download-strategy concern the
// worker re-probes for anyway). The terminal invariant (§12) then runs:
// never write a terminal status (vod/not_a_stream) without a rankable
// date — if the probe supplies none, the row stays 'unknown' instead, so a
// later probe still corrects it rather than the row sitting forever inside
// the window on a fabricated 'assumed' date.
func (fm *FeedMonitor) applyProbe(chID, videoID string, r ProbeClassifyResult) {
	status := r.StreamStatus
	if status == "post_live" {
		status = "vod"
	}
	if (status == "vod" || status == "not_a_stream") && r.PublishedAt == "" {
		status = "unknown"
	}
	if err := fm.db.ApplyProbeToFeedItem(chID, videoID, status, r.Title, r.PublishedAt, r.PublishedPrecision); err != nil {
		fm.logger.Warn("probe write failed; row retried next cycle", "id", videoID, "err", err)
	}
}
