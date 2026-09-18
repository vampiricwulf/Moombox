package routes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// ffmpegTestLogger satisfies FFmpegDeps.Logger. The anonymous per-struct
// logger interface is the project's rule; this is its test-side stand-in.
type ffmpegTestLogger struct{}

func (ffmpegTestLogger) Info(string, ...any)  {}
func (ffmpegTestLogger) Error(string, ...any) {}

// TestApplyValidatedFfmpegPathPersistsAndNotifies is the WEB-2 pin.
//
// Two consumers captured paths.ffmpeg_path when their muxers were built —
// worker.TrimService and worker.DownloadOrchestrator — and the ONLY thing that
// re-seats them is the OnFfmpegPathChange callback. POST /api/ffmpeg/check
// saved the new path and never fired it, so every mux, ffprobe and part merge
// kept the value that had just failed, and a later PUT /api/config could not
// repair it (the stored value already equalled the new one, so the config
// diff never fired the callback either).
//
// THE MUTANTS:
//   - delete the onChange(path) call: `notified` stays "" (assertion 2).
//   - skip SaveLocked: the next restart loses the verified path (assertion 1).
//   - notify BEFORE the save: a failed save leaves the muxers pointing at a
//     path the config does not hold (assertion 3).
func TestApplyValidatedFfmpegPathPersistsAndNotifies(t *testing.T) {
	dir := t.TempDir()
	store := config.NewStore(config.Defaults(), filepath.Join(dir, "config.toml"))

	const want = `C:\tools\ffmpeg.exe`
	notified := ""
	if err := applyValidatedFfmpegPath(store, want, func(p string) { notified = p }); err != nil {
		t.Fatalf("applyValidatedFfmpegPath: %v", err)
	}

	stored := ""
	store.Read(func(c *config.MoomboxConfig) { stored = c.Paths.FfmpegPath })
	if stored != want {
		t.Errorf("stored path: want %q, got %q — a restart would lose the verified path", want, stored)
	}
	if notified != want {
		t.Errorf("OnFfmpegPathChange: want %q, got %q — the live muxers keep the failing boot value and "+
			"every mux fails until a restart", want, notified)
	}

	// A save that cannot succeed must not tell the muxers a path the config
	// does not hold: aim the store at a path whose PARENT is a regular file.
	notADir := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	bad := config.NewStore(config.Defaults(), filepath.Join(notADir, "config.toml"))
	badNotified := false
	if err := applyValidatedFfmpegPath(bad, "ffmpeg", func(string) { badNotified = true }); err == nil {
		t.Fatal("a save into a non-directory must return an error")
	}
	if badNotified {
		t.Error("OnFfmpegPathChange fired although the save failed — notify must follow a SUCCESSFUL save")
	}
}

// TestFFmpegCheckRouteReAppliesTheVerifiedPath is the end-to-end half. It needs
// a real ffmpeg because the handler only saves a path that answers -version;
// CI installs one (.github/workflows/ci.yml) and so does the dev box
// (CLAUDE.md: "Runtime requires FFmpeg on PATH").
//
// MUTANT: put the inline mu.Lock / SaveLocked / mu.Unlock back in the handler
// in place of applyValidatedFfmpegPath — `notified` stays "".
func TestFFmpegCheckRouteReAppliesTheVerifiedPath(t *testing.T) {
	real, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH — this route only saves a path that answers -version")
	}

	dir := t.TempDir()
	store := config.NewStore(config.Defaults(), filepath.Join(dir, "config.toml"))

	notified := ""
	r := chi.NewRouter()
	FFmpegRoutes(r, &FFmpegDeps{
		Store:              store,
		Logger:             ffmpegTestLogger{},
		OnFfmpegPathChange: func(p string) { notified = p },
	})

	body, _ := json.Marshal(map[string]string{"path": real})
	req := httptest.NewRequest("POST", "/api/ffmpeg/check", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/ffmpeg/check: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Valid bool `json:"valid"`
	}
	json.NewDecoder(rec.Body).Decode(&resp)
	if !resp.Valid {
		t.Fatalf("ffmpeg at %q did not validate; nothing to assert", real)
	}
	if notified != real {
		t.Errorf("OnFfmpegPathChange: want %q, got %q — the route saved the path without telling the muxers",
			real, notified)
	}
}
