package routes

import (
	"encoding/json"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestJobPayloadCarriesTheGapFillState is owner decision PR-R5's API: GET
// /api/jobs/{id} carries a Finished live Twitch job's gap-fill as gapFill
// {state, spans, until, reason} — the field the dashboard and the TUI render
// "Gap-fill pending: N spans, until <time>" and its outcome from — and leaves
// it out for a job that has none.
//
// Mutant: GapFill dropped from enrichJob (gapFill absent for tw_gf).
func TestJobPayloadCarriesTheGapFillState(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "tw_gf", func(j *database.Job) { j.Platform = "twitch" })
	f.addJob(t, "tw_none", func(j *database.Job) { j.Platform = "twitch" })
	plan := `{"jobId":"tw_gf","deadline":"2026-10-02T13:00:00Z","spans":[{"fromMs":1,"toMs":2,"kind":"late_start","afterPart":-1},{"fromMs":3,"toMs":4,"kind":"lost","afterPart":0}],"lastReason":"the archive VOD is still recording"}`
	if err := f.db.SaveGapFill(database.GapFill{JobID: "tw_gf", Plan: plan, NextCheckAt: "2026-10-01T13:10:00Z"}); err != nil {
		t.Fatal(err)
	}

	var got struct {
		GapFill *struct {
			State  string `json:"state"`
			Spans  int    `json:"spans"`
			Until  string `json:"until"`
			Reason string `json:"reason"`
		} `json:"gapFill"`
	}
	body := f.getJSON(t, "/api/jobs/tw_gf", &got)
	if g := got.GapFill; g == nil || g.State != "pending" || g.Spans != 2 || g.Until != "2026-10-02T13:00:00Z" || g.Reason != "the archive VOD is still recording" {
		t.Errorf("gapFill = %+v; body = %s", got.GapFill, body)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(f.getJSON(t, "/api/jobs/tw_none", nil)), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["gapFill"]; ok {
		t.Errorf("gapFill = %s for a job without one, want it absent", raw["gapFill"])
	}
}
