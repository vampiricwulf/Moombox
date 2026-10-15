package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// R-05: DECAPI reports a channel's newest video, and when that is an ended
// broadcast whose ANDROID_VR answer has no formats — age-gated, the
// recording not available, formats withheld — the probe makes it post_live.
// The terminal memo recorded post_live raw, which decapiTerminalStatus never
// calls terminal, so the same ended video was probed every 15 s for good;
// on a channel that archives VODs, with the video outside the window, each
// cycle paid the WEB date fetch as well. A settled end — strong, or weak and
// confirmed by the end check — is the vod the feed stores it as, and settles
// after one probe exactly as a vod newest video does, for every reason the
// memo trusts a vod.
//
// Mutants: post_live recorded raw (decapiMemoStatus not applied in
// recordTerminalMemo — 4 probes in every case); the include_non_live_content
// note keyed on the raw status (the VODs-off cases probe 4 times); the date
// fetch asked after the end check (the dateless confirmed case's date
// fetches).
func TestDecapi_ASettledEndedNewestVideoIsMemoized(t *testing.T) {
	outside := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	strongEnd := func(verdict string) VideoProbeResult {
		return VideoProbeResult{StreamStatus: "upcoming", PlayabilityError: verdict, BroadcastEnded: true}
	}
	for _, tc := range []struct {
		name           string
		probe          VideoProbeResult
		check          *EndCheck // nil: no end check wired
		includeNonLive bool
		wantProbes     int
		wantDates      int
		wantChecks     int
	}{
		{"strong end, age gate, VODs off", strongEnd("age_restricted"), nil, false, 1, 0, 0},
		{"strong end, no recording, VODs off", strongEnd("unknown"), nil, false, 1, 0, 0},
		{"strong end, formats withheld, VODs off", strongEnd("ok"), nil, false, 1, 0, 0},
		{"strong end outside the window, VODs on", strongEnd("unknown"), nil, true, 1, 1, 0},
		{"weak end confirmed, VODs off", weakEnd(), &EndCheck{Ended: true}, false, 1, 0, 1},
		{"weak end confirmed outside the window, VODs on", weakEnd(),
			&EndCheck{Ended: true, PublishedAt: outside, PublishedPrecision: "started"}, true, 1, 0, 1},
		// The WEB answer had no date: the window is unverifiable, which is
		// never memoized (a dateless verdict), so each sighting asks again —
		// but the end check WAS the date fetch, so it is not asked twice.
		{"weak end confirmed without a date, VODs on", weakEnd(), &EndCheck{Ended: true}, true, 4, 0, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			probes, checks, dates := 0, 0, 0
			answer := tc.probe
			dm := newTestDecapiMonitor(t, db, countingProbe(&probes, &answer))
			dm.ProbeDate = func(context.Context, string) (string, string, error) {
				dates++
				return outside, "started", nil
			}
			if tc.check != nil {
				dm.ConfirmEnd = endCheckReturning(&checks, *tc.check, nil)
			}
			found := recordDecapiVideoFound(dm)
			ch := &config.ChannelConfig{ID: "UC1", Name: "UC1", IncludeNonLiveContent: tc.includeNonLive}
			body := decapiBody("vidEndedA01", "ended stream")
			for range 4 {
				_ = dm.processResponse(context.Background(), body, ch)
			}
			if len(*found) != 0 {
				t.Fatalf("an ended broadcast was jobbed: %v", *found)
			}
			if probes != tc.wantProbes || dates != tc.wantDates || checks != tc.wantChecks {
				t.Errorf("over 4 sightings: probes %d, date fetches %d, end checks %d; want %d, %d, %d",
					probes, dates, checks, tc.wantProbes, tc.wantDates, tc.wantChecks)
			}
		})
	}
}

// An unconfirmed end is neither settled nor re-asked every 15 s: the memo
// holds it for the re-check backoff — an hour, six, then daily — and a
// re-check that finds the stream on air announces it as a broadcast. An
// errored re-check answers nothing, so the next sighting asks again and the
// backoff resumes where it was. The answer never makes a job, VODs archived
// or not, and never pays the date fetch.
//
// Mutants: terminalMemoHit ignoring recheckAt (VODs on: a probe every
// sighting; VODs off: the nonLiveOff memo holds the vod-recorded answer for
// good, and the +1 h re-check never comes); the recheckAt rule answered
// after the terminal reasons instead of before them (VODs off, the same);
// recordTerminalMemo not scheduling a re-check (as ignoring it); the
// backoff flat at an hour; an errored probe resetting the step (the next
// wait an hour, not a day).
func TestDecapi_AnUnconfirmedEndFollowsTheReCheckBackoff(t *testing.T) {
	for _, includeNonLive := range []bool{false, true} {
		name := "VODs off"
		if includeNonLive {
			name = "VODs on"
		}
		t.Run(name, func(t *testing.T) {
			db := newTestDB(t)
			t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
			clock := t0
			probes, dates := 0, 0
			answer := weakEnd()
			fail := false
			dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
				probes++
				if fail {
					return nil, errors.New("player: transport error")
				}
				res := answer
				return &res, nil
			})
			dm.now = func() time.Time { return clock }
			dm.ProbeDate = func(context.Context, string) (string, string, error) {
				dates++
				return "", "", nil
			}
			found := recordDecapiVideoFound(dm)
			ch := &config.ChannelConfig{ID: "UC1", Name: "UC1", IncludeNonLiveContent: includeNonLive}
			body := decapiBody("vidWeakEnd01", "ended stream")
			sight := func(at time.Duration, wantProbes int) {
				t.Helper()
				clock = t0.Add(at)
				_ = dm.processResponse(context.Background(), body, ch)
				if probes != wantProbes {
					t.Fatalf("at +%v: probes %d, want %d", at, probes, wantProbes)
				}
			}

			sight(0, 1)
			sight(15*time.Second, 1)
			sight(59*time.Minute, 1)
			sight(time.Hour, 2) // the first re-check: next in 6 h
			sight(time.Hour+15*time.Second, 2)
			fail = true
			sight(7*time.Hour, 3) // due, but the probe fails
			fail = false
			sight(7*time.Hour+15*time.Second, 4) // asked again at once; the step was kept: next in 24 h
			sight(7*time.Hour+30*time.Minute, 4)
			sight(31*time.Hour, 4)
			sight(31*time.Hour+15*time.Second, 5)
			if len(*found) != 0 || dates != 0 {
				t.Fatalf("an unconfirmed end: found %v, date fetches %d; want no job and no date fetch", *found, dates)
			}

			// The daily re-check finds the stream on air.
			answer = VideoProbeResult{StreamStatus: "upcoming", PlayabilityError: "ok"}
			sight(55*time.Hour+15*time.Second, 6)
			if len(*found) != 1 || (*found)[0].d != DispositionBroadcast {
				t.Fatalf("the re-check that found it on air: found %v, want one broadcast job", *found)
			}
		})
	}
}
