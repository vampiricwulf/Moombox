package routes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
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
