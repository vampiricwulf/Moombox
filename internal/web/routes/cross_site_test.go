package routes

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/updater"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// TestActingGETsRefuseCrossSitePages: CSRFMiddleware passes every GET, but
// these three handlers act — a YouTube extraction with the operator's cookies,
// an ffmpeg spawn, a GitHub fetch — so an <img> on any page open in the
// operator's browser could fire them at a loopback dashboard. Each refuses a
// request the browser marks cross-site and still answers the dashboard's own.
// Every arm is wired so the request without the header never leaves the
// process: the formats lookup fails in-process, the ffmpeg path does not
// exist, and the running version is malformed so release notes 400 before
// dialling.
//
// Mutant: drop web.RefuseCrossSite from any one route.
func TestActingGETsRefuseCrossSitePages(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Paths.FfmpegPath = filepath.Join(dir, "missing", "ffmpeg")
	store := config.NewStore(cfg, filepath.Join(dir, "config.toml"))
	InvalidateFFmpegCache()
	t.Cleanup(InvalidateFFmpegCache)

	upd, err := updater.New("2.6.0 beta", silentLogger{})
	if err != nil {
		t.Fatal(err)
	}
	resetUpdateGlobals(t)
	t.Cleanup(func() { resetUpdateGlobals(t) })

	r := chi.NewRouter()
	FormatRoutes(r, &FormatRoutesDeps{YT: failingFormats{err: errors.New("offline")}})
	FFmpegRoutes(r, &FFmpegDeps{Store: store, Logger: ffmpegTestLogger{}})
	UpdateRoutes(r, &UpdateRouteDeps{Updater: upd, Version: "2.6.0 beta"}, store)

	for _, path := range []string{
		"/api/formats/dQw4w9WgXcQ",
		"/api/ffmpeg/check",
		"/api/update/release-notes",
	} {
		for _, site := range []string{"", "same-origin", "same-site", "none"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			if site != "" {
				req.Header.Set("Sec-Fetch-Site", site)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code == http.StatusForbidden {
				t.Errorf("%s with Sec-Fetch-Site %q: refused, want it answered", path, site)
			}
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s from another site's page: status %d, want 403", path, rec.Code)
		}
	}
}

// TestACrossSiteFormatsLookupSpendsNoBudget: the format picker shares its
// route's limiter with every other caller, so a refused cross-site request
// must be turned away before the limiter counts it — or a page firing <img>
// tags could still lock the operator's own picker out with 429s.
//
// Mutant: put limitedBy ahead of web.RefuseCrossSite.
func TestACrossSiteFormatsLookupSpendsNoBudget(t *testing.T) {
	r := chi.NewRouter()
	FormatRoutes(r, &FormatRoutesDeps{
		YT:        failingFormats{err: errors.New("offline")},
		RateLimit: web.NewRateLimiterCtx(t.Context(), 1, time.Minute),
	})
	for range 3 {
		req := httptest.NewRequest(http.MethodGet, "/api/formats/dQw4w9WgXcQ", nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		r.ServeHTTP(httptest.NewRecorder(), req)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/formats/dQw4w9WgXcQ", nil))
	if rec.Code == http.StatusTooManyRequests {
		t.Error("the dashboard's own lookup got 429 after three refused cross-site requests")
	}
}
