package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestClientTokensNavigationClearsTheRevokeConfirm: the dialog's list moves on
// paging and Home/End as well as on up/down (newTokenList's key map), but the
// revoke-confirm arming was cleared only on up/down — so PgDn away from an
// armed token left its "Press D again" hint standing over a different row,
// and the timer still counting. Every cursor-moving key now clears the id,
// the timer and the hint, as the files dialog already does.
//
// Mutant: the old `key == keyUp || key == keyDown` guard — the four paging
// rows fail.
func TestClientTokensNavigationClearsTheRevokeConfirm(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{
		{Code: tea.KeyPgDown}, {Code: tea.KeyPgUp}, {Code: tea.KeyHome}, {Code: tea.KeyEnd},
		{Code: tea.KeyUp}, {Code: tea.KeyDown},
	} {
		t.Run(key.String(), func(t *testing.T) {
			m := NewClientTokensDialogModel()
			m.SetSize(100, 30)
			m.Open()
			m.SetTokens([]*database.ClientToken{{ID: "t1", Label: "one"}, {ID: "t2", Label: "two"}})

			m.HandleKey(tea.KeyPressMsg{Code: 'd', Text: "d"})
			if m.revokeConfirmID != "t1" || m.confirmTimer.IsZero() || m.feedbackMsg == "" {
				t.Fatalf("premise lost: D did not arm the revoke confirm (id=%q timer=%v msg=%q)",
					m.revokeConfirmID, m.confirmTimer, m.feedbackMsg)
			}

			m.HandleKey(key)
			if m.revokeConfirmID != "" || !m.confirmTimer.IsZero() || m.feedbackMsg != "" {
				t.Errorf("%s left the revoke confirm armed: id=%q timer=%v msg=%q",
					key.String(), m.revokeConfirmID, m.confirmTimer, m.feedbackMsg)
			}
		})
	}
}
