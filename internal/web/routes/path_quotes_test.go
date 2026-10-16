package routes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// Explorer's "Copy as path" (Ctrl+Shift+C) wraps the path in double quotes,
// and only the TUI's Import Cookie File prompt took them off. POST
// /api/ffmpeg/check refused a pasted `"C:\ffmpeg\bin\ffmpeg.exe"` as "the
// executable must be named ffmpeg", and PUT /api/config stored a pasted
// `"E:\Archive"` as the output directory — a relative directory whose name
// holds `"` and `:` on Windows, which every mux then failed to create.

// postFFmpegCheck POSTs path to /api/ffmpeg/check on a fresh store and
// returns the answer, the decoded body and the path the store then holds.
func postFFmpegCheck(t *testing.T, path string) (*httptest.ResponseRecorder, map[string]any, string) {
	t.Helper()
	store := config.NewStore(config.Defaults(), filepath.Join(t.TempDir(), "config.toml"))
	r := chi.NewRouter()
	FFmpegRoutes(r, &FFmpegDeps{Store: store, Logger: ffmpegTestLogger{}})
	body, _ := json.Marshal(map[string]string{"path": path})
	req := httptest.NewRequest("POST", "/api/ffmpeg/check", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	var stored string
	store.Read(func(c *config.MoomboxConfig) { stored = c.Paths.FfmpegPath })
	return rec, resp, stored
}

// TestFFmpegCheckTakesPastedQuotesOff: the check route reads a quoted path as
// the path inside the quotes — the name rule passes it, it is run and
// answered without them, and a working one is saved without them, so the
// form the dashboard fills from the answer holds the bare path.
//
// Mutants it kills: the route without config.UnquotePath (the missing
// ffmpeg.exe is a 400 from the name rule, and a working ffmpeg is refused by
// pathFieldError's quote rule); ffmpegPathError without it (the name rule's
// own row below fails).
func TestFFmpegCheckTakesPastedQuotesOff(t *testing.T) {
	if msg := ffmpegPathError(`"C:\ffmpeg\bin\ffmpeg.exe"`); msg != "" {
		t.Errorf("name rule on Copy as path's form of ffmpeg.exe: %q", msg)
	}
	if msg := ffmpegPathError(`"C:\out\imports\x [abc].mp4"`); msg == "" {
		t.Error("name rule passed a quoted path not named ffmpeg")
	}

	// No ffmpeg needed: a missing one is answered (valid=false), not refused.
	missing := filepath.Join(t.TempDir(), "bin", "ffmpeg.exe")
	rec, resp, _ := postFFmpegCheck(t, ` "`+missing+`" `)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST a quoted missing ffmpeg.exe: %d %s, want 200 (answered, not refused)", rec.Code, rec.Body.String())
	}
	if resp["path"] != missing {
		t.Errorf("answered path %q, want %q without its quotes", resp["path"], missing)
	}

	real, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH — the rest checks a quoted path that answers -version")
	}
	rec, resp, stored := postFFmpegCheck(t, `"`+real+`"`)
	if rec.Code != http.StatusOK || resp["valid"] != true {
		t.Fatalf("POST the quoted form of a working ffmpeg: %d %s, want 200 valid", rec.Code, rec.Body.String())
	}
	if resp["path"] != real || stored != real {
		t.Errorf("answered %q and saved %q, want %q without its quotes", resp["path"], stored, real)
	}
}

// TestConfigRefusesAQuotedPath: PUT /api/config refuses a new path setting
// wrapped in quotes, naming the field and the quotes, rather than storing a
// value nothing downstream unquotes — and /api/setup/complete refuses it too.
// An FFmpeg path in quotes is refused for its quotes, not as an executable
// not named ffmpeg. A value stored quoted before the rule, sent back
// unchanged by the full-form save, is not refused.
//
// Mutants it kills: the PUT not calling newQuotedPathErrors (every quoted PUT
// answers 200 and stores the quotes); setup/complete not calling it (the
// wizard's quoted output directory is stored); its changed-value check
// dropped (the stored quoted value sent back is refused); ffmpegPathError
// without config.UnquotePath (paths.ffmpeg_path's message becomes the name
// rule's, which newFFmpegPathError writes over the quote rule's).
func TestConfigRefusesAQuotedPath(t *testing.T) {
	for _, tc := range []struct {
		section, key, value string
	}{
		{"paths", "output_directory", `"E:\Archive"`},
		{"paths", "staging_directory", `'/data/staging'`},
		{"paths", "ffmpeg_path", `"C:\ffmpeg\bin\ffmpeg.exe"`},
		{"cookies", "cookie_file", ` "C:\Users\me\Downloads\cookies.txt" `},
		{"network", "tls_cert_path", `"C:\certs\moombox.pem"`},
	} {
		f := newConfigRoutesFixture(t)
		before := quotedPathFields(f.store)
		rec := putConfigExpect(t, f, map[string]any{tc.section: map[string]any{tc.key: tc.value}})
		field := tc.section + "." + tc.key
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s = %s: %d %s, want 400", field, tc.value, rec.Code, rec.Body.String())
			continue
		}
		var resp struct {
			Details map[string]string `json:"details"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if msg := resp.Details[field]; !strings.Contains(msg, "quotes") {
			t.Errorf("PUT %s = %s: details %v, want %s refused for its quotes", field, tc.value, resp.Details, field)
		}
		if after := quotedPathFields(f.store); after != before {
			t.Errorf("PUT %s = %s: the refused value was stored (%+v, was %+v)", field, tc.value, after, before)
		}
	}

	// A quote inside a path, or at one end alone, is a name, not a paste.
	f := newConfigRoutesFixture(t)
	putConfig(t, f, map[string]any{"paths": map[string]any{"output_directory": `/data/the "best" streams`}})

	// Stored quoted before the rule: the full-form save sends it back with
	// the field the operator did change, and that save goes through.
	f = newConfigRoutesFixture(t)
	f.store.Update(func(c *config.MoomboxConfig) { c.Paths.OutputDirectory = `"E:\Archive"` })
	putConfig(t, f, map[string]any{
		"paths": map[string]any{"output_directory": `"E:\Archive"`},
		"logs":  map[string]any{"log_level": "DEBUG"},
	})

	sf := newSetupFixture(t)
	rec := postSetupComplete(t, sf.router, map[string]any{"paths": map[string]any{"output_directory": `"E:\Archive"`}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "paths.output_directory") {
		t.Errorf("setup/complete with a quoted output directory: %d %s, want a 400 naming paths.output_directory", rec.Code, rec.Body.String())
	}
}

// TestApplyValidatedFfmpegPathRefusesAQuotedPath: the save the check route
// makes refuses a quoted path as PUT /api/config refuses a new one, so no
// caller of it can store one either; the route itself takes the quotes off
// first (TestFFmpegCheckTakesPastedQuotesOff).
//
// Mutant it kills: applyValidatedFfmpegPath without its PathIsQuoted guard
// (the quoted path is stored).
func TestApplyValidatedFfmpegPathRefusesAQuotedPath(t *testing.T) {
	store := config.NewStore(config.Defaults(), filepath.Join(t.TempDir(), "config.toml"))
	notified := false
	if err := applyValidatedFfmpegPath(store, `"C:\tools\ffmpeg.exe"`, func(string) { notified = true }); err == nil {
		t.Error("a quoted path was stored")
	}
	var stored string
	store.Read(func(c *config.MoomboxConfig) { stored = c.Paths.FfmpegPath })
	if stored != "" || notified {
		t.Errorf("stored %q (notified %v) for a refused path", stored, notified)
	}
}

// pathSettings is the path settings TestConfigRefusesAQuotedPath PUTs.
type pathSettings struct {
	paths              config.PathsConfig
	cookieFile, tlsCrt string
}

func quotedPathFields(store *config.Store) pathSettings {
	var p pathSettings
	store.Read(func(c *config.MoomboxConfig) {
		p = pathSettings{paths: c.Paths, cookieFile: c.Cookies.CookieFile, tlsCrt: c.Network.TLSCertPath}
	})
	return p
}
