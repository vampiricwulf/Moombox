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
// re-probing. now is the cycle's one instant (§7), which an unconfirmed
// end's re-check is scheduled from (applyProbe).
func (fm *FeedMonitor) walk(ctx context.Context, ch *config.ChannelConfig, chID, cutoff string, now time.Time, scope []database.FeedItem) map[string]ProbeClassifyResult {
	fresh := map[string]ProbeClassifyResult{}
	st := walkState{exhausted: map[string]bool{}, lastDate: map[string]time.Time{}, noExit: map[string]bool{}}
	// Per-pass tallies for the summary line below — a healthy walk's probes
	// are individually silent (success writes no log), which made the first
	// production failure invisible for hours. The summary is the walk's
	// heartbeat: it must appear once per channel per cycle, whatever happened.
	var skipped, probed, applied, denied, errored, cooldown, dateFetchFailed int

	for _, row := range scope {
		if ctx.Err() != nil {
			// The pass budget expired (or shutdown): stop the pass — the
			// remaining rows retry next cycle. archive() carries the same
			// guard; without it the loop would churn every remaining row's
			// gates and a fast-failing probe after the budget is spent.
			// break, not return: the summary below is the walk's heartbeat
			// and must appear however the pass ended.
			break
		}

		// src is the source the row carried when the walk read it. A
		// members_only refusal can relabel a row mid-walk (§9); the relabel
		// takes effect next cycle — otherwise a row could exhaust a source it
		// was not in when the list was built.
		src := row.Source

		if st.exhausted[src] && row.Status != "upcoming" && row.Status != "live" && row.DatePrecision != "assumed" && row.RecheckAt == "" {
			// The exemptions are not optional: Q2 carries upcoming/live and
			// assumed rows unconditionally, and their published is an insert
			// or sighting instant, not a listing position — "everything
			// behind the boundary is older" says nothing about them. Nor
			// about a row whose end re-check is due: Q3 carries it because
			// the backoff ran out, wherever its date is.
			skipped++
			continue
		}

		if active, err := fm.db.HasActiveJob(row.VideoID); err != nil || active {
			skipped++
			continue // DB read error ⇒ skip the item, continue the cycle (§7)
		}

		// Title-only for store rows, like DECAPI — a store row carries no
		// description (§8: terms gate the probe on what the store has).
		if !MatchesTerms(row.Title, ch) {
			skipped++
			continue
		}

		if src == "membership" && !fm.membershipActive() {
			skipped++
			continue
		}

		switch row.Status {
		case "unknown", "upcoming", "live":
			// probe below
		case "vod":
			if row.RecheckAt != "" {
				// An unconfirmed end whose re-check is due — FeedScope keeps
				// one still pending out of scope. The one other way back for
				// a vod row: it was stored ended on weak signals nobody
				// confirmed, so it is probed again (probeAndClassify's
				// confirmEnd), and a probe that finds it live or upcoming
				// takes it up as a newly found broadcast.
				break
			}
			hasJob, err := fm.db.HasAnyJob(row.VideoID)
			if err != nil || hasJob || row.DatePrecision != "started" {
				skipped++
				continue // restart carve-out only: vod + started + no job row (§8)
			}
		default:
			skipped++
			continue // not_a_stream
		}

		probed++
		res, ok := fm.probeRowDated(ctx, ch, chID, row, cutoff)
		if !ok {
			// The date fetch failed (transport error): a transient fault, not
			// a classification. Nothing written, not FRESH, never exhausts —
			// exactly the errored-probe contract; retried next cycle.
			dateFetchFailed++
			continue
		}
		switch res.Outcome {
		case OutcomeDenied:
			denied++
		case OutcomeErrored:
			errored++
		case OutcomeCooldown:
			cooldown++
		}
		if res.Outcome == OutcomeProbed {
			applied++
			fm.applyProbe(chID, row, res, now) // normalization + terminal invariant + re-check, below
			fresh[row.VideoID] = res
			if res.PublishedAt != "" && row.RecheckAt == "" {
				// A due re-check is not at a listing position either (the
				// skip above): its date says nothing about the order of the
				// rows around it, so it neither trips the ordering check nor
				// exhausts its source.
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

	fm.logger.Debug("walk done", "channel", chID, "scope", len(scope),
		"skipped", skipped, "probed", probed, "applied", applied,
		"denied", denied, "errored", errored, "cooldown", cooldown,
		"dateFetchFailed", dateFetchFailed, "exhausted", len(st.exhausted))
	return fresh
}

// probeRowDated is the walk's and the archive-refresh's shared probe entry —
// probeRow plus the date-completing second phase (§9). The status probes
// (ANDROID_VR/TV clients) carry no microformat, so a successful vod-family
// classification arrives DATELESS in production. When the row's own date is
// only an estimate (coarse/assumed) — the rows the §10 window re-check and
// the §8 exhaustion inference actually need truth for — one ProbeDate call
// (a WEB player fetch that carries the jar's credentials — see
// youtube.PlayerAPI.ProbeVideoDate; only the STATUS probes are anonymous)
// supplies it; the ladder makes the upgrade one-time per video. Rows already
// holding day/exact/started dates never fetch: their date is authoritative and
// the probe's absence costs nothing — except an rss row dated before cutoff.
// Its <published> is the ANNOUNCEMENT for a stream, so a broadcast announced
// before the window and first seen after it ended (the channel just added,
// or Moombox down through it) probed as a VOD and was windowed out by the
// date it was announced, though it aired inside the window.
//
// A weak end the end check answered (res.endChecked) has made that WEB
// call already, so the fetch is not made again: the check's date is taken
// instead, under the same rule — only where the fetch would have run. Taken
// over an authoritative date, the check's 'started' outranked an RSS row's
// 'exact' on the ladder, and a settled vod became the restart carve-out's
// shape (vod, started, no job), probed every cycle for the whole window.
//
// ok=false means the date fetch itself FAILED (transport error): callers
// treat the row like an errored probe — no write, no FRESH, no exhaustion,
// retried next cycle. A fetch that succeeds but finds no date (YouTube has
// none) returns ok=true with the result still dateless: applyProbe's
// invariant and the archive's row-date fallback handle that honestly.
func (fm *FeedMonitor) probeRowDated(ctx context.Context, ch *config.ChannelConfig, chID string, row database.FeedItem, cutoff string) (ProbeClassifyResult, bool) {
	res := fm.probeRow(ctx, ch, chID, row)
	if res.Outcome != OutcomeProbed || res.PublishedAt != "" {
		return res, true
	}
	vodFamily := res.StreamStatus == "vod" || res.StreamStatus == "post_live" || res.StreamStatus == "not_a_stream"
	if !vodFamily {
		return res, true // broadcasts are never windowed — no date needed (§12)
	}
	announcedEarly := row.Source == "rss" && row.Published < cutoff
	if row.DatePrecision != "coarse" && row.DatePrecision != "assumed" && row.DatePrecision != "" && !announcedEarly {
		return res, true // the row's own date is already authoritative
	}
	if res.endChecked {
		// The end check was this same WEB call (confirmEnd): its date is the
		// fetch's answer.
		res.PublishedAt, res.PublishedPrecision = res.endCheckPublishedAt, res.endCheckPrecision
		return res, true
	}
	if fm.ProbeDate == nil {
		return res, true
	}
	pub, prec, err := fm.ProbeDate(ctx, row.VideoID)
	if err != nil {
		fm.logger.Warn("date fetch failed; row retried next cycle", "id", row.VideoID, "err", err)
		return res, false
	}
	res.PublishedAt, res.PublishedPrecision = pub, prec
	return res, true
}

// probeRow probes a single feed-scope row and returns its classification.
// The probe choice is row.Source — an authenticated probe for membership
// rows (an anonymous probe gets no formats on members-only content and the
// classifier misfires it as "upcoming"), anonymous otherwise.
//
// A members_only refusal from ANY probe relabels the row's source to
// 'membership' UNCONDITIONALLY — the refusal IS the sighting (§9). The
// relabel is what routes the row to the authenticated probe on every later
// cycle, and what lets the membership cookie gate park it when cookies
// lapse. If the refused probe was anonymous and membership is active, the
// authenticated probe retries the SAME cycle — a members-only premiere
// discovered via RSS must not wait a poll interval to be admitted.
// login_required does neither: on public videos it is anti-bot pushback,
// not a membership signal (§9).
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
		ConfirmEnd: fm.ConfirmEnd,
		Logger:     fm.logger,
	})
	if res.PlayabilityError != "members_only" {
		// Covers OutcomeErrored/OutcomeCooldown too: both carry a
		// zero-valued PlayabilityError, so neither can trigger a flip.
		return res
	}
	if err := fm.db.SetFeedItemSource(chID, row.VideoID, "membership"); err != nil {
		fm.logger.Warn("members_only source flip failed; row re-probes anonymously next cycle",
			"id", row.VideoID, "err", err)
	}
	if row.Source != "membership" && fm.membershipActive() && fm.ProbeVideoAuth != nil {
		// Same-cycle escalation, cooldown BYPASSED (Cooldown: nil): the
		// refusal was a successful probe, so probeAndClassify just recorded
		// fm.ProbeCooldown for this video — gating the retry on it would
		// suppress escalation entirely for any probe_cooldown > 0 (§9).
		res = probeAndClassify(ProbeClassifyParams{
			Ctx:        ctx,
			VideoID:    row.VideoID,
			Channel:    ch,
			ProbeVideo: fm.ProbeVideoAuth,
			Tracker:    fm.MetadataTracker,
			Cooldown:   nil,
			ConfirmEnd: fm.ConfirmEnd,
			Logger:     fm.logger,
		})
	}
	return res
}

// applyProbe writes a successful probe's classification to the store.
// post_live normalizes to vod on write (§12 — the store's status enum has
// no post_live; the DVR distinction is a download-strategy concern the
// worker re-probes for anyway). The terminal invariant (§12) then runs:
// never let a row END UP with a terminal status (vod/not_a_stream) and no
// rankable date. The invariant is about the ROW, not the probe: a row
// already holding a coarse-or-better date satisfies it with a dateless
// probe (production status probes carry no microformat, so dateless is the
// NORM, not the exception). Only when the probe supplies no date AND the
// row's own date is a fabrication ('assumed' = published:=sighting instant)
// does the terminal status demote to 'unknown', keeping the row in Q2's
// unresolved arm until some probe dates it.
//
// An unconfirmed end (r.EndUnconfirmed) is stored ended — that is what
// keeps an archived stream from being announced again — with its re-check
// scheduled in the same write, endRecheckDelay after now: an hour after the
// first, then six, then daily (row.RecheckStep counts the ones before).
// Every other classification clears a pending one, so the re-check ends the
// moment a probe settles the row: a confirmed end is a vod like any other,
// and a broadcast found live or upcoming is carried by its status.
func (fm *FeedMonitor) applyProbe(chID string, row database.FeedItem, r ProbeClassifyResult, now time.Time) {
	status := r.StreamStatus
	if status == "post_live" {
		status = "vod"
	}
	if (status == "vod" || status == "not_a_stream") && r.PublishedAt == "" &&
		(row.DatePrecision == "assumed" || row.DatePrecision == "") {
		status = "unknown"
	}
	var recheck database.FeedRecheck
	if r.EndUnconfirmed {
		recheck = database.FeedRecheck{
			At:   now.Add(endRecheckDelay(row.RecheckStep)).UTC().Format(time.RFC3339),
			Step: row.RecheckStep + 1,
		}
		fm.logger.Debug("broadcast end unconfirmed; row re-checked later",
			"id", row.VideoID, "channel", chID, "at", recheck.At, "recheck", recheck.Step)
	}
	if err := fm.db.ApplyProbeToFeedItem(chID, row.VideoID, status, r.Title, r.PublishedAt, r.PublishedPrecision, recheck); err != nil {
		fm.logger.Warn("probe write failed; row retried next cycle", "id", row.VideoID, "err", err)
	}
}
