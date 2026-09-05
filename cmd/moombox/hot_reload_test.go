package main

import (
	"net/http/httptest"
	"runtime/debug"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/web"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// TestApplyGoSoftLimit: the runtime limit follows the setting; zero clears it.
// debug.SetMemoryLimit(-1) reads the current limit without changing it.
func TestApplyGoSoftLimit(t *testing.T) {
	orig := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	s := &runState{}
	s.applyGoSoftLimit(512)
	if got := debug.SetMemoryLimit(-1); got != 512<<20 {
		t.Errorf("limit = %d, want %d", got, 512<<20)
	}
	// Cleared restores Go's BOOT limit, not MaxInt64: a GOMEMLIMIT set in the
	// environment is the operator's ceiling and must survive a save that leaves
	// go_soft_limit_mb blank.
	s.applyGoSoftLimit(0)
	if got := debug.SetMemoryLimit(-1); got != bootMemoryLimit {
		t.Errorf("limit after 0 = %d, want the boot limit %d", got, bootMemoryLimit)
	}
}

// TestApplyTrustForwardedProto: the web package's flag follows the setting,
// observable through IsRequestSecure on a plain request that carries the
// header.
func TestApplyTrustForwardedProto(t *testing.T) {
	t.Cleanup(func() { web.SetTrustForwardedProto(false) })
	s := &runState{}
	req := httptest.NewRequest("GET", "http://example/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")

	s.applyTrustForwardedProto(false)
	if web.IsRequestSecure(req) {
		t.Fatal("header trusted while the setting is off")
	}
	s.applyTrustForwardedProto(true)
	if !web.IsRequestSecure(req) {
		t.Fatal("header not trusted after applyTrustForwardedProto(true)")
	}
}

// TestApplyFfmpegPathReachesTheDownloadWorker: trims were the only consumer the
// hot-reload reached, so a saved ffmpeg_path left every download muxing, probing
// and part-merging with the boot binary. The download worker is reached
// independently of the trim service — a nil trimSvc must not skip it.
func TestApplyFfmpegPathReachesTheDownloadWorker(t *testing.T) {
	w := worker.NewDownloadWorker(nil, nil, &config.MoomboxConfig{}, sweepTestLogger{}, nil)
	s := &runState{dlWorker: w} // trimSvc deliberately nil
	s.applyFfmpegPath("C:/tools/ffmpeg.exe")
	if got := w.FFprobePath(); got != "C:/tools/ffprobe.exe" && got != `C:\tools\ffprobe.exe` {
		t.Errorf("download worker FFprobePath = %q after applyFfmpegPath", got)
	}
	s.applyFfmpegPath("")
	if got := w.FFprobePath(); got != "ffprobe" {
		t.Errorf("download worker FFprobePath = %q, want ffprobe after blank", got)
	}
}

// TestApplyFfmpegPath: the trim service's muxer is rebuilt for the new path;
// a nil trim service is tolerated (early wiring).
func TestApplyFfmpegPath(t *testing.T) {
	(&runState{}).applyFfmpegPath("C:/x/ffmpeg.exe") // must not panic

	s := &runState{trimSvc: worker.NewTrimService(nil, "", sweepTestLogger{})}
	s.applyFfmpegPath("C:/tools/ffmpeg.exe")
	if got := s.trimSvc.FFprobePath(); got != "C:/tools/ffprobe.exe" && got != `C:\tools\ffprobe.exe` {
		t.Errorf("FFprobePath = %q after applyFfmpegPath", got)
	}
	s.applyFfmpegPath("")
	if got := s.trimSvc.FFprobePath(); got != "ffprobe" {
		t.Errorf("FFprobePath = %q, want ffprobe after blank", got)
	}
}
