package cookies

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestCDPCommandsHonourTheCallersContext: both CDP helpers built their own
// context.Background() timeouts (5 s and 10 s), so Stop(), shutdown or the
// refresh's own deadline could not interrupt a command in flight, and a
// cookie read ran its full 10 s per tier after the refresh was cancelled.
//
// Mutant: derive the timeout from context.Background() again — the call runs
// its full 10 s and this test fails on its 3 s bound.
func TestCDPCommandsHonourTheCallersContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		<-r.Context().Done() // a browser that never answers
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := cdpSendCommandWithResult(ctx, wsURL, "Storage.getCookies", nil)
	if err == nil {
		t.Fatal("a command nobody answered succeeded")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the command ran %v after its caller's 50ms deadline", elapsed)
	}
}
