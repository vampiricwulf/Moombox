package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
)

// getSidecarStatusBody registers StatusRoute on a bare router and returns the
// decoded body.
func getSidecarStatusBody(t *testing.T) map[string]any {
	t.Helper()
	r := chi.NewRouter()
	StatusRoute(r, &StatusRouteDeps{Version: "test", StartTime: time.Now()})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /api/status: %v", err)
	}
	return body
}

// TestStatusCarriesSidecarHealth is the dashboard half of YOUTUBE-5's
// user-visible signal. The ABSENT case is asserted first and on purpose: a
// process with `[bgutils] use_sidecar = false` publishes nothing, and the key
// must then be absent rather than present-and-false — otherwise a correct
// config would paint a permanent warning in the header.
//
// Mutants this kills:
//   - emitting the key unconditionally    → the first subtest finds it
//   - dropping the key entirely           → the second subtest finds nothing
//   - reporting healthy while it is not   → healthy == true in the second subtest
func TestStatusCarriesSidecarHealth(t *testing.T) {
	t.Run("absent when the sidecar was never started", func(t *testing.T) {
		sidecar.ResetHealthForTesting()
		t.Cleanup(sidecar.ResetHealthForTesting)
		if _, ok := getSidecarStatusBody(t)["botguardSidecar"]; ok {
			t.Error("botguardSidecar is present although no health was ever published")
		}
	})

	t.Run("present and false once the supervisor reports a death", func(t *testing.T) {
		sidecar.ResetHealthForTesting()
		sidecar.PublishHealth(sidecar.Health{Healthy: false, Reason: "stdout EOF", Restarts: 2})
		t.Cleanup(sidecar.ResetHealthForTesting)

		raw, ok := getSidecarStatusBody(t)["botguardSidecar"]
		if !ok {
			t.Fatal("botguardSidecar missing from /api/status")
		}
		got, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("botguardSidecar = %T, want an object", raw)
		}
		if got["healthy"] != false {
			t.Errorf("healthy = %v, want false", got["healthy"])
		}
		if got["reason"] != "stdout EOF" {
			t.Errorf("reason = %v, want %q", got["reason"], "stdout EOF")
		}
		if got["restarts"] != float64(2) {
			t.Errorf("restarts = %v, want 2", got["restarts"])
		}
	})
}
