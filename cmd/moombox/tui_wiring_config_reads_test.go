package main

import (
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// CORE-24: four closures wired in runTUI read s.cfg.Network.HTTPSEnabled and
// s.cfg.Paths.FfmpegPath directly. They outlive wiring — the R Y overlay and
// the setup wizard's FFmpeg probe fire whenever the operator opens them — and
// PUT /api/config replaces the whole struct under the store's write lock, so
// an unlocked field read is a genuine data race, not a stale-value nuisance.
//
// Mutant: giving either helper the old body (`return s.cfg.Network.HTTPSEnabled`
// / `path := s.cfg.Paths.FfmpegPath`) — `go test -race` reports a data race on
// this test with "Previous write ... config.(*Store).Update".
func TestRunStateConfigReadsAreLocked(t *testing.T) {
	cfg := config.Defaults()
	s := &runState{cfg: cfg, configStore: config.NewStore(cfg, "")}

	const rounds = 300
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("the config writer panicked: %v", r)
			}
		}()
		for i := range rounds {
			_ = s.configStore.Update(func(c *config.MoomboxConfig) {
				c.Network.HTTPSEnabled = i%2 == 0
				if i%2 == 0 {
					c.Paths.FfmpegPath = "C:/tools/ffmpeg.exe"
				} else {
					c.Paths.FfmpegPath = ""
				}
			})
		}
	}()

	for range rounds {
		_ = s.httpsEnabled()
		if p := s.ffmpegPathOrDefault(); p == "" {
			t.Fatal("ffmpegPathOrDefault returned an empty path — the PATH fallback is what every caller applied itself")
		}
	}
	wg.Wait()
}

// The helpers must return the same values the four call sites read before,
// byte for byte — the point of CORE-24 is the lock, not a behaviour change.
//
// Mutant: dropping the `path == ""` fallback, or reading Network.HTTPSEnabled
// from a snapshot taken before the store was wired.
func TestRunStateConfigReadsReturnTheStoredValues(t *testing.T) {
	for _, tc := range []struct {
		name       string
		https      bool
		ffmpeg     string
		wantFfmpeg string
	}{
		{"unset", false, "", "ffmpeg"},
		{"set", true, "C:/tools/ffmpeg.exe", "C:/tools/ffmpeg.exe"},
		{"relative", false, "./bin/ffmpeg", "./bin/ffmpeg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Network.HTTPSEnabled = tc.https
			cfg.Paths.FfmpegPath = tc.ffmpeg
			s := &runState{cfg: cfg, configStore: config.NewStore(cfg, "")}

			if got := s.httpsEnabled(); got != tc.https {
				t.Errorf("httpsEnabled = %v, want %v", got, tc.https)
			}
			if got := s.ffmpegPathOrDefault(); got != tc.wantFfmpeg {
				t.Errorf("ffmpegPathOrDefault = %q, want %q", got, tc.wantFfmpeg)
			}
		})
	}
}
