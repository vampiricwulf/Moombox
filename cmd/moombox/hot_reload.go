package main

import (
	"log/slog"
	"runtime/debug"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// The four settings that are read once at startup and re-applied here on
// every config save from either UI (the web PUT via ConfigRoutesCallbacks,
// the TUI via OnSaveConfig) — and, for the ffmpeg path, from a third writer:
// POST /api/ffmpeg/check via FFmpegDeps.OnFfmpegPathChange, which persists a
// verified path with no restart in between. Each is idempotent and cheap, so
// the TUI path — which has no pre-mutation snapshot to diff against — calls
// them unconditionally.

// bootMemoryLimit is Go's memory limit as it stood before any service applied
// the config — math.MaxInt64 normally, or whatever GOMEMLIMIT was set to in the
// environment. Captured at package init so clearing go_soft_limit_mb restores
// the operator's ceiling instead of silently deleting it. SetMemoryLimit(-1)
// reads the current limit without changing it.
var bootMemoryLimit = debug.SetMemoryLimit(-1)

// applyGoSoftLimit re-applies memory.go_soft_limit_mb. Zero or negative
// restores the boot limit (an environment GOMEMLIMIT, else Go's "no limit").
func (s *runState) applyGoSoftLimit(mb int) {
	if mb > 0 {
		debug.SetMemoryLimit(int64(mb) << 20)
	} else {
		debug.SetMemoryLimit(bootMemoryLimit)
	}
	if s.log != nil {
		s.log.Debug("Go soft memory limit re-applied", slog.Int("mb", mb))
	}
}

// applyTrustForwardedProto re-applies network.trust_forwarded_proto to the
// web package's atomic flag.
func (s *runState) applyTrustForwardedProto(trust bool) {
	web.SetTrustForwardedProto(trust)
}

// applyFfmpegPath re-applies paths.ffmpeg_path to both consumers that captured
// it when their muxers were built: the trim service, and the download worker's
// orchestrator (mux, probe and part merge for every download). Each is reached
// independently — a nil one must not skip the other.
// Three callers: the config PUT's diff, the TUI save, and POST
// /api/ffmpeg/check (WEB-2).
func (s *runState) applyFfmpegPath(path string) {
	if s.trimSvc != nil {
		s.trimSvc.SetFfmpegPath(path)
	}
	if s.dlWorker != nil {
		s.dlWorker.SetFfmpegPath(path)
	}
	if s.log != nil {
		s.log.Debug("ffmpeg path re-applied", slog.String("path", path))
	}
}

// applyReorderBudget re-applies the two downloader reorder ceilings
// (reorder_buffer_mb / reorder_budget_mb) to the engine's process-wide
// budget. Reached from three places: initServices at boot, the config PUT's
// OnReorderBudgetChange callback, and the TUI's OnSaveConfig hot-reload
// block — so the value never travels through engine.DownloaderOptions or the
// worker's strategies, which would have meant threading one process-wide
// number through seven per-job call sites.
//
// The clamp warning is emitted whenever the pair is incoherent as saved (a
// per-job ceiling above the process budget), so boot warns exactly once and a
// save that leaves the pair incoherent says so again rather than going quiet
// about a setting that is not doing what it reads like.
func (s *runState) applyReorderBudget(d config.DownloaderConfig) {
	perJobBytes, totalBytes, clamped := d.ReorderLimitBytes()
	engine.ConfigureReorder(perJobBytes, totalBytes)
	if s.log == nil {
		return
	}
	if clamped {
		s.log.Warn("downloader.reorder_buffer_mb is above downloader.reorder_budget_mb — clamped to the process-wide budget",
			slog.Int("reorder_buffer_mb", d.ReorderBufferMB),
			slog.Int("reorder_budget_mb", d.ReorderBudgetMB))
		return
	}
	s.log.Debug("reorder ceilings applied",
		slog.Int("per_job_bytes", perJobBytes),
		slog.Int("total_bytes", totalBytes))
}
