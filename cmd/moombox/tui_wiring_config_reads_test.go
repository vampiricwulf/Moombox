package main

import (
	"path/filepath"
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
// Each case sets its values THROUGH the store after the runState is built, so
// a helper that answered from anything captured at wiring time is caught. The
// earlier shape — assigning the fields on the same pointer before NewStore —
// could not distinguish the two, because a pre-wiring snapshot of that pointer
// was byte-identical to a store read (review B3).
//
// Mutants: dropping the `path == ""` fallback (ffmpegPathOrDefault returns ""
// instead of "ffmpeg"); memoising either helper's first answer, or reading a
// snapshot taken when the store was wired (the post-Update assertions fail).
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
			store := config.NewStore(cfg, "")
			s := &runState{cfg: cfg, configStore: store}

			// Defaults first: this is the answer a memoising or
			// snapshot-at-wiring helper would keep handing back.
			if got := s.httpsEnabled(); got {
				t.Fatalf("httpsEnabled = %v before the update, want the false default", got)
			}
			if got := s.ffmpegPathOrDefault(); got != "ffmpeg" {
				t.Fatalf("ffmpegPathOrDefault = %q before the update, want %q", got, "ffmpeg")
			}

			if err := store.Update(func(c *config.MoomboxConfig) {
				c.Network.HTTPSEnabled = tc.https
				c.Paths.FfmpegPath = tc.ffmpeg
			}); err != nil {
				t.Fatalf("Update: %v", err)
			}

			if got := s.httpsEnabled(); got != tc.https {
				t.Errorf("httpsEnabled = %v, want %v", got, tc.https)
			}
			if got := s.ffmpegPathOrDefault(); got != tc.wantFfmpeg {
				t.Errorf("ffmpegPathOrDefault = %q, want %q", got, tc.wantFfmpeg)
			}
		})
	}
}

// B2: the comment this task added claimed ConfigLoaded is "written once at
// load and never mutated". It is not — config.Save's last statement is
// `cfg.ConfigLoaded = true`, and Store.SaveLocked/Update call Save on the very
// pointer s.cfg holds. The web handlers call SaveLocked under the store's
// write lock while runTUI's body is still running (the HTTP server is up
// first), so the two direct reads in runTUI race a whole-config save.
//
// The transition is only ever false -> true, so the practical impact is nil —
// but the read is a race and the race detector says so, which is why this
// helper exists rather than a corrected comment.
//
// Mutant: giving configLoaded() the old body (`return s.cfg.ConfigLoaded`) —
// `go test -race` reports a data race with "Previous read" in configLoaded and
// the write inside internal/config.Save.
func TestRunStateConfigLoadedReadIsLocked(t *testing.T) {
	cfg := config.Defaults()
	// A real save path, so Update reaches config.Save — the statement that
	// actually writes ConfigLoaded.
	savePath := filepath.Join(t.TempDir(), "config.toml")
	store := config.NewStore(cfg, savePath)
	s := &runState{cfg: cfg, configStore: store}

	const rounds = 60
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
			if err := store.Update(func(c *config.MoomboxConfig) {
				c.Network.HTTPSEnabled = i%2 == 0
			}); err != nil {
				t.Errorf("Update: %v", err)
				return
			}
		}
	}()

	for range rounds {
		_ = s.configLoaded()
	}
	wg.Wait()

	// The save ran, so the value the helper reads is the post-save one.
	if !s.configLoaded() {
		t.Error("configLoaded is false after config.Save ran — the helper is not reading the live struct")
	}
}
