package worker

import (
	"encoding/json"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// infoArgsLogger records the Info lines alone, each as its message followed by
// its args: what an operator at the default log level sees.
type infoArgsLogger struct {
	mu    sync.Mutex
	lines [][]any
}

func (l *infoArgsLogger) Debug(string, ...any) {}
func (l *infoArgsLogger) Info(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, append([]any{msg}, args...))
}
func (l *infoArgsLogger) Warn(string, ...any)  {}
func (l *infoArgsLogger) Error(string, ...any) {}

func (l *infoArgsLogger) take() [][]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.lines
	l.lines = nil
	return out
}

// TestLatencyBackfillLogsEachStateOnce: the walk logs one Info line when it
// enters a state (owner ruling RV-20: begun with how many are left, each
// pause with its reason and a rate limit's end, resumed, caught up with
// what the run filled and how many YouTube would not say, switched off and
// on), never again while the state lasts, and signals a change each time,
// a rate-limit pause whose end moved included.
//
// Mutants: enter logging on every step (a second line for a state that
// lasts); enter not comparing the pause reason (the capture-to-rate-limit
// pause logs nothing); the run flag not kept between a pause and its end
// (resumed logged as started); the run counters not counting unstated
// answers; the moved end not signalled.
func TestLatencyBackfillLogsEachStateOnce(t *testing.T) {
	t.Parallel()
	h := newBackfillHarness(t, openTestDB(t))
	log := &infoArgsLogger{}
	h.b.logger = log
	changes := 0
	h.b.changed = func() { changes++ }
	for _, id := range []string{"aaa", "bbb", "ccc"} {
		h.add(id, database.StatusFinished)
	}
	h.answers["aaa"] = answered("MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_LOW")
	h.answers["bbb"] = answered("MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_NORMAL")
	h.answers["ccc"] = answered("")

	expect := func(step string, want ...[]any) {
		t.Helper()
		got := log.take()
		if len(got) == 0 && len(want) == 0 {
			return
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: logged %v, want %v", step, got, want)
		}
	}

	h.online = false
	h.step(latencyBackfillPause)
	h.step(latencyBackfillPause)
	expect("offline", []any{"latency backfill: paused while the device is offline"})
	h.online = true

	h.step(latencyBackfillPause) // aaa
	h.step(latencyBackfillPause) // bbb
	expect("running", []any{"latency backfill: started, asking YouTube about one recording a minute", "toAsk", 3})

	h.starting = true
	h.step(latencyBackfillPause)
	h.step(latencyBackfillPause)
	expect("capture", []any{"latency backfill: paused while a capture is live"})
	h.starting = false

	h.held, h.schedUntil = true, h.now.Add(30*time.Minute)
	h.step(latencyBackfillPause)
	h.step(latencyBackfillPause)
	expect("rate limit", []any{"latency backfill: paused for a YouTube rate limit", "until", "2026-11-27T12:30:00Z"})
	before := changes
	h.schedUntil = h.now.Add(45 * time.Minute)
	h.step(latencyBackfillPause)
	expect("rate limit extended")
	if changes != before+1 {
		t.Errorf("a moved rate-limit end signalled %d changes, want 1", changes-before)
	}
	h.held = false

	h.step(latencyBackfillPause) // ccc
	expect("resumed", []any{"latency backfill: resumed", "toAsk", 1})
	h.step(latencyBackfillIdle)
	h.step(latencyBackfillIdle)
	expect("caught up", []any{"latency backfill: caught up", "filled", 3, "unstated", 1, "gaveUp", 0, "left", 0})

	h.switchedOff = true
	h.step(latencyBackfillPause)
	h.step(latencyBackfillPause)
	expect("off", []any{"latency backfill: switched off (monitors.latency_backfill = false); no recording is asked about until it is switched on"})

	h.switchedOff = false
	h.add("ddd", database.StatusFinished)
	h.answers["ddd"] = answered("")
	h.step(latencyBackfillPause)
	expect("on", []any{"latency backfill: switched on"},
		[]any{"latency backfill: started, asking YouTube about one recording a minute", "toAsk", 1})

	// waiting→offline, →running, →capture, →rate limit, its end moved,
	// →running, →caught up, →off, →running.
	if changes != 9 {
		t.Errorf("%d changes signalled, want 9", changes)
	}
}

// TestLatencyBackfillStatus: the status is the walk's last phase (waiting
// before its first step), off whenever the key is false whatever the walk
// last did, and the counts are the database's, read on each call.
//
// Mutants: the config not read (off shows the walk's paused phase); the
// walk's phase not read (paused shows waiting); the counts not read; the
// worker's walk not signalling LatencyBackfillChanged.
func TestLatencyBackfillStatus(t *testing.T) {
	t.Parallel()
	w, db := testWorkerSetup(t)
	for _, id := range []string{"aaa", "bbb"} {
		if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube", Status: database.StatusFinished}); err != nil {
			t.Fatal(err)
		}
	}
	db.UpdateJobFields("aaa", map[string]any{"latency_mode": database.LatencyModeLow})

	if got, want := w.LatencyBackfillStatus(), (LatencyBackfillStatus{State: LatencyBackfillWaiting, Done: 1, Total: 2}); got != want {
		t.Errorf("before Start: %+v, want %+v", got, want)
	}
	b := w.newLatencyBackfill()
	w.latencyWalk.Store(b)
	until := time.Date(2026, 11, 27, 14, 5, 0, 0, time.UTC)
	b.enter(latencyPhase{state: LatencyBackfillPaused, pause: LatencyPauseRateLimit, until: until})
	want := LatencyBackfillStatus{State: LatencyBackfillPaused, Pause: LatencyPauseRateLimit, Until: until, Done: 1, Total: 2}
	if got := w.LatencyBackfillStatus(); got != want {
		t.Errorf("paused: %+v, want %+v", got, want)
	}
	f := false
	w.cfg.Monitors.LatencyBackfill = &f
	if got, want := w.LatencyBackfillStatus(), (LatencyBackfillStatus{State: LatencyBackfillOff, Done: 1, Total: 2}); got != want {
		t.Errorf("switched off: %+v, want %+v", got, want)
	}
	select {
	case <-w.LatencyBackfillChanged():
	default:
		t.Error("the phase change was not signalled to LatencyBackfillChanged")
	}
}

// TestLatencyBackfillLineParity is the Go half of
// web/tests/fixtures/latency-backfill-line-parity.json; the JS half is in
// web/tests/settings-latency-backfill.test.mjs. Each row's status is
// decoded as the wire carries it and worded in the row's zone, so a row with
// a non-zero offset pins the rate-limit end's wall-clock time on both sides.
func TestLatencyBackfillLineParity(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../web/tests/fixtures/latency-backfill-line-parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Cases []struct {
			Name             string          `json:"name"`
			Status           json.RawMessage `json:"status"`
			UTCOffsetMinutes int             `json:"utcOffsetMinutes"`
			Line             string          `json:"line"`
			JSOnly           bool            `json:"jsOnly"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range fx.Cases {
		if c.JSOnly {
			continue
		}
		var s LatencyBackfillStatus
		if err := json.Unmarshal(c.Status, &s); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if got := LatencyBackfillLine(s, time.FixedZone("", c.UTCOffsetMinutes*60)); got != c.Line {
			t.Errorf("%s: %q, want %q", c.Name, got, c.Line)
		}
	}
}
