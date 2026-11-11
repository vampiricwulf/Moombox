package engine

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// edgeStatsLogger keeps the arguments of the DASH loop's exit line.
type edgeStatsLogger struct {
	testLogger
	mu    sync.Mutex
	lines [][]any
}

func (l *edgeStatsLogger) Debug(msg string, args ...any) {
	if msg != dashEdgeStatsMsg {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, args)
}

// arg reads one key of the single exit line.
func (l *edgeStatsLogger) arg(t *testing.T, key string) any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.lines) != 1 {
		t.Fatalf("exit lines logged = %d, want exactly 1", len(l.lines))
	}
	args := l.lines[0]
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == key {
			return args[i+1]
		}
	}
	t.Fatalf("exit line has no %q: %v", key, args)
	return nil
}

// TestDashEdgeStatsFold pins the arithmetic of the at-edge measurement: the
// count, the per-status tally in status order, and the smallest, mean and
// largest latency.
func TestDashEdgeStatsFold(t *testing.T) {
	var s dashEdgeStats
	if got := s.statuses(); got != "" {
		t.Errorf("statuses of nothing = %q, want empty", got)
	}
	if got := s.avgLatency(); got != 0 {
		t.Errorf("avgLatency of nothing = %v, want 0", got)
	}
	s.noteFailure(404, 30*time.Millisecond)
	s.noteFailure(0, 10*time.Millisecond)
	s.noteFailure(404, 50*time.Millisecond)
	if s.failures != 3 {
		t.Errorf("failures = %d, want 3", s.failures)
	}
	if got, want := s.statuses(), "0:1 404:2"; got != want {
		t.Errorf("statuses = %q, want %q", got, want)
	}
	if s.latencyMin != 10*time.Millisecond || s.latencyMax != 50*time.Millisecond {
		t.Errorf("latency min/max = %v/%v, want 10ms/50ms", s.latencyMin, s.latencyMax)
	}
	if got := s.avgLatency(); got != 30*time.Millisecond {
		t.Errorf("avgLatency = %v, want 30ms", got)
	}
}

// TestDashLoopLogsEdgeStatsOnExit drives the real loop to the edge of an
// ended stream: sequences 0..head are served and written, head+1 answers 503
// every time until the status check ends the stream. The loop's one Debug
// line on exit must count the six written segments and exactly the failed
// requests memGVS answered at head+1, all under status 503.
//
// Mutants: counting a failure below the head (the hole at sequence 2 is
// asked twice, so the failure count passes the attempts at head+1); not
// counting written segments; logging per failure instead of once.
func TestDashLoopLogsEdgeStatsOnExit(t *testing.T) {
	const head = 5
	g := serveGVS(t, constHead(head), func(r *http.Request, seq, attempt int) gvsAnswer {
		switch {
		case seq == 2 && attempt == 1:
			return gvsUnavailable // one transient failure below the head: not at the edge
		case seq <= head:
			return gvsServe
		}
		return gvsUnavailable
	})

	synctest.Test(t, func(t *testing.T) {
		log := &edgeStatsLogger{}
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:           gvsBase("edgestats"),
			OutputFile:        filepath.Join(t.TempDir(), "video_stream"),
			InitInlineAtStart: true,
			Logger:            log,
			CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
		})
		if err := d.Start(t.Context()); err != nil {
			t.Fatalf("Start = %v, want nil (an ended stream)", err)
		}

		if got := g.attemptsFor(2); got != 2 {
			t.Fatalf("sequence 2 asked %d times, want 2 (the scenario needs one failure below the head)", got)
		}
		atEdge := g.attemptsFor(head + 1)
		if atEdge < 2 {
			t.Fatalf("head+1 asked %d times, want several (the scenario needs at-edge retries)", atEdge)
		}
		if got := log.arg(t, "segmentsWritten"); got != head+1 {
			t.Errorf("segmentsWritten = %v, want %d", got, head+1)
		}
		if got := log.arg(t, "atEdgeFailures"); got != atEdge {
			t.Errorf("atEdgeFailures = %v, want %d (the requests answered at head+1)", got, atEdge)
		}
		if got, want := log.arg(t, "statuses"), "503:"+strconv.Itoa(atEdge); got != want {
			t.Errorf("statuses = %v, want %q", got, want)
		}
		for _, key := range []string{"latencyMin", "latencyAvg", "latencyMax"} {
			if _, ok := log.arg(t, key).(time.Duration); !ok {
				t.Errorf("%s is %T, want a time.Duration", key, log.arg(t, key))
			}
		}
	})
}
