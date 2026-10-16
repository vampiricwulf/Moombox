package worker

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/engine"
)

// newTestProgressTracker constructs a ProgressTracker with no DB hooked
// up — the tests below exercise the pure-logic format helpers without
// triggering the persistence path.
func newTestProgressTracker() *ProgressTracker {
	return &ProgressTracker{
		jobID:     "test-job",
		logger:    discardLogger{},
		now:       time.Now,
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
		{engine.ActivityWaitingForSegment, "Waiting for next segment... (1m 20s)"},
		{engine.ActivityRetrying, "Segment fetch failing - retrying... (1m 20s)"},
		{engine.ActivityWaitingResume, "Stream interrupted - waiting for resume... (1m 20s)"},
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
		engine.ActivityWaitingResume,
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

	t.Run("waiting-for-segment held back within its grace window", func(t *testing.T) {
		pt := newTestProgressTracker()
		pt.videoActivity = engine.ActivityWaitingForSegment
		pt.videoActivityStart = base.Add(-3 * time.Second)
		pt.lastSegmentAt = base.Add(-3 * time.Second) // 3s < waitingForSegmentGrace (5s)
		if a, _ := pt.dominantActivity(base); a != engine.ActivityNone {
			t.Errorf("gap 3s: got %v, want ActivityNone (no flicker on normal cadence)", a)
		}
	})

	t.Run("waiting-for-segment surfaces once the gap outgrows the grace", func(t *testing.T) {
		pt := newTestProgressTracker()
		pt.videoActivity = engine.ActivityWaitingForSegment
		pt.videoActivityStart = base.Add(-6 * time.Second)
		pt.lastSegmentAt = base.Add(-6 * time.Second) // 6s >= waitingForSegmentGrace (5s)
		if a, _ := pt.dominantActivity(base); a != engine.ActivityWaitingForSegment {
			t.Errorf("gap 6s: got %v, want ActivityWaitingForSegment", a)
		}
	})

	t.Run("recent FETCH suppresses waits while the flush is stale", func(t *testing.T) {
		// The wide-catch-up clump case: workers are pulling data (arrival is
		// fresh) but the ordered flush hasn't advanced in a while, and one
		// worker's transient retry latched an activity. The line must keep
		// showing the counter, not "retrying".
		pt := newTestProgressTracker()
		pt.videoActivity = engine.ActivityRetrying
		pt.videoActivityStart = base.Add(-10 * time.Second)
		pt.lastSegmentAt = base.Add(-30 * time.Second)     // flush quiet
		pt.lastFetchAt = base.Add(-200 * time.Millisecond) // network busy
		if a, _ := pt.dominantActivity(base); a != engine.ActivityNone {
			t.Errorf("recent fetch: got %v, want ActivityNone", a)
		}
	})

	t.Run("stale fetch AND stale flush still surface the wait", func(t *testing.T) {
		pt := newTestProgressTracker()
		pt.videoActivity = engine.ActivityRetrying
		pt.videoActivityStart = base.Add(-10 * time.Second)
		pt.lastSegmentAt = base.Add(-30 * time.Second)
		pt.lastFetchAt = base.Add(-10 * time.Second) // a genuine stall
		if a, _ := pt.dominantActivity(base); a != engine.ActivityRetrying {
			t.Errorf("genuine stall: got %v, want ActivityRetrying", a)
		}
	})

	t.Run("waiting-for-segment grace is specific — other waits still show", func(t *testing.T) {
		pt := newTestProgressTracker()
		pt.videoActivity = engine.ActivityWaitingForSegment
		pt.videoActivityStart = base.Add(-3 * time.Second)
		pt.audioActivity = engine.ActivityReconnecting // past the 2s segment grace
		pt.audioActivityStart = base.Add(-3 * time.Second)
		pt.lastSegmentAt = base.Add(-3 * time.Second)
		if a, _ := pt.dominantActivity(base); a != engine.ActivityReconnecting {
			t.Errorf("got %v, want ActivityReconnecting (only WaitingForSegment is held back)", a)
		}
	})
}

// TestSampleSpeedWindow pins the sliding-window speed average: delta over
// span across ~speedAvgWindow of samples, 0 until two samples exist, old
// samples evicted so a stall decays to 0 within the window, and a source
// switch (bytesTotal fallback → fetchedTotal) resetting the window rather
// than mixing incomparable counters.
func TestSampleSpeedWindow(t *testing.T) {
	base := time.Now()

	t.Run("averages delta over span", func(t *testing.T) {
		pt := newTestProgressTracker()
		pt.fetchedTotal = 1 // arrival source active from the first sample
		if got := pt.sampleSpeedLocked(base); got != 0 {
			t.Errorf("single sample: speed = %v, want 0", got)
		}
		pt.fetchedTotal = 1 + 3<<20
		if got := pt.sampleSpeedLocked(base.Add(1 * time.Second)); got != float64(3<<20) {
			t.Errorf("3 MB over 1s = %v, want %v", got, float64(3<<20))
		}
		pt.fetchedTotal = 1 + 6<<20
		if got := pt.sampleSpeedLocked(base.Add(2 * time.Second)); got != float64(3<<20) {
			t.Errorf("6 MB over 2s = %v, want steady %v", got, float64(3<<20))
		}
	})

	t.Run("stall decays to zero once the window slides past the transfer", func(t *testing.T) {
		pt := newTestProgressTracker()
		pt.speedWinFetched = true
		pt.fetchedTotal = 6 << 20
		pt.sampleSpeedLocked(base)
		// No new bytes for longer than the window: everything before the
		// stall evicts and the average must read 0, not a stale rate.
		if got := pt.sampleSpeedLocked(base.Add(speedAvgWindow + 2*time.Second)); got != 0 {
			t.Errorf("post-stall speed = %v, want 0", got)
		}
	})

	t.Run("source switch resets the window", func(t *testing.T) {
		pt := newTestProgressTracker()
		pt.bytesTotal = 50 << 20 // flush-counter samples (fallback source)
		pt.sampleSpeedLocked(base)
		pt.fetchedTotal = 1 << 20 // first fetch reported — source switches
		if got := pt.sampleSpeedLocked(base.Add(1 * time.Second)); got != 0 {
			t.Errorf("switch tick: speed = %v, want 0 (fresh window, one sample)", got)
		}
		pt.fetchedTotal = 3 << 20
		if got := pt.sampleSpeedLocked(base.Add(2 * time.Second)); got != float64(2<<20) {
			t.Errorf("post-switch: speed = %v, want %v from fetched samples only", got, float64(2<<20))
		}
	})
}

// TestBuildProgressString covers each branch of the progress line, one row
// per branch.
func TestBuildProgressString(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(pt *ProgressTracker)
		want string
	}{
		// The "vodTotalBytes > 0" branch — the function must return a
		// percentage rather than segment counts. Audit reports/worker.md F69.
		{"VODChunked", func(pt *ProgressTracker) {
			pt.vodTotalBytes = 1 << 30 // 1 GB
			pt.vodPercent = 25.5
		}, "V:25.5%"},
		// The same branch with chat: it appends C: 42.
		{"VODChunkedWithChat", func(pt *ProgressTracker) {
			pt.vodTotalBytes = 1 << 30
			pt.vodPercent = 25.5
			pt.chatCount = 42
		}, "V:25.5% C: 42"},
		// The "audio + video segments" branch — both totals known.
		{"SegmentedAV", func(pt *ProgressTracker) {
			pt.audioSeq = 100
			pt.audioTotal = 200
			pt.videoSeq = 50
			pt.videoTotal = 200
		}, "(V: 50/200 A: 100/200)"},
		// The audioTotal=0 / videoTotal=0 branch — segment count without
		// total.
		{"SegmentedAVUnknownTotal", func(pt *ProgressTracker) {
			pt.audioSeq = 100 // > 0 to trigger A+V branch
			pt.videoSeq = 50
		}, "(V: 50 A: 100)"},
		// The "chatCount > 0 but no video/audio progress yet" branch.
		{"ChatOnlyFallback", func(pt *ProgressTracker) {
			pt.chatCount = 17
			pt.videoSeq = 5
		}, "Seq: 5 C: 17"},
		// The catch-all bottom branch — just videoSeq, no audio/chat.
		{"VideoSeqOnly", func(pt *ProgressTracker) {
			pt.videoSeq = 1234
		}, "Seq: 1234"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pt := newTestProgressTracker()
			tc.set(pt)
			if got := pt.buildProgressString(); got != tc.want {
				t.Errorf("buildProgressString() = %q, want %q", got, tc.want)
			}
		})
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
// branch. With 30s elapsed (on a fake clock, so the figure is exact) and
// 50/100 segments, segsPerSec=50/30=1.67, remaining=50/1.67=30s — the
// sub-minute "Ns" bucket, where TestCalculateETAReadsTheTrackerClock lands in
// the minutes one.
func TestCalculateETASegmentBased(t *testing.T) {
	pt := newTestProgressTracker()
	clk := &fakeClock{t: pt.startTime.Add(30 * time.Second)}
	pt.now = clk.now
	pt.videoSeq = 50
	pt.videoTotal = 100

	if got := pt.calculateETA(); got != "30s" {
		t.Errorf("calculateETA() = %q, want %q", got, "30s")
	}
}

// TestCalculateETAVODCountsOnlyThisSessionsBytes pins the whole-file VOD
// ETA on a resumed run, driven through the real OnProgress callbacks. The
// session starts 30 s after the tracker (probing, slot handoff) with video at
// 600/1000 MB and audio at 100/200 MB — both inherited from disk — and ten
// seconds later each stream has moved 40 MB: 8 MB/s against 420 MB still to
// fetch, 52.5 s. The old formula divided the video total minus every byte
// either stream had ever written by that sum over the tracker's age, and
// showed a few seconds.
//
// Mutant: the first event counting its offset as moved — the inherited
// 700 MB reads as session speed, "5s". Mutant: dropping the audio stream's
// noteVodBytesLocked — video alone, "1m 30s". Mutant: measuring from
// pt.startTime instead of vodSessionStart — 40 s of clock, "3m 30s".
func TestCalculateETAVODCountsOnlyThisSessionsBytes(t *testing.T) {
	pt := NewProgressTracker(nil, "eta-vod", discardLogger{}, 24*time.Hour) // gate every DB write
	t.Cleanup(pt.Close)
	clk := &fakeClock{t: pt.startTime}
	pt.mu.Lock()
	pt.now = clk.now
	pt.mu.Unlock()

	newDl := func() *engine.SegmentDownloader {
		return engine.NewSegmentDownloader(engine.DownloaderOptions{BaseURL: "http://unused/", OutputFile: os.DevNull, IsDirectURL: true})
	}
	video, audio := newDl(), newDl()
	pt.AttachVideoDownloader(video)
	pt.AttachAudioDownloader(audio)
	const mb = int64(1 << 20)
	event := func(dl *engine.SegmentDownloader, bytes, total int64) {
		dl.OnProgress(engine.DownloadProgress{Bytes: bytes * mb, TotalBytes: total * mb, Percent: float64(bytes) / float64(total) * 100})
	}

	clk.advance(30 * time.Second)
	event(video, 600, 1000)
	event(audio, 100, 200)
	clk.advance(10 * time.Second)
	event(video, 640, 1000)
	event(audio, 140, 200)

	pt.mu.Lock()
	got := pt.calculateETA()
	pt.mu.Unlock()
	if got != "52s" {
		t.Errorf("calculateETA() = %q, want %q (80 MB moved in 10 s, 420 MB left)", got, "52s")
	}
}

// TestVODProgressFollowsTheStreamingHandoff is W20-26: a whole-file VOD whose
// chunked loop hands its transfer to the streaming fallback on a mid-download
// 200 goes on reporting progress from the fallback's events, which carry
// bytes but no total and no percent. The chunked loop reports 5 MB and then
// 25 MB of 100 MB ten seconds apart; the fallback 50 MB and 75 MB at the same
// pace — 70 MB moved in 30 s against 25 MB left, ten seconds. The ETA read
// only the chunked events, so it froze the offset at 25 MB while the clock
// ran on and climbed to "1m 52s"; the progress line stayed at "V:25.0%". A
// restart from byte 0 (the fallback discarding a partial the origin would
// not resume) then moves nothing, and the rate counts on from there.
//
// Mutants: noteVodBytesLocked ignoring events without a total — the ETA at
// 75 MB reads "1m 52s"; not reading the percent off the stream for them —
// the line stays "V:25.0%"; overwriting the total with an event's 0 — no ETA
// at all; counting a restart's drop as movement (`p.Bytes > s.latest` →
// `true`) — the rate goes negative and the ETA after the restart is wrong.
func TestVODProgressFollowsTheStreamingHandoff(t *testing.T) {
	pt := NewProgressTracker(nil, "eta-handoff", discardLogger{}, 24*time.Hour) // gate every DB write
	t.Cleanup(pt.Close)
	clk := &fakeClock{t: pt.startTime}
	pt.mu.Lock()
	pt.now = clk.now
	pt.mu.Unlock()

	video := engine.NewSegmentDownloader(engine.DownloaderOptions{BaseURL: "http://unused/", OutputFile: os.DevNull, IsDirectURL: true})
	pt.AttachVideoDownloader(video)
	const mb = int64(1 << 20)
	chunked := func(bytes int64) {
		video.OnProgress(engine.DownloadProgress{Bytes: bytes * mb, TotalBytes: 100 * mb, Percent: float64(bytes)})
	}
	streamed := func(bytes int64) {
		video.OnProgress(engine.DownloadProgress{Bytes: bytes * mb})
	}
	read := func() (eta, line string) {
		pt.mu.Lock()
		defer pt.mu.Unlock()
		return pt.calculateETA(), pt.buildProgressString()
	}

	chunked(5)
	clk.advance(10 * time.Second)
	chunked(25)
	clk.advance(10 * time.Second)
	streamed(50)
	clk.advance(10 * time.Second)
	streamed(75)
	if eta, line := read(); eta != "10s" || line != "V:75.0%" {
		t.Errorf("after the handoff: ETA %q, line %q; want \"10s\" and \"V:75.0%%\" (70 MB in 30 s, 25 MB left)", eta, line)
	}

	// The fallback restarts the file: 2 MB, then 12 MB ten seconds on. 80 MB
	// moved in 40 s against 88 MB left: 44 s.
	streamed(2)
	clk.advance(10 * time.Second)
	streamed(12)
	if eta, line := read(); eta != "44s" || line != "V:12.0%" {
		t.Errorf("after a restart: ETA %q, line %q; want \"44s\" and \"V:12.0%%\" (80 MB in 40 s, 88 MB left)", eta, line)
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
