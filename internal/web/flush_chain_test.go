package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestFlushReachesTheClientThroughTheRealChain: RecoveryMiddleware's writer
// had no Flush, and the gzip writer inside it asserted http.Flusher on it, so
// every handler's Flush stopped there. The handlers that answer before a
// blocking re-check (POST /api/cookies/import, the setup wizard's finish)
// held their response until it ended — up to 45 s — while their tests, built
// on a bare router, saw it at once. This drives the server's own chain over a
// real socket, with and without gzip.
//
// Mutant: drop recoveryWriter.Flush — the headers arrive only when the
// handler returns.
func TestFlushReachesTheClientThroughTheRealChain(t *testing.T) {
	s := NewServer(config.NewStore(config.Defaults(), ""), testWSLogger{})
	release := make(chan struct{})
	s.Router().Get("/flushed", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true}`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // the blocking re-check
	})
	ts := httptest.NewServer(s.Router())
	defer ts.Close()
	defer close(release)

	for _, enc := range []string{"identity", "gzip"} {
		t.Run(enc, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, ts.URL+"/flushed", nil)
			req.Header.Set("Accept-Encoding", enc)
			type result struct {
				resp *http.Response
				err  error
			}
			got := make(chan result, 1)
			go func() {
				resp, err := http.DefaultTransport.RoundTrip(req)
				got <- result{resp, err}
			}()
			select {
			case r := <-got:
				if r.err != nil {
					t.Fatalf("request: %v", r.err)
				}
				r.resp.Body.Close()
				if r.resp.StatusCode != http.StatusOK {
					t.Errorf("status %d, want 200", r.resp.StatusCode)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the flushed response did not reach the client while the handler was still running")
			}
		})
	}
}
