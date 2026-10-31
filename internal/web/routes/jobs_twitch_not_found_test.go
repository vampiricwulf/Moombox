package routes

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestTwitchAddRefusesALoginThatDoesNotExist is the wave's third offline
// test, the add-route half (G53): a channel add whose metadata lookup Twitch
// answers with "no such user" is a 404 with no row behind it. It used to be a
// 201 and a tw_manual_ job that waited at "Waiting for stream..." forever —
// yt-dlp refuses the same input at once with "<channel> does not exist".
//
// A lookup that FAILED is not that answer, and still falls through to the
// waiting manual job as it always has: an outage at add time is no verdict on
// the login, and the wait gives up on its own if Twitch later says so.
//
// Mutants: the ErrTwitchChannelNotFound arm deleted (201, and a manual row);
// the arm widened to any lookup error (the outage row is refused too).
func TestTwitchAddRefusesALoginThatDoesNotExist(t *testing.T) {
	f := newJobsFixture(t)

	f.tw.streamErr = fmt.Errorf("lookup: %w", ErrTwitchChannelNotFound)
	rec := doRequest(t, f.router, "POST", "/api/jobs", map[string]any{"platform": "twitch", "videoId": "nosuchstreamer"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("add of a login that does not exist: want 404, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Twitch channel not found") {
		t.Errorf("404 body = %s, want it to say \"Twitch channel not found\"", rec.Body.String())
	}
	if rows, err := f.db.ManualTwitchJobs("nosuchstreamer"); err != nil || len(rows) != 0 {
		t.Errorf("manual rows for the refused login = %d (err %v), want none", len(rows), err)
	}

	f.tw.streamErr = errors.New("gql request (StreamMetadata+ComscoreStreamingQuery): i/o timeout")
	rec = doRequest(t, f.router, "POST", "/api/jobs", map[string]any{"platform": "twitch", "videoId": "realstreamer"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add whose lookup failed: want 201 and a waiting job, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if rows, err := f.db.ManualTwitchJobs("realstreamer"); err != nil || len(rows) != 1 {
		t.Errorf("manual rows for the add whose lookup failed = %d (err %v), want 1", len(rows), err)
	}
}
