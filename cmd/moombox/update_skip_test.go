package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/updater"
	"github.com/vampiricwulf/Moombox/internal/web"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

// dashboardFrame is one frame an open dashboard received.
type dashboardFrame struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// connectDashboard opens one dashboard's socket on hub and returns a reader of
// the frames it receives after its initial_state, which is read here: the hub
// registers a client before it writes the snapshot, so every broadcast from
// then on reaches it. next fails the test when no frame comes within a second.
func connectDashboard(t *testing.T, hub *web.WebSocketHub) (next func() dashboardFrame) {
	t.Helper()
	hub.InitialState = func() map[string]any { return map[string]any{} }
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		cancel()
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		conn.CloseNow()
		cancel()
		srv.Close()
		hub.Close()
	})
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("read initial_state: %v", err)
	}
	return func() dashboardFrame {
		t.Helper()
		rctx, rcancel := context.WithTimeout(ctx, time.Second)
		defer rcancel()
		_, data, err := conn.Read(rctx)
		if err != nil {
			t.Fatalf("no frame reached the dashboard: %v", err)
		}
		var f dashboardFrame
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("frame %q: %v", data, err)
		}
		return f
	}
}

// tagOf reads the tagName a frame's payload carries.
func tagOf(t *testing.T, f dashboardFrame) string {
	t.Helper()
	var p struct {
		TagName string `json:"tagName"`
	}
	if err := json.Unmarshal(f.Payload, &p); err != nil {
		t.Fatalf("payload %s: %v", f.Payload, err)
	}
	return p.TagName
}

// pendingRelease stores rel as the pending release for the test's duration.
func pendingRelease(t *testing.T, rel *updater.ReleaseInfo) {
	t.Helper()
	prev := routes.SharedUpdateInfo.Load()
	t.Cleanup(func() { routes.SharedUpdateInfo.Store(prev) })
	routes.SharedUpdateInfo.Store(rel)
}

// The TUI's S tells every open dashboard the release it skipped is withdrawn,
// as the dashboard's own Skip does (POST /api/update/dismiss → OnCleared). A
// dashboard learns of a withdrawal only from update_cleared or a reload, and
// without it the badge stayed up with both of its buttons failing: Update Now
// answered "no update available" and Skip "no update pending".
//
// Mutant: dismissUpdateFromTUI returning after DismissUpdate without the
// announcement — no frame reaches the dashboard.
func TestTheTUISkipClearsEveryDashboardsBadge(t *testing.T) {
	pendingRelease(t, &updater.ReleaseInfo{TagName: "v9.9.9", Version: "9.9.9"})
	store := config.NewStore(config.Defaults(), filepath.Join(t.TempDir(), "config.toml"))
	hub := web.NewWebSocketHub(sweepTestLogger{})
	next := connectDashboard(t, hub)
	ch := make(chan tui.UpdateStatusMsg, 2)
	s := &runState{configStore: store, wsHub: hub, tuiUpdateStatusCh: ch}

	if err := s.dismissUpdateFromTUI("v9.9.9"); err != nil {
		t.Fatalf("skip: %v", err)
	}
	var skipped string
	store.Read(func(c *config.MoomboxConfig) { skipped = c.Updates.SkippedVersion })
	if skipped != "v9.9.9" {
		t.Errorf("skipped version = %q, want v9.9.9", skipped)
	}
	if got := routes.SharedUpdateInfo.Load(); got != nil {
		t.Errorf("pending after the skip = %s, want none", got.TagName)
	}
	if f := next(); f.Type != "update_cleared" || tagOf(t, f) != "v9.9.9" {
		t.Errorf("dashboard got %s %s, want update_cleared naming v9.9.9", f.Type, f.Payload)
	}
}

// The TUI's U and S act on the release whose notes it shows, and only while
// that is still the pending one — the rule the dashboard's dialog is held to
// (routes.PendingUpdate). A TUI whose copy is stale (the message naming the
// newer release was dropped) is refused, nothing is installed, skipped or
// announced, and the TUI is handed the release now pending, so R N shows the
// notes to read first; with nothing pending it is told to drop its badge.
//
// Mutants: applyUpdateFromTUI or dismissUpdateFromTUI resolving
// PendingUpdate("") / skipping without DismissUpdate's check — the stale
// request goes ahead; resyncTUIUpdate sending nothing — the TUI keeps the
// stale badge.
func TestTheTUIActsOnlyOnTheReleaseItShows(t *testing.T) {
	newer := &updater.ReleaseInfo{TagName: "v9.9.2", Version: "9.9.2", ReleaseNotes: "notes 9.9.2"}
	for _, tc := range []struct {
		name    string
		pending *updater.ReleaseInfo
		want    tui.UpdateStatusMsg // what the TUI is handed
	}{
		{"a newer release pending", newer, tui.UpdateStatusMsg{TagName: "v9.9.2", Version: "9.9.2", ReleaseNotes: "notes 9.9.2"}},
		{"nothing pending", nil, tui.UpdateStatusMsg{TagName: "v9.9.1"}},
	} {
		for _, key := range []string{"U", "S"} {
			t.Run(key+" with "+tc.name, func(t *testing.T) {
				pendingRelease(t, tc.pending)
				store := config.NewStore(config.Defaults(), filepath.Join(t.TempDir(), "config.toml"))
				hub := web.NewWebSocketHub(sweepTestLogger{})
				next := connectDashboard(t, hub)
				ch := make(chan tui.UpdateStatusMsg, 2)
				// No updater: an apply that got past the check would panic.
				s := &runState{configStore: store, wsHub: hub, tuiUpdateStatusCh: ch}

				var refused bool
				if key == "U" {
					refused = s.applyUpdateFromTUI("v9.9.1") != ""
				} else {
					refused = s.dismissUpdateFromTUI("v9.9.1") != nil
				}
				if !refused {
					t.Fatal("acting on v9.9.1, which is not pending, was not refused")
				}
				var skipped string
				store.Read(func(c *config.MoomboxConfig) { skipped = c.Updates.SkippedVersion })
				if skipped != "" {
					t.Errorf("a refused skip saved %q", skipped)
				}
				if routes.SharedUpdateInfo.Load() != tc.pending {
					t.Error("the pending release changed under a refused request")
				}
				select {
				case got := <-ch:
					if got != tc.want {
						t.Errorf("TUI handed %+v, want %+v", got, tc.want)
					}
				default:
					t.Errorf("the TUI was not re-synced; want %+v", tc.want)
				}
				hub.Broadcast("sentinel", nil)
				if f := next(); f.Type != "sentinel" {
					t.Errorf("a refused request sent the dashboards %s %s", f.Type, f.Payload)
				}
			})
		}
	}
}
