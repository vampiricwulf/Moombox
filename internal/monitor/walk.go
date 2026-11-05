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

// recheckDue reports whether row's end re-check has come due by now, the
// cycle's instant as RFC3339 UTC (the form RecheckAt is written in, so the
// strings order as the instants do). A due row is probed with the end check
// wherever its date is (FeedScope's Q3).
func recheckDue(row database.FeedItem, now string) bool {
	return row.RecheckAt != "" && row.RecheckAt <= now
}

// recheckPending reports whether row's end re-check is scheduled and not yet
// due. A pending row is in scope wherever its arm puts it, like any row —
// the restart carve-out probes a vod one every cycle — but no probe before
// the re-check makes the end check, and none moves its schedule.
func recheckPending(row database.FeedItem, now string) bool {
	return row.RecheckAt > now
}

// settledTerminal reports whether row already holds a settled terminal
// classification: stored vod or not_a_stream with no re-check pending, so
// whatever stored it — a strong end, a confirmed weak one, formats, the
// recording itself — was not a guess the backoff is testing.
func settledTerminal(row database.FeedItem) bool {
	return (row.Status == "vod" || row.Status == "not_a_stream") && row.RecheckAt == ""
}

// endCheckFor is the end check a probe of row may make (ProbeClassifyParams.
// ConfirmEnd), judged against now, the cycle's instant. The signed-in WEB
// call settles an end nobody has settled, and only that:
//
//   - a row whose re-check is pending gets none: the backoff decides when
//     it is asked again, and a probe on any other rule before then (the
//     restart carve-out, the unresolved arm) asking it every cycle was the
//     cost the backoff exists to bound;
//   - a row holding a settled terminal classification (settledTerminal)
//     gets none: its probe — the restart carve-out's, or the archive's
//     refresh — looks for a stream that resumed, which the status probe
//     reads live or upcoming, and a weak end from it agrees with what the
//     row holds (probeRowDated);
//   - every other row — stored unknown, upcoming or live — gets
//     fm.ConfirmEnd, and so does a due re-check.
func (fm *FeedMonitor) endCheckFor(row database.FeedItem, now time.Time) ConfirmEndFunc {
	if recheckPending(row, now.UTC().Format(time.RFC3339)) || settledTerminal(row) {
		return withheldEndCheck
	}
	return fm.ConfirmEnd
}

// walk is the WALK step (spec §8): a SERIAL probe pass over scope (Q1 ∪ Q2
// ∪ Q3, in Q1's order — concurrent probes would already be in flight past a
// window boundary before it's known). For each row it applies the probe
// gates in order — HasActiveJob, term-match, the membership cookie gate,
// then the status rule (probe unknown/upcoming/live; vod only via the
// restart carve-out or a due end re-check; not_a_stream never), then the
// inconclusive hold (probeHeld) — and, for date-ordered sources, infers when
// the rest of that source must be older than the window and stops probing
// it early.
//
// Returns the FRESH set — rows a probe actually ran against this pass —
// keyed by video ID, for the ARCHIVE step to disposition without
// re-probing. now is the cycle's one instant (§7), which an unconfirmed
// end's re-check and an inconclusive hold are judged against and scheduled
// from (recheckDue, applyProbe, noteProbeOutcome).
func (fm *FeedMonitor) walk(ctx context.Context, ch *config.ChannelConfig, chID, cutoff string, now time.Time, scope []database.FeedItem) map[string]ProbeClassifyResult {
	fresh := map[string]ProbeClassifyResult{}
	st := walkState{exhausted: map[string]bool{}, lastDate: map[string]time.Time{}, noExit: map[string]bool{}}
	// Per-pass tallies for the summary line below — a healthy walk's probes
	// are individually silent (success writes no log), which made the first
	// production failure invisible for hours. The summary is the walk's
	// heartbeat: it must appear once per channel per cycle, whatever happened.
	var skipped, held, probed, applied, denied, errored, cooldown, dateFetchFailed int
	nowStr := now.UTC().Format(time.RFC3339)

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
		due := recheckDue(row, nowStr)

		if st.exhausted[src] && row.Status != "upcoming" && row.Status != "live" && row.DatePrecision != "assumed" && !due {
			// The exemptions are not optional: Q2 carries upcoming/live and
			// assumed rows unconditionally, and their published is an insert
			// or sighting instant, not a listing position — "everything
			// behind the boundary is older" says nothing about them. Nor
			// about a row whose end re-check is due: Q3 carries it because
			// the backoff ran out, wherever its date is. One still pending
			// is where its arm put it, like any other row.
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
			if due {
				// An unconfirmed end whose re-check is due. The other way
				// back for a vod row: it was stored ended on weak signals
				// nobody confirmed, so it is probed again, with the end check
				// (endCheckFor), and a probe that finds it live or upcoming
				// takes it up as a newly found broadcast.
				break
			}
			// The restart carve-out, whether or not a re-check is pending:
			// the backoff only ever adds probes. A pending re-check that
			// took the row out of scope left a stream that resumed on the
			// same ID unfound for an hour, then six, then a day, when the
			// carve-out would have found it on the next cycle. Its probe is
			// made without the end check (endCheckFor), pending or settled:
			// it looks for a restart, which the status probe reads live or
			// upcoming, and the backoff — or the end the row already holds
			// — answers the rest.
			hasJob, err := fm.db.HasAnyJob(row.VideoID)
			if err != nil || hasJob || row.DatePrecision != "started" {
				skipped++
				continue // restart carve-out only: vod + started + no job row (§8)
			}
		default:
			skipped++
			continue // not_a_stream
		}

		if fm.probeHeld(chID, row.VideoID, now) {
			// The row's last probe was inconclusive, and its hold has not
			// ended (noteProbeOutcome): the ladder, not the cycle, says when
			// it is asked again. Whatever the rule above that would probe it
			// — upcoming, the restart carve-out, a due end re-check — it is
			// asked when the hold ends, as an errored probe leaves it: no
			// write, not FRESH, never exhausts.
			held++
			continue
		}

		probed++
		res, ok := fm.probeRowDated(ctx, ch, chID, row, cutoff, now)
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
			if res.PublishedAt != "" && !due {
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
		"skipped", skipped, "held", held, "probed", probed, "applied", applied,
		"denied", denied, "errored", errored, "cooldown", cooldown,
		"dateFetchFailed", dateFetchFailed, "exhausted", len(st.exhausted))
	fm.pruneFailureSpells(chID, scope)
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
// The probe is given the end check the row's state calls for (endCheckFor,
// against now, the cycle's instant). None for a row whose re-check is still
// pending: its weak end stays unconfirmed, for applyProbe to leave the
// schedule as it stands. None either for a row already holding a settled
// terminal classification — stored vod or not_a_stream, no re-check pending
// (settledTerminal) — and a weak end from that probe agrees with what it
// holds: settled it stays, never turned back into an unconfirmed end by an
// answer weaker than the one that settled it.
//
// ok=false means the date fetch itself FAILED (transport error): callers
// treat the row like an errored probe — no write, no FRESH, no exhaustion,
// retried next cycle. A fetch that succeeds but finds no date (YouTube has
// none) returns ok=true with the result still dateless: applyProbe's
// invariant and the archive's row-date fallback handle that honestly.
func (fm *FeedMonitor) probeRowDated(ctx context.Context, ch *config.ChannelConfig, chID string, row database.FeedItem, cutoff string, now time.Time) (ProbeClassifyResult, bool) {
	res := fm.probeRow(ctx, ch, chID, row, fm.endCheckFor(row, now), now)
	if res.EndUnconfirmed && settledTerminal(row) {
		res.EndUnconfirmed = false
	}
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
//
// A members_only refusal with no videoDetails reaches here classified
// not_a_stream, the one classification reached by absence. With no
// authenticated retry to answer it — membership inactive, no cookies — it
// stores nothing (owner decision, wave FC): it is returned as the
// OutcomeDenied a refusal of an upcoming stream is, so the row stays
// undecided instead of settling not_a_stream, which no rule probes again.
// The relabel still lands, so the cookie gate parks the row until cookies
// return, and then the signed-in probe classifies it.
//
// confirm is the end check this probe may make (endCheckFor), passed on to
// both probes when the refusal escalates.
//
// Each probe's failure log follows the row's failure spell (inFailureSpell):
// Warn for the first failed probe since the row last answered, Debug for
// every one after it. Each probe's outcome is recorded against the spell
// and the row's hold (noteProbeOutcome) as of now, the cycle's instant; the
// caller has already checked that the row is not held (probeHeld).
func (fm *FeedMonitor) probeRow(ctx context.Context, ch *config.ChannelConfig, chID string, row database.FeedItem, confirm ConfirmEndFunc, now time.Time) ProbeClassifyResult {
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
		ConfirmEnd: confirm,
		Repeat:     fm.inFailureSpell(chID, row.VideoID),
		Logger:     fm.logger,
	})
	fm.noteProbeOutcome(ctx, chID, row.VideoID, res, now)
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
		//
		// Repeat false, not the spell: the escalation runs only on the
		// answer a members_only refusal is, and noteProbeOutcome has just
		// ended the row's spell for that answer, so a failure here is a new
		// spell's first and warns. Its outcome is noted like any probe's: a
		// failure starts that spell — next cycle's authenticated probe of
		// the relabelled row is its repeat — and an inconclusive one holds
		// the row.
		res = probeAndClassify(ProbeClassifyParams{
			Ctx:        ctx,
			VideoID:    row.VideoID,
			Channel:    ch,
			ProbeVideo: fm.ProbeVideoAuth,
			Tracker:    fm.MetadataTracker,
			Cooldown:   nil,
			ConfirmEnd: confirm,
			Repeat:     false,
			Logger:     fm.logger,
		})
		fm.noteProbeOutcome(ctx, chID, row.VideoID, res, now)
	} else if res.Outcome == OutcomeProbed && res.StreamStatus == "not_a_stream" {
		// No authenticated retry answered the detail-less refusal: nothing
		// is stored on it (see the doc above).
		res.Outcome = OutcomeDenied
	}
	return res
}

// probeSpell is one feed row's failure spell (failingProbes): the run of
// failed probes since the row last answered, and the hold its inconclusive
// probes put it on.
type probeSpell struct {
	// holds counts the inconclusive probes in the spell, the ladder's step:
	// the next one holds the row for endRecheckDelay(holds), an hour after
	// the first, then six, then daily.
	holds int
	// until is when the hold ends (probeHeld): before it neither the walk
	// nor the archive's refresh probes the row. Zero while no probe in the
	// spell has been inconclusive — a transient failure is retried next
	// cycle — and left as it stands by one that fails transiently once the
	// hold is due, so that row is retried next cycle too, on the same step.
	until time.Time
	// rateLimited: the spell's last probe was refused by the session's rate
	// limit (ProbeClassifyResult.RateLimited). It climbs no ladder — holds
	// and until stand as they were — and holds the row only while the
	// backlog's rate-limit hold stands (FeedMonitor.RateLimitHeld, probeHeld).
	rateLimited bool
}

// inFailureSpell reports whether chID's row videoID is in a failure spell:
// its last probe failed, and none has answered since (failingProbes).
//
// The first failed probe in a spell logs at Warn, every failed probe after
// it at Debug (probeRow passes this as ProbeClassifyParams.Repeat), and an
// answered probe ends the spell, so the next failure warns again
// (noteProbeOutcome). A spell is a row's run of failed probes, any failure:
// an inconclusive status probe under a lasting bot wall (G8,
// ErrProbeInconclusive) is the case it exists for, which stores nothing and
// was warned on every probe of the row. Owner decision (2026-10-31, W6
// follow-up 2): warn once in the walk, Warn on the first in a spell, Debug
// after. DECAPI's rule is close but not the same
// (ProcessYouTubeVideoParams.Repeat, sameAsLastAnswer): it demotes every
// repeat probe of the same newest video, answered in between or not, so it
// warns once per newest video and never re-arms; the walk re-arms on an
// answer, since a feed row is probed for as long as it is in scope, and a
// failure after an answer is a new fault worth one Warn.
func (fm *FeedMonitor) inFailureSpell(chID, videoID string) bool {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	_, ok := fm.failingProbes[chID][videoID]
	return ok
}

// probeHeld reports whether chID's row videoID is on an inconclusive hold
// that has not ended by now, the cycle's instant (probeSpell.until): the
// walk skips its probe, and the archive its refresh probe, until it ends.
//
// A row whose last probe was refused by the session's rate limit
// (probeSpell.rateLimited) is held instead for as long as the backlog's
// rate-limit hold stands (RateLimitHeld, read outside fm.mu): its re-check
// would be one more call into the same limit. Once that hold has run out the
// row is asked every cycle until a probe answers — the limit lifts on its
// own, and the ladder is for a refusal of the video (owner decision, wave FC).
func (fm *FeedMonitor) probeHeld(chID, videoID string, now time.Time) bool {
	fm.mu.Lock()
	spell, ok := fm.failingProbes[chID][videoID]
	fm.mu.Unlock()
	if !ok {
		return false
	}
	if now.Before(spell.until) {
		return true
	}
	return spell.rateLimited && fm.RateLimitHeld != nil && fm.RateLimitHeld(now)
}

// noteProbeOutcome records one probe of chID's row videoID, made at now, the
// cycle's instant, against its failure spell (inFailureSpell) and its hold
// (probeHeld):
//
//   - a failed probe starts or continues the spell; an inconclusive one
//     (ProbeClassifyResult.Inconclusive) also holds the row, on
//     endRecheckBackoff: an hour after the spell's first, six after its
//     second, then daily, for as long as no probe answers. Owner decision
//     (2026-10-31, W6 follow-up 1, revised): a feed row whose probe stays
//     inconclusive backs off. The refusal can stand for good — a
//     made-for-kids video, a stream removed after its job ended, which
//     FeedScope's Q2 keeps in scope as upcoming for good — and each probe of
//     it is three requests signed out (the escalation). DECAPI's newest video
//     is not held: it is asked about every cycle (ErrProbeInconclusive). A
//     transient failure holds nothing — a freshly live video must not be
//     blinded for an hour by one flaky probe (probeAndClassify). Nor does a
//     probe refused by the session's rate limit (ProbeClassifyResult
//     .RateLimited) climb the ladder (owner decision, wave FC): the limit is
//     the session's, not the video's, so it neither starts nor advances the
//     row's hold — the spell's step and until stand as they were — and marks
//     the spell rate-limited, which holds the row only while the backlog's
//     rate-limit hold stands (probeHeld), then every cycle;
//   - an answered probe (OutcomeProbed, OutcomeDenied — a refusal is an
//     answer) ends the spell and resets the ladder, so the row is probed on
//     its own rule again from the next cycle, and its next inconclusive
//     probe holds it for an hour, not a day;
//   - a cooldown-suppressed one changes nothing;
//   - and so does a failed probe made as ctx ended — the pass budget spent,
//     or shutdown: the probe was never really asked (confirmEnd's rule for
//     the end check), and a spell it started would have demoted the first
//     real failure's line, the one that names the cause, to Debug.
//
// A newly seen video has no spell, so its first probe runs at once whatever
// the rows around it are held for: the hold is a row's, never the
// channel's.
func (fm *FeedMonitor) noteProbeOutcome(ctx context.Context, chID, videoID string, res ProbeClassifyResult, now time.Time) {
	if res.Outcome == OutcomeErrored && ctx.Err() != nil {
		return
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()
	switch res.Outcome {
	case OutcomeErrored:
		if fm.failingProbes == nil {
			fm.failingProbes = make(map[string]map[string]probeSpell)
		}
		if fm.failingProbes[chID] == nil {
			fm.failingProbes[chID] = make(map[string]probeSpell)
		}
		spell := fm.failingProbes[chID][videoID]
		spell.rateLimited = res.RateLimited
		if res.RateLimited {
			fm.logger.Debug("status probe refused by a rate limit; row re-checked once the rate-limit hold has run out",
				"id", videoID, "channel", chID)
		} else if res.Inconclusive {
			spell.until = now.Add(endRecheckDelay(spell.holds))
			spell.holds++
			fm.logger.Debug("status probe inconclusive; row held",
				"id", videoID, "channel", chID, "until", spell.until.UTC().Format(time.RFC3339), "hold", spell.holds)
		}
		fm.failingProbes[chID][videoID] = spell
	case OutcomeProbed, OutcomeDenied:
		delete(fm.failingProbes[chID], videoID)
	}
}

// pruneFailureSpells drops chID's failure spells for rows outside scope, the
// walk's: a row that left scope while failing is not probed again, so its
// spell would otherwise stay for the life of the process. A row that comes
// back starts a new spell, and its first failure warns; its hold goes with
// the old one, so it is probed at once, as a newly seen row is.
func (fm *FeedMonitor) pruneFailureSpells(chID string, scope []database.FeedItem) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	spells := fm.failingProbes[chID]
	if len(spells) == 0 {
		return
	}
	inScope := make(map[string]struct{}, len(scope))
	for _, row := range scope {
		inScope[row.VideoID] = struct{}{}
	}
	for id := range spells {
		if _, ok := inScope[id]; !ok {
			delete(spells, id)
		}
	}
	if len(spells) == 0 {
		delete(fm.failingProbes, chID)
	}
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
// On a row whose re-check is still pending the schedule stands instead:
// that probe was not the re-check (the restart carve-out, or the unresolved
// arm, probing it on their own rules, without the end check — endCheckFor),
// so it neither advances the backoff nor restarts it.
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
	switch {
	case r.EndUnconfirmed && recheckPending(row, now.UTC().Format(time.RFC3339)):
		recheck = database.FeedRecheck{At: row.RecheckAt, Step: row.RecheckStep}
	case r.EndUnconfirmed:
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
