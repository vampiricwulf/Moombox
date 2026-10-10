package routes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

// TestApplyValidatedFfmpegPathRefusesATraversalPathAndPersistsToDisk closes
// W-CW1's second half and Task 2's minor in one test.
//
// PUT /api/config refuses any path with a ".." segment (pathFieldError), and
// validateConfigUpdates has no grandfather clause for paths — so a
// ".."-bearing value that reached paths.ffmpeg_path made EVERY later full-form
// save 400 on a field the operator never touched, until config.toml was edited
// by hand. Executing `-version` is not a superset of that string rule:
// `<dir>\other\..\bin\ffmpeg.exe` answers it perfectly well. The guard lives
// here as well as in the handler so no future caller can persist one either.
//
// The disk re-read is the direct persistence pin: store.Read returns the LIVE
// in-memory struct, so on its own it cannot tell "written to config.toml" from
// "mutated in memory".
//
// THE MUTANTS:
//   - drop the pathFieldError guard from applyValidatedFfmpegPath: the
//     traversal call returns nil (assertion "want an error").
//   - skip SaveLocked: the happy path's disk re-read still holds the default.
func TestApplyValidatedFfmpegPathRefusesATraversalPathAndPersistsToDisk(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	store := config.NewStore(config.Defaults(), cfgPath)

	const want = `C:\tools\ffmpeg.exe`
	if err := applyValidatedFfmpegPath(store, want, func(string) {}); err != nil {
		t.Fatalf("applyValidatedFfmpegPath (happy path): %v", err)
	}
	fromDisk, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load after the happy path: %v", err)
	}
	if fromDisk.Paths.FfmpegPath != want {
		t.Fatalf("config.toml holds %q, want %q — the verified path never reached DISK, so the next "+
			"restart loses it", fromDisk.Paths.FfmpegPath, want)
	}

	// Mixed separators on purpose: config.PathHasTraversal treats both as
	// separators on every platform, and filepath.Join would clean the ".."
	// away before the guard ever saw it.
	traversal := dir + "/other/../bin/ffmpeg.exe"
	notified := false
	if err := applyValidatedFfmpegPath(store, traversal, func(string) { notified = true }); err == nil {
		t.Error("a path PUT /api/config refuses must not be persistable here either — want an error")
	}
	if notified {
		t.Error("OnFfmpegPathChange fired for a refused path — the muxers must never be told about one")
	}

	live := ""
	store.Read(func(c *config.MoomboxConfig) { live = c.Paths.FfmpegPath })
	if live != want {
		t.Errorf("in-memory paths.ffmpeg_path = %q, want the previous %q — the refusal must not have "+
			"mutated the config", live, want)
	}
	fromDisk, err = config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load after the refusal: %v", err)
	}
	if fromDisk.Paths.FfmpegPath != want {
		t.Errorf("config.toml holds %q, want the previous %q — a refused path reached disk and every "+
			"later PUT /api/config would 400 on it", fromDisk.Paths.FfmpegPath, want)
	}
}

// TestFFmpegCheckRouteRefusesATraversalPathBeforeExecutingIt is the route half
// of W-CW1. No ffmpeg is needed: the refusal happens BEFORE checkFFmpeg
// spawns anything, which is the point — Moombox must not execute a path it
// has already decided it will not store.
//
// THE MUTANT: drop the pathFieldError call from the POST handler — the path is
// spawned (valid=false for a file that does not exist) and the answer is 200,
// not 400.
func TestFFmpegCheckRouteRefusesATraversalPathBeforeExecutingIt(t *testing.T) {
	dir := t.TempDir()
	store := config.NewStore(config.Defaults(), filepath.Join(dir, "config.toml"))
	before := ""
	store.Read(func(c *config.MoomboxConfig) { before = c.Paths.FfmpegPath })

	notified := ""
	r := chi.NewRouter()
	FFmpegRoutes(r, &FFmpegDeps{
		Store:              store,
		Logger:             ffmpegTestLogger{},
		OnFfmpegPathChange: func(p string) { notified = p },
	})

	body, _ := json.Marshal(map[string]string{"path": dir + "/other/../bin/ffmpeg.exe"})
	req := httptest.NewRequest("POST", "/api/ffmpeg/check", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/ffmpeg/check with a .. segment: want 400, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	json.NewDecoder(rec.Body).Decode(&resp)
	// The PUT's own string, so the two writers of paths.ffmpeg_path cannot
	// disagree about what a valid path is.
	if resp.Error != "Path cannot contain a .. segment" {
		t.Errorf("error = %q, want PUT /api/config's own message for this field", resp.Error)
	}

	after := ""
	store.Read(func(c *config.MoomboxConfig) { after = c.Paths.FfmpegPath })
	if after != before {
		t.Errorf("paths.ffmpeg_path moved to %q — a refused path must not be persisted", after)
	}
	if notified != "" {
		t.Errorf("OnFfmpegPathChange fired with %q for a refused path", notified)
	}
}

// TestFFmpegPathMustNameFFmpeg: Moombox runs whatever paths.ffmpeg_path names
// (`-version` on the check route, every mux once stored), and a LAN client
// could point it at bytes it planted — POST /api/import writes an upload as
// "<title> [<id>].mp4", and Windows runs a PE whatever its extension. The
// executable must be named ffmpeg / ffmpeg.exe, which an import never is.
//
// Mutants: drop ffmpegPathError from the check route (the script below RUNS
// and the marker appears); drop the newFFmpegPathError call from PUT
// /api/config (the changed path is stored).
func TestFFmpegPathMustNameFFmpeg(t *testing.T) {
	for p, want := range map[string]bool{
		"":                                 true,
		"ffmpeg":                           true,
		"/usr/bin/ffmpeg":                  true,
		`C:\tools\ffmpeg\bin\FFMPEG.EXE`:   true,
		"/data/out/imports/x [abc].mp4":    false,
		`C:\Moombox\out\imports\x [a].mp4`: false,
		"/usr/bin/python3":                 false,
	} {
		if got := ffmpegPathError(p) == ""; got != want {
			t.Errorf("ffmpegPathError(%q) accepted = %v, want %v", p, got, want)
		}
	}

	if runtime.GOOS == "windows" {
		return // the planted script below is a shell script
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	script := filepath.Join(dir, "planted [abc].mp4")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(config.Defaults(), filepath.Join(dir, "config.toml"))
	r := chi.NewRouter()
	FFmpegRoutes(r, &FFmpegDeps{Store: store, Logger: ffmpegTestLogger{}})
	body, _ := json.Marshal(map[string]string{"path": script})
	req := httptest.NewRequest("POST", "/api/ffmpeg/check", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("check route: status %d, want 400 for a path not named ffmpeg", rec.Code)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the check route EXECUTED a path not named ffmpeg")
	}

	// PUT /api/config: a changed path is refused, the stored one is not.
	f := newConfigRoutesFixture(t)
	raw, _ := json.Marshal(map[string]any{"paths": map[string]any{"ffmpeg_path": script}})
	put := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(raw))
	rec = httptest.NewRecorder()
	f.router.ServeHTTP(rec, put)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "paths.ffmpeg_path") {
		t.Errorf("PUT a planted ffmpeg_path: got %d %s, want a 400 naming paths.ffmpeg_path", rec.Code, rec.Body.String())
	}
	f.store.Update(func(c *config.MoomboxConfig) { c.Paths.FfmpegPath = "/opt/legacy/ffmpeg-7" })
	putConfig(t, f, map[string]any{"paths": map[string]any{"ffmpeg_path": "/opt/legacy/ffmpeg-7"}})
}

// A path that answers -version but cannot be SAVED is not a 200 "valid": the
// setup step writes the path into its cached config on that answer, because
// the server saved it — and the live muxers never got it either — so the UI
// and the disk disagreed with nothing on screen saying so.
//
// Mutant: logging the save failure and answering valid:true again.
func TestFFmpegCheckRouteReportsAFailedSave(t *testing.T) {
	real, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH — this route only saves a path that answers -version")
	}
	dir := t.TempDir()
	notADir := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(config.Defaults(), filepath.Join(notADir, "config.toml"))
	r := chi.NewRouter()
	FFmpegRoutes(r, &FFmpegDeps{Store: store, Logger: ffmpegTestLogger{}})

	body, _ := json.Marshal(map[string]string{"path": real})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/api/ffmpeg/check", bytes.NewReader(body)))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("a valid path whose save failed: %d %s, want 500", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"valid":true`) {
		t.Errorf("the failed save still answered valid: %s", rec.Body.String())
	}
}
