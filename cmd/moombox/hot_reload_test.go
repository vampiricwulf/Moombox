package main

import (
	"math"
	"net/http/httptest"
	"runtime/debug"
	"testing"

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
	s.applyGoSoftLimit(0)
	if got := debug.SetMemoryLimit(-1); got != math.MaxInt64 {
		t.Errorf("limit after 0 = %d, want MaxInt64 (cleared)", got)
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
