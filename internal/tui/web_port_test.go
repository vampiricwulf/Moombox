package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestTUIUsesTheBoundWebPort: when the configured port is busy at boot the
// web server binds the next free one, and the TUI's local API calls and its
// "open in browser" URL have to use that. cmd/moombox used to tell it by
// writing the bound port into cfg.Network.Port, which every later save then
// persisted as the operator's setting; the TUI now asks SetWebPort's getter
// and the config keeps the configured port.
//
// Mutant: drop the webPort branch from getPort — both rows use 774.
func TestTUIUsesTheBoundWebPort(t *testing.T) {
	cfg := config.Defaults()
	cfg.Network.Port = 774
	a := NewApp()
	a.SetConfigStore(config.NewStore(cfg, ""))
	a.SetWebPort(func() int { return 775 })

	if got := a.getPort(); got != 775 {
		t.Errorf("getPort = %d, want the bound 775", got)
	}
	if got, want := a.apiBaseURL(), "http://127.0.0.1:775"; got != want {
		t.Errorf("apiBaseURL = %q, want %q", got, want)
	}

	// No bound port yet (or the server failed to start): the configured one.
	a.SetWebPort(func() int { return 0 })
	if got := a.getPort(); got != 774 {
		t.Errorf("getPort with nothing bound = %d, want the configured 774", got)
	}
}
