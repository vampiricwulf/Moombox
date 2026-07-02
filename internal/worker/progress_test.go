package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/engine"
)

// nopProgressLogger satisfies the worker package's anonymous logger
// interface for tests.
type nopProgressLogger struct{}

func (nopProgressLogger) Debug(msg string, args ...any) {}
func (nopProgressLogger) Info(msg string, args ...any)  {}
func (nopProgressLogger) Warn(msg string, args ...any)  {}
func (nopProgressLogger) Error(msg string, args ...any) {}

// newTestProgressTracker constructs a ProgressTracker with no DB hooked
// up — the tests below exercise the pure-logic format helpers without
// triggering the persistence path.
func newTestProgressTracker() *ProgressTracker {
	return &ProgressTracker{
		jobID:     "test-job",
		logger:    nopProgressLogger{},
		startTime: time.Now().Add(-30 * time.Second),
	}
}

func TestActivityMessage(t *testing.T) {
	elapsed := 80 * time.Second // FormatDurationHuman -> "1m 20s"
	cases := []struct {
		a    engine.DownloadActivity
		want string
	}{
		{engine.ActivityVerifyingEnd, "Verifying stream ended... (1m 20s)"},
		{engine.ActivityReconnecting, "Connection lost - reconnecting... (1m 20s)"},
		{engine.ActivityRateLimited, "Rate-limited - backing off... (1m 20s)"},
		{engine.ActivityFindingFirstSegment, "Waiting for first segment... (1m 20s)"},
		{engine.ActivityRetrying, "Segment fetch failing - retrying... (1m 20s)"},
		{engine.ActivityNone, ""},
	}
	for _, c := range cases {
		if got := activityMessage(c.a, elapsed); got != c.want {
			t.Errorf("activityMessage(%v) = %q, want %q", c.a, got, c.want)
		}
	}
}

// TestActivityMessagesCarryWaitMarker pins the cross-layer contract the
// TUI's task rows rely on (internal/tui isWaitProgress): every rendered
// activity message carries the "... (" tail that distinguishes a wait from
// a segment counter — and no counter format may carry it.
func TestActivityMessagesCarryWaitMarker(t *testing.T) {
	for _, a := range []engine.DownloadActivity{
		engine.ActivityVerifyingEnd,
		engine.ActivityReconnecting,
		engine.ActivityRateLimited,
		engine.ActivityFindingFirstSegment,
		engine.ActivityRetrying,
	} {
		if msg := activityMessage(a, time.Second); !strings.Contains(msg, "... (") {
			t.Errorf("activityMessage(%v) = %q lacks the %q wait marker", a, msg, "... (")
		}
	}

	// Counter shapes must never carry the marker.
	pt := newTestProgressTracker()
	pt.audioSeq = 100
	pt.audioTotal = 200
	pt.videoSeq = 50
	pt.videoTotal = 200
	pt.chatCount = 7
	if s := pt.buildProgressString(); strings.Contains(s, "... (") {
		t.Errorf("segment counter %q carries the wait marker", s)
	}
	pt2 := newTestProgressTracker()
	pt2.vodTotalBytes = 1 << 30
	pt2.vodPercent = 25.5
	if s := pt2.buildProgressString(); strings.Contains(s, "... (") {
		t.Errorf("VOD counter %q carries the wait marker", s)
	}
}

func TestDominantActivity(t *testing.T) {
	base := time.Now()

	t.Run("recent segment shows the counter (None)", func(t *testing.T) {
		pt := newTestProgressTracker()
		pt.videoActivity = engine.ActivityReconnecting
		pt.videoActivityStart = base.Add(-10 * time.Second)
		pt.lastSegmentAt = base.Add(-200 * time.Millisecond) // a stream just delivered
		if a, _ := pt.dominantActivity(base); a != engine.ActivityNone {
			t.Errorf("recent segment: got %v, want ActivityNone", a)
		}
	})

	t.Run("one stream stalled keeps its own elapsed", func(t *testing.T) {
		pt := newTestProgressTracker()
		pt.videoActivity = engine.ActivityReconnecting
		pt.videoActivityStart = base.Add(-30 * time.Second)
		pt.lastSegmentAt = base.Add(-10 * time.Second) // stale — no recent segment
		a, start := pt.dominantActivity(base)
		if a != engine.ActivityReconnecting || !start.Equal(base.Add(-30*time.Second)) {
			t.Errorf("got (%v, %v), want (Reconnecting, -30s)", a, start)
		}
	})

	t.Run("both stalled: the longest-running wait wins", func(t *testing.T) {
		pt := newTestProgressTracker()
		pt.videoActivity = engine.ActivityVerifyingEnd
		pt.videoActivityStart = base.Add(-5 * time.Second)
		pt.audioActivity = engine.ActivityVerifyingEnd
		pt.audioActivityStart = base.Add(-20 * time.Second) // earlier start = longer wait
		pt.lastSegmentAt = base.Add(-10 * time.Second)
		_, start := pt.dominantActivity(base)
		if !start.Equal(base.Add(-20 * time.Second)) {
			t.Errorf("want the earlier (audio) start, got %v", start)
		}
	})

	t.Run("both idle: None", func(t *testing.T) {
		pt := newTestProgressTracker()
		pt.lastSegmentAt = base.Add(-10 * time.Second)
		if a, _ := pt.dominantActivity(base); a != engine.ActivityNone {
			t.Errorf("got %v, want ActivityNone", a)
		}
	})
}

// TestBuildProgressStringVODChunked covers the "vodTotalBytes > 0"
// branch — the function must return a percentage rather than segment
// counts. Audit reports/worker.md F69.
func TestBuildProgressStringVODChunked(t *testing.T) {
	pt := newTestProgressTracker()
	pt.vodTotalBytes = 1 << 30 // 1 GB
	pt.vodPercent = 25.5

	got := pt.buildProgressString()
	want := "V:25.5%"
	if got != want {
		t.Errorf("VOD chunked: want %q, got %q", want, got)
	}

	pt.chatCount = 42
	got = pt.buildProgressString()
	if !strings.Contains(got, "C: 42") {
		t.Errorf("VOD chunked with chat: should append C: 42, got %q", got)
	}
}

// TestBuildProgressStringSegmentedAV covers the "audio + video segments"
// branch — both totals known.
func TestBuildProgressStringSegmentedAV(t *testing.T) {
	pt := newTestProgressTracker()
	pt.audioSeq = 100
	pt.audioTotal = 200
	pt.videoSeq = 50
	pt.videoTotal = 200

	got := pt.buildProgressString()
	want := "(A: 100/200 V: 50/200)"
	if got != want {
		t.Errorf("segmented A+V: want %q, got %q", want, got)
	}
}

// TestBuildProgressStringSegmentedAVUnknownTotal covers the
// audioTotal=0 / videoTotal=0 branch — segment count without total.
func TestBuildProgressStringSegmentedAVUnknownTotal(t *testing.T) {
	pt := newTestProgressTracker()
	pt.audioSeq = 100 // > 0 to trigger A+V branch
	pt.videoSeq = 50

	got := pt.buildProgressString()
	want := "(A: 100 V: 50)"
	if got != want {
		t.Errorf("segmented A+V no totals: want %q, got %q", want, got)
	}
}

// TestBuildProgressStringChatOnlyFallback covers the "chatCount > 0
// but no video/audio progress yet" branch.
func TestBuildProgressStringChatOnlyFallback(t *testing.T) {
	pt := newTestProgressTracker()
	pt.chatCount = 17
	pt.videoSeq = 5

	got := pt.buildProgressString()
	want := "Seq: 5 C: 17"
	if got != want {
		t.Errorf("chat-only fallback: want %q, got %q", want, got)
	}
}

// TestBuildProgressStringVideoSeqOnly covers the catch-all bottom
// branch — just videoSeq, no audio/chat.
func TestBuildProgressStringVideoSeqOnly(t *testing.T) {
	pt := newTestProgressTracker()
	pt.videoSeq = 1234

	got := pt.buildProgressString()
	want := "Seq: 1234"
	if got != want {
		t.Errorf("seq only: want %q, got %q", want, got)
	}
}

// TestCalculateETAEarlyReturnUnderFiveSeconds verifies the helper
// refuses to estimate before 5s of elapsed time — too noisy to be
// useful for sub-5s downloads.
func TestCalculateETAEarlyReturnUnderFiveSeconds(t *testing.T) {
	pt := newTestProgressTracker()
	pt.startTime = time.Now().Add(-2 * time.Second) // only 2s elapsed
	pt.videoSeq = 10
	pt.videoTotal = 100

	got := pt.calculateETA()
	if got != "" {
		t.Errorf("ETA before 5s: want empty, got %q", got)
	}
}

// TestCalculateETASegmentBased exercises the segments-based ETA
// branch. With 30s elapsed and 50/100 segments, segsPerSec=50/30=1.67,
// remaining=50/1.67=~30s.
func TestCalculateETASegmentBased(t *testing.T) {
	pt := newTestProgressTracker()
	pt.startTime = time.Now().Add(-30 * time.Second)
	pt.videoSeq = 50
	pt.videoTotal = 100

	got := pt.calculateETA()
	// Format depends on duration buckets: <60s = "Ns".
	if !strings.HasSuffix(got, "s") {
		t.Errorf("ETA: want sub-minute format like 'Ns', got %q", got)
	}
}

// TestCalculateETAVODBytesBased exercises the VOD bytes-based ETA
// branch. bytesTotal/elapsed = 1GB/30s = ~33MB/s; remaining 1GB / 33MB/s = ~30s.
func TestCalculateETAVODBytesBased(t *testing.T) {
	pt := newTestProgressTracker()
	pt.startTime = time.Now().Add(-30 * time.Second)
	pt.vodTotalBytes = 2 << 30 // 2 GB total
	pt.bytesTotal = 1 << 30    // 1 GB done

	got := pt.calculateETA()
	if got == "" {
		t.Errorf("ETA VOD-bytes: want non-empty, got empty")
	}
}

// TestCalculateETARefusesZeroRate covers the "segsPerSec <= 0" guard.
// videoSeq=0 with videoTotal>0 means segsPerSec=0 → return "".
func TestCalculateETARefusesZeroRate(t *testing.T) {
	pt := newTestProgressTracker()
	pt.videoTotal = 100
	pt.videoSeq = 0 // no progress

	got := pt.calculateETA()
	if got != "" {
		t.Errorf("ETA with zero progress: want empty, got %q", got)
	}
}
