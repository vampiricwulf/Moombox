package monitor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// newTestDecapiMonitor builds a DecapiMonitor over a real (temp-file) db and
// an in-memory config store — the DECAPI counterpart of newTestFeedMonitor.
// Tests drive processResponse directly with a synthesized DECAPI body; the
// HTTP layer (checkChannel) is not under test here.
func newTestDecapiMonitor(t *testing.T, db *database.Database, probe VideoProbeFunc) *DecapiMonitor {
	t.Helper()
	dm := NewDecapiMonitor(
		config.NewStore(config.Defaults(), ""),
		db,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	dm.ProbeVideo = probe
	return dm
}

// recordDecapiVideoFound wires dm.OnVideoFound to a recorder and returns a
// pointer to the recorded calls, in emission order (archive_test.go's
// recordVideoFound, for the DECAPI monitor).
func recordDecapiVideoFound(dm *DecapiMonitor) *[]foundCall {
	calls := &[]foundCall{}
	dm.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig, d JobDisposition) {
		*calls = append(*calls, foundCall{videoID: videoID, d: d})
	}
	return calls
}

// decapiBody renders the DECAPI latest_video response shape processResponse
// parses: "<title> - <video URL>". videoID must be 11 chars (the real ID
// shape decapiVideoIDRe captures).
func decapiBody(videoID, title string) string {
	return fmt.Sprintf("%s - https://youtu.be/%s", title, videoID)
}

// TestDecapi_VodOutsideWindowNotJobbed covers §13's date check: "the newest
// video on the channel" is not the same as "recent" — on a dormant channel
// with include_non_live_content=true, DECAPI's first cycle must not job a
// six-month-old VOD against the default 3-day window (the headline bug
// through a second door).
//
// The DECAPI monitor has no injectable clock — the window check reads
// time.Now() directly — so the fixture's published date sits 6 months behind
// the 3-day window: far enough from the boundary that wall-clock skew cannot
// flip the assertion.
func TestDecapi_VodOutsideWindowNotJobbed(t *testing.T) {
	db := newTestDB(t)
	published := time.Now().UTC().Add(-6 * 30 * 24 * time.Hour).Format(time.RFC3339)
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		return &VideoProbeResult{
			StreamStatus: "vod", Title: "old vod",
			PublishedAt: published, PublishedPrecision: "day",
		}, nil
	})
	found := recordDecapiVideoFound(dm)

	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1", IncludeNonLiveContent: true}
	if err := dm.processResponse(context.Background(), decapiBody("vidDecOld01", "old vod"), ch); err != nil {
		t.Fatalf("processResponse: %v", err)
	}
	if len(*found) != 0 {
		t.Fatalf("a 6-month-old VOD was jobbed against a 3-day window: %v", *found)
	}
}

// TestDecapi_LiveNeverWindowBlocked covers §13's other half: live/upcoming
// job ALWAYS, no date check — this IS the RSS redundancy, and a date must
// never block it. A broadcast probe supplies no date at all (§12), so a
// window rule that consulted the (empty) date here would block every live
// stream DECAPI exists to catch.
func TestDecapi_LiveNeverWindowBlocked(t *testing.T) {
	db := newTestDB(t)
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		return &VideoProbeResult{StreamStatus: "live", Title: "going live"}, nil // dateless, like every broadcast probe
	})
	found := recordDecapiVideoFound(dm)

	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1"}
	if err := dm.processResponse(context.Background(), decapiBody("vidDecLiv02", "going live"), ch); err != nil {
		t.Fatalf("processResponse: %v", err)
	}
	if len(*found) != 1 || (*found)[0].videoID != "vidDecLiv02" || (*found)[0].d != DispositionBroadcast {
		t.Fatalf("found = %v, want [{vidDecLiv02 broadcast}] — a date must never block the redundancy (§13)", *found)
	}
}

// TestDecapi_DatelessVodTreatedAsOutside: a vod-family probe with no date
// cannot verify the window ⇒ treated as outside, no job. Unlike the feed
// path (§12's terminal invariant keeps such a row 'unknown' in the store to
// self-heal on a later dated probe), DECAPI writes no feed_items row, so
// there is nothing to heal — the skip is final for this sighting.
func TestDecapi_DatelessVodTreatedAsOutside(t *testing.T) {
	db := newTestDB(t)
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		return &VideoProbeResult{StreamStatus: "vod", Title: "dateless vod"}, nil
	})
	found := recordDecapiVideoFound(dm)

	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1", IncludeNonLiveContent: true}
	if err := dm.processResponse(context.Background(), decapiBody("vidDecNod03", "dateless vod"), ch); err != nil {
		t.Fatalf("processResponse: %v", err)
	}
	if len(*found) != 0 {
		t.Fatalf("a dateless VOD was jobbed: %v — the window cannot be verified, and DECAPI has no store row to self-heal from", *found)
	}
}

// TestDecapi_TwoPhaseDateFetch: production status probes are dateless, so
// without the §9 date fetch EVERY DECAPI vod sighting would be window-
// unverifiable and skipped. An in-window fetched date must job.
func TestDecapi_TwoPhaseDateFetch(t *testing.T) {
	db := newTestDB(t)
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		return &VideoProbeResult{StreamStatus: "vod", Title: "fresh vod"}, nil // dateless
	})
	var fetched []string
	dm.ProbeDate = func(ctx context.Context, videoID string) (string, string, error) {
		fetched = append(fetched, videoID)
		return time.Now().UTC().Add(-6 * time.Hour).Format(time.RFC3339), "day", nil
	}
	found := recordDecapiVideoFound(dm)

	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1", IncludeNonLiveContent: true}
	if err := dm.processResponse(context.Background(), decapiBody("vidDecTpf04", "fresh vod"), ch); err != nil {
		t.Fatalf("processResponse: %v", err)
	}
	if len(fetched) != 1 || fetched[0] != "vidDecTpf04" {
		t.Fatalf("date-fetch calls = %v, want exactly [vidDecTpf04]", fetched)
	}
	if len(*found) != 1 || (*found)[0].videoID != "vidDecTpf04" {
		t.Fatalf("an in-window fetched date must job: %v", *found)
	}
}

// TestDecapi_DateFetchErrorSkips: a FAILED date fetch leaves the window
// unverifiable ⇒ the §13 treated-as-outside arm (no job, Info-logged);
// a later sighting retries.
//
// The second cycle is the pin for the guard at decapi.go:747
// (`if result.PublishedAt != ""` inside the window-skip arm). A DATED
// verdict is durable and is memoized; a DATELESS one must not be, because
// "treated as outside" there means only that this cycle could not verify the
// window. Latching it would let one transient DECAPI/date failure freeze the
// channel until its newest video changes — and on an
// include_non_live_content channel the out-of-window arm writes no history
// row, so `reprobe` never re-opens it either.
//
// Mutant this kills: deleting the `if result.PublishedAt != ""` guard so the
// dateless skip calls noteTerminalMemoOutsideWindow. Cycle 2 then hits the
// memo and costs no requests at all — probes/dates stay 1/1. (Before this
// cycle existed, deleting the guard passed the whole package.)
func TestDecapi_DateFetchErrorSkips(t *testing.T) {
	db := newTestDB(t)
	probes, dates := 0, 0
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		probes++
		return &VideoProbeResult{StreamStatus: "vod", Title: "fresh vod"}, nil
	})
	dm.ProbeDate = func(ctx context.Context, videoID string) (string, string, error) {
		dates++
		return "", "", fmt.Errorf("boom")
	}
	found := recordDecapiVideoFound(dm)

	// include_non_live_content keeps the vod arm from writing a history row,
	// so `reprobe` is false on every cycle and the memo's outsideWindow arm
	// is the only one that could ever fire here.
	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1", IncludeNonLiveContent: true}
	body := decapiBody("vidDecTpe05", "fresh vod")

	if err := dm.processResponse(context.Background(), body, ch); err != nil {
		t.Fatalf("cycle 1: processResponse: %v", err)
	}
	if len(*found) != 0 {
		t.Fatalf("a window-unverifiable vod must not job: %v", *found)
	}
	if probes != 1 || dates != 1 {
		t.Fatalf("cycle 1: probes=%d dates=%d, want 1/1 — the first sighting classifies and tries to date the video", probes, dates)
	}

	if err := dm.processResponse(context.Background(), body, ch); err != nil {
		t.Fatalf("cycle 2: processResponse: %v", err)
	}
	if probes != 2 || dates != 2 {
		t.Fatalf("cycle 2: probes=%d dates=%d, want 2/2 — a DATELESS treated-as-outside skip must not be memoized, or one transient date failure freezes the channel until its newest video changes", probes, dates)
	}
	if len(*found) != 0 {
		t.Fatalf("a window-unverifiable vod must not job on a later sighting either: %v", *found)
	}
}

// TestDecapi_ProbeBudgetIsIndependentOfTheRequestTimeout pins T1-10: the
// classification phase must NOT inherit the DECAPI request deadline.
//
// The old code wrapped the whole channel check in one decapiRequestTimeout
// context and passed it to processResponse, so the player probe and the §9
// date fetch shared whatever was left of 15 s after DECAPI answered. The
// fixture's server is fast; it does not need to burn 14 s to kill that
// mutant, because a probe running under the REQUEST context sees a deadline
// of at most decapiRequestTimeout (15 s) no matter how quick the answer was,
// and the assertion below demands 30 s.
//
// Mutants this fails on:
//   - `return dm.processResponse(ctx, …)` with the request context: deadline ~15 s.
//   - no probe context at all: the cycle context here carries no deadline.
//   - `context.WithTimeout(context.Background(), decapiProbeBudget)`: the
//     cancel-propagation assertion fires (a stopped monitor must cancel an
//     in-flight probe).
func TestDecapi_ProbeBudgetIsIndependentOfTheRequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond) // DECAPI latency, in miniature
		fmt.Fprint(w, decapiBody("vidDecBud06", "budget probe"))
	}))
	t.Cleanup(srv.Close)

	origURL := decapiLatestVideoURL
	decapiLatestVideoURL = srv.URL + "?id=%s"
	t.Cleanup(func() { decapiLatestVideoURL = origURL })

	cycleCtx, cancelCycle := context.WithCancel(context.Background())
	t.Cleanup(cancelCycle)

	var (
		budget             time.Duration
		hadDeadline        bool
		cancelledWithCycle bool
	)
	db := newTestDB(t)
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		var dl time.Time
		dl, hadDeadline = ctx.Deadline()
		budget = time.Until(dl)
		// A CHILD of the cycle context, not of context.Background(): stopping
		// the monitor has to cancel a probe that is already in flight.
		cancelCycle()
		cancelledWithCycle = ctx.Err() != nil
		return &VideoProbeResult{StreamStatus: "live", Title: "budget probe"}, nil
	})

	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1"}
	if err := dm.checkChannel(cycleCtx, ch); err != nil {
		t.Fatalf("checkChannel: %v", err)
	}
	if !hadDeadline {
		t.Fatal("the probe context carried no deadline — the probe must run under decapiProbeBudget")
	}
	if budget < 30*time.Second {
		t.Fatalf("probe budget = %v, want >= 30s — the probe is still running under the %v request timeout", budget, decapiRequestTimeout)
	}
	if budget > decapiProbeBudget {
		t.Fatalf("probe budget = %v, want <= %v", budget, decapiProbeBudget)
	}
	if !cancelledWithCycle {
		t.Fatal("cancelling the cycle context did not cancel the probe context — the probe budget must derive from the cycle context, not context.Background()")
	}
}

// TestDecapi_TerminalMemoSkipsTheUnchangedNewestVideo pins T2-12's first
// case: a channel whose newest video is a processed, terminal VOD must not be
// re-probed every cycle.
//
// Mutant: dropping the memo check restores two probes — ~240 anonymous player
// calls/hour/channel for an answer that cannot change.
func TestDecapi_TerminalMemoSkipsTheUnchangedNewestVideo(t *testing.T) {
	db := newTestDB(t)
	probes := 0
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		probes++
		return &VideoProbeResult{StreamStatus: "vod", Title: "finished vod"}, nil
	})

	// include_non_live_content is off, so the vod arm skips AND writes the
	// history row that makes the next sighting a re-probe.
	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1"}
	body := decapiBody("vidDecMem07", "finished vod")
	for cycle := range 2 {
		if err := dm.processResponse(context.Background(), body, ch); err != nil {
			t.Fatalf("cycle %d: processResponse: %v", cycle, err)
		}
	}
	if probes != 1 {
		t.Fatalf("probes = %d, want 1 — the second cycle must skip a processed terminal VOD it already classified", probes)
	}
}

// TestDecapi_TerminalMemoProbesANewVideoID pins the reset half: publishing
// something new must re-open the channel immediately.
//
// Mutant: memoizing per CHANNEL without comparing the video ID blinds the
// channel forever — the headline monitor bug through a third door.
func TestDecapi_TerminalMemoProbesANewVideoID(t *testing.T) {
	db := newTestDB(t)
	var probed []string
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		probed = append(probed, videoID)
		return &VideoProbeResult{StreamStatus: "vod", Title: "finished vod"}, nil
	})

	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1"}
	if err := dm.processResponse(context.Background(), decapiBody("vidDecOld08", "old vod"), ch); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if err := dm.processResponse(context.Background(), decapiBody("vidDecNew09", "new vod"), ch); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if len(probed) != 2 || probed[0] != "vidDecOld08" || probed[1] != "vidDecNew09" {
		t.Fatalf("probed = %v, want [vidDecOld08 vidDecNew09] — a new newest video must reset the memo", probed)
	}
}

// TestDecapi_TerminalMemoDoesNotLatchOnUpcoming pins the terminal predicate:
// an upcoming stream's classification changes (upcoming -> live -> vod), so
// memoizing it would mean never noticing it went live.
//
// Mutant: treating any classification as terminal probes once and stops.
func TestDecapi_TerminalMemoDoesNotLatchOnUpcoming(t *testing.T) {
	db := newTestDB(t)
	probes := 0
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		probes++
		return &VideoProbeResult{StreamStatus: "upcoming", Title: "premiere"}, nil
	})
	recordDecapiVideoFound(dm)

	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1"}
	body := decapiBody("vidDecUpc10", "premiere")
	for cycle := range 2 {
		if err := dm.processResponse(context.Background(), body, ch); err != nil {
			t.Fatalf("cycle %d: processResponse: %v", cycle, err)
		}
	}
	if probes != 2 {
		t.Fatalf("probes = %d, want 2 — an upcoming classification is not terminal", probes)
	}
}

// TestDecapi_TerminalMemoReleasesWhenHistoryIsCleared pins the third conjunct.
// Clearing an orphaned history row is the documented way to put a video back
// in play (internal/database/database_extras.go:48-56), so the memo must be
// gated on a LIVE HasProcessed read, never on a remembered one.
//
// Mutant: caching the processed flag alongside the memo makes the operator's
// remedy silently do nothing.
func TestDecapi_TerminalMemoReleasesWhenHistoryIsCleared(t *testing.T) {
	db := newTestDB(t)
	probes := 0
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		probes++
		return &VideoProbeResult{StreamStatus: "vod", Title: "finished vod"}, nil
	})

	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1"}
	body := decapiBody("vidDecClr11", "finished vod")
	if err := dm.processResponse(context.Background(), body, ch); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if n, err := db.DeleteHistoryEntries([]string{"vidDecClr11"}); err != nil || n != 1 {
		t.Fatalf("DeleteHistoryEntries = (%d, %v), want (1, nil)", n, err)
	}
	if err := dm.processResponse(context.Background(), body, ch); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if probes != 2 {
		t.Fatalf("probes = %d, want 2 — clearing the history row must re-open the video on the next cycle", probes)
	}
}

// TestDecapi_TerminalMemoCoversTheOutOfWindowArm pins T2-12's remaining hole.
// The memo's gate used to require HasProcessed, and the §13 out-of-window skip
// writes no history row: with include_non_live_content=true, a terminal VOD
// older than the archive window left `reprobe` false forever, so every 15 s
// cycle re-ran BOTH anonymous requests — the classifying player probe and the
// §9 date fetch — on exactly the dormant channels the memo exists to quiet.
//
// Memoizing that skip is sound because it is monotone: the cutoff only moves
// forward, so a video already behind it can never come back inside on its own.
// The two things that CAN change the answer are a new video (a different ID is
// a miss) and a wider window, which is why the window the decision was made
// against is memoized alongside it.
//
// Mutants this fails on:
//   - dropping the outsideWindow arm (memo gated on reprobe alone): cycle 2
//     probes and date-fetches again, every cycle, forever.
//   - ignoring windowDays: cycle 3 keeps skipping after the operator widened
//     the window, and a VOD that is now in scope is never archived.
func TestDecapi_TerminalMemoCoversTheOutOfWindowArm(t *testing.T) {
	db := newTestDB(t)
	probes, dates := 0, 0
	// Dateless, like every production status probe (§9) — so the date fetch
	// below is the second request the memo has to suppress.
	dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
		probes++
		return &VideoProbeResult{StreamStatus: "vod", Title: "old vod"}, nil
	})
	published := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	dm.ProbeDate = func(ctx context.Context, videoID string) (string, string, error) {
		dates++
		return published, "day", nil
	}
	found := recordDecapiVideoFound(dm)

	// include_non_live_content keeps the vod arm from writing a history row,
	// which is what leaves this sighting outside the old memo's reach.
	ch := &config.ChannelConfig{ID: "UC1", Name: "UC1", IncludeNonLiveContent: true}
	body := decapiBody("vidDecWin12", "old vod")

	if err := dm.processResponse(context.Background(), body, ch); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if probes != 1 || dates != 1 {
		t.Fatalf("cycle 1: probes=%d dates=%d, want 1/1 — the first sighting has to classify and date the video", probes, dates)
	}
	if len(*found) != 0 {
		t.Fatalf("a 30-day-old VOD was jobbed against the 3-day default window: %v", *found)
	}

	if err := dm.processResponse(context.Background(), body, ch); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if probes != 1 || dates != 1 {
		t.Fatalf("cycle 2: probes=%d dates=%d, want 1/1 — an unchanged terminal VOD already judged outside the window must cost no request at all", probes, dates)
	}

	// The operator widens the window. The memo recorded the window it judged
	// against, so this is a miss and the video is re-probed exactly once.
	widened := 90
	ch.ArchiveWindowDays = &widened
	if err := dm.processResponse(context.Background(), body, ch); err != nil {
		t.Fatalf("cycle 3: %v", err)
	}
	if probes != 2 || dates != 2 {
		t.Fatalf("cycle 3: probes=%d dates=%d, want 2/2 — widening the archive window must re-open the video", probes, dates)
	}
	if len(*found) != 1 || (*found)[0].videoID != "vidDecWin12" {
		t.Fatalf("found = %v, want [vidDecWin12] — 30 days is inside a 90-day window", *found)
	}
}
