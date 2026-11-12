package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestASecurityCommitReportsThePasswordChange: Settings, Security set and
// removed the password without telling the wiring, so the client tokens,
// sessions and sockets the old password had admitted all stayed good.
//
// Mutant: drop the OnPasswordChanged call from the OnSecurityChanged closure.
func TestASecurityCommitReportsThePasswordChange(t *testing.T) {
	a := NewApp()
	a.SetConfigStore(config.NewStore(config.Defaults(), ""))
	a.width, a.height = 120, 40
	a.recalcLayout()
	changed := 0
	a.OnPasswordChanged = func() { changed++ }

	a.dispatchAction("`", nil)
	if a.settings.OnSecurityChanged == nil {
		t.Fatal("opening Settings did not wire OnSecurityChanged")
	}
	a.settings.OnSecurityChanged()
	if changed != 1 {
		t.Errorf("OnPasswordChanged ran %d times after a Security commit, want 1", changed)
	}
}
