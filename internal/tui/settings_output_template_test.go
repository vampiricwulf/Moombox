package tui

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// The TUI refuses an over-long output template up front, naming the field,
// rather than letting config.Save refuse the whole config.
//
// Mutant: the TUI check removed — applyValues accepts it.
func TestSettingsRefusesAnOverlongOutputTemplate(t *testing.T) {
	cfg := config.Defaults()
	m := NewSettingsModel()
	m.configStore = config.NewStore(cfg, "")
	m.Open(cfg)
	m.values["output_template"] = strings.Repeat("x", config.OutputTemplateMaxLen+1)
	m.applyValues()
	if m.status != saveError || !strings.Contains(m.errorMsg, "Output template") {
		t.Errorf("status %v, message %q — want the output template refused by name", m.status, m.errorMsg)
	}
}
