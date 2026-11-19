package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
)

// sidecarMemAction is what the stats ticker does about the sidecar's memory
// on one tick.
type sidecarMemAction int

const (
	memNone          sidecarMemAction = iota
	memGC                             // ask the sidecar for a full V8 GC
	memRecycleQuiet                   // replace an idle child that stayed over the soft limit
	memRecycleForced                  // replace a child whose V8 heap is near the hard limit
)

const (
	// memGrowStepPct: within an episode a GC is asked for again only once RSS
	// has grown this share of the soft limit over its lowest reading since
	// the last GC.
	memGrowStepPct = 10
	// memSustainTicks: over-limit ticks after the episode's first GC before
	// an idle child may be recycled.
	memSustainTicks = 5
	// memRecycleSpacing: the least time between two quiet recycles.
	memRecycleSpacing = 6 * time.Hour
	// memForcedHeapPct: a V8 heap at this share of the hard limit is recycled
	// without waiting for the host to be idle.
	memForcedHeapPct = 80
)

// sidecarMemReading is one tick's view of the sidecar.
type sidecarMemReading struct {
	RSS, HeapUsed  int64
	SoftMB, HardMB int
	Gen            uint64 // sidecar.Generation()
}

// sidecarMemQuiet is whether a recycle would interrupt anything. A field that
// could not be read is reported non-zero.
type sidecarMemQuiet struct {
	JobsWorking                                 bool
	PlayersLoaded, GenerationsInFlight, Pending int
}

// sidecarMemGuard turns the stats ticker's sidecar readings into actions: one
// GC per episode over the soft limit (again only on real growth), a recycle
// of an idle child that stays over it, and a recycle of a child whose V8 heap
// nears the hard limit.
//
// Not goroutine-safe: the stats ticker is its only caller.
type sidecarMemGuard struct {
	log interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	seen       bool // a reading has been taken; lastGen and lastSoftMB hold it
	lastGen    uint64
	lastSoftMB int

	// The episode: the stretch of readings over the soft limit.
	episode     bool
	lastFireRSS int64 // the lowest RSS since the last GC
	ticks       int   // over-limit ticks since the episode's first GC

	// belowBaseline is set when the child a quiet recycle started is over the
	// soft limit at its first quiet reading: the limit is below what a fresh
	// sidecar needs, so quiet recycles stop until the limit changes.
	belowBaseline bool
	// quietRecycled: the last action was a quiet recycle whose new child has
	// not been read yet.
	quietRecycled bool
	// baselineOpen: the child being read is the one a quiet recycle started,
	// and it has not yet been read under the limit, nor quiet and over it.
	// A reading over the limit with players loaded, a generation in flight or
	// a request pending answers nothing: the first mint after a restart is a
	// cold BotGuard generation.
	baselineOpen bool
	recycledEver  bool
	lastRecycle   time.Time
}

func newSidecarMemGuard(log interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *sidecarMemGuard {
	return &sidecarMemGuard{log: log}
}

// decide answers one reading. quiet is called only when a recycle or the
// below-baseline question is open, and at most once.
func (g *sidecarMemGuard) decide(r sidecarMemReading, quiet func() sidecarMemQuiet, now time.Time) sidecarMemAction {
	limit := int64(r.SoftMB) << 20
	over := r.SoftMB > 0 && r.RSS >= limit

	if g.seen && r.Gen != g.lastGen {
		g.closeEpisode()
		// Only the child the quiet recycle started answers the question; one
		// that replaced it after a death does not inherit it.
		g.baselineOpen = g.quietRecycled
		g.quietRecycled = false
	}
	if g.seen && r.SoftMB != g.lastSoftMB {
		g.closeEpisode()
		g.belowBaseline, g.baselineOpen = false, false
	}
	g.seen, g.lastGen, g.lastSoftMB = true, r.Gen, r.SoftMB

	var q *sidecarMemQuiet
	askQuiet := func() sidecarMemQuiet {
		if q == nil {
			v := quiet()
			q = &v
		}
		return *q
	}

	if g.baselineOpen {
		if !over {
			g.baselineOpen = false
		} else if v := askQuiet(); v.PlayersLoaded == 0 && v.GenerationsInFlight == 0 && v.Pending == 0 {
			g.baselineOpen, g.belowBaseline = false, true
			g.log.Warn("[Memory] sidecar is over its soft limit right after a restart; sidecar_soft_limit_mb is below its baseline",
				"rss_mb", float64(r.RSS)/1048576, "threshold_mb", r.SoftMB)
		}
	}

	if r.HardMB > 0 && r.HeapUsed >= int64(r.HardMB)<<20*memForcedHeapPct/100 {
		if v := askQuiet(); v.Pending == 0 && v.GenerationsInFlight == 0 {
			return memRecycleForced
		}
	}

	if !over {
		if g.episode {
			g.log.Info("[Memory] sidecar back under soft limit",
				"rss_mb", float64(r.RSS)/1048576, "threshold_mb", r.SoftMB)
		}
		g.closeEpisode()
		return memNone
	}

	if !g.episode {
		g.episode, g.lastFireRSS, g.ticks = true, r.RSS, 0
		return memGC
	}

	g.lastFireRSS = min(g.lastFireRSS, r.RSS)
	g.ticks++
	if r.RSS >= g.lastFireRSS+limit*memGrowStepPct/100 {
		g.lastFireRSS = r.RSS
		return memGC
	}

	if g.ticks >= memSustainTicks && !g.belowBaseline &&
		(!g.recycledEver || now.Sub(g.lastRecycle) >= memRecycleSpacing) {
		v := askQuiet()
		if !v.JobsWorking && v.PlayersLoaded == 0 && v.GenerationsInFlight == 0 && v.Pending == 0 {
			return memRecycleQuiet
		}
	}
	return memNone
}

func (g *sidecarMemGuard) closeEpisode() {
	g.episode, g.lastFireRSS, g.ticks = false, 0, 0
}

// gcDone logs a GC the guard asked for.
func (g *sidecarMemGuard) gcDone(before, after sidecar.MemoryStats, softMB int) {
	g.log.Info("[Memory] sidecar soft limit hit; ran GC",
		"threshold_mb", softMB,
		"rss_mb", float64(after.RSS)/1048576,
		"reclaimed_mb", float64(before.RSS-after.RSS)/1048576.0,
		"heap_reclaimed_mb", float64(before.HeapUsed-after.HeapUsed)/1048576.0,
	)
}

// gcFailed reopens the episode, so the next over-limit reading asks again.
func (g *sidecarMemGuard) gcFailed() { g.closeEpisode() }

// recycled records a recycle that went through.
func (g *sidecarMemGuard) recycled(now time.Time, forced bool) {
	g.recycledEver, g.lastRecycle = true, now
	g.quietRecycled = !forced
}

// guardSidecarMemory runs the guard on one healthy reading of the sidecar and
// carries out what it decides. The stats ticker is its only caller.
func (s *runState) guardSidecarMemory(ctx context.Context, g *sidecarMemGuard, stats sidecar.MemoryStats) {
	var softMB, hardMB int
	s.configStore.Read(func(c *config.MoomboxConfig) {
		softMB = c.Memory.SidecarSoftLimitMB
		hardMB = c.Memory.SidecarHardLimitMB
	})
	now := time.Now()
	r := sidecarMemReading{
		RSS:      stats.RSS,
		HeapUsed: stats.HeapUsed,
		SoftMB:   softMB,
		HardMB:   hardMB,
		Gen:      s.bgSidecar.Generation(),
	}
	switch action := g.decide(r, func() sidecarMemQuiet { return s.sidecarQuiet(ctx) }, now); action {
	case memGC:
		gcCtx, gcCancel := context.WithTimeout(ctx, 5*time.Second)
		res, err := s.bgSidecar.TriggerGC(gcCtx)
		gcCancel()
		if err != nil {
			g.log.Debug("[Memory] sidecar TriggerGC failed", slog.String("err", err.Error()))
			g.gcFailed()
			return
		}
		g.gcDone(res.Before, res.After, softMB)
	case memRecycleQuiet, memRecycleForced:
		if s.bgSupervisor == nil {
			return
		}
		forced := action == memRecycleForced
		if forced {
			g.log.Warn("[Memory] recycling sidecar: V8 heap near the hard limit",
				"heap_mb", float64(stats.HeapUsed)/1048576, "hard_limit_mb", hardMB)
		} else {
			g.log.Info("[Memory] recycling idle sidecar: memory stayed over the soft limit",
				"rss_mb", float64(stats.RSS)/1048576, "threshold_mb", softMB)
		}
		err := s.bgSupervisor.Recycle(ctx)
		switch {
		case err == nil:
			g.recycled(now, forced)
		case errors.Is(err, sidecar.ErrRecycleBusy), errors.Is(err, sidecar.ErrStopped):
			g.log.Debug("[Memory] sidecar recycle skipped", "err", err.Error())
		default:
			g.log.Warn("[Memory] sidecar recycle failed", "err", err.Error())
		}
	}
}

// sidecarQuiet reads what a recycle would interrupt. A read that fails counts
// as not quiet.
func (s *runState) sidecarQuiet(ctx context.Context) sidecarMemQuiet {
	var q sidecarMemQuiet
	if jobs, err := s.db.GetAllJobs(); err != nil {
		q.JobsWorking = true
	} else {
		for _, j := range jobs {
			if j != nil && jobstate.KeepsHostAwake(j.Status) {
				q.JobsWorking = true
				break
			}
		}
	}
	stCtx, stCancel := context.WithTimeout(ctx, time.Second)
	st, err := s.bgSidecar.GetStats(stCtx)
	stCancel()
	if err != nil {
		// Unknown: block both recycles, the forced one included.
		q.PlayersLoaded, q.GenerationsInFlight = -1, -1
	} else {
		q.PlayersLoaded, q.GenerationsInFlight = st.PlayersLoaded, st.GenerationsInFlight
	}
	// Read after GetStats has returned, so its own request is not counted.
	q.Pending = s.bgSidecar.PendingCount()
	return q
}
