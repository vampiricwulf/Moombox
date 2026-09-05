package main

import (
	"log/slog"
	"math"
	"runtime/debug"

	"github.com/vampiricwulf/Moombox/internal/web"
)

// The three settings that are read once at startup and re-applied here on
// every config save from either UI (the web PUT via ConfigRoutesCallbacks,
// the TUI via OnSaveConfig). Each is idempotent and cheap, so the TUI path
// — which has no pre-mutation snapshot to diff against — calls them
// unconditionally.

// applyGoSoftLimit re-applies memory.go_soft_limit_mb. Zero or negative
// clears the limit (math.MaxInt64 is Go's documented "no limit").
func (s *runState) applyGoSoftLimit(mb int) {
	if mb > 0 {
		debug.SetMemoryLimit(int64(mb) << 20)
	} else {
		debug.SetMemoryLimit(math.MaxInt64)
	}
	if s.log != nil {
		s.log.Info("Go soft memory limit re-applied", slog.Int("mb", mb))
	}
}

// applyTrustForwardedProto re-applies network.trust_forwarded_proto to the
// web package's atomic flag.
func (s *runState) applyTrustForwardedProto(trust bool) {
	web.SetTrustForwardedProto(trust)
}

// applyFfmpegPath re-applies paths.ffmpeg_path to the trim service, which
// captured the path when its muxer was built.
func (s *runState) applyFfmpegPath(path string) {
	if s.trimSvc == nil {
		return
	}
	s.trimSvc.SetFfmpegPath(path)
	if s.log != nil {
		s.log.Info("trim service ffmpeg path re-applied", slog.String("path", path))
	}
}
