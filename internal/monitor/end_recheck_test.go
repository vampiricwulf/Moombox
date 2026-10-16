package monitor

import (
	"context"
	"errors"
	"slices"
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
// delay an hour); FeedScope's pending filter dropped (probed at +30 min,
// while the row is still in the window); Q3 dropped (never probed once out
// of the window).
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
