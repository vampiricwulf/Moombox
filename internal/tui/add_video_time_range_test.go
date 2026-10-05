package tui

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// postedAddBody runs the real addVideoCmd against a stand-in /api/jobs and
// returns the raw body it posted.
func postedAddBody(t *testing.T, start, end string) []byte {
	t.Helper()
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	a := NewApp()
	a.SetWebPort(func() int { return port })
	a.addVideo.platform = "youtube"
	a.addVideo.startTimeInput = start
	a.addVideo.endTimeInput = end
	if msg, ok := a.addVideoCmd("dQw4w9WgXcQ")().(addVideoResultMsg); !ok || msg.Feedback != "Added to queue" {
		t.Fatalf("the add did not succeed: %#v", msg)
	}
	return got
}

// POST /api/jobs decodes startTime/endTime as *float64. The TUI posted the
// text inputs verbatim — "1:30" as a JSON string — so encoding/json refused
// the whole body and every Add Video with a range came back "invalid request
// body". The range must arrive as seconds, decodable into the route's own
// field types.
//
// Mutant: posting GetStartTime()'s raw text again, or dropping either field.
func TestAddVideoPostsTheRangeAsSeconds(t *testing.T) {
	raw := postedAddBody(t, "1:30", "1:02:03.5")
	var body struct {
		StartTime *float64 `json:"startTime"`
		EndTime   *float64 `json:"endTime"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("the route's decode would refuse this body (%v): %s", err, raw)
	}
	if body.StartTime == nil || *body.StartTime != 90 {
		t.Errorf("startTime = %v, want 90: %s", body.StartTime, raw)
	}
	if body.EndTime == nil || *body.EndTime != 3723.5 {
		t.Errorf("endTime = %v, want 3723.5: %s", body.EndTime, raw)
	}
}

// A blank field is no field, and a zero start is no start — left off exactly
// as the dashboard leaves it off.
//
// Mutant: dropping TimeRange's `s > 0`.
func TestAddVideoLeavesAnEmptyRangeOff(t *testing.T) {
	for _, tc := range []struct{ start, end string }{{"", ""}, {"0", ""}, {"0:00", "10"}} {
		var body map[string]any
		raw := postedAddBody(t, tc.start, tc.end)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("%q-%q: %v", tc.start, tc.end, err)
		}
		if _, ok := body["startTime"]; ok {
			t.Errorf("%q-%q: a blank or zero start was posted: %s", tc.start, tc.end, raw)
		}
		if _, ok := body["endTime"]; ok != (tc.end != "") {
			t.Errorf("%q-%q: endTime present = %v, want %v: %s", tc.start, tc.end, ok, tc.end != "", raw)
		}
	}
}

// The timestamps step refuses what the server would: a negative start, and
// an end at or before the start — a blank start being the video's beginning,
// so an end of 0 is an empty range too, not a pass.
//
// Mutant: dropping the `s < 0` check, or gating the end check on a typed
// start again.
func TestAddVideoTimestampsStepRefusesEmptyRanges(t *testing.T) {
	for _, tc := range []struct{ start, end, wantErr string }{
		{"-5", "", "Start time cannot be negative"},
		{"", "0", "End time must be after start time"},
		{"30", "0:30", "End time must be after start time"},
		{"1:NaN", "", "Invalid start time format"},
		{"", "10", ""},
		{"0:05", "1:59.5", ""},
	} {
		m := NewAddVideoModel()
		m.visible = true
		m.step = AddStepTimestamps
		m.startTimeInput, m.endTimeInput = tc.start, tc.end
		m.handleTimestampsStep(keyEnter)
		if m.errorMsg != tc.wantErr {
			t.Errorf("%q-%q: error %q, want %q", tc.start, tc.end, m.errorMsg, tc.wantErr)
		}
		if advanced := m.step == AddStepConfirm; advanced != (tc.wantErr == "") {
			t.Errorf("%q-%q: advanced to confirm = %v, want %v", tc.start, tc.end, advanced, tc.wantErr == "")
		}
	}
}
