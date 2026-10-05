package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// https_enabled takes effect at the next restart, but the TUI read it live for
// its own API calls and O W, so saving it without the restart pointed every
// TUI action at https:// on a listener still serving http. The scheme now
// comes from the bound server (SetWebHTTPS), like the port; the setting is
// only the fallback before one is wired.
//
// Mutants: apiBaseURL or apiClient reading the setting directly again.
func TestLocalCallsUseTheBoundServersScheme(t *testing.T) {
	cfg := config.Defaults()
	cfg.Network.HTTPSEnabled = true // saved, restart declined
	a := NewApp()
	a.SetConfigStore(config.NewStore(cfg, ""))
	a.SetConfig(cfg)
	a.SetWebPort(func() int { return 775 })

	if got := a.apiBaseURL(); got != "https://127.0.0.1:775" {
		t.Errorf("without a bound server the setting decides: apiBaseURL = %q", got)
	}
	a.SetWebHTTPS(func() bool { return false }) // the listener booted plain HTTP
	if got := a.apiBaseURL(); got != "http://127.0.0.1:775" {
		t.Errorf("apiBaseURL = %q, want the listener's http scheme", got)
	}
	a.apiClient()
	if a.cachedClientHTTPS {
		t.Error("apiClient was built for HTTPS against a plain-HTTP listener")
	}
}
