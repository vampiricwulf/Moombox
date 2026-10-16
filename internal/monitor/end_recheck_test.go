package monitor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// weakEnd is how the monitors' ANDROID_VR probe answers the field case: an
// age-gated ended stream, refused with nothing but a length on live content
// (TestBroadcastEnded's "age-gated ended" row) — upcoming, since it carries
// no formats, and ended on a weak signal alone. It is also exactly what an
// age-gated stream that is live NOW might look like, which nobody has
// captured, and why a weak end is confirmed before it is trusted.
func weakEnd() VideoProbeResult {
	return VideoProbeResult{StreamStatus: "upcoming", PlayabilityError: "age_restricted",
		BroadcastEnded: true, BroadcastEndWeak: true}
}

// countingProbe answers every probe with *answer, counting calls.
func countingProbe(n *int, answer *VideoProbeResult) VideoProbeFunc {
	return func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		*n++
		res := *answer
		res.Title = videoID
		return &res, nil
	}
}

// endCheckReturning is a ConfirmEndFunc that counts calls and answers chk,
// err.
func endCheckReturning(n *int, chk EndCheck, err error) ConfirmEndFunc {
	return func(ctx context.Context, videoID string) (EndCheck, error) {
		*n++
		return chk, err
	}
}

// seedRecheck seeds a row stored ended with its end re-check scheduled —
// what applyProbe writes for an unconfirmed end.
func seedRecheck(t *testing.T, db *database.Database, chID, videoID string, published time.Time, precision, source string, at time.Time, step int) {
	t.Helper()
	seedRow(t, db, chID, videoID, published, precision, source, "unknown")
	pub := published.UTC().Format(time.RFC3339)
	if err := db.ApplyProbeToFeedItem(chID, videoID, "vod", videoID, pub, precision,
		database.FeedRecheck{At: at.UTC().Format(time.RFC3339), Step: step}); err != nil {
		t.Fatalf("seedRecheck %s: %v", videoID, err)
	}
}

// A weak end is settled by the one signed-in WEB call before anyone trusts
// it; a strong end stands without one. The call's three answers: its own end
// is strong (ended — confirmed), it says live or upcoming (not ended — the
// broadcast job and the waiting loop, as before BroadcastEnded), or it could
// not run or said neither (ended for announcing, unconfirmed). Its date
// rides along beside the result's own, never in it (the callers take it only
// where the §9 date fetch would have run), and a refusal is decided before
// any of it.
//
// Mutants: a weak end trusted unconfirmed (the rewrite ignoring
// BroadcastEndWeak — the unsettled rows read confirmed and the end check is
// never asked); the OnAir answer ignored (the on-air row reads post_live);
// the Ended answer ignored (the confirmed row reads unconfirmed); the end
// check asked for a strong end too (its call count); the check's date not
// carried (the confirmed row's endCheckPublishedAt); the check's date
// written into PublishedAt, as 69342f6b did (the confirmed row's
// PublishedAt); endChecked left unset (the date fetch
// asks the same WEB call again); the check consulted ahead of isDenied (the
// members-only row's call count); a call cut short by the pass's own
// context treated as an unconfirmed end (the errored case).
func TestProbeAndClassify_AWeakEndIsConfirmedBeforeItIsTrusted(t *testing.T) {
	const date = "2026-09-11T02:14:17Z"
	strong := VideoProbeResult{StreamStatus: "upcoming", PlayabilityError: "unknown", BroadcastEnded: true}
	membersOnly := weakEnd()
	membersOnly.PlayabilityError = "members_only"
	boom := errors.New("player: transport error")
	for _, tc := range []struct {
		name            string
		probe           VideoProbeResult
		check           ConfirmEndFunc // nil: not wired
		chk             EndCheck
		checkErr        error
		wantOutcome     ProbeOutcome
		wantStatus      string
		wantUnconfirmed bool
		wantChecks      int
		wantDate        string
		wantChecked     bool
	}{
		{name: "strong end stands", probe: strong, chk: EndCheck{Ended: true},
			wantOutcome: OutcomeProbed, wantStatus: "post_live", wantChecks: 0},
		{name: "weak end, WEB confirms", probe: weakEnd(), chk: EndCheck{Ended: true, PublishedAt: date, PublishedPrecision: "started"},
			wantOutcome: OutcomeProbed, wantStatus: "post_live", wantChecks: 1, wantDate: date, wantChecked: true},
		{name: "weak end, WEB says on air", probe: weakEnd(), chk: EndCheck{OnAir: true},
			wantOutcome: OutcomeProbed, wantStatus: "upcoming", wantChecks: 1, wantChecked: true},
		{name: "weak end, WEB settles nothing", probe: weakEnd(), chk: EndCheck{PublishedAt: date, PublishedPrecision: "started"},
			wantOutcome: OutcomeProbed, wantStatus: "post_live", wantUnconfirmed: true, wantChecks: 1, wantDate: date, wantChecked: true},
		{name: "weak end, WEB call fails", probe: weakEnd(), checkErr: boom,
			wantOutcome: OutcomeProbed, wantStatus: "post_live", wantUnconfirmed: true, wantChecks: 1},
		{name: "weak end, no signed-in session", probe: weakEnd(), checkErr: ErrEndCheckUnavailable,
			wantOutcome: OutcomeProbed, wantStatus: "post_live", wantUnconfirmed: true, wantChecks: 1},
		{name: "members-only refusal first", probe: membersOnly, chk: EndCheck{Ended: true},
			wantOutcome: OutcomeDenied, wantStatus: "upcoming", wantChecks: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := 0
			probes := 0
			probe := tc.probe
			r := probeAndClassify(ProbeClassifyParams{Ctx: context.Background(), VideoID: "v",
				Channel: &config.ChannelConfig{Name: "c"}, ProbeVideo: countingProbe(&probes, &probe),
				Tracker: NewMetadataFailureTracker(), Logger: silentLogger{},
				ConfirmEnd: endCheckReturning(&checks, tc.chk, tc.checkErr)})
			if r.Outcome != tc.wantOutcome || r.StreamStatus != tc.wantStatus || r.EndUnconfirmed != tc.wantUnconfirmed {
				t.Errorf("outcome %v status %q unconfirmed %v, want %v %q %v",
					r.Outcome, r.StreamStatus, r.EndUnconfirmed, tc.wantOutcome, tc.wantStatus, tc.wantUnconfirmed)
			}
			if checks != tc.wantChecks {
				t.Errorf("end check asked %d times, want %d", checks, tc.wantChecks)
			}
			if r.endCheckPublishedAt != tc.wantDate || r.endChecked != tc.wantChecked {
				t.Errorf("check's date %q endChecked %v, want %q %v", r.endCheckPublishedAt, r.endChecked, tc.wantDate, tc.wantChecked)
			}
			if r.PublishedAt != "" {
				t.Errorf("the result's own date is %q, want the status probe's (none)", r.PublishedAt)
			}
		})
	}

	// Not wired at all is "could not run".
	probe := weakEnd()
	probes := 0
	r := probeAndClassify(ProbeClassifyParams{Ctx: context.Background(), VideoID: "v",
		Channel: &config.ChannelConfig{Name: "c"}, ProbeVideo: countingProbe(&probes, &probe),
		Tracker: NewMetadataFailureTracker(), Logger: silentLogger{}})
	if r.StreamStatus != "post_live" || !r.EndUnconfirmed {
		t.Errorf("no end check wired: status %q unconfirmed %v, want post_live, unconfirmed", r.StreamStatus, r.EndUnconfirmed)
	}

	// A call cut short because the pass ended learned nothing: an errored
	// probe (nothing written, retried next cycle), never an unconfirmed end
	// that would wait an hour for a question that was never asked.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r = probeAndClassify(ProbeClassifyParams{Ctx: ctx, VideoID: "v",
		Channel: &config.ChannelConfig{Name: "c"}, ProbeVideo: countingProbe(&probes, &probe),
		Tracker: NewMetadataFailureTracker(), Logger: silentLogger{},
		ConfirmEnd: func(ctx context.Context, videoID string) (EndCheck, error) {
			cancel()
			return EndCheck{}, ctx.Err()
		}})
	if r.Outcome != OutcomeErrored || r.EndUnconfirmed {
		t.Errorf("a call cut short by the pass: outcome %v unconfirmed %v, want errored", r.Outcome, r.EndUnconfirmed)
	}
}

// The re-check of an unconfirmed end runs on its backoff, and the backoff is
// the database's, not the process's: every cycle below is a fresh monitor
// over the same store, as after a restart. The row is announced nowhere and
// jobbed never (its channel archives VODs, and it is inside the window for
// the first day); it is probed an hour after the first probe, then six hours
// after that, then daily — and the daily one finds it after it has left the
// window, which only the re-check's own scope arm (Q3) reaches.
//
// Mutants: the re-check not written (applyProbe passing the zero
// FeedRecheck — the row is a settled vod and never probed again); the
// backoff flat at an hour (the second delay); the step not advanced (every
// delay an hour); the walk reading a pending re-check as due, or the
// archive refresh-probing one (probed at +30 min, while the row is still in
// the window); Q3 dropped (never probed once out of the window).
func TestFeed_AnUnconfirmedEndIsRecheckedOnTheBackoff(t *testing.T) {
	db := newTestDB(t)
	t0 := fixedNow()
	seedRow(t, db, "UC1", "weak1", t0.Add(-24*time.Hour), "exact", "rss", "upcoming")
	probes := 0
	answer := weakEnd()
	var found []foundCall
	cycle := func(at time.Time) {
		t.Helper()
		fm := newTestFeedMonitor(t, db, withRSS(rssWith()), withMembership(membWith()), withWindowDays(3),
			withProbe(countingProbe(&probes, &answer)), withNow(at))
		calls := recordVideoFound(fm)
		fm.runCycleForTest(t, "UC1")
		found = append(found, *calls...)
	}
	expect := func(label string, wantProbes int, wantAt time.Time, wantStep int) {
		t.Helper()
		row := mustGetFeedItem(t, db, "UC1", "weak1")
		if probes != wantProbes || row.Status != "vod" || row.RecheckAt != wantAt.Format(time.RFC3339) || row.RecheckStep != wantStep {
			t.Fatalf("%s: probes %d, row %q re-check %q step %d; want probes %d, vod, re-check %s step %d",
				label, probes, row.Status, row.RecheckAt, row.RecheckStep, wantProbes, wantAt.Format(time.RFC3339), wantStep)
		}
		if len(found) != 0 {
			t.Fatalf("%s: an unconfirmed end was jobbed: %v", label, found)
		}
	}

	cycle(t0)
	expect("first probe", 1, t0.Add(time.Hour), 1)
	cycle(t0.Add(30 * time.Minute))
	expect("before the first re-check", 1, t0.Add(time.Hour), 1)
	cycle(t0.Add(time.Hour))
	expect("first re-check", 2, t0.Add(7*time.Hour), 2)
	cycle(t0.Add(6 * time.Hour))
	expect("before the second", 2, t0.Add(7*time.Hour), 2)
	cycle(t0.Add(7 * time.Hour))
	expect("second re-check", 3, t0.Add(31*time.Hour), 3)
	cycle(t0.Add(31 * time.Hour))
	expect("third re-check", 4, t0.Add(55*time.Hour), 4)
	// Published t0-24h, so by t0+55h it is outside the 3-day window:
	// only Q3 carries it now.
	cycle(t0.Add(55 * time.Hour))
	expect("a daily re-check outside the window", 5, t0.Add(79*time.Hour), 5)
}

// The risk the re-check exists for: the probe reads a broadcast as ended on
// a weak signal alone — the field case's shape, and the shape an age-gated
// stream that is live NOW may well have — and an hour later YouTube says it
// is live. At bcab5da9 the row became a vod on the first probe and was never
// probed again (one probe over four cycles, no job, the row vod for good).
// Now the re-check finds it and takes it up as a newly found broadcast: a
// broadcast job, the row live, nothing left pending. The channel archives no
// VODs, so the archive's refresh probe never touches the vod row: only the
// walk's re-check arm can find it — and a broadcast is never gated on that
// setting.
//
// Mutants: a settled write leaving the pending re-check in place
// (ApplyProbeToFeedItem not writing the zero FeedRecheck — the live row
// keeps its re-check); the walk's re-check arm dropped (the restart
// carve-out skips the vod row), or Q3 (the row is out of every other arm) —
// either way it is never probed again and nothing is found, bcab5da9's
// behaviour.
func TestFeed_AReCheckThatFindsTheBroadcastOnAirAnnouncesIt(t *testing.T) {
	db := newTestDB(t)
	t0 := fixedNow()
	seedRow(t, db, "UC1", "weak1", t0.Add(-35*24*time.Hour), "exact", "rss", "upcoming")
	probes := 0
	answer := weakEnd()
	cycle := func(at time.Time) []foundCall {
		t.Helper()
		fm := newTestFeedMonitor(t, db, withRSS(rssWith()), withMembership(membWith()), withWindowDays(3),
			withProbe(countingProbe(&probes, &answer)), withNow(at))
		found := recordVideoFound(fm)
		if err := fm.checkChannel(context.Background(), &config.ChannelConfig{ID: "UC1", Name: "UC1"}); err != nil {
			t.Fatalf("checkChannel: %v", err)
		}
		return *found
	}
	if found := cycle(t0); len(found) != 0 {
		t.Fatalf("the weak end was announced: %v", found)
	}
	answer = VideoProbeResult{StreamStatus: "live", PlayabilityError: "ok"}
	if found := cycle(t0.Add(30 * time.Minute)); len(found) != 0 || probes != 1 {
		t.Fatalf("before the re-check: found %v, probes %d; want nothing, 1", found, probes)
	}
	found := cycle(t0.Add(time.Hour))
	if len(found) != 1 || found[0].d != DispositionBroadcast || probes != 2 {
		t.Fatalf("at the re-check: found %v, probes %d; want one broadcast job, 2", found, probes)
	}
	row := mustGetFeedItem(t, db, "UC1", "weak1")
	if row.Status != "live" || row.RecheckAt != "" || row.RecheckStep != 0 {
		t.Fatalf("row %q re-check %q step %d, want live and nothing pending", row.Status, row.RecheckAt, row.RecheckStep)
	}
}

// The end check's answers, end to end through the walk and the archive:
// confirmed, the row is a settled vod — archived like any ended stream on a
// channel that archives VODs, never probed again — and the check's date
// spares the date fetch, which would have been the same WEB call; on air,
// the broadcast job comes back exactly as it was before BroadcastEnded; and
// an answer that settles nothing schedules the re-check without a second
// WEB call for the date.
//
// Mutants: the date fetch asked after the end check (probeRowDated without
// its endChecked arm — two WEB calls for one question); the OnAir answer
// ignored (no broadcast job for a stream the WEB client says is live); the
// Ended answer ignored (the confirmed row is left pending, and unjobbed).
func TestFeed_TheEndCheckSettlesAWeakEnd(t *testing.T) {
	for _, tc := range []struct {
		name        string
		chk         EndCheck
		wantStatus  string
		wantRecheck bool
		wantFound   []JobDisposition
	}{
		{"confirmed", EndCheck{Ended: true}, "vod", false, []JobDisposition{DispositionBacklogVOD}},
		{"on air", EndCheck{OnAir: true}, "upcoming", false, []JobDisposition{DispositionBroadcast}},
		{"settles nothing", EndCheck{}, "vod", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			now := fixedNow()
			// A coarse row: its date is an estimate, so a dateless probe
			// would go on to the date fetch.
			seedRow(t, db, "UC1", "weak1", now.Add(-24*time.Hour), "coarse", "videos", "upcoming")
			probes, checks, dates := 0, 0, 0
			answer := weakEnd()
			fm := newTestFeedMonitor(t, db, withRSS(rssWith()), withMembership(membWith()), withWindowDays(3),
				withProbe(countingProbe(&probes, &answer)), withNow(now),
				withProbeDate(func(context.Context, string) (string, string, error) {
					dates++
					return "", "", nil
				}))
			fm.ConfirmEnd = endCheckReturning(&checks, tc.chk, nil)
			found := recordVideoFound(fm)
			fm.runCycleForTest(t, "UC1")

			var got []JobDisposition
			for _, c := range *found {
				got = append(got, c.d)
			}
			if !slices.Equal(got, tc.wantFound) {
				t.Errorf("jobs %v, want %v", got, tc.wantFound)
			}
			row := mustGetFeedItem(t, db, "UC1", "weak1")
			if row.Status != tc.wantStatus || (row.RecheckAt != "") != tc.wantRecheck {
				t.Errorf("row %q re-check %q, want %q, re-check pending %v", row.Status, row.RecheckAt, tc.wantStatus, tc.wantRecheck)
			}
			if probes != 1 || checks != 1 || dates != 0 {
				t.Errorf("probes %d, end checks %d, date fetches %d; want 1, 1, 0", probes, checks, dates)
			}
		})
	}
}

// The archive's refresh probe meets an unconfirmed end too — a due row the
// walk did not settle (its probe failed there), probed again by the archive
// on a channel that archives VODs. It must make no job: ended for
// announcing, and for nothing else.
//
// Mutant: emitJob's unconfirmed-end gate dropped (a VOD job on an end nobody
// confirmed).
func TestArchive_AnUnconfirmedEndMakesNoJob(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	seedRecheck(t, db, "UC1", "weak1", now.Add(-24*time.Hour), "exact", "rss", now.Add(-time.Minute), 1)
	probes := 0
	fm := newTestFeedMonitor(t, db, withRSS(rssWith()), withMembership(membWith()), withWindowDays(3),
		withProbe(func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
			probes++
			if probes == 1 {
				return nil, errors.New("player: transport error") // the walk's probe
			}
			res := weakEnd()
			return &res, nil // the archive's refresh probe
		}), withNow(now))
	found := recordVideoFound(fm)
	fm.runCycleForTest(t, "UC1")

	if probes != 2 {
		t.Fatalf("probes %d, want 2 — the walk's, then the archive's refresh", probes)
	}
	if len(*found) != 0 {
		t.Fatalf("an unconfirmed end was jobbed: %v", *found)
	}
	row := mustGetFeedItem(t, db, "UC1", "weak1")
	if row.RecheckAt != now.Add(6*time.Hour).Format(time.RFC3339) || row.RecheckStep != 2 {
		t.Fatalf("row re-check %q step %d, want the next backoff (+6h, step 2)", row.RecheckAt, row.RecheckStep)
	}
}

// A due re-check is in scope because its backoff ran out, not because of
// where a listing put it: it is neither skipped by the exhaustion of its
// source nor read for the source's date order. Here a coarse row of the
// date-ordered 'videos' source probes to a date outside the window and
// exhausts the source; the re-check of an older row of the same source,
// carried by Q3 after it, is still probed, and its date — newer than the
// coarse row's — raises no ordering warning.
//
// Mutants: the exhaustion skip without its re-check exemption (the due row
// is skipped, and stays due cycle after cycle while the source keeps
// exhausting first); the ordering check run on a re-check row (the
// "listing order violated" warning).
func TestWalk_ADueReCheckIsNotAListingPosition(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	seedCoarseLump(t, db, "UC1", now, "videos", "c1")
	seedRecheck(t, db, "UC1", "r1", now.Add(-40*24*time.Hour), "exact", "videos", now.Add(-time.Minute), 1)
	var probed []string
	fm := newTestFeedMonitor(t, db, withProbe(func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		probed = append(probed, videoID)
		if videoID == "c1" {
			return &VideoProbeResult{StreamStatus: "vod", PublishedAt: now.Add(-20 * 24 * time.Hour).Format(time.RFC3339), PublishedPrecision: "day"}, nil
		}
		res := weakEnd()
		res.PublishedAt, res.PublishedPrecision = now.Add(-15*24*time.Hour).Format(time.RFC3339), "started"
		return &res, nil
	}), withWindowDays(10), withNow(now))
	rec := &warnRecordingLogger{}
	fm.logger = rec
	fm.walkForTest(t, "UC1")

	if !slices.Equal(probed, []string{"c1", "r1"}) {
		t.Fatalf("probed %v, want [c1 r1] — the due re-check after the source exhausted", probed)
	}
	for _, w := range rec.warnings {
		if w == "listing order violated; early exit disabled" {
			t.Fatalf("a re-check row tripped the ordering check: %v", rec.warnings)
		}
	}
}

// ProcessYouTubeVideo (DECAPI's path) takes the same end check and draws
// the same line: an unconfirmed end is no job even on a channel that
// archives VODs, and a WEB answer that says on air is the broadcast job.
//
// Mutants: ProcessYouTubeVideo's unconfirmed-end arm dropped (the
// unconfirmed end proceeds as an included VOD); ConfirmEnd not passed on to
// probeAndClassify (the on-air answer is never asked for, and the stream
// reads ended).
func TestProcessYouTubeVideo_AnUnconfirmedEndIsNoJob(t *testing.T) {
	ch := &config.ChannelConfig{Name: "c", IncludeNonLiveContent: true}
	run := func(check ConfirmEndFunc) ProcessYouTubeVideoResult {
		probes := 0
		answer := weakEnd()
		return ProcessYouTubeVideo(ProcessYouTubeVideoParams{Ctx: context.Background(), VideoID: "v", Title: "t",
			Channel: ch, ProbeVideo: countingProbe(&probes, &answer), Tracker: NewMetadataFailureTracker(),
			ConfirmEnd: check, Logger: silentLogger{}})
	}
	if r := run(nil); r.ShouldProcess || !r.EndUnconfirmed || r.StreamStatus != "post_live" {
		t.Errorf("unconfirmed: ShouldProcess %v EndUnconfirmed %v status %q, want false true post_live",
			r.ShouldProcess, r.EndUnconfirmed, r.StreamStatus)
	}
	checks := 0
	if r := run(endCheckReturning(&checks, EndCheck{OnAir: true}, nil)); !r.ShouldProcess || r.EndUnconfirmed || r.StreamStatus != "upcoming" || checks != 1 {
		t.Errorf("on air: ShouldProcess %v EndUnconfirmed %v status %q checks %d, want true false upcoming 1",
			r.ShouldProcess, r.EndUnconfirmed, r.StreamStatus, checks)
	}
}

// The end check's date is the §9 date fetch's answer, so it is taken only
// where that fetch would have run — over a coarse, assumed or missing date,
// or the date of an rss row announced before the window — and never over a
// date the row already holds as authoritative. The field case for the
// second half is an RSS row ('exact') on a channel that archives no VODs: a
// weak end the check confirms is a settled vod, probed once, as a strong end
// is. With the check's 'started' written over its 'exact' it became the
// restart carve-out's shape (vod, started, no job) and was probed — and the
// signed-in WEB call made again — every cycle for the whole window.
//
// Mutants: confirmEnd writing the check's date into PublishedAt, as
// 69342f6b did (the exact row's date 'started', six probes and six end
// checks); probeRowDated taking the check's date ahead of its
// authoritative-date test (the same); probeRowDated not taking it at all
// (the coarse, assumed and announced-early rows left on their estimates);
// the date fetch asked after the end check (a date fetch).
func TestFeed_TheEndChecksDateIsTakenOnlyWhereTheDateFetchWouldRun(t *testing.T) {
	t0 := fixedNow()
	started := t0.Add(-5 * time.Hour).Format(time.RFC3339)
	confirmed := EndCheck{Ended: true, PublishedAt: started, PublishedPrecision: "started"}

	t.Run("an authoritative date stands, and the settled vod is probed once", func(t *testing.T) {
		db := newTestDB(t)
		announced := t0.Add(-6 * time.Hour)
		seedRow(t, db, "UC1", "e1", announced, "exact", "rss", "unknown")
		probes, checks, dates := 0, 0, 0
		answer := weakEnd()
		for i := range 6 {
			fm := newTestFeedMonitor(t, db, withRSS(rssWith()), withMembership(membWith()), withWindowDays(3),
				withProbe(countingProbe(&probes, &answer)), withNow(t0.Add(time.Duration(i)*10*time.Minute)),
				withProbeDate(func(context.Context, string) (string, string, error) {
					dates++
					return started, "started", nil
				}))
			fm.ConfirmEnd = endCheckReturning(&checks, confirmed, nil)
			// include_non_live_content off: nothing but the walk looks at the row.
			if err := fm.checkChannel(context.Background(), &config.ChannelConfig{ID: "UC1", Name: "UC1"}); err != nil {
				t.Fatalf("checkChannel: %v", err)
			}
		}
		row := mustGetFeedItem(t, db, "UC1", "e1")
		if row.Status != "vod" || row.DatePrecision != "exact" || row.Published != announced.Format(time.RFC3339) || row.RecheckAt != "" {
			t.Errorf("row %q %q %s re-check %q, want a settled vod on its own exact date %s",
				row.Status, row.DatePrecision, row.Published, row.RecheckAt, announced.Format(time.RFC3339))
		}
		if probes != 1 || checks != 1 || dates != 0 {
			t.Errorf("over 6 cycles: probes %d, end checks %d, date fetches %d; want 1, 1, 0", probes, checks, dates)
		}
	})

	for _, tc := range []struct {
		name      string
		published time.Time
		precision string
		source    string
	}{
		{"a coarse row", t0.Add(-24 * time.Hour), "coarse", "videos"},
		{"an assumed row", t0, "assumed", "rss"},
		{"an rss row announced before the window", t0.Add(-5 * 24 * time.Hour), "exact", "rss"},
	} {
		t.Run("the check's date completes "+tc.name, func(t *testing.T) {
			db := newTestDB(t)
			// First seen now: an rss row announced before the window is in
			// scope by Q2's unresolved arm (§7).
			if _, err := db.UpsertFeedItem(database.FeedItem{ChannelID: "UC1", VideoID: "w1", Title: "w1",
				Published: tc.published.Format(time.RFC3339), DatePrecision: tc.precision,
				Source: tc.source, FirstSeen: t0.Format(time.RFC3339)}); err != nil {
				t.Fatal(err)
			}
			probes, checks, dates := 0, 0, 0
			answer := weakEnd()
			fm := newTestFeedMonitor(t, db, withWindowDays(3), withProbe(countingProbe(&probes, &answer)), withNow(t0),
				withProbeDate(func(context.Context, string) (string, string, error) {
					dates++
					return "", "", nil
				}))
			fm.ConfirmEnd = endCheckReturning(&checks, confirmed, nil)
			fm.walkForTest(t, "UC1")
			row := mustGetFeedItem(t, db, "UC1", "w1")
			if row.Status != "vod" || row.DatePrecision != "started" || row.Published != started {
				t.Errorf("row %q %q %s, want vod on the check's started date %s", row.Status, row.DatePrecision, row.Published, started)
			}
			if probes != 1 || checks != 1 || dates != 0 {
				t.Errorf("probes %d, end checks %d, date fetches %d; want 1, 1, 0", probes, checks, dates)
			}
		})
	}
}

// A pending re-check takes nothing away from the restart carve-out (§8): a
// vod row with a started date and no job is probed every cycle whether or
// not its end is waiting on the backoff, so a stream that resumes on the
// same video ID is found on the next cycle, not an hour (six hours, a day)
// later. Those probes are not the re-check: they make no end check and move
// no schedule, and the re-check, when due, still asks and still advances.
// The row is the field shape: a /streams-tab row (coarse), last probed
// upcoming, on a channel that archives no VODs; its ended stream answers on
// weak signals alone, nobody can confirm them, and the date fetch dates it
// 'started'.
//
// Mutants: a pending row kept out of FeedScope's Q1 again, as 69342f6b did
// (the carve-out never probes it: one probe at +10 min short, and the
// resumed stream unfound); the walk's carve-out skipping a pending row (the
// same); endCheckFor giving a pending row the end check (an end check every
// cycle); applyProbe advancing the backoff on a pending row's probe (the
// schedule moves at +10 min); confirmEnd logging a withheld check's answer
// as an unconfirmed end re-checked later (that Debug line on the carve-out's
// cycles).
func TestWalk_APendingReCheckKeepsTheRestartCarveOut(t *testing.T) {
	db := newTestDB(t)
	t0 := fixedNow()
	seedRow(t, db, "UC1", "s1", t0.Add(-24*time.Hour), "coarse", "streams", "upcoming")
	started := t0.Add(-20 * time.Hour).Format(time.RFC3339)
	probes, checks := 0, 0
	answer := weakEnd()
	var rec *warnRecordingLogger
	cycle := func(at time.Time) []foundCall {
		t.Helper()
		fm := newTestFeedMonitor(t, db, withRSS(rssWith()), withMembership(membWith()), withWindowDays(3),
			withProbe(countingProbe(&probes, &answer)), withNow(at),
			withProbeDate(func(context.Context, string) (string, string, error) { return started, "started", nil }))
		rec = &warnRecordingLogger{}
		fm.logger = rec
		fm.ConfirmEnd = endCheckReturning(&checks, EndCheck{}, ErrEndCheckUnavailable)
		found := recordVideoFound(fm)
		// include_non_live_content off: the carve-out is the walk's one rule for this vod row.
		if err := fm.checkChannel(context.Background(), &config.ChannelConfig{ID: "UC1", Name: "UC1"}); err != nil {
			t.Fatalf("checkChannel: %v", err)
		}
		return *found
	}
	// scheduled: the cycle logged an unconfirmed end as re-checked later.
	scheduled := func() bool {
		for _, d := range rec.debugs {
			if strings.Contains(d, "re-checked later") {
				return true
			}
		}
		return false
	}
	expect := func(label string, found []foundCall, wantProbes, wantChecks int, wantAt string, wantStep int, wantScheduled bool) {
		t.Helper()
		row := mustGetFeedItem(t, db, "UC1", "s1")
		if probes != wantProbes || checks != wantChecks || len(found) != 0 ||
			row.Status != "vod" || row.RecheckAt != wantAt || row.RecheckStep != wantStep {
			t.Fatalf("%s: probes %d, end checks %d, found %v, row %q %q re-check %q step %d; want %d, %d, nothing, vod started, %q step %d",
				label, probes, checks, found, row.Status, row.DatePrecision, row.RecheckAt, row.RecheckStep,
				wantProbes, wantChecks, wantAt, wantStep)
		}
		if scheduled() != wantScheduled {
			t.Fatalf("%s: logged an end re-checked later %v, want %v (debugs %q)", label, scheduled(), wantScheduled, rec.debugs)
		}
	}
	at := func(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }

	expect("first probe", cycle(t0), 1, 1, at(time.Hour), 1, true)
	if row := mustGetFeedItem(t, db, "UC1", "s1"); row.DatePrecision != "started" {
		t.Fatalf("fixture: the row is %q, want the carve-out's started date", row.DatePrecision)
	}
	expect("the carve-out, re-check pending", cycle(t0.Add(10*time.Minute)), 2, 1, at(time.Hour), 1, false)
	expect("the re-check, due", cycle(t0.Add(time.Hour)), 3, 2, at(7*time.Hour), 2, true)
	expect("the carve-out again", cycle(t0.Add(70*time.Minute)), 4, 2, at(7*time.Hour), 2, false)

	// The stream resumes on the same ID: the next cycle's carve-out finds it.
	answer = VideoProbeResult{StreamStatus: "live", PlayabilityError: "ok"}
	found := cycle(t0.Add(80 * time.Minute))
	if probes != 5 || len(found) != 1 || found[0].d != DispositionBroadcast {
		t.Fatalf("the stream live again: probes %d, found %v; want 5 and one broadcast job", probes, found)
	}
	if row := mustGetFeedItem(t, db, "UC1", "s1"); row.Status != "live" || row.RecheckAt != "" || row.RecheckStep != 0 {
		t.Fatalf("row %q re-check %q step %d, want live and nothing pending", row.Status, row.RecheckAt, row.RecheckStep)
	}
}

// The restart carve-out's probe of a row holding a settled end makes no
// end check: it looks for a stream that resumed, which the status probe
// reads live or upcoming, and a weak end from it agrees with the end the row
// already holds — settled it stays. Asked every cycle, the signed-in WEB call
// cost one more request per cycle for the whole window on every such row
// (about 430 at the default poll and window), and an unconfirmed answer
// would have turned a settled end back into a pending one. Two rows have the
// carve-out's shape (vod, started, no job): one a strong end settled, and a
// coarse /streams-tab row whose weak end the end check confirmed, taking the
// check's 'started' date because the date fetch would have run — asked once,
// on the probe that settled it, and never again.
//
// Nor does it log the answer as an unconfirmed end to be re-checked: no
// re-check is scheduled on its account.
//
// Mutants: endCheckFor giving a settled row the end check (an end check per
// cycle); probeRowDated not settling a settled row's weak answer (the
// strong-settled row goes pending on the first cycle); confirmEnd logging a
// withheld check's answer as unconfirmed and re-checked later (that Debug
// line every cycle).
func TestWalk_TheCarveOutProbeOfASettledEndMakesNoEndCheck(t *testing.T) {
	t0 := fixedNow()
	started := t0.Add(-20 * time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name       string
		seed       func(t *testing.T, db *database.Database)
		check      EndCheck
		checkErr   error
		wantChecks int
	}{
		{"a strong end settled it", func(t *testing.T, db *database.Database) {
			seedRow(t, db, "UC1", "r1", t0.AddDate(0, 0, -1), "started", "rss", "vod")
		}, EndCheck{}, ErrEndCheckUnavailable, 0},
		{"the end check settled it", func(t *testing.T, db *database.Database) {
			seedRow(t, db, "UC1", "r1", t0.Add(-24*time.Hour), "coarse", "streams", "upcoming")
		}, EndCheck{Ended: true, PublishedAt: started, PublishedPrecision: "started"}, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			tc.seed(t, db)
			probes, checks, dates := 0, 0, 0
			answer := weakEnd()
			for i := range 6 {
				rec := &warnRecordingLogger{}
				fm := newTestFeedMonitor(t, db, withRSS(rssWith()), withMembership(membWith()), withWindowDays(3),
					withProbe(countingProbe(&probes, &answer)), withNow(t0.Add(time.Duration(i)*10*time.Minute)),
					withProbeDate(func(context.Context, string) (string, string, error) {
						dates++
						return started, "started", nil
					}))
				fm.logger = rec
				fm.ConfirmEnd = endCheckReturning(&checks, tc.check, tc.checkErr)
				found := recordVideoFound(fm)
				// include_non_live_content off: the carve-out is the walk's one rule for this vod row.
				if err := fm.checkChannel(context.Background(), &config.ChannelConfig{ID: "UC1", Name: "UC1"}); err != nil {
					t.Fatalf("checkChannel: %v", err)
				}
				if len(*found) != 0 {
					t.Fatalf("cycle %d: an ended broadcast was jobbed: %v", i, *found)
				}
				for _, d := range rec.debugs {
					if strings.Contains(d, "re-checked later") {
						t.Fatalf("cycle %d: a settled end's carve-out probe logged %q", i, d)
					}
				}
			}
			row := mustGetFeedItem(t, db, "UC1", "r1")
			if probes != 6 || checks != tc.wantChecks || dates != 0 ||
				row.Status != "vod" || row.DatePrecision != "started" || row.RecheckAt != "" {
				t.Fatalf("over 6 cycles: probes %d, end checks %d, date fetches %d, row %q %q re-check %q; want 6 (the carve-out's), %d, 0, a settled vod on its started date",
					probes, checks, dates, row.Status, row.DatePrecision, row.RecheckAt, tc.wantChecks)
			}
		})
	}
}

// The archive's refresh probe follows the same two rules. A row holding a
// settled terminal classification — a vod, or a not_a_stream (a finished
// premiere whose formats are withheld now reads the same weak end) — is
// refreshed without the end check, and a weak answer agrees with it: the
// VOD job it was always going to get, as at bcab5da9. A row whose re-check
// is pending is not refresh-probed at all — no job until a re-check settles
// it, and a probe every cycle while it waits is the cost the backoff bounds.
// The settled rows are what include_non_live_content turned on after they
// settled leaves behind, or the channel established after, or an orphaned
// history row cleared.
//
// Mutants: endCheckFor giving a settled vod the end check, or a settled
// not_a_stream (an end check, and the unconfirmed answer makes no job);
// probeRowDated not settling a settled row's weak answer (no job, and the
// row goes pending); the archive's pending gate dropped (a refresh probe of
// the pending row).
func TestArchive_TheRefreshProbeAndTheEndReCheck(t *testing.T) {
	for _, status := range []string{"vod", "not_a_stream"} {
		t.Run("a settled "+status+" is refreshed without the end check", func(t *testing.T) {
			db := newTestDB(t)
			now := fixedNow()
			seedRow(t, db, "UC1", "v1", now.Add(-24*time.Hour), "exact", "rss", status)
			probes, checks := 0, 0
			answer := weakEnd()
			fm := newTestFeedMonitor(t, db, withRSS(rssWith()), withMembership(membWith()), withWindowDays(3),
				withProbe(countingProbe(&probes, &answer)), withNow(now))
			fm.ConfirmEnd = endCheckReturning(&checks, EndCheck{}, ErrEndCheckUnavailable)
			found := recordVideoFound(fm)
			fm.runCycleForTest(t, "UC1") // archives VODs
			if probes != 1 || checks != 0 || len(*found) != 1 || (*found)[0].d != DispositionBacklogVOD {
				t.Fatalf("probes %d, end checks %d, found %v; want the refresh probe, no end check, one backlog VOD job",
					probes, checks, *found)
			}
			if row := mustGetFeedItem(t, db, "UC1", "v1"); row.RecheckAt != "" {
				t.Fatalf("the settled row went pending: re-check %q", row.RecheckAt)
			}
		})
	}
	t.Run("a pending re-check is not refreshed", func(t *testing.T) {
		db := newTestDB(t)
		now := fixedNow()
		seedRecheck(t, db, "UC1", "v1", now.Add(-24*time.Hour), "exact", "rss", now.Add(time.Hour), 1)
		probes := 0
		answer := weakEnd()
		fm := newTestFeedMonitor(t, db, withRSS(rssWith()), withMembership(membWith()), withWindowDays(3),
			withProbe(countingProbe(&probes, &answer)), withNow(now))
		found := recordVideoFound(fm)
		fm.runCycleForTest(t, "UC1")
		row := mustGetFeedItem(t, db, "UC1", "v1")
		if probes != 0 || len(*found) != 0 || row.RecheckAt != now.Add(time.Hour).Format(time.RFC3339) || row.RecheckStep != 1 {
			t.Fatalf("probes %d, found %v, re-check %q step %d; want no probe, no job, the schedule as it was",
				probes, *found, row.RecheckAt, row.RecheckStep)
		}
	})
}

// endCheckFor, row by row: the end check is for an end nobody has settled —
// a first probe of a row stored unknown, upcoming or live, or a due re-check
// — and for nothing else.
//
// Mutants: either withheld arm dropped (the pending or the settled rows get
// the end check); settledTerminal without not_a_stream (the settled
// not_a_stream gets it); the pending test reading a due re-check as pending
// (the due rows get none).
func TestEndCheckFor(t *testing.T) {
	now := fixedNow()
	pending := now.Add(time.Minute).Format(time.RFC3339)
	due := now.Format(time.RFC3339)
	checks := 0
	fm := &FeedMonitor{ConfirmEnd: endCheckReturning(&checks, EndCheck{Ended: true}, nil)}
	for _, tc := range []struct {
		name  string
		row   database.FeedItem
		wants bool
	}{
		{"upcoming", database.FeedItem{Status: "upcoming"}, true},
		{"unknown", database.FeedItem{Status: "unknown"}, true},
		{"live", database.FeedItem{Status: "live"}, true},
		{"vod, re-check due", database.FeedItem{Status: "vod", RecheckAt: due}, true},
		{"unknown, re-check due", database.FeedItem{Status: "unknown", RecheckAt: due}, true},
		{"vod, settled", database.FeedItem{Status: "vod"}, false},
		{"not_a_stream, settled", database.FeedItem{Status: "not_a_stream"}, false},
		{"vod, re-check pending", database.FeedItem{Status: "vod", RecheckAt: pending}, false},
		{"unknown, re-check pending", database.FeedItem{Status: "unknown", RecheckAt: pending}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := checks
			_, err := fm.endCheckFor(tc.row, now)(context.Background(), "v")
			asked := checks > before
			if asked != tc.wants || (!asked && !errors.Is(err, errEndCheckWithheld)) {
				t.Errorf("end check asked %v (err %v), want asked %v", asked, err, tc.wants)
			}
		})
	}
}

// A row whose re-check is still pending is where its arm put it, like any
// row — unlike a due one, which Q3 carries wherever its date is: its source
// exhausting skips it, as it skips the carve-out row it is, and its date is
// read for the source's order.
//
// Mutants: the exhaustion skip exempting every re-check row, pending
// included (the pending row behind the exhausted source is probed); the
// ordering check skipping a pending row's date (no "listing order violated"
// warning when the next row probes newer).
func TestWalk_APendingReCheckIsAListingPosition(t *testing.T) {
	now := fixedNow()
	date := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	t.Run("skipped behind an exhausted source", func(t *testing.T) {
		db := newTestDB(t)
		seedCoarseLump(t, db, "UC1", now, "videos", "c1")
		seedRecheck(t, db, "UC1", "p1", now.Add(-8*24*time.Hour), "started", "videos", now.Add(time.Hour), 1)
		var probed []string
		fm := newTestFeedMonitor(t, db, withProbe(func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
			probed = append(probed, videoID)
			if videoID == "c1" {
				return &VideoProbeResult{StreamStatus: "vod", PublishedAt: date(20 * 24 * time.Hour), PublishedPrecision: "day"}, nil
			}
			res := weakEnd()
			return &res, nil
		}), withWindowDays(10), withNow(now))
		fm.walkForTest(t, "UC1")
		if !slices.Equal(probed, []string{"c1"}) {
			t.Fatalf("probed %v, want [c1] — the pending row behind the exhausted source skipped", probed)
		}
	})
	t.Run("read for the source's order", func(t *testing.T) {
		db := newTestDB(t)
		seedRecheck(t, db, "UC1", "p1", now.Add(-24*time.Hour), "started", "videos", now.Add(time.Hour), 1)
		seedCoarseLump(t, db, "UC1", now, "videos", "c1")
		fm := newTestFeedMonitor(t, db, withProbe(func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
			if videoID == "c1" {
				return &VideoProbeResult{StreamStatus: "vod", PublishedAt: date(12 * time.Hour), PublishedPrecision: "day"}, nil
			}
			res := weakEnd()
			res.PublishedAt, res.PublishedPrecision = date(24*time.Hour), "started"
			return &res, nil
		}), withWindowDays(10), withNow(now))
		rec := &warnRecordingLogger{}
		fm.logger = rec
		fm.walkForTest(t, "UC1")
		if !slices.Contains(rec.warnings, "listing order violated; early exit disabled") {
			t.Fatalf("warnings %v: the coarse row probed newer than the pending one before it, want the ordering warning", rec.warnings)
		}
	})
}
