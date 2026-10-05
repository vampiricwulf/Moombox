package routes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/web"
)

type failingFormats struct{ err error }

func (f failingFormats) GetFormats(context.Context, string) (map[string]any, error) {
	return nil, f.err
}

// TestFormatLookupFailureIsLogged: the route answers a failed extraction with
// a generic 500 and used to drop the cause, so "Could not load format
// options" left nothing to look up in the log.
//
// Mutant: delete the Logger.Warn — the cause is not in the log.
func TestFormatLookupFailureIsLogged(t *testing.T) {
	log := &recordingLogger{}
	r := chi.NewRouter()
	FormatRoutes(r, &FormatRoutesDeps{
		YT:     failingFormats{err: errors.New("playability: members_only")},
		Logger: log,
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/formats/dQw4w9WgXcQ", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "members_only") {
		t.Errorf("the response carries the extraction error: %s", rec.Body.String())
	}
	got := log.all()
	if !strings.Contains(got, "members_only") || !strings.Contains(got, "dQw4w9WgXcQ") {
		t.Errorf("the failure was not logged with its cause and video ID:\n%s", got)
	}
}

// TestExpensiveRoutesAreRateLimited: the spec's general API limiter covered
// three route groups, leaving routes that spend upstream requests or spawn
// FFmpeg unbounded for any client the IP gate admits — a hostile LAN device
// could get the operator's IP rate-limited by YouTube. Formats,
// resolve-channel and trim creation now take the limiter.
//
// Mutant: drop r.With(limitedBy(...)) from any of the three.
func TestExpensiveRoutesAreRateLimited(t *testing.T) {
	newLimiter := func() *web.RateLimiter {
		rl := web.NewRateLimiterCtx(t.Context(), 1, time.Minute)
		return rl
	}
	r := chi.NewRouter()
	FormatRoutes(r, &FormatRoutesDeps{
		YT:        failingFormats{err: errors.New("x")},
		Logger:    &recordingLogger{},
		RateLimit: newLimiter(),
	})
	ChannelRoutes(r, config.NewStore(config.Defaults(), ""), func() {}, newLimiter())
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	TrimRoutes(r, db, nil, newLimiter()) // the job is unknown, so the handler answers 404 before the service

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/formats/dQw4w9WgXcQ"},
		{http.MethodPost, "/api/resolve-channel"},
		{http.MethodPost, "/api/jobs/abc/trims"},
	} {
		var codes []int
		for range 2 {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}")))
			codes = append(codes, rec.Code)
		}
		if codes[1] != http.StatusTooManyRequests {
			t.Errorf("%s %s: second call answered %d, want 429 — the route is not rate limited (codes %v)",
				tc.method, tc.path, codes[1], codes)
		}
	}
}
