package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
)

// memLogger records the Info and Warn lines the guard writes.
type memLogger struct {
	nopLogger
	infos, warns []string
}

func (l *memLogger) Info(msg string, _ ...any) { l.infos = append(l.infos, msg) }
func (l *memLogger) Warn(msg string, _ ...any) { l.warns = append(l.warns, msg) }

func (l *memLogger) count(lines []string, sub string) int {
	n := 0
	for _, s := range lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

const mb = int64(1) << 20

// memHarness drives one guard with readings 2 minutes apart.
type memHarness struct {
	g      *sidecarMemGuard
	log    *memLogger
	now    time.Time
	soft   int
	hard   int
	gen    uint64
	heap   int64 // V8 heap used, in bytes
	q      sidecarMemQuiet
	quiets int
}

func newMemHarness() *memHarness {
	l := &memLogger{}
	return &memHarness{
		g:    newSidecarMemGuard(l),
		log:  l,
		now:  time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC),
		soft: 200,
		gen:  1,
	}
}

// tick feeds one reading of rssMB and returns the guard's answer.
func (h *memHarness) tick(rssMB int64) sidecarMemAction {
	h.now = h.now.Add(2 * time.Minute)
	return h.g.decide(sidecarMemReading{
		RSS:      rssMB * mb,
		HeapUsed: h.heap,
		SoftMB:   h.soft,
		HardMB:   h.hard,
		Gen:      h.gen,
	}, func() sidecarMemQuiet { h.quiets++; return h.q }, h.now)
}

// TestSidecarMemGCOncePerEpisode: an over-limit sidecar that stays put gets one
// GC, not one every tick; the quiet check first runs on the fifth tick after it.
func TestSidecarMemGCOncePerEpisode(t *testing.T) {
	h := newMemHarness()
	h.q = sidecarMemQuiet{JobsWorking: true}
	if got := h.tick(250); got != memGC {
		t.Fatalf("first over-limit tick = %v, want memGC", got)
	}
	for i := 1; i <= 9; i++ {
		if got := h.tick(250); got != memNone {
			t.Fatalf("steady tick %d = %v, want memNone", i, got)
		}
		wantQuiets := 0
		if i >= 5 {
			wantQuiets = i - 4
		}
		if h.quiets != wantQuiets {
			t.Fatalf("after steady tick %d quiet() ran %d times, want %d", i, h.quiets, wantQuiets)
		}
	}
}

// TestSidecarMemGrowthRefires: growth of 10% of the limit over the lowest
// reading of the episode fires again; 9% does not.
func TestSidecarMemGrowthRefires(t *testing.T) {
	h := newMemHarness()
	h.q = sidecarMemQuiet{JobsWorking: true}
	h.tick(250)
	if got := h.tick(250 + 18); got != memNone {
		t.Errorf("growth of 9%% of the limit = %v, want memNone", got)
	}
	if got := h.tick(250 + 20); got != memGC {
		t.Errorf("growth of 10%% of the limit = %v, want memGC", got)
	}
	if got := h.tick(270 + 19); got != memNone {
		t.Errorf("growth measured from the re-fire: 19 MB = %v, want memNone", got)
	}
}

// TestSidecarMemBaselineFollowsRSSDown: the re-fire step is measured from the
// lowest reading since the last GC, so a GC that reclaimed a lot does not
// leave a baseline far above where RSS now sits.
func TestSidecarMemBaselineFollowsRSSDown(t *testing.T) {
	h := newMemHarness()
	h.q = sidecarMemQuiet{JobsWorking: true}
	if got := h.tick(440); got != memGC {
		t.Fatalf("440 = %v, want memGC", got)
	}
	if got := h.tick(285); got != memNone {
		t.Errorf("285 = %v, want memNone", got)
	}
	if got := h.tick(300); got != memNone {
		t.Errorf("300 = %v, want memNone (285 + 20 not reached)", got)
	}
	if got := h.tick(306); got != memGC {
		t.Errorf("306 = %v, want memGC (285 + 20 passed)", got)
	}
}

// TestSidecarMemBackUnderClosesTheEpisode: dropping under the limit logs once
// and closes the episode; the next crossing is a new episode with its own GC.
func TestSidecarMemBackUnderClosesTheEpisode(t *testing.T) {
	h := newMemHarness()
	h.q = sidecarMemQuiet{JobsWorking: true}
	h.tick(250)
	h.tick(150)
	h.tick(150)
	if n := h.log.count(h.log.infos, "back under soft limit"); n != 1 {
		t.Errorf("back-under logged %d times, want 1", n)
	}
	if got := h.tick(210); got != memGC {
		t.Errorf("a new crossing = %v, want memGC", got)
	}
}

// TestSidecarMemLimitZeroAndLimitChange: a limit of 0 never acts; a changed
// limit starts over from the new one.
func TestSidecarMemLimitZeroAndLimitChange(t *testing.T) {
	h := newMemHarness()
	h.soft = 0
	h.hard = 0
	for i := 0; i < 10; i++ {
		if got := h.tick(5000); got != memNone {
			t.Fatalf("limit 0 tick %d = %v, want memNone", i, got)
		}
	}
	if h.quiets != 0 {
		t.Errorf("limit 0 ran quiet() %d times", h.quiets)
	}

	// Raising the limit above RSS closes the episode.
	h = newMemHarness()
	h.q = sidecarMemQuiet{JobsWorking: true}
	h.tick(250)
	h.soft = 300
	if got := h.tick(250); got != memNone {
		t.Errorf("limit raised above RSS = %v, want memNone", got)
	}
	h.soft = 200
	if got := h.tick(250); got != memGC {
		t.Errorf("limit lowered back under RSS = %v, want memGC (a new episode)", got)
	}

	// Lowering it while under opens an episode.
	h = newMemHarness()
	h.tick(150)
	h.soft = 100
	if got := h.tick(150); got != memGC {
		t.Errorf("limit lowered under RSS = %v, want memGC", got)
	}
}

// TestSidecarMemLimitChangeClearsTheLatch: a soft limit that was below the
// sidecar's fresh baseline stops quiet recycles until the operator changes it.
func TestSidecarMemLimitChangeClearsTheLatch(t *testing.T) {
	h := newMemHarness()
	latchAfterQuietRecycle(t, h)
	h.soft = 210
	h.tick(250) // GC
	h.now = h.now.Add(7 * time.Hour)
	var got sidecarMemAction
	for i := 0; i < 5; i++ {
		got = h.tick(250)
	}
	if got != memRecycleQuiet {
		t.Errorf("after a limit change the fifth steady tick = %v, want memRecycleQuiet", got)
	}
}

// TestSidecarMemGenerationChangeResetsTheEpisode: a new child is a new episode.
func TestSidecarMemGenerationChangeResetsTheEpisode(t *testing.T) {
	h := newMemHarness()
	h.q = sidecarMemQuiet{JobsWorking: true}
	h.tick(250)
	h.tick(250)
	h.gen = 2
	if got := h.tick(250); got != memGC {
		t.Errorf("first reading of a new child = %v, want memGC", got)
	}
	if n := len(h.log.warns); n != 0 {
		t.Errorf("a generation change after no recycle warned %d times", n)
	}
}

// TestSidecarMemQuietRecycle: an idle sidecar that stays over the limit is
// recycled, and only when every quiet field says so.
func TestSidecarMemQuietRecycle(t *testing.T) {
	quiet := sidecarMemQuiet{}
	run := func(q sidecarMemQuiet) sidecarMemAction {
		h := newMemHarness()
		h.q = q
		h.tick(250)
		var got sidecarMemAction
		for i := 0; i < 5; i++ {
			got = h.tick(250)
		}
		return got
	}
	if got := run(quiet); got != memRecycleQuiet {
		t.Fatalf("idle, fifth steady tick = %v, want memRecycleQuiet", got)
	}
	for name, q := range map[string]sidecarMemQuiet{
		"JobsWorking":         {JobsWorking: true},
		"PlayersLoaded":       {PlayersLoaded: 1},
		"GenerationsInFlight": {GenerationsInFlight: 1},
		"Pending":             {Pending: 1},
	} {
		if got := run(q); got != memNone {
			t.Errorf("%s alone: fifth steady tick = %v, want memNone", name, got)
		}
	}

	// Fewer than 5 ticks.
	h := newMemHarness()
	h.tick(250)
	for i := 1; i <= 4; i++ {
		if got := h.tick(250); got != memNone {
			t.Errorf("idle, steady tick %d = %v, want memNone", i, got)
		}
	}

	// Spacing: a recycle 5 h 59 m before the fifth steady tick blocks it,
	// one 6 h before allows it.
	spacing := func(ago time.Duration) sidecarMemAction {
		h := newMemHarness()
		h.g.recycled(h.now.Add(-time.Hour), false)
		h.gen = 2
		h.tick(150) // the new child comes up under the limit: no latch
		h.tick(250) // GC
		for i := 1; i <= 4; i++ {
			h.tick(250)
		}
		h.g.recycled(h.now.Add(2*time.Minute-ago), false)
		return h.tick(250)
	}
	if got := spacing(5*time.Hour + 59*time.Minute); got != memNone {
		t.Errorf("recycle 5h59m ago = %v, want memNone", got)
	}
	if got := spacing(6 * time.Hour); got != memRecycleQuiet {
		t.Errorf("recycle 6h ago = %v, want memRecycleQuiet", got)
	}
}

// latchAfterQuietRecycle records a quiet recycle and feeds the new child's
// first reading over the limit, which sets the below-baseline latch.
func latchAfterQuietRecycle(t *testing.T, h *memHarness) {
	t.Helper()
	h.tick(250)
	h.g.recycled(h.now, false)
	h.gen++
	h.tick(250)
	if n := h.log.count(h.log.warns, "below its baseline"); n != 1 {
		t.Fatalf("below-baseline WARN logged %d times, want 1", n)
	}
}

// TestSidecarMemBelowBaselineStopsQuietRecycles: a child already over the
// limit right after a quiet recycle means the limit is below what a fresh
// sidecar needs; recycling again would loop, so the guard stops recycling
// quietly. A heap near the hard limit is still recycled.
func TestSidecarMemBelowBaselineStopsQuietRecycles(t *testing.T) {
	h := newMemHarness()
	latchAfterQuietRecycle(t, h)
	h.now = h.now.Add(7 * time.Hour)
	for i := 0; i < 20; i++ {
		if got := h.tick(250); got == memRecycleQuiet {
			t.Fatalf("tick %d recycled quietly under the below-baseline latch", i)
		}
	}
	if n := h.log.count(h.log.warns, "below its baseline"); n != 1 {
		t.Errorf("below-baseline WARN logged %d times over 20 ticks, want 1", n)
	}
	h.hard = 512
	h.heap = 512 * mb * 80 / 100
	if got := h.tick(250); got != memRecycleForced {
		t.Errorf("heap at 80%% of the hard limit under the latch = %v, want memRecycleForced", got)
	}
}

// TestSidecarMemForcedRecycleDoesNotLatch: a generation change that
// does not directly follow a quiet recycle does not set the latch, even over
// the limit.
func TestSidecarMemForcedRecycleDoesNotLatch(t *testing.T) {
	h := newMemHarness()
	h.tick(250)
	h.g.recycled(h.now, true)
	h.gen++
	h.tick(250)
	if n := len(h.log.warns); n != 0 {
		t.Errorf("a new child over the limit after a forced recycle warned %d times", n)
	}
	// A quiet recycle followed by a child under the limit, then a crash-restart
	// over it later, does not latch either.
	h = newMemHarness()
	h.tick(250)
	h.g.recycled(h.now, false)
	h.gen++
	h.tick(150)
	h.gen++
	h.tick(250)
	if n := len(h.log.warns); n != 0 {
		t.Errorf("a crash-restart long after a quiet recycle warned %d times", n)
	}
}

// TestSidecarMemForcedRecycle: a V8 heap at 80% of the hard limit recycles
// regardless of jobs and spacing, but never over a request in flight.
func TestSidecarMemForcedRecycle(t *testing.T) {
	h := newMemHarness()
	h.hard = 512
	h.heap = 512 * mb * 80 / 100 // exactly 80%
	h.q = sidecarMemQuiet{JobsWorking: true, PlayersLoaded: 3}
	h.g.recycled(h.now.Add(-time.Minute), false)
	if got := h.tick(250); got != memRecycleForced {
		t.Errorf("heap at 80%% with a job working and a recycle 1m ago = %v, want memRecycleForced", got)
	}

	h = newMemHarness()
	h.hard = 512
	h.heap = 512 * mb * 80 / 100 // exactly 80%
	h.q = sidecarMemQuiet{Pending: 1}
	if got := h.tick(250); got != memGC {
		t.Errorf("forced blocked by a pending request = %v, want memGC", got)
	}
	if got := h.tick(250); got != memNone {
		t.Errorf("forced blocked by a pending request, second tick = %v, want memNone", got)
	}
	if h.quiets != 2 {
		t.Errorf("quiet() ran %d times over two ticks, want 2 (once per decide)", h.quiets)
	}

	h = newMemHarness()
	h.hard = 512
	h.heap = 512*mb*80/100 - 1
	if got := h.tick(150); got != memNone {
		t.Errorf("heap under 80%% = %v, want memNone", got)
	}

	h = newMemHarness()
	h.hard = 0
	h.heap = 10000 * mb
	for i := 0; i < 10; i++ {
		if got := h.tick(150); got != memNone {
			t.Fatalf("HardMB 0 tick %d = %v, want memNone", i, got)
		}
	}
}

// TestSidecarMemGCFailedRefires: a GC that failed is tried again next tick.
func TestSidecarMemGCFailedRefires(t *testing.T) {
	h := newMemHarness()
	h.q = sidecarMemQuiet{JobsWorking: true}
	h.tick(250)
	h.g.gcFailed()
	if got := h.tick(250); got != memGC {
		t.Errorf("tick after a failed GC = %v, want memGC", got)
	}
	if got := h.tick(250); got != memNone {
		t.Errorf("tick after the retried GC = %v, want memNone", got)
	}
}

// TestSidecarMemGCDoneLogsTheFireLine: the log line the operator searches
// for stays byte-identical.
func TestSidecarMemGCDoneLogsTheFireLine(t *testing.T) {
	l := &memLogger{}
	g := newSidecarMemGuard(l)
	g.gcDone(sidecar.MemoryStats{RSS: 300 * mb, HeapUsed: 100 * mb}, sidecar.MemoryStats{RSS: 200 * mb, HeapUsed: 50 * mb}, 200)
	if len(l.infos) != 1 || l.infos[0] != "[Memory] sidecar soft limit hit; ran GC" {
		t.Errorf("gcDone logged %q", l.infos)
	}
}
